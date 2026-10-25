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

func TestWebModelsTestProvider(t *testing.T) {
	const draft = "sk-draft-fixture-key-0123"
	var got *http.Request
	code := http.StatusOK
	s := testModels(t, writeSettingsConfig(t), nil, func(r *http.Request) (*http.Response, error) {
		got = r
		return jsonResponse(code, `{"data":[{"id":"m"}],"error":"`+fileSecret+`"}`), nil
	})
	ctx := context.Background()

	// The saved openai key, at its saved apiBase.
	res, err := s.TestProvider(ctx, "openai", websocket.ProviderTest{})
	if err != nil || !res.OK || res.Status != websocket.ProviderTestOK {
		t.Fatalf("openai = %+v, %v", res, err)
	}
	if got.URL.String() != "https://example.test/v1/models" || got.Header.Get("Authorization") != "Bearer "+fileSecret+"-openai" {
		t.Errorf("request = %s %v", got.URL, got.Header)
	}
	assertNoKey(t, res, fileSecret)

	// Anthropic lists models with x-api-key at its default base.
	if res, err = s.TestProvider(ctx, "anthropic", websocket.ProviderTest{}); err != nil || !res.OK {
		t.Fatalf("anthropic = %+v, %v", res, err)
	}
	if got.URL.String() != "https://api.anthropic.com/v1/models" || got.Header.Get("x-api-key") != fileSecret+"-anthropic" ||
		got.Header.Get("anthropic-version") == "" || got.Header.Get("Authorization") != "" {
		t.Errorf("anthropic request = %s %v", got.URL, got.Header)
	}

	// A draft key and base are used, not saved; "" is the default base.
	code = http.StatusUnauthorized
	res, err = s.TestProvider(ctx, "deepseek", websocket.ProviderTest{APIKey: ptr(" " + draft + " "), APIBase: ptr("")})
	if err != nil || res.OK || res.Status != websocket.ProviderTestRejected || !strings.Contains(res.Message, "401") {
		t.Fatalf("deepseek draft = %+v, %v", res, err)
	}
	if got.URL.String() != "https://api.deepseek.com/models" || got.Header.Get("Authorization") != "Bearer "+draft {
		t.Errorf("draft request = %s %v", got.URL, got.Header)
	}
	assertNoKey(t, res, draft, fileSecret)
	if cfg, _ := config.Load(s.settings.path); cfg.Providers.DeepSeek.APIKey != "" {
		t.Error("the draft key was saved")
	}

	code = http.StatusForbidden
	if res, _ = s.TestProvider(ctx, "openai", websocket.ProviderTest{APIBase: ptr("http://localhost:9/v1")}); res.Status != websocket.ProviderTestRejected ||
		got.URL.String() != "http://localhost:9/v1/models" {
		t.Errorf("403 = %+v at %s", res, got.URL)
	}
	code = http.StatusInternalServerError
	if res, _ = s.TestProvider(ctx, "openai", websocket.ProviderTest{}); res.Status != websocket.ProviderTestError || res.Message != "openai answered HTTP 500." {
		t.Errorf("500 = %+v", res)
	}
}

func TestWebModelsTestProviderFailures(t *testing.T) {
	const draft = "sk-draft-fixture-key-0123"
	s := testModels(t, writeSettingsConfig(t), nil, func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial %s with %s: refused", r.URL, r.Header.Get("Authorization"))
	})
	ctx := context.Background()
	res, err := s.TestProvider(ctx, "openai", websocket.ProviderTest{APIKey: ptr(draft)})
	if err != nil || res.Status != websocket.ProviderTestError || res.Message != "Cannot reach openai: request failed." {
		t.Errorf("network = %+v, %v", res, err)
	}
	assertNoKey(t, res, draft, "example.test")

	if res, err = s.TestProvider(ctx, "deepseek", websocket.ProviderTest{}); err != nil || res.OK || res.Status != websocket.ProviderTestNotConfigured {
		t.Errorf("no key = %+v, %v", res, err)
	}
	if _, err = s.TestProvider(ctx, "custom", websocket.ProviderTest{}); !errors.Is(err, websocket.ErrProviderNotFound) {
		t.Errorf("unknown = %v", err)
	}
	for _, bad := range []websocket.ProviderTest{
		{APIKey: ptr("***")}, {APIKey: ptr("sk-a••••xyz1")}, {APIKey: ptr(" ")}, {APIKey: ptr("a b")},
		{APIBase: ptr("ftp://x")}, {APIBase: ptr("https://user:" + draft + "@x.test")},
	} {
		_, err := s.TestProvider(ctx, "openai", bad)
		if !errors.Is(err, websocket.ErrInvalidProvider) || strings.Contains(err.Error(), draft) {
			t.Errorf("TestProvider(%+v) = %v", bad, err)
		}
	}

	// The environment's key is tested, as the gateway would run.
	env := map[string]string{"NANOBOT_GO_DEEPSEEK_API_KEY": envSecret}
	var auth string
	s = testModels(t, writeSettingsConfig(t), env, func(r *http.Request) (*http.Response, error) {
		auth = r.Header.Get("Authorization")
		return jsonResponse(http.StatusOK, `{}`), nil
	})
	if res, _ = s.TestProvider(ctx, "deepseek", websocket.ProviderTest{}); !res.OK || auth != "Bearer "+envSecret {
		t.Errorf("env key = %+v, auth set %v", res, auth != "")
	}
}
