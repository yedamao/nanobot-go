package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
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
	m := NewManager(cfgs, reg, nil, WithLookupEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }))
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
