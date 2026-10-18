package config

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/providers"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name            string
		keys            map[string]string
		provider, model string
		wantProvider    string
		wantModel       string
		wantErr         string
	}{
		{name: "explicit", keys: map[string]string{"anthropic": "k"}, provider: "anthropic", model: "claude-x",
			wantProvider: "anthropic", wantModel: "claude-x"},
		{name: "explicit strips own prefix", keys: map[string]string{"anthropic": "k"}, provider: "anthropic",
			model: "anthropic/claude-x", wantProvider: "anthropic", wantModel: "claude-x"},
		{name: "explicit without key", provider: "openai", model: "gpt-4o", wantErr: "NANOBOT_GO_OPENAI_API_KEY"},
		{name: "unknown", provider: "nope", model: "m", wantErr: "unknown provider"},
		{name: "auto by prefix", keys: map[string]string{"deepseek": "k", "openrouter": "k"}, provider: "auto",
			model: "deepseek/deepseek-chat", wantProvider: "deepseek", wantModel: "deepseek-chat"},
		{name: "auto by keyword", keys: map[string]string{"anthropic": "k", "openai": "k"}, provider: "auto",
			model: "Claude-Opus", wantProvider: "anthropic", wantModel: "Claude-Opus"},
		{name: "auto openai keyword", keys: map[string]string{"anthropic": "k", "openai": "k"}, provider: "",
			model: "gpt-4o", wantProvider: "openai", wantModel: "gpt-4o"},
		{name: "auto gateway keeps namespace", keys: map[string]string{"openrouter": "k"}, provider: "auto",
			model: "anthropic/claude-x", wantProvider: "openrouter", wantModel: "anthropic/claude-x"},
		{name: "auto gateway strips own prefix", keys: map[string]string{"openrouter": "k"}, provider: "auto",
			model: "openrouter/meta/llama", wantProvider: "openrouter", wantModel: "meta/llama"},
		{name: "auto nothing configured", provider: "auto", model: "claude-x", wantErr: "no provider with an API key"},
		{name: "auto no match", keys: map[string]string{"anthropic": "k"}, provider: "auto", model: "llama3",
			wantErr: "no provider with an API key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			for name, key := range tt.keys {
				cfg.Providers.Get(name).APIKey = key
			}
			got, err := cfg.Resolve(tt.provider, tt.model)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Resolve error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Preset.Name != tt.wantProvider || got.Model != tt.wantModel {
				t.Errorf("Resolve = %s/%s, want %s/%s", got.Preset.Name, got.Model, tt.wantProvider, tt.wantModel)
			}
		})
	}
}

func TestResolveAPIBase(t *testing.T) {
	cfg := Default()
	cfg.Providers.DeepSeek.APIKey = "k"
	got, err := cfg.Resolve("deepseek", "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIBase != "https://api.deepseek.com" {
		t.Errorf("default APIBase = %q", got.APIBase)
	}
	cfg.Providers.DeepSeek.APIBase = "http://proxy"
	got, _ = cfg.Resolve("deepseek", "deepseek-chat")
	if got.APIBase != "http://proxy" {
		t.Errorf("configured APIBase = %q", got.APIBase)
	}
}

// TestBuildProviderFallback checks that the built chain fails over from a
// failing primary to the fallback, each using its own resolved model.
func TestBuildProviderFallback(t *testing.T) {
	var primaryHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	}))
	defer primary.Close()

	var fallbackModel, fallbackAuth string
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		fallbackModel, fallbackAuth = body.Model, r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer fallback.Close()

	cfg := Default()
	cfg.Agents.Defaults.Provider = "anthropic"
	cfg.Agents.Defaults.Model = "claude-x"
	cfg.Agents.Defaults.FallbackModels = []FallbackModel{{Model: "deepseek/deepseek-chat"}}
	cfg.Providers.Anthropic = ProviderConfig{APIKey: "ant", APIBase: primary.URL}
	cfg.Providers.DeepSeek = ProviderConfig{APIKey: "ds", APIBase: fallback.URL}

	p, target, err := cfg.BuildProvider(ProviderOptions{
		HTTPClient: primary.Client(),
		Retry:      providers.RetryPolicy{Delays: []time.Duration{}},
	})
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if target.Preset.Name != "anthropic" || target.Model != "claude-x" {
		t.Errorf("target = %+v", target)
	}
	resp, err := p.Chat(context.Background(), providers.ChatRequest{
		Messages: []providers.Message{{Role: providers.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("content = %q", resp.Content)
	}
	if primaryHits != 1 {
		t.Errorf("primary hits = %d, want 1", primaryHits)
	}
	if fallbackModel != "deepseek-chat" || fallbackAuth != "Bearer ds" {
		t.Errorf("fallback got model %q auth %q", fallbackModel, fallbackAuth)
	}
}

func TestBuildProviderErrors(t *testing.T) {
	cfg := Default()
	if _, _, err := cfg.BuildProvider(ProviderOptions{}); err == nil {
		t.Error("expected error without any API key")
	}
	cfg.Providers.Anthropic.APIKey = "k"
	cfg.Agents.Defaults.FallbackModels = []FallbackModel{{Provider: "openai", Model: "gpt-4o"}}
	_, _, err := cfg.BuildProvider(ProviderOptions{})
	if err == nil || !strings.Contains(err.Error(), "fallbackModels[0]") {
		t.Errorf("error = %v, want fallbackModels[0] error", err)
	}
	cfg.Agents.Defaults.FallbackModels = nil
	p, _, err := cfg.BuildProvider(ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*providers.Anthropic); !ok {
		t.Errorf("provider without fallbacks = %T, want *providers.Anthropic", p)
	}
}
