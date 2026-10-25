package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/gatewayruntime"
)

const gatewayUsage = `Usage:
  nanobot gateway [--background] [--config path] [--workspace dir] [--host addr] [--port n] [--verbose]
  nanobot gateway status [--config path] [--workspace dir]
  nanobot gateway stop [--config path] [--workspace dir] [--timeout d]
  nanobot gateway restart [--config path] [--workspace dir] [--host addr] [--port n] [--verbose] [--timeout d]
  nanobot gateway logs [--config path] [--workspace dir] [--tail n]

Without a subcommand the gateway runs in the foreground until Ctrl-C.
--background starts it detached from the terminal instead (output goes to
the log file) and returns once it is healthy; it keeps running until
'nanobot gateway stop'. status, stop, restart and logs manage that
gateway; state lives in run/ and logs/ next to the config file.
`

// gatewayReadyTimeout bounds waiting for a background gateway to answer
// /healthz, like nanobot's 20 seconds.
const gatewayReadyTimeout = 20 * time.Second

// gatewaySubcommand runs `gateway status|stop|restart|logs`; ok is false
// when args do not name one.
func (a *app) gatewaySubcommand(ctx context.Context, args []string) (ok bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "status":
		return true, a.gatewayStatus(args[1:])
	case "stop":
		return true, a.gatewayStop(args[1:])
	case "restart":
		return true, a.gatewayRestart(ctx, args[1:])
	case "logs":
		return true, a.gatewayLogs(args[1:])
	case "help":
		fmt.Fprint(a.stdout, gatewayUsage)
		return true, nil
	}
	return false, nil
}

// gatewayRuntime returns the lifecycle manager of the gateway for the
// config file at path and the workspace override.
func (a *app) gatewayRuntime(path, workspace string) (*gatewayruntime.Runtime, error) {
	inst, err := gatewayruntime.Resolve(path, workspace)
	if err != nil {
		return nil, err
	}
	rt := gatewayruntime.New(inst)
	rt.Executable = a.executable
	rt.Command = a.newCommand
	return rt, nil
}

// claimGateway records this process in the gateway's state file (see
// gatewayruntime.Runtime.Claim) once its health endpoint listens on
// addr; release clears it.
func (a *app) claimGateway(path, workspace, host string, addr net.Addr) (release func(), err error) {
	rt, err := a.gatewayRuntime(path, workspace)
	if err != nil {
		return nil, err
	}
	port := 0
	if tcp, ok := addr.(*net.TCPAddr); ok {
		port = tcp.Port
	}
	release, err = rt.Claim(gatewayruntime.StartOptions{Host: host, Port: port})
	var running *gatewayruntime.AlreadyRunningError
	if errors.As(err, &running) {
		return nil, fmt.Errorf("%w; see 'nanobot gateway status', or stop it with 'nanobot gateway stop'", err)
	}
	return release, err
}

// gatewayFlags is a management subcommand's flag set, with the --config
// and --workspace flags naming the gateway.
type gatewayFlags struct {
	fs                *flag.FlagSet
	config, workspace string
}

func (a *app) newGatewayFlags(name, synopsis string) *gatewayFlags {
	s := &gatewayFlags{fs: a.newFlagSet(name, synopsis)}
	s.fs.StringVar(&s.config, "config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	s.fs.StringVar(&s.workspace, "workspace", "", "workspace directory (as passed to the gateway)")
	return s
}

// runtime resolves the config path and returns its gateway runtime.
func (s *gatewayFlags) runtime(a *app) (*gatewayruntime.Runtime, string, error) {
	path, err := config.ResolvePath(s.config, a.getenv)
	if err != nil {
		return nil, "", err
	}
	rt, err := a.gatewayRuntime(path, s.workspace)
	return rt, path, err
}

func (a *app) gatewayStatus(args []string) error {
	s := a.newGatewayFlags("gateway status", "gateway status [--config path] [--workspace dir]")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	rt, _, err := s.runtime(a)
	if err != nil {
		return err
	}
	st, err := rt.Status()
	if err != nil {
		return err
	}
	a.printGatewayStatus(st)
	return nil
}

func (a *app) gatewayStop(args []string) error {
	s := a.newGatewayFlags("gateway stop", "gateway stop [--config path] [--workspace dir] [--timeout d]")
	timeout := s.fs.Duration("timeout", 0, "how long to wait for a graceful exit before killing it (default: gateway.shutdownTimeout + 15s)")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	rt, path, err := s.runtime(a)
	if err != nil {
		return err
	}
	res, err := rt.Stop(stopWait(path, *timeout))
	if err != nil {
		return err
	}
	switch {
	case res.OK:
		fmt.Fprintln(a.stdout, "Gateway stopped.")
	case res.Message == gatewayruntime.MsgNotRunning || res.Message == gatewayruntime.MsgStateStale:
		fmt.Fprintln(a.stdout, "Gateway is not running.")
	}
	a.printGatewayStatus(res.Status)
	if !res.OK && res.Message != gatewayruntime.MsgNotRunning && res.Message != gatewayruntime.MsgStateStale {
		return fmt.Errorf("gateway was not stopped: %s", res.Message)
	}
	return nil
}

// stopWait is timeout, or else the time a gateway may take to shut down
// gracefully: gateway.shutdownTimeout for the turns in flight plus
// stopping the channels.
func stopWait(path string, timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	wait := time.Duration(config.DefaultGatewayShutdownTimeout) * time.Second
	if cfg, err := config.Load(path); err == nil {
		wait = cfg.Gateway.ShutdownWait()
	}
	return wait + gatewayStopTimeout + 5*time.Second
}

// gatewayStartConfig loads the config with --host and --port applied,
// for a background start or restart.
func gatewayStartConfig(path, host string, port int, verbose bool) (*config.Config, gatewayruntime.StartOptions, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, gatewayruntime.StartOptions{}, err
	}
	if host != "" {
		cfg.Gateway.Host = host
	}
	if port != -1 {
		cfg.Gateway.Port = port
	}
	if err := cfg.Validate(); err != nil {
		return nil, gatewayruntime.StartOptions{}, err
	}
	if cfg.Gateway.Port == 0 {
		return nil, gatewayruntime.StartOptions{}, errors.New("a background gateway needs a fixed port: gateway.port is 0 (a random port); pass --port")
	}
	return cfg, gatewayruntime.StartOptions{Host: host, Port: cfg.Gateway.Port, Verbose: verbose}, nil
}

// gatewayBackground runs `gateway --background`.
func (a *app) gatewayBackground(ctx context.Context, flags *commonFlags, host string, port int) error {
	path, err := config.ResolvePath(flags.config, a.getenv)
	if err != nil {
		return err
	}
	cfg, opts, err := gatewayStartConfig(path, host, port, flags.verbose)
	if err != nil {
		return err
	}
	rt, err := a.gatewayRuntime(path, flags.workspace)
	if err != nil {
		return err
	}
	base := localURL(cfg.Gateway.Host, cfg.Gateway.Port, "")
	client := &http.Client{Timeout: time.Second}
	if st, err := rt.Status(); err != nil {
		return err
	} else if !st.Running && gatewayHealthy(ctx, client, base) {
		return fmt.Errorf("a gateway this command does not manage already answers on %s (started by an older nanobot-go, or for another config); stop it from the terminal where it runs first", base)
	}
	res, err := rt.StartBackground(opts)
	if err != nil {
		return err
	}
	switch {
	case res.OK:
	case res.Message == gatewayruntime.MsgAlreadyRunning && res.Status.LaunchMode == gatewayruntime.LaunchForeground:
		a.printGatewayStatus(res.Status)
		return errors.New("the gateway already runs in the foreground and cannot be detached in place: stop it in its terminal, then run 'nanobot gateway --background'")
	case res.Message == gatewayruntime.MsgAlreadyRunning:
		if res.Promoted {
			fmt.Fprintln(a.stdout, "Existing on-demand gateway promoted to persistent background mode.")
			fmt.Fprintln(a.stdout, "It keeps running after all local clients exit; use 'nanobot gateway stop' to stop it.")
		} else {
			fmt.Fprintln(a.stdout, "Gateway is already running in persistent background mode.")
		}
		a.printGatewayStatus(res.Status)
		return nil
	default:
		a.printGatewayLogTail(rt)
		a.printGatewayStatus(res.Status)
		return fmt.Errorf("gateway was not started: %s", res.Message)
	}
	if err := a.waitGatewayHealthy(ctx, rt, res.Status.PID, client, base); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "Gateway started in the background.")
	st, err := rt.Status()
	if err != nil {
		return err
	}
	a.printGatewayStatus(st)
	return nil
}

// waitGatewayHealthy waits for a just-started background gateway to
// answer /healthz, showing the end of its log when it fails.
func (a *app) waitGatewayHealthy(ctx context.Context, rt *gatewayruntime.Runtime, pid int, client *http.Client, base string) error {
	err := rt.WaitReady(ctx, pid, gatewayReadyTimeout, func() bool { return gatewayHealthy(ctx, client, base) })
	if err == nil {
		return nil
	}
	a.printGatewayLogTail(rt)
	if errors.Is(err, gatewayruntime.ErrExited) {
		return fmt.Errorf("the background gateway exited during startup; see %s", rt.Paths().LogPath)
	}
	return fmt.Errorf("%w (it may still be starting: 'nanobot gateway status'; logs: %s)", err, rt.Paths().LogPath)
}

func (a *app) gatewayRestart(ctx context.Context, args []string) error {
	s := a.newGatewayFlags("gateway restart", "gateway restart [--config path] [--workspace dir] [--host addr] [--port n] [--verbose] [--timeout d]")
	host := s.fs.String("host", "", "health endpoint host (overrides gateway.host)")
	port := s.fs.Int("port", -1, "health endpoint port (overrides gateway.port)")
	verbose := s.fs.Bool("verbose", false, "log debug output")
	timeout := s.fs.Duration("timeout", 0, "how long to wait for a graceful exit before killing it (default: gateway.shutdownTimeout + 15s)")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	rt, path, err := s.runtime(a)
	if err != nil {
		return err
	}
	cfg, opts, err := gatewayStartConfig(path, *host, *port, *verbose)
	if err != nil {
		return err
	}
	res, err := rt.Restart(opts, stopWait(path, *timeout))
	if err != nil {
		return err
	}
	switch {
	case res.OK:
	case res.Message == gatewayruntime.MsgNotRunning:
		a.printGatewayStatus(res.Status)
		return errors.New("gateway is not running; there is nothing to restart. Start a persistent gateway with 'nanobot gateway --background'")
	case res.Message == gatewayruntime.MsgForegroundRestartRequired:
		a.printGatewayStatus(res.Status)
		return errors.New("the gateway is attached to a foreground terminal and cannot be restarted as a background process: restart it in that terminal, or stop it and run 'nanobot gateway --background'")
	default:
		a.printGatewayLogTail(rt)
		a.printGatewayStatus(res.Status)
		return fmt.Errorf("gateway restart failed: %s", res.Message)
	}
	client := &http.Client{Timeout: time.Second}
	if err := a.waitGatewayHealthy(ctx, rt, res.Status.PID, client, localURL(cfg.Gateway.Host, cfg.Gateway.Port, "")); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "Gateway restarted in the background.")
	st, err := rt.Status()
	if err != nil {
		return err
	}
	a.printGatewayStatus(st)
	return nil
}

func (a *app) gatewayLogs(args []string) error {
	s := a.newGatewayFlags("gateway logs", "gateway logs [--config path] [--workspace dir] [--tail n]")
	tail := s.fs.Int("tail", 200, "number of recent lines to show")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	rt, _, err := s.runtime(a)
	if err != nil {
		return err
	}
	lines, err := rt.LogTail(*tail)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintf(a.stdout, "No gateway log output yet (%s).\n", rt.Paths().LogPath)
	}
	for _, l := range lines {
		fmt.Fprintln(a.stdout, l)
	}
	return nil
}

// printGatewayLogTail shows the last log lines on stderr.
func (a *app) printGatewayLogTail(rt *gatewayruntime.Runtime) {
	lines, _ := rt.LogTail(20)
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(a.stderr, "Last lines of %s:\n", rt.Paths().LogPath)
	for _, l := range lines {
		fmt.Fprintf(a.stderr, "  %s\n", l)
	}
}

func (a *app) printGatewayStatus(s gatewayruntime.Status) {
	w := a.stdout
	running := "no"
	if s.Running {
		running = "yes"
	}
	fmt.Fprintf(w, "Running:     %s\n", running)
	fmt.Fprintf(w, "Reason:      %s\n", s.Reason)
	if s.PID != 0 {
		fmt.Fprintf(w, "PID:         %d\n", s.PID)
	}
	if s.Port != 0 {
		fmt.Fprintf(w, "Port:        %d\n", s.Port)
	}
	if s.StartedAt != "" {
		fmt.Fprintf(w, "Started At:  %s\n", s.StartedAt)
	}
	if s.Running {
		fmt.Fprintf(w, "Launch Mode: %s\n", s.LaunchMode)
		fmt.Fprintf(w, "Lifetime:    %s\n", s.Lifetime)
		fmt.Fprintf(w, "Clients:     %d\n", s.Clients)
	}
	fmt.Fprintf(w, "State:       %s\n", s.StatePath)
	fmt.Fprintf(w, "Logs:        %s\n", s.LogPath)
}
