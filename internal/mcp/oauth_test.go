package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// fakeIdP is an MCP server protected by its own OAuth authorization
// server: protected resource and server metadata, dynamic registration,
// and a token endpoint for codes and refresh tokens.
type fakeIdP struct {
	srv *httptest.Server

	mu       sync.Mutex
	valid    map[string]bool // access tokens the MCP endpoint accepts
	refresh  map[string]bool // refresh tokens the token endpoint accepts
	refuse   string          // error code for refresh grants, if set
	n        int
	expires  int // expires_in for issued tokens
	register atomic.Int32
	requests atomic.Int32
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{valid: map[string]bool{}, refresh: map[string]bool{}, expires: 3600}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	base := f.srv.URL
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"registration_endpoint": base + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		f.register.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, 201, map[string]any{"client_id": "client-1", "redirect_uris": body["redirect_uris"], "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if f.refuse != "" {
				writeJSON(w, 400, map[string]any{"error": f.refuse})
				return
			}
			if !f.refresh[r.Form.Get("refresh_token")] {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			delete(f.refresh, r.Form.Get("refresh_token")) // rotated
		default:
			writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
			return
		}
		f.n++
		at, rt := "at-"+string(rune('0'+f.n)), "rt-"+string(rune('0'+f.n))
		f.valid[at], f.refresh[rt] = true, true
		writeJSON(w, 200, map[string]any{"access_token": at, "token_type": "Bearer", "refresh_token": rt, "expires_in": f.expires})
	})
	mcpHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return testServer() }, nil)
	sseHandler := sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return testServer() }, nil)
	protect := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			f.mu.Lock()
			ok := f.valid[tok]
			f.mu.Unlock()
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	mux.Handle("/mcp", protect(mcpHandler))
	mux.Handle("/sse", protect(sseHandler))
	return f
}

func (f *fakeIdP) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid = map[string]bool{}
}

// browserFetch plays the user: it checks the authorization URL and
// approves.
func browserFetch(t *testing.T, redirect string, calls *atomic.Int32) auth.AuthorizationCodeFetcher {
	return func(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		calls.Add(1)
		u, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		if q.Get("redirect_uri") != redirect || q.Get("client_id") != "client-1" || q.Get("code_challenge_method") != "S256" {
			t.Errorf("authorization URL %s", args.URL)
		}
		return &auth.AuthorizationResult{Code: "good-code", State: q.Get("state")}, nil
	}
}

func oauthManager(t *testing.T, st *OAuthStore, servers map[string]config.MCPServerConfig, opts ...Option) *Manager {
	t.Helper()
	opts = append([]Option{WithSSRFPolicy(loopbackPolicy(t)), WithOAuthStore(st), WithConnectTimeout(10 * time.Second)}, opts...)
	m := NewManager(servers, tools.NewRegistry(), nil, opts...)
	t.Cleanup(func() { m.Close() })
	return m
}

func state(t *testing.T, m *Manager) ServerStatus {
	t.Helper()
	if err := m.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m.Status()[0]
}

// Background connects never start a sign-in: without credentials the
// server waits for authorization and nothing is requested.
func TestOAuthBackgroundWithoutCredentials(t *testing.T) {
	idp := newFakeIdP(t)
	st := newTestStore(t)
	m := oauthManager(t, st, map[string]config.MCPServerConfig{"srv": {URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}})
	got := state(t, m)
	if got.State != StateAuthorizationRequired || !errors.Is(got.Err, ErrAuthorizationRequired) {
		t.Fatalf("state = %s, %v", got.State, got.Err)
	}
	if n := idp.requests.Load(); n != 0 {
		t.Errorf("%d requests reached the server", n)
	}
}

func TestOAuthBrowserThenStoredTokens(t *testing.T) {
	for _, path := range []string{"/mcp", "/sse"} {
		t.Run(path, func(t *testing.T) {
			idp := newFakeIdP(t)
			st := newTestStore(t)
			cfg := config.MCPServerConfig{URL: idp.srv.URL + path, Auth: config.MCPAuthOAuth,
				// A configured Authorization header is replaced by the token.
				Headers: map[string]string{"Authorization": "Bearer stale"}}
			servers := map[string]config.MCPServerConfig{"srv": cfg}
			redirect := "http://127.0.0.1:18790/auth/mcp/callback"

			var calls atomic.Int32
			m := oauthManager(t, st, servers, WithOAuthBrowser("srv", &OAuthBrowser{RedirectURI: redirect, Fetch: browserFetch(t, redirect, &calls)}))
			if got := state(t, m); got.State != StateConnected {
				t.Fatalf("browser connect: %s %v", got.State, got.Err)
			}
			m.Close()
			if calls.Load() != 1 || idp.register.Load() != 1 {
				t.Errorf("fetch calls %d, registrations %d; want 1, 1", calls.Load(), idp.register.Load())
			}
			if !st.HasCredentials("srv", cfg.URL) {
				t.Fatal("no stored credentials")
			}
			snap, _ := st.bind("srv", cfg.URL).snapshot()
			if snap.metadata == nil || snap.metadata.TokenEndpoint != idp.srv.URL+"/token" || snap.oauthIssuer != idp.srv.URL ||
				snap.tokenIssuer != idp.srv.URL || snap.client == nil || snap.client.ClientID != "client-1" || snap.redirectURI != redirect {
				t.Errorf("stored %+v metadata %+v client %+v", snap, snap.metadata, snap.client)
			}

			// A background connect (gateway start) uses the stored tokens.
			bg := oauthManager(t, st, servers)
			if got := state(t, bg); got.State != StateConnected {
				t.Fatalf("background connect: %s %v", got.State, got.Err)
			}
		})
	}
}

func seedAuthorized(t *testing.T, idp *fakeIdP, st *OAuthStore, cfg config.MCPServerConfig) {
	t.Helper()
	redirect := "http://127.0.0.1/auth/mcp/callback"
	var calls atomic.Int32
	m := oauthManager(t, st, map[string]config.MCPServerConfig{"srv": cfg}, WithOAuthBrowser("srv", &OAuthBrowser{RedirectURI: redirect, Fetch: browserFetch(t, redirect, &calls)}))
	if got := state(t, m); got.State != StateConnected {
		t.Fatalf("seed: %s %v", got.State, got.Err)
	}
	m.Close()
}

func expireStored(t *testing.T, st *OAuthStore, cfg config.MCPServerConfig) {
	t.Helper()
	if _, err := st.bind("srv", cfg.URL).update(func(e *storedServer) {
		past := float64(time.Now().Add(-time.Minute).Unix())
		e.ExpiresAt = &past
	}, false, false); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthRefresh(t *testing.T) {
	idp := newFakeIdP(t)
	st := newTestStore(t)
	cfg := config.MCPServerConfig{URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}
	seedAuthorized(t, idp, st, cfg)

	// An expired token is refreshed before use, and the rotated pair is
	// stored.
	expireStored(t, st, cfg)
	idp.revokeAll()
	m := oauthManager(t, st, map[string]config.MCPServerConfig{"srv": cfg})
	if got := state(t, m); got.State != StateConnected {
		t.Fatalf("refresh connect: %s %v", got.State, got.Err)
	}
	m.Close()
	snap, _ := st.bind("srv", cfg.URL).snapshot()
	if tok := snap.token(); tok.AccessToken != "at-2" || tok.RefreshToken != "rt-2" || !tok.Valid() {
		t.Errorf("stored after refresh: %+v", tok)
	}

	// A token the server rejects before its expiry is refreshed once.
	idp.revokeAll()
	m = oauthManager(t, st, map[string]config.MCPServerConfig{"srv": cfg})
	if got := state(t, m); got.State != StateConnected {
		t.Fatalf("401 refresh connect: %s %v", got.State, got.Err)
	}
	if snap, _ := st.bind("srv", cfg.URL).snapshot(); snap.token().AccessToken != "at-3" {
		t.Errorf("after 401 refresh: %+v", snap.token())
	}
}

func TestOAuthRefreshRefused(t *testing.T) {
	for _, tc := range []struct {
		code       string
		keepClient bool
	}{{"invalid_grant", true}, {"invalid_client", false}, {"unauthorized_client", false}} {
		t.Run(tc.code, func(t *testing.T) {
			idp := newFakeIdP(t)
			st := newTestStore(t)
			cfg := config.MCPServerConfig{URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}
			seedAuthorized(t, idp, st, cfg)
			expireStored(t, st, cfg)
			idp.mu.Lock()
			idp.refuse = tc.code
			idp.mu.Unlock()

			m := oauthManager(t, st, map[string]config.MCPServerConfig{"srv": cfg})
			got := state(t, m)
			if got.State != StateAuthorizationRequired {
				t.Fatalf("state = %s %v, want authorization_required", got.State, got.Err)
			}
			if st.HasCredentials("srv", cfg.URL) {
				t.Error("refused tokens kept")
			}
			snap, _ := st.bind("srv", cfg.URL).snapshot()
			if (snap.client != nil) != tc.keepClient {
				t.Errorf("client kept = %v, want %v", snap.client != nil, tc.keepClient)
			}
		})
	}
}

// A transient refresh failure keeps the credentials for a later retry.
func TestOAuthRefreshTransientKeepsTokens(t *testing.T) {
	idp := newFakeIdP(t)
	st := newTestStore(t)
	cfg := config.MCPServerConfig{URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}
	seedAuthorized(t, idp, st, cfg)
	expireStored(t, st, cfg)
	idp.mu.Lock()
	idp.refuse = "temporarily_unavailable"
	idp.mu.Unlock()
	m := oauthManager(t, st, map[string]config.MCPServerConfig{"srv": cfg})
	if got := state(t, m); got.State != StateFailed {
		t.Fatalf("state = %s %v, want failed", got.State, got.Err)
	}
	if !st.HasCredentials("srv", cfg.URL) {
		t.Error("transient failure dropped the tokens")
	}
}

// Tokens the server rejects, with no refresh possible, are dropped in
// the background and the server waits for a new sign-in.
func TestOAuthRejectedWithoutRefresh(t *testing.T) {
	idp := newFakeIdP(t)
	st := newTestStore(t)
	cfg := config.MCPServerConfig{URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}
	seedAuthorized(t, idp, st, cfg)
	idp.revokeAll()
	idp.mu.Lock()
	idp.refresh = map[string]bool{}
	idp.mu.Unlock()
	m := oauthManager(t, st, map[string]config.MCPServerConfig{"srv": cfg})
	if got := state(t, m); got.State != StateAuthorizationRequired {
		t.Fatalf("state = %s %v", got.State, got.Err)
	}
	if st.HasCredentials("srv", cfg.URL) {
		t.Error("rejected tokens kept")
	}
	if n := idp.register.Load(); n != 1 {
		t.Errorf("background registered a client (%d registrations)", n)
	}
}

func TestOAuthReconnectAfterSignIn(t *testing.T) {
	idp := newFakeIdP(t)
	st := newTestStore(t)
	cfg := config.MCPServerConfig{URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}
	live := oauthManager(t, st, map[string]config.MCPServerConfig{"srv": cfg})
	if got := state(t, live); got.State != StateAuthorizationRequired {
		t.Fatalf("state = %s", got.State)
	}
	seedAuthorized(t, idp, st, cfg)
	if err := live.Reconnect(context.Background(), "srv"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if got := live.Status()[0]; got.State != StateConnected || len(got.Tools) == 0 {
		t.Errorf("after reconnect: %s tools %v", got.State, got.Tools)
	}
}
