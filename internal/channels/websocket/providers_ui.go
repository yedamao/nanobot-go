package websocket

import (
	"errors"
	"net/http"
)

const (
	// ProvidersPath lists the known providers' credentials, redacted (GET
	// /ui/providers); PATCH /ui/providers/{name} sets or clears one's
	// apiKey and sets its apiBase (providers.<name> in the config file).
	ProvidersPath = "/ui/providers"
	// maxProviderBody caps the JSON body of a provider change.
	maxProviderBody = 16 << 10
)

var (
	// ErrInvalidProvider marks a ProvidersStore error caused by the
	// requested change; it is answered with 400 and the error's text,
	// which must never hold the key.
	ErrInvalidProvider = errors.New("invalid provider change")
	// ErrProviderNotFound marks a ProvidersStore error for a name that
	// is not a known provider; it is answered with 404.
	ErrProviderNotFound = errors.New("no such provider")
)

// ProvidersStore reads and changes providers.<name>.apiKey and apiBase.
// Implementations must load the config fresh from disk for every change
// and write back only that provider, and must never return an apiKey.
type ProvidersStore interface {
	Providers() (ProvidersList, error)
	UpdateProvider(name string, p ProviderPatch) (ProviderView, error)
}

// ProvidersList is the body of GET /ui/providers.
type ProvidersList struct {
	Providers []ProviderView `json:"providers"`
}

// ProviderView is one provider as the page sees it: never its apiKey,
// only whether one is saved and a short hint of it.
type ProviderView struct {
	Name string `json:"name"`
	// HasAPIKey reports that the config file holds an apiKey.
	HasAPIKey bool `json:"hasApiKey"`
	// APIKeyHint shows the saved key's first and last four characters
	// (sk-a••••xyz1), or •••• for a short one; empty without a key.
	APIKeyHint string `json:"apiKeyHint"`
	// APIBase is the saved endpoint override; empty uses DefaultAPIBase.
	APIBase        string `json:"apiBase"`
	DefaultAPIBase string `json:"defaultApiBase,omitempty"`
	// RestartRequired reports that the saved credentials, with the
	// environment applied, differ from the ones the gateway runs with:
	// provider clients are built when it starts.
	RestartRequired bool `json:"restart_required"`
	// EnvOverrides names the fields ("apiKey", "apiBase") set by
	// NANOBOT_GO_<NAME>_API_KEY / _API_BASE, which win over the file.
	EnvOverrides []string `json:"env_overrides"`
}

// ProviderPatch is the body of PATCH /ui/providers/{name}. A missing
// apiKey keeps the saved one; clearApiKey removes it (and apiKey is
// ignored). A missing apiBase keeps the saved one; "" removes it.
type ProviderPatch struct {
	APIKey      *string `json:"apiKey"`
	ClearAPIKey bool    `json:"clearApiKey"`
	APIBase     *string `json:"apiBase"`
}

// serveProviders answers GET /ui/providers.
func (c *Channel) serveProviders(w http.ResponseWriter, _ *http.Request) {
	list, err := c.providers.Providers()
	if err != nil {
		c.providerError(w, "list", err)
		return
	}
	writeJSON(w, list)
}

// patchProvider answers PATCH /ui/providers/{name}.
func (c *Channel) patchProvider(w http.ResponseWriter, r *http.Request) {
	var p ProviderPatch
	if err := decodeJSONBody(r, &p, maxProviderBody); err != nil {
		http.Error(w, "body must be {apiKey?, clearApiKey?, apiBase?}", http.StatusBadRequest)
		return
	}
	if p.APIKey == nil && !p.ClearAPIKey && p.APIBase == nil {
		http.Error(w, "nothing to change: send apiKey, clearApiKey or apiBase", http.StatusBadRequest)
		return
	}
	v, err := c.providers.UpdateProvider(r.PathValue("name"), p)
	if err != nil {
		c.providerError(w, "update", err)
		return
	}
	writeJSON(w, v)
}

// providerError answers a failed provider request. Only
// ErrInvalidProvider errors are shown; they never hold the key.
func (c *Channel) providerError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrInvalidProvider):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrProviderNotFound):
		http.Error(w, "no such provider", http.StatusNotFound)
	default:
		c.log.Warn("websocket: "+op+" provider", "error", err)
		http.Error(w, "cannot "+op+" the provider", http.StatusInternalServerError)
	}
}
