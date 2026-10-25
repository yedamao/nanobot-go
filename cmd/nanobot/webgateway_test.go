package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/gatewayruntime"
)

// fakeWebGateway returns a control of process 42 whose recorded status
// is st and whose spawned restarts are collected.
func fakeWebGateway(st gatewayruntime.Status) (*webGateway, *[][]string) {
	var spawned [][]string
	g := &webGateway{
		status: func() (gatewayruntime.Status, error) { return st, nil },
		pid:    42,
		argv:   func() []string { return []string{"gateway", "restart", "--port", "18999"} },
		spawn: func(argv []string) error {
			spawned = append(spawned, argv)
			return nil
		},
	}
	return g, &spawned
}

func TestWebGatewayForegroundRefused(t *testing.T) {
	g, spawned := fakeWebGateway(gatewayruntime.Status{Running: true, PID: 42, LaunchMode: gatewayruntime.LaunchForeground, Lifetime: gatewayruntime.LifetimeExplicit})
	info, err := g.Gateway()
	if err != nil || info.RestartAvailable || info.RestartUnavailableReason != reasonForeground || info.LaunchMode != "foreground" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	_, err = g.Restart()
	var unavailable *websocket.RestartUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != reasonForeground || len(*spawned) != 0 {
		t.Errorf("restart = %v, spawned %v", err, *spawned)
	}
}

func TestWebGatewayOtherProcessRefused(t *testing.T) {
	for _, st := range []gatewayruntime.Status{
		{Running: false, LaunchMode: gatewayruntime.LaunchUnknown},
		{Running: true, PID: 7, LaunchMode: gatewayruntime.LaunchBackground},
	} {
		g, spawned := fakeWebGateway(st)
		info, _ := g.Gateway()
		if info.RestartAvailable || info.RestartUnavailableReason != reasonUnmanaged {
			t.Errorf("%+v: info = %+v", st, info)
		}
		if _, err := g.Restart(); err == nil || len(*spawned) != 0 {
			t.Errorf("%+v: restart = %v, spawned %v", st, err, *spawned)
		}
	}
}

func TestWebGatewayBackgroundRestartsOnce(t *testing.T) {
	g, spawned := fakeWebGateway(gatewayruntime.Status{Running: true, PID: 42, LaunchMode: gatewayruntime.LaunchBackground, Lifetime: gatewayruntime.LifetimeOnDemand, StartedAt: "2026-10-25T00:00:00Z"})
	info, err := g.Gateway()
	if err != nil || !info.RestartAvailable || info.Lifetime != "on_demand" || info.Restarting {
		t.Fatalf("info = %+v, %v", info, err)
	}
	res, err := g.Restart()
	if err != nil || !res.OK || len(*spawned) != 1 || !slices.Equal((*spawned)[0], []string{"gateway", "restart", "--port", "18999"}) {
		t.Fatalf("restart = %+v, %v, spawned %v", res, err, *spawned)
	}
	res, err = g.Restart()
	if err != nil || !res.OK || len(*spawned) != 1 {
		t.Errorf("second restart = %+v, %v, spawned %v", res, err, *spawned)
	}
	if info, _ := g.Gateway(); !info.Restarting {
		t.Errorf("info after restart = %+v", info)
	}
}

func TestWebGatewaySpawnFailureRetries(t *testing.T) {
	g, _ := fakeWebGateway(gatewayruntime.Status{Running: true, PID: 42, LaunchMode: gatewayruntime.LaunchBackground})
	g.spawn = func([]string) error { return errors.New("no exec") }
	if _, err := g.Restart(); err == nil {
		t.Fatal("restart succeeded")
	}
	if info, _ := g.Gateway(); info.Restarting {
		t.Error("marked restarting after a failed spawn")
	}
}

func TestWebGatewayRestartArgv(t *testing.T) {
	a := &app{getenv: func(string) string { return "" }}
	port := 0
	g, err := a.newWebGateway(t.TempDir()+"/config.json", "", "127.0.0.1", true, func() int { return port })
	if err != nil {
		t.Fatal(err)
	}
	port = 18999
	argv := g.argv()
	if argv[0] != "gateway" || argv[1] != "restart" || !slices.Contains(argv, "--config") ||
		!slices.Equal(argv[len(argv)-5:], []string{"--host", "127.0.0.1", "--port", "18999", "--verbose"}) {
		t.Errorf("argv = %v", argv)
	}
}
