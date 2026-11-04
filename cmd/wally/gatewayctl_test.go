package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestGatewayBackground starts a (fake) background gateway, manages it
// with status, logs and stop, and refuses a second gateway for the
// config.
func TestGatewayBackground(t *testing.T) {
	e := newTestEnv(t)
	port := freePort(t)
	e.writeWebUIConfig(t, port, false)
	argsFile := filepath.Join(e.dir, "args")
	run := func(args ...string) int {
		e.stdout.Reset()
		e.stderr.Reset()
		a := e.app(strings.NewReader(""))
		a.newCommand = helperCommand(argsFile)
		return a.run(context.Background(), args)
	}
	t.Cleanup(func() { run("gateway", "stop") })

	if code := run("gateway", "--background"); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	for _, want := range []string{"Gateway started in the background.", "Launch Mode: background", "Lifetime:    explicit", "Logs:"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	base := localURL("127.0.0.1", port, "")
	if !gatewayHealthy(context.Background(), http.DefaultClient, base) {
		t.Fatal("background gateway not serving")
	}
	got, _ := os.ReadFile(argsFile + ".gateway")
	if want := "gateway --config " + e.configPath + " --port " + strconv.Itoa(port); string(got) != want {
		t.Errorf("gateway args %q, want %q", got, want)
	}

	if code := run("gateway", "--background"); code != 0 || !strings.Contains(e.stdout.String(), "already running in persistent background mode") {
		t.Errorf("second --background: exit %d, stdout %s", code, e.stdout)
	}
	// A foreground gateway for the same config is refused.
	if code := run("gateway", "--port", "0"); code != 1 || !strings.Contains(e.stderr.String(), "already running") {
		t.Errorf("foreground over background: exit %d, stderr %s", code, e.stderr)
	}
	if code := run("gateway", "status"); code != 0 || !strings.Contains(e.stdout.String(), "Running:     yes") {
		t.Errorf("status: exit %d, stdout %s", code, e.stdout)
	}
	if code := run("gateway", "logs", "--tail", "50"); code != 0 || !strings.Contains(e.stdout.String(), "starting in the background") {
		t.Errorf("logs: exit %d, stdout %s", code, e.stdout)
	}
	if code := run("gateway", "stop", "--timeout", "5s"); code != 0 || !strings.Contains(e.stdout.String(), "Gateway stopped.") {
		t.Fatalf("stop: exit %d, stdout %s, stderr %s", code, e.stdout, e.stderr)
	}
	if tcpBusy(net.JoinHostPort("127.0.0.1", strconv.Itoa(port))) {
		t.Error("the gateway still runs after stop")
	}
	if code := run("gateway", "stop"); code != 0 || !strings.Contains(e.stdout.String(), "not running") {
		t.Errorf("second stop: exit %d, stdout %s", code, e.stdout)
	}
	if code := run("gateway", "restart"); code != 1 || !strings.Contains(e.stderr.String(), "nothing to restart") {
		t.Errorf("restart idle: exit %d, stderr %s", code, e.stderr)
	}
}

// TestGatewayBackgroundUnmanaged refuses to start next to a gateway that
// has no state file (an older nanobot-go).
func TestGatewayBackgroundUnmanaged(t *testing.T) {
	e := newTestEnv(t)
	e.writeWebUIConfig(t, fakeGateway(t, false), false)
	if code := e.run(t, nil, "gateway", "--background"); code != 1 || !strings.Contains(e.stderr.String(), "does not manage") {
		t.Errorf("exit %d, stderr %s", code, e.stderr)
	}
}

// TestGatewayForegroundState records a foreground gateway in the state
// file while it runs.
func TestGatewayForegroundState(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, nil)
	g := startGateway(t, e, newFakeChat(&eventLog{}))
	e.stdout.Reset()
	if code := e.run(t, nil, "gateway", "status"); code != 0 {
		t.Fatalf("status exit %d", code)
	}
	out := e.stdout.String()
	for _, want := range []string{"Running:     yes", "PID:         " + strconv.Itoa(os.Getpid()), "Launch Mode: foreground", "Port:        " + strconv.Itoa(portOf(t, g.addr))} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	// It cannot be restarted as a background process.
	if code := e.run(t, nil, "gateway", "restart"); code != 1 || !strings.Contains(e.stderr.String(), "foreground terminal") {
		t.Errorf("restart: exit %d, stderr %s", code, e.stderr)
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("gateway exit %d", code)
	}
	e.stdout.Reset()
	e.run(t, nil, "gateway", "status")
	if !strings.Contains(e.stdout.String(), "Running:     no") {
		t.Errorf("status after exit:\n%s", e.stdout)
	}
}
