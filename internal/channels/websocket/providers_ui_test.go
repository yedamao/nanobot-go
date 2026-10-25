package websocket

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeProviders is an in-memory ProvidersStore.
type fakeProviders struct {
	keys      map[string]string
	lastPatch ProviderPatch
}

func (f *fakeProviders) view(name string) ProviderView {
	return ProviderView{Name: name, HasAPIKey: f.keys[name] != "", EnvOverrides: []string{}}
}

func (f *fakeProviders) Providers() (ProvidersList, error) {
	return ProvidersList{Providers: []ProviderView{f.view("anthropic")}}, nil
}

func (f *fakeProviders) UpdateProvider(name string, p ProviderPatch) (ProviderView, error) {
	if name != "anthropic" {
		return ProviderView{}, ErrProviderNotFound
	}
	if p.APIKey != nil && *p.APIKey == "***" {
		return ProviderView{}, fmt.Errorf("%w: apiKey is a redacted placeholder", ErrInvalidProvider)
	}
	f.lastPatch = p
	switch {
	case p.ClearAPIKey:
		f.keys[name] = ""
	case p.APIKey != nil:
		f.keys[name] = *p.APIKey
	}
	return f.view(name), nil
}

func TestProvidersRoutes(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodGet, ProvidersPath, ""); code != http.StatusNotFound {
		t.Errorf("GET without store = %d", code)
	}

	const key = "sk-route-fixture-key"
	store := &fakeProviders{keys: map[string]string{"anthropic": key}}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", Providers: store})
	code, body := skillsRequest(t, h, http.MethodGet, ProvidersPath, "")
	if code != http.StatusOK || !strings.Contains(string(body), `"hasApiKey":true`) || strings.Contains(string(body), key) {
		t.Errorf("GET = %d %s", code, body)
	}

	code, body = skillsRequest(t, h, http.MethodPatch, ProvidersPath+"/anthropic", `{"clearApiKey":true}`)
	if code != http.StatusOK || !strings.Contains(string(body), `"hasApiKey":false`) {
		t.Errorf("clear = %d %s", code, body)
	}
	code, body = skillsRequest(t, h, http.MethodPatch, ProvidersPath+"/anthropic", `{"apiKey":"sk-new-route-key","apiBase":""}`)
	if code != http.StatusOK || strings.Contains(string(body), "sk-new-route-key") || store.lastPatch.APIBase == nil || *store.lastPatch.APIBase != "" {
		t.Errorf("set = %d %s, patch %+v", code, body, store.lastPatch)
	}
	if code, _ := skillsRequest(t, h, http.MethodPatch, ProvidersPath+"/anthropic", `{"apiKey":"***"}`); code != http.StatusBadRequest {
		t.Errorf("redacted = %d", code)
	}
	for _, bad := range []string{`{}`, `{"apiKey":"x","bogus":1}`, `[]`, ``} {
		if code, _ := skillsRequest(t, h, http.MethodPatch, ProvidersPath+"/anthropic", bad); code != http.StatusBadRequest {
			t.Errorf("patch %q = %d", bad, code)
		}
	}
	if code, _ := skillsRequest(t, h, http.MethodPatch, ProvidersPath+"/custom", `{"clearApiKey":true}`); code != http.StatusNotFound {
		t.Errorf("unknown = %d", code)
	}

	// Cross-origin changes are refused.
	req, _ := http.NewRequest(http.MethodPatch, h.srv.URL+ProvidersPath+"/anthropic", strings.NewReader(`{"clearApiKey":true}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin patch = %d", resp.StatusCode)
	}
	// Non-local requests without sign-in are refused.
	req, _ = http.NewRequest(http.MethodGet, h.srv.URL+ProvidersPath, nil)
	req.Host = "evil.example"
	if resp, err = h.srv.Client().Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host GET = %d", resp.StatusCode)
	}
}
