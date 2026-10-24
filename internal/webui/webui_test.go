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
		{"/ui/style.css", http.StatusOK, "#composer"},
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
