package security

import (
	"context"
	"net/netip"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"::1", true},
		{"::", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"::ffff:127.0.0.1", true},
		{"64:ff9b::a00:1", true},
	}
	for _, tt := range tests {
		if got := IsBlockedIP(netip.MustParseAddr(tt.ip)); got != tt.blocked {
			t.Errorf("IsBlockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
		}
	}
}

func TestCheckURL(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		url     string
		allow   func(netip.Addr) bool
		wantErr bool
	}{
		{"http://8.8.8.8/x", nil, false},
		{"https://[2606:4700:4700::1111]/", nil, false},
		{"http://127.0.0.1:8080/", nil, true},
		{"http://169.254.169.254/latest", nil, true},
		{"http://localhost/", nil, true}, // resolves to loopback
		{"http://127.0.0.1/", func(netip.Addr) bool { return true }, false},
		{"ftp://example.com/", nil, true},
		{"file:///etc/passwd", nil, true},
		{"http:///nohost", nil, true},
		{"://bad", nil, true},
	}
	for _, tt := range tests {
		err := CheckURL(ctx, tt.url, tt.allow, nil)
		if (err != nil) != tt.wantErr {
			t.Errorf("CheckURL(%q) = %v, wantErr %v", tt.url, err, tt.wantErr)
		}
	}
}

func TestContainsInternalURL(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"curl https://example.com", false},
		{"no urls here", false},
		{"see http://LOCALHOST:3000/api", true},
		{"x=http://192.168.0.10/y", true},
		{"https://8.8.8.8/dns", false},
	}
	for _, tt := range tests {
		if got := ContainsInternalURL(tt.text); got != tt.want {
			t.Errorf("ContainsInternalURL(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestParseLegacyIPv4(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" means not an address
	}{
		{"127.0.0.1", "127.0.0.1"},
		{"127.1", "127.0.0.1"},
		{"10.1.2", "10.1.0.2"},
		{"2130706433", "127.0.0.1"},
		{"0X7F000001", "127.0.0.1"},
		{"0177.0.0.01", "127.0.0.1"},
		{"0", "0.0.0.0"},
		{"256.1", ""},
		{"1.2.3.256", ""},
		{"1.16777216", ""},
		{"4294967296", ""},
		{"0x", ""},
		{"08", ""},
		{"", ""},
		{"1.2.3.4.5", ""},
		{"example.com", ""},
	}
	for _, tt := range tests {
		ip, ok := parseLegacyIPv4(tt.in)
		got := ""
		if ok {
			got = ip.String()
		}
		if got != tt.want {
			t.Errorf("parseLegacyIPv4(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
