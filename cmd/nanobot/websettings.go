package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/yedamao/nanobot-go/internal/channels/telegram"
	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/channels/weixin"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security/sandbox"
)

// maxModelLen caps a model name set from the WebUI.
const maxModelLen = 200

// editableChannels are the channels whose "enabled" flag the WebUI may
// set, and only when their section is already in the config file: a
// section added from the page would lack its token.
var editableChannels = []string{telegram.Name, websocket.Name, weixin.Name}

// webSettings is the WebUI's settings store (GET and PATCH /ui/settings).
// Every change loads the config file afresh, sets only the whitelisted
// fields on it in place and saves it, so keys, tokens and every other
// setting in the file survive; the running config, which has the
// environment applied, is never saved. Changes take effect on the next
// start: running holds the settings the gateway runs with, to report
// which saved ones differ.
type webSettings struct {
	path    string
	running websocket.Settings
	envSet  []string

	mu sync.Mutex
}

// newWebSettings returns the settings store for the config file at path.
// cfg is the running config; getenv tells which settings the environment
// overrides.
func newWebSettings(path string, cfg *config.Config, getenv func(string) string) *webSettings {
	s := &webSettings{path: path, running: settingsOf(cfg)}
	for _, e := range []struct{ name, env string }{{"model", "NANOBOT_GO_MODEL"}, {"provider", "NANOBOT_GO_PROVIDER"}} {
		if getenv(e.env) != "" {
			s.envSet = append(s.envSet, e.name)
		}
	}
	return s
}

// Settings implements websocket.SettingsStore.
func (s *webSettings) Settings() (websocket.SettingsView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := config.Load(s.path)
	if err != nil {
		return websocket.SettingsView{}, err
	}
	return s.view(cfg), nil
}

// UpdateSettings implements websocket.SettingsStore.
func (s *webSettings) UpdateSettings(p websocket.SettingsPatch) (websocket.SettingsView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := config.Load(s.path)
	if err != nil {
		return websocket.SettingsView{}, err
	}
	if err := applySettings(cfg, p); err != nil {
		return websocket.SettingsView{}, fmt.Errorf("%w: %v", websocket.ErrInvalidSettings, err)
	}
	if err := cfg.Validate(); err != nil {
		return websocket.SettingsView{}, fmt.Errorf("%w: %v", websocket.ErrInvalidSettings, err)
	}
	if err := cfg.Save(s.path); err != nil {
		return websocket.SettingsView{}, err
	}
	return s.view(cfg), nil
}

// saveDisabledSkills saves names as agents.defaults.disabledSkills: the
// persist function of the gateway's skills.Manager. It shares the
// settings' lock so the two never save over each other's change.
func (s *webSettings) saveDisabledSkills(names []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := config.Load(s.path)
	if err != nil {
		return err
	}
	cfg.Agents.Defaults.DisabledSkills = slices.Clone(names)
	return cfg.Save(s.path)
}

func (s *webSettings) view(cfg *config.Config) websocket.SettingsView {
	saved := settingsOf(cfg)
	return websocket.SettingsView{
		Settings:        saved,
		RestartRequired: settingsDiff(saved, s.running),
		EnvOverrides:    slices.Clone(s.envSet),
		Choices:         websocket.SettingsChoices{Provider: providerChoices(), ExecSandbox: sandboxChoices()},
	}
}

// settingsOf reads the whitelisted settings from cfg. Channel sections are
// read for their "enabled" flag only.
func settingsOf(cfg *config.Config) websocket.Settings {
	d := cfg.Agents.Defaults
	st := websocket.Settings{
		Model:    d.Model,
		Provider: d.Provider,
		Tools: websocket.ToolSettings{
			Exec:        cfg.Tools.Exec.Enable,
			WebFetch:    cfg.Tools.Web.Enable,
			ExecSandbox: cfg.Tools.Exec.Sandbox,
		},
		Channels: map[string]websocket.ChannelSettings{},
	}
	for _, name := range editableChannels {
		raw, ok := cfg.Channels[name]
		if !ok {
			continue
		}
		var head struct {
			Enabled bool `json:"enabled"`
		}
		json.Unmarshal(raw, &head)
		st.Channels[name] = websocket.ChannelSettings{Enabled: head.Enabled}
	}
	return st
}

// settingsDiff lists, as dotted paths, the settings that differ between
// saved and running.
func settingsDiff(saved, running websocket.Settings) []string {
	var diff []string
	add := func(name string, changed bool) {
		if changed {
			diff = append(diff, name)
		}
	}
	add("model", saved.Model != running.Model)
	add("provider", saved.Provider != running.Provider)
	add("tools.exec", saved.Tools.Exec != running.Tools.Exec)
	add("tools.web_fetch", saved.Tools.WebFetch != running.Tools.WebFetch)
	add("tools.exec_sandbox", saved.Tools.ExecSandbox != running.Tools.ExecSandbox)
	names := slices.Sorted(maps.Keys(saved.Channels))
	for name := range running.Channels {
		if _, ok := saved.Channels[name]; !ok {
			names = append(names, name)
		}
	}
	for _, name := range names {
		add("channels."+name+".enabled", saved.Channels[name] != running.Channels[name])
	}
	return diff
}

// applySettings sets p's fields on cfg in place. It checks each value, and
// leaves checking the whole config to Validate.
func applySettings(cfg *config.Config, p websocket.SettingsPatch) error {
	d := &cfg.Agents.Defaults
	if p.Model != nil {
		m := strings.TrimSpace(*p.Model)
		switch {
		case m == "":
			return fmt.Errorf("model must not be empty")
		case len(m) > maxModelLen:
			return fmt.Errorf("model must be at most %d bytes", maxModelLen)
		case strings.ContainsFunc(m, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
			return fmt.Errorf("model must not contain spaces or control characters")
		}
		d.Model = m
	}
	if p.Provider != nil {
		name := strings.TrimSpace(*p.Provider)
		if name == "" {
			name = config.ProviderAuto
		}
		if !slices.Contains(providerChoices(), name) {
			return fmt.Errorf("provider %q is not one of %s", name, strings.Join(providerChoices(), ", "))
		}
		d.Provider = name
	}
	if t := p.Tools; t != nil {
		if t.Exec != nil {
			cfg.Tools.Exec.Enable = *t.Exec
		}
		if t.WebFetch != nil {
			cfg.Tools.Web.Enable = *t.WebFetch
		}
		if t.ExecSandbox != nil {
			name := *t.ExecSandbox
			if !slices.Contains(sandboxChoices(), name) {
				return fmt.Errorf("tools.exec_sandbox %q is not one of %q on this host", name, sandboxChoices())
			}
			cfg.Tools.Exec.Sandbox = name
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Channels)) {
		on := p.Channels[name].Enabled
		if on == nil {
			continue
		}
		if !slices.Contains(editableChannels, name) {
			return fmt.Errorf("channels.%s is not an editable channel (editable: %s)", name, strings.Join(editableChannels, ", "))
		}
		raw, ok := cfg.Channels[name]
		if !ok {
			return fmt.Errorf("channels.%s is not configured: add its section to config.json first", name)
		}
		patched, err := setEnabled(raw, *on)
		if err != nil {
			return fmt.Errorf("channels.%s: %v", name, err)
		}
		cfg.Channels[name] = patched
	}
	return nil
}

// setEnabled returns the channel section raw with its "enabled" flag set
// to on. Every other key is kept with its value as it was.
func setEnabled(raw json.RawMessage, on bool) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("section is not a JSON object")
	}
	// Keys match case-insensitively when the section is decoded, so a
	// differently cased "Enabled" would otherwise compete with ours.
	for k := range obj {
		if strings.EqualFold(k, "enabled") {
			delete(obj, k)
		}
	}
	obj["enabled"] = json.RawMessage(fmt.Sprint(on))
	return json.Marshal(obj)
}

// providerChoices are the provider settings the WebUI offers: "auto" and
// the presets.
func providerChoices() []string {
	names := []string{config.ProviderAuto}
	for _, p := range config.Presets() {
		names = append(names, p.Name)
	}
	return names
}

// sandboxChoices are the exec sandboxes the WebUI offers: none, and the
// backends built for this operating system. Another system's backend
// would stop the gateway from starting.
func sandboxChoices() []string {
	names := []string{""}
	for _, name := range sandbox.Names() {
		if b, err := sandbox.Lookup(name); err == nil && b.GOOS() == runtime.GOOS {
			names = append(names, name)
		}
	}
	return names
}
