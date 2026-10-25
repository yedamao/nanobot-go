package websocket

import (
	"context"
	"errors"
	"net/http"
)

const (
	// ChannelConfigPath lists the configurable channels' settings,
	// redacted (GET /ui/channels/config); PATCH
	// /ui/channels/config/{name} creates or changes channels.<name> in
	// the config file and POST /ui/channels/config/{name}/test probes its
	// token.
	ChannelConfigPath = "/ui/channels/config"
	// maxChannelConfigBody caps the JSON body of a channel change.
	maxChannelConfigBody = 16 << 10
)

// Channel token test statuses (ProviderTest* plus unsupported).
const (
	ChannelTestOK            = ProviderTestOK
	ChannelTestNotConfigured = ProviderTestNotConfigured
	ChannelTestRejected      = ProviderTestRejected
	ChannelTestError         = ProviderTestError
	// ChannelTestUnsupported: the channel has no probe; Message says how
	// to check it instead.
	ChannelTestUnsupported = "unsupported"
)

var (
	// ErrInvalidChannelConfig marks a ChannelConfigStore error caused by
	// the requested change; it is answered with 400 and the error's text,
	// which must never hold the token.
	ErrInvalidChannelConfig = errors.New("invalid channel change")
	// ErrChannelNotConfigurable marks a ChannelConfigStore error for a
	// name the page cannot configure; it is answered with 404.
	ErrChannelNotConfigurable = errors.New("no such configurable channel")
)

// ChannelConfigStore reads and changes the channels.<name> sections the
// page can configure (Telegram, WeChat) and probes their tokens.
// Implementations must load the config fresh from disk for every change
// and write back only that section, and must never return a token.
type ChannelConfigStore interface {
	ChannelConfigs() (ChannelConfigList, error)
	UpdateChannelConfig(name string, p ChannelConfigPatch) (ChannelConfigView, error)
	TestChannelConfig(ctx context.Context, name string, t ChannelConfigTest) (ChannelTestResult, error)
}

// ChannelConfigList is the body of GET /ui/channels/config.
type ChannelConfigList struct {
	Channels []ChannelConfigView `json:"channels"`
}

// ChannelConfigView is one channel's section as the page sees it: never
// its token, only whether one is saved and a short hint of it. Fields of
// another channel are left out.
type ChannelConfigView struct {
	Name string `json:"name"`
	// Present reports that the config file has the section; a PATCH
	// creates it.
	Present bool `json:"present"`
	Enabled bool `json:"enabled"`
	// HasToken reports that the section holds a token; TokenHint shows
	// a little of it (123456:••••abcd), empty without one.
	HasToken  bool     `json:"hasToken"`
	TokenHint string   `json:"tokenHint"`
	AllowFrom []string `json:"allowFrom"`
	// PollTimeout is the saved long-poll timeout in seconds; 0 uses the
	// default.
	PollTimeout int `json:"pollTimeout"`

	// Telegram.
	APIBase        string `json:"apiBase,omitempty"`
	GroupPolicy    string `json:"groupPolicy,omitempty"`
	ReplyToMessage bool   `json:"replyToMessage,omitempty"`
	// HasProxy reports a proxy setting, which is not shown since it may
	// hold credentials.
	HasProxy bool `json:"hasProxy,omitempty"`

	// WeChat.
	BaseURL  string `json:"baseUrl,omitempty"`
	RouteTag string `json:"routeTag,omitempty"`
	// HasLoginState reports a token saved by `nanobot channels login
	// weixin`, which is used when the section has none.
	HasLoginState *bool `json:"hasLoginState,omitempty"`
	// Login describes the saved WeChat login without its secrets.
	Login *WeixinLoginView `json:"login,omitempty"`

	// DefaultBase is the API endpoint used when apiBase/baseUrl is empty.
	DefaultBase string `json:"defaultBase"`
	// EnvOverrides names the fields ("token") the environment sets,
	// which win over the file.
	EnvOverrides []string `json:"env_overrides"`
	// RestartRequired reports that the saved section, with the
	// environment applied, differs from the one the gateway started
	// with: channels are built when it starts.
	RestartRequired bool `json:"restart_required"`
	// Running reports that the gateway runs the channel now.
	Running bool `json:"running"`
	// TestSupported reports that POST .../test can probe the token.
	TestSupported bool `json:"testSupported"`
}

// WeixinLoginView is the non-secret part of the login saved by `nanobot
// channels login weixin`: never the token, cursor or context tokens.
type WeixinLoginView struct {
	LoggedIn bool   `json:"loggedIn"`
	BotID    string `json:"botId,omitempty"`
	UserID   string `json:"userId,omitempty"`
	// BaseURL is the API host the login assigned, if any.
	BaseURL string `json:"baseUrl,omitempty"`
	// StatePath is where the login is saved.
	StatePath string `json:"statePath"`
	// Error describes a state file that cannot be read, without its
	// contents.
	Error string `json:"error,omitempty"`
	// CLICommand is the shell command that logs in (QR code in the
	// terminal); LoginHint says when to run it.
	CLICommand string `json:"cliCommand"`
	LoginHint  string `json:"loginHint"`
}

// ChannelConfigPatch is the body of PATCH /ui/channels/config/{name}. A
// missing field keeps the saved value; token sets the token and
// clearToken removes it (token is then ignored). An empty apiBase,
// baseUrl, groupPolicy or routeTag and a pollTimeout of 0 remove the
// setting (the default applies). Telegram takes apiBase, groupPolicy and
// replyToMessage; WeChat takes baseUrl and routeTag.
type ChannelConfigPatch struct {
	Enabled        *bool     `json:"enabled"`
	Token          *string   `json:"token"`
	ClearToken     bool      `json:"clearToken"`
	AllowFrom      *[]string `json:"allowFrom"`
	PollTimeout    *int      `json:"pollTimeout"`
	APIBase        *string   `json:"apiBase"`
	GroupPolicy    *string   `json:"groupPolicy"`
	ReplyToMessage *bool     `json:"replyToMessage"`
	BaseURL        *string   `json:"baseUrl"`
	RouteTag       *string   `json:"routeTag"`
}

// empty reports that p changes nothing.
func (p ChannelConfigPatch) empty() bool {
	return p.Enabled == nil && p.Token == nil && !p.ClearToken && p.AllowFrom == nil && p.PollTimeout == nil &&
		p.APIBase == nil && p.GroupPolicy == nil && p.ReplyToMessage == nil && p.BaseURL == nil && p.RouteTag == nil
}

// ChannelConfigTest is the body of POST /ui/channels/config/{name}/test.
// A missing token tests the saved one (environment applied, as the
// gateway runs). The draft is used once, never saved.
type ChannelConfigTest struct {
	Token *string `json:"token"`
}

// ChannelTestResult is the answer of POST
// /ui/channels/config/{name}/test. Message is short, safe text: never
// the token.
type ChannelTestResult struct {
	OK      bool   `json:"ok"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// serveChannelConfigs answers GET /ui/channels/config.
func (c *Channel) serveChannelConfigs(w http.ResponseWriter, _ *http.Request) {
	list, err := c.channelConfig.ChannelConfigs()
	if err != nil {
		c.channelConfigError(w, "list", err)
		return
	}
	writeJSON(w, list)
}

// patchChannelConfig answers PATCH /ui/channels/config/{name}.
func (c *Channel) patchChannelConfig(w http.ResponseWriter, r *http.Request) {
	var p ChannelConfigPatch
	if err := decodeJSONBody(r, &p, maxChannelConfigBody); err != nil {
		http.Error(w, "body must be {enabled?, token?, clearToken?, allowFrom?, pollTimeout?, apiBase?, groupPolicy?, replyToMessage?, baseUrl?, routeTag?}", http.StatusBadRequest)
		return
	}
	if p.empty() {
		http.Error(w, "nothing to change", http.StatusBadRequest)
		return
	}
	v, err := c.channelConfig.UpdateChannelConfig(r.PathValue("name"), p)
	if err != nil {
		c.channelConfigError(w, "update", err)
		return
	}
	writeJSON(w, v)
}

// testChannelConfig answers POST /ui/channels/config/{name}/test. An
// empty body is taken as {}.
func (c *Channel) testChannelConfig(w http.ResponseWriter, r *http.Request) {
	var t ChannelConfigTest
	if r.ContentLength != 0 {
		if err := decodeJSONBody(r, &t, maxChannelConfigBody); err != nil {
			http.Error(w, "body must be {token?}", http.StatusBadRequest)
			return
		}
	}
	res, err := c.channelConfig.TestChannelConfig(r.Context(), r.PathValue("name"), t)
	if err != nil {
		c.channelConfigError(w, "test", err)
		return
	}
	writeJSON(w, res)
}

// channelConfigError answers a failed channel config request. Only
// ErrInvalidChannelConfig errors are shown; they never hold the token.
func (c *Channel) channelConfigError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrInvalidChannelConfig):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrChannelNotConfigurable):
		http.Error(w, "no such configurable channel", http.StatusNotFound)
	default:
		c.log.Warn("websocket: "+op+" channel config", "error", err)
		http.Error(w, "cannot "+op+" the channel config", http.StatusInternalServerError)
	}
}
