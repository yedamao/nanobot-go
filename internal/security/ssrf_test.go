package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeResolver answers from a fixed table and counts lookups per host.
// A host with several answers returns them in turn, repeating the last.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]string
	calls   map[string]int
}

func newFakeResolver(answers map[string][][]string) *fakeResolver {
	return &fakeResolver{answers: answers, calls: map[string]int{}}
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq, ok := r.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	n := r.calls[host]
	r.calls[host]++
	var addrs []netip.Addr
	for _, s := range seq[min(n, len(seq)-1)] {
		addrs = append(addrs, netip.MustParseAddr(s))
	}
	return addrs, nil
}

func (r *fakeResolver) count(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func once(addrs ...string) [][]string { return [][]string{addrs} }

func TestParseWhitelist(t *testing.T) {
	tests := []struct {
		in      []string
		want    []string
		wantErr string
	}{
		{in: nil, want: nil},
		{in: []string{"100.64.0.0/10", " 10.0.0.5/32 ", "fd00::/8"}, want: []string{"100.64.0.0/10", "10.0.0.5/32", "fd00::/8"}},
		{in: []string{"10.1.2.3/8"}, want: []string{"10.0.0.0/8"}}, // host bits ignored
		{in: []string{"192.168.1.10", "::1"}, want: []string{"192.168.1.10/32", "::1/128"}},
		{in: []string{"::ffff:10.0.0.0/104"}, want: []string{"10.0.0.0/8"}},
		{in: []string{"10.0.0.0/33"}, wantErr: "[0]"},
		{in: []string{"10.0.0.0/8", "not-a-cidr"}, wantErr: "[1]"},
		{in: []string{""}, wantErr: "[0]"},
		{in: []string{"fe80::1%eth0/128"}, wantErr: "zone"},
	}
	for _, tt := range tests {
		got, err := ParseWhitelist(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseWhitelist(%q) error = %v, want %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWhitelist(%q): %v", tt.in, err)
			continue
		}
		var gotS []string
		for _, p := range got {
			gotS = append(gotS, p.String())
		}
		if fmt.Sprint(gotS) != fmt.Sprint(tt.want) {
			t.Errorf("ParseWhitelist(%q) = %v, want %v", tt.in, gotS, tt.want)
		}
	}
}

func TestPolicyAllow(t *testing.T) {
	p, err := NewPolicy([]string{"100.100.1.2/32", "10.0.0.0/24", "fd7a:115c:a1e0::/48"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true}, // public stays allowed
		{"2606:4700:4700::1111", true},
		{"100.100.1.2", true}, // whitelisted CGNAT host
		{"100.100.1.3", false},
		{"10.0.0.200", true},
		{"::ffff:10.0.0.7", true}, // mapped form matches the IPv4 range
		{"10.0.1.1", false},
		{"fd7a:115c:a1e0::1", true},
		{"fd00::1", false},
		{"127.0.0.1", false},
		{"169.254.169.254", false},
		{"192.168.1.1", false},
	}
	for _, tt := range tests {
		if got := p.Allow(netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("Allow(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
	var nilPolicy *Policy
	if nilPolicy.Allow(netip.MustParseAddr("10.0.0.1")) || !nilPolicy.Allow(netip.MustParseAddr("1.1.1.1")) {
		t.Error("nil policy must block private and allow public addresses")
	}
}

func TestResolveAndValidate(t *testing.T) {
	r := newFakeResolver(map[string][][]string{
		"public.test":   once("93.184.216.34", "2606:2800:220:1::1"),
		"rfc1918.test":  once("192.168.0.10"),
		"loopback.test": once("127.0.0.1"),
		"metadata.test": once("169.254.169.254"),
		"cgnat.test":    once("100.64.1.1"),
		"mixed.test":    once("93.184.216.34", "10.0.0.1"),
		"mapped.test":   once("::ffff:127.0.0.1"),
		"tailnet.test":  once("100.101.102.103"),
		"empty.test":    once(),
	})
	p := &Policy{Resolver: r}
	wl := &Policy{Resolver: r, Whitelist: []netip.Prefix{netip.MustParsePrefix("100.101.102.103/32")}}
	tests := []struct {
		p       *Policy
		host    string
		want    string
		wantErr string
	}{
		{p, "public.test", "[93.184.216.34 2606:2800:220:1::1]", ""},
		{p, "8.8.8.8", "[8.8.8.8]", ""},
		{p, "[2606:4700:4700::1111]", "[2606:4700:4700::1111]", ""},
		{p, "rfc1918.test", "", "192.168.0.10"},
		{p, "loopback.test", "", "127.0.0.1"},
		{p, "metadata.test", "", "169.254.169.254"},
		{p, "cgnat.test", "", "100.64.1.1"},
		{p, "mixed.test", "", "10.0.0.1"},
		{p, "mapped.test", "", "127.0.0.1"},
		{p, "tailnet.test", "", "100.101.102.103"},
		{wl, "tailnet.test", "[100.101.102.103]", ""},
		{wl, "cgnat.test", "", "100.64.1.1"}, // only the /32 is whitelisted
		{p, "169.254.169.254", "", "169.254.169.254"},
		{p, "10.0.0.1", "", "10.0.0.1"},
		{p, "::1", "", "::1"},
		{p, "empty.test", "", "no addresses"},
		{p, "nxdomain.test", "", "resolve nxdomain.test"},
		{p, "", "", "empty host"},
	}
	for _, tt := range tests {
		ips, err := tt.p.ResolveAndValidate(context.Background(), tt.host)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ResolveAndValidate(%q) = %v, %v; want error %q", tt.host, ips, err, tt.wantErr)
			}
			continue
		}
		if err != nil || fmt.Sprint(ips) != tt.want {
			t.Errorf("ResolveAndValidate(%q) = %v, %v; want %s", tt.host, ips, err, tt.want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	r := newFakeResolver(map[string][][]string{
		"example.test":  once("93.184.216.34"),
		"internal.test": once("10.1.2.3"),
		"localhost":     once("127.0.0.1", "::1"),
	})
	p := &Policy{Resolver: r}
	lo := &Policy{Resolver: r, Whitelist: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	tests := []struct {
		p       *Policy
		url     string
		wantErr string
	}{
		{p, "http://example.test/x", ""},
		{p, "https://8.8.8.8/x", ""},
		{p, "https://[2606:4700:4700::1111]/", ""},
		{p, "http://internal.test/", "blocked"},
		{p, "http://127.0.0.1:8080/", "blocked"},
		{p, "http://169.254.169.254/latest/meta-data", "blocked"},
		{p, "http://100.64.0.1/", "blocked"},
		{p, "http://localhost/", "blocked"},
		{lo, "http://127.0.0.1:8080/", ""},
		{lo, "http://localhost/", "::1"}, // ::1 is not whitelisted
		{p, "ftp://example.test/", "not allowed"},
		{p, "file:///etc/passwd", "not allowed"},
		{p, "http:///nohost", "no host"},
		{p, "://bad", "invalid URL"},
		{nil, "http://10.0.0.1/", "blocked"},
	}
	for _, tt := range tests {
		err := CheckURL(context.Background(), tt.url, tt.p)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("CheckURL(%q) = %v", tt.url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("CheckURL(%q) = %v, want %q", tt.url, err, tt.wantErr)
		}
	}
}

// loopbackServer starts an httptest server and returns it with its port.
func loopbackServer(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return srv, u.Port()
}

// The rebinding resolver answers 127.0.0.1 (whitelisted, standing in for
// a public address) first and a private address afterwards. Within one
// pinned request the host is resolved exactly once, so the private answer
// is never seen, let alone dialled.
func TestPinnedDNSDefeatsRebinding(t *testing.T) {
	var hits atomic.Int32
	_, port := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/end", http.StatusFound) // same host again
			return
		}
		fmt.Fprint(w, "ok")
	}))
	r := newFakeResolver(map[string][][]string{"rebind.test": {{"127.0.0.1"}, {"10.0.0.1"}}})
	p := &Policy{Resolver: r, Whitelist: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	client := &http.Client{Transport: p.NewTransport(), CheckRedirect: p.CheckRedirect(5)}
	defer client.CloseIdleConnections()

	ctx := WithPinnedDNS(context.Background())
	target := "http://rebind.test:" + port + "/start"
	if err := CheckURL(ctx, target, p); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status %d after %d hits", resp.StatusCode, hits.Load())
	}
	if n := r.count("rebind.test"); n != 1 {
		t.Errorf("rebind.test resolved %d times in one pinned request, want 1", n)
	}

	// Without a pin set every dial resolves once and dials only what it
	// validated: the rebound (private) answer is refused before dialling.
	dial := p.DialContext(nil)
	if _, err := dial(context.Background(), "tcp", "rebind.test:"+port); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("dial after rebinding = %v, want ErrBlockedAddress", err)
	}
	if n := r.count("rebind.test"); n != 2 {
		t.Errorf("rebind.test resolved %d times, want 2", n)
	}
}

// The dialler connects to the validated address only: the host name
// itself is never handed to the system resolver.
func TestDialContextUsesResolvedAddresses(t *testing.T) {
	_, port := loopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := newFakeResolver(map[string][][]string{
		// Nothing listens on [::1]:port, so the dialler falls through
		// to the next validated address.
		"multi.test":  once("::1", "127.0.0.1"),
		"v6only.test": once("::1"),
	})
	p := &Policy{Resolver: r, Whitelist: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128"),
	}}
	dial := p.DialContext(nil)
	conn, err := dial(context.Background(), "tcp", "multi.test:"+port)
	if err != nil {
		t.Fatal(err)
	}
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:"+port {
		t.Errorf("dialled %s", got)
	}
	conn.Close()
	if _, err := dial(context.Background(), "tcp4", "v6only.test:"+port); err == nil || !strings.Contains(err.Error(), "no tcp4 address") {
		t.Errorf("tcp4 dial of an IPv6-only host = %v", err)
	}
	if _, err := dial(context.Background(), "tcp", "169.254.169.254:80"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("dial metadata = %v", err)
	}
	if _, err := dial(context.Background(), "tcp", "nxdomain.test:80"); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Errorf("dial unknown host = %v", err)
	}
}

func TestCheckRedirect(t *testing.T) {
	r := newFakeResolver(map[string][][]string{
		"public.test":  once("93.184.216.34"),
		"private.test": once("172.16.5.4"),
	})
	p := &Policy{Resolver: r}
	check := p.CheckRedirect(3)
	via := []*http.Request{{}}
	tests := []struct {
		target  string
		via     []*http.Request
		wantErr string
	}{
		{"https://public.test/next", via, ""},
		{"file:///etc/passwd", via, "not allowed"},
		{"gopher://public.test/", via, "not allowed"},
		{"http://169.254.169.254/latest/meta-data/", via, "blocked"},
		{"http://10.0.0.1/", via, "blocked"},
		{"http://[::1]:8080/", via, "blocked"},
		{"http://private.test/", via, "172.16.5.4"},
		{"http://nxdomain.test/", via, "resolve"},
		{"https://public.test/", make([]*http.Request, 3), "stopped after 3 redirects"},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.target)
		if err != nil {
			t.Fatal(err)
		}
		req := (&http.Request{URL: u}).WithContext(context.Background())
		err = check(req, tt.via)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("redirect to %s = %v", tt.target, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("redirect to %s = %v, want %q", tt.target, err, tt.wantErr)
		}
	}
}
