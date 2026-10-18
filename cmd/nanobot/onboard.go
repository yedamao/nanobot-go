package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/config"
)

// onboard writes a default config file (unless one exists) and creates the
// workspace with its bootstrap files.
func (a *app) onboard(args []string) error {
	fs := a.newFlagSet("onboard", "onboard [--config path] [--force]")
	configFlag := fs.String("config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	force := fs.Bool("force", false, "overwrite an existing config file with defaults")
	if err := parse(fs, args); err != nil {
		return err
	}
	path, err := config.ResolvePath(*configFlag, a.getenv)
	if err != nil {
		return err
	}

	cfg := config.Default()
	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	switch {
	case exists && !*force:
		if cfg, err = config.Load(path); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Config already exists at %s (kept; use --force to reset it to defaults).\n", path)
	default:
		if err := cfg.Save(path); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Wrote config to %s\n", path)
	}

	ws, err := cfg.WorkspacePath()
	if err != nil {
		return err
	}
	created, err := agent.WriteBootstrapFiles(ws)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Workspace: %s\n", ws)
	if len(created) > 0 {
		fmt.Fprintf(a.stdout, "Created %s\n", strings.Join(created, ", "))
	}
	fmt.Fprintf(a.stdout, `
Next steps:
  1. Add an API key to %s, for example:
       "providers": {"anthropic": {"apiKey": "sk-ant-..."}}
     or export %s.
  2. Chat: nanobot agent -m "Hello!"
`, path, config.EnvKeyName("anthropic"))
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}
