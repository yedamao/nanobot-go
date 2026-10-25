package websocket

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// fakeCatalog is a ModelCatalog knowing only anthropic.
type fakeCatalog struct{}

func (fakeCatalog) ProviderModels(_ context.Context, name string) (ProviderModels, error) {
	if name != "anthropic" {
		return ProviderModels{}, ErrProviderNotFound
	}
	return ProviderModels{Provider: name, CatalogKind: CatalogCurated, Status: CatalogAvailable, Source: "curated",
		Models: []CatalogModel{{ID: "claude-x"}}}, nil
}

func TestProviderModelsRoute(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodGet, ProvidersPath+"/anthropic/models", ""); code != http.StatusNotFound {
		t.Errorf("GET without catalog = %d", code)
	}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", Models: fakeCatalog{}})
	code, body := skillsRequest(t, h, http.MethodGet, ProvidersPath+"/anthropic/models", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"catalog_kind":"curated"`) || !strings.Contains(string(body), `"id":"claude-x"`) {
		t.Errorf("GET = %d %s", code, body)
	}
	if code, _ := skillsRequest(t, h, http.MethodGet, ProvidersPath+"/custom/models", ""); code != http.StatusNotFound {
		t.Errorf("unknown = %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+ProvidersPath+"/anthropic/models", nil)
	req.Host = "evil.example"
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host GET = %d", resp.StatusCode)
	}
}
