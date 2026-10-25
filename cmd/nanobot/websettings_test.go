package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

const (
	fileSecret = "sk-file-secret-value"
	envSecret  = "sk-env-secret-value"
)

// writeSettingsConfig writes a config file holding secrets of every kind
// to a temporary directory and returns its path.
func writeSettingsConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
  "agents": {"defaults": {"workspace": ` + jsonString(filepath.Join(dir, "workspace")) + `, "model": "claude-test", "provider": "anthropic"}},
  "providers": {"anthropic": {"apiKey": "` + fileSecret + `-anthropic"}, "openai": {"apiKey": "` + fileSecret + `-openai", "apiBase": "https://example.test/v1"}},
  "api": {"enabled": false, "apiKey": "` + fileSecret + `-api"},
  "tools": {
    "exec": {"enable": true, "denyPatterns": ["rm -rf"]},
    "web": {"enable": true},
    "mcpServers": {"srv": {"url": "https://mcp.example.test", "headers": {"Authorization": "Bearer ` + fileSecret + `-mcp"}}}
  },
  "channels": {
    "telegram": {"token": "` + fileSecret + `-telegram", "allowFrom": ["12345"], "enabled": false},
    "websocket": {"enabled": true},
    "custom": {"enabled": true, "token": "` + fileSecret + `-custom"}
  }
}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// runningSettings returns a store for path whose running config is the
// file with the environment applied, as the gateway has it.
func runningSettings(t *testing.T, path string, env map[string]string) *webSettings {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return env[k] }
	cfg.ApplyEnv(getenv)
	return newWebSettings(path, cfg, getenv)
}

func ptr[T any](v T) *T { return &v }

func TestWebSettingsRoundTrip(t *testing.T) {
	path := writeSettingsConfig(t)
	before, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := runningSettings(t, path, map[string]string{"NANOBOT_GO_OPENAI_API_KEY": envSecret, "NANOBOT_GO_API_KEY": envSecret})

	v, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	want := websocket.Settings{
		Model: "claude-test", Provider: "anthropic",
		Tools:    websocket.ToolSettings{Exec: true, WebFetch: true},
		Channels: map[string]websocket.ChannelSettings{"telegram": {}, "websocket": {Enabled: true}},
	}
	if !reflect.DeepEqual(v.Settings, want) || len(v.RestartRequired) != 0 || len(v.EnvOverrides) != 0 {
		t.Errorf("view = %+v, want settings %+v", v, want)
	}
	if !slices.Contains(v.Choices.Provider, "auto") || !slices.Contains(v.Choices.Provider, "anthropic") || v.Choices.ExecSandbox[0] != "" {
		t.Errorf("choices = %+v", v.Choices)
	}

	v, err = s.UpdateSettings(websocket.SettingsPatch{
		Model:    ptr(" gpt-test "),
		Provider: ptr("openai"),
		Tools:    &websocket.ToolSettingsPatch{Exec: ptr(false), WebFetch: ptr(false)},
		Channels: map[string]websocket.ChannelPatch{"telegram": {Enabled: ptr(true)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantDiff := []string{"model", "provider", "tools.exec", "tools.web_fetch", "channels.telegram.enabled"}
	if v.Settings.Model != "gpt-test" || v.Settings.Provider != "openai" || v.Settings.Tools.Exec ||
		!v.Settings.Channels["telegram"].Enabled || !slices.Equal(v.RestartRequired, wantDiff) {
		t.Errorf("after update: %+v", v)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), envSecret) {
		t.Error("the saved file holds a key from the environment")
	}
	for _, kept := range []string{"-anthropic", "-openai", "-api", "-mcp", "-telegram", "-custom"} {
		if !strings.Contains(string(data), fileSecret+kept) {
			t.Errorf("the saved file lost the secret %q", fileSecret+kept)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}

	// Everything but the patched fields is unchanged.
	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var tg map[string]any
	if err := json.Unmarshal(after.Channels["telegram"], &tg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tg, map[string]any{"token": fileSecret + "-telegram", "allowFrom": []any{"12345"}, "enabled": true}) {
		t.Errorf("telegram section = %v", tg)
	}
	exp := before
	exp.Agents.Defaults.Model, exp.Agents.Defaults.Provider = "gpt-test", "openai"
	exp.Tools.Exec.Enable, exp.Tools.Web.Enable = false, false
	exp.Channels["telegram"] = after.Channels["telegram"]
	for name, raw := range exp.Channels {
		if name != "telegram" && compactJSON(t, after.Channels[name]) != compactJSON(t, raw) {
			t.Errorf("channels.%s = %s, want %s", name, after.Channels[name], raw)
		}
		exp.Channels[name] = after.Channels[name]
	}
	if !reflect.DeepEqual(after, exp) {
		t.Errorf("config after update =\n%+v\nwant\n%+v", after, exp)
	}

	// Setting the running values back leaves nothing to restart for.
	v, err = s.UpdateSettings(websocket.SettingsPatch{
		Model: ptr("claude-test"), Provider: ptr("anthropic"),
		Tools:    &websocket.ToolSettingsPatch{Exec: ptr(true), WebFetch: ptr(true)},
		Channels: map[string]websocket.ChannelPatch{"telegram": {Enabled: ptr(false)}},
	})
	if err != nil || len(v.RestartRequired) != 0 {
		t.Errorf("revert = %+v, %v", v.RestartRequired, err)
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestWebSettingsRefusals(t *testing.T) {
	other := "bwrap"
	if runtime.GOOS == "linux" {
		other = "seatbelt"
	}
	for _, tt := range []struct {
		name  string
		patch websocket.SettingsPatch
	}{
		{"empty model", websocket.SettingsPatch{Model: ptr("  ")}},
		{"model with spaces", websocket.SettingsPatch{Model: ptr("a b")}},
		{"long model", websocket.SettingsPatch{Model: ptr(strings.Repeat("m", maxModelLen+1))}},
		{"unknown provider", websocket.SettingsPatch{Provider: ptr("acme")}},
		{"unknown sandbox", websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{ExecSandbox: ptr("docker")}}},
		{"other system's sandbox", websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{ExecSandbox: ptr(other)}}},
		{"unknown channel", websocket.SettingsPatch{Channels: map[string]websocket.ChannelPatch{"slack": {Enabled: ptr(true)}}}},
		{"custom channel", websocket.SettingsPatch{Channels: map[string]websocket.ChannelPatch{"custom": {Enabled: ptr(false)}}}},
		{"unconfigured channel", websocket.SettingsPatch{Channels: map[string]websocket.ChannelPatch{"weixin": {Enabled: ptr(true)}}}},
		{"valid then invalid", websocket.SettingsPatch{Model: ptr("ok"), Provider: ptr("acme")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeSettingsConfig(t)
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s := runningSettings(t, path, nil)
			if _, err := s.UpdateSettings(tt.patch); !errors.Is(err, websocket.ErrInvalidSettings) {
				t.Errorf("UpdateSettings = %v, want ErrInvalidSettings", err)
			}
			if data, _ := os.ReadFile(path); string(data) != string(orig) {
				t.Error("a refused change rewrote the config file")
			}
		})
	}
}

func TestWebSettingsSandboxAndProvider(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningSettings(t, path, map[string]string{"NANOBOT_GO_MODEL": "env-model"})
	choices := sandboxChoices()
	for _, name := range choices {
		v, err := s.UpdateSettings(websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{ExecSandbox: ptr(name)}})
		if err != nil || v.Settings.Tools.ExecSandbox != name {
			t.Errorf("sandbox %q: %+v, %v", name, v.Settings.Tools, err)
		}
	}
	v, err := s.UpdateSettings(websocket.SettingsPatch{Provider: ptr("")})
	if err != nil || v.Settings.Provider != "auto" {
		t.Errorf("empty provider = %q, %v", v.Settings.Provider, err)
	}
	// The environment's model is the running one, so the saved one differs.
	if !slices.Equal(v.EnvOverrides, []string{"model"}) || !slices.Contains(v.RestartRequired, "model") {
		t.Errorf("env overrides = %v, restart = %v", v.EnvOverrides, v.RestartRequired)
	}
}

func TestSetEnabled(t *testing.T) {
	raw := json.RawMessage(`{"Enabled": false, "token": "t", "nested": {"a": [1, 2]}}`)
	got, err := setEnabled(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"enabled":true,"nested":{"a":[1,2]},"token":"t"}` {
		t.Errorf("setEnabled = %s", got)
	}
	for _, bad := range []string{`[]`, `null`, `"x"`} {
		if _, err := setEnabled(json.RawMessage(bad), true); err == nil {
			t.Errorf("setEnabled(%s) succeeded", bad)
		}
	}
}

func TestWebSettingsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newWebSettings(path, config.Default(), func(string) string { return "" })
	if _, err := s.UpdateSettings(websocket.SettingsPatch{Model: ptr("m")}); err == nil || errors.Is(err, websocket.ErrInvalidSettings) {
		t.Errorf("unreadable config: %v, want a non-validation error", err)
	}
	if data, _ := os.ReadFile(path); string(data) != `{not json` {
		t.Error("an unreadable config was overwritten")
	}
}
