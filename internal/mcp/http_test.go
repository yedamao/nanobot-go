package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// requireBearer rejects requests without the expected Authorization.
func requireBearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestHTTPTransports connects DefaultTransport to local streamable HTTP
// and SSE servers, with headers expanded from the environment.
func TestHTTPTransports(t *testing.T) {
	getServer := func(*http.Request) *sdk.Server { return testServer() }
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireBearer("tok-123", sdk.NewStreamableHTTPHandler(getServer, nil)))
	mux.Handle("/sse", requireBearer("tok-123", sdk.NewSSEHandler(getServer, nil)))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	auth := map[string]string{"Authorization": "Bearer ${MCP_TEST_TOKEN}"}
	cfgs := map[string]config.MCPServerConfig{
		"stream":  {URL: ts.URL + "/mcp", Headers: auth},
		"sse":     {URL: ts.URL + "/sse", Headers: auth}, // type inferred from the path
		"noauth":  {URL: ts.URL + "/mcp"},
		"refused": {URL: "http://127.0.0.1:1/mcp"},
	}
	reg := tools.NewRegistry()
	env := map[string]string{"MCP_TEST_TOKEN": "tok-123"}
	m := NewManager(cfgs, reg, nil, WithSSRFPolicy(loopbackPolicy(t)),
		WithLookupEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }))
	defer m.Close()
	if err := m.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct {
		state     State
		transport string
	}{
		"stream":  {StateConnected, config.MCPStreamableHTTP},
		"sse":     {StateConnected, config.MCPSSE},
		"noauth":  {StateFailed, config.MCPStreamableHTTP},
		"refused": {StateFailed, config.MCPStreamableHTTP},
	}
	for _, st := range m.Status() {
		w := want[st.Name]
		if st.State != w.state || st.Transport != w.transport || (st.State == StateConnected) != (len(st.Tools) == 4) {
			t.Errorf("%s: %+v", st.Name, st)
		}
		if st.Err != nil && strings.Contains(st.Err.Error(), "tok-123") {
			t.Errorf("%s: error leaks the token: %v", st.Name, st.Err)
		}
	}
	for _, name := range []string{"mcp_stream_echo", "mcp_sse_echo"} {
		r := reg.Execute(context.Background(), name, json.RawMessage(`{"text": "over http"}`))
		if r.IsError || r.Content != "over http" {
			t.Errorf("%s = %+v", name, r)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// With an environment proxy the URL is checked and the proxy connects;
// without one the pinned direct transport is used.
func TestGuardTransportRouting(t *testing.T) {
	var routed string
	ok := func(name string) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			routed = name
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
	}
	proxyURL, _ := url.Parse("http://10.9.9.9:3128") // a private proxy is fine
	gt := &guardTransport{policy: &security.Policy{}, direct: ok("direct"), proxied: ok("proxied")}
	tests := []struct {
		proxy   *url.URL
		target  string
		want    string
		wantErr string
	}{
		{nil, "http://10.0.0.1/mcp", "direct", ""}, // the direct transport's dialer enforces the policy
		{proxyURL, "http://93.184.216.34/mcp", "proxied", ""},
		{proxyURL, "http://10.0.0.1/mcp", "", "blocked internal/private address 10.0.0.1"},
		{proxyURL, "http://169.254.169.254/", "", "blocked"},
	}
	for _, tt := range tests {
		routed = ""
		gt.proxy = func(*http.Request) (*url.URL, error) { return tt.proxy, nil }
		req, _ := http.NewRequest(http.MethodPost, tt.target, strings.NewReader("{}"))
		resp, err := gt.RoundTrip(req)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || routed != "" {
				t.Errorf("%s via %v: err %v, routed %q", tt.target, tt.proxy, err, routed)
			}
			continue
		}
		if err != nil || routed != tt.want {
			t.Errorf("%s via %v: err %v, routed %q, want %q", tt.target, tt.proxy, err, routed, tt.want)
			continue
		}
		resp.Body.Close()
	}
}

// loopbackPolicy whitelists 127.0.0.1/32, where httptest listens.
func loopbackPolicy(t *testing.T) *security.Policy {
	t.Helper()
	p, err := security.NewPolicy([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// HTTP servers get the same SSRF policy as web_fetch: without a whitelist
// a local server is refused before any request (or header) reaches it,
// and a whitelisted server cannot redirect the client to a blocked one.
func TestHTTPTransportsSSRF(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
	}))
	defer internal.Close()
	// The redirect target is outside the whitelisted 127.0.0.1/32.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(internal.URL, "127.0.0.1", "127.0.0.2", 1)+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	tests := []struct {
		name string
		opts []Option
		url  string
		want string
	}{
		{"default policy", nil, internal.URL + "/mcp", "blocked internal/private address 127.0.0.1"},
		{"metadata", []Option{WithSSRFPolicy(loopbackPolicy(t))}, "http://169.254.169.254/mcp", "blocked internal/private address 169.254.169.254"},
		{"redirect", []Option{WithSSRFPolicy(loopbackPolicy(t))}, redirector.URL + "/mcp", "blocked internal/private address 127.0.0.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgs := map[string]config.MCPServerConfig{"srv": {URL: tt.url, Headers: map[string]string{"Authorization": "Bearer x"}}}
			m := NewManager(cfgs, tools.NewRegistry(), nil, tt.opts...)
			defer m.Close()
			if err := m.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			st := m.Status()[0]
			if st.State != StateFailed || st.Err == nil || !strings.Contains(st.Err.Error(), tt.want) {
				t.Errorf("status = %+v, want failure %q", st, tt.want)
			}
		})
	}
	if n := internalHits.Load(); n != 0 {
		t.Errorf("blocked server got %d requests", n)
	}
}
