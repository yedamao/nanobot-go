package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// eventLog records gateway events in order.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// fakeChat is a channel whose inbound side is driven by the test through
// the bus it was built with.
type fakeChat struct {
	log  *eventLog
	sent chan bus.OutboundMessage

	mu  sync.Mutex
	bus *bus.MessageBus
}

func newFakeChat(log *eventLog) *fakeChat {
	return &fakeChat{log: log, sent: make(chan bus.OutboundMessage, 16)}
}

func (f *fakeChat) Name() string { return "fake" }
func (f *fakeChat) Start(context.Context) error {
	f.log.add("channel start")
	return nil
}
func (f *fakeChat) Stop(context.Context) error {
	f.log.add("channel stop")
	return nil
}
func (f *fakeChat) Send(_ context.Context, msg bus.OutboundMessage) error {
	if bus.IsProgress(msg) {
		return nil // like Telegram: no status display
	}
	f.log.add("send " + msg.Content)
	f.sent <- msg
	return nil
}

// receive publishes an inbound message as a chat platform would.
func (f *fakeChat) receive(t *testing.T, chatID, content string) {
	t.Helper()
	f.mu.Lock()
	b := f.bus
	f.mu.Unlock()
	msg := bus.InboundMessage{Channel: "fake", SenderID: "u1", ChatID: chatID, Content: content}
	if err := b.PublishInbound(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeChat) factory(raw json.RawMessage, deps channels.Deps) (channels.Channel, error) {
	var cfg struct {
		Enabled bool `json:"enabled"`
	}
	if err := channels.DecodeConfig(raw, &cfg); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.bus = deps.Bus
	f.mu.Unlock()
	return f, nil
}

func waitOutbound(t *testing.T, f *fakeChat) bus.OutboundMessage {
	t.Helper()
	select {
	case m := <-f.sent:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message delivered to the fake channel")
		return bus.OutboundMessage{}
	}
}

// writeGatewayConfig writes a config that enables the fake channel.
func (e *testEnv) writeGatewayConfig(t *testing.T, mutate func(*config.Config)) {
	t.Helper()
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	cfg.Channels["fake"] = json.RawMessage(`{"enabled": true}`)
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
}

// gatewayRun is a gateway running in the background.
type gatewayRun struct {
	addr   string
	cancel context.CancelFunc // like the first SIGTERM
	done   chan int
	exits  chan int // codes passed to app.exit
}

// startGateway runs `nanobot gateway --port 0 args...` until it serves.
func startGateway(t *testing.T, e *testEnv, fake *fakeChat, args ...string) *gatewayRun {
	t.Helper()
	a := e.app(strings.NewReader(""))
	if fake != nil {
		a.channelFactories = map[string]channels.Factory{"fake": fake.factory}
	}
	ready := make(chan string, 1)
	a.gatewayReady = func(addr string) { ready <- addr }
	g := &gatewayRun{done: make(chan int, 1), exits: make(chan int, 1)}
	a.exit = func(code int) { g.exits <- code }
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	t.Cleanup(cancel)
	go func() { g.done <- a.run(ctx, append([]string{"gateway", "--port", "0"}, args...)) }()
	select {
	case g.addr = <-ready:
	case code := <-g.done:
		t.Fatalf("gateway exited with %d before serving; stderr:\n%s", code, e.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not start")
	}
	return g
}

func (g *gatewayRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-g.done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not exit")
		return -1
	}
}

// running reports whether the gateway has not exited yet.
func (g *gatewayRun) running() bool {
	select {
	case code := <-g.done:
		g.done <- code
		return false
	default:
		return true
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// blockingProvider holds each request until released or canceled.
func blockingProvider(started chan<- string, release <-chan struct{}) func(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	return func(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		last := req.Messages[len(req.Messages)-1].Content
		started <- last
		select {
		case <-release:
			return &providers.ChatResponse{Content: "done: " + last, FinishReason: providers.FinishStop}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func TestGatewayRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, nil)
	events := &eventLog{}
	fake := newFakeChat(events)
	g := startGateway(t, e, fake)

	fake.receive(t, "42", "hello")
	out := waitOutbound(t, fake)
	if out.Channel != "fake" || out.ChatID != "42" || out.Content != "echo: hello" {
		t.Errorf("outbound = %+v", out)
	}

	// The health endpoint answers on the configured address.
	if code, body := get(t, "http://"+g.addr+"/healthz"); code != http.StatusOK || body != healthBody {
		t.Errorf("GET /healthz = %d %q", code, body)
	}
	if !strings.Contains(e.stdout.String(), "Health endpoint: http://"+g.addr+"/healthz") ||
		!strings.Contains(e.stdout.String(), "Channels: fake") {
		t.Errorf("banner = %q", e.stdout)
	}

	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	if got := events.list(); !slices.Equal(got, []string{"channel start", "send echo: hello", "channel stop"}) {
		t.Errorf("events = %v", got)
	}
	// The session store was closed cleanly and holds the turn.
	if s, err := e.loadSession(t, "fake:42"); err != nil || len(s.Messages) != 2 {
		t.Errorf("session fake:42 = %+v, %v", s, err)
	}
	// The health endpoint is closed.
	if c, err := net.DialTimeout("tcp", g.addr, time.Second); err == nil {
		c.Close()
		t.Error("health endpoint still listening after exit")
	}
}

func TestGatewayDropsRepliesForUnknownChannels(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, nil)
	fake := newFakeChat(&eventLog{})
	g := startGateway(t, e, fake)

	fake.mu.Lock()
	b := fake.bus
	fake.mu.Unlock()
	if err := b.PublishInbound(context.Background(), bus.InboundMessage{Channel: "ghost", ChatID: "1", Content: "boo"}); err != nil {
		t.Fatal(err)
	}
	fake.receive(t, "1", "after")
	if out := waitOutbound(t, fake); out.Content != "echo: after" {
		t.Errorf("outbound = %+v", out)
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(e.stderr.String(), "unknown channel") || !strings.Contains(e.stderr.String(), "channel=ghost") {
		t.Errorf("stderr = %q, want a drop warning for ghost", e.stderr)
	}
}

func TestGatewayGracefulShutdownFinishesInFlightTurn(t *testing.T) {
	before := runtime.NumGoroutine()
	e := newTestEnv(t)
	e.writeGatewayConfig(t, nil)
	started, release := make(chan string, 1), make(chan struct{})
	e.provider.chat = blockingProvider(started, release)
	events := &eventLog{}
	fake := newFakeChat(events)
	g := startGateway(t, e, fake)

	fake.receive(t, "7", "slow")
	<-started
	e.interrupts <- os.Interrupt
	// Shutdown waits for the turn: the channel stays up meanwhile.
	time.Sleep(100 * time.Millisecond)
	if !g.running() {
		t.Fatal("gateway exited with a turn in flight")
	}
	if slices.Contains(events.list(), "channel stop") {
		t.Fatal("channel stopped before the in-flight turn finished")
	}
	close(release)
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	// The reply went out before the channel stopped.
	if got := events.list(); !slices.Equal(got, []string{"channel start", "send done: slow", "channel stop"}) {
		t.Errorf("events = %v", got)
	}
	if !strings.Contains(e.stderr.String(), "Shutting down") {
		t.Errorf("stderr = %q", e.stderr)
	}
	select {
	case code := <-g.exits:
		t.Errorf("graceful shutdown called exit(%d)", code)
	default:
	}

	// No goroutine outlives the gateway.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<20)
		t.Errorf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}

func TestGatewayShutdownTimeoutCancelsTurn(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) { c.Gateway.ShutdownTimeout = 0 })
	started := make(chan string, 1)
	e.provider.chat = blockingProvider(started, nil) // never released
	events := &eventLog{}
	fake := newFakeChat(events)
	g := startGateway(t, e, fake)

	fake.receive(t, "7", "stuck")
	<-started
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	if got := events.list(); !slices.Equal(got, []string{"channel start", "channel stop"}) {
		t.Errorf("events = %v, want no reply for the canceled turn", got)
	}
	if !strings.Contains(e.stderr.String(), "did not finish in time") {
		t.Errorf("stderr = %q", e.stderr)
	}
	if s, err := e.loadSession(t, "fake:7"); err == nil && len(s.Messages) != 0 {
		t.Errorf("canceled turn persisted: %+v", s)
	}
}

func TestGatewaySecondSignalForcesExit(t *testing.T) {
	tests := []struct {
		name          string
		first, second func(*testEnv, *gatewayRun)
	}{
		{"sigterm then ctrl-c", sigterm, ctrlC},
		{"ctrl-c twice", ctrlC, ctrlC},
		{"sigterm twice", sigterm, func(e *testEnv, _ *gatewayRun) { e.terminations <- syscall.SIGTERM }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.writeGatewayConfig(t, nil) // 30s shutdown timeout
			started := make(chan string, 1)
			e.provider.chat = blockingProvider(started, nil) // never released
			fake := newFakeChat(&eventLog{})
			g := startGateway(t, e, fake)

			fake.receive(t, "1", "stuck")
			<-started
			tt.first(e, g)
			time.Sleep(50 * time.Millisecond)
			if !g.running() {
				t.Fatal("gateway exited without waiting for the turn")
			}
			tt.second(e, g)
			select {
			case code := <-g.exits:
				if code != 1 {
					t.Errorf("exit code = %d, want 1", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("second signal did not force the exit")
			}
			// In tests exit returns, so the gateway finishes its (now
			// immediate) cleanup instead of waiting 30s for the turn.
			start := time.Now()
			g.wait(t)
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("cleanup after a forced exit took %v", d)
			}
			if !strings.Contains(e.stderr.String(), "Forcing exit") {
				t.Errorf("stderr = %q", e.stderr)
			}
		})
	}
}

// sigterm and ctrlC deliver signals the way main routes them.
func sigterm(_ *testEnv, g *gatewayRun) { g.cancel() }
func ctrlC(e *testEnv, _ *gatewayRun)   { e.interrupts <- os.Interrupt }

func TestGatewayStartupErrors(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	takenPort := taken.Addr().(*net.TCPAddr).Port

	tests := []struct {
		name   string
		mutate func(*config.Config)
		args   []string
		code   int
		want   string
	}{
		{"unknown channel", func(c *config.Config) { c.Channels["nope"] = json.RawMessage(`{"enabled": true}`) }, nil, 1, "channels.nope: unknown channel"},
		{"bad channel config", func(c *config.Config) { c.Channels["fake"] = json.RawMessage(`{"enabled": true, "tokn": "x"}`) }, nil, 1, "tokn"},
		{"webui beyond loopback", func(c *config.Config) {
			c.Channels["websocket"] = json.RawMessage(`{"enabled": true}`)
			c.Gateway.Host = "0.0.0.0"
		}, nil, 1, "gateway.host must be a loopback address"},
		{"webui host flag beyond loopback", func(c *config.Config) {
			c.Channels["websocket"] = json.RawMessage(`{"enabled": true}`)
		}, []string{"--host", "0.0.0.0"}, 1, "refusing to serve the WebUI"},
		{"webui own port", func(c *config.Config) {
			c.Channels["websocket"] = json.RawMessage(`{"enabled": true, "port": 8765}`)
		}, nil, 1, "port"},
		{"port in use", nil, []string{"--port", strconv.Itoa(takenPort)}, 1, "health endpoint"},
		{"port flag out of range", nil, []string{"--port", "70000"}, 1, "gateway.port"},
		{"port flag not a number", nil, []string{"--port", "x"}, 2, "invalid value"},
		{"bad shutdown timeout", func(c *config.Config) { c.Gateway.ShutdownTimeout = -1 }, nil, 1, "gateway.shutdownTimeout"},
		{"extra argument", nil, []string{"extra"}, 2, "unexpected arguments"},
		{"help", nil, []string{"-h"}, 0, "-port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.writeGatewayConfig(t, tt.mutate)
			a := e.app(strings.NewReader(""))
			fake := newFakeChat(&eventLog{})
			a.channelFactories = map[string]channels.Factory{"fake": fake.factory}
			a.gatewayReady = func(string) { t.Error("gateway started") }
			code := a.run(context.Background(), append([]string{"gateway"}, tt.args...))
			if code != tt.code {
				t.Errorf("exit = %d, want %d; stderr:\n%s", code, tt.code, e.stderr)
			}
			if !strings.Contains(e.stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", e.stderr, tt.want)
			}
		})
	}
}

func TestGatewayHostFlagAndNoChannels(t *testing.T) {
	e := newTestEnv(t)
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	cfg.Gateway.Host = "0.0.0.0" // overridden by --host
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
	g := startGateway(t, e, nil, "--host", "localhost")
	host, _, err := net.SplitHostPort(g.addr)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("listening on %s, want loopback", g.addr)
	}
	if !strings.Contains(e.stdout.String(), "Channels: none running") {
		t.Errorf("stdout = %q", e.stdout)
	}
	if strings.Contains(e.stderr.String(), "beyond loopback") {
		t.Errorf("loopback host warned about: %q", e.stderr)
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
}

func TestHealthHandler(t *testing.T) {
	srv := httptest.NewServer(healthHandler())
	defer srv.Close()
	tests := []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodHead, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/healthz/x", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.code {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.code)
			}
			if tt.code == http.StatusOK && tt.method == http.MethodGet {
				var got map[string]string
				if err := json.Unmarshal(body, &got); err != nil || got["status"] != "ok" {
					t.Errorf("body = %q (%v)", body, err)
				}
				if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q", ct)
				}
			}
		})
	}
}

func TestRouteSignals(t *testing.T) {
	sigs := make(chan os.Signal, 8)
	interrupts := make(chan os.Signal, 1)
	terminations := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		routeSignals(sigs, cancel, interrupts, terminations)
		close(done)
	}()

	sigs <- os.Interrupt
	if got := <-interrupts; got != os.Interrupt {
		t.Errorf("interrupt routed as %v", got)
	}
	sigs <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first SIGTERM did not cancel the context")
	}
	sigs <- syscall.SIGTERM
	if got := <-terminations; got != syscall.SIGTERM {
		t.Errorf("second SIGTERM routed as %v", got)
	}
	// Nobody reading: signals are dropped instead of blocking the router.
	sigs <- os.Interrupt
	sigs <- os.Interrupt
	sigs <- syscall.SIGTERM
	sigs <- syscall.SIGTERM
	close(sigs)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("router blocked on an unread signal")
	}
	if len(interrupts) != 1 || len(terminations) != 1 {
		t.Errorf("buffered: %d interrupts, %d terminations", len(interrupts), len(terminations))
	}
}
