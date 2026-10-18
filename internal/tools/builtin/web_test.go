package builtin

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func allowAll(netip.Addr) bool { return true }

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>T</title><style>body{color:red}</style>
<script>alert("x")</script></head><body><h1>Hello &amp; welcome</h1>
<p>First   paragraph.</p><!-- secret comment --><p>Second<br>line</p></body></html>`)
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ua":%q}`, r.UserAgent())
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", 5000))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G', 0, 0})
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nothing here", http.StatusNotFound)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/json", http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWebFetch(t *testing.T) {
	srv := newTestServer(t)
	tool := NewWebFetch(WebFetchConfig{AllowIP: allowAll, MaxBytes: 1000, UserAgent: "test-agent"})

	tests := []struct {
		name    string
		args    map[string]any
		has     []string
		hasNot  []string
		wantErr string
	}{
		{
			name:   "html to text",
			args:   map[string]any{"url": srv.URL + "/page"},
			has:    []string{"Status: 200", untrustedNote, "Hello & welcome", "First paragraph.", "Second\nline"},
			hasNot: []string{"alert", "color:red", "secret comment", "<p>"},
		},
		{name: "json and user agent", args: map[string]any{"url": srv.URL + "/json"}, has: []string{`{"ua":"test-agent"}`}},
		{name: "redirect followed", args: map[string]any{"url": srv.URL + "/redirect"}, has: []string{"URL: " + srv.URL + "/json"}},
		{name: "body cap", args: map[string]any{"url": srv.URL + "/big"}, has: []string{"body truncated at 1000 bytes"}},
		{name: "max chars", args: map[string]any{"url": srv.URL + "/big", "max_chars": 10}, has: []string{"aaaaaaaaaa\n... (truncated, 990 more characters)"}},
		{name: "http error", args: map[string]any{"url": srv.URL + "/missing"}, wantErr: "HTTP 404"},
		{name: "binary", args: map[string]any{"url": srv.URL + "/bin"}, wantErr: "unsupported content type"},
		{name: "redirect to file", args: map[string]any{"url": srv.URL + "/to-file"}, wantErr: "not allowed"},
		{name: "redirect loop", args: map[string]any{"url": srv.URL + "/loop"}, wantErr: "redirects"},
		{name: "bad scheme", args: map[string]any{"url": "file:///etc/passwd"}, wantErr: "not allowed"},
		{name: "no host", args: map[string]any{"url": "http:///x"}, wantErr: "no host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := run(t, tool, tt.args)
			if tt.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Content, tt.wantErr) {
					t.Fatalf("got %+v, want error %q", res, tt.wantErr)
				}
				return
			}
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			for _, h := range tt.has {
				if !strings.Contains(res.Content, h) {
					t.Errorf("missing %q in:\n%s", h, res.Content)
				}
			}
			for _, h := range tt.hasNot {
				if strings.Contains(res.Content, h) {
					t.Errorf("unexpected %q in:\n%s", h, res.Content)
				}
			}
		})
	}
}

func TestWebFetchBlocksPrivateAddressesByDefault(t *testing.T) {
	srv := newTestServer(t)
	tool := NewWebFetch(WebFetchConfig{}) // default policy: public IPs only
	for _, u := range []string{
		srv.URL + "/json",
		strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/json",
	} {
		res := run(t, tool, map[string]any{"url": u})
		if !res.IsError || !strings.Contains(res.Content, "blocked internal/private address") {
			t.Errorf("fetch %s: got %+v", u, res)
		}
	}
}

// A permitted host that redirects to a blocked address must not get the
// request through: every hop is dialled through the address check.
func TestWebFetchRedirectToBlockedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	var hit atomic.Bool
	internal := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		fmt.Fprint(w, "internal secret")
	}))
	internal.Listener.Close()
	internal.Listener = ln
	internal.Start()
	t.Cleanup(internal.Close)

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/", http.StatusFound)
	}))
	t.Cleanup(public.Close)

	// Treat 127.0.0.1 as "public" and everything else as internal.
	onlyV4Loopback := func(ip netip.Addr) bool { return ip == netip.MustParseAddr("127.0.0.1") }
	res := run(t, NewWebFetch(WebFetchConfig{AllowIP: onlyV4Loopback}), map[string]any{"url": public.URL})
	if !res.IsError || !strings.Contains(res.Content, "blocked internal/private address") {
		t.Errorf("got %+v", res)
	}
	if hit.Load() || strings.Contains(res.Content, "internal secret") {
		t.Error("redirect reached the blocked server")
	}
}

func TestWebFetchTimeout(t *testing.T) {
	srv := newTestServer(t)
	tool := NewWebFetch(WebFetchConfig{AllowIP: allowAll, Timeout: 200 * time.Millisecond})
	start := time.Now()
	res := run(t, tool, map[string]any{"url": srv.URL + "/slow"})
	if !res.IsError {
		t.Fatalf("got %+v", res)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("timeout not enforced")
	}
}

func TestHTMLToText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"<p>a</p><p>b</p>", "a\n\nb"},
		{"x&nbsp;&lt;y&gt;", "x <y>"},
		{"<SCRIPT type=x>bad()</SCRIPT>ok", "ok"},
		{"<ul><li>one</li><li>two</li></ul>", "one\n\ntwo"},
		{"  lots   of\t\tspace  ", "lots of space"},
	}
	for _, tt := range tests {
		if got := HTMLToText(tt.in); got != tt.want {
			t.Errorf("HTMLToText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
