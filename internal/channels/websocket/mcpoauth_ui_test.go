package websocket

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeMCPOAuth knows the OAuth server "linear" and flow "f1".
type fakeMCPOAuth struct {
	redirect string
	reset    bool
	pasted   string
	callback []string
}

func (f *fakeMCPOAuth) StartMCPOAuth(name, redirectURI string, reset bool) (MCPOAuthFlow, error) {
	if name != "linear" {
		return MCPOAuthFlow{}, ErrMCPNotFound
	}
	f.redirect, f.reset = redirectURI, reset
	return MCPOAuthFlow{FlowID: "f1", Name: name, Status: "authorization_required", AuthorizationURL: "https://as.example/authorize?state=s1"}, nil
}

func (f *fakeMCPOAuth) MCPOAuthStatus(name, id string) (MCPOAuthFlow, error) {
	if id != "f1" {
		return MCPOAuthFlow{}, &MCPOAuthError{Message: "Unknown or expired MCP OAuth flow.", Status: http.StatusNotFound}
	}
	return MCPOAuthFlow{FlowID: id, Name: name, Status: "connected"}, nil
}

func (f *fakeMCPOAuth) CompleteMCPOAuth(name, id, cb string) (MCPOAuthFlow, error) {
	f.pasted = cb
	return MCPOAuthFlow{FlowID: id, Name: name, Status: "connecting"}, nil
}

func (f *fakeMCPOAuth) CancelMCPOAuth(name, id string) (MCPOAuthFlow, error) {
	return MCPOAuthFlow{FlowID: id, Name: name, Status: "cancelled"}, nil
}

func (f *fakeMCPOAuth) MCPOAuthCallback(state, code, oauthError, iss string) (string, error) {
	f.callback = []string{state, code, oauthError, iss}
	if state != "s1" {
		return "", &MCPOAuthError{Message: "This MCP authorization request has expired.", Status: http.StatusGone}
	}
	return "<linear>", nil
}

func TestMCPOAuthRoutes(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodPost, MCPPath+"/linear/oauth/start", ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("start without MCPOAuth = %d", code)
	}

	f := &fakeMCPOAuth{}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", MCPOAuth: f})
	code, body := skillsRequest(t, h, http.MethodPost, MCPPath+"/linear/oauth/start", `{"reset":true}`)
	if code != http.StatusOK || !strings.Contains(string(body), `"authorization_url":"https://as.example/authorize?state=s1"`) {
		t.Fatalf("start = %d %s", code, body)
	}
	// The callback is on the origin the page was loaded from.
	if want := h.srv.URL + MCPOAuthCallbackPath; f.redirect != want || !f.reset {
		t.Errorf("redirect = %q reset %v, want %q", f.redirect, f.reset, want)
	}
	if code, _ := skillsRequest(t, h, http.MethodPost, MCPPath+"/linear/oauth/start", ""); code != http.StatusOK || f.reset {
		t.Errorf("start without body = %d reset %v", code, f.reset)
	}
	if code, _ := skillsRequest(t, h, http.MethodPost, MCPPath+"/nope/oauth/start", ""); code != http.StatusNotFound {
		t.Errorf("unknown server = %d", code)
	}

	if code, body := skillsRequest(t, h, http.MethodGet, MCPPath+"/linear/oauth/status?flow_id=f1", ""); code != http.StatusOK || !strings.Contains(string(body), `"status":"connected"`) {
		t.Errorf("status = %d %s", code, body)
	}
	if code, _ := skillsRequest(t, h, http.MethodGet, MCPPath+"/linear/oauth/status", ""); code != http.StatusBadRequest {
		t.Errorf("status without flow = %d", code)
	}
	if code, body := skillsRequest(t, h, http.MethodGet, MCPPath+"/linear/oauth/status?flow_id=zz", ""); code != http.StatusNotFound || !strings.Contains(string(body), "Unknown or expired") {
		t.Errorf("unknown flow = %d %s", code, body)
	}

	if code, _ := skillsRequest(t, h, http.MethodPost, MCPPath+"/linear/oauth/complete", `{"flow_id":"f1"}`); code != http.StatusBadRequest {
		t.Errorf("complete without URL = %d", code)
	}
	cb := "http://127.0.0.1:18790/auth/mcp/callback?code=c&state=s1"
	if code, _ := skillsRequest(t, h, http.MethodPost, MCPPath+"/linear/oauth/complete", `{"flow_id":"f1","callback_url":"`+cb+`"}`); code != http.StatusOK || f.pasted != cb {
		t.Errorf("complete = %d pasted %q", code, f.pasted)
	}
	long := `{"flow_id":"f1","callback_url":"http://x/` + strings.Repeat("a", maxMCPOAuthCallbackURL) + `"}`
	if code, _ := skillsRequest(t, h, http.MethodPost, MCPPath+"/linear/oauth/complete", long); code != http.StatusBadRequest {
		t.Errorf("long callback = %d", code)
	}
	if code, body := skillsRequest(t, h, http.MethodPost, MCPPath+"/linear/oauth/cancel", `{"flow_id":"f1"}`); code != http.StatusOK || !strings.Contains(string(body), "cancelled") {
		t.Errorf("cancel = %d %s", code, body)
	}

	// Mutations are same-origin only.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+MCPPath+"/linear/oauth/start", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin start = %d", resp.StatusCode)
	}
}

func TestMCPOAuthCallbackPage(t *testing.T) {
	f := &fakeMCPOAuth{}
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", MCPOAuth: f})
	get := func(query string) (int, string, http.Header) {
		resp, err := h.srv.Client().Get(h.srv.URL + MCPOAuthCallbackPath + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data), resp.Header
	}
	code, page, hdr := get("?code=c1&state=s1&iss=https%3A%2F%2Fas.example")
	if code != http.StatusOK || !strings.Contains(page, "Authorization received for &lt;linear&gt;") || strings.Contains(page, "<linear>") {
		t.Errorf("callback = %d %s", code, page)
	}
	if strings.Join(f.callback, ",") != "s1,c1,,https://as.example" {
		t.Errorf("delivered %v", f.callback)
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") || hdr.Get("Referrer-Policy") != "no-referrer" || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", hdr)
	}
	if code, page, _ := get("?code=c1"); code != http.StatusBadRequest || !strings.Contains(page, "missing its security state") {
		t.Errorf("no state = %d %s", code, page)
	}
	if code, page, _ := get("?code=c1&state=old"); code != http.StatusGone || !strings.Contains(page, "expired") {
		t.Errorf("stale state = %d %s", code, page)
	}
}
