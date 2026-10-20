package weixin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/yedamao/nanobot-go/internal/channels"
)

const (
	// Name is the channel's name: its config section, routing key and
	// session key prefix.
	Name = "weixin"
	// EnvToken overrides channels.weixin.token.
	EnvToken = "NANOBOT_GO_WEIXIN_TOKEN"
	// DefaultBaseURL is Tencent's iLink bot API.
	DefaultBaseURL = "https://ilinkai.weixin.qq.com"
	// DefaultPollTimeout is the getupdates long-poll timeout in seconds
	// (nanobot: DEFAULT_LONG_POLL_TIMEOUT_S).
	DefaultPollTimeout = 35
	// StateFileName is the saved login state in the state directory
	// (nanobot: account.json).
	StateFileName = "account.json"
)

// DefaultStateDir returns the state directory for a config file:
// state/weixin next to it, so ~/.nanobot-go/state/weixin by default.
func DefaultStateDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "state", Name)
}

// Config is the channels.weixin section.
type Config struct {
	Enabled bool `json:"enabled"`
	// Token is a bot token. Usually it is left empty and comes from the
	// state saved by `nanobot channels login weixin`;
	// NANOBOT_GO_WEIXIN_TOKEN overrides it.
	Token string `json:"token"`
	// AllowFrom lists the WeChat user IDs allowed to talk to the bot, or
	// "*" for everyone. Empty allows only senders approved through
	// pairing.
	AllowFrom []string `json:"allowFrom"`
	// BaseURL is the iLink API (default https://ilinkai.weixin.qq.com).
	// The login can assign the account another host, which the saved
	// state keeps.
	BaseURL string `json:"baseUrl"`
	// RouteTag, when set, is sent as the SKRouteTag header.
	RouteTag routeTag `json:"routeTag"`
	// StateDir holds account.json (default: state/weixin next to the
	// config file).
	StateDir string `json:"stateDir"`
	// PollTimeout is the getupdates long-poll timeout in seconds
	// (default 35); the server can suggest another.
	PollTimeout int `json:"pollTimeout"`
}

// routeTag is a string that also accepts a JSON number, like nanobot's
// str | int.
type routeTag string

func (r *routeTag) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*r = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*r = routeTag(strings.TrimSpace(s))
		return nil
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("routeTag must be a string or a number, got %s", data)
	}
	*r = routeTag(n.String())
	return nil
}

// ParseConfig decodes a channels.weixin section, applies the token's
// environment override (getenv may be nil) and the defaults, and
// validates it. A missing token is not an error here: it can come from
// the saved login state. Errors never contain the token.
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
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.PollTimeout == 0 {
		cfg.PollTimeout = DefaultPollTimeout
	}
	if cfg.StateDir != "" {
		dir, err := expandHome(cfg.StateDir)
		if err != nil {
			return Config{}, err
		}
		if cfg.StateDir, err = filepath.Abs(dir); err != nil {
			return Config{}, fmt.Errorf("stateDir: %w", err)
		}
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !validToken(c.Token) {
		bad("token must not contain spaces or control characters")
	}
	if err := checkBaseURL(c.BaseURL); err != nil {
		bad("baseUrl: %v", err)
	}
	if c.PollTimeout < 0 {
		bad("pollTimeout must not be negative, got %d", c.PollTimeout)
	}
	if strings.ContainsFunc(string(c.RouteTag), unicode.IsControl) {
		bad("routeTag must not contain control characters")
	}
	return errors.Join(errs...)
}

// StatePath returns the account.json the channel and the login use:
// under StateDir, or under defaultDir when StateDir is empty.
func (c Config) StatePath(defaultDir string) string {
	dir := c.StateDir
	if dir == "" {
		dir = defaultDir
	}
	return filepath.Join(dir, StateFileName)
}

// pollWait returns PollTimeout as a duration.
func (c Config) pollWait() time.Duration { return time.Duration(c.PollTimeout) * time.Second }

// validToken reports whether a token can go into a header as is. The
// empty token is valid (not logged in).
func validToken(t string) bool {
	return !strings.ContainsFunc(t, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// checkBaseURL accepts an http(s) URL without a query or fragment. The
// error does not quote the URL.
func checkBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("must be an http(s) URL")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("must not have credentials, a query or a fragment")
	}
	return nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("stateDir: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
