package websocket

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

const (
	// SettingsPath reads (GET) and changes (PATCH) the small whitelist of
	// settings the page may edit (see Settings).
	SettingsPath = "/ui/settings"
	// maxSettingsBody caps the body of a settings change.
	maxSettingsBody = 8 << 10
)

// ErrInvalidSettings marks a SettingsStore.UpdateSettings error caused by
// the requested change rather than by reading or writing the config; it
// is answered with 400 and the error's text.
var ErrInvalidSettings = errors.New("invalid settings")

// SettingsStore reads and changes the editable settings. Implementations
// must load the config fresh from disk for every change and write back
// only the whitelisted fields, so the keys and tokens it holds survive.
type SettingsStore interface {
	Settings() (SettingsView, error)
	UpdateSettings(SettingsPatch) (SettingsView, error)
}

// Settings is the whitelist of settings the page may edit, as saved in
// the config file. It never holds key material, tokens, allow lists,
// headers, environment values or paths.
type Settings struct {
	Model    string       `json:"model"`
	Provider string       `json:"provider"`
	Tools    ToolSettings `json:"tools"`
	// Channels holds the configured built-in channels by name.
	Channels map[string]ChannelSettings `json:"channels"`
}

// ToolSettings are the editable tool switches.
type ToolSettings struct {
	Exec        bool   `json:"exec"`
	WebFetch    bool   `json:"web_fetch"`
	ExecSandbox string `json:"exec_sandbox"`
}

// ChannelSettings is one channel's editable setting.
type ChannelSettings struct {
	Enabled bool `json:"enabled"`
}

// SettingsView is the body of GET and PATCH /ui/settings.
type SettingsView struct {
	Settings Settings `json:"settings"`
	// RestartRequired lists the settings, as dotted paths such as
	// "tools.exec", whose saved value differs from the running gateway's.
	// Settings take effect only when the gateway restarts.
	RestartRequired []string `json:"restart_required"`
	// EnvOverrides lists the settings an environment variable overrides,
	// so their saved value is not used while it is set.
	EnvOverrides []string        `json:"env_overrides"`
	Choices      SettingsChoices `json:"choices"`
}

// SettingsChoices are the values the page offers for the settings that
// take one of a fixed set.
type SettingsChoices struct {
	Provider    []string `json:"provider"`
	ExecSandbox []string `json:"exec_sandbox"`
}

// SettingsPatch is a decoded PATCH /ui/settings body: nil fields are left
// unchanged.
type SettingsPatch struct {
	Model    *string                 `json:"model"`
	Provider *string                 `json:"provider"`
	Tools    *ToolSettingsPatch      `json:"tools"`
	Channels map[string]ChannelPatch `json:"channels"`
}

// ToolSettingsPatch changes ToolSettings.
type ToolSettingsPatch struct {
	Exec        *bool   `json:"exec"`
	WebFetch    *bool   `json:"web_fetch"`
	ExecSandbox *string `json:"exec_sandbox"`
}

// ChannelPatch changes one channel's ChannelSettings.
type ChannelPatch struct {
	Enabled *bool `json:"enabled"`
}

// patchSchema is the exact shape of a settings change: objects list their
// keys, nil marks a value and "*" stands for any channel name.
var patchSchema = map[string]any{
	"model":    nil,
	"provider": nil,
	"tools":    map[string]any{"exec": nil, "web_fetch": nil, "exec_sandbox": nil},
	"channels": map[string]any{"*": map[string]any{"enabled": nil}},
}

// secretKeyRE matches key names that look like they hold a secret. They
// are refused by name, at any depth, before the whitelist is consulted.
var secretKeyRE = regexp.MustCompile(`(?i)api_?key|token|secret|passw|allow_?from|header|env|credential|auth|cookie|private`)

// ParseSettingsPatch decodes a PATCH /ui/settings body. It accepts only
// the keys of patchSchema, spelled exactly, and refuses key names that
// look secret, nulls and values of the wrong type.
func ParseSettingsPatch(data []byte) (SettingsPatch, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return SettingsPatch{}, fmt.Errorf("body must be a JSON object: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SettingsPatch{}, errors.New("body must be a single JSON object")
	}
	if err := checkKeys(tree, patchSchema, ""); err != nil {
		return SettingsPatch{}, err
	}
	var p SettingsPatch
	if err := json.Unmarshal(data, &p); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return SettingsPatch{}, fmt.Errorf("%s must be a %s", te.Field, te.Type)
		}
		return SettingsPatch{}, err
	}
	return p, nil
}

// checkKeys checks v against schema; path is v's dotted path.
func checkKeys(v any, schema any, path string) error {
	if v == nil {
		return fmt.Errorf("%s must not be null", orBody(path))
	}
	fields, ok := schema.(map[string]any)
	if !ok {
		return nil // a value; its type is checked when decoded
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a JSON object", orBody(path))
	}
	for _, key := range slices.Sorted(maps.Keys(obj)) {
		at := key
		if path != "" {
			at = path + "." + key
		}
		if secretKeyRE.MatchString(key) {
			return fmt.Errorf("%s cannot be changed here: keys, tokens and allow lists are edited in config.json only", at)
		}
		sub, ok := fields[key]
		if !ok {
			if sub, ok = fields["*"]; !ok {
				return fmt.Errorf("%s is not an editable setting", at)
			}
		}
		if err := checkKeys(obj[key], sub, at); err != nil {
			return err
		}
	}
	return nil
}

func orBody(path string) string {
	if path == "" {
		return "body"
	}
	return path
}

// serveSettings answers GET /ui/settings with the saved settings.
func (c *Channel) serveSettings(w http.ResponseWriter, _ *http.Request) {
	v, err := c.settings.Settings()
	if err != nil {
		c.log.Warn("websocket: read settings", "error", err)
		http.Error(w, "cannot read the settings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, fillSettings(v))
}

// updateSettings answers PATCH /ui/settings: it saves the whitelisted
// fields of the body and returns the settings as saved.
func (c *Channel) updateSettings(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	if err != nil {
		http.Error(w, fmt.Sprintf("body must be at most %d bytes", maxSettingsBody), http.StatusBadRequest)
		return
	}
	p, err := ParseSettingsPatch(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := c.settings.UpdateSettings(p)
	if errors.Is(err, ErrInvalidSettings) {
		http.Error(w, strings.TrimPrefix(err.Error(), ErrInvalidSettings.Error()+": "), http.StatusBadRequest)
		return
	}
	if err != nil {
		c.log.Warn("websocket: save settings", "error", err)
		http.Error(w, "cannot save the settings", http.StatusInternalServerError)
		return
	}
	c.log.Info("websocket: settings saved", "restart_required", v.RestartRequired)
	writeJSON(w, fillSettings(v))
}

// fillSettings replaces v's nil lists and maps with empty ones, so the
// JSON always has them.
func fillSettings(v SettingsView) SettingsView {
	if v.Settings.Channels == nil {
		v.Settings.Channels = map[string]ChannelSettings{}
	}
	for _, l := range []*[]string{&v.RestartRequired, &v.EnvOverrides, &v.Choices.Provider, &v.Choices.ExecSandbox} {
		if *l == nil {
			*l = []string{}
		}
	}
	return v
}
