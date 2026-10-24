package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// statusMCPTimeout bounds each server's connection in `status --mcp`.
const statusMCPTimeout = 10 * time.Second

// maxErrSummary caps an error in one-line summaries; a stdio server's
// stderr can make errors long.
const maxErrSummary = 160

// newMCP returns a Manager for the configured MCP servers, registering
// their tools in reg once connected.
func (a *app) newMCP(cfg *config.Config, reg *tools.Registry, logger *slog.Logger, verbose bool, opts ...mcp.Option) *mcp.Manager {
	// Load validated the whitelist; should parsing fail anyway, the
	// default policy (no whitelist) is the stricter fallback.
	ssrf, err := security.NewPolicy(cfg.Tools.SSRFWhitelist)
	if err != nil {
		ssrf = &security.Policy{}
	}
	opts = append([]mcp.Option{mcp.WithVersion(version), mcp.WithSSRFPolicy(ssrf)}, opts...)
	if a.mcpTransport != nil {
		opts = append(opts, mcp.WithTransport(a.mcpTransport))
	}
	if verbose {
		opts = append(opts, mcp.WithStderr(a.stderr))
	}
	return mcp.NewManager(cfg.Tools.MCPServers, reg, logger, opts...)
}

// connectMCP connects the servers before the first turn. Ctrl-C stops
// waiting for slow servers. With report set it prints a one-line summary
// to stderr.
func (a *app) connectMCP(ctx context.Context, m *mcp.Manager, report bool) error {
	if len(m.Status()) == 0 {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-a.interrupts:
			cancel()
		case <-stop:
		}
	}()
	_ = m.Connect(cctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if report {
		fmt.Fprintln(a.stderr, mcpSummary(m.Status()))
	}
	return nil
}

// mcpSummary is a one-line report such as
// "MCP: fs connected (3 tools), web failed: connect: timed out after 30s".
func mcpSummary(statuses []mcp.ServerStatus) string {
	parts := make([]string, 0, len(statuses))
	for _, st := range statuses {
		parts = append(parts, st.Name+" "+describeState(st))
	}
	return "MCP: " + strings.Join(parts, ", ")
}

func describeState(st mcp.ServerStatus) string {
	switch st.State {
	case mcp.StateConnected:
		return fmt.Sprintf("connected (%d tools)", len(st.Tools))
	case mcp.StateFailed:
		msg := "unknown error"
		if st.Err != nil {
			msg = st.Err.Error()
		}
		if r := []rune(msg); len(r) > maxErrSummary {
			msg = string(r[:maxErrSummary]) + "…"
		}
		return "failed: " + msg
	}
	return string(st.State)
}

// mcpStatusRows prints one row per configured server. Header and env
// values, which may hold secrets, are never printed, only their names;
// args are counted, not shown. live holds connection results for
// `status --mcp`, or is nil.
func mcpStatusRows(row func(k, format string, args ...any), servers map[string]config.MCPServerConfig, live []mcp.ServerStatus) {
	if len(servers) == 0 {
		row("MCP servers:", "none")
		return
	}
	row("MCP servers:", "%d configured", len(servers))
	states := map[string]mcp.ServerStatus{}
	for _, st := range live {
		states[st.Name] = st
	}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		var desc []string
		if s.Transport() == config.MCPStdio {
			target := s.Command
			if n := len(s.Args); n > 0 {
				target += fmt.Sprintf(" (%d args)", n)
			}
			desc = append(desc, config.MCPStdio+" "+target)
			if len(s.Env) > 0 {
				desc = append(desc, "env "+strings.Join(slices.Sorted(maps.Keys(s.Env)), ", "))
			}
		} else {
			desc = append(desc, s.Transport()+" "+redactURL(s.URL))
			if len(s.Headers) > 0 {
				desc = append(desc, "headers "+strings.Join(slices.Sorted(maps.Keys(s.Headers)), ", "))
			}
		}
		if allowed := s.AllowedTools(); len(allowed) == 0 {
			desc = append(desc, "tools none")
		} else {
			desc = append(desc, "tools "+strings.Join(allowed, ", "))
		}
		if s.ToolTimeout > 0 {
			desc = append(desc, fmt.Sprintf("timeout %s", s.Timeout()))
		}
		state := "configured"
		switch st, ok := states[name]; {
		case !s.IsEnabled():
			state = "disabled"
		case ok:
			state = describeState(st)
		}
		desc = append(desc, state)
		row("  "+name, "%s", strings.Join(desc, "; "))
	}
}

// redactURL keeps a URL's scheme, host and port. Credentials, query and
// path are dropped: some servers take tokens in any of them.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<url>"
	}
	out := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		out += "/..."
	}
	return out
}
