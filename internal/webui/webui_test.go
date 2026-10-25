package webui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("%s: CSP = %q", path, csp)
	}
	return rec
}

func TestHandlerRoutes(t *testing.T) {
	h := Handler()
	tests := []struct {
		path string
		code int
	}{
		{"/ui/", http.StatusOK},
		{"/ui/missing.js", http.StatusNotFound},
		{"/ui/assets/missing.js", http.StatusNotFound},
		{"/", http.StatusFound},
		{"/other", http.StatusNotFound},
	}
	for _, tt := range tests {
		if rec := get(t, h, tt.path); rec.Code != tt.code {
			t.Errorf("%s: status = %d, want %d", tt.path, rec.Code, tt.code)
		}
	}
}

var assetRef = regexp.MustCompile(`(?:src|href)="/ui/(assets/[^"]+\.(js|css))"`)

// TestBundle checks the committed build of webui/: the page loads only
// its own hashed bundle (no inline code, which the CSP would block) and
// the bundle speaks the channel's protocol.
func TestBundle(t *testing.T) {
	h := Handler()
	rec := get(t, h, "/ui/")
	if rec.Code != http.StatusOK {
		t.Fatalf("/ui/: status = %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{"<title>nanobot-go</title>", `<div id="root">`} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`).MatchString(page) || strings.Contains(page, "<style") ||
		strings.Contains(page, " style=") {
		t.Errorf("page has inline script or style:\n%s", page)
	}

	bundle := map[string]string{} // "js"/"css" → contents
	for _, m := range assetRef.FindAllStringSubmatch(page, -1) {
		rec := get(t, h, "/ui/"+m[1])
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", m[1], rec.Code)
		}
		ctype := map[string]string{"js": "text/javascript", "css": "text/css"}[m[2]]
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, ctype) {
			t.Errorf("%s: Content-Type = %q, want %s", m[1], got, ctype)
		}
		bundle[m[2]] += rec.Body.String()
	}
	if bundle["js"] == "" || bundle["css"] == "" {
		t.Fatalf("page references no JS or CSS bundle:\n%s", page)
	}

	// Minifiers pick their own quotes.
	quoted := func(s string) *regexp.Regexp {
		return regexp.MustCompile("[\"'`]" + regexp.QuoteMeta(s) + "[\"'`]")
	}
	for _, lit := range []string{
		"/ws", "?chat_id=", "nanobot-go.chatId",
		"hello", "history", "progress", "delta", "message", "error", "chat", "stop",
	} {
		if !quoted(lit).MatchString(bundle["js"]) {
			t.Errorf("JS bundle lacks the literal %q", lit)
		}
	}
	for _, sel := range []string{".msg.assistant.streaming", "#composer", "#sidebar", ".status[data-state=open]"} {
		if !strings.Contains(strings.ReplaceAll(bundle["css"], `"`, ""), sel) {
			t.Errorf("CSS bundle lacks %q", sel)
		}
	}
}
