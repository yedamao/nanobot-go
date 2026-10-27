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
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/security/sandbox"
)

// maxModelLen caps a model name set from the WebUI.
const maxModelLen = 200

// maxImageOptionLen caps tools.imageGeneration's default aspect ratio
// and image size set from the WebUI.
const maxImageOptionLen = 32

// maxFallbackModels caps agents.defaults.fallbackModels set from the
// WebUI.
const maxFallbackModels = 16

// maxSSRFWhitelist caps tools.ssrfWhitelist set from the WebUI; with
// entries of at most maxSSRFEntryLen bytes the change fits in the
// settings body.
const (
	maxSSRFWhitelist = 64
	maxSSRFEntryLen  = 64
)

// ssrfWhitelistSetting is tools.ssrfWhitelist's dotted settings path.
const ssrfWhitelistSetting = "tools.ssrf_whitelist"

// editableChannels are the channels whose "enabled" flag the WebUI may
// set, and only when their section is already in the config file: a
// section added from the page would lack its token.
var editableChannels = []string{telegram.Name, websocket.Name, weixin.Name}

// webSettings is the WebUI's settings store (GET and PATCH /ui/settings).
// Every change loads the config file afresh, sets only the whitelisted
// fields on it in place and saves it, so keys, tokens and every other
// setting in the file survive; the running config, which has the
// environment applied, is never saved. Changes take effect on the next
// start, the model, provider and fallbackModels included (the provider
// chain is built once when the gateway starts): running holds the
// settings the gateway runs with, to report which saved ones differ.
// The SSRF whitelist is the exception: with ssrf set it is applied to the
// running gateway as soon as it is saved.
type webSettings struct {
	path    string
	running websocket.Settings
	envSet  []string
	// ssrf, when set, is the running gateway's SSRF policy, shared by
	// web_fetch, MCP HTTP servers and the channels' media downloads.
	ssrf *security.Policy
	// ssrfApplied, when set, runs once a changed whitelist was applied,
	// without the lock held: the gateway retries the MCP servers that are
	// not connected, which it may have refused before.
	ssrfApplied func()

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
	var applied []string
	if p.Tools != nil && p.Tools.SSRFWhitelist != nil && s.applySSRF(cfg.Tools.SSRFWhitelist) {
		applied = append(applied, ssrfWhitelistSetting)
		if s.ssrfApplied != nil {
			go s.ssrfApplied()
		}
	}
	v := s.view(cfg)
	v.Applied = applied
	return v, nil
}

// applySSRF applies the saved whitelist list to the running policy, if
// there is one and list differs from the running list, and reports
// whether it did. The caller holds s.mu.
func (s *webSettings) applySSRF(list []string) bool {
	if s.ssrf == nil || slices.Equal(list, s.running.Tools.SSRFWhitelist) {
		return false
	}
	wl, err := security.ParseWhitelist(list)
	if err != nil { // applySettings and Validate checked it
		return false
	}
	s.ssrf.SetWhitelist(wl)
	s.running.Tools.SSRFWhitelist = slices.Clone(list)
	return true
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
		Choices: websocket.SettingsChoices{Provider: providerChoices(), ExecSandbox: sandboxChoices(),
			ImageGenerationProvider: slices.Clone(config.ImageProviders)},
	}
}

// settingsOf reads the whitelisted settings from cfg. Channel sections are
// read for their "enabled" flag only.
func settingsOf(cfg *config.Config) websocket.Settings {
	d := cfg.Agents.Defaults
	ig := cfg.Tools.ImageGeneration
	st := websocket.Settings{
		Model:          d.Model,
		Provider:       d.Provider,
		FallbackModels: make([]websocket.FallbackModel, 0, len(d.FallbackModels)),
		Tools: websocket.ToolSettings{
			Exec:        cfg.Tools.Exec.Enable,
			WebFetch:    cfg.Tools.Web.Enable,
			ExecSandbox: cfg.Tools.Exec.Sandbox,
			ImageGeneration: websocket.ImageGenerationSettings{
				Enabled:            ig.Enabled,
				Provider:           ig.Provider,
				Model:              ig.Model,
				DefaultAspectRatio: ig.DefaultAspectRatio,
				DefaultImageSize:   ig.DefaultImageSize,
				MaxImagesPerTurn:   ig.MaxImagesPerTurn,
			},
			SSRFWhitelist: append([]string{}, cfg.Tools.SSRFWhitelist...),
		},
		Channels: map[string]websocket.ChannelSettings{},
	}
	for _, fb := range d.FallbackModels {
		st.FallbackModels = append(st.FallbackModels, websocket.FallbackModel{Provider: fb.Provider, Model: fb.Model})
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
	add("fallbackModels", !slices.Equal(saved.FallbackModels, running.FallbackModels))
	add("tools.exec", saved.Tools.Exec != running.Tools.Exec)
	add("tools.web_fetch", saved.Tools.WebFetch != running.Tools.WebFetch)
	add("tools.exec_sandbox", saved.Tools.ExecSandbox != running.Tools.ExecSandbox)
	si, ri := saved.Tools.ImageGeneration, running.Tools.ImageGeneration
	add("tools.image_generation.enabled", si.Enabled != ri.Enabled)
	add("tools.image_generation.provider", si.Provider != ri.Provider)
	add("tools.image_generation.model", si.Model != ri.Model)
	add("tools.image_generation.default_aspect_ratio", si.DefaultAspectRatio != ri.DefaultAspectRatio)
	add("tools.image_generation.default_image_size", si.DefaultImageSize != ri.DefaultImageSize)
	add("tools.image_generation.max_images_per_turn", si.MaxImagesPerTurn != ri.MaxImagesPerTurn)
	add(ssrfWhitelistSetting, !slices.Equal(saved.Tools.SSRFWhitelist, running.Tools.SSRFWhitelist))
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
		m, err := checkModel("model", *p.Model)
		if err != nil {
			return err
		}
		d.Model = m
	}
	if p.Provider != nil {
		name, err := checkProvider("provider", *p.Provider)
		if err != nil {
			return err
		}
		d.Provider = name
	}
	if p.FallbackModels != nil {
		list := *p.FallbackModels
		if len(list) > maxFallbackModels {
			return fmt.Errorf("fallbackModels must have at most %d entries", maxFallbackModels)
		}
		fbs := make([]config.FallbackModel, 0, len(list))
		for i, fb := range list {
			at := fmt.Sprintf("fallbackModels[%d]", i)
			name, err := checkProvider(at+".provider", fb.Provider)
			if err != nil {
				return err
			}
			m, err := checkModel(at+".model", fb.Model)
			if err != nil {
				return err
			}
			fbs = append(fbs, config.FallbackModel{Provider: name, Model: m})
		}
		d.FallbackModels = fbs
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
		if t.ImageGeneration != nil {
			if err := applyImageGeneration(&cfg.Tools.ImageGeneration, *t.ImageGeneration); err != nil {
				return err
			}
		}
		if t.SSRFWhitelist != nil {
			list, err := checkSSRFWhitelist(*t.SSRFWhitelist)
			if err != nil {
				return err
			}
			cfg.Tools.SSRFWhitelist = list
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

// applyImageGeneration sets p's fields on ig. Validate checks that an
// enabled tool has a provider and model.
func applyImageGeneration(ig *config.ImageGenerationConfig, p websocket.ImageGenerationPatch) error {
	const at = "tools.image_generation."
	if p.Enabled != nil {
		ig.Enabled = *p.Enabled
	}
	if p.Provider != nil {
		name := strings.TrimSpace(*p.Provider)
		if !slices.Contains(config.ImageProviders, name) {
			return fmt.Errorf("%sprovider %q is not one of %s", at, name, strings.Join(config.ImageProviders, ", "))
		}
		ig.Provider = name
	}
	if p.Model != nil {
		m, err := checkModel(at+"model", *p.Model)
		if err != nil {
			return err
		}
		ig.Model = m
	}
	for _, f := range []struct {
		name string
		val  *string
		dst  *string
	}{
		{"default_aspect_ratio", p.DefaultAspectRatio, &ig.DefaultAspectRatio},
		{"default_image_size", p.DefaultImageSize, &ig.DefaultImageSize},
	} {
		if f.val == nil {
			continue
		}
		v, err := checkWord(at+f.name, *f.val, maxImageOptionLen)
		if err != nil {
			return err
		}
		*f.dst = v
	}
	if p.MaxImagesPerTurn != nil {
		n := *p.MaxImagesPerTurn
		if n < 1 || n > config.MaxImagesPerTurnLimit {
			return fmt.Errorf("%smax_images_per_turn must be in [1, %d], got %d", at, config.MaxImagesPerTurnLimit, n)
		}
		ig.MaxImagesPerTurn = n
	}
	return nil
}

// checkSSRFWhitelist returns the whitelist list with its entries
// trimmed, or an error when it is too long or an entry is not an IP
// address or CIDR range (security.ParseWhitelist, as config.Load checks
// it).
func checkSSRFWhitelist(list []string) ([]string, error) {
	if len(list) > maxSSRFWhitelist {
		return nil, fmt.Errorf("%s must have at most %d entries", ssrfWhitelistSetting, maxSSRFWhitelist)
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		e = strings.TrimSpace(e)
		if len(e) > maxSSRFEntryLen {
			return nil, fmt.Errorf("%s[%d] must be at most %d bytes", ssrfWhitelistSetting, i, maxSSRFEntryLen)
		}
		out = append(out, e)
	}
	if _, err := security.ParseWhitelist(out); err != nil {
		return nil, fmt.Errorf("%s%v", ssrfWhitelistSetting, err)
	}
	return out, nil
}

// checkModel returns the model name m, trimmed, or an error naming it
// as field.
func checkModel(field, m string) (string, error) {
	return checkWord(field, m, maxModelLen)
}

// checkWord returns s, trimmed, or an error naming it as field when it
// is empty, longer than max bytes or holds spaces or control characters.
func checkWord(field, s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", fmt.Errorf("%s must not be empty", field)
	case len(s) > max:
		return "", fmt.Errorf("%s must be at most %d bytes", field, max)
	case strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return "", fmt.Errorf("%s must not contain spaces or control characters", field)
	}
	return s, nil
}

// checkProvider returns the provider setting name, trimmed, with empty
// meaning "auto", or an error naming it as field.
func checkProvider(field, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = config.ProviderAuto
	}
	if !slices.Contains(providerChoices(), name) {
		return "", fmt.Errorf("%s %q is not one of %s", field, name, strings.Join(providerChoices(), ", "))
	}
	return name, nil
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
