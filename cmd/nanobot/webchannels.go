package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/yedamao/nanobot-go/internal/channels/telegram"
	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/channels/weixin"
	"github.com/yedamao/nanobot-go/internal/config"
)

const (
	// maxChannelTokenLen caps a channel token set from the WebUI.
	maxChannelTokenLen = 4096
	// maxAllowFrom caps allowFrom set from the WebUI, and
	// maxAllowFromEntry each of its entries.
	maxAllowFrom      = 1000
	maxAllowFromEntry = 256
	// maxRouteTagLen caps a WeChat routeTag set from the WebUI.
	maxRouteTagLen = 256
	// channelTestTimeout bounds a token probe.
	channelTestTimeout = 15 * time.Second
	// placeholderTelegramToken stands in for a missing token when a
	// disabled Telegram section is checked: the token is only required
	// to run.
	placeholderTelegramToken = "0:placeholder"
)

// configurableChannels are the channels the WebUI can configure, in page
// order. The websocket channel is left out: its secrets would be shown
// to the page they protect.
var configurableChannels = []string{telegram.Name, weixin.Name}

// webChannels is the WebUI's channel configuration store
// (/ui/channels/config). Like webSettings it loads the config file
// afresh for every change, rewrites only the whitelisted keys of one
// channels.<name> section and saves it, under the settings' lock so the
// stores never save over each other's change. It never returns a token,
// only whether one is saved and a hint of it. Channels are built when
// the gateway starts: running holds the sections it started with, to
// report which channels need a restart for a saved change to take effect.
type webChannels struct {
	settings *webSettings
	running  map[string]json.RawMessage
	// live names the channels running now; nil reports none.
	live   func() []string
	getenv func(string) string
	// weixinStateDir is where `nanobot channels login weixin` saves the
	// login when the section sets no stateDir.
	weixinStateDir string
	// client probes Telegram tokens; nil uses one honoring the proxy.
	client *http.Client
	// weixinLoginCleared is set when a saved WeChat login was cleared
	// while the channel ran: it keeps the token in memory until the
	// gateway restarts.
	weixinLoginCleared atomic.Bool
}

// newWebChannels returns the channel config store sharing settings'
// config file and lock. running is the gateway's channels config.
func newWebChannels(settings *webSettings, running map[string]json.RawMessage, live func() []string, getenv func(string) string, weixinStateDir string) *webChannels {
	r := map[string]json.RawMessage{}
	for _, name := range configurableChannels {
		if raw, ok := running[name]; ok {
			r[name] = slices.Clone(raw)
		}
	}
	return &webChannels{settings: settings, running: r, live: live, getenv: getenv, weixinStateDir: weixinStateDir}
}

// ChannelConfigs implements websocket.ChannelConfigStore.
func (s *webChannels) ChannelConfigs() (websocket.ChannelConfigList, error) {
	sections, err := s.load()
	if err != nil {
		return websocket.ChannelConfigList{}, err
	}
	out := websocket.ChannelConfigList{Channels: []websocket.ChannelConfigView{}}
	for _, name := range configurableChannels {
		raw, ok := sections[name]
		out.Channels = append(out.Channels, s.view(name, raw, ok))
	}
	return out, nil
}

// load reads the channel sections from the config file.
func (s *webChannels) load() (map[string]json.RawMessage, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	cfg, err := config.Load(s.settings.path)
	if err != nil {
		return nil, err
	}
	return cfg.Channels, nil
}

// UpdateChannelConfig implements websocket.ChannelConfigStore.
func (s *webChannels) UpdateChannelConfig(name string, p websocket.ChannelConfigPatch) (websocket.ChannelConfigView, error) {
	if !slices.Contains(configurableChannels, name) {
		return websocket.ChannelConfigView{}, websocket.ErrChannelNotConfigurable
	}
	set, del, err := channelChanges(name, p)
	if err != nil {
		return websocket.ChannelConfigView{}, fmt.Errorf("%w: %v", websocket.ErrInvalidChannelConfig, err)
	}

	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	cfg, err := config.Load(s.settings.path)
	if err != nil {
		return websocket.ChannelConfigView{}, err
	}
	raw, ok := cfg.Channels[name]
	if !ok {
		raw = json.RawMessage(`{"enabled":false}`)
	}
	patched, err := setKeys(raw, set, del)
	if err != nil {
		return websocket.ChannelConfigView{}, fmt.Errorf("%w: channels.%s: %v", websocket.ErrInvalidChannelConfig, name, err)
	}
	if err := s.check(name, patched); err != nil {
		return websocket.ChannelConfigView{}, fmt.Errorf("%w: channels.%s: %v", websocket.ErrInvalidChannelConfig, name, err)
	}
	if cfg.Channels == nil {
		cfg.Channels = map[string]json.RawMessage{}
	}
	cfg.Channels[name] = patched
	if err := cfg.Validate(); err != nil {
		return websocket.ChannelConfigView{}, fmt.Errorf("%w: %v", websocket.ErrInvalidChannelConfig, scrubToken(err.Error(), patched))
	}
	if err := cfg.Save(s.settings.path); err != nil {
		return websocket.ChannelConfigView{}, err
	}
	return s.view(name, patched, true), nil
}

// TestChannelConfig implements websocket.ChannelConfigStore: Telegram's
// token is checked with getMe; WeChat has no probe short of the login.
func (s *webChannels) TestChannelConfig(ctx context.Context, name string, t websocket.ChannelConfigTest) (websocket.ChannelTestResult, error) {
	switch name {
	case weixin.Name:
		return websocket.ChannelTestResult{Status: websocket.ChannelTestUnsupported,
			Message: "WeChat cannot be tested from here; run 'nanobot channels login weixin' to log in and check the account."}, nil
	case telegram.Name:
	default:
		return websocket.ChannelTestResult{}, websocket.ErrChannelNotConfigurable
	}
	var draft string
	if t.Token != nil {
		tok, err := checkChannelToken(name, *t.Token)
		if err != nil {
			return websocket.ChannelTestResult{}, fmt.Errorf("%w: %v", websocket.ErrInvalidChannelConfig, err)
		}
		draft = tok
	}
	sections, err := s.load()
	if err != nil {
		return websocket.ChannelTestResult{}, err
	}
	raw, ok := sections[name]
	if !ok {
		raw = json.RawMessage(`{}`)
	}
	// The draft wins over the environment, which wins over the file.
	getenv := s.getenv
	if draft != "" {
		getenv = func(k string) string {
			if k == telegram.EnvToken {
				return draft
			}
			return s.getenv(k)
		}
	}
	var head struct {
		Token string `json:"token"`
	}
	json.Unmarshal(raw, &head)
	if strings.TrimSpace(getenv(telegram.EnvToken)) == "" && strings.TrimSpace(head.Token) == "" {
		return websocket.ChannelTestResult{Status: websocket.ChannelTestNotConfigured, Message: "No bot token to test."}, nil
	}
	cfg, err := telegram.ParseConfig(raw, getenv)
	if err != nil {
		return websocket.ChannelTestResult{}, fmt.Errorf("%w: %v", websocket.ErrInvalidChannelConfig, scrubSecret(err.Error(), cfg.Token, draft, head.Token))
	}
	ctx, cancel := context.WithTimeout(ctx, channelTestTimeout)
	defer cancel()
	user, err := telegram.CheckToken(ctx, cfg, s.client)
	switch {
	case errors.Is(err, telegram.ErrTokenRejected):
		return websocket.ChannelTestResult{Status: websocket.ChannelTestRejected, Message: "Telegram rejected the bot token."}, nil
	case err != nil:
		return websocket.ChannelTestResult{Status: websocket.ChannelTestError,
			Message: "Cannot reach the Telegram Bot API: " + scrubSecret(err.Error(), cfg.Token, draft, head.Token)}, nil
	}
	msg := "Telegram accepted the bot token."
	if user != "" {
		msg = "Telegram accepted the token of @" + user + "."
	}
	return websocket.ChannelTestResult{OK: true, Status: websocket.ChannelTestOK, Message: msg}, nil
}

// ClearChannelLogin implements websocket.ChannelConfigStore: it removes
// WeChat's account.json (where the section's stateDir puts it), nothing
// else.
func (s *webChannels) ClearChannelLogin(name string) (websocket.ChannelConfigView, error) {
	if name != weixin.Name {
		return websocket.ChannelConfigView{}, websocket.ErrChannelNotConfigurable
	}
	sections, err := s.load()
	if err != nil {
		return websocket.ChannelConfigView{}, err
	}
	raw, ok := sections[name]
	path := s.weixinStatePath(raw, ok)
	had := weixin.LoginStatus(path)
	if err := weixin.ClearLogin(path); err != nil {
		return websocket.ChannelConfigView{}, err
	}
	if (had.LoggedIn || had.Error != "") && s.live != nil && slices.Contains(s.live(), name) {
		s.weixinLoginCleared.Store(true)
	}
	return s.view(name, raw, ok), nil
}

// weixinStatePath is where WeChat's login is saved for section raw
// (present reports whether the file has it).
func (s *webChannels) weixinStatePath(raw json.RawMessage, present bool) string {
	if present {
		if cfg, err := weixin.ParseConfig(raw, nil); err == nil {
			return cfg.StatePath(s.weixinStateDir)
		}
	}
	return weixin.Config{}.StatePath(s.weixinStateDir)
}

// channelChanges returns the keys p sets (as JSON) and removes in
// channel name's section, checking each value. Errors never hold the
// token.
func channelChanges(name string, p websocket.ChannelConfigPatch) (map[string]any, []string, error) {
	set := map[string]any{}
	var del []string
	optional := func(key, v string) {
		if v == "" {
			del = append(del, key)
		} else {
			set[key] = v
		}
	}
	if p.Enabled != nil {
		set["enabled"] = *p.Enabled
	}
	switch {
	case p.ClearToken:
		del = append(del, "token")
	case p.Token != nil:
		tok, err := checkChannelToken(name, *p.Token)
		if err != nil {
			return nil, nil, err
		}
		set["token"] = tok
	}
	if p.AllowFrom != nil {
		list, err := checkAllowFrom(*p.AllowFrom)
		if err != nil {
			return nil, nil, err
		}
		set["allowFrom"] = list
	}
	if p.PollTimeout != nil {
		switch n := *p.PollTimeout; {
		case n < 0 || n > 3600:
			return nil, nil, fmt.Errorf("pollTimeout must be between 0 and 3600 seconds")
		case n == 0:
			del = append(del, "pollTimeout")
		default:
			set["pollTimeout"] = n
		}
	}
	other := func(field string) error {
		return fmt.Errorf("%s is not a %s setting", field, name)
	}
	if name == telegram.Name {
		if p.BaseURL != nil {
			return nil, nil, other("baseUrl")
		}
		if p.RouteTag != nil {
			return nil, nil, other("routeTag")
		}
		if p.APIBase != nil {
			b, err := checkAPIBase(*p.APIBase)
			if err != nil {
				return nil, nil, err
			}
			optional("apiBase", b)
		}
		if p.GroupPolicy != nil {
			g := strings.TrimSpace(*p.GroupPolicy)
			if g != "" && g != telegram.GroupPolicyMention && g != telegram.GroupPolicyOpen {
				return nil, nil, fmt.Errorf("groupPolicy must be %q or %q", telegram.GroupPolicyMention, telegram.GroupPolicyOpen)
			}
			optional("groupPolicy", g)
		}
		if p.ReplyToMessage != nil {
			set["replyToMessage"] = *p.ReplyToMessage
		}
	} else {
		if p.APIBase != nil {
			return nil, nil, other("apiBase")
		}
		if p.GroupPolicy != nil {
			return nil, nil, other("groupPolicy")
		}
		if p.ReplyToMessage != nil {
			return nil, nil, other("replyToMessage")
		}
		if p.BaseURL != nil {
			b, err := checkAPIBase(*p.BaseURL)
			if err != nil {
				return nil, nil, fmt.Errorf("baseUrl: %v", strings.TrimPrefix(err.Error(), "apiBase "))
			}
			optional("baseUrl", b)
		}
		if p.RouteTag != nil {
			r := strings.TrimSpace(*p.RouteTag)
			if len(r) > maxRouteTagLen || strings.ContainsFunc(r, unicode.IsControl) {
				return nil, nil, fmt.Errorf("routeTag must be at most %d bytes without control characters", maxRouteTagLen)
			}
			optional("routeTag", r)
		}
	}
	return set, del, nil
}

// checkChannelToken trims a new token and rejects an empty one, a
// redacted placeholder or hint sent back, one with spaces or control
// characters and, for Telegram, one not shaped as BotFather's. Errors
// never hold the token.
func checkChannelToken(name, raw string) (string, error) {
	k := strings.TrimSpace(raw)
	switch {
	case k == "":
		return "", fmt.Errorf("token must not be empty; send clearToken to remove it")
	case strings.Contains(k, redacted) || strings.Contains(k, "•"):
		return "", fmt.Errorf("token is a redacted placeholder or hint; send the real token")
	case len(k) > maxChannelTokenLen:
		return "", fmt.Errorf("token must be at most %d bytes", maxChannelTokenLen)
	case strings.IndexFunc(k, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "", fmt.Errorf("token must not contain spaces or control characters")
	}
	if name == telegram.Name {
		if _, err := telegram.ParseConfig(json.RawMessage(`{}`), func(k2 string) string {
			if k2 == telegram.EnvToken {
				return k
			}
			return ""
		}); err != nil {
			return "", fmt.Errorf("token is malformed (want <bot id>:<secret> as issued by @BotFather)")
		}
	}
	return k, nil
}

// checkAllowFrom trims allowFrom's entries and drops empty ones.
func checkAllowFrom(list []string) ([]string, error) {
	if len(list) > maxAllowFrom {
		return nil, fmt.Errorf("allowFrom must have at most %d entries", maxAllowFrom)
	}
	out := []string{}
	for _, e := range list {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
			continue
		case len(e) > maxAllowFromEntry || strings.ContainsFunc(e, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
			return nil, fmt.Errorf("allowFrom entries must be at most %d bytes without spaces", maxAllowFromEntry)
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// check parses a patched section as the gateway would at start. A
// disabled Telegram section may lack its token; it is only required to
// run. Errors never hold the token.
func (s *webChannels) check(name string, raw json.RawMessage) error {
	var err error
	switch name {
	case telegram.Name:
		var head struct {
			Enabled bool   `json:"enabled"`
			Token   string `json:"token"`
		}
		json.Unmarshal(raw, &head)
		getenv := s.getenv
		if !head.Enabled && strings.TrimSpace(head.Token) == "" && s.getenv(telegram.EnvToken) == "" {
			getenv = func(k string) string {
				if k == telegram.EnvToken {
					return placeholderTelegramToken
				}
				return s.getenv(k)
			}
		}
		_, err = telegram.ParseConfig(raw, getenv)
	case weixin.Name:
		_, err = weixin.ParseConfig(raw, s.getenv)
	}
	if err != nil {
		return errors.New(scrubToken(err.Error(), raw))
	}
	return nil
}

// view describes channel name's section raw (present reports whether
// the file has it) without the token.
func (s *webChannels) view(name string, raw json.RawMessage, present bool) websocket.ChannelConfigView {
	v := websocket.ChannelConfigView{
		Name:         name,
		Present:      present,
		AllowFrom:    []string{},
		EnvOverrides: []string{},
		Running:      s.live != nil && slices.Contains(s.live(), name),
	}
	if !present {
		raw = json.RawMessage(`{}`)
	}
	// The section as written, without defaults: lenient so a section the
	// gateway rejects still shows what it holds.
	var sec struct {
		Enabled        bool            `json:"enabled"`
		Token          string          `json:"token"`
		AllowFrom      []any           `json:"allowFrom"`
		PollTimeout    int             `json:"pollTimeout"`
		APIBase        string          `json:"apiBase"`
		GroupPolicy    string          `json:"groupPolicy"`
		ReplyToMessage bool            `json:"replyToMessage"`
		Proxy          string          `json:"proxy"`
		BaseURL        string          `json:"baseUrl"`
		RouteTag       json.RawMessage `json:"routeTag"`
		StateDir       string          `json:"stateDir"`
	}
	json.Unmarshal(raw, &sec)
	tok := strings.TrimSpace(sec.Token)
	v.Enabled = sec.Enabled
	v.HasToken = tok != ""
	v.TokenHint = channelTokenHint(name, tok)
	for _, e := range sec.AllowFrom {
		v.AllowFrom = append(v.AllowFrom, strings.TrimSpace(fmt.Sprint(e)))
	}
	v.PollTimeout = sec.PollTimeout
	switch name {
	case telegram.Name:
		if s.getenv(telegram.EnvToken) != "" {
			v.EnvOverrides = append(v.EnvOverrides, "token")
		}
		v.APIBase = sec.APIBase
		v.GroupPolicy = sec.GroupPolicy
		v.ReplyToMessage = sec.ReplyToMessage
		v.HasProxy = sec.Proxy != ""
		v.DefaultBase = telegram.DefaultAPIBase
		v.TestSupported = true
	case weixin.Name:
		if s.getenv(weixin.EnvToken) != "" {
			v.EnvOverrides = append(v.EnvOverrides, "token")
		}
		v.BaseURL = sec.BaseURL
		var tag any
		if json.Unmarshal(sec.RouteTag, &tag) == nil && tag != nil {
			v.RouteTag = strings.TrimSpace(fmt.Sprint(tag))
		}
		v.DefaultBase = weixin.DefaultBaseURL
		v.Login = s.weixinLogin(s.weixinStatePath(raw, present))
		v.HasLoginState = &v.Login.LoggedIn
	}
	run, ok := s.running[name]
	v.RestartRequired = s.differs(name, raw, present, run, ok) || (name == weixin.Name && s.weixinLoginCleared.Load())
	return v
}

// weixinLogin describes the WeChat login saved at statePath and the
// command that logs in for this gateway's config file.
func (s *webChannels) weixinLogin(statePath string) *websocket.WeixinLoginView {
	info := weixin.LoginStatus(statePath)
	l := &websocket.WeixinLoginView{
		LoggedIn: info.LoggedIn, BotID: info.BotID, UserID: info.UserID, BaseURL: info.BaseURL,
		StatePath: statePath, Error: info.Error, CLICommand: weixinLoginCommand(s.settings.path),
	}
	switch {
	case s.weixinLoginCleared.Load() && !info.LoggedIn:
		l.LoginHint = "The saved login was cleared, but the running WeChat channel keeps using it until the gateway restarts. Restart the gateway; to log in again, run the command below in a terminal first."
	case info.Error != "":
		l.LoginHint = "The saved login cannot be used. Run the command below with --force in a terminal to log in again."
	case info.LoggedIn:
		l.LoginHint = "A login is saved. To switch accounts, run the command below with --force in a terminal, then restart the gateway."
	default:
		l.LoginHint = "Not logged in. Run the command below in a terminal and scan the QR code with WeChat, then restart the gateway."
	}
	return l
}

// weixinLoginCommand is the shell command that logs in to WeChat for the
// config file at path; --config is left out for the default file.
func weixinLoginCommand(path string) string {
	args := []string{"nanobot", "channels", "login", weixin.Name}
	if def, err := config.DefaultPath(); err != nil || filepath.Clean(path) != filepath.Clean(def) {
		args = append(args, "--config", path)
	}
	return shellJoin(args)
}

// differs reports whether the gateway would run channel name
// differently from saved (the section in the file) than from running
// (the one it started with), the environment applied to both.
func (s *webChannels) differs(name string, saved json.RawMessage, savedOK bool, running json.RawMessage, runningOK bool) bool {
	enabled := func(raw json.RawMessage, ok bool) bool {
		var head struct {
			Enabled bool `json:"enabled"`
		}
		return ok && json.Unmarshal(raw, &head) == nil && head.Enabled
	}
	on, wasOn := enabled(saved, savedOK), enabled(running, runningOK)
	if on != wasOn {
		return true
	}
	if !on {
		return false
	}
	parse := func(raw json.RawMessage) (any, error) {
		if name == telegram.Name {
			return telegram.ParseConfig(raw, s.getenv)
		}
		return weixin.ParseConfig(raw, s.getenv)
	}
	a, errA := parse(saved)
	b, errB := parse(running)
	if errA != nil || errB != nil {
		var ca, cb bytes.Buffer
		if json.Compact(&ca, saved) != nil || json.Compact(&cb, running) != nil {
			return true
		}
		return !bytes.Equal(ca.Bytes(), cb.Bytes())
	}
	return !reflect.DeepEqual(a, b)
}

// setKeys returns the channel section raw with set's keys set and del's
// removed. Every other key is kept with its value as it was. Keys match
// case-insensitively when the section is decoded, so differently cased
// duplicates of a key are removed too.
func setKeys(raw json.RawMessage, set map[string]any, del []string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("section is not a JSON object")
	}
	drop := func(key string) {
		for k := range obj {
			if strings.EqualFold(k, key) {
				delete(obj, k)
			}
		}
	}
	for _, k := range del {
		drop(k)
	}
	for k, v := range set {
		drop(k)
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		obj[k] = b
	}
	return json.Marshal(obj)
}

// channelTokenHint shows a little of a token: a Telegram token's bot ID
// (public: it is the bot's user ID) and the secret's last four
// characters, otherwise as apiKeyHint.
func channelTokenHint(name, tok string) string {
	if tok == "" {
		return ""
	}
	if name == telegram.Name {
		if id, secret, ok := strings.Cut(tok, ":"); ok && id != "" {
			r := []rune(secret)
			if len(r) <= 8 {
				return id + ":" + hintMask
			}
			return id + ":" + hintMask + string(r[len(r)-4:])
		}
	}
	return apiKeyHint(tok)
}

// scrubToken removes the token of section raw from s.
func scrubToken(s string, raw json.RawMessage) string {
	var head struct {
		Token string `json:"token"`
	}
	json.Unmarshal(raw, &head)
	return scrubSecret(s, head.Token, strings.TrimSpace(head.Token))
}

// scrubSecret removes every non-empty secret from s.
func scrubSecret(s string, secrets ...string) string {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

var _ websocket.ChannelConfigStore = (*webChannels)(nil)
