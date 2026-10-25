package config

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// MCP transports. The names match nanobot's config values.
const (
	MCPStdio          = "stdio"
	MCPStreamableHTTP = "streamableHttp"
	MCPSSE            = "sse"
)

// DefaultMCPToolTimeout is nanobot's tool_timeout default, in seconds.
const DefaultMCPToolTimeout = 30

// MCPAllTools in enabledTools exposes every tool a server offers.
const MCPAllTools = "*"

var (
	validMCPName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	envVarRef    = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// MCPServerConfig configures one MCP server (tools.mcpServers.<name>). It
// mirrors nanobot's MCPServerConfig. Zero values mean "default"; use the
// accessor methods rather than reading the fields directly.
type MCPServerConfig struct {
	// Type is "stdio", "streamableHttp" or "sse". When empty it is
	// inferred: command means stdio; a url ending in /sse means sse; any
	// other url means streamableHttp.
	Type string `json:"type,omitempty"`
	// Command and Args start a stdio server.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Env is added to the stdio server's minimal environment.
	Env map[string]string `json:"env,omitempty"`
	// Cwd is the stdio server's working directory; ~ is expanded.
	Cwd string `json:"cwd,omitempty"`
	// URL is the endpoint of an HTTP (streamableHttp or sse) server.
	URL string `json:"url,omitempty"`
	// Headers are sent with every HTTP request, e.g. Authorization.
	Headers map[string]string `json:"headers,omitempty"`
	// ToolTimeout is the per-call timeout in seconds (0 means 30).
	ToolTimeout int `json:"toolTimeout,omitempty"`
	// EnabledTools lists the tools to expose, by their name on the server
	// or their mcp_<server>_<tool> name. Missing (null) means ["*"], all
	// tools; an empty list exposes none.
	EnabledTools []string `json:"enabledTools"`
	// Enabled turns the server off without deleting it (default true).
	Enabled *bool `json:"enabled,omitempty"`
}

// ValidMCPName reports whether name may name an MCP server.
func ValidMCPName(name string) bool { return validMCPName.MatchString(name) }

// Transport returns the configured or inferred transport.
func (m MCPServerConfig) Transport() string {
	switch {
	case m.Type != "":
		return m.Type
	case m.Command != "":
		return MCPStdio
	case strings.HasSuffix(strings.TrimRight(m.URL, "/"), "/sse"):
		return MCPSSE
	}
	return MCPStreamableHTTP
}

// IsEnabled reports whether the server should be connected.
func (m MCPServerConfig) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// Timeout returns the per-call tool timeout.
func (m MCPServerConfig) Timeout() time.Duration {
	if m.ToolTimeout <= 0 {
		return DefaultMCPToolTimeout * time.Second
	}
	return time.Duration(m.ToolTimeout) * time.Second
}

// AllowedTools returns enabledTools with its default applied.
func (m MCPServerConfig) AllowedTools() []string {
	if m.EnabledTools == nil {
		return []string{MCPAllTools}
	}
	return m.EnabledTools
}

// Expand replaces ${VAR} references in args, env, url and headers with
// values from getenv, so secrets can stay out of the config file. A
// reference to an unset variable is an error. The receiver is unchanged.
func (m MCPServerConfig) Expand(getenv func(string) (string, bool)) (MCPServerConfig, error) {
	var missing []string
	expand := func(s string) string {
		return envVarRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := envVarRef.FindStringSubmatch(ref)[1]
			v, ok := getenv(name)
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
	}
	out := m
	out.Args = make([]string, len(m.Args))
	for i, a := range m.Args {
		out.Args[i] = expand(a)
	}
	out.Env = expandMap(m.Env, expand)
	out.Headers = expandMap(m.Headers, expand)
	out.URL = expand(m.URL)
	if len(missing) > 0 {
		slices.Sort(missing)
		return MCPServerConfig{}, fmt.Errorf("environment variable(s) not set: %s", strings.Join(slices.Compact(missing), ", "))
	}
	return out, nil
}

func expandMap(m map[string]string, expand func(string) string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = expand(v)
	}
	return out
}

// validateMCP checks tools.mcpServers. Disabled servers are validated too,
// so turning one back on cannot surface a latent error.
func validateMCP(servers map[string]MCPServerConfig, bad func(string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		p := fmt.Sprintf("tools.mcpServers.%s", name)
		if !validMCPName.MatchString(name) {
			bad("tools.mcpServers: server name %q must match %s", name, validMCPName)
			p = fmt.Sprintf("tools.mcpServers[%q]", name)
		}
		hasCmd, hasURL := strings.TrimSpace(s.Command) != "", strings.TrimSpace(s.URL) != ""
		switch {
		case hasCmd && hasURL:
			bad("%s: set either command or url, not both", p)
		case !hasCmd && !hasURL:
			bad("%s: command or url is required", p)
		}
		switch s.Type {
		case "":
		case MCPStdio:
			if !hasCmd {
				bad("%s: type stdio needs a command", p)
			}
		case MCPStreamableHTTP, MCPSSE:
			if !hasURL {
				bad("%s: type %s needs a url", p, s.Type)
			}
		default:
			bad("%s.type %q is not one of %s, %s, %s", p, s.Type, MCPStdio, MCPStreamableHTTP, MCPSSE)
		}
		if hasURL && !strings.Contains(s.URL, "${") {
			if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				bad("%s.url must be an http(s) URL", p)
			}
		}
		if !hasCmd && (len(s.Args) > 0 || len(s.Env) > 0 || s.Cwd != "") {
			bad("%s: args, env and cwd only apply to stdio servers", p)
		}
		if !hasURL && len(s.Headers) > 0 {
			bad("%s: headers only apply to HTTP servers", p)
		}
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				bad("%s.env: invalid variable name %q", p, k)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
			if strings.TrimSpace(k) == "" || strings.ContainsAny(k, ": \t\r\n") {
				bad("%s.headers: invalid header name %q", p, k)
			}
		}
		if s.ToolTimeout < 0 {
			bad("%s.toolTimeout must not be negative, got %d", p, s.ToolTimeout)
		}
		for i, t := range s.EnabledTools {
			if strings.TrimSpace(t) == "" {
				bad("%s.enabledTools[%d] must not be empty", p, i)
			}
		}
	}
}
