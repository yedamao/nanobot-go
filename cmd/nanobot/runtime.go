package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/cron"
	"github.com/yedamao/nanobot-go/internal/mcp"
	"github.com/yedamao/nanobot-go/internal/memory"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools"
	"github.com/yedamao/nanobot-go/internal/tools/builtin"
)

// commonFlags are the flags of every command that runs the agent.
type commonFlags struct {
	config, workspace string
	verbose           bool
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	f := &commonFlags{}
	fs.StringVar(&f.config, "config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	fs.StringVar(&f.workspace, "workspace", "", "workspace directory (overrides the config)")
	fs.BoolVar(&f.verbose, "verbose", false, "log debug output to stderr")
	return f
}

// loadOptions tune load for a command.
type loadOptions struct {
	// level is the log level without --verbose (which means debug).
	level slog.Level
	// configure applies command flags to the config before it is
	// validated.
	configure func(*config.Config)
}

// setup is what every agent-running command needs.
type setup struct {
	cfg        *config.Config
	configPath string
	ws         *security.Workspace
	provider   providers.Provider
	logger     *slog.Logger
}

// load reads the config (file, then environment, then flags), creates the
// workspace and builds the provider and logger.
func (a *app) load(f *commonFlags, opts loadOptions) (*setup, error) {
	path, err := config.ResolvePath(f.config, a.getenv)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	cfg.ApplyEnv(a.getenv)
	if f.workspace != "" {
		cfg.Agents.Defaults.Workspace = f.workspace
	}
	if opts.configure != nil {
		opts.configure(cfg)
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
	// Like nanobot, an exec sandbox also confines the file tools: they run
	// in this process, outside the sandbox, and could otherwise read the
	// config directory the sandbox hides.
	ws, err := security.NewWorkspace(wsPath, cfg.Tools.RestrictToWorkspace || cfg.Tools.Exec.Sandbox != "")
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
	level := opts.level
	if f.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level}))
	return &setup{cfg: cfg, configPath: path, ws: ws, provider: provider, logger: logger}, nil
}

// runtimeOptions tune buildRuntime for a command.
type runtimeOptions struct {
	loadOptions
	// bus is the loop's bus; nil when turns only go through
	// ProcessDirect.
	bus *bus.MessageBus
	// hooks observe every run.
	hooks agent.Hooks
	// cronDeliver sends the replies of cron jobs run by this process; nil
	// when it runs none.
	cronDeliver cron.DeliverFunc
	// cronNote is appended to the cron tool's add results.
	cronNote string
}

// agentRuntime is the agent as `agent`, `gateway` and `cron run` run
// it: the loaded setup, the tools, sessions, MCP servers, loop and cron
// service.
type agentRuntime struct {
	*setup
	loop     *agent.Loop
	sessions *session.Manager
	// mcp is not connected yet: the command connects it before the first
	// turn, so it decides how long to wait and how to report.
	mcp *mcp.Manager
	// cron manages the workspace's jobs; its jobs run through runCronJob.
	cron *cron.Service
	// dream runs the Dream system job.
	dream *memory.Dream
	// runBound runs agent-turn jobs.
	runBound cron.RunFunc
}

// runCronJob runs one cron job: the Dream system job, or an agent turn
// in the job's chat.
func (rt *agentRuntime) runCronJob(ctx context.Context, job cron.Job) error {
	if job.System() && job.ID == cron.DreamJobID {
		res, err := rt.dream.Run(ctx)
		rt.logger.Info("cron: dream run", "pending", res.Pending, "processed", res.Processed,
			"completed", res.Outcome.Completed, "commit", res.Commit)
		return err
	}
	return rt.runBound(ctx, job)
}

// dreamSchedule returns the Dream system job's schedule, if Dream is
// scheduled.
func dreamSchedule(d config.DreamConfig) (cron.Schedule, bool) {
	switch {
	case strings.TrimSpace(d.Cron) != "":
		return cron.Schedule{Kind: cron.KindCron, Expr: strings.TrimSpace(d.Cron)}, true
	case d.IntervalH > 0:
		return cron.Schedule{Kind: cron.KindEvery, Every: time.Duration(d.IntervalH) * time.Hour}, true
	}
	return cron.Schedule{}, false
}

// syncDreamJob registers the Dream system job when Dream is scheduled
// and removes it otherwise, like nanobot's gateway startup.
func (rt *agentRuntime) syncDreamJob() (cron.Schedule, bool, error) {
	sched, ok := dreamSchedule(rt.cfg.Agents.Defaults.Dream)
	if !ok {
		return sched, false, rt.cron.RemoveSystemJob(cron.DreamJobID)
	}
	return sched, true, rt.cron.RegisterSystemJob(cron.Job{ID: cron.DreamJobID, Name: "dream", Schedule: sched})
}

// buildRuntime wires the tools, sessions, MCP servers and agent loop.
// Call close once the loop is done.
func (a *app) buildRuntime(f *commonFlags, opts runtimeOptions) (*agentRuntime, error) {
	s, err := a.load(f, opts.loadOptions)
	if err != nil {
		return nil, err
	}
	cfg, ws, provider, logger := s.cfg, s.ws, s.provider, s.logger
	reg := tools.NewRegistry()
	err = builtin.RegisterAll(reg, builtin.Config{
		Workspace: ws,
		Exec: builtin.ExecConfig{
			Disabled:       !cfg.Tools.Exec.Enable,
			Timeout:        time.Duration(cfg.Tools.Exec.Timeout) * time.Second,
			DenyPatterns:   cfg.Tools.Exec.DenyPatterns,
			AllowPatterns:  cfg.Tools.Exec.AllowPatterns,
			AllowEnv:       cfg.Tools.Exec.AllowedEnvKeys,
			Sandbox:        cfg.Tools.Exec.Sandbox,
			SandboxROBinds: cfg.Tools.Exec.SandboxRoBinds,
			SandboxRWBinds: cfg.Tools.Exec.SandboxRwBinds,
		},
		Web: builtin.WebFetchConfig{
			Disabled: !cfg.Tools.Web.Enable,
			MaxBytes: cfg.Tools.Web.FetchMaxBytes,
		},
	})
	if err != nil {
		return nil, err
	}
	rt := &agentRuntime{setup: s}
	rt.cron, err = cron.NewService(cron.ServiceConfig{Store: cron.NewStore(ws.Root), Run: rt.runCronJob, Logger: logger})
	if err != nil {
		return nil, err
	}
	if err := reg.Register(cron.NewTool(rt.cron, cron.ToolConfig{AddNote: opts.cronNote})); err != nil {
		return nil, err
	}
	if rt.dream, err = newDream(s, nil); err != nil {
		return nil, err
	}

	d := cfg.Agents.Defaults
	var consolidator *memory.Consolidator
	if d.Consolidation.Enable {
		consolidator, err = memory.NewConsolidator(memory.Config{
			Provider:            provider,
			Store:               memory.NewStore(ws.Root),
			Temperature:         &d.Temperature,
			ContextWindowTokens: d.ContextWindowTokens,
			ReplyMaxTokens:      d.MaxTokens,
			MaxMessages:         d.MaxHistoryMessages,
			SummaryMaxTokens:    d.Consolidation.SummaryMaxTokens,
			Logger:              logger,
		})
		if err != nil {
			return nil, err
		}
	}
	// Opened last so no earlier failure leaves the database open.
	store, err := session.Open(config.SessionsDir(s.configPath), ws.Root)
	if err != nil {
		return nil, err
	}
	sessions := session.NewManager(store)
	loop, err := agent.NewLoop(agent.LoopConfig{
		Bus:                opts.bus,
		Provider:           provider,
		Tools:              reg,
		Sessions:           sessions,
		Context:            agent.NewContextBuilder(ws.Root),
		MaxIterations:      d.MaxToolIterations,
		MaxTokens:          d.MaxTokens,
		Temperature:        &d.Temperature,
		MaxHistoryMessages: d.MaxHistoryMessages,
		Consolidator:       consolidator,
		Hooks:              opts.hooks,
		Logger:             logger,
	})
	if err != nil {
		sessions.Close()
		return nil, err
	}
	// MCP tools join reg once the caller connects the servers; the loop
	// reads the registry on every request.
	rt.loop = loop
	rt.sessions = sessions
	rt.mcp = a.newMCP(cfg, reg, logger, f.verbose)
	deliver := opts.cronDeliver
	if deliver == nil {
		deliver = func(context.Context, bus.OutboundMessage) error {
			return errors.New("no chat channels run in this process")
		}
	}
	rt.runBound = cron.BoundRunner(loop.ProcessMessage, deliver)
	return rt, nil
}

// close releases the runtime in reverse order of use: it waits for the
// loop's turns and background work (consolidation), then stops the MCP
// servers, then closes the session store.
func (rt *agentRuntime) close() error {
	rt.loop.Wait()
	rt.mcp.Close()
	if err := rt.sessions.Close(); err != nil {
		return fmt.Errorf("close sessions: %w", err)
	}
	return nil
}

// closeInto closes rt and joins any error into *err, for defers.
func (rt *agentRuntime) closeInto(err *error) {
	*err = errors.Join(*err, rt.close())
}
