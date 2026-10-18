// Package config loads, validates and saves nanobot-go's JSON config file.
// Its shape mirrors nanobot/config/schema.py where it makes sense:
//
//	{"agents": {"defaults": {...}}, "providers": {...}, "tools": {...}}
//
// Keys are camelCase like nanobot's JSON aliases. Fields missing from the
// file keep their defaults.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// Defaults.
const (
	DefaultDirName            = ".nanobot-go"
	DefaultFileName           = "config.json"
	DefaultWorkspace          = "~/" + DefaultDirName + "/workspace"
	DefaultModel              = "claude-opus-4-5" // nanobot: anthropic/claude-opus-4-5
	DefaultProvider           = ProviderAuto
	DefaultMaxTokens          = 8192
	DefaultTemperature        = 0.1
	DefaultMaxToolIterations  = 200
	DefaultMaxHistoryMessages = 100
	// DefaultContextWindowTokens matches nanobot's context_window_tokens.
	DefaultContextWindowTokens = 200_000
	DefaultSummaryMaxTokens    = 2048
	// DefaultDreamBatchEntries and DefaultMaxHistoryEntries match
	// nanobot's Dream batch and history.jsonl limits.
	DefaultDreamBatchEntries = 20
	DefaultMaxHistoryEntries = 1000
	DefaultExecTimeout       = 60
	DefaultFetchMaxBytes     = 2 << 20
)

// ProviderAuto picks the provider from the model name and configured keys.
const ProviderAuto = "auto"

// EnvConfigPath overrides the config file location.
const EnvConfigPath = "NANOBOT_GO_CONFIG"

// envPrefix prefixes every environment override. Provider keys use
// NANOBOT_GO_<PROVIDER>_API_KEY so they never clobber (or get clobbered by)
// the variables other tools read, such as ANTHROPIC_API_KEY.
const envPrefix = "NANOBOT_GO_"

// Config is the root of the config file.
type Config struct {
	Agents    AgentsConfig    `json:"agents"`
	Providers ProvidersConfig `json:"providers"`
	Tools     ToolsConfig     `json:"tools"`
}

// AgentsConfig holds agent settings.
type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

// AgentDefaults configures the agent.
type AgentDefaults struct {
	// Workspace is the agent's working directory; ~ is expanded.
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
	// Provider is a provider name (see Presets) or "auto".
	Provider    string  `json:"provider"`
	MaxTokens   int     `json:"maxTokens"`
	Temperature float64 `json:"temperature"`
	// MaxToolIterations caps model requests per turn.
	MaxToolIterations int `json:"maxToolIterations"`
	// MaxHistoryMessages caps how many past session messages are replayed
	// to the model each turn. With consolidation enabled, exceeding it
	// summarizes the oldest messages instead of dropping them.
	MaxHistoryMessages int `json:"maxHistoryMessages"`
	// ContextWindowTokens is the model's context window. Consolidation
	// runs when the estimated prompt plus maxTokens would not fit.
	ContextWindowTokens int `json:"contextWindowTokens"`
	// Consolidation configures history summaries.
	Consolidation ConsolidationConfig `json:"consolidation"`
	// Dream configures `nanobot dream`, which folds the history summaries
	// into the long-term memory files.
	Dream DreamConfig `json:"dream"`
	// FallbackModels are tried in order when the primary provider fails
	// with a failover-worthy error.
	FallbackModels []FallbackModel `json:"fallbackModels"`
}

// ConsolidationConfig configures history consolidation: summarizing old
// session messages into <workspace>/memory/history.jsonl.
type ConsolidationConfig struct {
	// Enable turns consolidation (and the /compact command) on. When off,
	// messages beyond maxHistoryMessages are simply not replayed.
	Enable bool `json:"enable"`
	// SummaryMaxTokens caps each summary.
	SummaryMaxTokens int `json:"summaryMaxTokens"`
}

// DreamConfig configures Dream: turning memory/history.jsonl entries into
// long-term memory (SOUL.md, USER.md, memory/MEMORY.md, skills/).
type DreamConfig struct {
	// BatchEntries caps the history entries one run processes.
	BatchEntries int `json:"batchEntries"`
	// MaxHistoryEntries is the size history.jsonl is trimmed to after a
	// run; entries Dream has not processed are always kept. 0 disables
	// trimming.
	MaxHistoryEntries int `json:"maxHistoryEntries"`
}

// FallbackModel names a fallback provider and model. An empty Provider
// means "auto".
type FallbackModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ProviderConfig holds credentials for one provider.
type ProviderConfig struct {
	APIKey string `json:"apiKey"`
	// APIBase overrides the provider's default endpoint.
	APIBase string `json:"apiBase"`
}

// ProvidersConfig holds per-provider credentials. Every provider except
// anthropic speaks the OpenAI-compatible chat completions API.
type ProvidersConfig struct {
	Anthropic  ProviderConfig `json:"anthropic"`
	OpenAI     ProviderConfig `json:"openai"`
	DeepSeek   ProviderConfig `json:"deepseek"`
	OpenRouter ProviderConfig `json:"openrouter"`
}

// Get returns the config for the named provider, or nil if unknown.
func (p *ProvidersConfig) Get(name string) *ProviderConfig {
	switch name {
	case "anthropic":
		return &p.Anthropic
	case "openai":
		return &p.OpenAI
	case "deepseek":
		return &p.DeepSeek
	case "openrouter":
		return &p.OpenRouter
	}
	return nil
}

// ToolsConfig configures the built-in tools.
type ToolsConfig struct {
	// RestrictToWorkspace confines file tools and the shell's paths to the
	// workspace.
	RestrictToWorkspace bool           `json:"restrictToWorkspace"`
	Exec                ExecToolConfig `json:"exec"`
	Web                 WebToolConfig  `json:"web"`
}

// ExecToolConfig configures the exec tool.
type ExecToolConfig struct {
	Enable bool `json:"enable"`
	// Timeout is the default per-command timeout in seconds.
	Timeout       int      `json:"timeout"`
	DenyPatterns  []string `json:"denyPatterns"`
	AllowPatterns []string `json:"allowPatterns"`
	// AllowedEnvKeys are environment variables passed through to commands
	// on top of a minimal base set (HOME, PATH, LANG, ...).
	AllowedEnvKeys []string `json:"allowedEnvKeys"`
}

// WebToolConfig configures the web_fetch tool.
type WebToolConfig struct {
	Enable        bool  `json:"enable"`
	FetchMaxBytes int64 `json:"fetchMaxBytes"`
}

// Default returns the default config.
func Default() *Config {
	return &Config{
		Agents: AgentsConfig{Defaults: AgentDefaults{
			Workspace:           DefaultWorkspace,
			Model:               DefaultModel,
			Provider:            DefaultProvider,
			MaxTokens:           DefaultMaxTokens,
			Temperature:         DefaultTemperature,
			MaxToolIterations:   DefaultMaxToolIterations,
			MaxHistoryMessages:  DefaultMaxHistoryMessages,
			ContextWindowTokens: DefaultContextWindowTokens,
			Consolidation:       ConsolidationConfig{Enable: true, SummaryMaxTokens: DefaultSummaryMaxTokens},
			Dream:               DreamConfig{BatchEntries: DefaultDreamBatchEntries, MaxHistoryEntries: DefaultMaxHistoryEntries},
			FallbackModels:      []FallbackModel{},
		}},
		Tools: ToolsConfig{
			RestrictToWorkspace: true,
			Exec:                ExecToolConfig{Enable: true, Timeout: DefaultExecTimeout, DenyPatterns: []string{}, AllowPatterns: []string{}, AllowedEnvKeys: []string{}},
			Web:                 WebToolConfig{Enable: true, FetchMaxBytes: DefaultFetchMaxBytes},
		},
	}
}

// DefaultPath returns ~/.nanobot-go/config.json.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config path: %w", err)
	}
	return filepath.Join(home, DefaultDirName, DefaultFileName), nil
}

// ResolvePath picks the config file: flagValue if set, else $NANOBOT_GO_CONFIG,
// else DefaultPath. ~ is expanded and the result is absolute.
func ResolvePath(flagValue string, getenv func(string) string) (string, error) {
	p := flagValue
	if p == "" && getenv != nil {
		p = getenv(EnvConfigPath)
	}
	if p == "" {
		return DefaultPath()
	}
	p, err := ExpandHome(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// SessionsDir returns the directory holding session files for a config
// file: a sessions/ directory next to it. Keeping sessions out of the
// workspace stops the agent from editing its own history with file tools.
func SessionsDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "sessions")
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	return filepath.Join(home, p[1:]), nil
}

// WorkspacePath returns the absolute workspace directory.
func (c *Config) WorkspacePath() (string, error) {
	p, err := ExpandHome(c.Agents.Defaults.Workspace)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// Load reads the config file at path over the defaults and validates it.
// A missing file yields the defaults. Environment overrides are not
// applied; see ApplyEnv.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Reject unknown keys so a typo such as "api_key" fails loudly instead
	// of silently leaving a provider without credentials. (Key matching is
	// case-insensitive, so "apikey" is accepted.)
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	// Decode stops after one value; anything else (a botched merge, a
	// second object) would otherwise be ignored silently.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("load config %s: trailing data after the JSON object", path)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes c to path atomically with mode 0600, since the file may hold
// API keys.
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	data = append(data, '\n')
	if err := fsutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// ApplyEnv overrides settings from environment variables:
//
//	NANOBOT_GO_MODEL, NANOBOT_GO_PROVIDER, NANOBOT_GO_WORKSPACE
//	NANOBOT_GO_<PROVIDER>_API_KEY, NANOBOT_GO_<PROVIDER>_API_BASE
//
// where <PROVIDER> is an upper-case preset name such as ANTHROPIC. Empty
// values are ignored. Call Validate afterwards.
func (c *Config) ApplyEnv(getenv func(string) string) {
	set := func(dst *string, name string) {
		if v := getenv(envPrefix + name); v != "" {
			*dst = v
		}
	}
	d := &c.Agents.Defaults
	set(&d.Model, "MODEL")
	set(&d.Provider, "PROVIDER")
	set(&d.Workspace, "WORKSPACE")
	for _, p := range Presets() {
		pc := c.Providers.Get(p.Name)
		up := strings.ToUpper(p.Name)
		set(&pc.APIKey, up+"_API_KEY")
		set(&pc.APIBase, up+"_API_BASE")
	}
}

// EnvKeyName returns the environment variable that overrides a provider's
// API key.
func EnvKeyName(provider string) string {
	return envPrefix + strings.ToUpper(provider) + "_API_KEY"
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	d := c.Agents.Defaults
	if strings.TrimSpace(d.Workspace) == "" {
		bad("agents.defaults.workspace must not be empty")
	}
	if strings.TrimSpace(d.Model) == "" {
		bad("agents.defaults.model must not be empty")
	}
	if !validProvider(d.Provider) {
		bad("agents.defaults.provider %q is not one of %s", d.Provider, providerChoices())
	}
	if d.MaxTokens <= 0 {
		bad("agents.defaults.maxTokens must be positive, got %d", d.MaxTokens)
	}
	if d.Temperature < 0 || d.Temperature > 2 {
		bad("agents.defaults.temperature must be in [0, 2], got %g", d.Temperature)
	}
	if d.MaxToolIterations <= 0 {
		bad("agents.defaults.maxToolIterations must be positive, got %d", d.MaxToolIterations)
	}
	if d.MaxHistoryMessages < 0 {
		bad("agents.defaults.maxHistoryMessages must not be negative, got %d", d.MaxHistoryMessages)
	}
	if d.ContextWindowTokens <= d.MaxTokens {
		bad("agents.defaults.contextWindowTokens (%d) must be larger than maxTokens (%d)", d.ContextWindowTokens, d.MaxTokens)
	}
	if d.Consolidation.SummaryMaxTokens <= 0 {
		bad("agents.defaults.consolidation.summaryMaxTokens must be positive, got %d", d.Consolidation.SummaryMaxTokens)
	}
	if d.Dream.BatchEntries <= 0 {
		bad("agents.defaults.dream.batchEntries must be positive, got %d", d.Dream.BatchEntries)
	}
	if d.Dream.MaxHistoryEntries < 0 {
		bad("agents.defaults.dream.maxHistoryEntries must not be negative, got %d", d.Dream.MaxHistoryEntries)
	}
	for i, fb := range d.FallbackModels {
		if fb.Provider != "" && !validProvider(fb.Provider) {
			bad("agents.defaults.fallbackModels[%d].provider %q is not one of %s", i, fb.Provider, providerChoices())
		}
		if strings.TrimSpace(fb.Model) == "" {
			bad("agents.defaults.fallbackModels[%d].model must not be empty", i)
		}
	}
	if c.Tools.Exec.Timeout < 0 {
		bad("tools.exec.timeout must not be negative, got %d", c.Tools.Exec.Timeout)
	}
	for _, list := range []struct {
		name     string
		patterns []string
	}{{"denyPatterns", c.Tools.Exec.DenyPatterns}, {"allowPatterns", c.Tools.Exec.AllowPatterns}} {
		for i, p := range list.patterns {
			if _, err := regexp.Compile(p); err != nil {
				bad("tools.exec.%s[%d]: %w", list.name, i, err)
			}
		}
	}
	if c.Tools.Web.FetchMaxBytes < 0 {
		bad("tools.web.fetchMaxBytes must not be negative, got %d", c.Tools.Web.FetchMaxBytes)
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}
	return nil
}

func validProvider(name string) bool {
	_, ok := LookupPreset(name)
	return name == ProviderAuto || ok
}

func providerChoices() string {
	names := []string{ProviderAuto}
	for _, p := range Presets() {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}
