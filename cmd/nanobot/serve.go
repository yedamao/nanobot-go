package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yedamao/nanobot-go/internal/api"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security"
)

// serveCronNote follows every job the cron tool adds from `nanobot
// serve`: the job is stored, but only the gateway runs jobs, and an API
// session has no chat to deliver to.
const serveCronNote = "Note: scheduled jobs only run while `nanobot gateway` is running. " +
	"A job created through the API runs in its API session; its reply is saved to the session history but is not delivered anywhere."

// serve runs the OpenAI-compatible API on api.host:api.port until SIGINT
// or SIGTERM, like nanobot's `serve`. It runs no chat channels and no
// cron jobs. On the first signal it stops accepting requests, gives the
// ones in flight gateway.shutdownTimeout seconds to finish (then cancels
// their turns), and stops the MCP servers and the session store. A
// second signal exits at once with status 1.
func (a *app) serve(ctx context.Context, args []string) (err error) {
	fs := a.newFlagSet("serve", "serve [--config path] [--workspace dir] [--host addr] [--port n] [--timeout seconds] [--verbose]")
	flags := addCommonFlags(fs)
	host := fs.String("host", "", "bind address (overrides api.host)")
	port := fs.Int("port", -1, "port (overrides api.port; 0 picks a free port)")
	timeout := fs.Int("timeout", 0, "per-request turn timeout in seconds (overrides api.timeout)")
	if err := parse(fs, args); err != nil {
		return err
	}

	// The same three contexts as the gateway: runCtx ends on the first
	// signal, turnCtx when the requests in flight run out of time, and
	// forceCtx on the second signal.
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	forceCtx, force := context.WithCancel(context.Background())
	defer force()
	turnCtx, cancelTurns := context.WithCancel(forceCtx)
	defer cancelTurns()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go a.watchShutdown(ctx, stopRun, force, watchDone)

	rt, err := a.buildRuntime(flags, runtimeOptions{
		loadOptions: loadOptions{
			level: slog.LevelInfo,
			configure: func(c *config.Config) {
				if *host != "" {
					c.API.Host = *host
				}
				if *port != -1 {
					c.API.Port = *port
				}
				if *timeout != 0 {
					c.API.Timeout = *timeout
				}
			},
		},
		cronNote: serveCronNote,
	})
	if err != nil {
		return err
	}
	defer rt.closeInto(&err)
	srv, err := newAPIServer(rt)
	if err != nil {
		return err
	}

	if len(rt.mcp.Status()) > 0 {
		_ = rt.mcp.Connect(runCtx)
		fmt.Fprintln(a.stderr, mcpSummary(rt.mcp.Status()))
	}
	if runCtx.Err() != nil { // signaled while connecting
		return nil
	}
	addr, err := srv.Start(turnCtx)
	if err != nil {
		return err
	}
	a.printServeBanner(rt.cfg, addr)
	if a.apiReady != nil {
		a.apiReady(addr)
	}

	<-runCtx.Done()
	waitForTurns(func() { shutdownAPI(forceCtx, srv, rt.logger) }, rt.cfg.Gateway.ShutdownWait(), cancelTurns, rt.logger)
	// The deferred rt.close waits for the loop, then stops the MCP servers
	// and closes the session store.
	return nil
}

// newAPIServer builds the API server for rt's config. It fails for a
// host beyond loopback without api.apiKey.
func newAPIServer(rt *agentRuntime) (*api.Server, error) {
	c := rt.cfg.API
	return api.New(api.Config{
		Host:    c.Host,
		Port:    c.Port,
		APIKey:  c.APIKey,
		Model:   rt.cfg.Agents.Defaults.Model,
		Timeout: c.TurnTimeout(),
		Turn:    rt.loop.Process,
		Logger:  rt.logger,
	})
}

// shutdownAPI stops srv once its requests are done (or ctx ends).
func shutdownAPI(ctx context.Context, srv *api.Server, log *slog.Logger) {
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("stop api server", "error", err)
	}
}

func (a *app) printServeBanner(cfg *config.Config, addr string) {
	fmt.Fprintf(a.stdout, "nanobot-go OpenAI-compatible API %s\n", version)
	a.printAPIEndpoint(cfg, addr)
	fmt.Fprintf(a.stdout, "Model: %s\n", cfg.Agents.Defaults.Model)
	fmt.Fprintf(a.stdout, "Session: %s (or api:<session_id>)\n", api.DefaultSessionKey)
	fmt.Fprintf(a.stdout, "Timeout: %s\n", cfg.API.TurnTimeout())
	fmt.Fprintln(a.stdout, "Press Ctrl-C to stop.")
}

// printAPIEndpoint reports where the API listens and whether it needs a
// key; it never prints the key.
func (a *app) printAPIEndpoint(cfg *config.Config, addr string) {
	auth := "no API key (loopback only)"
	if cfg.API.APIKey != "" {
		auth = "bearer API key required"
	}
	fmt.Fprintf(a.stdout, "OpenAI API: http://%s/v1/chat/completions (%s)\n", addr, auth)
	if !security.IsLoopbackHost(cfg.API.Host) {
		fmt.Fprintf(a.stderr, "Warning: the API listens on %s, beyond this device (authentication required).\n", cfg.API.Host)
	}
}
