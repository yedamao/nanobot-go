package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

// roundTrip answers a request in tests, which never reach the network.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

// testModels returns a catalog for path whose HTTP requests rt answers.
func testModels(t *testing.T, path string, env map[string]string, rt roundTrip) *webModels {
	t.Helper()
	getenv := func(k string) string { return env[k] }
	s := newWebModels(runningSettings(t, path, env), getenv)
	s.client = &http.Client{Transport: rt}
	return s
}

func noNetwork(t *testing.T) roundTrip {
	return func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return nil, errors.New("no network in tests")
	}
}

func modelIDs(m websocket.ProviderModels) []string {
	var ids []string
	for _, x := range m.Models {
		ids = append(ids, x.ID)
	}
	return ids
}

func assertNoKey(t *testing.T, v any, keys ...string) {
	t.Helper()
	data, _ := json.Marshal(v)
	for _, k := range keys {
		if strings.Contains(string(data), k) {
			t.Errorf("response holds a key: %s", data)
		}
	}
}

func TestWebModelsCuratedAnthropic(t *testing.T) {
	s := testModels(t, writeSettingsConfig(t), nil, noNetwork(t))
	m, err := s.ProviderModels(context.Background(), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	ids := modelIDs(m)
	if m.CatalogKind != websocket.CatalogCurated || m.Status != websocket.CatalogAvailable || m.Source != "curated" || len(ids) == 0 {
		t.Errorf("anthropic = %+v", m)
	}
	if !strings.Contains(strings.Join(ids, ","), config.DefaultModel) {
		t.Errorf("curated list lacks the default model: %v", ids)
	}
	assertNoKey(t, m, fileSecret)
}

func TestWebModelsFetch(t *testing.T) {
	var got *http.Request
	s := testModels(t, writeSettingsConfig(t), nil, func(r *http.Request) (*http.Response, error) {
		got = r
		return jsonResponse(http.StatusOK, `{"object":"list","data":[{"id":"gpt-z"},{"id":"gpt-a"},{"id":"gpt-a"},{"id":" "},{"id":"bad id"}]}`), nil
	})
	m, err := s.ProviderModels(context.Background(), "openai")
	if err != nil {
		t.Fatal(err)
	}
	// The fixture saves openai's apiBase https://example.test/v1.
	if got == nil || got.URL.String() != "https://example.test/v1/models" || got.Header.Get("Authorization") != "Bearer "+fileSecret+"-openai" {
		t.Fatalf("request = %+v", got)
	}
	if m.CatalogKind != websocket.CatalogFetched || m.Status != websocket.CatalogAvailable || m.Source != "provider" ||
		strings.Join(modelIDs(m), ",") != "gpt-a,gpt-z" {
		t.Errorf("openai = %+v", m)
	}
	assertNoKey(t, m, fileSecret)
}

func TestWebModelsFetchCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := range maxCatalogModels + 20 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"m-%04d"}`, i)
	}
	b.WriteString(`]}`)
	s := testModels(t, writeSettingsConfig(t), nil, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, b.String()), nil
	})
	m, _ := s.ProviderModels(context.Background(), "openai")
	if len(m.Models) != maxCatalogModels || m.Models[0].ID != "m-0000" {
		t.Errorf("listed %d models", len(m.Models))
	}
}

func TestWebModelsFallBackToCurated(t *testing.T) {
	path := writeSettingsConfig(t)
	// deepseek has no key: the curated list, without a request.
	s := testModels(t, path, nil, noNetwork(t))
	m, err := s.ProviderModels(context.Background(), "deepseek")
	if err != nil || m.Status != websocket.CatalogNotConfigured || m.CatalogKind != websocket.CatalogCurated || len(m.Models) == 0 {
		t.Errorf("deepseek = %+v, %v", m, err)
	}

	// A failing fetch: the curated list with the status, never the key.
	const envKey = "sk-env-fixture-deepseek-key"
	s = testModels(t, path, map[string]string{"NANOBOT_GO_DEEPSEEK_API_KEY": envKey}, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.deepseek.com/models" || r.Header.Get("Authorization") != "Bearer "+envKey {
			t.Errorf("request = %s %v", r.URL, r.Header)
		}
		return jsonResponse(http.StatusUnauthorized, `{"error":"bad key `+envKey+`"}`), nil
	})
	m, err = s.ProviderModels(context.Background(), "deepseek")
	if err != nil || m.Status != websocket.CatalogError || m.Source != "curated" || len(m.Models) == 0 || !strings.Contains(m.Message, "HTTP 401") {
		t.Errorf("deepseek failing = %+v, %v", m, err)
	}
	assertNoKey(t, m, envKey, fileSecret)
}

func TestWebModelsAutoAndUnknown(t *testing.T) {
	s := testModels(t, writeSettingsConfig(t), nil, noNetwork(t))
	m, err := s.ProviderModels(context.Background(), "auto")
	if err != nil || m.CatalogKind != websocket.CatalogCurated || len(m.Models) < len(curatedModels["anthropic"])+len(curatedModels["openai"]) {
		t.Errorf("auto = %+v, %v", m, err)
	}
	if m.Models[0].Label != "anthropic" {
		t.Errorf("auto's first model = %+v", m.Models[0])
	}
	if _, err := s.ProviderModels(context.Background(), "custom"); !errors.Is(err, websocket.ErrProviderNotFound) {
		t.Errorf("unknown = %v", err)
	}
}
