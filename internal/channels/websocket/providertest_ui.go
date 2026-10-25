package websocket

import (
	"context"
	"net/http"
)

// ProviderTestPath is POST /ui/providers/{name}/test: probe whether a
// provider's credential works with a cheap call (its models list). The
// body may hold a draft apiKey/apiBase used for this probe only.
const ProviderTestPath = ProvidersPath + "/{name}/test"

// Provider test statuses.
const (
	// ProviderTestOK: the provider accepted the credential.
	ProviderTestOK = "ok"
	// ProviderTestNotConfigured: no apiKey to test with.
	ProviderTestNotConfigured = "not_configured"
	// ProviderTestRejected: the provider answered 401 or 403.
	ProviderTestRejected = "rejected"
	// ProviderTestError: the probe failed otherwise (network, timeout,
	// another HTTP status, an unreadable answer).
	ProviderTestError = "error"
)

// ProviderTester probes a provider's credential. It returns
// ErrProviderNotFound for an unknown name and ErrInvalidProvider for a
// bad draft (whose text must never hold the key). A completed probe,
// failed or not, is a result, not an error.
type ProviderTester interface {
	TestProvider(ctx context.Context, name string, t ProviderTest) (ProviderTestResult, error)
}

// ProviderTest is the body of POST /ui/providers/{name}/test. A missing
// apiKey tests the saved one (environment applied, as the gateway runs);
// a missing apiBase uses the saved one, "" the default. Neither is saved.
type ProviderTest struct {
	APIKey  *string `json:"apiKey"`
	APIBase *string `json:"apiBase"`
}

// ProviderTestResult is the answer of POST /ui/providers/{name}/test.
// Message is short, safe text: never the key, a header or the URL.
type ProviderTestResult struct {
	OK      bool   `json:"ok"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// testProvider answers POST /ui/providers/{name}/test. An empty body
// is taken as {}.
func (c *Channel) testProvider(w http.ResponseWriter, r *http.Request) {
	var t ProviderTest
	if r.ContentLength != 0 {
		if err := decodeJSONBody(r, &t, maxProviderBody); err != nil {
			http.Error(w, "body must be {apiKey?, apiBase?}", http.StatusBadRequest)
			return
		}
	}
	res, err := c.tester.TestProvider(r.Context(), r.PathValue("name"), t)
	if err != nil {
		c.providerError(w, "test", err)
		return
	}
	writeJSON(w, res)
}
