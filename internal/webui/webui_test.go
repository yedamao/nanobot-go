package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	tests := []struct {
		path     string
		code     int
		contains string
	}{
		{"/ui/", http.StatusOK, "<title>nanobot-go</title>"},
		{"/ui/app.js", http.StatusOK, `"/ws"`},
		{"/ui/app.js", http.StatusOK, `case "history":`},
		{"/ui/app.js", http.StatusOK, `case "progress":`},
		{"/ui/app.js", http.StatusOK, `{ type: "stop" }`},
		{"/ui/", http.StatusOK, `<aside id="sidebar"`},
		{"/ui/", http.StatusOK, `<ul id="chats">`},
		{"/ui/app.js", http.StatusOK, `fetch("chats"`},
		{"/ui/app.js", http.StatusOK, `aria-current`},
		{"/ui/style.css", http.StatusOK, "#composer"},
		{"/ui/style.css", http.StatusOK, "#sidebar"},
		{"/ui/", http.StatusOK, `<button id="show-status"`},
		{"/ui/", http.StatusOK, `<dialog id="status-panel"`},
		{"/ui/", http.StatusOK, `<dl id="status-list">`},
		{"/ui/app.js", http.StatusOK, `fetch("status"`},
		{"/ui/app.js", http.StatusOK, `showModal()`},
		{"/ui/style.css", http.StatusOK, "#status-panel"},
		{"/ui/app.js", http.StatusOK, `fetch("chats/" + encodeURIComponent(id)`},
		{"/ui/app.js", http.StatusOK, `"PATCH", { title }`},
		{"/ui/app.js", http.StatusOK, `"DELETE"`},
		{"/ui/app.js", http.StatusOK, `confirm("Delete this chat?`},
		{"/ui/app.js", http.StatusOK, `"Rename this chat"`},
		{"/ui/app.js", http.StatusOK, `switchChat(null)`},
		{"/ui/style.css", http.StatusOK, ".chat-actions"},
		{"/ui/missing.js", http.StatusNotFound, ""},
		{"/", http.StatusFound, ""},
		{"/other", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.code {
				t.Fatalf("status = %d, want %d", rec.Code, tt.code)
			}
			if !strings.Contains(rec.Body.String(), tt.contains) {
				t.Errorf("body does not contain %q", tt.contains)
			}
			if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
				t.Errorf("CSP = %q", csp)
			}
		})
	}
}
