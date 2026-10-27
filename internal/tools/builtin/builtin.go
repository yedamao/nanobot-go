// Package builtin implements nanobot's built-in tools: filesystem access,
// shell execution, web fetching and web search.
package builtin

import (
	"errors"
	"fmt"

	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// Config selects and configures the built-in tools.
type Config struct {
	// Workspace roots every filesystem path and the shell's working dir.
	Workspace *security.Workspace
	Exec      ExecConfig
	Web       WebFetchConfig
	// Search configures web_search, registered when web tools are on
	// (Web.Disabled false) and Search.Enabled.
	Search WebSearchConfig
}

// RegisterAll registers every enabled built-in tool with r.
func RegisterAll(r *tools.Registry, cfg Config) error {
	if cfg.Workspace == nil {
		return errors.New("register built-in tools: workspace is required")
	}
	ts := []tools.Tool{
		NewReadFile(cfg.Workspace),
		NewWriteFile(cfg.Workspace),
		NewEditFile(cfg.Workspace),
		NewListDir(cfg.Workspace),
	}
	if !cfg.Exec.Disabled {
		e, err := NewExec(cfg.Workspace, cfg.Exec)
		if err != nil {
			return fmt.Errorf("register built-in tools: %w", err)
		}
		ts = append(ts, e)
	}
	if !cfg.Web.Disabled {
		ts = append(ts, NewWebFetch(cfg.Web))
		if cfg.Search.Enabled() {
			ts = append(ts, NewWebSearch(cfg.Search))
		}
	}
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			return fmt.Errorf("register built-in tools: %w", err)
		}
	}
	return nil
}
