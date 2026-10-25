package main

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

const (
	// maxAPIKeyLen caps an apiKey set from the WebUI.
	maxAPIKeyLen = 4096
	// maxAPIBaseLen caps an apiBase set from the WebUI.
	maxAPIBaseLen = 2048
	// hintMask stands for the hidden middle of an apiKey hint.
	hintMask = "••••"
)

// webProviders is the WebUI's provider credentials store (/ui/providers).
// Like webSettings it loads the config file afresh for every change, sets
// only providers.<name>.apiKey/apiBase on it and saves it, under the
// settings' lock so the stores never save over each other's change. It
// never returns a key, only whether one is saved and a hint of it.
// Provider clients are built when the gateway starts: running holds the
// credentials it started with (environment applied), to report which
// providers need a restart for a saved change to take effect.
type webProviders struct {
	settings *webSettings
	running  config.ProvidersConfig
	getenv   func(string) string
}

// newWebProviders returns the providers store sharing settings' config
// file and lock. running is the gateway's providers config.
func newWebProviders(settings *webSettings, running config.ProvidersConfig, getenv func(string) string) *webProviders {
	return &webProviders{settings: settings, running: running, getenv: getenv}
}

// Providers implements websocket.ProvidersStore.
func (s *webProviders) Providers() (websocket.ProvidersList, error) {
	s.settings.mu.Lock()
	cfg, err := config.Load(s.settings.path)
	s.settings.mu.Unlock()
	if err != nil {
		return websocket.ProvidersList{}, err
	}
	out := websocket.ProvidersList{Providers: []websocket.ProviderView{}}
	for _, p := range config.Presets() {
		out.Providers = append(out.Providers, s.view(p, cfg))
	}
	return out, nil
}

// UpdateProvider implements websocket.ProvidersStore.
func (s *webProviders) UpdateProvider(name string, p websocket.ProviderPatch) (websocket.ProviderView, error) {
	preset, ok := config.LookupPreset(name)
	if !ok {
		return websocket.ProviderView{}, websocket.ErrProviderNotFound
	}
	var key, base *string
	switch {
	case p.ClearAPIKey:
		key = new(string)
	case p.APIKey != nil:
		k, err := checkAPIKey(*p.APIKey)
		if err != nil {
			return websocket.ProviderView{}, fmt.Errorf("%w: %v", websocket.ErrInvalidProvider, err)
		}
		key = &k
	}
	if p.APIBase != nil {
		b, err := checkAPIBase(*p.APIBase)
		if err != nil {
			return websocket.ProviderView{}, fmt.Errorf("%w: %v", websocket.ErrInvalidProvider, err)
		}
		base = &b
	}

	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	cfg, err := config.Load(s.settings.path)
	if err != nil {
		return websocket.ProviderView{}, err
	}
	pc := cfg.Providers.Get(name)
	if key != nil {
		pc.APIKey = *key
	}
	if base != nil {
		pc.APIBase = *base
	}
	if err := cfg.Validate(); err != nil {
		return websocket.ProviderView{}, fmt.Errorf("%w: %v", websocket.ErrInvalidProvider, err)
	}
	if err := cfg.Save(s.settings.path); err != nil {
		return websocket.ProviderView{}, err
	}
	return s.view(preset, cfg), nil
}

// view describes preset's saved credentials without the key.
func (s *webProviders) view(p config.Preset, cfg *config.Config) websocket.ProviderView {
	saved := *cfg.Providers.Get(p.Name)
	v := websocket.ProviderView{
		Name:           p.Name,
		HasAPIKey:      saved.APIKey != "",
		APIKeyHint:     apiKeyHint(saved.APIKey),
		APIBase:        saved.APIBase,
		DefaultAPIBase: p.DefaultAPIBase,
		EnvOverrides:   []string{},
	}
	// What the gateway would run with after a restart: the saved
	// credentials with the environment applied, as at start.
	effective := saved
	up := strings.ToUpper(p.Name)
	if e := s.getenv(config.EnvKeyName(p.Name)); e != "" {
		effective.APIKey = e
		v.EnvOverrides = append(v.EnvOverrides, "apiKey")
	}
	if e := s.getenv("NANOBOT_GO_" + up + "_API_BASE"); e != "" {
		effective.APIBase = e
		v.EnvOverrides = append(v.EnvOverrides, "apiBase")
	}
	v.RestartRequired = effective != *s.running.Get(p.Name)
	return v
}

// apiKeyHint shows a key's first and last four characters, or only the
// mask for a key of 8 or fewer (as Python's mask_secret_hint).
func apiKeyHint(key string) string {
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) <= 8 {
		return hintMask
	}
	return string(r[:4]) + hintMask + string(r[len(r)-4:])
}

// checkAPIKey trims a new apiKey and rejects an empty one, a redacted
// placeholder or hint sent back, and one with spaces or control
// characters. Errors never hold the key.
func checkAPIKey(raw string) (string, error) {
	k := strings.TrimSpace(raw)
	switch {
	case k == "":
		return "", fmt.Errorf("apiKey must not be empty; send clearApiKey to remove it")
	case strings.Contains(k, redacted) || strings.Contains(k, "•"):
		return "", fmt.Errorf("apiKey is a redacted placeholder or hint; send the real key")
	case len(k) > maxAPIKeyLen:
		return "", fmt.Errorf("apiKey must be at most %d bytes", maxAPIKeyLen)
	case strings.IndexFunc(k, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "", fmt.Errorf("apiKey must not contain spaces or control characters")
	}
	return k, nil
}

// checkAPIBase trims a new apiBase and requires, unless empty, an http(s)
// URL with a host and without credentials, which the page shows in clear.
func checkAPIBase(raw string) (string, error) {
	b := strings.TrimSpace(raw)
	if b == "" {
		return "", nil
	}
	if len(b) > maxAPIBaseLen {
		return "", fmt.Errorf("apiBase must be at most %d bytes", maxAPIBaseLen)
	}
	u, err := url.Parse(b)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		strings.IndexFunc(b, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", fmt.Errorf("apiBase must be an http:// or https:// URL")
	}
	if u.User != nil {
		return "", fmt.Errorf("apiBase must not hold credentials; set apiKey instead")
	}
	return b, nil
}

var _ websocket.ProvidersStore = (*webProviders)(nil)
