package security

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// blockedPrefixes are ranges never reachable by agent network tools, in
// addition to the loopback/private/link-local checks in IsBlockedIP.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this" network
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 can reach IPv4 internals
}

// IsBlockedIP reports whether ip is loopback, private, link-local (which
// includes cloud metadata at 169.254.169.254), multicast, unspecified or in
// another internal range.
func IsBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowPublicIP is the default IP policy: everything IsBlockedIP rejects.
func AllowPublicIP(ip netip.Addr) bool { return !IsBlockedIP(ip) }

// CheckURL validates that rawURL is http(s) and that every address its host
// resolves to passes allow. It is a pre-flight check; callers making the
// request should also check the dialled address to defeat DNS rebinding.
func CheckURL(ctx context.Context, rawURL string, allow func(netip.Addr) bool, resolver *net.Resolver) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme %q not allowed (only http and https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL %q has no host", rawURL)
	}
	if allow == nil {
		allow = AllowPublicIP
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !allow(ip) {
			return fmt.Errorf("blocked internal/private address %s", ip)
		}
		return nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range addrs {
		if !allow(ip) {
			return fmt.Errorf("blocked internal/private address %s for host %s", ip, host)
		}
	}
	return nil
}

var urlPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>|;&()]+`)

// ContainsInternalURL reports whether text contains a URL whose host is
// localhost or a literal internal IP address. It does not resolve DNS.
func ContainsInternalURL(text string) bool {
	for _, raw := range urlPattern.FindAllString(text, -1) {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return true
		}
		if ip, err := netip.ParseAddr(host); err == nil && IsBlockedIP(ip) {
			return true
		}
	}
	return false
}
