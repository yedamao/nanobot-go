package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/api"
	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/channels/telegram"
	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/channels/weixin"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/cron"
	"github.com/yedamao/nanobot-go/internal/pairing"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/skills"
	"github.com/yedamao/nanobot-go/internal/transcribe"
)

const (
	// gatewayStopTimeout bounds delivering the last replies, stopping the
	// channels and closing the health endpoint, after the turns are done.
	gatewayStopTimeout = 10 * time.Second
	// healthReadTimeout matches nanobot's health server read timeout.
	healthReadTimeout = 2 * time.Second
	// webIdleTimeout closes idle keep-alive connections when the WebUI
	// is mounted (which drops the read and write timeouts; see
	// newGatewayServer).
	webIdleTimeout = 2 * time.Minute
	// webMaxHeaderBytes leaves room for the cookies a browser sends to
	// localhost.
	webMaxHeaderBytes = 64 << 10
	// healthBody is the health endpoint's response.
	healthBody = `{"status":"ok"}` + "\n"
)

// gateway runs the agent behind the enabled chat channels until SIGINT or
// SIGTERM, with a health endpoint on gateway.host:gateway.port, and runs
// the workspace's cron jobs (and Dream and the heartbeat, as the system
// jobs "dream" and "heartbeat") once the loop is serving. With api.enabled it also serves the
// OpenAI-compatible API on api.host:api.port. On the first signal it
// stops taking inbound messages, API requests and starting cron jobs,
// gives the turns in flight (a cron job's and API requests' included)
// gateway.shutdownTimeout seconds to finish (then cancels them),
// delivers their replies, and stops the channels, the health endpoint,
// the MCP servers and the session store, in that order. A second signal
// exits at once with status 1.
//
// With --background it instead starts that gateway detached from the
// terminal (see gatewayBackground); the subcommands status, stop,
// restart and logs manage it (see gatewayUsage). A foreground gateway
// records itself in the same state file, so a second gateway for the
// config is refused and `gateway status` and `nanobot webui` see it.
func (a *app) gateway(ctx context.Context, args []string) (err error) {
	if ok, err := a.gatewaySubcommand(ctx, args); ok {
		return err
	}
	fs := a.newFlagSet("gateway", "")
	flags := addCommonFlags(fs)
	host := fs.String("host", "", "health endpoint host (overrides gateway.host)")
	port := fs.Int("port", -1, "health endpoint port (overrides gateway.port; 0 picks a free port)")
	background := fs.Bool("background", false, "start the gateway detached from the terminal (stop it with 'nanobot gateway stop')")
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "%s\nFlags:\n", gatewayUsage)
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if *background {
		return a.gatewayBackground(ctx, flags, *host, *port)
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
	heartbeatSched, heartbeatOn, err := rt.syncHeartbeatJob()
	if err != nil {
		return err
	}

	// The WebUI's status reads mgr, which is set before the HTTP server
	// starts serving.
	var mgr *channels.Manager
	running := func() []string {
		if mgr == nil {
			return nil
		}
		return mgr.Running()
	}
	status := webStatus(rt.cfg, time.Now(), running)
	settings := newWebSettings(rt.configPath, rt.cfg, a.getenv)
	rt.skills.SetPersist(settings.saveDisabledSkills)
	mcpStore := newWebMCP(settings, rt.cfg.Tools.MCPServers, rt.mcp.Status, a.getenv, a.probeMCP)
	oauthFlows := mcpStore.enableOAuth(mcpOAuthStore(rt.configPath, log), rt.mcp.Reconnect, func(ctx context.Context, u *url.URL) error {
		_, err := rt.ssrf.CheckTarget(ctx, u)
		return err
	})
	defer oauthFlows.Close()
	providersStore := newWebProviders(settings, rt.cfg.Providers, a.getenv)
	modelCatalog := newWebModels(settings, a.getenv)
	channelConfig := newWebChannels(settings, rt.cfg.Channels, running, a.getenv, weixin.DefaultStateDir(rt.configPath))
	// The restart keeps the port the health endpoint listens on, set
	// before it starts serving.
	listenPort := 0
	gatewayCtl, err := a.newWebGateway(rt.configPath, flags.workspace, *host, flags.verbose, func() int { return listenPort })
	if err != nil {
		return err
	}
	reg, err := a.channelRegistry(rt.configPath, rt.cfg.Gateway.Host, rt.ws, rt.ssrf, voiceTranscriber(rt.cfg), rt.sessions, status, settings, rt.cron, rt.skills, mcpStore, mcpStore, mcpStore, providersStore, modelCatalog, modelCatalog, gatewayCtl, channelConfig)
	if err != nil {
		return err
	}
	chans, err := reg.Build(rt.cfg.Channels, channels.Deps{Bus: b, Logger: log})
	if err != nil {
		return err
	}
	mgr, err = channels.NewManager(b, chans, channels.WithLogger(log))
	if err != nil {
		return err
	}
	// The heartbeat reports to chats of the channels that started.
	rt.channels = mgr.Running
	// Built now so an unauthenticated non-loopback API fails right away.
	var apiSrv *api.Server
	if rt.cfg.API.Enabled {
		if apiSrv, err = newAPIServer(rt); err != nil {
			return err
		}
	}

	// Listen before the slow parts so a taken port fails right away.
	g := rt.cfg.Gateway
	ln, err := net.Listen("tcp", g.Addr())
	if err != nil {
		return fmt.Errorf("health endpoint: %w", err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		listenPort = tcp.Port
	}
	release, err := a.claimGateway(rt.configPath, flags.workspace, g.Host, ln.Addr())
	if err != nil {
		ln.Close()
		return err
	}
	defer release()
	srv, webUI := newGatewayServer(chans, log)
	healthErr := make(chan error, 1)
	go func() { healthErr <- srv.Serve(ln) }()
	if !security.IsLoopbackHost(g.Host) {
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
		apiAddr := ""
		if apiSrv != nil && runErr == nil {
			// API turns run under turnCtx like inbound turns.
			if apiAddr, err = apiSrv.Start(turnCtx); err != nil {
				runErr = err
				stopRun()
			}
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
		a.printGatewayBanner(ln.Addr().String(), mgr, webUI)
		a.printCronBanner(rt.cron, dreamSched, dreamOn)
		if heartbeatOn {
			fmt.Fprintf(a.stdout, "Heartbeat: %s\n", heartbeatSched)
		} else {
			fmt.Fprintln(a.stdout, "Heartbeat: disabled")
		}
		if apiAddr != "" {
			a.printAPIEndpoint(rt.cfg, apiAddr)
			if a.apiReady != nil {
				a.apiReady(apiAddr)
			}
		}
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
	// The API stops accepting requests and waits for those in flight.
	apiDone := make(chan struct{})
	go func() {
		defer close(apiDone)
		if apiSrv != nil {
			shutdownAPI(forceCtx, apiSrv, log)
		}
	}()
	// The cron service starts no job after runCtx ends; once it and the
	// API are done no new turn can start, and the loop's turns (the cron
	// job's and the API requests' too) get the shutdown timeout.
	waitForTurns(func() {
		<-cronDone
		<-apiDone
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

// newGatewayServer returns the gateway's HTTP server: the health endpoint
// plus whatever the channels mount (channels.Mounter; the websocket
// channel mounts /ws and the WebUI). webUI reports whether anything was
// mounted. Health alone keeps nanobot's short read and write timeouts. A
// WebSocket stays open for whole agent turns, and the server's
// ReadTimeout and WriteTimeout would still apply to it after the
// upgrade, so with mounted handlers only the header read is bounded
// (ReadHeaderTimeout), idle keep-alive connections are closed after
// webIdleTimeout, and the websocket channel bounds its own writes and
// pings its connections.
func newGatewayServer(chans []channels.Channel, log *slog.Logger) (srv *http.Server, webUI bool) {
	mux := healthHandler()
	for _, ch := range chans {
		if m, ok := ch.(channels.Mounter); ok {
			m.Mount(mux)
			webUI = true
		}
	}
	srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: healthReadTimeout,
		MaxHeaderBytes:    4 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	if webUI {
		srv.IdleTimeout = webIdleTimeout
		srv.MaxHeaderBytes = webMaxHeaderBytes
	} else {
		srv.ReadTimeout = healthReadTimeout
		srv.WriteTimeout = healthReadTimeout
	}
	return srv, webUI
}

// channelRegistry returns the channels the gateway can run: the built-in
// ones (Telegram, WeChat, websocket) and a.channelFactories, which
// replace a built-in of the same name. Pairing approvals live next to the
// config file, and so does the WeChat login (state/weixin). host is the
// gateway's listen address, which the websocket channel requires to be
// loopback unless it has a token or tokenIssueSecret; ws is the agent's
// workspace, whose files the WebUI previews (GET /ui/media/); ssrf is
// the SSRF policy Telegram and WeChat check remote media URLs with (nil
// is the default policy); transcriber turns Telegram voice notes into
// text (nil leaves them as paths); sessions is the agent loop's session manager, which it loads
// chats from (fresh copies, not the cached ones) and renames and deletes
// them through, keeping the agent's cache in step; status is the source
// of its GET /ui/status (see webStatus), settings is the store of its
// /ui/settings (see webSettings), cronSvc backs /ui/cron, skillsMgr backs
// /ui/skills, mcpStore backs /ui/mcp (see webMCP; nil leaves it
// unmounted), mcpOAuth backs its OAuth sign-in and /auth/mcp/callback, providersStore backs /ui/providers (see webProviders; nil
// leaves it unmounted), modelCatalog backs /ui/providers/{name}/models
// (see webModels; nil leaves it unmounted), providerTester backs
// /ui/providers/{name}/test (see webModels.TestProvider; nil leaves it
// unmounted), gatewayCtl backs /ui/gateway (see webGateway; nil
// leaves it unmounted), channelConfig backs /ui/channels/config (see
// webChannels; nil leaves it unmounted), and the pairing store backs /ui/pairing (shared with
// Telegram/WeChat).
func (a *app) channelRegistry(configPath, host string, ws *security.Workspace, ssrf *security.Policy, transcriber telegram.Transcriber, sessions *session.Manager, status func() websocket.Status, settings websocket.SettingsStore, cronSvc *cron.Service, skillsMgr *skills.Manager, mcpStore websocket.MCPStore, mcpTester websocket.MCPTester, mcpOAuth websocket.MCPOAuth, providersStore websocket.ProvidersStore, modelCatalog websocket.ModelCatalog, providerTester websocket.ProviderTester, gatewayCtl websocket.GatewayControl, channelConfig websocket.ChannelConfigStore) (*channels.Registry, error) {
	r := channels.NewRegistry()
	store := pairing.NewStore(pairing.PathFor(configPath))
	builtins := map[string]channels.Factory{
		telegram.Name: telegram.Factory(telegram.Options{
			Getenv:      a.getenv,
			Pairing:     store,
			Workspace:   ws.Root,
			SSRF:        ssrf,
			Transcriber: transcriber,
		}),
		weixin.Name: weixin.Factory(weixin.Options{
			Getenv:          a.getenv,
			Pairing:         store,
			DefaultStateDir: weixin.DefaultStateDir(configPath),
			Version:         version,
			Workspace:       ws.Root,
			SSRF:            ssrf,
		}),
		websocket.Name: websocket.Factory(websocket.Options{
			Host: host, Sessions: sessions, Status: status, Settings: settings,
			Workspace: ws, Cron: cronSvc, Pairing: store, Skills: skillsMgr,
			MCP: mcpStore, MCPTester: mcpTester, MCPOAuth: mcpOAuth, Providers: providersStore, Models: modelCatalog, ProviderTester: providerTester,
			Gateway: gatewayCtl, ChannelConfig: channelConfig,
		}),
	}
	for name, f := range builtins {
		if _, override := a.channelFactories[name]; override {
			continue
		}
		if err := r.Register(name, f); err != nil {
			return nil, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(a.channelFactories)) {
		if err := r.Register(name, a.channelFactories[name]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (a *app) printGatewayBanner(addr string, mgr *channels.Manager, webUI bool) {
	fmt.Fprintf(a.stdout, "nanobot-go gateway %s\n", version)
	fmt.Fprintf(a.stdout, "Health endpoint: http://%s/healthz\n", addr)
	if webUI && slices.Contains(mgr.Running(), websocket.Name) {
		fmt.Fprintf(a.stdout, "WebUI: http://%s/\n", addr)
	}
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
// {"status":"ok"}. Channels may mount more routes on the returned mux.
func healthHandler() *http.ServeMux {
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

// voiceTranscriber returns the Whisper client for inbound voice notes:
// OpenAI's (providers.openai), when it has an API key, else nil.
func voiceTranscriber(cfg *config.Config) telegram.Transcriber {
	pc := cfg.Providers.OpenAI
	if pc.APIKey == "" {
		return nil
	}
	return &transcribe.OpenAI{APIKey: pc.APIKey, APIBase: pc.APIBase}
}
