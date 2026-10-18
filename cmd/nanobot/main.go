// Command nanobot is the nanobot-go CLI: onboarding, an interactive or
// one-shot agent, a Dream pass over archived history, and status
// reporting.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

const usage = `nanobot-go: a small, always-on personal AI agent.

Usage:
  nanobot <command> [flags]

Commands:
  onboard   write a default config file and workspace
  agent     chat with the agent (interactive, or one-shot with -m)
  dream     fold archived history into long-term memory (run on a schedule)
  status    show the configuration (API keys are never printed)
  version   print the version

Run 'nanobot <command> -h' for the command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	a := &app{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		getenv:     os.Getenv,
		interrupts: interrupts,
	}
	code := a.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// app holds the CLI's dependencies so tests can replace them.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	// interrupts delivers Ctrl-C: it cancels the running turn, or exits
	// when idle.
	interrupts <-chan os.Signal
	// newProvider builds the model provider; nil uses the config.
	newProvider func(*config.Config) (providers.Provider, error)
}

// run dispatches a subcommand and returns the exit code.
func (a *app) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "onboard":
		err = a.onboard(args[1:])
	case "agent":
		err = a.agent(ctx, args[1:])
	case "dream":
		err = a.dream(ctx, args[1:])
	case "status":
		err = a.status(args[1:])
	case "version", "-v", "--version":
		fmt.Fprintf(a.stdout, "nanobot-go %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout, usage)
	default:
		fmt.Fprintf(a.stderr, "nanobot: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintf(a.stderr, "nanobot: %v\n", err)
		return 1
	}
}

// errUsage marks flag errors, which the flag package already reported.
var errUsage = errors.New("usage error")

// newFlagSet returns a flag set that reports errors to stderr.
func (a *app) newFlagSet(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "Usage: nanobot %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses args, mapping flag errors to errUsage.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected arguments: %q\n", fs.Args())
		fs.Usage()
		return errUsage
	}
	return nil
}
