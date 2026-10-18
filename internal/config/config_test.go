package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("Load(missing) = %+v, want defaults", cfg)
	}
}

func TestLoadMergesOverDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
	  "agents": {"defaults": {"model": "deepseek-chat", "maxTokens": 1024,
	    "fallbackModels": [{"provider": "openai", "model": "gpt-4o"}]}},
	  "providers": {"deepseek": {"apiKey": "sk-ds"}},
	  "tools": {"exec": {"timeout": 5}}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Agents.Defaults
	if d.Model != "deepseek-chat" || d.MaxTokens != 1024 {
		t.Errorf("model/maxTokens = %q/%d", d.Model, d.MaxTokens)
	}
	// Unset fields keep their defaults.
	if d.Temperature != DefaultTemperature || d.MaxToolIterations != DefaultMaxToolIterations || d.Workspace != DefaultWorkspace {
		t.Errorf("defaults lost: %+v", d)
	}
	if !cfg.Tools.RestrictToWorkspace || !cfg.Tools.Exec.Enable || cfg.Tools.Exec.Timeout != 5 {
		t.Errorf("tools = %+v", cfg.Tools)
	}
	if cfg.Providers.DeepSeek.APIKey != "sk-ds" {
		t.Errorf("deepseek key not loaded")
	}
	if want := []FallbackModel{{Provider: "openai", Model: "gpt-4o"}}; !reflect.DeepEqual(d.FallbackModels, want) {
		t.Errorf("fallbackModels = %+v, want %+v", d.FallbackModels, want)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, data, want string
	}{
		{"bad json", `{`, "unexpected EOF"},
		{"unknown key", `{"providers": {"anthropic": {"api_key": "x"}}}`, `unknown field "api_key"`},
		{"invalid value", `{"agents": {"defaults": {"maxTokens": 0}}}`, "maxTokens must be positive"},
		{"unknown provider", `{"agents": {"defaults": {"provider": "nope"}}}`, `provider "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string // empty means valid
	}{
		{"ok", func(*Config) {}, ""},
		{"empty model", func(c *Config) { c.Agents.Defaults.Model = " " }, "model must not be empty"},
		{"empty workspace", func(c *Config) { c.Agents.Defaults.Workspace = "" }, "workspace must not be empty"},
		{"temperature", func(c *Config) { c.Agents.Defaults.Temperature = 3 }, "temperature"},
		{"iterations", func(c *Config) { c.Agents.Defaults.MaxToolIterations = 0 }, "maxToolIterations"},
		{"history", func(c *Config) { c.Agents.Defaults.MaxHistoryMessages = -1 }, "maxHistoryMessages"},
		{"fallback provider", func(c *Config) {
			c.Agents.Defaults.FallbackModels = []FallbackModel{{Provider: "x", Model: "m"}}
		}, "fallbackModels[0].provider"},
		{"fallback model", func(c *Config) {
			c.Agents.Defaults.FallbackModels = []FallbackModel{{Provider: "openai"}}
		}, "fallbackModels[0].model"},
		{"exec timeout", func(c *Config) { c.Tools.Exec.Timeout = -1 }, "tools.exec.timeout"},
		{"bad pattern", func(c *Config) { c.Tools.Exec.DenyPatterns = []string{"("} }, "denyPatterns[0]"},
		{"fetch bytes", func(c *Config) { c.Tools.Web.FetchMaxBytes = -1 }, "fetchMaxBytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")
	cfg := Default()
	cfg.Providers.Anthropic.APIKey = "sk-ant"
	cfg.Agents.Defaults.FallbackModels = []FallbackModel{{Provider: "openrouter", Model: "anthropic/claude-x"}}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("found %d files next to config, want 1", len(entries))
	}
}

func TestApplyEnv(t *testing.T) {
	env := map[string]string{
		"NANOBOT_GO_MODEL":             "gpt-4o",
		"NANOBOT_GO_PROVIDER":          "openai",
		"NANOBOT_GO_WORKSPACE":         "/tmp/ws",
		"NANOBOT_GO_OPENAI_API_KEY":    "sk-oa",
		"NANOBOT_GO_OPENAI_API_BASE":   "http://localhost:1234/v1",
		"NANOBOT_GO_ANTHROPIC_API_KEY": "",
		// Must not be read: it belongs to other tools.
		"ANTHROPIC_API_KEY": "sk-should-not-be-used",
	}
	cfg := Default()
	cfg.Providers.Anthropic.APIKey = "from-file"
	cfg.ApplyEnv(func(k string) string { return env[k] })

	d := cfg.Agents.Defaults
	if d.Model != "gpt-4o" || d.Provider != "openai" || d.Workspace != "/tmp/ws" {
		t.Errorf("agent defaults = %+v", d)
	}
	if cfg.Providers.OpenAI.APIKey != "sk-oa" || cfg.Providers.OpenAI.APIBase != "http://localhost:1234/v1" {
		t.Errorf("openai = %+v", cfg.Providers.OpenAI)
	}
	if cfg.Providers.Anthropic.APIKey != "from-file" {
		t.Errorf("empty env var or ANTHROPIC_API_KEY overrode the file: %q", cfg.Providers.Anthropic.APIKey)
	}
}

func TestResolvePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	tests := []struct {
		name, flag, env, want string
	}{
		{"default", "", "", filepath.Join(home, ".nanobot-go", "config.json")},
		{"env", "", "/etc/nb.json", "/etc/nb.json"},
		{"flag wins", "/a/b.json", "/etc/nb.json", "/a/b.json"},
		{"tilde", "~/x/c.json", "", filepath.Join(home, "x", "c.json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePath(tt.flag, func(k string) string {
				if k == EnvConfigPath {
					return tt.env
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("ResolvePath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkspacePathAndSessionsDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got, err := Default().WorkspacePath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".nanobot-go", "workspace"); got != want {
		t.Errorf("WorkspacePath = %q, want %q", got, want)
	}
	if got, want := SessionsDir("/x/y/config.json"), "/x/y/sessions"; got != want {
		t.Errorf("SessionsDir = %q, want %q", got, want)
	}
}
