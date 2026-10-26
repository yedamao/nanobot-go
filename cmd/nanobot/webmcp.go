package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
)

// redacted stands for a value the WebUI is never shown.
const redacted = "***"

// mcpTestTimeout bounds the connection of POST /ui/mcp/{name}/test.
const mcpTestTimeout = 15 * time.Second

// maxMCPError caps a connection error shown in the WebUI.
const maxMCPError = 300

// secretArgRE matches flag and key names whose value looks secret.
var secretArgRE = regexp.MustCompile(`(?i)key|token|secret|passw|auth|credential|bearer|cookie|private`)

// secretValueRE matches values that look like well-known token formats.
var secretValueRE = regexp.MustCompile(`^(sk-|ghp_|gho_|github_pat_|xox[abp]-|glpat-|AKIA)`)

// webMCP is the WebUI's MCP server store (/ui/mcp). Like webSettings it
// loads the config file afresh for every change, sets only
// tools.mcpServers on it and saves it, under the settings' lock so the
// two never save over each other's change. Servers connect when the
// gateway starts: running holds the servers it started with, and live
// their connection states, to report which saved ones differ.
type webMCP struct {
	settings *webSettings
	running  map[string]config.MCPServerConfig
	// live returns the running Manager's states; nil when there is none.
	live   func() []mcp.ServerStatus
	getenv func(string) string
	// probe returns a throwaway Manager for cfg's MCP servers, connecting
	// within mcpTestTimeout; nil disables TestMCPServer.
	probe func(cfg *config.Config) *mcp.Manager
}

// newWebMCP returns the MCP store sharing settings' config file and lock.
func newWebMCP(settings *webSettings, running map[string]config.MCPServerConfig, live func() []mcp.ServerStatus, getenv func(string) string, probe func(*config.Config) *mcp.Manager) *webMCP {
	return &webMCP{settings: settings, running: maps.Clone(running), live: live, getenv: getenv, probe: probe}
}

// MCPServers implements websocket.MCPStore.
func (s *webMCP) MCPServers() (websocket.MCPList, error) {
	s.settings.mu.Lock()
	cfg, err := config.Load(s.settings.path)
	s.settings.mu.Unlock()
	if err != nil {
		return websocket.MCPList{}, err
	}
	saved := cfg.Tools.MCPServers
	states := s.states()
	out := websocket.MCPList{Servers: []websocket.MCPServerView{}, Live: states != nil, RestartRequired: []string{}}
	names := slices.Collect(maps.Keys(saved))
	for name, r := range s.running {
		if _, ok := saved[name]; !ok && r.IsEnabled() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		v := s.view(name, saved, states)
		if v.RestartRequired {
			out.RestartRequired = append(out.RestartRequired, name)
		}
		out.Servers = append(out.Servers, v)
	}
	return out, nil
}

// PutMCPServer implements websocket.MCPStore.
func (s *webMCP) PutMCPServer(name string, in websocket.MCPServerInput) (websocket.MCPServerView, bool, error) {
	if !config.ValidMCPName(name) {
		return websocket.MCPServerView{}, false, fmt.Errorf("%w: server name must be 1-64 letters, digits, _ or -", websocket.ErrInvalidMCP)
	}
	if slices.Contains(in.Args, redacted) {
		return websocket.MCPServerView{}, false, fmt.Errorf("%w: args hold a redacted value (%s); send the real value", websocket.ErrInvalidMCP, redacted)
	}
	var created bool
	cfg, err := s.update(func(servers map[string]config.MCPServerConfig) error {
		old, exists := servers[name]
		created = !exists
		srv := config.MCPServerConfig{
			Type:         strings.TrimSpace(in.Type),
			Command:      strings.TrimSpace(in.Command),
			Args:         in.Args,
			Cwd:          strings.TrimSpace(in.Cwd),
			URL:          strings.TrimSpace(in.URL),
			ToolTimeout:  in.ToolTimeout,
			EnabledTools: in.EnabledTools,
			Enabled:      in.Enabled,
		}
		// Missing env/headers keep the saved ones, which the page never
		// sees, as long as they still apply to the transport.
		if in.Env != nil {
			srv.Env = *in.Env
		} else if srv.Command != "" {
			srv.Env = old.Env
		}
		if in.Headers != nil {
			srv.Headers = *in.Headers
		} else if srv.URL != "" {
			srv.Headers = old.Headers
		}
		if len(srv.Env) == 0 {
			srv.Env = nil
		}
		if len(srv.Headers) == 0 {
			srv.Headers = nil
		}
		if in.Enabled == nil {
			srv.Enabled = old.Enabled
		} else if *in.Enabled {
			srv.Enabled = nil // the default
		}
		servers[name] = srv
		return nil
	})
	if err != nil {
		return websocket.MCPServerView{}, false, err
	}
	return s.view(name, cfg.Tools.MCPServers, s.states()), created, nil
}

// SetMCPServerEnabled implements websocket.MCPStore.
func (s *webMCP) SetMCPServerEnabled(name string, enabled bool) (websocket.MCPServerView, error) {
	cfg, err := s.update(func(servers map[string]config.MCPServerConfig) error {
		srv, ok := servers[name]
		if !ok {
			return websocket.ErrMCPNotFound
		}
		srv.Enabled = nil
		if !enabled {
			srv.Enabled = &enabled
		}
		servers[name] = srv
		return nil
	})
	if err != nil {
		return websocket.MCPServerView{}, err
	}
	return s.view(name, cfg.Tools.MCPServers, s.states()), nil
}

// DeleteMCPServer implements websocket.MCPStore.
func (s *webMCP) DeleteMCPServer(name string) error {
	_, err := s.update(func(servers map[string]config.MCPServerConfig) error {
		if _, ok := servers[name]; !ok {
			return websocket.ErrMCPNotFound
		}
		delete(servers, name)
		return nil
	})
	return err
}

// TestMCPServer implements websocket.MCPTester: it connects the saved
// server alone through a Manager of its own, lists its tools (ignoring
// enabledTools) and closes it, as the gateway would connect it after a
// restart. The running gateway's connections are untouched and nothing
// is saved. A disabled server, or one needing an unset ${VAR}, is not
// dialed.
func (s *webMCP) TestMCPServer(ctx context.Context, name string) (websocket.MCPTestResult, error) {
	if s.probe == nil {
		return websocket.MCPTestResult{}, errors.New("MCP test is not available")
	}
	s.settings.mu.Lock()
	cfg, err := config.Load(s.settings.path)
	s.settings.mu.Unlock()
	if err != nil {
		return websocket.MCPTestResult{}, err
	}
	srv, ok := cfg.Tools.MCPServers[name]
	if !ok {
		return websocket.MCPTestResult{}, websocket.ErrMCPNotFound
	}
	if !srv.IsEnabled() {
		return websocket.MCPTestResult{Status: websocket.MCPTestNotConfigured,
			Message: name + " is disabled: enable it to test."}, nil
	}
	// The error names the unset variables, never values.
	if _, err := srv.Expand(s.lookupEnv); err != nil {
		return websocket.MCPTestResult{Status: websocket.MCPTestNotConfigured,
			Message: "Cannot test " + name + ": " + err.Error() + "."}, nil
	}

	srv.EnabledTools = nil
	cfg.Tools.MCPServers = map[string]config.MCPServerConfig{name: srv}
	m := s.probe(cfg)
	defer m.Close()
	if err := m.Connect(ctx); err != nil {
		return websocket.MCPTestResult{Status: websocket.MCPTestError, Message: "The test of " + name + " was canceled."}, nil
	}
	st := m.Status()[0]
	switch {
	case st.State == mcp.StateConnected && len(st.Tools) == 0:
		return websocket.MCPTestResult{OK: true, Status: websocket.MCPTestOK,
			Message: name + " connected, but reported no tools."}, nil
	case st.State == mcp.StateConnected:
		noun := "tools"
		if len(st.Tools) == 1 {
			noun = "tool"
		}
		return websocket.MCPTestResult{OK: true, Status: websocket.MCPTestOK,
			Message: fmt.Sprintf("%s connected with %d %s.", name, len(st.Tools), noun), Tools: st.Tools}, nil
	case st.Err == nil:
		return websocket.MCPTestResult{Status: websocket.MCPTestError, Message: name + " did not complete an MCP handshake."}, nil
	}
	msg := st.Err.Error()
	switch {
	case srv.Transport() == config.MCPStdio && (errors.Is(st.Err, exec.ErrNotFound) || strings.Contains(msg, exec.ErrNotFound.Error())):
		return websocket.MCPTestResult{Status: websocket.MCPTestError,
			Message: s.scrub(fmt.Sprintf("%s needs %q, which is not on PATH.", name, srv.Command), srv)}, nil
	case strings.Contains(msg, "timed out after"):
		return websocket.MCPTestResult{Status: websocket.MCPTestError,
			Message: fmt.Sprintf("%s did not answer within %s.", name, mcpTestTimeout)}, nil
	case srv.Transport() != config.MCPStdio && (strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "Forbidden")):
		return websocket.MCPTestResult{Status: websocket.MCPTestRejected,
			Message: s.scrub(name+" refused access (HTTP 401/403): check its headers. "+msg, srv)}, nil
	}
	return websocket.MCPTestResult{Status: websocket.MCPTestError,
		Message: s.scrub("Cannot connect to "+name+": "+msg, srv)}, nil
}

// lookupEnv is getenv as the Manager looks variables up: empty is unset.
func (s *webMCP) lookupEnv(k string) (string, bool) {
	v := s.getenv(k)
	return v, v != ""
}

// update loads the config file, lets f change its servers, validates and
// saves it. Validation errors wrap websocket.ErrInvalidMCP.
func (s *webMCP) update(f func(map[string]config.MCPServerConfig) error) (*config.Config, error) {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	cfg, err := config.Load(s.settings.path)
	if err != nil {
		return nil, err
	}
	if cfg.Tools.MCPServers == nil {
		cfg.Tools.MCPServers = map[string]config.MCPServerConfig{}
	}
	if err := f(cfg.Tools.MCPServers); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", websocket.ErrInvalidMCP, err)
	}
	if err := cfg.Save(s.settings.path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// states returns the live states by server name, or nil without a
// running Manager.
func (s *webMCP) states() map[string]mcp.ServerStatus {
	if s.live == nil {
		return nil
	}
	out := map[string]mcp.ServerStatus{}
	for _, st := range s.live() {
		out[st.Name] = st
	}
	return out
}

// view describes server name, saved or (if removed) running, without
// secrets.
func (s *webMCP) view(name string, saved map[string]config.MCPServerConfig, states map[string]mcp.ServerStatus) websocket.MCPServerView {
	srv, ok := saved[name]
	running, wasRunning := s.running[name]
	if !ok {
		srv = running
	}
	v := websocket.MCPServerView{
		Name:            name,
		Enabled:         ok && srv.IsEnabled(),
		Transport:       srv.Transport(),
		Type:            srv.Type,
		Command:         srv.Command,
		Args:            redactArgs(srv.Args),
		Cwd:             srv.Cwd,
		URL:             redactMCPURL(srv.URL),
		EnvKeys:         slices.Sorted(maps.Keys(srv.Env)),
		HeaderKeys:      slices.Sorted(maps.Keys(srv.Headers)),
		ToolTimeout:     srv.ToolTimeout,
		EnabledTools:    srv.EnabledTools,
		RestartRequired: !sameMCPServer(srv, ok, running, wasRunning),
		Removed:         !ok,
	}
	switch st, live := states[name]; {
	case states == nil:
		v.State = "unknown"
	case !live:
		v.State = "not_running"
	default:
		v.State = string(st.State)
		if st.State == mcp.StateConnected {
			v.Tools = st.Tools
		}
		if st.Err != nil {
			v.Error = s.scrub(st.Err.Error(), running)
		}
	}
	return v
}

// sameMCPServer reports whether a saved server (ok when present) runs as
// configured: both off (missing or disabled), or both on and equal.
func sameMCPServer(saved config.MCPServerConfig, ok bool, running config.MCPServerConfig, wasRunning bool) bool {
	savedOn, runningOn := ok && saved.IsEnabled(), wasRunning && running.IsEnabled()
	if !savedOn || !runningOn {
		return savedOn == runningOn
	}
	saved.Enabled, running.Enabled = nil, nil
	return reflect.DeepEqual(saved, running)
}

// scrub removes the server's secrets, as configured and as expanded from
// the environment, from a connection error, and caps its length.
func (s *webMCP) scrub(msg string, srv config.MCPServerConfig) string {
	expanded, err := srv.Expand(s.lookupEnv)
	var secrets []string
	for _, c := range []config.MCPServerConfig{srv, expanded} {
		if c.URL != "" {
			msg = strings.ReplaceAll(msg, c.URL, redactMCPURL(c.URL))
		}
		for _, v := range slices.Concat(slices.Collect(maps.Values(c.Env)), slices.Collect(maps.Values(c.Headers))) {
			secrets = append(secrets, v)
			secrets = append(secrets, strings.Fields(v)...)
		}
		for i, a := range c.Args {
			if r := redactArgs(c.Args)[i]; r != a {
				secrets = append(secrets, a)
			}
		}
		if err != nil {
			break
		}
	}
	// Longest first, so a whole value goes before its parts.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, v := range secrets {
		if len(v) >= 6 && !strings.EqualFold(v, "bearer") {
			msg = strings.ReplaceAll(msg, v, redacted)
		}
	}
	if r := []rune(msg); len(r) > maxMCPError {
		msg = string(r[:maxMCPError]) + "…"
	}
	return msg
}

// redactArgs hides args that look secret: the value of a secret-looking
// flag (--api-key X, --token=X, KEY=X) and well-known token formats.
func redactArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if k, _, ok := strings.Cut(a, "="); ok && secretArgRE.MatchString(k) {
			out[i] = k + "=" + redacted
			continue
		}
		prev := ""
		if i > 0 {
			prev = args[i-1]
		}
		if strings.HasPrefix(prev, "-") && !strings.Contains(prev, "=") && secretArgRE.MatchString(prev) && !strings.HasPrefix(a, "-") ||
			secretValueRE.MatchString(a) {
			out[i] = redacted
		}
	}
	return out
}

// redactMCPURL drops a URL's credentials, query and fragment, which may
// hold tokens; ${VAR} references are kept as written.
func redactMCPURL(raw string) string {
	if raw == "" {
		return ""
	}
	out, cut := raw, false
	if i := strings.IndexAny(out, "?#"); i >= 0 {
		out, cut = out[:i], true
	}
	scheme, rest, ok := strings.Cut(out, "://")
	if ok {
		host, path, _ := strings.Cut(rest, "/")
		if j := strings.LastIndex(host, "@"); j >= 0 {
			host = host[j+1:]
		}
		out = scheme + "://" + host
		if path != "" || strings.HasSuffix(rest, "/") {
			out += "/" + path
		}
	}
	if cut {
		out += "?" + redacted
	}
	return out
}

var (
	_ websocket.MCPStore  = (*webMCP)(nil)
	_ websocket.MCPTester = (*webMCP)(nil)
)
