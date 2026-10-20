package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/cron"
)

const (
	// gatewayStopTimeout bounds delivering the last replies, stopping the
	// channels and closing the health endpoint, after the turns are done.
	gatewayStopTimeout = 10 * time.Second
	// healthReadTimeout matches nanobot's health server read timeout.
	healthReadTimeout = 2 * time.Second
	// healthBody is the health endpoint's response.
	healthBody = `{"status":"ok"}` + "\n"
)

// gateway runs the agent behind the enabled chat channels until SIGINT or
// SIGTERM, with a health endpoint on gateway.host:gateway.port, and runs
// the workspace's cron jobs (and Dream, as the system job "dream") once
// the loop is serving. On the first signal it stops taking inbound
// messages and starting cron jobs, gives the turns in flight (a cron
// job's included) gateway.shutdownTimeout seconds to finish (then
// cancels them),
// delivers their replies, and stops the channels, the health endpoint,
// the MCP servers and the session store, in that order. A second signal
// exits at once with status 1.
func (a *app) gateway(ctx context.Context, args []string) (err error) {
	fs := a.newFlagSet("gateway", "gateway [--config path] [--workspace dir] [--host addr] [--port n] [--verbose]")
	flags := addCommonFlags(fs)
	host := fs.String("host", "", "health endpoint host (overrides gateway.host)")
	port := fs.Int("port", -1, "health endpoint port (overrides gateway.port; 0 picks a free port)")
	if err := parse(fs, args); err != nil {
		return err
	}

	// runCtx ends on the first signal: stop accepting work. turnCtx ends
	// when the in-flight turns run out of time. forceCtx ends on the
	// second signal and cuts every wait short. The watcher outlives every
	// other defer so a second signal can always force the exit.
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	forceCtx, force := context.WithCancel(context.Background())
	defer force()
	turnCtx, cancelTurns := context.WithCancel(forceCtx)
	defer cancelTurns()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go a.watchShutdown(ctx, stopRun, force, watchDone)

	b := bus.NewMessageBus(0)
	rt, err := a.buildRuntime(flags, runtimeOptions{
		loadOptions: loadOptions{
			level: slog.LevelInfo,
			configure: func(c *config.Config) {
				if *host != "" {
					c.Gateway.Host = *host
				}
				if *port != -1 {
					c.Gateway.Port = *port
				}
			},
		},
		bus:         b,
		cronDeliver: b.PublishOutbound,
	})
	if err != nil {
		return err
	}
	defer rt.closeInto(&err)
	log := rt.logger

	// This also fails early on an unreadable job store.
	dreamSched, dreamOn, err := rt.syncDreamJob()
	if err != nil {
		return err
	}

	reg, err := a.channelRegistry()
	if err != nil {
		return err
	}
	chans, err := reg.Build(rt.cfg.Channels, channels.Deps{Bus: b, Logger: log})
	if err != nil {
		return err
	}
	mgr, err := channels.NewManager(b, chans, channels.WithLogger(log))
	if err != nil {
		return err
	}

	// Listen before the slow parts so a taken port fails right away.
	g := rt.cfg.Gateway
	ln, err := net.Listen("tcp", g.Addr())
	if err != nil {
		return fmt.Errorf("health endpoint: %w", err)
	}
	srv := &http.Server{
		Handler:           healthHandler(),
		ReadHeaderTimeout: healthReadTimeout,
		ReadTimeout:       healthReadTimeout,
		WriteTimeout:      healthReadTimeout,
		MaxHeaderBytes:    4 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	healthErr := make(chan error, 1)
	go func() { healthErr <- srv.Serve(ln) }()
	if !isLoopbackHost(g.Host) {
		fmt.Fprintf(a.stderr, "Warning: the unauthenticated health endpoint listens on %s, beyond loopback. Keep port %d private.\n", g.Host, g.Port)
	}

	if len(rt.mcp.Status()) > 0 {
		_ = rt.mcp.Connect(runCtx)
		fmt.Fprintln(a.stderr, mcpSummary(rt.mcp.Status()))
	}
	var runErr error
	served := make(chan error, 1)
	cronDone := make(chan struct{})
	if runCtx.Err() == nil { // no signal while connecting
		if err := mgr.Start(runCtx); err != nil {
			runErr = err
			stopRun()
		}
		go func() { served <- rt.loop.Serve(runCtx, turnCtx) }()
		// Jobs run under turnCtx like inbound turns, so a shutdown lets
		// the one in progress finish.
		go func() {
			defer close(cronDone)
			if err := rt.cron.Serve(runCtx, turnCtx); err != nil && runCtx.Err() == nil {
				log.Error("cron service stopped", "error", err)
			}
		}()
		a.printGatewayBanner(ln.Addr().String(), mgr)
		a.printCronBanner(rt.cron, dreamSched, dreamOn)
		if a.gatewayReady != nil {
			a.gatewayReady(ln.Addr().String())
		}
	} else {
		served <- runCtx.Err()
		close(cronDone)
	}
	select {
	case <-runCtx.Done():
	case err := <-healthErr:
		runErr = fmt.Errorf("health endpoint: %w", err)
		stopRun()
	}

	<-served
	// The cron service starts no job after runCtx ends; once it is done
	// no new turn can start, and the loop's turns (the cron job's too)
	// get the shutdown timeout.
	waitForTurns(func() {
		<-cronDone
		rt.loop.Wait()
	}, g.ShutdownWait(), cancelTurns, log)
	stopCtx, cancelStop := context.WithTimeout(forceCtx, gatewayStopTimeout)
	defer cancelStop()
	if err := mgr.Stop(stopCtx); err != nil {
		log.Warn("stop channels", "error", err)
	}
	if err := srv.Shutdown(stopCtx); err != nil {
		srv.Close()
	}
	cancelTurns()
	// The deferred rt.close stops the MCP servers and closes the session
	// store.
	return runErr
}

// watchShutdown stops the gateway on the first of ctx ending (SIGTERM)
// or Ctrl-C, and forces the exit on the next Ctrl-C or SIGTERM. It
// returns when done is closed.
func (a *app) watchShutdown(ctx context.Context, stop, force context.CancelFunc, done <-chan struct{}) {
	select {
	case <-ctx.Done():
	case <-a.interrupts:
	case <-done:
		return
	}
	fmt.Fprintln(a.stderr, "\nShutting down... press Ctrl-C again to force.")
	stop()
	select {
	case <-a.interrupts:
	case <-a.terminations:
	case <-done:
		return
	}
	fmt.Fprintln(a.stderr, "Forcing exit.")
	force()
	exit := a.exit
	if exit == nil {
		exit = os.Exit
	}
	exit(1)
}

// waitForTurns runs wait (which returns once the in-flight turns are
// done), canceling the turns once timeout has passed.
func waitForTurns(wait func(), timeout time.Duration, cancel context.CancelFunc, log *slog.Logger) {
	idle := make(chan struct{})
	go func() {
		wait()
		close(idle)
	}()
	select {
	case <-idle:
		return
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return
	case <-timer.C:
		log.Warn("in-flight turns did not finish in time; canceling them", "timeout", timeout)
		cancel()
	}
	<-idle
}

// channelRegistry returns the channels the gateway can run: the built-in
// ones (none yet) and a.channelFactories.
func (a *app) channelRegistry() (*channels.Registry, error) {
	r := channels.NewRegistry()
	for _, name := range slices.Sorted(maps.Keys(a.channelFactories)) {
		if err := r.Register(name, a.channelFactories[name]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (a *app) printGatewayBanner(addr string, mgr *channels.Manager) {
	fmt.Fprintf(a.stdout, "nanobot-go gateway %s\n", version)
	fmt.Fprintf(a.stdout, "Health endpoint: http://%s/healthz\n", addr)
	if running := mgr.Running(); len(running) > 0 {
		fmt.Fprintf(a.stdout, "Channels: %s\n", strings.Join(running, ", "))
	} else {
		fmt.Fprintln(a.stdout, "Channels: none running")
	}
	fmt.Fprintln(a.stdout, "Press Ctrl-C to stop.")
}

// printCronBanner reports the scheduled jobs and Dream's schedule.
func (a *app) printCronBanner(svc *cron.Service, dream cron.Schedule, dreamOn bool) {
	if jobs, err := svc.List(false); err == nil {
		n := 0
		for _, j := range jobs {
			if !j.System() {
				n++
			}
		}
		fmt.Fprintf(a.stdout, "Cron: %d scheduled jobs (%s)\n", n, svc.Store().Path())
	}
	if dreamOn {
		fmt.Fprintf(a.stdout, "Dream: %s\n", dream)
	} else {
		fmt.Fprintln(a.stdout, "Dream: not scheduled")
	}
}

// healthHandler serves GET /healthz (and nanobot's /health) with
// {"status":"ok"}.
func healthHandler() http.Handler {
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, healthBody)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /health", ok)
	return mux
}

// isLoopbackHost reports whether host only accepts local connections.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
