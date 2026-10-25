package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/gatewayruntime"
)

const (
	reasonForeground = "This gateway runs in the foreground: restart it in its terminal, or stop it and run 'nanobot gateway --background'."
	reasonUnmanaged  = "This gateway is not the one recorded for its config (see 'nanobot gateway status'): restart it from the command line."
)

// webGateway is the WebUI's gateway control (/ui/gateway). Only a
// background gateway recorded in the state file as this very process
// can be restarted from the page: restart then runs `nanobot gateway
// restart` (see gatewayRestart) as a process of its own, which stops
// this one, starts a new one with the same flags and keeps its lifetime.
// A foreground gateway would come back detached from its terminal, so
// it is refused.
type webGateway struct {
	status func() (gatewayruntime.Status, error)
	pid    int
	// argv is the restart command; spawn starts it detached.
	argv  func() []string
	spawn func(argv []string) error

	mu         sync.Mutex
	restarting bool
}

// newWebGateway returns the control of the gateway for configPath and
// workspace, run with the --host flag host (empty when not given) and
// listening on port (fixed: the restart keeps it, so the page's address
// stays valid).
func (a *app) newWebGateway(configPath, workspace, host string, verbose bool, port func() int) (*webGateway, error) {
	rt, err := a.gatewayRuntime(configPath, workspace)
	if err != nil {
		return nil, err
	}
	argv := func() []string {
		args := []string{"gateway", "restart", "--config", rt.Instance.ConfigPath}
		if rt.Instance.Workspace != "" {
			args = append(args, "--workspace", rt.Instance.Workspace)
		}
		if host != "" {
			args = append(args, "--host", host)
		}
		if p := port(); p > 0 {
			args = append(args, "--port", strconv.Itoa(p))
		}
		if verbose {
			args = append(args, "--verbose")
		}
		return args
	}
	spawn := func(args []string) error { return a.spawnDetached(rt.Paths().LogPath, args) }
	return &webGateway{status: rt.Status, pid: os.Getpid(), argv: argv, spawn: spawn}, nil
}

// Gateway implements websocket.GatewayControl.
func (g *webGateway) Gateway() (websocket.GatewayInfo, error) {
	st, err := g.status()
	if err != nil {
		return websocket.GatewayInfo{}, err
	}
	info := websocket.GatewayInfo{PID: g.pid, LaunchMode: gatewayruntime.LaunchUnknown, Lifetime: st.Lifetime}
	switch {
	case !st.Running || st.PID != g.pid:
		info.RestartUnavailableReason = reasonUnmanaged
	case st.LaunchMode != gatewayruntime.LaunchBackground:
		info.LaunchMode, info.StartedAt = st.LaunchMode, st.StartedAt
		info.RestartUnavailableReason = reasonForeground
	default:
		info.LaunchMode, info.StartedAt = st.LaunchMode, st.StartedAt
		info.RestartAvailable = true
	}
	g.mu.Lock()
	info.Restarting = g.restarting
	g.mu.Unlock()
	return info, nil
}

// Restart implements websocket.GatewayControl. A second request while
// the first one's restart is under way is accepted without another.
func (g *webGateway) Restart() (websocket.GatewayRestart, error) {
	info, err := g.Gateway()
	if err != nil {
		return websocket.GatewayRestart{}, err
	}
	if !info.RestartAvailable {
		return websocket.GatewayRestart{}, &websocket.RestartUnavailableError{Reason: info.RestartUnavailableReason}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.restarting {
		return websocket.GatewayRestart{OK: true, Message: "already restarting"}, nil
	}
	if err := g.spawn(g.argv()); err != nil {
		return websocket.GatewayRestart{}, err
	}
	g.restarting = true
	return websocket.GatewayRestart{OK: true, Message: "restarting"}, nil
}

// spawnDetached starts this binary with args in a session of its own,
// its output appended to logPath, and does not wait for it.
func (a *app) spawnDetached(logPath string, args []string) error {
	exe := os.Executable
	if a.executable != nil {
		exe = a.executable
	}
	bin, err := exe()
	if err != nil {
		return fmt.Errorf("find the nanobot binary: %w", err)
	}
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := a.command(bin, args...)
	cmd.Stdout, cmd.Stderr = log, log
	newSession(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the restart: %w", err)
	}
	go cmd.Wait()
	return nil
}
