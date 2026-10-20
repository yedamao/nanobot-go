package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/config"
)

// serveRun is `nanobot serve` running in the background.
type serveRun struct {
	addr   string
	cancel context.CancelFunc // like the first SIGTERM
	done   chan int
}

// writeServeConfig writes a config for the test workspace.
func (e *testEnv) writeServeConfig(t *testing.T, mutate func(*config.Config)) {
	t.Helper()
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
}

// startServe runs `nanobot serve --port 0 args...` until it serves.
func startServe(t *testing.T, e *testEnv, args ...string) *serveRun {
	t.Helper()
	a := e.app(strings.NewReader(""))
	ready := make(chan string, 1)
	a.apiReady = func(addr string) { ready <- addr }
	a.exit = func(int) {}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &serveRun{cancel: cancel, done: make(chan int, 1)}
	go func() { s.done <- a.run(ctx, append([]string{"serve", "--port", "0"}, args...)) }()
	select {
	case s.addr = <-ready:
	case code := <-s.done:
		t.Fatalf("serve exited with %d before serving; stderr:\n%s", code, e.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	return s
}

func (s *serveRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-s.done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit")
		return -1
	}
}

// apiPost posts a chat completion for content and returns the status and
// body.
func apiPost(t *testing.T, addr, auth, content string) (int, string) {
	t.Helper()
	body := `{"messages":[{"role":"user","content":` + strconvQuote(content) + `}]}`
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// replyContent extracts choices[0].message.content.
func replyContent(t *testing.T, body string) string {
	t.Helper()
	var c struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil || len(c.Choices) != 1 {
		t.Fatalf("completion %q: %v", body, err)
	}
	return c.Choices[0].Message.Content
}

func TestServeRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	e.writeServeConfig(t, nil)
	s := startServe(t, e)

	code, body := apiPost(t, s.addr, "", "hello")
	if code != http.StatusOK || replyContent(t, body) != "echo: hello" {
		t.Errorf("POST = %d %s", code, body)
	}
	if code, body := get(t, "http://"+s.addr+"/v1/models"); code != http.StatusOK || !strings.Contains(body, `"id":"`+config.DefaultModel+`"`) {
		t.Errorf("GET /v1/models = %d %s", code, body)
	}
	if code, _ := get(t, "http://"+s.addr+"/health"); code != http.StatusOK {
		t.Errorf("GET /health = %d", code)
	}
	out := e.stdout.String()
	for _, want := range []string{"OpenAI API: http://" + s.addr + "/v1/chat/completions (no API key (loopback only))", "Model: " + config.DefaultModel, "Session: api:default", "Timeout: 2m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q:\n%s", want, out)
		}
	}

	s.cancel()
	if code := s.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	// The session store was closed cleanly and holds the turn.
	if sess, err := e.loadSession(t, "api:default"); err != nil || len(sess.Messages) != 2 {
		t.Errorf("session api:default = %+v, %v", sess, err)
	}
	if c, err := net.DialTimeout("tcp", s.addr, time.Second); err == nil {
		c.Close()
		t.Error("API still listening after exit")
	}
}

func TestServeAPIKey(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		env    string
		key    string
	}{
		{"from config", func(c *config.Config) { c.API.APIKey = "file-key" }, "", "file-key"},
		{"from env", nil, "env-key", "env-key"},
		{"env overrides config", func(c *config.Config) { c.API.APIKey = "file-key" }, "env-key", "env-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.writeServeConfig(t, tt.mutate)
			if tt.env != "" {
				e.env[config.EnvAPIKey] = tt.env
			}
			s := startServe(t, e)
			if code, _ := apiPost(t, s.addr, "", "hi"); code != http.StatusUnauthorized {
				t.Errorf("no key: status %d", code)
			}
			if code, _ := apiPost(t, s.addr, "wrong", "hi"); code != http.StatusUnauthorized {
				t.Errorf("wrong key: status %d", code)
			}
			if code, body := apiPost(t, s.addr, tt.key, "hi"); code != http.StatusOK {
				t.Errorf("right key: %d %s", code, body)
			}
			if !strings.Contains(e.stdout.String(), "bearer API key required") || strings.Contains(e.stdout.String(), tt.key) {
				t.Errorf("banner = %q", e.stdout)
			}
			s.cancel()
			s.wait(t)
		})
	}
}

func TestServeRemoteHost(t *testing.T) {
	// A non-loopback host without a key never listens.
	e := newTestEnv(t)
	e.writeServeConfig(t, func(c *config.Config) { c.API.Host = "0.0.0.0" })
	a := e.app(strings.NewReader(""))
	a.apiReady = func(string) { t.Error("serve started") }
	if code := a.run(context.Background(), []string{"serve", "--port", "0"}); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(e.stderr.String(), "api.apiKey is not set") {
		t.Errorf("stderr = %q", e.stderr)
	}

	// With a key it starts and warns.
	e = newTestEnv(t)
	e.writeServeConfig(t, func(c *config.Config) { c.API.APIKey = "k" })
	s := startServe(t, e, "--host", "0.0.0.0")
	if !strings.Contains(e.stderr.String(), "beyond this device") {
		t.Errorf("stderr = %q, want a warning", e.stderr)
	}
	s.cancel()
	if code := s.wait(t); code != 0 {
		t.Errorf("exit = %d", code)
	}
}

func TestServeStartupErrors(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	_, takenPort, _ := net.SplitHostPort(taken.Addr().String())

	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"port in use", []string{"--port", takenPort}, 1, "api:"},
		{"port out of range", []string{"--port", "70000"}, 1, "api.port"},
		{"bad timeout", []string{"--timeout", "-1"}, 1, "api.timeout"},
		{"remote host flag", []string{"--host", "192.168.1.5"}, 1, "api.apiKey is not set"},
		{"extra argument", []string{"extra"}, 2, "unexpected arguments"},
		{"help", []string{"-h"}, 0, "-timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.writeServeConfig(t, nil)
			a := e.app(strings.NewReader(""))
			a.apiReady = func(string) { t.Error("serve started") }
			if code := a.run(context.Background(), append([]string{"serve"}, tt.args...)); code != tt.code {
				t.Errorf("exit = %d, want %d; stderr:\n%s", code, tt.code, e.stderr)
			}
			if !strings.Contains(e.stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", e.stderr, tt.want)
			}
		})
	}
}

func TestServeTimeoutFlag(t *testing.T) {
	e := newTestEnv(t)
	e.writeServeConfig(t, nil)
	started := make(chan string, 1)
	e.provider.chat = blockingProvider(started, nil) // never released
	s := startServe(t, e, "--timeout", "1")
	code, body := apiPost(t, s.addr, "", "slow")
	if code != http.StatusGatewayTimeout || !strings.Contains(body, "timed out after 1s") {
		t.Errorf("POST = %d %s", code, body)
	}
	s.cancel()
	s.wait(t)
}

func TestServeGracefulShutdown(t *testing.T) {
	e := newTestEnv(t)
	e.writeServeConfig(t, nil)
	started, release := make(chan string, 1), make(chan struct{})
	e.provider.chat = blockingProvider(started, release)
	s := startServe(t, e)

	type result struct {
		code int
		body string
	}
	res := make(chan result, 1)
	go func() {
		code, body := apiPost(t, s.addr, "", "slow")
		res <- result{code, body}
	}()
	<-started
	e.interrupts <- os.Interrupt
	// The request in flight keeps the server up.
	time.Sleep(100 * time.Millisecond)
	select {
	case code := <-s.done:
		t.Fatalf("serve exited (%d) with a request in flight", code)
	default:
	}
	close(release)
	if r := <-res; r.code != http.StatusOK || replyContent(t, r.body) != "done: slow" {
		t.Errorf("in-flight request = %+v", r)
	}
	if code := s.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	if sess, err := e.loadSession(t, "api:default"); err != nil || len(sess.Messages) != 2 {
		t.Errorf("session = %+v, %v", sess, err)
	}
}

func TestServeShutdownTimeoutCancelsRequest(t *testing.T) {
	e := newTestEnv(t)
	e.writeServeConfig(t, func(c *config.Config) { c.Gateway.ShutdownTimeout = 0 })
	started := make(chan string, 1)
	e.provider.chat = blockingProvider(started, nil) // never released
	s := startServe(t, e)

	res := make(chan int, 1)
	go func() {
		code, _ := apiPost(t, s.addr, "", "stuck")
		res <- code
	}()
	<-started
	s.cancel()
	if code := s.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	// The canceled turn answers 503, and is not saved.
	if code := <-res; code != http.StatusServiceUnavailable {
		t.Errorf("canceled request status = %d", code)
	}
	if sess, err := e.loadSession(t, "api:default"); err == nil && len(sess.Messages) != 0 {
		t.Errorf("canceled turn persisted: %+v", sess)
	}
}

func TestGatewayServesAPI(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		c.API.Enabled = true
		c.API.Port = 0
	})
	apiAddr := make(chan string, 1)
	events := &eventLog{}
	fake := newFakeChat(events)

	a := e.app(strings.NewReader(""))
	a.channelFactories = map[string]channels.Factory{"fake": fake.factory}
	a.apiReady = func(addr string) { apiAddr <- addr }
	ready := make(chan string, 1)
	a.gatewayReady = func(addr string) { ready <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- a.run(ctx, []string{"gateway", "--port", "0"}) }()
	var addr string
	select {
	case <-ready:
		addr = <-apiAddr
	case code := <-done:
		t.Fatalf("gateway exited with %d; stderr:\n%s", code, e.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not start")
	}

	code, body := apiPost(t, addr, "", "via api")
	if code != http.StatusOK || replyContent(t, body) != "echo: via api" {
		t.Errorf("POST = %d %s", code, body)
	}
	// The chat channel still works alongside.
	fake.receive(t, "1", "via chat")
	if out := waitOutbound(t, fake); out.Content != "echo: via chat" {
		t.Errorf("outbound = %+v", out)
	}
	if !strings.Contains(e.stdout.String(), "OpenAI API: http://"+addr+"/v1/chat/completions") {
		t.Errorf("banner = %q", e.stdout)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not exit")
	}
	if sess, err := e.loadSession(t, "api:default"); err != nil || len(sess.Messages) != 2 {
		t.Errorf("session api:default = %+v, %v", sess, err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Error("API still listening after exit")
	}
}

func TestGatewayAPIStartupErrors(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	takenPort := taken.Addr().(*net.TCPAddr).Port

	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"remote without key", func(c *config.Config) { c.API.Enabled, c.API.Host = true, "0.0.0.0" }, "api.apiKey is not set"},
		{"port in use", func(c *config.Config) { c.API.Enabled, c.API.Port = true, takenPort }, "api:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.writeGatewayConfig(t, tt.mutate)
			a := e.app(strings.NewReader(""))
			a.channelFactories = map[string]channels.Factory{"fake": newFakeChat(&eventLog{}).factory}
			a.apiReady = func(string) { t.Error("API started") }
			if code := a.run(context.Background(), []string{"gateway", "--port", "0"}); code != 1 {
				t.Errorf("exit = %d, want 1; stderr:\n%s", code, e.stderr)
			}
			if !strings.Contains(e.stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", e.stderr, tt.want)
			}
		})
	}
}

func TestGatewayAPIDisabledByDefault(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, nil)
	a := e.app(strings.NewReader(""))
	a.channelFactories = map[string]channels.Factory{"fake": newFakeChat(&eventLog{}).factory}
	a.apiReady = func(string) { t.Error("API started without api.enabled") }
	ready := make(chan string, 1)
	a.gatewayReady = func(addr string) { ready <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- a.run(ctx, []string{"gateway", "--port", "0"}) }()
	select {
	case <-ready:
	case code := <-done:
		t.Fatalf("gateway exited with %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not start")
	}
	if strings.Contains(e.stdout.String(), "OpenAI API") {
		t.Errorf("banner = %q", e.stdout)
	}
	cancel()
	<-done
}
