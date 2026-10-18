package config

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/yedamao/nanobot-go/internal/providers"
)

// API kinds a preset can speak.
const (
	KindAnthropic = "anthropic"
	KindOpenAI    = "openai"
)

// Preset describes a known provider.
type Preset struct {
	Name string
	// Kind is KindAnthropic or KindOpenAI (OpenAI-compatible).
	Kind string
	// DefaultAPIBase is used when the config sets no apiBase; empty means
	// the client's own default.
	DefaultAPIBase string
	// ModelPrefixes select this provider in auto mode when the model name
	// starts with one of them (after an optional "<provider>/" prefix).
	ModelPrefixes []string
	// Gateway providers route any model; auto mode uses them when no direct
	// provider has a key.
	Gateway bool
}

// Presets returns the known providers in auto-detection order.
func Presets() []Preset {
	return []Preset{
		{Name: "anthropic", Kind: KindAnthropic, ModelPrefixes: []string{"claude"}},
		{Name: "openai", Kind: KindOpenAI, ModelPrefixes: []string{"gpt-", "chatgpt", "o1", "o3", "o4"}},
		{Name: "deepseek", Kind: KindOpenAI, DefaultAPIBase: "https://api.deepseek.com", ModelPrefixes: []string{"deepseek"}},
		{Name: "openrouter", Kind: KindOpenAI, DefaultAPIBase: "https://openrouter.ai/api/v1", Gateway: true},
	}
}

// LookupPreset returns the named preset.
func LookupPreset(name string) (Preset, bool) {
	for _, p := range Presets() {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// Target is a resolved provider and model.
type Target struct {
	Preset Preset
	// Model is the name sent to the provider's API.
	Model   string
	APIKey  string
	APIBase string
}

// Resolve picks the provider for a (provider, model) pair. An explicit
// provider is used as-is, stripping a matching "<provider>/" prefix from
// the model. In auto mode ("" or "auto") it tries, in order: the provider
// named by the model's "<provider>/" prefix, a provider whose model
// prefixes match, then a gateway; each only if it has an API key.
func (c *Config) Resolve(provider, model string) (Target, error) {
	if provider != "" && provider != ProviderAuto {
		p, ok := LookupPreset(provider)
		if !ok {
			return Target{}, fmt.Errorf("unknown provider %q (want one of %s)", provider, providerChoices())
		}
		return c.target(p, strings.TrimPrefix(model, p.Name+"/"))
	}

	prefix, bare, hasPrefix := strings.Cut(model, "/")
	if hasPrefix {
		if p, ok := LookupPreset(prefix); ok && c.hasKey(p) {
			return c.target(p, bare)
		}
	} else {
		bare = model
	}
	lower := strings.ToLower(bare)
	for _, p := range Presets() {
		for _, mp := range p.ModelPrefixes {
			if strings.HasPrefix(lower, mp) && c.hasKey(p) {
				return c.target(p, bare)
			}
		}
	}
	for _, p := range Presets() {
		if p.Gateway && c.hasKey(p) {
			// Gateways take namespaced names such as anthropic/claude-x.
			return c.target(p, strings.TrimPrefix(model, p.Name+"/"))
		}
	}
	return Target{}, fmt.Errorf("no provider with an API key matches model %q: set providers.<name>.apiKey "+
		"(or NANOBOT_GO_<NAME>_API_KEY) and, if needed, agents.defaults.provider", model)
}

func (c *Config) hasKey(p Preset) bool {
	return c.Providers.Get(p.Name).APIKey != ""
}

func (c *Config) target(p Preset, model string) (Target, error) {
	pc := c.Providers.Get(p.Name)
	if pc.APIKey == "" {
		return Target{}, fmt.Errorf("provider %s has no API key: set providers.%s.apiKey or %s", p.Name, p.Name, EnvKeyName(p.Name))
	}
	if model == "" {
		return Target{}, fmt.Errorf("provider %s: empty model name", p.Name)
	}
	base := pc.APIBase
	if base == "" {
		base = p.DefaultAPIBase
	}
	return Target{Preset: p, Model: model, APIKey: pc.APIKey, APIBase: base}, nil
}

// ProviderOptions tunes the clients built by BuildProvider.
type ProviderOptions struct {
	// HTTPClient is shared by every provider; nil uses http.DefaultClient.
	HTTPClient *http.Client
	Retry      providers.RetryPolicy
}

// BuildProvider builds the primary provider from agents.defaults and wraps
// it in a FallbackProvider when fallbackModels are configured. Each
// provider has its resolved model as default, so callers should leave
// ChatRequest.Model empty. It also returns the primary target.
func (c *Config) BuildProvider(opts ProviderOptions) (providers.Provider, Target, error) {
	d := c.Agents.Defaults
	primary, err := c.Resolve(d.Provider, d.Model)
	if err != nil {
		return nil, Target{}, fmt.Errorf("build provider: %w", err)
	}
	p := c.newProvider(primary, opts)
	if len(d.FallbackModels) == 0 {
		return p, primary, nil
	}
	fbs := make([]providers.Fallback, 0, len(d.FallbackModels))
	for i, fm := range d.FallbackModels {
		t, err := c.Resolve(fm.Provider, fm.Model)
		if err != nil {
			return nil, Target{}, fmt.Errorf("build provider: fallbackModels[%d]: %w", i, err)
		}
		fbs = append(fbs, providers.Fallback{Provider: c.newProvider(t, opts), Model: t.Model})
	}
	return providers.NewFallback(p, fbs), primary, nil
}

func (c *Config) newProvider(t Target, opts ProviderOptions) providers.Provider {
	maxTokens := c.Agents.Defaults.MaxTokens
	if t.Preset.Kind == KindAnthropic {
		return providers.NewAnthropic(providers.AnthropicConfig{
			APIKey:     t.APIKey,
			BaseURL:    t.APIBase,
			Model:      t.Model,
			MaxTokens:  maxTokens,
			HTTPClient: opts.HTTPClient,
			Retry:      opts.Retry,
		})
	}
	return providers.NewOpenAI(providers.OpenAIConfig{
		Name:       t.Preset.Name,
		APIKey:     t.APIKey,
		BaseURL:    t.APIBase,
		Model:      t.Model,
		MaxTokens:  maxTokens,
		HTTPClient: opts.HTTPClient,
		Retry:      opts.Retry,
	})
}
