// Package webui is the browser chat page the gateway serves when the
// websocket channel is enabled: the React app in the repository's webui/
// directory, built by Vite into static/ (committed, so Go needs no Node)
// and embedded in the binary. Regenerate it with
// `cd webui && npm install && npm run build`. The page talks to the
// channel's WebSocket endpoint (/ws) and /ui/ endpoints on the same
// origin. It is a lean app of its own, not a port of nanobot's WebUI.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

// Prefix is the path the page is served under.
const Prefix = "/ui/"

//go:embed static
var files embed.FS

// Handler serves the embedded files under Prefix (GET /ui/ is the page;
// GET / redirects to it) with headers that keep the page from being
// framed or loading anything but its own files and WebSocket.
func Handler() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embed directive guarantees the directory
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+Prefix, http.StripPrefix(Prefix[:len(Prefix)-1], http.FileServerFS(static)))
	mux.Handle("GET /{$}", http.RedirectHandler(Prefix, http.StatusFound))
	return secureHeaders(mux)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
