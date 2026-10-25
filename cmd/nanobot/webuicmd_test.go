package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	wschan "github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

// fakeGateway serves /healthz and, with ui, /ui/ on a loopback port.
func fakeGateway(t *testing.T, ui bool) (port int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, healthBody) })
	if ui {
		mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html>") })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return portOf(t, srv.Listener.Addr().String())
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

// freePort returns a loopback port nothing listens on (for now).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return portOf(t, ln.Addr().String())
}

// writeWebUIConfig saves a config on port, with the websocket channel
// enabled when webUI is set.
func (e *testEnv) writeWebUIConfig(t *testing.T, port int, webUI bool) {
	t.Helper()
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	cfg.Gateway.Port = port
	cfg.Channels["telegram"] = json.RawMessage(`{"enabled":false}`)
	if webUI {
		cfg.Channels[wschan.Name] = json.RawMessage(`{"enabled":true}`)
	}
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
}

// webuiRun runs `webui args` in the background. opened receives the
// URLs the command opens in the browser; code its exit code.
type webuiRun struct {
	opened chan string
	code   chan int
}

func (e *testEnv) startWebUI(t *testing.T, a *app, args ...string) *webuiRun {
	t.Helper()
	r := &webuiRun{opened: make(chan string, 1), code: make(chan int, 1)}
	a.openBrowser = func(url string) error {
		r.opened <- url
		return nil
	}
	go func() { r.code <- a.run(context.Background(), append([]string{"webui"}, args...)) }()
	return r
}

func (r *webuiRun) waitOpened(t *testing.T, e *testEnv) string {
	t.Helper()
	select {
	case url := <-r.opened:
		return url
	case code := <-r.code:
		t.Fatalf("webui exited with %d before opening the browser; stderr %s", code, e.stderr)
	case <-time.After(30 * time.Second):
		t.Fatalf("webui did not open the browser; stderr %s", e.stderr)
	}
	return ""
}

func (r *webuiRun) waitExit(t *testing.T, e *testEnv) int {
	t.Helper()
	select {
	case code := <-r.code:
		return code
	case <-time.After(30 * time.Second):
		t.Fatalf("webui did not exit; stderr %s", e.stderr)
	}
	return 0
}

// helperCommand runs TestWebUIHelperProcess in place of the gateway and
// npm; it records its arguments in argsFile.
func helperCommand(argsFile string) func(name string, args ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestWebUIHelperProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "NANOBOT_WEBUI_HELPER=1", "NANOBOT_WEBUI_HELPER_ARGS="+argsFile)
		return cmd
	}
}

// TestWebUIHelperProcess is not a test: it is the fake gateway (args
// "gateway ... --port n") or fake Vite ("run dev -- --host h --port n
// --strictPort", which needs $NANOBOT_GO_GATEWAY_URL) the launcher
// tests start. It serves until interrupted.
func TestWebUIHelperProcess(t *testing.T) {
	if os.Getenv("NANOBOT_WEBUI_HELPER") == "" {
		t.Skip("helper process")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.WriteFile(os.Getenv("NANOBOT_WEBUI_HELPER_ARGS")+"."+args[0], []byte(strings.Join(args, " ")), 0o600)
	host, port := "127.0.0.1", ""
	for i, a := range args {
		switch a {
		case "--port":
			port = args[i+1]
		case "--host":
			host = args[i+1]
		}
	}
	mux := http.NewServeMux()
	if args[0] == "gateway" {
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, healthBody) })
	} else if os.Getenv(envGatewayURL) == "" {
		os.Exit(3)
	}
	mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html>") })
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		os.Exit(4)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go http.Serve(ln, mux)
	<-sigs
	os.Exit(0)
}

func TestWebUIBackgroundDeprecated(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run(t, nil, "webui", "--background", "-w", "/my ws"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	out := e.stderr.String()
	for _, want := range []string{"no longer owns gateway lifecycle", "nanobot gateway --config " + e.configPath + " --workspace '/my ws'", "nanobot webui --config " + e.configPath} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(e.configPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("--background created a config")
	}
}

func TestWebUIPortFlags(t *testing.T) {
	for _, c := range []struct {
		port, gw, want int
		err            string
	}{
		{0, 0, 0, ""},
		{8000, 0, 8000, ""},
		{0, 8001, 8001, ""},
		{8000, 8000, 8000, ""},
		{8000, 8001, 0, "no separate WebUI port"},
		{70000, 0, 0, "[1, 65535]"},
	} {
		got, err := webuiPort(c.port, c.gw)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("webuiPort(%d, %d) error = %v, want %q", c.port, c.gw, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("webuiPort(%d, %d) = %d, %v, want %d", c.port, c.gw, got, err, c.want)
		}
	}

	e := newTestEnv(t)
	if code := e.run(t, nil, "webui", "-p", "8000", "--gateway-port", "8001"); code != 1 || !strings.Contains(e.stderr.String(), "serves the WebUI on the gateway port") {
		t.Errorf("mismatch exit = %d, stderr %s", code, e.stderr)
	}
}

func TestEnsureLocalWebUI(t *testing.T) {
	cfg := config.Default()
	changes, created, err := ensureLocalWebUI(cfg, "", 0)
	if err != nil || len(changes) != 1 || created {
		t.Fatalf("changes = %v, %v, %v", changes, created, err)
	}
	if got := string(cfg.Channels[wschan.Name]); got != `{"enabled":true}` {
		t.Errorf("section = %s", got)
	}
	if changes, _, err := ensureLocalWebUI(cfg, "", 0); err != nil || len(changes) != 0 {
		t.Errorf("second pass changes = %v, %v", changes, err)
	}
	if changes, _, err := ensureLocalWebUI(cfg, "", 18888); err != nil || len(changes) != 1 || cfg.Gateway.Port != 18888 {
		t.Errorf("port changes = %v, %v, port %d", changes, err, cfg.Gateway.Port)
	}

	cfg.Channels[wschan.Name] = json.RawMessage(`{"enabled": false}`)
	if changes, _, _ := ensureLocalWebUI(cfg, "", 18888); len(changes) != 1 || string(cfg.Channels[wschan.Name]) != `{"enabled":true}` {
		t.Errorf("disabled section: changes %v, section %s", changes, cfg.Channels[wschan.Name])
	}
	cfg.Channels[wschan.Name] = json.RawMessage(`{"enabled": true, "tokn": "x"}`)
	if _, _, err := ensureLocalWebUI(cfg, "", 0); err == nil || !strings.Contains(err.Error(), "channels.websocket") {
		t.Errorf("unknown key error = %v", err)
	}
}

func TestEnsureLocalWebUIBeyondLoopback(t *testing.T) {
	cfg := config.Default()
	cfg.Channels[wschan.Name] = json.RawMessage(`{"enabled": true, "token_issue_secret": ""}`)
	changes, created, err := ensureLocalWebUI(cfg, "0.0.0.0", 0)
	if err != nil || !created || len(changes) != 2 || cfg.Gateway.Host != "0.0.0.0" {
		t.Fatalf("changes = %v, created %v, %v, host %q", changes, created, err, cfg.Gateway.Host)
	}
	ws, err := wschan.ParseConfig(cfg.Channels[wschan.Name])
	if err != nil || len(ws.TokenIssueSecret) < 40 || !ws.Enabled {
		t.Fatalf("section = %+v, %v", ws, err)
	}
	if strings.Contains(strings.Join(changes, " "), ws.TokenIssueSecret) {
		t.Errorf("changes show the secret: %v", changes)
	}
	// A configured secret is kept.
	if changes, created, err := ensureLocalWebUI(cfg, "0.0.0.0", 0); err != nil || created || len(changes) != 0 {
		t.Errorf("second pass = %v, %v, %v", changes, created, err)
	}
	cfg.Channels[wschan.Name] = json.RawMessage(`{"enabled": true, "token": "mine"}`)
	if _, created, _ := ensureLocalWebUI(cfg, "", 0); created || compact(cfg.Channels[wschan.Name]) != `{"enabled":true,"token":"mine"}` {
		t.Errorf("token kept: created %v, section %s", created, cfg.Channels[wschan.Name])
	}
}

func TestWebUIURLs(t *testing.T) {
	for host, want := range map[string]string{
		"127.0.0.1": "http://127.0.0.1:18790/ui/",
		"0.0.0.0":   "http://127.0.0.1:18790/ui/",
		"":          "http://127.0.0.1:18790/ui/",
		"::":        "http://[::1]:18790/ui/",
		"::1":       "http://[::1]:18790/ui/",
		"[::1]":     "http://[::1]:18790/ui/",
		"localhost": "http://localhost:18790/ui/",
	} {
		if got := localURL(host, 18790, "/ui/"); got != want {
			t.Errorf("localURL(%q) = %s, want %s", host, got, want)
		}
	}
}

func TestWebUILogFilter(t *testing.T) {
	var out strings.Builder
	f := &logFilter{w: &out}
	io.WriteString(f, "time=x level=INFO msg=hi\ntime=x level=WARN msg=careful\ntime=x lev")
	io.WriteString(f, "el=ERROR msg=bad\nnanobot: boom\n")
	if got, want := out.String(), "time=x level=WARN msg=careful\ntime=x level=ERROR msg=bad\nnanobot: boom\n"; got != want {
		t.Errorf("filtered = %q, want %q", got, want)
	}
}

func TestWebUINeedsConfirmation(t *testing.T) {
	e := newTestEnv(t)
	// No config and no terminal: nothing is written.
	if code := e.run(t, nil, "webui", "--no-open"); code != 1 || !strings.Contains(e.stderr.String(), "--yes") {
		t.Fatalf("missing config exit = %d, stderr %s", code, e.stderr)
	}
	if _, err := os.Stat(e.configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("webui created a config without confirmation")
	}

	// A config without the WebUI is kept as it is.
	e.writeWebUIConfig(t, freePort(t), false)
	before, _ := os.ReadFile(e.configPath)
	if code := e.run(t, nil, "webui", "--no-open"); code != 1 || !strings.Contains(e.stdout.String(), "enable channels.websocket") {
		t.Fatalf("exit = %d, stdout %s, stderr %s", code, e.stdout, e.stderr)
	}
	// A terminal answering no.
	a := e.app(strings.NewReader("n\n"))
	a.isTerminal = func() bool { return true }
	if code := a.run(context.Background(), []string{"webui", "--no-open"}); code != 1 || !strings.Contains(e.stderr.String(), "cancelled") {
		t.Fatalf("declined exit = %d, stderr %s", code, e.stderr)
	}
	if after, _ := os.ReadFile(e.configPath); string(after) != string(before) {
		t.Error("declined setup changed the config")
	}
}

// TestWebUIBeyondLoopback needs confirmation to generate a sign-in
// secret for a gateway.host beyond loopback, and never prints it.
func TestWebUIBeyondLoopback(t *testing.T) {
	e := newTestEnv(t)
	port := fakeGateway(t, true)
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	cfg.Gateway.Host = "0.0.0.0"
	cfg.Gateway.Port = port
	cfg.Channels[wschan.Name] = json.RawMessage(`{"enabled":true}`)
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(e.configPath)
	if code := e.run(t, nil, "webui", "--no-open"); code != 1 || !strings.Contains(e.stderr.String(), "--yes") ||
		!strings.Contains(e.stdout.String(), "tokenIssueSecret") || !strings.Contains(e.stderr.String(), "beyond this device") {
		t.Fatalf("exit = %d, stdout %s, stderr %s", code, e.stdout, e.stderr)
	}
	if after, _ := os.ReadFile(e.configPath); string(after) != string(before) {
		t.Fatal("unconfirmed setup changed the config")
	}

	e.stdout.Reset()
	e.stderr.Reset()
	r := e.startWebUI(t, e.app(nil), "--yes")
	if url, want := r.waitOpened(t, e), localURL("127.0.0.1", port, "/ui/"); url != want {
		t.Errorf("opened %s, want %s", url, want)
	}
	e.interrupts <- os.Interrupt
	if code := r.waitExit(t, e); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	saved, err := config.Load(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := wschan.ParseConfig(saved.Channels[wschan.Name])
	if err != nil || ws.TokenIssueSecret == "" {
		t.Fatalf("websocket section %+v, %v", ws, err)
	}
	out := e.stdout.String() + e.stderr.String()
	if strings.Contains(out, ws.TokenIssueSecret) {
		t.Error("output shows the secret")
	}
	if !strings.Contains(out, "A tokenIssueSecret was written") {
		t.Errorf("output: %s", out)
	}

	// With a secret configured nothing changes and nothing is asked.
	e.stdout.Reset()
	e.stderr.Reset()
	r = e.startWebUI(t, e.app(nil), "--host", "0.0.0.0")
	r.waitOpened(t, e)
	e.interrupts <- os.Interrupt
	if code := r.waitExit(t, e); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if strings.Contains(e.stdout.String(), "Saved WebUI settings") {
		t.Errorf("stdout: %s", e.stdout)
	}
}

func TestWebUIHostFlag(t *testing.T) {
	e := newTestEnv(t)
	e.writeWebUIConfig(t, freePort(t), true)
	if code := e.run(t, nil, "webui", "--no-open", "--host", "192.168.1.5"); code != 1 ||
		!strings.Contains(e.stdout.String(), "set gateway.host to 192.168.1.5") {
		t.Errorf("exit = %d, stdout %s, stderr %s", code, e.stdout, e.stderr)
	}
}

// TestWebUIAttach opens the WebUI of a gateway already serving it,
// enabling it in the config after a terminal prompt, and leaves that
// gateway running.
func TestWebUIAttach(t *testing.T) {
	e := newTestEnv(t)
	port := fakeGateway(t, true)
	e.writeWebUIConfig(t, port, false)
	a := e.app(strings.NewReader("y\n"))
	a.isTerminal = func() bool { return true }
	r := e.startWebUI(t, a)
	if url, want := r.waitOpened(t, e), localURL("127.0.0.1", port, "/ui/"); url != want {
		t.Errorf("opened %s, want %s", url, want)
	}
	e.interrupts <- os.Interrupt
	if code := r.waitExit(t, e); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if !strings.Contains(e.stdout.String(), "already running") {
		t.Errorf("stdout: %s", e.stdout)
	}
	cfg, err := config.Load(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if compact(cfg.Channels[wschan.Name]) != `{"enabled":true}` || compact(cfg.Channels["telegram"]) != `{"enabled":false}` {
		t.Errorf("channels after setup: %s", cfg.Channels)
	}
	if !gatewayHealthy(context.Background(), http.DefaultClient, localURL("127.0.0.1", port, "")) {
		t.Error("webui stopped a gateway it attached to")
	}
}

// TestWebUICreatesConfig sets up a missing config with --yes.
func TestWebUICreatesConfig(t *testing.T) {
	e := newTestEnv(t)
	port := fakeGateway(t, true)
	r := e.startWebUI(t, e.app(nil), "-y", "-p", strconv.Itoa(port), "-w", e.workspace)
	r.waitOpened(t, e)
	e.interrupts <- os.Interrupt
	if code := r.waitExit(t, e); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	cfg, err := config.Load(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Port != port || compact(cfg.Channels[wschan.Name]) != `{"enabled":true}` {
		t.Errorf("saved port %d, websocket %s", cfg.Gateway.Port, cfg.Channels[wschan.Name])
	}
	if _, err := os.Stat(filepath.Join(e.workspace, "AGENTS.md")); err != nil {
		t.Errorf("workspace not bootstrapped: %v", err)
	}
}

func TestWebUIPortConflicts(t *testing.T) {
	e := newTestEnv(t)
	e.writeWebUIConfig(t, fakeGateway(t, false), true)
	if code := e.run(t, nil, "webui", "--no-open"); code != 1 || !strings.Contains(e.stderr.String(), "without the WebUI") {
		t.Errorf("health-only gateway: exit = %d, stderr %s", code, e.stderr)
	}

	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	e.writeWebUIConfig(t, portOf(t, other.Listener.Addr().String()), true)
	if code := e.run(t, nil, "webui", "--no-open"); code != 1 || !strings.Contains(e.stderr.String(), "already in use") {
		t.Errorf("other server: exit = %d, stderr %s", code, e.stderr)
	}
}

// TestWebUIStartsGateway starts a (fake) gateway child, waits for it,
// and stops it on Ctrl-C.
func TestWebUIStartsGateway(t *testing.T) {
	e := newTestEnv(t)
	port := freePort(t)
	e.writeWebUIConfig(t, port, true)
	argsFile := filepath.Join(e.dir, "args")
	a := e.app(nil)
	a.newCommand = helperCommand(argsFile)
	r := e.startWebUI(t, a, "--workspace", e.workspace)
	r.waitOpened(t, e)
	base := localURL("127.0.0.1", port, "")
	if !gatewayHealthy(context.Background(), http.DefaultClient, base) {
		t.Fatal("gateway not serving")
	}
	e.interrupts <- os.Interrupt
	if code := r.waitExit(t, e); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if tcpBusy(net.JoinHostPort("127.0.0.1", strconv.Itoa(port))) {
		t.Error("the gateway child still runs")
	}
	got, _ := os.ReadFile(argsFile + ".gateway")
	if want := "gateway --config " + e.configPath + " --workspace " + e.workspace + " --port " + strconv.Itoa(port); string(got) != want {
		t.Errorf("gateway args %q, want %q", got, want)
	}
	if !strings.Contains(e.stdout.String(), "This command started the gateway") {
		t.Errorf("stdout: %s", e.stdout)
	}
}

// TestWebUIDev runs (fake) Vite next to an attached gateway and stops
// only Vite.
func TestWebUIDev(t *testing.T) {
	e := newTestEnv(t)
	port := fakeGateway(t, true)
	e.writeWebUIConfig(t, port, true)
	src := filepath.Join(e.dir, "webui")
	os.MkdirAll(filepath.Join(src, "node_modules"), 0o700)
	os.WriteFile(filepath.Join(src, "package.json"), []byte("{}"), 0o600)
	e.env[envWebUIDir] = src

	a := e.app(nil)
	a.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if code := a.run(context.Background(), []string{"webui", "--dev", "--no-open"}); code != 1 || !strings.Contains(e.stderr.String(), "npm on PATH") {
		t.Fatalf("no npm: exit = %d, stderr %s", code, e.stderr)
	}

	argsFile := filepath.Join(e.dir, "args")
	vitePort := freePort(t)
	a = e.app(nil)
	a.lookPath = func(string) (string, error) { return "npm", nil }
	a.newCommand = helperCommand(argsFile)
	a.viteAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(vitePort))
	r := e.startWebUI(t, a, "--dev")
	if url, want := r.waitOpened(t, e), "http://"+a.viteAddr+"/ui/"; url != want {
		t.Errorf("opened %s, want %s", url, want)
	}
	e.interrupts <- os.Interrupt
	if code := r.waitExit(t, e); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if tcpBusy(a.viteAddr) {
		t.Error("Vite still runs")
	}
	if !gatewayHealthy(context.Background(), http.DefaultClient, localURL("127.0.0.1", port, "")) {
		t.Error("webui --dev stopped a gateway it attached to")
	}
	got, _ := os.ReadFile(argsFile + ".run")
	if want := "run dev -- --host 127.0.0.1 --port " + strconv.Itoa(vitePort) + " --strictPort"; string(got) != want {
		t.Errorf("npm args %q, want %q", got, want)
	}
}

// compact returns raw without insignificant whitespace.
func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}
