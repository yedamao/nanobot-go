package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
)

func TestPrepareOAuthRedirectURI(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		manual   bool
		err      bool
	}{
		{in: "https://gw.example/auth/mcp/callback", want: "https://gw.example/auth/mcp/callback"},
		{in: "http://127.0.0.1:18790/auth/mcp/callback", want: "http://127.0.0.1:18790/auth/mcp/callback"},
		{in: "http://localhost:18790/auth/mcp/callback", want: "http://localhost:18790/auth/mcp/callback"},
		{in: "http://[::1]:8/auth/mcp/callback", want: "http://[::1]:8/auth/mcp/callback"},
		// A remote WebUI over HTTP pastes a loopback callback instead.
		{in: "http://192.168.1.5:18790/auth/mcp/callback", want: "http://127.0.0.1:18790/auth/mcp/callback", manual: true},
		{in: "http://gw.lan/auth/mcp/callback", want: "http://127.0.0.1/auth/mcp/callback", manual: true},
		{in: "https://gw.example/other", err: true},
		{in: "https://gw.example/auth/mcp/callback?x=1", err: true},
		{in: "https://u:p@gw.example/auth/mcp/callback", err: true},
		{in: "https://gw.example/auth/mcp/callback#f", err: true},
		{in: "ftp://gw.example/auth/mcp/callback", err: true},
		{in: "/auth/mcp/callback", err: true},
	} {
		got, manual, err := PrepareOAuthRedirectURI(tc.in)
		if (err != nil) != tc.err || got != tc.want || manual != tc.manual {
			t.Errorf("PrepareOAuthRedirectURI(%q) = %q, %v, %v; want %q, %v, err %v", tc.in, got, manual, err, tc.want, tc.manual, tc.err)
		}
	}
	if _, err := ValidateOAuthRedirectURI("http://gw.lan/auth/mcp/callback"); err == nil {
		t.Error("remote HTTP callback validated")
	}
}

// flowsFor returns flows that sign srv in against idp.
func flowsFor(t *testing.T, idp *fakeIdP, st *OAuthStore, activate OAuthActivateFunc) (*OAuthFlows, config.MCPServerConfig) {
	t.Helper()
	cfg := config.MCPServerConfig{URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}
	connect := func(ctx context.Context, name string, b *OAuthBrowser) error {
		m := oauthManager(t, st, map[string]config.MCPServerConfig{name: cfg}, WithOAuthBrowser(name, b), WithConnectTimeout(OAuthFlowTTL))
		defer m.Close()
		if err := m.Connect(ctx); err != nil {
			return err
		}
		if st := m.Status()[0]; st.State != StateConnected {
			return st.Err
		}
		return nil
	}
	if activate == nil {
		activate = func(context.Context, string) HotReload { return HotReload{OK: true, Message: "connected"} }
	}
	f := NewOAuthFlows(connect, activate, nil)
	f.allowHTTPAuthURL = true // the fake IdP is plain HTTP
	t.Cleanup(f.Close)
	return f, cfg
}

func waitStatus(t *testing.T, f *OAuthFlows, name, id string, want ...string) OAuthFlowStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := f.Status(name, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if st.Status == w {
				return st
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v, want one of %v", st, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOAuthFlowCallback(t *testing.T) {
	idp := newFakeIdP(t)
	st := newTestStore(t)
	f, cfg := flowsFor(t, idp, st, nil)

	got, err := f.Start("srv", "http://127.0.0.1:18790/auth/mcp/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != OAuthAuthorizationRequired || got.AuthorizationURL == "" || got.CompletionInput != "" || got.ExpiresIn <= 0 {
		t.Fatalf("start = %+v", got)
	}
	u, _ := url.Parse(got.AuthorizationURL)
	state := u.Query().Get("state")

	if _, err := f.Callback("wrong", "good-code", "", ""); statusOf(err) != http.StatusGone {
		t.Errorf("unknown state = %v, want 410", err)
	}
	name, err := f.Callback(state, "good-code", "", "")
	if err != nil || name != "srv" {
		t.Fatalf("Callback = %q, %v", name, err)
	}
	if _, err := f.Callback(state, "good-code", "", ""); err == nil {
		t.Error("callback accepted twice")
	}
	done := waitStatus(t, f, "srv", got.FlowID, OAuthConnected, OAuthFailed)
	if done.Status != OAuthConnected || done.HotReload == nil || !done.HotReload.OK {
		t.Fatalf("done = %+v", done)
	}
	if !st.HasCredentials("srv", cfg.URL) {
		t.Error("no credentials after sign-in")
	}
	if _, err := f.Status("other", got.FlowID); statusOf(err) != http.StatusNotFound {
		t.Errorf("flow of another server = %v, want 404", err)
	}
}

func TestOAuthFlowCallbackError(t *testing.T) {
	idp := newFakeIdP(t)
	f, _ := flowsFor(t, idp, newTestStore(t), nil)
	got, err := f.Start("srv", "https://gw.example/auth/mcp/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.AuthorizationURL)
	if _, err := f.Callback(u.Query().Get("state"), "", "access_denied", ""); statusOf(err) != http.StatusBadRequest {
		t.Errorf("error callback = %v", err)
	}
	done := waitStatus(t, f, "srv", got.FlowID, OAuthFailed)
	if done.Error != "Authorization was not completed (access_denied)." {
		t.Errorf("error = %q", done.Error)
	}
}

func TestOAuthFlowManualPaste(t *testing.T) {
	idp := newFakeIdP(t)
	activated := make(chan string, 1)
	f, _ := flowsFor(t, idp, newTestStore(t), func(_ context.Context, name string) HotReload {
		activated <- name
		return HotReload{RequiresRestart: true, Message: "restart"}
	})
	got, err := f.Start("srv", "http://192.168.1.5:18790/auth/mcp/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.CompletionInput != "callback_url" || got.RedirectURI != "http://127.0.0.1:18790/auth/mcp/callback" {
		t.Fatalf("start = %+v", got)
	}
	u, _ := url.Parse(got.AuthorizationURL)
	if u.Query().Get("redirect_uri") != got.RedirectURI {
		t.Errorf("authorization redirect_uri = %q", u.Query().Get("redirect_uri"))
	}
	state := u.Query().Get("state")
	for _, bad := range []string{
		"http://127.0.0.1:9999/auth/mcp/callback?code=x&state=" + state, // other port
		"http://127.0.0.1:18790/auth/mcp/callback",                      // no query
		"http://127.0.0.1:18790/auth/mcp/callback?state=" + state,       // no code
		"http://127.0.0.1:18790/auth/mcp/callback?code=a&code=b&state=" + state,
	} {
		if _, err := f.CompleteURL("srv", got.FlowID, bad); statusOf(err) != http.StatusBadRequest {
			t.Errorf("CompleteURL(%q) = %v, want 400", bad, err)
		}
	}
	if _, err := f.CompleteURL("srv", got.FlowID, "http://127.0.0.1:18790/auth/mcp/callback?code=x&state=other"); statusOf(err) != http.StatusGone {
		t.Errorf("other state = %v, want 410", err)
	}
	if _, err := f.CompleteURL("srv", got.FlowID, " http://127.0.0.1:18790/auth/mcp/callback?code=good-code&state="+state+" "); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, f, "srv", got.FlowID, OAuthAuthorized, OAuthFailed)
	if done.Status != OAuthAuthorized || done.HotReload == nil || !done.HotReload.RequiresRestart {
		t.Fatalf("done = %+v", done)
	}
	if n := <-activated; n != "srv" {
		t.Errorf("activated %q", n)
	}
}

func TestOAuthFlowCancelAndRestart(t *testing.T) {
	idp := newFakeIdP(t)
	f, _ := flowsFor(t, idp, newTestStore(t), nil)
	first, err := f.Start("srv", "https://gw.example/auth/mcp/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	// Starting again replaces the running flow.
	second, err := f.Start("srv", "https://gw.example/auth/mcp/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Status("srv", first.FlowID); st.Status != OAuthCancelled {
		t.Errorf("replaced flow = %+v", st)
	}
	u, _ := url.Parse(first.AuthorizationURL)
	if _, err := f.Callback(u.Query().Get("state"), "good-code", "", ""); statusOf(err) != http.StatusGone {
		t.Errorf("callback of a replaced flow = %v, want 410", err)
	}
	got, err := f.Cancel("srv", second.FlowID)
	if err != nil || got.Status != OAuthCancelled {
		t.Fatalf("Cancel = %+v, %v", got, err)
	}
}

func TestOAuthFlowExpires(t *testing.T) {
	idp := newFakeIdP(t)
	f, _ := flowsFor(t, idp, newTestStore(t), nil)
	f.ttl = 200 * time.Millisecond
	got, err := f.Start("srv", "https://gw.example/auth/mcp/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := f.Status("srv", got.FlowID); statusOf(err) != http.StatusNotFound {
		t.Errorf("expired flow = %v, want 404", err)
	}
}

func TestOAuthFlowRejectsUnsafeAuthorizationURL(t *testing.T) {
	idp := newFakeIdP(t)
	f, _ := flowsFor(t, idp, newTestStore(t), nil)
	f.allowHTTPAuthURL = false // the fake IdP's http:// URL is unsafe now
	got, err := f.Start("srv", "https://gw.example/auth/mcp/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, f, "srv", got.FlowID, OAuthFailed)
	if done.Error != "The MCP server returned an unsafe authorization URL." || done.AuthorizationURL != "" {
		t.Errorf("done = %+v", done)
	}
}

func statusOf(err error) int {
	var fe *OAuthFlowError
	if errors.As(err, &fe) {
		return fe.Status
	}
	return 0
}
