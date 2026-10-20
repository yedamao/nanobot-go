package config

import (
	"bytes"
	"encoding/json"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Gateway defaults. The port matches nanobot's.
const (
	DefaultGatewayHost            = "127.0.0.1"
	DefaultGatewayPort            = 18790
	DefaultGatewayShutdownTimeout = 30
)

// GatewayConfig configures `nanobot gateway`.
type GatewayConfig struct {
	// Host and Port are where the health endpoint listens. The default
	// host keeps it on loopback; port 0 picks a free port.
	Host string `json:"host"`
	Port int    `json:"port"`
	// ShutdownTimeout is how many seconds in-flight turns get to finish
	// after a shutdown signal before they are canceled.
	ShutdownTimeout int `json:"shutdownTimeout"`
}

// Addr returns host:port.
func (g GatewayConfig) Addr() string {
	return net.JoinHostPort(g.Host, strconv.Itoa(g.Port))
}

// ShutdownWait returns ShutdownTimeout as a duration.
func (g GatewayConfig) ShutdownWait() time.Duration {
	return time.Duration(g.ShutdownTimeout) * time.Second
}

func defaultGateway() GatewayConfig {
	return GatewayConfig{Host: DefaultGatewayHost, Port: DefaultGatewayPort, ShutdownTimeout: DefaultGatewayShutdownTimeout}
}

func validateGateway(g GatewayConfig, bad func(string, ...any)) {
	if strings.TrimSpace(g.Host) == "" {
		bad("gateway.host must not be empty")
	}
	if g.Port < 0 || g.Port > 65535 {
		bad("gateway.port must be in [0, 65535], got %d", g.Port)
	}
	if g.ShutdownTimeout < 0 {
		bad("gateway.shutdownTimeout must not be negative, got %d", g.ShutdownTimeout)
	}
}

// validateChannels checks that every channel section is a JSON object.
// The section's keys are checked by the channel's factory when the
// gateway builds it.
func validateChannels(sections map[string]json.RawMessage, bad func(string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		if strings.TrimSpace(name) == "" {
			bad("channels: empty channel name")
			continue
		}
		raw := bytes.TrimSpace(sections[name])
		if len(raw) == 0 || raw[0] != '{' {
			bad("channels.%s must be a JSON object", name)
		}
	}
}
