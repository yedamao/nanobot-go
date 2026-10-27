package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security"
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
		FallbackModels: []websocket.FallbackModel{},
		Tools: websocket.ToolSettings{Exec: true, WebFetch: true, ImageGeneration: websocket.ImageGenerationSettings{
			Provider: config.DefaultImageProvider, Model: config.DefaultImageModel, DefaultAspectRatio: config.DefaultImageAspectRatio,
			DefaultImageSize: config.DefaultImageSize, MaxImagesPerTurn: config.DefaultMaxImagesPerTurn,
		}, SSRFWhitelist: []string{}, WebSearch: websocket.WebSearchSettings{
			MaxResults: config.DefaultWebSearchResults, Timeout: config.DefaultWebSearchTimeout,
		}},
		Channels: map[string]websocket.ChannelSettings{"telegram": {}, "websocket": {Enabled: true}},
	}
	if !reflect.DeepEqual(v.Settings, want) || len(v.RestartRequired) != 0 || len(v.EnvOverrides) != 0 {
		t.Errorf("view = %+v, want settings %+v", v, want)
	}
	if !slices.Equal(v.Choices.ImageGenerationProvider, config.ImageProviders) {
		t.Errorf("image generation choices = %v", v.Choices.ImageGenerationProvider)
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
		{"fallback unknown provider", websocket.SettingsPatch{FallbackModels: &[]websocket.FallbackModel{{Provider: "acme", Model: "m"}}}},
		{"fallback empty model", websocket.SettingsPatch{FallbackModels: &[]websocket.FallbackModel{{Provider: "openai", Model: " "}}}},
		{"fallback model with spaces", websocket.SettingsPatch{FallbackModels: &[]websocket.FallbackModel{{Model: "a b"}}}},
		{"image unknown provider", imagePatch(websocket.ImageGenerationPatch{Provider: ptr("gemini")})},
		{"image auto provider", imagePatch(websocket.ImageGenerationPatch{Provider: ptr("auto")})},
		{"image empty model", imagePatch(websocket.ImageGenerationPatch{Model: ptr(" ")})},
		{"image model with spaces", imagePatch(websocket.ImageGenerationPatch{Model: ptr("a b")})},
		{"image max 0", imagePatch(websocket.ImageGenerationPatch{MaxImagesPerTurn: ptr(0)})},
		{"image max 9", imagePatch(websocket.ImageGenerationPatch{MaxImagesPerTurn: ptr(config.MaxImagesPerTurnLimit + 1)})},
		{"image empty aspect", imagePatch(websocket.ImageGenerationPatch{DefaultAspectRatio: ptr("")})},
		{"image long size", imagePatch(websocket.ImageGenerationPatch{DefaultImageSize: ptr(strings.Repeat("k", maxImageOptionLen+1))})},
		{"image size with spaces", imagePatch(websocket.ImageGenerationPatch{DefaultImageSize: ptr("1 K")})},
		{"too many fallbacks", websocket.SettingsPatch{FallbackModels: ptr(slices.Repeat([]websocket.FallbackModel{{Model: "m"}}, maxFallbackModels+1))}},
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

func imagePatch(p websocket.ImageGenerationPatch) websocket.SettingsPatch {
	return websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{ImageGeneration: &p}}
}

func TestWebSettingsImageGeneration(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningSettings(t, path, map[string]string{"NANOBOT_GO_OPENAI_API_KEY": envSecret})

	v, err := s.UpdateSettings(imagePatch(websocket.ImageGenerationPatch{
		Enabled: ptr(true), Provider: ptr(" openai "), Model: ptr(" gpt-image-1 "),
		DefaultAspectRatio: ptr("16:9"), DefaultImageSize: ptr(" 2K"), MaxImagesPerTurn: ptr(2),
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := websocket.ImageGenerationSettings{Enabled: true, Provider: "openai", Model: "gpt-image-1",
		DefaultAspectRatio: "16:9", DefaultImageSize: "2K", MaxImagesPerTurn: 2}
	wantDiff := []string{"tools.image_generation.enabled", "tools.image_generation.provider", "tools.image_generation.model",
		"tools.image_generation.default_aspect_ratio", "tools.image_generation.default_image_size", "tools.image_generation.max_images_per_turn"}
	if v.Settings.Tools.ImageGeneration != want || !slices.Equal(v.RestartRequired, wantDiff) {
		t.Errorf("after update: %+v, restart %v", v.Settings.Tools.ImageGeneration, v.RestartRequired)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ig := cfg.Tools.ImageGeneration
	if !ig.Enabled || ig.Provider != "openai" || ig.Model != "gpt-image-1" || ig.DefaultAspectRatio != "16:9" ||
		ig.DefaultImageSize != "2K" || ig.MaxImagesPerTurn != 2 || ig.SaveDir != config.DefaultImageSaveDir {
		t.Errorf("saved imageGeneration = %+v", ig)
	}
	if cfg.Providers.OpenAI.APIKey != fileSecret+"-openai" {
		t.Error("the provider's key was not kept")
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), envSecret) {
		t.Error("the saved file holds a key from the environment")
	}
	body, _ := json.Marshal(v)
	for _, leak := range []string{fileSecret, envSecret, "apiKey", "extraHeaders", "extraBody", "saveDir"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("settings JSON holds %q: %s", leak, body)
		}
	}

	// Setting the running values back leaves nothing to restart for.
	v, err = s.UpdateSettings(imagePatch(websocket.ImageGenerationPatch{
		Enabled: ptr(false), Provider: ptr(config.DefaultImageProvider), Model: ptr(config.DefaultImageModel),
		DefaultAspectRatio: ptr(config.DefaultImageAspectRatio), DefaultImageSize: ptr(config.DefaultImageSize),
		MaxImagesPerTurn: ptr(config.DefaultMaxImagesPerTurn),
	}))
	if err != nil || len(v.RestartRequired) != 0 {
		t.Errorf("revert = %+v, %v", v.RestartRequired, err)
	}
}

func ssrfPatch(list ...string) websocket.SettingsPatch {
	return websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{SSRFWhitelist: &list}}
}

func TestWebSettingsSSRFWhitelist(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningSettings(t, path, nil)

	// Without a running policy (no gateway) the change waits for a restart.
	v, err := s.UpdateSettings(ssrfPatch(" 10.19.3.17 ", "100.64.0.0/10"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.19.3.17", "100.64.0.0/10"}
	if !slices.Equal(v.Settings.Tools.SSRFWhitelist, want) || !slices.Equal(v.RestartRequired, []string{"tools.ssrf_whitelist"}) || len(v.Applied) != 0 {
		t.Errorf("after update: %v, restart %v, applied %v", v.Settings.Tools.SSRFWhitelist, v.RestartRequired, v.Applied)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Tools.SSRFWhitelist, want) || cfg.Providers.Anthropic.APIKey != fileSecret+"-anthropic" {
		t.Errorf("saved whitelist = %v", cfg.Tools.SSRFWhitelist)
	}
	if v, err = s.UpdateSettings(ssrfPatch()); err != nil || len(v.Settings.Tools.SSRFWhitelist) != 0 || len(v.RestartRequired) != 0 {
		t.Errorf("clear = %+v, %v", v, err)
	}

	for _, list := range [][]string{{"10.0.0.0/33"}, {"example.com"}, {""}, {strings.Repeat("1", maxSSRFEntryLen+1)},
		slices.Repeat([]string{"10.0.0.1"}, maxSSRFWhitelist+1)} {
		if _, err := s.UpdateSettings(ssrfPatch(list...)); !errors.Is(err, websocket.ErrInvalidSettings) {
			t.Errorf("%.40q: err = %v", list, err)
		}
	}
	if _, err := s.UpdateSettings(ssrfPatch("10.0.0.1", "bad")); err == nil || !strings.Contains(err.Error(), "tools.ssrf_whitelist[1]") {
		t.Errorf("bad entry error = %v", err)
	}
}

func TestWebSettingsWebSearch(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningSettings(t, path, nil)

	v, err := s.UpdateSettings(websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{
		WebSearch:        &websocket.WebSearchPatch{Provider: ptr(" DeepSeek "), MaxResults: ptr(12), Timeout: ptr(45)},
		WebFetchProvider: ptr("deepseek"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	ws := v.Settings.Tools.WebSearch
	if ws.Provider != "deepseek" || ws.MaxResults != 12 || ws.Timeout != 45 || v.Settings.Tools.WebFetchProvider != "deepseek" {
		t.Errorf("after update: %+v, fetch %q", ws, v.Settings.Tools.WebFetchProvider)
	}
	wantRestart := []string{"tools.web_search.provider", "tools.web_search.max_results", "tools.web_search.timeout", "tools.web_fetch_provider"}
	if !slices.Equal(v.RestartRequired, wantRestart) {
		t.Errorf("restart = %v", v.RestartRequired)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Tools.Web.Search; got.Provider != "deepseek" || got.MaxResults != 12 || got.Timeout != 45 || got.APIKey != "" ||
		cfg.Tools.Web.Fetch.Provider != "deepseek" || cfg.Providers.Anthropic.APIKey != fileSecret+"-anthropic" {
		t.Errorf("saved web = %+v", cfg.Tools.Web)
	}

	for _, p := range []websocket.WebSearchPatch{
		{Provider: ptr("brave")}, {MaxResults: ptr(0)}, {MaxResults: ptr(51)}, {Timeout: ptr(0)}, {Timeout: ptr(301)},
	} {
		if _, err := s.UpdateSettings(websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{WebSearch: &p}}); !errors.Is(err, websocket.ErrInvalidSettings) {
			t.Errorf("%+v: err = %v", p, err)
		}
	}
	if _, err := s.UpdateSettings(websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{WebFetchProvider: ptr("jina")}}); !errors.Is(err, websocket.ErrInvalidSettings) {
		t.Errorf("fetch provider jina: err = %v", err)
	}
}

func TestWebSearchKeySource(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	cfg := config.Default()
	for _, step := range []struct {
		set  func()
		want string
	}{
		{func() {}, ""},
		{func() { env["WEB_SEARCH_API_KEY"] = envSecret }, "environment"},
		{func() { env["NANOBOT_GO_DEEPSEEK_API_KEY"] = envSecret }, "providers.deepseek.apiKey"},
		{func() { cfg.Tools.Web.Search.APIKey = fileSecret }, "tools.web.search.apiKey"},
	} {
		step.set()
		if got := webSearchKeySource(cfg, getenv); got != step.want {
			t.Errorf("key source = %q, want %q", got, step.want)
		}
	}
}

func TestWebSettingsSSRFWhitelistHotApply(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningSettings(t, path, nil)
	s.ssrf = &security.Policy{}
	applied := make(chan struct{}, 4)
	s.ssrfApplied = func() { applied <- struct{}{} }
	ip := netip.MustParseAddr("10.19.3.17")
	if s.ssrf.Allow(ip) {
		t.Fatal("private address allowed before the change")
	}

	v, err := s.UpdateSettings(ssrfPatch("10.19.3.17"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Applied, []string{"tools.ssrf_whitelist"}) || len(v.RestartRequired) != 0 {
		t.Errorf("applied %v, restart %v", v.Applied, v.RestartRequired)
	}
	if !s.ssrf.Allow(ip) || s.ssrf.Allow(netip.MustParseAddr("10.19.3.18")) {
		t.Error("running policy did not take the new whitelist")
	}
	<-applied
	if v, err = s.Settings(); err != nil || len(v.RestartRequired) != 0 || len(v.Applied) != 0 {
		t.Errorf("GET after apply = %+v, %v", v, err)
	}

	// The same list again changes nothing; another change does not apply.
	if v, _ = s.UpdateSettings(ssrfPatch("10.19.3.17")); len(v.Applied) != 0 {
		t.Errorf("unchanged list applied %v", v.Applied)
	}
	if v, _ = s.UpdateSettings(websocket.SettingsPatch{Tools: &websocket.ToolSettingsPatch{Exec: ptr(false)}}); len(v.Applied) != 0 {
		t.Errorf("exec change applied %v", v.Applied)
	}
	if _, err := s.UpdateSettings(ssrfPatch("nope")); err == nil || !s.ssrf.Allow(ip) {
		t.Errorf("invalid list: err %v, running policy changed", err)
	}
	if v, _ = s.UpdateSettings(ssrfPatch()); !slices.Equal(v.Applied, []string{"tools.ssrf_whitelist"}) || s.ssrf.Allow(ip) {
		t.Errorf("clearing: applied %v, still allowed %v", v.Applied, s.ssrf.Allow(ip))
	}
	<-applied
	if len(applied) != 0 {
		t.Errorf("%d extra retries", len(applied))
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

func TestWebSettingsFallbackModels(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningSettings(t, path, map[string]string{"NANOBOT_GO_OPENAI_API_KEY": envSecret})

	v, err := s.UpdateSettings(websocket.SettingsPatch{FallbackModels: &[]websocket.FallbackModel{
		{Provider: "openai", Model: " gpt-test "},
		{Provider: "", Model: "deepseek-chat"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []websocket.FallbackModel{{Provider: "openai", Model: "gpt-test"}, {Provider: "auto", Model: "deepseek-chat"}}
	if !slices.Equal(v.Settings.FallbackModels, want) || !slices.Equal(v.RestartRequired, []string{"fallbackModels"}) {
		t.Errorf("after update: fallbacks %+v, restart %v", v.Settings.FallbackModels, v.RestartRequired)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if fbs := cfg.Agents.Defaults.FallbackModels; len(fbs) != 2 || fbs[0] != (config.FallbackModel{Provider: "openai", Model: "gpt-test"}) ||
		fbs[1] != (config.FallbackModel{Provider: "auto", Model: "deepseek-chat"}) {
		t.Errorf("saved fallbackModels = %+v", fbs)
	}
	if cfg.Providers.OpenAI.APIKey != fileSecret+"-openai" {
		t.Error("the provider's key was not kept")
	}

	// The view never carries key material.
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{fileSecret, envSecret, "apiKey", "apiBase", "example.test"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("settings JSON holds %q: %s", leak, body)
		}
	}

	// Reordering is a change; an omitted list is left alone.
	v, err = s.UpdateSettings(websocket.SettingsPatch{FallbackModels: &[]websocket.FallbackModel{want[1], want[0]}})
	if err != nil || !slices.Equal(v.Settings.FallbackModels, []websocket.FallbackModel{want[1], want[0]}) {
		t.Errorf("reorder = %+v, %v", v.Settings.FallbackModels, err)
	}
	v, err = s.UpdateSettings(websocket.SettingsPatch{Model: ptr("claude-test")})
	if err != nil || len(v.Settings.FallbackModels) != 2 {
		t.Errorf("unrelated patch = %+v, %v", v.Settings.FallbackModels, err)
	}

	// Clearing the list matches the running gateway again.
	v, err = s.UpdateSettings(websocket.SettingsPatch{FallbackModels: &[]websocket.FallbackModel{}})
	if err != nil || len(v.Settings.FallbackModels) != 0 || len(v.RestartRequired) != 0 {
		t.Errorf("clear = %+v, restart %v, %v", v.Settings.FallbackModels, v.RestartRequired, err)
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

func TestWebSettingsSaveDisabledSkills(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningSettings(t, path, map[string]string{"NANOBOT_GO_OPENAI_API_KEY": envSecret})
	if err := s.saveDisabledSkills([]string{"cron", "deploy"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents.Defaults.DisabledSkills; !slices.Equal(got, []string{"cron", "deploy"}) {
		t.Errorf("disabledSkills = %v", got)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), envSecret) || !strings.Contains(string(data), fileSecret+"-mcp") {
		t.Error("saving disabledSkills changed the config's secrets")
	}
	if cfg.Agents.Defaults.Model != "claude-test" {
		t.Errorf("model = %q", cfg.Agents.Defaults.Model)
	}
}
