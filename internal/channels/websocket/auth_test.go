package websocket

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
)

const (
	testToken  = "static-token-for-tests"
	testSecret = "issue-secret-for-tests"
	lanHost    = "192.168.1.5:18790"
)

// authMux returns a started channel's mux for cfg, listening (as far as
// New knows) on host.
func authMux(t *testing.T, cfg Config, host string) (*Channel, *http.ServeMux) {
	t.Helper()
	ch, err := New(cfg, channels.Deps{Bus: bus.NewMessageBus(8)}, Options{
		Host:     host,
		Status:   func() Status { return Status{Version: "test"} },
		Settings: &fakeSettings{saved: Settings{Model: "m", Channels: map[string]ChannelSettings{Name: {Enabled: true}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ch.Mount(mux)
	return ch, mux
}

// do serves one request from peer (host:port) to the mux.
func do(mux http.Handler, method, target, host, peer string, edit func(*http.Request)) *httptest.ResponseRecorder {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = host
	req.RemoteAddr = peer
	if edit != nil {
		edit(req)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestNewAllowsNonLoopbackWithSecret(t *testing.T) {
	b := bus.NewMessageBus(1)
	for _, cfg := range []Config{{Token: testToken}, {TokenIssueSecret: testSecret}} {
		for _, host := range []string{"0.0.0.0", "::", "192.168.1.5"} {
			if _, err := New(cfg, channels.Deps{Bus: b}, Options{Host: host}); err != nil {
				t.Errorf("New(%+v, host %q) = %v", cfg, host, err)
			}
		}
	}
	on := true
	if _, err := New(Config{WebsocketRequiresToken: &on}, channels.Deps{Bus: b}, Options{Host: "127.0.0.1"}); err == nil {
		t.Error("New with websocketRequiresToken and no secret succeeded")
	}
}

func TestParseConfigSecrets(t *testing.T) {
	cfg, err := ParseConfig(json.RawMessage(`{"enabled":true,"token_issue_secret":" s1 ","token":" t "}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenIssueSecret != "s1" || cfg.Token != "t" || cfg.TokenIssueSecretAlt != "" || !cfg.HasSecret() {
		t.Errorf("ParseConfig = %+v", cfg)
	}
	if _, err := ParseConfig(json.RawMessage(`{"tokenIssueSecret":"a","token_issue_secret":"b"}`)); err == nil {
		t.Error("differing tokenIssueSecret spellings accepted")
	}
	cfg, err = ParseConfig(json.RawMessage(`{"tokenIssueSecret":"a","websocketRequiresToken":true}`))
	if err != nil || cfg.WebsocketRequiresToken == nil || !*cfg.WebsocketRequiresToken {
		t.Errorf("ParseConfig = %+v, %v", cfg, err)
	}
	if cfg, _ := ParseConfig(json.RawMessage(`{"enabled":true}`)); cfg.HasSecret() {
		t.Error("HasSecret without a secret")
	}
}

func TestLocalRequestsNeedNoSignIn(t *testing.T) {
	for name, cfg := range map[string]Config{"no secret": {}, "with secret": {TokenIssueSecret: testSecret}} {
		t.Run(name, func(t *testing.T) {
			_, mux := authMux(t, cfg, "127.0.0.1")
			for _, path := range []string{StatusPath, ChatsPath, SettingsPath, "/ui/"} {
				if rec := do(mux, http.MethodGet, path, "127.0.0.1:18790", "127.0.0.1:5555", nil); rec.Code != http.StatusOK {
					t.Errorf("GET %s = %d", path, rec.Code)
				}
			}
			rec := do(mux, http.MethodGet, AuthPath, "localhost:18790", "[::1]:5555", nil)
			var st AuthState
			json.Unmarshal(rec.Body.Bytes(), &st)
			if rec.Code != http.StatusOK || st.Required || !st.Authenticated {
				t.Errorf("GET %s = %d %+v", AuthPath, rec.Code, st)
			}
		})
	}
}

func TestNoSecretServesOnlyLocal(t *testing.T) {
	_, mux := authMux(t, Config{}, "127.0.0.1")
	tests := []struct{ name, host, peer string }{
		{"rebound host", "evil.example", "127.0.0.1:5555"},
		{"lan peer with loopback host", "127.0.0.1:18790", "192.168.1.9:5555"},
	}
	for _, tt := range tests {
		for _, path := range []string{StatusPath, "/ui/", AuthPath} {
			if rec := do(mux, http.MethodGet, path, tt.host, tt.peer, nil); rec.Code != http.StatusForbidden {
				t.Errorf("%s: GET %s = %d, want 403", tt.name, path, rec.Code)
			}
		}
	}
}

func TestRemoteRequestsNeedSignIn(t *testing.T) {
	_, mux := authMux(t, Config{Token: testToken, TokenIssueSecret: testSecret}, "0.0.0.0")
	const peer = "192.168.1.9:5555"
	tests := []struct {
		name, host, peer string
		edit             func(*http.Request)
	}{
		{"lan", lanHost, peer, nil},
		{"spoofed loopback host", "127.0.0.1:18790", peer, nil},
		{"forwarded by a local proxy", "127.0.0.1:18790", "127.0.0.1:5555", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.7") }},
		{"wrong bearer", lanHost, peer, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }},
		{"wrong cookie", lanHost, peer, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "nope"}) }},
		{"query token off /ws", lanHost, peer, func(r *http.Request) { r.URL.RawQuery = "token=" + testToken }},
	}
	for _, tt := range tests {
		for _, path := range []string{StatusPath, ChatsPath, SettingsPath, Path} {
			rec := do(mux, http.MethodGet, path, tt.host, tt.peer, tt.edit)
			if tt.name == "query token off /ws" && path == Path {
				continue
			}
			if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s: GET %s = %d %q, want 401 with WWW-Authenticate", tt.name, path, rec.Code, rec.Header().Get("WWW-Authenticate"))
			}
		}
	}
	// The page and sign-in stay reachable.
	for _, path := range []string{"/ui/", AuthPath} {
		if rec := do(mux, http.MethodGet, path, lanHost, peer, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
	rec := do(mux, http.MethodGet, AuthPath, lanHost, peer, nil)
	var st AuthState
	json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Required || st.Authenticated {
		t.Errorf("GET %s = %+v, want required and not authenticated", AuthPath, st)
	}

	for _, auth := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testSecret) },
		func(r *http.Request) { r.Header.Set("X-Nanobot-Auth", testSecret) },
	} {
		for _, path := range []string{StatusPath, ChatsPath, SettingsPath} {
			if rec := do(mux, http.MethodGet, path, lanHost, peer, auth); rec.Code != http.StatusOK {
				t.Errorf("GET %s with a secret = %d", path, rec.Code)
			}
		}
	}
}

// login signs in and returns the session cookie.
func login(t *testing.T, mux http.Handler, token string) (*http.Cookie, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, LoginPath, strings.NewReader(`{"token":"`+token+`"}`))
	req.Host = lanHost
	req.RemoteAddr = "192.168.1.9:5555"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c, rec.Code
		}
	}
	return nil, rec.Code
}

func TestLoginAndLogout(t *testing.T) {
	ch, mux := authMux(t, Config{TokenIssueSecret: testSecret}, "0.0.0.0")
	cookie, code := login(t, mux, testSecret)
	if code != http.StatusNoContent || cookie == nil {
		t.Fatalf("login = %d, cookie %v", code, cookie)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Value == testSecret || strings.Contains(cookie.Value, testSecret) {
		t.Errorf("cookie = %+v", cookie)
	}
	withCookie := func(r *http.Request) { r.AddCookie(cookie) }
	const peer = "192.168.1.9:5555"
	if rec := do(mux, http.MethodGet, StatusPath, lanHost, peer, withCookie); rec.Code != http.StatusOK {
		t.Errorf("GET %s with the cookie = %d", StatusPath, rec.Code)
	}
	var st AuthState
	json.Unmarshal(do(mux, http.MethodGet, AuthPath, lanHost, peer, withCookie).Body.Bytes(), &st)
	if !st.Required || !st.Authenticated {
		t.Errorf("auth state with the cookie = %+v", st)
	}

	// A session expires.
	ch.auth.now = func() time.Time { return time.Now().Add(SessionTTL + time.Minute) }
	if rec := do(mux, http.MethodGet, StatusPath, lanHost, peer, withCookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET %s with an expired cookie = %d", StatusPath, rec.Code)
	}
	ch.auth.now = time.Now

	cookie, _ = login(t, mux, testSecret)
	withCookie = func(r *http.Request) { r.AddCookie(cookie) }
	if rec := do(mux, http.MethodPost, LogoutPath, lanHost, peer, withCookie); rec.Code != http.StatusNoContent {
		t.Errorf("logout = %d", rec.Code)
	}
	if rec := do(mux, http.MethodGet, StatusPath, lanHost, peer, withCookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET %s after logout = %d", StatusPath, rec.Code)
	}
}

func TestLoginRefusals(t *testing.T) {
	_, mux := authMux(t, Config{Token: testToken}, "0.0.0.0")
	if c, code := login(t, mux, "wrong"); code != http.StatusUnauthorized || c != nil {
		t.Errorf("wrong token = %d, cookie %v", code, c)
	}
	// Cross-site sign-in is refused.
	rec := do(mux, http.MethodPost, LoginPath, lanHost, "192.168.1.9:5555", func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site login = %d", rec.Code)
	}
	// Repeated failures are throttled, even for the right token.
	for range loginFailLimit {
		login(t, mux, "wrong")
	}
	if _, code := login(t, mux, testToken); code != http.StatusTooManyRequests {
		t.Errorf("login after %d failures = %d, want 429", loginFailLimit, code)
	}

	_, mux = authMux(t, Config{}, "127.0.0.1")
	if rec := do(mux, http.MethodPost, LoginPath, "127.0.0.1", "127.0.0.1:5555", nil); rec.Code != http.StatusNotFound {
		t.Errorf("login without a secret = %d", rec.Code)
	}
}

func TestSettingsNeverShowSecrets(t *testing.T) {
	_, mux := authMux(t, Config{Token: testToken, TokenIssueSecret: testSecret}, "0.0.0.0")
	rec := do(mux, http.MethodGet, SettingsPath, lanHost, "192.168.1.9:5555", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", SettingsPath, rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, testToken) || strings.Contains(body, testSecret) {
		t.Errorf("settings expose a secret: %s", body)
	}
	for _, key := range []string{"token", "tokenIssueSecret", "token_issue_secret"} {
		body := `{"channels":{"websocket":{"` + key + `":"x"}}}`
		if _, err := ParseSettingsPatch([]byte(body)); err == nil {
			t.Errorf("settings patch %s accepted", body)
		}
	}
}

// TestWebSocketAuth needs sign-in on loopback too (websocketRequiresToken),
// since the test server's peer is always loopback.
func TestWebSocketAuth(t *testing.T) {
	on := true
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1"})
	ch, err := New(Config{TokenIssueSecret: testSecret, WebsocketRequiresToken: &on}, channels.Deps{Bus: h.bus}, Options{Host: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ch.Mount(mux)
	h.srv.Config.Handler = mux
	h.ch = ch
	if err := ch.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Stop(t.Context()) })

	ctx := testCtx(t)
	_, resp, err := websocket.Dial(ctx, h.wsURL(""), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dial without a token = %v, %v", resp, err)
	}
	conn, _, err := websocket.Dial(ctx, h.wsURL("?token="+testSecret), nil)
	if err != nil {
		t.Fatalf("dial with ?token= = %v", err)
	}
	conn.CloseNow()

	cookie, code := login(t, mux, testSecret)
	if code != http.StatusNoContent {
		t.Fatalf("login = %d", code)
	}
	hdr := http.Header{}
	hdr.Set("Cookie", cookie.Name+"="+cookie.Value)
	conn, _, err = websocket.Dial(ctx, h.wsURL(""), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial with the session cookie = %v", err)
	}
	conn.CloseNow()
}
