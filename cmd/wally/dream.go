package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/memory"
)

// dream runs one Dream pass: it folds unprocessed history.jsonl entries
// into the long-term memory files. The gateway also runs it on the
// agents.defaults.dream schedule.
func (a *app) dream(ctx context.Context, args []string) error {
	fs := a.newFlagSet("dream", "dream [--config path] [--workspace dir] [--verbose]")
	flags := addCommonFlags(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	s, err := a.load(flags, loadOptions{level: slog.LevelWarn})
	if err != nil {
		return err
	}
	job, err := newDream(s, &toolHints{w: a.stderr})
	if err != nil {
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
	res, err := job.Run(ctx)
	a.reportDream(res)
	return err
}

// newDream builds the Dream job for s; hooks observe its runs.
func newDream(s *setup, hooks agent.Hooks) (*memory.Dream, error) {
	d := s.cfg.Agents.Defaults
	maxHistory := d.Dream.MaxHistoryEntries
	if maxHistory == 0 {
		maxHistory = -1 // the config's 0 disables trimming
	}
	return memory.NewDream(memory.DreamConfig{
		Workspace: s.ws.Root,
		Store:     memory.NewStore(s.ws.Root),
		Run: agent.DreamRun(agent.NewRunner(s.provider), agent.NewContextBuilder(s.ws.Root), agent.RunSpec{
			MaxIterations: d.MaxToolIterations,
			MaxTokens:     d.MaxTokens,
			Temperature:   &d.Temperature,
			Hooks:         hooks,
		}),
		BatchEntries:      d.Dream.BatchEntries,
		MaxHistoryEntries: maxHistory,
		Logger:            s.logger,
	})
}

func (a *app) reportDream(res memory.DreamResult) {
	switch {
	case res.Pending == 0:
		fmt.Fprintf(a.stdout, "Dream: nothing to process (cursor %d).\n", res.Cursor)
	case res.Outcome.Completed:
		fmt.Fprintf(a.stdout, "Dream: processed %d of %d new history entries (cursor %d).\n", res.Processed, res.Pending, res.Cursor)
		if res.Outcome.Reply != "" {
			fmt.Fprintf(a.stdout, "\n%s\n\n", res.Outcome.Reply)
		}
	default:
		fmt.Fprintf(a.stdout, "Dream: run did not complete; %d new history entries remain (cursor %d).\n", res.Pending, res.Cursor)
	}
	if res.Commit != "" {
		fmt.Fprintf(a.stdout, "Committed memory changes: %s\n", res.Commit)
	}
	if res.Dropped > 0 {
		fmt.Fprintf(a.stdout, "Trimmed %d old entries from history.jsonl.\n", res.Dropped)
	}
}
