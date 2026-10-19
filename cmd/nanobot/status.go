package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/session"
)

// status prints where things are and which providers have keys. It never
// prints key material.
func (a *app) status(args []string) error {
	fs := a.newFlagSet("status", "status [--config path]")
	configFlag := fs.String("config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
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
	row("API keys:", "")
	for _, p := range config.Presets() {
		row("  "+p.Name, "%s", keySource(keyInFile[p.Name], a.getenv(config.EnvKeyName(p.Name)) != ""))
	}
	return w.Flush()
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
