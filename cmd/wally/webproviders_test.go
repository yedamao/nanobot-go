package main

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

// runningProviders returns a providers store for path whose running
// credentials are the file's with env applied.
func runningProviders(t *testing.T, path string, env map[string]string) *webProviders {
	t.Helper()
	s := runningSettings(t, path, env)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return env[k] }
	cfg.ApplyEnv(getenv)
	return newWebProviders(s, cfg.Providers, getenv)
}

func providerByName(t *testing.T, list websocket.ProvidersList, name string) websocket.ProviderView {
	t.Helper()
	for _, v := range list.Providers {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no provider %q in %+v", name, list)
	return websocket.ProviderView{}
}

func TestWebProvidersListRedacts(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningProviders(t, path, map[string]string{"NANOBOT_GO_DEEPSEEK_API_BASE": "http://localhost:9/v1"})
	list, err := s.Providers()
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, list)
	var names []string
	for _, v := range list.Providers {
		names = append(names, v.Name)
		if v.RestartRequired {
			t.Errorf("%s needs a restart before any change", v.Name)
		}
	}
	if !slices.Equal(names, []string{"anthropic", "openai", "deepseek", "openrouter"}) {
		t.Errorf("names = %v", names)
	}
	// fileSecret+"-anthropic" is sk-file-secret-value-anthropic.
	if a := providerByName(t, list, "anthropic"); !a.HasAPIKey || a.APIKeyHint != "sk-f••••opic" || a.APIBase != "" {
		t.Errorf("anthropic = %+v", a)
	}
	if o := providerByName(t, list, "openai"); !o.HasAPIKey || o.APIBase != "https://example.test/v1" {
		t.Errorf("openai = %+v", o)
	}
	d := providerByName(t, list, "deepseek")
	if d.HasAPIKey || d.APIKeyHint != "" || d.DefaultAPIBase == "" || !slices.Equal(d.EnvOverrides, []string{"apiBase"}) {
		t.Errorf("deepseek = %+v", d)
	}
}

func TestWebProvidersUpdate(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningProviders(t, path, nil)

	v, err := s.UpdateProvider("anthropic", websocket.ProviderPatch{ClearAPIKey: true, APIKey: ptr("ignored-key-value")})
	if err != nil || v.HasAPIKey || v.APIKeyHint != "" || !v.RestartRequired {
		t.Fatalf("clear = %+v, %v", v, err)
	}
	const newKey = "sk-new-fixture-key-0123"
	v, err = s.UpdateProvider("deepseek", websocket.ProviderPatch{APIKey: ptr("  " + newKey + "\n"), APIBase: ptr(" https://ds.example.test/v1 ")})
	if err != nil || !v.HasAPIKey || v.APIKeyHint != "sk-n••••0123" || v.APIBase != "https://ds.example.test/v1" || !v.RestartRequired {
		t.Fatalf("set = %+v, %v", v, err)
	}
	if data, _ := json.Marshal(v); strings.Contains(string(data), newKey) {
		t.Error("view holds the new key")
	}
	// Leaving the key out keeps it; "" clears apiBase.
	if v, err = s.UpdateProvider("openai", websocket.ProviderPatch{APIBase: ptr("")}); err != nil || !v.HasAPIKey || v.APIBase != "" {
		t.Fatalf("clear base = %+v, %v", v, err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.Anthropic.APIKey != "" || cfg.Providers.DeepSeek.APIKey != newKey ||
		cfg.Providers.DeepSeek.APIBase != "https://ds.example.test/v1" ||
		cfg.Providers.OpenAI.APIKey != fileSecret+"-openai" || cfg.Providers.OpenAI.APIBase != "" {
		t.Errorf("saved providers differ from the patches")
	}
	data, _ := os.ReadFile(path)
	for _, kept := range []string{"-api", "-mcp", "-telegram", "-custom"} {
		if !strings.Contains(string(data), fileSecret+kept) {
			t.Errorf("the saved file lost the secret %q", fileSecret+kept)
		}
	}

	// Setting the running key back clears restart_required.
	if v, err = s.UpdateProvider("anthropic", websocket.ProviderPatch{APIKey: ptr(fileSecret + "-anthropic")}); err != nil || v.RestartRequired {
		t.Errorf("restore = %+v, %v", v, err)
	}
}

func TestWebProvidersEnvOverrideRestart(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningProviders(t, path, map[string]string{"NANOBOT_GO_OPENAI_API_KEY": envSecret})
	// The environment's key wins, so a saved change needs no restart.
	v, err := s.UpdateProvider("openai", websocket.ProviderPatch{APIKey: ptr("sk-another-fixture-key")})
	if err != nil || v.RestartRequired || !slices.Equal(v.EnvOverrides, []string{"apiKey"}) {
		t.Errorf("openai = %+v, %v", v, err)
	}
	assertNoSecrets(t, v)
	if data, _ := os.ReadFile(path); strings.Contains(string(data), envSecret) {
		t.Error("the saved file holds a key from the environment")
	}
}

func TestWebProvidersRejects(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningProviders(t, path, nil)
	before, _ := os.ReadFile(path)
	for _, p := range []websocket.ProviderPatch{
		{APIKey: ptr("***")},
		{APIKey: ptr(" *** ")},
		{APIKey: ptr("sk-f••••opic")},
		{APIKey: ptr("••••")},
		{APIKey: ptr("   ")},
		{APIKey: ptr("sk-has space")},
		{APIKey: ptr(strings.Repeat("k", maxAPIKeyLen+1))},
		{APIBase: ptr("ftp://example.test")},
		{APIBase: ptr("example.test/v1")},
		{APIBase: ptr("https://user:pw@example.test")},
		{APIBase: ptr("https://" + strings.Repeat("a", maxAPIBaseLen))},
	} {
		_, err := s.UpdateProvider("anthropic", p)
		if !errors.Is(err, websocket.ErrInvalidProvider) {
			t.Errorf("patch %+v: err = %v", p, err)
		} else if strings.Contains(err.Error(), "sk-has") || strings.Contains(err.Error(), "user:pw") {
			t.Errorf("error repeats the input: %v", err)
		}
	}
	if _, err := s.UpdateProvider("custom", websocket.ProviderPatch{ClearAPIKey: true}); !errors.Is(err, websocket.ErrProviderNotFound) {
		t.Errorf("unknown = %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a rejected change was saved")
	}
}

func TestAPIKeyHint(t *testing.T) {
	for in, want := range map[string]string{"": "", "short": "••••", "12345678": "••••", "sk-abcdefxyz1": "sk-a••••xyz1"} {
		if got := apiKeyHint(in); got != want {
			t.Errorf("apiKeyHint(%q) = %q, want %q", in, got, want)
		}
	}
}
