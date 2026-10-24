// Command nanobot is the nanobot-go CLI: onboarding, an interactive or
// one-shot agent, the long-running gateway (which also runs scheduled
// jobs and the Telegram and WeChat channels), the OpenAI-compatible API,
// managing scheduled jobs and paired chat senders, the WeChat QR login, a
// Dream pass over archived history, and status reporting.
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

	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
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
  gateway   run the agent behind the configured chat channels until
            SIGINT/SIGTERM, with a health endpoint (and the API when
            api.enabled is set)
  serve     run the OpenAI-compatible API (/v1/chat/completions) until
            SIGINT/SIGTERM
  cron      list, remove or run the scheduled jobs (the gateway runs them)
  pairing   list, approve, deny or revoke chat senders paired by code
  channels  log in to a chat channel ('channels login weixin' scans a
            WeChat QR code)
  dream     fold archived history into long-term memory (the gateway
            also runs it on a schedule)
  status    show the configuration (API keys are never printed);
            --mcp also connects to the MCP servers
  version   print the version

Run 'nanobot <command> -h' for the command's flags.
`

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	interrupts := make(chan os.Signal, 1)
	terminations := make(chan os.Signal, 1)
	go routeSignals(sigs, cancel, interrupts, terminations)
	a := &app{
		stdin:        os.Stdin,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		getenv:       os.Getenv,
		interrupts:   interrupts,
		terminations: terminations,
	}
	code := a.run(ctx, os.Args[1:])
	signal.Stop(sigs)
	cancel()
	os.Exit(code)
}

// routeSignals forwards Ctrl-C to interrupts. The first SIGTERM cancels
// the command's context; later ones go to terminations. Sends never
// block: a signal nobody is waiting for is dropped, as with
// signal.Notify. It returns when sigs is closed.
func routeSignals(sigs <-chan os.Signal, cancel context.CancelFunc, interrupts, terminations chan<- os.Signal) {
	terminated := false
	for s := range sigs {
		out := interrupts
		if s == syscall.SIGTERM {
			if !terminated {
				terminated = true
				cancel()
				continue
			}
			out = terminations
		}
		select {
		case out <- s:
		default:
		}
	}
}

// app holds the CLI's dependencies so tests can replace them.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	// interrupts delivers Ctrl-C: it cancels the running turn, or exits
	// when idle.
	interrupts <-chan os.Signal
	// terminations delivers SIGTERMs after the first, which cancels the
	// command's context.
	terminations <-chan os.Signal
	// exit ends the process at once (a forced gateway shutdown); nil
	// uses os.Exit.
	exit func(code int)
	// newProvider builds the model provider; nil uses the config.
	newProvider func(*config.Config) (providers.Provider, error)
	// mcpTransport connects to MCP servers; nil uses mcp's default
	// transports with the configured SSRF policy.
	mcpTransport mcp.TransportFactory
	// channelFactories are registered on top of the built-in channels
	// (for tests).
	channelFactories map[string]channels.Factory
	// gatewayReady, when set, is called with the health endpoint's
	// address once the gateway is serving (for tests).
	gatewayReady func(healthAddr string)
	// apiReady, when set, is called with the API's address once `serve`
	// or the gateway serves it (for tests).
	apiReady func(apiAddr string)
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
	case "channels":
		err = a.channelsCmd(ctx, args[1:])
	case "cron":
		err = a.cronCmd(ctx, args[1:])
	case "dream":
		err = a.dream(ctx, args[1:])
	case "gateway":
		err = a.gateway(ctx, args[1:])
	case "pairing":
		err = a.pairingCmd(args[1:])
	case "serve":
		err = a.serve(ctx, args[1:])
	case "status":
		err = a.status(ctx, args[1:])
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
