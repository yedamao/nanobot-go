package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

const (
	// modelsFetchTimeout bounds a provider's GET /models.
	modelsFetchTimeout = 10 * time.Second
	// maxModelsBody caps the /models response read.
	maxModelsBody = 8 << 20
	// maxCatalogModels caps the models listed for one provider.
	maxCatalogModels = 500
	// openAIAPIBase is the OpenAI client's default endpoint.
	openAIAPIBase = "https://api.openai.com/v1"
	curatedNote   = "Built-in list; it may lag the provider. Any model ID can be typed."
)

// curatedModels are the built-in suggestions per preset, most useful
// first. They are advisory: providers add and retire models faster than
// nanobot releases, so the Models page always accepts a typed ID.
// Last refreshed 2026-10-25 against each provider's model docs.
var curatedModels = map[string][]string{
	"anthropic": {
		"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1",
		"claude-opus-5", "claude-sonnet-5", "claude-opus-4-8", "claude-opus-4-7",
		"claude-opus-4-6", "claude-sonnet-4-6", config.DefaultModel, "claude-haiku-4-5",
	},
	"openai": {
		"gpt-6-astra", "gpt-6.1-sol", "gpt-6-luna", "gpt-6-sol",
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
	},
	"deepseek": {"deepseek-flash", "deepseek-v4-pro"},
	"openrouter": {
		"anthropic/claude-opus-5.5", "anthropic/claude-sonnet-5.5", "anthropic/claude-haiku-5.5",
		"openai/gpt-6-astra", "openai/gpt-6.1-sol", "openai/gpt-6-luna",
		"deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4-pro",
		"google/gemini-4-argon", "google/gemini-3.8-flash",
	},
}

// webModels is the WebUI's model catalog (/ui/providers/{name}/models).
// It offers the curated list for every preset, and for OpenAI-compatible
// ones with an apiKey (saved, or from the environment as the gateway
// would run) the provider's own GET {apiBase}/models instead. It reads
// the config under the settings' lock and never returns the key.
type webModels struct {
	settings *webSettings
	getenv   func(string) string
	client   *http.Client
}

// newWebModels returns the catalog reading settings' config file.
func newWebModels(settings *webSettings, getenv func(string) string) *webModels {
	return &webModels{settings: settings, getenv: getenv, client: &http.Client{Timeout: modelsFetchTimeout}}
}

// ProviderModels implements websocket.ModelCatalog.
func (s *webModels) ProviderModels(ctx context.Context, name string) (websocket.ProviderModels, error) {
	if name == "auto" {
		return s.autoModels(), nil
	}
	preset, ok := config.LookupPreset(name)
	if !ok {
		return websocket.ProviderModels{}, websocket.ErrProviderNotFound
	}
	out := curated(name)
	if preset.Kind != config.KindOpenAI {
		return out, nil
	}

	s.settings.mu.Lock()
	cfg, err := config.Load(s.settings.path)
	s.settings.mu.Unlock()
	if err != nil {
		return websocket.ProviderModels{}, err
	}
	cfg.ApplyEnv(s.getenv)
	pc := cfg.Providers.Get(name)
	if pc.APIKey == "" {
		out.Status = websocket.CatalogNotConfigured
		out.Message = "No API key for " + name + ", so its live list is not fetched; showing the built-in list."
		return out, nil
	}
	base := cmp.Or(pc.APIBase, preset.DefaultAPIBase, openAIAPIBase)
	ids, err := s.fetch(ctx, base, pc.APIKey)
	if err != nil {
		out.Status = websocket.CatalogError
		out.Message = "Cannot fetch " + name + "'s model list (" + err.Error() + "); showing the built-in list."
		return out, nil
	}
	models := make([]websocket.CatalogModel, len(ids))
	for i, id := range ids {
		models[i] = websocket.CatalogModel{ID: id}
	}
	return websocket.ProviderModels{
		Provider: name, CatalogKind: websocket.CatalogFetched, Status: websocket.CatalogAvailable,
		Source: "provider", Models: models,
	}, nil
}

// curated is name's built-in list.
func curated(name string) websocket.ProviderModels {
	out := websocket.ProviderModels{
		Provider: name, CatalogKind: websocket.CatalogCurated, Status: websocket.CatalogAvailable,
		Message: curatedNote, Source: "curated", Models: []websocket.CatalogModel{},
	}
	for _, id := range curatedModels[name] {
		out.Models = append(out.Models, websocket.CatalogModel{ID: id})
	}
	return out
}

// autoModels merges the presets' curated lists, each labelled with its
// provider, for provider "auto" (which picks one by the model name).
func (s *webModels) autoModels() websocket.ProviderModels {
	out := curated("auto")
	seen := map[string]bool{}
	for _, p := range config.Presets() {
		for _, id := range curatedModels[p.Name] {
			if !seen[id] {
				seen[id] = true
				out.Models = append(out.Models, websocket.CatalogModel{ID: id, Label: p.Name})
			}
		}
	}
	return out
}

// fetch lists the model IDs of an OpenAI-compatible GET {base}/models,
// deduplicated, sorted and capped. Its errors never hold the key or URL.
func (s *webModels) fetch(ctx context.Context, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("bad apiBase")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timed out")
		}
		return nil, fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("unreadable response")
	}
	var ids []string
	for _, m := range body.Data {
		if id := strings.TrimSpace(m.ID); id != "" && len(id) <= 200 && !strings.ContainsAny(id, " \t\r\n") {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("no models listed")
	}
	if len(ids) > maxCatalogModels {
		ids = ids[:maxCatalogModels]
	}
	return ids, nil
}

var _ websocket.ModelCatalog = (*webModels)(nil)
