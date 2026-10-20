package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/cron"
)

const cronUsage = `Usage:
  nanobot cron list [--all] [--config path] [--workspace dir]
  nanobot cron remove <id> [--config path] [--workspace dir]
  nanobot cron run <id> [--force] [--config path] [--workspace dir] [--verbose]

list and remove only touch <workspace>/cron/jobs.json and need no API key.
run runs a job's turn now in this process, prints the reply (it is not
sent to the chat: no channels run here) and records the run.
`

// cronCmd manages the workspace's scheduled jobs. Jobs are created by
// the agent's cron tool and run by `nanobot gateway`.
func (a *app) cronCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(a.stderr, cronUsage)
		if len(args) == 0 {
			return errUsage
		}
		return flag.ErrHelp
	}
	switch args[0] {
	case "list":
		return a.cronList(args[1:])
	case "remove", "rm":
		return a.cronRemove(args[1:])
	case "run":
		return a.cronRun(ctx, args[1:])
	}
	fmt.Fprintf(a.stderr, "nanobot cron: unknown subcommand %q\n\n%s", args[0], cronUsage)
	return errUsage
}

// parseWithID parses flags around exactly one positional job id.
func parseWithID(fs *flag.FlagSet, args []string) (string, error) {
	// Accept the id before or after the flags.
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", err
		}
		return "", errUsage
	}
	rest := fs.Args()
	if id == "" && len(rest) > 0 {
		id, rest = rest[0], rest[1:]
	}
	if id == "" || len(rest) > 0 {
		fmt.Fprintln(fs.Output(), "want exactly one job id")
		fs.Usage()
		return "", errUsage
	}
	return id, nil
}

// cronService opens the job store of the configured workspace without
// building a provider, so it needs no API key.
func (a *app) cronService(configFlag, workspaceFlag string) (*cron.Service, error) {
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
	ws, err := cfg.WorkspacePath()
	if err != nil {
		return nil, err
	}
	return cron.NewService(cron.ServiceConfig{Store: cron.NewStore(ws)})
}

func (a *app) cronList(args []string) error {
	fs := a.newFlagSet("cron list", "cron list [--all] [--config path] [--workspace dir]")
	all := fs.Bool("all", false, "include disabled jobs")
	configFlag := fs.String("config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	workspace := fs.String("workspace", "", "workspace directory (overrides the config)")
	if err := parse(fs, args); err != nil {
		return err
	}
	svc, err := a.cronService(*configFlag, *workspace)
	if err != nil {
		return err
	}
	jobs, err := svc.List(*all)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Fprintln(a.stdout, "No scheduled jobs.")
		return nil
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSCHEDULE\tNEXT RUN\tLAST RUN\tCHAT")
	for _, j := range jobs {
		next := "-"
		if !j.State.NextRun.IsZero() {
			next = j.State.NextRun.Local().Format(time.DateTime)
		}
		if !j.Enabled {
			next = "disabled"
		}
		last := "-"
		if !j.State.LastRun.IsZero() {
			last = j.State.LastRun.Local().Format(time.DateTime) + " " + string(j.State.LastStatus)
		}
		chat := "-"
		switch {
		case j.System():
			chat = "(system)"
		case j.Binding != nil:
			chat = j.Binding.SessionKey
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, oneLine(j.Name), j.Schedule, next, last, chat)
	}
	return w.Flush()
}

func (a *app) cronRemove(args []string) error {
	fs := a.newFlagSet("cron remove", "cron remove <id> [--config path] [--workspace dir]")
	configFlag := fs.String("config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	workspace := fs.String("workspace", "", "workspace directory (overrides the config)")
	id, err := parseWithID(fs, args)
	if err != nil {
		return err
	}
	svc, err := a.cronService(*configFlag, *workspace)
	if err != nil {
		return err
	}
	res, err := svc.Remove(id)
	if err != nil {
		return err
	}
	switch res {
	case cron.Removed:
		fmt.Fprintf(a.stdout, "Removed job %s.\n", id)
		return nil
	case cron.Protected:
		return fmt.Errorf("job %s is a protected system job; change its config instead", id)
	}
	return fmt.Errorf("job %s not found", id)
}

// cronRun runs one job now. Its reply is printed instead of delivered.
func (a *app) cronRun(ctx context.Context, args []string) (err error) {
	fs := a.newFlagSet("cron run", "cron run <id> [--force] [--config path] [--workspace dir] [--verbose]")
	force := fs.Bool("force", false, "run the job even if it is disabled")
	flags := addCommonFlags(fs)
	id, err := parseWithID(fs, args)
	if err != nil {
		return err
	}
	rt, err := a.buildRuntime(flags, runtimeOptions{
		loadOptions: loadOptions{level: slog.LevelWarn},
		hooks:       &toolHints{w: a.stderr},
		cronDeliver: func(_ context.Context, msg bus.OutboundMessage) error {
			fmt.Fprintf(a.stdout, "[reply for %s:%s, not delivered]\n%s\n", msg.Channel, msg.ChatID, msg.Content)
			return nil
		},
	})
	if err != nil {
		return err
	}
	defer rt.closeInto(&err)
	if _, ok, err := rt.cron.Get(id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	if err := a.connectMCP(ctx, rt.mcp, flags.verbose); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-a.interrupts:
			cancel()
		case <-ctx.Done():
		}
	}()
	job, ran, err := rt.cron.RunJob(ctx, id, *force)
	switch {
	case !ran && err == nil:
		return fmt.Errorf("job %s is disabled; use --force to run it anyway", id)
	case errors.Is(err, cron.ErrSkipped):
		fmt.Fprintf(a.stdout, "Job %s skipped: %v\n", id, err)
		return nil
	case err != nil:
		return err
	}
	fmt.Fprintf(a.stdout, "Job %s ran (%s).\n", id, job.State.LastStatus)
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
