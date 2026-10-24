package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
}

func TestDefaultModelResolvesToAnthropic(t *testing.T) {
	cfg := Default()
	if cfg.Agents.Defaults.Model != "claude-opus-4-5" {
		t.Errorf("default model = %q, want nanobot's claude-opus-4-5", cfg.Agents.Defaults.Model)
	}
	cfg.Providers.Anthropic.APIKey = "k"
	tgt, err := cfg.Resolve(cfg.Agents.Defaults.Provider, cfg.Agents.Defaults.Model)
	if err != nil || tgt.Preset.Name != "anthropic" || tgt.Model != "claude-opus-4-5" {
		t.Errorf("Resolve(default) = %+v, %v", tgt, err)
	}
	// nanobot's namespaced form resolves the same way.
	if tgt, err := cfg.Resolve(ProviderAuto, "anthropic/claude-opus-4-5"); err != nil || tgt.Model != "claude-opus-4-5" {
		t.Errorf("Resolve(anthropic/claude-opus-4-5) = %+v, %v", tgt, err)
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
	    "contextWindowTokens": 64000, "consolidation": {"enable": false}, "dream": {"batchEntries": 5},
	    "fallbackModels": [{"provider": "openai", "model": "gpt-4o"}]}},
	  "providers": {"deepseek": {"apiKey": "sk-ds"}},
	  "tools": {"exec": {"timeout": 5, "allowedEnvKeys": ["GOPATH"]}}
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
	// Unset fields keep their defaults, also inside partly set objects.
	if d.Temperature != DefaultTemperature || d.MaxToolIterations != DefaultMaxToolIterations || d.Workspace != DefaultWorkspace {
		t.Errorf("defaults lost: %+v", d)
	}
	if d.ContextWindowTokens != 64000 || d.Consolidation.Enable || d.Consolidation.SummaryMaxTokens != DefaultSummaryMaxTokens {
		t.Errorf("consolidation settings = %d %+v", d.ContextWindowTokens, d.Consolidation)
	}
	if d.Dream.BatchEntries != 5 || d.Dream.MaxHistoryEntries != DefaultMaxHistoryEntries || d.Dream.IntervalH != DefaultDreamIntervalH || !d.Dream.Scheduled() {
		t.Errorf("dream settings = %+v", d.Dream)
	}
	if !cfg.Tools.RestrictToWorkspace || !cfg.Tools.Exec.Enable || cfg.Tools.Exec.Timeout != 5 {
		t.Errorf("tools = %+v", cfg.Tools)
	}
	if !reflect.DeepEqual(cfg.Tools.Exec.AllowedEnvKeys, []string{"GOPATH"}) {
		t.Errorf("allowedEnvKeys = %q", cfg.Tools.Exec.AllowedEnvKeys)
	}
	if cfg.Providers.DeepSeek.APIKey != "sk-ds" {
		t.Errorf("deepseek key not loaded")
	}
	if want := []FallbackModel{{Provider: "openai", Model: "gpt-4o"}}; !reflect.DeepEqual(d.FallbackModels, want) {
		t.Errorf("fallbackModels = %+v, want %+v", d.FallbackModels, want)
	}
}

func TestLoadExecSandbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"tools": {"exec": {"sandbox": "seatbelt", "sandboxRoBinds": ["/opt/bin"], "sandboxRwBinds": ["/var/cache/tool"]}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Tools.Exec
	if e.Sandbox != "seatbelt" || !reflect.DeepEqual(e.SandboxRoBinds, []string{"/opt/bin"}) ||
		!reflect.DeepEqual(e.SandboxRwBinds, []string{"/var/cache/tool"}) || !e.Enable {
		t.Errorf("exec = %+v", e)
	}
	if d := Default().Tools.Exec; d.Sandbox != "" || d.SandboxRoBinds == nil || len(d.SandboxRoBinds) != 0 {
		t.Errorf("default sandbox settings = %+v", d)
	}

	// Saved configs keep the fields.
	out := filepath.Join(dir, "saved.json")
	if err := cfg.Save(out); err != nil {
		t.Fatal(err)
	}
	again, err := Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Tools.Exec, e) {
		t.Errorf("round trip = %+v, want %+v", again.Tools.Exec, e)
	}

	if err := os.WriteFile(path, []byte(`{"tools": {"exec": {"sandbox": "chroot"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), `unknown sandbox backend "chroot"`) {
		t.Errorf("Load(unknown sandbox) err = %v", err)
	}
}

func TestLoadGatewayAndChannels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"gateway": {"port": 9000}, "channels": {"telegram": {"enabled": true, "token": "x"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Gateway
	if g.Host != DefaultGatewayHost || g.Port != 9000 || g.ShutdownTimeout != DefaultGatewayShutdownTimeout {
		t.Errorf("gateway = %+v", g)
	}
	if g.Addr() != "127.0.0.1:9000" || g.ShutdownWait() != 30*time.Second {
		t.Errorf("Addr = %q, ShutdownWait = %v", g.Addr(), g.ShutdownWait())
	}
	if got := string(cfg.Channels["telegram"]); got != `{"enabled": true, "token": "x"}` {
		t.Errorf("channels.telegram = %s", got)
	}
	if d := Default().Gateway; d.Addr() != "127.0.0.1:18790" {
		t.Errorf("default gateway addr = %q", d.Addr())
	}
	if (GatewayConfig{Host: "::1", Port: 1}).Addr() != "[::1]:1" {
		t.Error("IPv6 host not bracketed")
	}
}

func TestLoadAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"api": {"enabled": true, "port": 9100, "apiKey": "k"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := APIConfig{Enabled: true, Host: DefaultAPIHost, Port: 9100, Timeout: DefaultAPITimeout, APIKey: "k"}
	if cfg.API != want {
		t.Errorf("api = %+v, want %+v", cfg.API, want)
	}
	if cfg.API.Addr() != "127.0.0.1:9100" || cfg.API.TurnTimeout() != 120*time.Second {
		t.Errorf("Addr = %q, TurnTimeout = %v", cfg.API.Addr(), cfg.API.TurnTimeout())
	}
	if d := Default().API; d.Enabled || d.Addr() != "127.0.0.1:8900" || d.APIKey != "" {
		t.Errorf("default api = %+v", d)
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
		{"trailing data", `{"tools": {"exec": {"timeout": 5}}} {"tools": {"exec": {"enable": false}}}`, "trailing data"},
		{"trailing garbage", "{}\n}", "trailing data"},
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
		{"context window", func(c *Config) { c.Agents.Defaults.ContextWindowTokens = c.Agents.Defaults.MaxTokens }, "contextWindowTokens"},
		{"summary tokens", func(c *Config) { c.Agents.Defaults.Consolidation.SummaryMaxTokens = 0 }, "summaryMaxTokens"},
		{"consolidation off", func(c *Config) { c.Agents.Defaults.Consolidation.Enable = false }, ""},
		{"dream batch", func(c *Config) { c.Agents.Defaults.Dream.BatchEntries = 0 }, "dream.batchEntries"},
		{"dream history", func(c *Config) { c.Agents.Defaults.Dream.MaxHistoryEntries = -1 }, "dream.maxHistoryEntries"},
		{"dream no trimming", func(c *Config) { c.Agents.Defaults.Dream.MaxHistoryEntries = 0 }, ""},
		{"dream interval", func(c *Config) { c.Agents.Defaults.Dream.IntervalH = -1 }, "dream.intervalH"},
		{"dream unscheduled", func(c *Config) { c.Agents.Defaults.Dream.IntervalH = 0 }, ""},
		{"dream cron", func(c *Config) { c.Agents.Defaults.Dream.Cron = "0 3 * * *" }, ""},
		{"dream bad cron", func(c *Config) { c.Agents.Defaults.Dream.Cron = "nightly" }, "dream.cron"},
		{"fallback provider", func(c *Config) {
			c.Agents.Defaults.FallbackModels = []FallbackModel{{Provider: "x", Model: "m"}}
		}, "fallbackModels[0].provider"},
		{"fallback model", func(c *Config) {
			c.Agents.Defaults.FallbackModels = []FallbackModel{{Provider: "openai"}}
		}, "fallbackModels[0].model"},
		{"exec timeout", func(c *Config) { c.Tools.Exec.Timeout = -1 }, "tools.exec.timeout"},
		{"bad pattern", func(c *Config) { c.Tools.Exec.DenyPatterns = []string{"("} }, "denyPatterns[0]"},
		// The host OS is checked when the exec tool is built, so any known
		// backend validates everywhere.
		{"sandbox bwrap", func(c *Config) { c.Tools.Exec.Sandbox = "bwrap" }, ""},
		{"sandbox seatbelt", func(c *Config) { c.Tools.Exec.Sandbox = "seatbelt" }, ""},
		{"sandbox unknown", func(c *Config) { c.Tools.Exec.Sandbox = "docker" }, "tools.exec.sandbox"},
		{"sandbox binds", func(c *Config) {
			c.Tools.Exec.SandboxRoBinds = []string{"/opt/tools", "~/.local/bin"}
			c.Tools.Exec.SandboxRwBinds = []string{"/var/cache/x"}
		}, ""},
		{"sandbox relative ro bind", func(c *Config) { c.Tools.Exec.SandboxRoBinds = []string{"bin"} }, "sandboxRoBinds[0] must be an absolute path"},
		{"sandbox empty rw bind", func(c *Config) { c.Tools.Exec.SandboxRwBinds = []string{"/x", ""} }, "sandboxRwBinds[1]"},
		{"fetch bytes", func(c *Config) { c.Tools.Web.FetchMaxBytes = -1 }, "fetchMaxBytes"},
		{"gateway host", func(c *Config) { c.Gateway.Host = " " }, "gateway.host"},
		{"gateway port high", func(c *Config) { c.Gateway.Port = 65536 }, "gateway.port"},
		{"gateway port negative", func(c *Config) { c.Gateway.Port = -1 }, "gateway.port"},
		{"gateway port any", func(c *Config) { c.Gateway.Port = 0 }, ""},
		{"gateway shutdown", func(c *Config) { c.Gateway.ShutdownTimeout = -1 }, "gateway.shutdownTimeout"},
		{"gateway no wait", func(c *Config) { c.Gateway.ShutdownTimeout = 0 }, ""},
		{"api host", func(c *Config) { c.API.Host = "" }, "api.host"},
		{"api port high", func(c *Config) { c.API.Port = 65536 }, "api.port"},
		{"api port any", func(c *Config) { c.API.Port = 0 }, ""},
		{"api timeout", func(c *Config) { c.API.Timeout = 0 }, "api.timeout"},
		{"api remote without key", func(c *Config) { c.API.Host = "0.0.0.0" }, ""}, // refused when the API starts
		{"channel object", func(c *Config) { c.Channels["telegram"] = json.RawMessage(`{"enabled": true}`) }, ""},
		{"channel not object", func(c *Config) { c.Channels["telegram"] = json.RawMessage(`true`) }, "channels.telegram must be a JSON object"},
		{"channel null", func(c *Config) { c.Channels["telegram"] = json.RawMessage(`null`) }, "channels.telegram"},
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
		"NANOBOT_GO_API_KEY":           "bearer",
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
	if cfg.API.APIKey != "bearer" {
		t.Errorf("api.apiKey = %q", cfg.API.APIKey)
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
