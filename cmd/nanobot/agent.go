package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools"
	"github.com/yedamao/nanobot-go/internal/tools/builtin"
)

const (
	defaultSessionKey = "cli:direct"
	maxInputLine      = 1 << 20
	toolHintChars     = 80
)

// agent runs one message (-m) or an interactive session.
func (a *app) agent(ctx context.Context, args []string) error {
	fs := a.newFlagSet("agent", "agent [-m message] [-s session] [--config path] [--workspace dir] [--verbose]")
	var message, sessionKey string
	fs.StringVar(&message, "m", "", "send one message, print the reply and exit")
	fs.StringVar(&message, "message", "", "same as -m")
	fs.StringVar(&sessionKey, "s", defaultSessionKey, "session key")
	fs.StringVar(&sessionKey, "session", defaultSessionKey, "same as -s")
	configFlag := fs.String("config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	workspace := fs.String("workspace", "", "workspace directory (overrides the config)")
	verbose := fs.Bool("verbose", false, "log debug output to stderr")
	if err := parse(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(sessionKey) == "" {
		return errors.New("session key must not be empty")
	}

	loop, err := a.buildLoop(*configFlag, *workspace, *verbose)
	if err != nil {
		return err
	}
	defer loop.Wait()

	if message != "" {
		return a.oneShot(ctx, loop, message, sessionKey)
	}
	return a.repl(ctx, loop, sessionKey)
}

// buildLoop loads the config (file, then environment, then flags) and
// wires the provider, tools, sessions and agent loop.
func (a *app) buildLoop(configFlag, workspaceFlag string, verbose bool) (*agent.Loop, error) {
	path, err := config.ResolvePath(configFlag, a.getenv)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	cfg.ApplyEnv(a.getenv)
	if workspaceFlag != "" {
		cfg.Agents.Defaults.Workspace = workspaceFlag
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	wsPath, err := cfg.WorkspacePath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(wsPath, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	ws, err := security.NewWorkspace(wsPath, cfg.Tools.RestrictToWorkspace)
	if err != nil {
		return nil, err
	}
	reg := tools.NewRegistry()
	err = builtin.RegisterAll(reg, builtin.Config{
		Workspace: ws,
		Exec: builtin.ExecConfig{
			Disabled:      !cfg.Tools.Exec.Enable,
			Timeout:       time.Duration(cfg.Tools.Exec.Timeout) * time.Second,
			DenyPatterns:  cfg.Tools.Exec.DenyPatterns,
			AllowPatterns: cfg.Tools.Exec.AllowPatterns,
		},
		Web: builtin.WebFetchConfig{
			Disabled: !cfg.Tools.Web.Enable,
			MaxBytes: cfg.Tools.Web.FetchMaxBytes,
		},
	})
	if err != nil {
		return nil, err
	}

	var provider providers.Provider
	if a.newProvider != nil {
		provider, err = a.newProvider(cfg)
	} else {
		provider, _, err = cfg.BuildProvider(config.ProviderOptions{})
	}
	if err != nil {
		return nil, err
	}

	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	d := cfg.Agents.Defaults
	return agent.NewLoop(agent.LoopConfig{
		Provider:           provider,
		Tools:              reg,
		Sessions:           session.NewManager(config.SessionsDir(path)),
		Context:            agent.NewContextBuilder(ws.Root),
		MaxIterations:      d.MaxToolIterations,
		MaxTokens:          d.MaxTokens,
		Temperature:        &d.Temperature,
		MaxHistoryMessages: d.MaxHistoryMessages,
		Hooks:              &toolHints{w: a.stderr},
		Logger:             slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level})),
	})
}

// oneShot sends one message and prints the reply. Ctrl-C cancels it.
func (a *app) oneShot(ctx context.Context, loop *agent.Loop, message, key string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-a.interrupts:
			cancel()
		case <-ctx.Done():
		}
	}()
	reply, err := loop.ProcessDirect(ctx, message, key)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, reply)
	return nil
}

// repl reads one message per line until EOF, "exit" or "quit". Ctrl-C
// cancels the turn in progress, or exits when idle.
func (a *app) repl(ctx context.Context, loop *agent.Loop, key string) error {
	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(a.stdin)
		sc.Buffer(make([]byte, 0, 64*1024), maxInputLine)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
		close(lines)
	}()

	fmt.Fprintf(a.stdout, "nanobot-go interactive mode (session %s). Type /help for commands, exit or Ctrl-D to quit.\n", key)
	for {
		fmt.Fprint(a.stdout, "\nyou> ")
		var line string
		select {
		case l, ok := <-lines:
			if !ok {
				fmt.Fprintln(a.stdout)
				return <-readErr
			}
			line = strings.TrimSpace(l)
		case <-a.interrupts:
			fmt.Fprintln(a.stdout)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		switch strings.ToLower(line) {
		case "":
			continue
		case "exit", "quit", ":q":
			return nil
		}

		reply, err := a.turn(ctx, loop, line, key)
		switch {
		case errors.Is(err, context.Canceled) && ctx.Err() == nil:
			fmt.Fprintln(a.stdout, "(canceled)")
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			fmt.Fprintf(a.stderr, "error: %v\n", err)
		default:
			fmt.Fprintf(a.stdout, "\n%s\n", reply)
		}
	}
}

// turn runs one message, canceling it on Ctrl-C.
func (a *app) turn(ctx context.Context, loop *agent.Loop, line, key string) (string, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		reply string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reply, err := loop.ProcessDirect(turnCtx, line, key)
		done <- result{reply, err}
	}()
	select {
	case r := <-done:
		return r.reply, r.err
	case <-a.interrupts:
		cancel()
		r := <-done
		if r.err == nil {
			return r.reply, nil // finished just before the interrupt
		}
		return "", context.Canceled
	}
}

// toolHints prints a short line to w for each tool call.
type toolHints struct {
	agent.NopHooks
	mu sync.Mutex
	w  io.Writer
}

func (h *toolHints) OnToolStart(_ context.Context, call providers.ToolCall) {
	args := strings.Join(strings.Fields(string(call.Arguments)), " ")
	if r := []rune(args); len(r) > toolHintChars {
		args = string(r[:toolHintChars]) + "…"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(h.w, "  ↳ %s %s\n", call.Name, args)
}
