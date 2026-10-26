package telegram

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/channels"
)

const (
	// Name is the channel's name: its config section, routing key and
	// session key prefix.
	Name = "telegram"
	// EnvToken overrides channels.telegram.token.
	EnvToken = "NANOBOT_GO_TELEGRAM_TOKEN"
	// DefaultAPIBase is the Bot API server.
	DefaultAPIBase = "https://api.telegram.org"
	// DefaultPollTimeout is the getUpdates long-poll timeout in seconds.
	DefaultPollTimeout = 30
	// Group policies (nanobot: groupPolicy).
	GroupPolicyMention = "mention"
	GroupPolicyOpen    = "open"
)

// tokenRE is the shape of a BotFather token. Checking it also keeps the
// token from changing the request path it is pasted into.
var tokenRE = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// Config is the channels.telegram section.
type Config struct {
	Enabled bool `json:"enabled"`
	// Token is the bot token from @BotFather; NANOBOT_GO_TELEGRAM_TOKEN
	// overrides it.
	Token string `json:"token"`
	// AllowFrom lists the senders allowed to talk to the bot: numeric
	// user IDs and/or usernames, or "*" for everyone. Empty allows only
	// senders approved through pairing.
	AllowFrom allowList `json:"allowFrom"`
	// APIBase is the Bot API server (default https://api.telegram.org).
	APIBase string `json:"apiBase"`
	// PollTimeout is the getUpdates long-poll timeout in seconds
	// (default 30).
	PollTimeout int `json:"pollTimeout"`
	// Proxy is an optional http, https or socks5 proxy URL for the Bot
	// API. Without it the standard proxy environment variables apply.
	Proxy string `json:"proxy"`
	// GroupPolicy is "mention" (default: in groups, answer only messages
	// that mention the bot or reply to it) or "open" (answer every
	// message from an allowed sender).
	GroupPolicy string `json:"groupPolicy"`
	// ReplyToMessage sends answers as replies to the message they answer.
	ReplyToMessage bool `json:"replyToMessage"`
	// InlineKeyboards shows the message tool's buttons as an inline
	// keyboard (nanobot: inline_keyboards) and receives taps on them;
	// off, the buttons are written under the text ("[Yes] [No]").
	InlineKeyboards bool `json:"inlineKeyboards"`
}

// allowList is a list of strings that also accepts JSON numbers, so a
// user ID can be written without quotes.
type allowList []string

func (l *allowList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(allowList, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err != nil {
			var n json.Number
			dec := json.NewDecoder(bytes.NewReader(r))
			dec.UseNumber()
			if dec.Decode(&n) != nil {
				return fmt.Errorf("allowFrom entries must be strings or numbers, got %s", r)
			}
			s = n.String()
		}
		out = append(out, s)
	}
	*l = out
	return nil
}

// ParseConfig decodes a channels.telegram section, applies the token's
// environment override (getenv may be nil) and the defaults, and
// validates it. Errors never contain the token.
func ParseConfig(raw json.RawMessage, getenv func(string) string) (Config, error) {
	var cfg Config
	if err := channels.DecodeConfig(raw, &cfg); err != nil {
		return Config{}, err
	}
	if getenv != nil {
		if v := getenv(EnvToken); v != "" {
			cfg.Token = v
		}
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	if cfg.PollTimeout == 0 {
		cfg.PollTimeout = DefaultPollTimeout
	}
	if cfg.GroupPolicy == "" {
		cfg.GroupPolicy = GroupPolicyMention
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch {
	case c.Token == "":
		bad("token is required (set channels.telegram.token or %s)", EnvToken)
	case !tokenRE.MatchString(c.Token):
		bad("token is malformed (want <bot id>:<secret> as issued by @BotFather)")
	}
	if u, err := url.Parse(c.APIBase); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		bad("apiBase must be an http(s) URL, got %q", c.APIBase)
	}
	if c.PollTimeout < 0 {
		bad("pollTimeout must not be negative, got %d", c.PollTimeout)
	}
	if c.GroupPolicy != GroupPolicyMention && c.GroupPolicy != GroupPolicyOpen {
		bad("groupPolicy must be %q or %q, got %q", GroupPolicyMention, GroupPolicyOpen, c.GroupPolicy)
	}
	if c.Proxy != "" {
		if _, err := proxyURL(c.Proxy); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pollWait returns PollTimeout as a duration.
func (c Config) pollWait() time.Duration { return time.Duration(c.PollTimeout) * time.Second }

// proxyURL parses a proxy setting; a bare host:port means http. The
// error does not quote the URL, which may hold credentials.
func proxyURL(s string) (*url.URL, error) {
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, errors.New("proxy must be a URL such as http://host:port or socks5://host:port")
	}
	switch u.Scheme {
	case "http", "https", "socks5":
		return u, nil
	}
	return nil, fmt.Errorf("proxy scheme %q is not supported (use http, https or socks5)", u.Scheme)
}
