package websocket

import (
	"context"
	"errors"
	"net/http"
)

// ProviderModelsPath is GET /ui/providers/{name}/models: an advisory list
// of model IDs for the Models page's picker. {name} is a provider preset
// or "auto". The list never holds credentials.
const ProviderModelsPath = ProvidersPath + "/{name}/models"

// Model catalog kinds and statuses of a ModelCatalog answer.
const (
	// CatalogCurated is the built-in list shipped with nanobot.
	CatalogCurated = "curated"
	// CatalogFetched is the list the provider's /models endpoint answered.
	CatalogFetched = "fetched"
	// CatalogUnsupported means no list is offered; type a model ID.
	CatalogUnsupported = "unsupported"

	// CatalogAvailable: the models are the catalog_kind's list.
	CatalogAvailable = "available"
	// CatalogNotConfigured: no apiKey to fetch with; the curated list.
	CatalogNotConfigured = "not_configured"
	// CatalogError: the fetch failed; the curated list.
	CatalogError = "error"
)

// ModelCatalog lists a provider's models. It returns ErrProviderNotFound
// for an unknown name and must never return an apiKey.
type ModelCatalog interface {
	ProviderModels(ctx context.Context, name string) (ProviderModels, error)
}

// ProviderModels is the body of GET /ui/providers/{name}/models.
type ProviderModels struct {
	Provider string `json:"provider"`
	// CatalogKind is CatalogCurated, CatalogFetched or CatalogUnsupported.
	CatalogKind string `json:"catalog_kind"`
	// Status is CatalogAvailable, CatalogNotConfigured, CatalogError or
	// CatalogUnsupported.
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	// Source is "curated" or "provider" (fetched from its API).
	Source string         `json:"source"`
	Models []CatalogModel `json:"models"`
}

// CatalogModel is one suggested model.
type CatalogModel struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// serveProviderModels answers GET /ui/providers/{name}/models.
func (c *Channel) serveProviderModels(w http.ResponseWriter, r *http.Request) {
	m, err := c.models.ProviderModels(r.Context(), r.PathValue("name"))
	if err != nil {
		if errors.Is(err, ErrProviderNotFound) {
			http.Error(w, "no such provider", http.StatusNotFound)
			return
		}
		c.log.Warn("websocket: list provider models", "error", err)
		http.Error(w, "cannot list the provider's models", http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}
