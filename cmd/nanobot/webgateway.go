package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/gatewayruntime"
	"github.com/yedamao/nanobot-go/internal/gatewayservice"
)

const (
	reasonForeground = "This gateway runs in the foreground: restart it in its terminal, or stop it and run 'nanobot gateway --background'."
	reasonUnmanaged  = "This gateway is not the one recorded for its config (see 'nanobot gateway status'): restart it from the command line."
)

// webGateway is the WebUI's gateway control (/ui/gateway). A gateway
// recorded as this process can be restarted when it is either a background
// gateway or the process owned by the installed OS service. Background
// gateways spawn `nanobot gateway restart`; service gateways spawn `nanobot
// gateway service restart`. Both are handed to a detached process so the
// WebUI's HTTP 202 response can be flushed before this process is stopped.
// Other foreground gateways are refused because they would come back detached
// from their terminal.
type webGateway struct {
	status        func() (gatewayruntime.Status, error)
	serviceStatus func() (gatewayservice.Status, error)
	pid           int
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
	in, err := a.serviceInstaller()
	if err != nil {
		return nil, err
	}
	serviceStatus := func() (gatewayservice.Status, error) { return in.Status("", gatewayservice.ManagerAuto, "") }
	return &webGateway{status: rt.Status, serviceStatus: serviceStatus, pid: os.Getpid(), argv: argv, spawn: spawn}, nil
}

// Gateway implements websocket.GatewayControl.
func (g *webGateway) Gateway() (websocket.GatewayInfo, error) {
	info, _, err := g.gateway()
	return info, err
}

func (g *webGateway) gateway() (websocket.GatewayInfo, bool, error) {
	st, err := g.status()
	if err != nil {
		return websocket.GatewayInfo{}, false, err
	}
	info := websocket.GatewayInfo{PID: g.pid, LaunchMode: gatewayruntime.LaunchUnknown, Lifetime: st.Lifetime}
	serviceOwned := false
	if st.Running && st.PID == g.pid && g.serviceStatus != nil {
		svc, serviceErr := g.serviceStatus()
		serviceOwned = serviceErr == nil && svc.Loaded && svc.PID == g.pid
	}
	switch {
	case !st.Running || st.PID != g.pid:
		info.RestartUnavailableReason = reasonUnmanaged
	case st.LaunchMode != gatewayruntime.LaunchBackground && !serviceOwned:
		info.LaunchMode, info.StartedAt = st.LaunchMode, st.StartedAt
		info.RestartUnavailableReason = reasonForeground
	default:
		info.LaunchMode, info.StartedAt = st.LaunchMode, st.StartedAt
		info.RestartAvailable = true
	}
	g.mu.Lock()
	info.Restarting = g.restarting
	g.mu.Unlock()
	return info, serviceOwned, nil
}

// Restart implements websocket.GatewayControl. A second request while
// the first one's restart is under way is accepted without another.
func (g *webGateway) Restart() (websocket.GatewayRestart, error) {
	info, serviceOwned, err := g.gateway()
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
	if serviceOwned {
		err = g.spawn([]string{"gateway", "service", "restart"})
	} else {
		err = g.spawn(g.argv())
	}
	if err != nil {
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
