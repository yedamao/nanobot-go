package security

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestIsHTTPURLAndURLName(t *testing.T) {
	for s, want := range map[string]bool{
		"https://example.com/a.png": true,
		" HTTP://example.com/":      true,
		"ftp://example.com/a":       false,
		"out/a.png":                 false,
		"/abs/https://x":            false,
	} {
		if IsHTTPURL(s) != want {
			t.Errorf("IsHTTPURL(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]string{
		"https://example.com/img/cat%20one.png?sig=1#x": "cat one.png",
		"https://example.com/":                          "example.com",
		"https://example.com":                           "example.com",
	} {
		if got := URLName(s); got != want {
			t.Errorf("URLName(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestDownload(t *testing.T) {
	_, port := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.png":
			w.Write([]byte("png-bytes"))
		case "/big":
			w.Write([]byte(strings.Repeat("x", 100)))
		case "/redirect":
			http.Redirect(w, r, "http://10.0.0.1/x", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	r := newFakeResolver(map[string][][]string{"media.test": once("127.0.0.1")})
	lo := &Policy{Resolver: r, Whitelist: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	ctx := context.Background()
	base := "http://media.test:" + port

	data, err := lo.Download(ctx, base+"/a.png", 50)
	if err != nil || string(data) != "png-bytes" {
		t.Fatalf("Download = %q, %v", data, err)
	}
	if _, err := lo.Download(ctx, base+"/big", 50); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big: err = %v", err)
	}
	if _, err := lo.Download(ctx, base+"/missing", 50); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: err = %v", err)
	}
	if _, err := lo.Download(ctx, base+"/redirect", 50); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("redirect to a private address: err = %v", err)
	}
	// The default policy refuses loopback before connecting.
	var def *Policy
	if _, err := def.Download(ctx, "http://127.0.0.1:"+port+"/a.png", 50); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("default policy: err = %v", err)
	}
	if _, err := def.Download(ctx, "file:///etc/passwd", 50); err == nil {
		t.Error("file URL downloaded")
	}
}
