package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/yedamao/nanobot-go/internal/channels/telegram"
	"github.com/yedamao/nanobot-go/internal/channels/weixin"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// status prints where things are, which providers have keys and which MCP
// servers are configured. It never prints key material, MCP header or env
// values. With --mcp it also connects to the MCP servers and reports
// their state.
func (a *app) status(ctx context.Context, args []string) error {
	fs := a.newFlagSet("status", "status [--config path] [--mcp]")
	configFlag := fs.String("config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	probeMCP := fs.Bool("mcp", false, fmt.Sprintf("connect to the MCP servers (at most %s each) and report their state", statusMCPTimeout))
	if err := parse(fs, args); err != nil {
		return err
	}
	path, err := config.ResolvePath(*configFlag, a.getenv)
	if err != nil {
		return err
	}
	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	keyInFile := map[string]bool{}
	for _, p := range config.Presets() {
		keyInFile[p.Name] = cfg.Providers.Get(p.Name).APIKey != ""
	}
	apiKeyInFile := cfg.API.APIKey != ""
	cfg.ApplyEnv(a.getenv)
	if err := cfg.Validate(); err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	row := func(k, format string, args ...any) { fmt.Fprintf(w, "%s\t%s\n", k, fmt.Sprintf(format, args...)) }

	row("Config:", "%s%s", path, unless(exists, " (not found; run 'nanobot onboard')"))
	ws, err := cfg.WorkspacePath()
	if err != nil {
		return err
	}
	wsExists, err := fileExists(ws)
	if err != nil {
		return err
	}
	row("Workspace:", "%s%s", ws, unless(wsExists, " (missing)"))
	if db, err := session.SQLitePath(config.SessionsDir(path), ws); err != nil {
		row("Sessions:", "unavailable: %v", err)
	} else {
		dbExists, err := fileExists(db)
		if err != nil {
			return err
		}
		row("Sessions:", "%s%s", db, unless(dbExists, " (not created yet)"))
	}

	d := cfg.Agents.Defaults
	row("Model:", "%s", d.Model)
	if t, err := cfg.Resolve(d.Provider, d.Model); err != nil {
		row("Provider:", "%s (unresolved: %v)", d.Provider, err)
	} else {
		row("Provider:", "%s -> %s, model %s", d.Provider, t.Preset.Name, t.Model)
	}
	if len(d.FallbackModels) == 0 {
		row("Fallbacks:", "none")
	} else {
		var fbs []string
		for _, fb := range d.FallbackModels {
			p := fb.Provider
			if p == "" {
				p = config.ProviderAuto
			}
			fbs = append(fbs, p+":"+fb.Model)
		}
		row("Fallbacks:", "%s", strings.Join(fbs, ", "))
	}
	row("Restrict to workspace:", "%t", cfg.Tools.RestrictToWorkspace)
	row("Tools:", "exec %s, web_fetch %s", onOff(cfg.Tools.Exec.Enable), onOff(cfg.Tools.Web.Enable))
	var live []mcp.ServerStatus
	if *probeMCP && len(cfg.Tools.MCPServers) > 0 {
		m := a.newMCP(cfg, tools.NewRegistry(), nil, false, mcp.WithConnectTimeout(statusMCPTimeout))
		err := m.Connect(ctx)
		live = m.Status()
		m.Close()
		if err != nil {
			return err
		}
	}
	mcpStatusRows(row, cfg.Tools.MCPServers, live)
	row("Gateway:", "health http://%s/healthz, shutdown timeout %s", cfg.Gateway.Addr(), cfg.Gateway.ShutdownWait())
	row("Channels:", "%s", channelSummary(cfg.Channels, a.getenv, path))
	row("API:", "http://%s/v1, in the gateway %s, timeout %s, key %s", cfg.API.Addr(), onOff(cfg.API.Enabled),
		cfg.API.TurnTimeout(), keySource(apiKeyInFile, a.getenv(config.EnvAPIKey) != ""))
	row("API keys:", "")
	for _, p := range config.Presets() {
		row("  "+p.Name, "%s", keySource(keyInFile[p.Name], a.getenv(config.EnvKeyName(p.Name)) != ""))
	}
	return w.Flush()
}

// channelSummary lists the configured channels, whether each is enabled
// and, for Telegram, where its token comes from; for WeChat, whether a
// login is saved (or where the token comes from). Sections hold tokens,
// so nothing else of them is printed.
func channelSummary(sections map[string]json.RawMessage, getenv func(string) string, configPath string) string {
	if len(sections) == 0 {
		return "none"
	}
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		var head struct {
			Enabled bool   `json:"enabled"`
			Token   string `json:"token"`
		}
		state := "disabled"
		if err := json.Unmarshal(sections[name], &head); err != nil {
			state = "invalid"
		} else if head.Enabled {
			state = "enabled"
		}
		if name == telegram.Name && state != "invalid" {
			state += ", token " + keySource(head.Token != "", getenv(telegram.EnvToken) != "")
		}
		if name == weixin.Name && state != "invalid" {
			state += ", " + weixinLogin(sections[name], getenv, configPath)
		}
		parts = append(parts, name+" ("+state+")")
	}
	return strings.Join(parts, ", ")
}

// weixinLogin says where the WeChat channel's token comes from: a token
// in the config or the environment, or the login saved by `nanobot
// channels login weixin`.
func weixinLogin(raw json.RawMessage, getenv func(string) string, configPath string) string {
	var head struct {
		Token string `json:"token"`
	}
	json.Unmarshal(raw, &head)
	if getenv(weixin.EnvToken) != "" || strings.TrimSpace(head.Token) != "" {
		return "token " + keySource(head.Token != "", getenv(weixin.EnvToken) != "")
	}
	cfg, err := weixin.ParseConfig(raw, nil)
	if err != nil {
		return "invalid config"
	}
	switch ok, err := weixin.HasSavedLogin(cfg.StatePath(weixin.DefaultStateDir(configPath))); {
	case err != nil:
		return "saved login unreadable"
	case ok:
		return "logged in"
	}
	return "not logged in (run 'nanobot channels login weixin')"
}

func keySource(inFile, inEnv bool) string {
	switch {
	case inEnv:
		return "set (env)"
	case inFile:
		return "set (config)"
	}
	return "not set"
}

// unless returns s when cond is false.
func unless(cond bool, s string) string {
	if cond {
		return ""
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
