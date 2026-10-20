package config

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// API defaults. The port and timeout match nanobot's.
const (
	DefaultAPIHost    = "127.0.0.1"
	DefaultAPIPort    = 8900
	DefaultAPITimeout = 120
)

// EnvAPIKey overrides api.apiKey.
const EnvAPIKey = envPrefix + "API_KEY"

// APIConfig configures the OpenAI-compatible API: `nanobot serve` always
// runs it, `nanobot gateway` when Enabled.
type APIConfig struct {
	// Enabled runs the API inside `nanobot gateway`.
	Enabled bool `json:"enabled"`
	// Host and Port are where the API listens. The default host keeps it
	// on loopback; any other host requires APIKey. Port 0 picks a free
	// port.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Timeout is how many seconds one request's agent turn may run.
	Timeout int `json:"timeout"`
	// APIKey, when set, is the bearer token every route but /health
	// requires.
	APIKey string `json:"apiKey"`
}

// Addr returns host:port.
func (a APIConfig) Addr() string {
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

// TurnTimeout returns Timeout as a duration.
func (a APIConfig) TurnTimeout() time.Duration {
	return time.Duration(a.Timeout) * time.Second
}

func defaultAPI() APIConfig {
	return APIConfig{Host: DefaultAPIHost, Port: DefaultAPIPort, Timeout: DefaultAPITimeout}
}

func validateAPI(a APIConfig, bad func(string, ...any)) {
	if strings.TrimSpace(a.Host) == "" {
		bad("api.host must not be empty")
	}
	if a.Port < 0 || a.Port > 65535 {
		bad("api.port must be in [0, 65535], got %d", a.Port)
	}
	if a.Timeout <= 0 {
		bad("api.timeout must be positive, got %d", a.Timeout)
	}
}
