package mcp

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/yedamao/nanobot-go/internal/config"
)

// ReloadResult reports a Reload, as nanobot's MCPProvider.reload does.
type ReloadResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	// Added, Changed and Removed are the servers whose config appeared,
	// differs or went away; Retried the unchanged enabled ones that were
	// not connected and were tried again.
	Added   []string `json:"added"`
	Changed []string `json:"changed"`
	Removed []string `json:"removed"`
	Retried []string `json:"retried"`
	// Connected are the servers connected after the reload; Failed those
	// of this reload's connection attempts that failed. Servers waiting
	// for an OAuth sign-in are neither.
	Connected  []string `json:"connected"`
	Failed     []string `json:"failed"`
	Configured []string `json:"configured"`
	// ToolsRemoved counts the tools unregistered from removed and changed
	// servers.
	ToolsRemoved int `json:"tools_removed"`
	// RequiresRestart is set only when the Manager is closing.
	RequiresRestart bool `json:"requires_restart"`
}

// closingResult is the Reload of a closed Manager.
func closingResult() ReloadResult {
	return ReloadResult{Message: "MCP connections are shutting down.", RequiresRestart: true}
}

// Reload reconciles the live servers with next, the full server set
// (disabled servers included), without touching the unchanged ones: a
// removed or changed server's tools are unregistered and its connection
// closed; added and changed servers connect, as do unchanged enabled
// servers that are not connected. OAuth servers without credentials are
// not dialed: they wait for a sign-in (StateAuthorizationRequired).
// Later Status, Reconnect and Connect calls see next. Reload returns an
// error, with RequiresRestart set, only once the Manager is closed.
func (m *Manager) Reload(ctx context.Context, next map[string]config.MCPServerConfig) (ReloadResult, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	res := ReloadResult{Added: []string{}, Changed: []string{}, Removed: []string{}, Retried: []string{},
		Connected: []string{}, Failed: []string{}, Configured: slices.Sorted(maps.Keys(next))}
	fresh := map[*server]bool{}
	var retired []*server

	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		return closingResult(), errClosed
	}
	current := map[string]*server{}
	for _, s := range m.servers {
		current[s.name] = s
	}
	servers := make([]*server, 0, len(next))
	for _, name := range res.Configured {
		cfg := next[name]
		s, ok := current[name]
		switch {
		case !ok:
			res.Added = append(res.Added, name)
		case !reflect.DeepEqual(s.cfg, cfg):
			res.Changed = append(res.Changed, name)
			retired = append(retired, s)
		default:
			servers = append(servers, s)
			continue
		}
		s = newServer(name, cfg)
		fresh[s] = true
		servers = append(servers, s)
	}
	for _, name := range slices.Sorted(maps.Keys(current)) {
		if _, ok := next[name]; !ok {
			res.Removed = append(res.Removed, name)
			retired = append(retired, current[name])
		}
	}
	m.servers = servers
	m.mu.Unlock()

	// Retire first: a changed server's new tools take the old names.
	for _, s := range retired {
		res.ToolsRemoved += m.retire(s)
	}

	var connect []*server
	for _, s := range servers {
		if !s.cfg.IsEnabled() {
			continue
		}
		s.mu.Lock()
		live := s.conn != nil
		s.mu.Unlock()
		if live && !fresh[s] {
			continue
		}
		if m.authorizationPending(s) {
			s.mu.Lock()
			s.state, s.err = StateAuthorizationRequired, ErrAuthorizationRequired
			s.mu.Unlock()
			continue
		}
		if !fresh[s] {
			res.Retried = append(res.Retried, s.name)
		}
		connect = append(connect, s)
	}

	errs := make([]error, len(connect))
	var wg sync.WaitGroup
	for i, s := range connect {
		wg.Go(func() { _, errs[i] = m.connect(ctx, s, nil) })
	}
	wg.Wait()
	for i, err := range errs {
		switch {
		case errors.Is(err, errClosed):
			return closingResult(), errClosed
		case err != nil && !errors.Is(err, ErrAuthorizationRequired):
			res.Failed = append(res.Failed, connect[i].name)
		}
	}
	for _, st := range m.Status() {
		if st.State == StateConnected {
			res.Connected = append(res.Connected, st.Name)
		}
	}

	res.OK = len(res.Failed) == 0
	unchanged := len(res.Added)+len(res.Changed)+len(res.Removed) == 0
	switch {
	case !res.OK:
		res.Message = "MCP config reloaded, but some servers did not connect: " + strings.Join(res.Failed, ", ")
	case unchanged && len(res.Retried) == 0:
		res.Message = "MCP config is already live."
	case unchanged:
		res.Message = "MCP connections refreshed without restarting the gateway."
	default:
		res.Message = "MCP config reloaded without restarting the gateway."
	}
	m.logger.Info("MCP hot reload", "added", res.Added, "changed", res.Changed, "removed", res.Removed,
		"retried", res.Retried, "connected", res.Connected, "failed", res.Failed, "tools_removed", res.ToolsRemoved)
	return res, nil
}

// retire unregisters s's tools and closes its connection for good,
// waiting for a connection attempt in flight. It returns how many tools
// it unregistered.
func (m *Manager) retire(s *server) int {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	s.mu.Lock()
	c, names := s.conn, s.tools
	m.reg.Unregister(names...)
	s.retired, s.conn, s.tools = true, nil, nil
	if s.state != StateDisabled {
		s.state = StateClosed
	}
	s.mu.Unlock()
	if c != nil {
		c.close()
	}
	return len(names)
}

// authorizationPending reports whether s uses OAuth and has no stored
// credentials, so a background connection could only fail.
func (m *Manager) authorizationPending(s *server) bool {
	if !s.cfg.UsesOAuth() || m.browsers[s.name] != nil {
		return false
	}
	if m.oauth == nil {
		return true
	}
	cfg, err := s.cfg.Expand(m.lookupEnv)
	return err == nil && !m.oauth.HasCredentials(s.name, cfg.URL)
}
