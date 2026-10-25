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
	// DefaultHeartbeatIntervalS matches nanobot's heartbeat.interval_s
	// (30 minutes).
	DefaultHeartbeatIntervalS = 30 * 60
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
	// Heartbeat configures the heartbeat system job, which runs the
	// workspace's HEARTBEAT.md tasks periodically.
	Heartbeat HeartbeatConfig `json:"heartbeat"`
}

// HeartbeatConfig configures the heartbeat, like nanobot's
// gateway.heartbeat: while enabled, the gateway runs the active tasks of
// HEARTBEAT.md every IntervalS seconds and tells the most recent chat
// only what is worth telling.
type HeartbeatConfig struct {
	Enabled   bool `json:"enabled"`
	IntervalS int  `json:"intervalS"`
}

// Interval returns IntervalS as a duration.
func (h HeartbeatConfig) Interval() time.Duration {
	return time.Duration(h.IntervalS) * time.Second
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
	return GatewayConfig{
		Host:            DefaultGatewayHost,
		Port:            DefaultGatewayPort,
		ShutdownTimeout: DefaultGatewayShutdownTimeout,
		Heartbeat:       HeartbeatConfig{Enabled: true, IntervalS: DefaultHeartbeatIntervalS},
	}
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
	if h := g.Heartbeat; h.IntervalS < 0 || (h.Enabled && h.IntervalS == 0) {
		bad("gateway.heartbeat.intervalS must be positive, got %d", h.IntervalS)
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
