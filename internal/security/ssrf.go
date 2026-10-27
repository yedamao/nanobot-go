package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned (wrapped) when a URL, host or dialled
// address is rejected by a Policy.
var ErrBlockedAddress = errors.New("blocked internal/private address")

// Resolver looks up a host's addresses. *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Policy decides which addresses outgoing HTTP requests of agent tools may
// reach (SSRF protection). The zero value, like a nil *Policy, blocks
// everything IsBlockedIP reports and resolves with net.DefaultResolver.
// A Policy must not be copied after first use.
type Policy struct {
	// Whitelist ranges are reachable even if IsBlockedIP would block them
	// (tools.ssrfWhitelist, e.g. a Tailscale node's 100.x.y.z/32). Set it
	// before the Policy is shared; change it later with SetWhitelist.
	Whitelist []netip.Prefix
	// Resolver resolves host names; nil uses net.DefaultResolver.
	Resolver Resolver

	mu sync.RWMutex
}

// ParseWhitelist parses CIDR ranges such as "100.64.0.0/10". A bare
// address means just that host (/32 or /128). Host bits are ignored, like
// nanobot's ip_network(strict=False).
func ParseWhitelist(cidrs []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for i, s := range cidrs {
		s = strings.TrimSpace(s)
		var (
			p   netip.Prefix
			err error
		)
		if strings.Contains(s, "/") {
			p, err = netip.ParsePrefix(s)
		} else {
			var ip netip.Addr
			if ip, err = netip.ParseAddr(s); err == nil {
				p = netip.PrefixFrom(ip, ip.BitLen())
			}
		}
		if err != nil {
			return nil, fmt.Errorf("[%d] invalid CIDR %q: %w", i, s, err)
		}
		if p.Addr().Zone() != "" {
			return nil, fmt.Errorf("[%d] invalid CIDR %q: zones are not allowed", i, s)
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
		}
		prefixes = append(prefixes, p.Masked())
	}
	return prefixes, nil
}

// NewPolicy returns a Policy whitelisting cidrs (see ParseWhitelist).
func NewPolicy(cidrs []string) (*Policy, error) {
	wl, err := ParseWhitelist(cidrs)
	if err != nil {
		return nil, fmt.Errorf("ssrf whitelist: %w", err)
	}
	return &Policy{Whitelist: wl}, nil
}

// SetWhitelist replaces p's whitelist while it is in use, so the
// requests and connections dialled afterwards honor wl; connections
// already open are kept.
func (p *Policy) SetWhitelist(wl []netip.Prefix) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Whitelist = slices.Clone(wl)
}

// Allow reports whether ip may be reached: it is whitelisted or not
// blocked. IPv4-mapped IPv6 addresses are checked as IPv4.
func (p *Policy) Allow(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	if p != nil {
		p.mu.RLock()
		defer p.mu.RUnlock()
		for _, w := range p.Whitelist {
			if w.Contains(ip) {
				return true
			}
		}
	}
	return !IsBlockedIP(ip)
}

func (p *Policy) resolver() Resolver {
	if p == nil || p.Resolver == nil {
		return net.DefaultResolver
	}
	return p.Resolver
}

// ResolveAndValidate returns the addresses host resolves to if every one
// of them is allowed. A host resolving to both public and blocked
// addresses is rejected, like nanobot, since either may be the one used.
// A literal IP is checked without a lookup.
func (p *Policy) ResolveAndValidate(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return nil, errors.New("empty host")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !p.Allow(ip) {
			return nil, fmt.Errorf("%w %s", ErrBlockedAddress, ip)
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	ips := make([]netip.Addr, 0, len(addrs))
	for _, ip := range addrs {
		if !p.Allow(ip) {
			return nil, fmt.Errorf("%w %s for host %s", ErrBlockedAddress, ip, host)
		}
		ips = append(ips, ip.Unmap())
	}
	return ips, nil
}

// CheckURL validates rawURL with p (nil means the default policy); see
// Policy.CheckURL.
func CheckURL(ctx context.Context, rawURL string, p *Policy) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	_, err = p.CheckTarget(ctx, u)
	return err
}

// CheckTarget validates that u is http(s) with a host whose addresses are
// all allowed, and returns them. Within a context from WithPinnedDNS the
// result is pinned: later checks and dials of the same host reuse it
// instead of resolving again. Use it for the pre-flight check and for
// every redirect hop.
func (p *Policy) CheckTarget(ctx context.Context, u *url.URL) ([]netip.Addr, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL scheme %q not allowed (only http and https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL %q has no host", u.Redacted())
	}
	return p.pinned(ctx, host)
}

// CheckRedirect returns an http.Client CheckRedirect that stops after
// maxRedirects and runs CheckTarget on every hop before it is followed.
func (p *Policy) CheckRedirect(maxRedirects int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if _, err := p.CheckTarget(req.Context(), req.URL); err != nil {
			return fmt.Errorf("redirect to %s: %w", req.URL.Redacted(), err)
		}
		return nil
	}
}

// DialContext returns a dial function that resolves the host once with
// ResolveAndValidate (or takes the addresses pinned in ctx) and connects
// only to those addresses, trying them in order. The system resolver is
// never consulted again for the connection, so a DNS answer that changes
// between check and connect (rebinding) cannot redirect it. d supplies
// timeouts; nil uses a zero Dialer.
func (p *Policy) DialContext(d *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	if d != nil {
		dialer = *d
	}
	inner := dialer.Control
	// Defense in depth: the dialled address must itself be allowed.
	dialer.Control = func(network, address string, c syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w %q: %w", ErrBlockedAddress, address, err)
		}
		if !p.Allow(ap.Addr()) {
			return fmt.Errorf("%w %s", ErrBlockedAddress, ap.Addr())
		}
		if inner != nil {
			return inner(network, address, c)
		}
		return nil
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", address, err)
		}
		ips, err := p.pinned(ctx, host)
		if err != nil {
			return nil, err
		}
		var errs []error
		for _, ip := range ips {
			if (network == "tcp4" && !ip.Is4()) || (network == "tcp6" && !ip.Is6()) {
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		if len(errs) == 0 {
			return nil, fmt.Errorf("dial %s: no %s address among %v", address, network, ips)
		}
		return nil, errors.Join(errs...)
	}
}

// NewTransport returns a clone of http.DefaultTransport that dials through
// DialContext and uses no proxy: a proxy would make the dialled peer the
// proxy instead of the checked destination.
func (p *Policy) NewTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = p.DialContext(&net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive})
	return t
}

const (
	dialTimeout   = 10 * time.Second
	dialKeepAlive = 30 * time.Second
)

type pinsKey struct{}

// pinSet holds the validated addresses of each host seen in one request.
type pinSet struct {
	mu   sync.Mutex
	addr map[string][]netip.Addr
}

// WithPinnedDNS returns a context in which each host is resolved and
// validated at most once: CheckTarget, CheckRedirect and DialContext share
// the answer. Use one per logical request (including its redirects).
func WithPinnedDNS(ctx context.Context) context.Context {
	if _, ok := ctx.Value(pinsKey{}).(*pinSet); ok {
		return ctx
	}
	return context.WithValue(ctx, pinsKey{}, &pinSet{addr: map[string][]netip.Addr{}})
}

// pinned returns host's validated addresses, from ctx's pin set if it has
// them, resolving (and pinning) them otherwise.
func (p *Policy) pinned(ctx context.Context, host string) ([]netip.Addr, error) {
	pins, ok := ctx.Value(pinsKey{}).(*pinSet)
	if !ok {
		return p.ResolveAndValidate(ctx, host)
	}
	key := strings.ToLower(strings.TrimSuffix(host, "."))
	pins.mu.Lock()
	defer pins.mu.Unlock()
	if ips, ok := pins.addr[key]; ok {
		return ips, nil
	}
	ips, err := p.ResolveAndValidate(ctx, host)
	if err != nil {
		return nil, err
	}
	pins.addr[key] = ips
	return ips, nil
}
