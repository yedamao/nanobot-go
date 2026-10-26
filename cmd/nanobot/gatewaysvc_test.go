package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/gatewayservice"
)

// serviceEnv is a test env whose service installer writes under a fake
// home and records launchctl/systemctl commands instead of running them.
type serviceEnv struct {
	*testEnv
	home string
	cmds []string
}

func newServiceEnv(t *testing.T, port int) *serviceEnv {
	t.Helper()
	s := &serviceEnv{testEnv: newTestEnv(t)}
	s.home = filepath.Join(s.dir, "fake-home")
	s.writeWebUIConfig(t, port, false)
	return s
}

func (s *serviceEnv) run(args ...string) int {
	s.stdout.Reset()
	s.stderr.Reset()
	a := s.app(strings.NewReader(""))
	a.newServiceInstaller = func() (*gatewayservice.Installer, error) {
		return &gatewayservice.Installer{
			Home:       s.home,
			Platform:   "darwin",
			UID:        501,
			Executable: func() (string, error) { return "/usr/local/bin/nanobot", nil },
			Run: func(name string, args ...string) ([]byte, error) {
				s.cmds = append(s.cmds, strings.Join(append([]string{name}, args...), " "))
				return nil, nil
			},
			Getenv: func(string) string { return "" },
		}, nil
	}
	return a.run(context.Background(), args)
}

func TestGatewayServiceDryRun(t *testing.T) {
	port := freePort(t)
	s := newServiceEnv(t, port)
	if code := s.run("gateway", "install-service", "--dry-run", "--verbose"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr)
	}
	out := s.stdout.String()
	for _, want := range []string{
		"Gateway service dry run",
		"Manager:     launchd",
		"Service:     ai.nanobot-go.gateway",
		"launchctl bootstrap gui/501 " + filepath.Join(s.home, "Library/LaunchAgents/ai.nanobot-go.gateway.plist"),
		"launchctl kickstart -k gui/501/ai.nanobot-go.gateway",
		"<string>" + s.configPath + "</string>",
		"<string>--port</string>\n\t\t<string>" + strconv.Itoa(port) + "</string>",
		"<string>--verbose</string>",
		filepath.Join(filepath.Dir(s.configPath), "logs", "nanobot-go-gateway.launchd.log"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--background") {
		t.Error("service gateway runs with --background")
	}
	if len(s.cmds) != 0 {
		t.Errorf("dry run ran %v", s.cmds)
	}
	if _, err := os.Stat(filepath.Join(s.home, "Library")); !os.IsNotExist(err) {
		t.Error("dry run wrote under home")
	}

	// systemd, a port flag and the other dry runs.
	if code := s.run("gateway", "service", "install", "--dry-run", "--manager", "systemd", "--port", "18799", "--no-start"); code != 0 {
		t.Fatalf("systemd exit %d: %s", code, s.stderr)
	}
	out = s.stdout.String()
	for _, want := range []string{"Restart=always", "--port 18799", "systemctl --user enable nanobot-go-gateway.service"} {
		if !strings.Contains(out, want) {
			t.Errorf("systemd stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "systemctl --user restart") {
		t.Error("--no-start restarts the unit")
	}
	if code := s.run("gateway", "uninstall-service", "--dry-run"); code != 0 || !strings.Contains(s.stdout.String(), "launchctl bootout gui/501/ai.nanobot-go.gateway") {
		t.Errorf("uninstall dry run %d:\n%s", code, s.stdout)
	}
	if len(s.cmds) != 0 {
		t.Errorf("dry runs ran %v", s.cmds)
	}
}

func TestGatewayServiceRejects(t *testing.T) {
	s := newServiceEnv(t, 0)
	if code := s.run("gateway", "service", "install", "--dry-run"); code != 1 || !strings.Contains(s.stderr.String(), "fixed port") {
		t.Errorf("port 0: exit %d, %s", code, s.stderr)
	}
	if code := s.run("gateway", "service", "install", "--dry-run", "--port", "18799", "--name", "nanobot-gateway"); code != 1 || !strings.Contains(s.stderr.String(), "Python") {
		t.Errorf("python name: exit %d, %s", code, s.stderr)
	}
	if code := s.run("gateway", "service", "bogus"); code != 2 {
		t.Errorf("unknown subcommand: exit %d", code)
	}
}

// TestGatewayServiceInstall writes the LaunchAgent (without starting it),
// reports it in status and removes it again.
func TestGatewayServiceInstall(t *testing.T) {
	s := newServiceEnv(t, freePort(t))
	if code := s.run("gateway", "service", "install", "--no-start"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr)
	}
	plist := filepath.Join(s.home, "Library", "LaunchAgents", "ai.nanobot-go.gateway.plist")
	data, err := os.ReadFile(plist)
	if err != nil || !strings.Contains(string(data), "<key>RunAtLoad</key>\n\t<true/>") {
		t.Fatalf("plist %v:\n%s", err, data)
	}
	if want := []string{"launchctl enable gui/501/ai.nanobot-go.gateway"}; strings.Join(s.cmds, "|") != strings.Join(want, "|") {
		t.Errorf("ran %v", s.cmds)
	}
	if code := s.run("gateway", "service", "status"); code != 0 {
		t.Fatalf("status exit %d: %s", code, s.stderr)
	}
	for _, want := range []string{"Installed:   yes", "Path:        " + plist, "Running:     no"} {
		if !strings.Contains(s.stdout.String(), want) {
			t.Errorf("status missing %q:\n%s", want, s.stdout)
		}
	}
	if code := s.run("gateway", "service", "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, s.stderr)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Error("plist left behind")
	}
}

// TestGatewayServiceConflict refuses to start the service while its port
// is taken, before writing anything.
func TestGatewayServiceConflict(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := newServiceEnv(t, ln.Addr().(*net.TCPAddr).Port)
	if code := s.run("gateway", "service", "install"); code != 1 || !strings.Contains(s.stderr.String(), "already listens") {
		t.Errorf("exit %d: %s", code, s.stderr)
	}
	if _, err := os.Stat(filepath.Join(s.home, "Library")); !os.IsNotExist(err) {
		t.Error("refused install wrote the plist")
	}
	if code := s.run("gateway", "service", "install", "--no-start"); code != 0 {
		t.Errorf("--no-start ignores the port: exit %d, %s", code, s.stderr)
	}
	if code := s.run("gateway", "service", "start"); code != 1 || !strings.Contains(s.stderr.String(), "already listens") {
		t.Errorf("start: exit %d: %s", code, s.stderr)
	}
}

// TestGatewayServiceStartUsesInstalledPort checks the installed service's
// port, not the config's, before starting it.
func TestGatewayServiceStartUsesInstalledPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port
	s := newServiceEnv(t, freePort(t))
	if code := s.run("gateway", "service", "install", "--no-start", "--port", strconv.Itoa(taken)); code != 0 {
		t.Fatalf("install exit %d: %s", code, s.stderr)
	}
	if code := s.run("gateway", "service", "start"); code != 1 || !strings.Contains(s.stderr.String(), "127.0.0.1:"+strconv.Itoa(taken)) {
		t.Errorf("start: exit %d: %s", code, s.stderr)
	}
	if code := s.run("gateway", "service", "status", "--config", "/nonexistent/config.json"); code != 0 || !strings.Contains(s.stdout.String(), "Gateway for "+s.configPath) {
		t.Errorf("status ignores the installed config: exit %d\n%s%s", code, s.stdout, s.stderr)
	}
}
