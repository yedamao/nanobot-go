package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// fakeServers serves in-memory MCP servers to a Manager.
type fakeServers struct {
	mu       sync.Mutex
	build    map[string]func() *sdk.Server
	fail     map[string]error // transport factory errors
	hang     map[string]bool  // accept the connection but never answer
	dials    map[string]int
	sessions map[string][]*sdk.ServerSession
}

func newFakeServers() *fakeServers {
	return &fakeServers{
		build:    map[string]func() *sdk.Server{},
		fail:     map[string]error{},
		hang:     map[string]bool{},
		dials:    map[string]int{},
		sessions: map[string][]*sdk.ServerSession{},
	}
}

func (f *fakeServers) transport(name string, _ config.MCPServerConfig, _ io.Writer) (sdk.Transport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials[name]++
	if err := f.fail[name]; err != nil {
		return nil, err
	}
	client, server := sdk.NewInMemoryTransports()
	if f.hang[name] {
		conn, err := server.Connect(context.Background())
		if err != nil {
			return nil, err
		}
		go func() { // read and drop everything until the client hangs up
			for {
				if _, err := conn.Read(context.Background()); err != nil {
					return
				}
			}
		}()
		return client, nil
	}
	build, ok := f.build[name]
	if !ok {
		return nil, errors.New("no such fake server")
	}
	ss, err := build().Connect(context.Background(), server, nil)
	if err != nil {
		return nil, err
	}
	f.sessions[name] = append(f.sessions[name], ss)
	return client, nil
}

func (f *fakeServers) dialCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials[name]
}

// dropSessions closes the server side of every session of name, as if
// the server went away.
func (f *fakeServers) dropSessions(name string) {
	f.mu.Lock()
	ss := f.sessions[name]
	f.sessions[name] = nil
	f.mu.Unlock()
	for _, s := range ss {
		_ = s.Close()
	}
}

func textResult(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

var objectSchema = map[string]any{"type": "object"}

// testServer offers echo (read-only), fail (IsError), sleep and add.
func testServer() *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{
		Name:        "echo",
		Description: "Echo text back.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []any{"text"},
		},
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ Text string }
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return textResult(args.Text), nil
	})
	srv.AddTool(&sdk.Tool{Name: "fail", InputSchema: objectSchema},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "it broke"}}, IsError: true}, nil
		})
	srv.AddTool(&sdk.Tool{Name: "sleep", InputSchema: objectSchema},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Second):
				return textResult("woke"), nil
			}
		})
	srv.AddTool(&sdk.Tool{
		Name: "add",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"$ref": "#/$defs/num"},
				"b": map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}},
			},
			"$defs": map[string]any{"num": map[string]any{"type": "integer"}},
		},
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ A, B int }
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return textResult(strings.Repeat("|", args.A+args.B)), nil
	})
	return srv
}

// syncBuffer is a log sink safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	fakes *fakeServers
	reg   *tools.Registry
	mgr   *Manager
	logs  *syncBuffer
}

func newHarness(t *testing.T, cfgs map[string]config.MCPServerConfig, opts ...Option) *harness {
	t.Helper()
	h := &harness{fakes: newFakeServers(), reg: tools.NewRegistry(), logs: &syncBuffer{}}
	for name := range cfgs {
		h.fakes.build[name] = testServer
	}
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opts = append([]Option{WithTransport(h.fakes.transport), WithConnectTimeout(5 * time.Second)}, opts...)
	h.mgr = NewManager(cfgs, h.reg, logger, opts...)
	t.Cleanup(func() { _ = h.mgr.Close() })
	return h
}

func (h *harness) connect(t *testing.T) {
	t.Helper()
	if err := h.mgr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func (h *harness) status(t *testing.T, name string) ServerStatus {
	t.Helper()
	for _, s := range h.mgr.Status() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for %s", name)
	return ServerStatus{}
}

func (h *harness) exec(name, args string) tools.Result {
	return h.reg.Execute(context.Background(), name, json.RawMessage(args))
}

func stdio() config.MCPServerConfig { return config.MCPServerConfig{Command: "unused"} }

func TestConnectRegistersNamespacedTools(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio(), "beta-2": stdio()})
	h.connect(t)

	want := []string{
		"mcp_alpha_add", "mcp_alpha_echo", "mcp_alpha_fail", "mcp_alpha_sleep",
		"mcp_beta-2_add", "mcp_beta-2_echo", "mcp_beta-2_fail", "mcp_beta-2_sleep",
	}
	if got := h.reg.Names(); !slices.Equal(got, want) {
		t.Fatalf("registered %v, want %v", got, want)
	}
	echo, _ := h.reg.Get("mcp_alpha_echo")
	if echo.Description() != "[MCP alpha] Echo text back." || !echo.ReadOnly() {
		t.Errorf("echo: description %q, read-only %t", echo.Description(), echo.ReadOnly())
	}
	if fail, _ := h.reg.Get("mcp_alpha_fail"); fail.ReadOnly() || fail.Description() != "[MCP alpha] fail" {
		t.Errorf("fail: description %q, read-only %t", fail.Description(), fail.ReadOnly())
	}
	add, _ := h.reg.Get("mcp_alpha_add")
	props := add.Parameters()["properties"].(map[string]any)
	if props["a"].(map[string]any)["type"] != "integer" || props["b"].(map[string]any)["nullable"] != true {
		t.Errorf("add schema not normalized: %v", add.Parameters())
	}
	if _, ok := add.Parameters()["$defs"]; ok {
		t.Error("$defs left in a fully inlined schema")
	}

	st := h.status(t, "alpha")
	if st.State != StateConnected || st.Transport != config.MCPStdio || len(st.Tools) != 4 || st.Err != nil {
		t.Errorf("alpha status = %+v", st)
	}
	// Connecting again is a no-op for connected servers.
	h.connect(t)
	if n := h.fakes.dialCount("alpha"); n != 1 {
		t.Errorf("second Connect dialed again (%d dials)", n)
	}
}

func TestCallTool(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.connect(t)

	if r := h.exec("mcp_alpha_echo", `{"text": "hello"}`); r.IsError || r.Content != "hello" {
		t.Errorf("echo = %+v", r)
	}
	// Registry validation runs on the normalized schema: "2" is coerced.
	if r := h.exec("mcp_alpha_add", `{"a": "2", "b": 1}`); r.IsError || r.Content != "|||" {
		t.Errorf("add = %+v", r)
	}
	if r := h.exec("mcp_alpha_echo", `{}`); !r.IsError || !strings.Contains(r.Content, "missing required parameter 'text'") {
		t.Errorf("echo without text = %+v", r)
	}
	r := h.exec("mcp_alpha_fail", `{}`)
	if !r.IsError || !strings.HasPrefix(r.Content, "it broke") {
		t.Errorf("fail = %+v", r)
	}
}

func TestCallTimeout(t *testing.T) {
	cfg := stdio()
	cfg.ToolTimeout = 1
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": cfg})
	h.connect(t)

	start := time.Now()
	r := h.exec("mcp_alpha_sleep", `{}`)
	if !r.IsError || !strings.Contains(r.Content, "timed out after 1s") {
		t.Errorf("sleep = %+v", r)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("timeout took %s", d)
	}
	// The session is still usable.
	if r := h.exec("mcp_alpha_echo", `{"text": "still here"}`); r.Content != "still here" {
		t.Errorf("echo after timeout = %+v", r)
	}
	if n := h.fakes.dialCount("alpha"); n != 1 {
		t.Errorf("a timeout caused a reconnect (%d dials)", n)
	}
}

func TestCallCanceled(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	tool, _ := h.reg.Get("mcp_alpha_sleep")
	r := tool.Execute(ctx, json.RawMessage(`{}`))
	if !r.IsError || !strings.Contains(r.Content, "canceled") {
		t.Errorf("canceled call = %+v", r)
	}
}

func TestEnabledTools(t *testing.T) {
	tests := []struct {
		name    string
		enabled []string
		want    []string
		warn    string
	}{
		{"default", nil, []string{"mcp_s_add", "mcp_s_echo", "mcp_s_fail", "mcp_s_sleep"}, ""},
		{"star", []string{"*"}, []string{"mcp_s_add", "mcp_s_echo", "mcp_s_fail", "mcp_s_sleep"}, ""},
		{"raw and wrapped names", []string{"echo", "mcp_s_add", "nope"}, []string{"mcp_s_add", "mcp_s_echo"}, "entry=nope"},
		{"none", []string{}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := stdio()
			cfg.EnabledTools = tt.enabled
			h := newHarness(t, map[string]config.MCPServerConfig{"s": cfg})
			h.connect(t)
			if got := h.reg.Names(); !slices.Equal(got, tt.want) {
				t.Errorf("registered %v, want %v", got, tt.want)
			}
			if st := h.status(t, "s"); st.State != StateConnected || !slices.Equal(st.Tools, tt.want) {
				t.Errorf("status = %+v", st)
			}
			if tt.warn != "" && !strings.Contains(h.logs.String(), tt.warn) {
				t.Errorf("no warning containing %q in logs:\n%s", tt.warn, h.logs)
			}
		})
	}
}

func TestNameCollisions(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"s": stdio()})
	h.fakes.build["s"] = func() *sdk.Server {
		srv := sdk.NewServer(&sdk.Implementation{Name: "dup", Version: "1"}, nil)
		for _, name := range []string{"a.b", "a_b", "taken", strings.Repeat("long_name_", 8) + "1", strings.Repeat("long_name_", 8) + "2"} {
			srv.AddTool(&sdk.Tool{Name: name, InputSchema: objectSchema},
				func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					return textResult(name), nil
				})
		}
		return srv
	}
	// A built-in already owns the name the "taken" tool maps to.
	builtin := &stubTool{name: "mcp_s_taken"}
	if err := h.reg.Register(builtin); err != nil {
		t.Fatal(err)
	}
	h.connect(t)

	names := h.reg.Names()
	if len(names) != 4 { // mcp_s_a_b once, two hashed long names, the built-in
		t.Fatalf("registered %v", names)
	}
	if got, _ := h.reg.Get("mcp_s_taken"); got != builtin {
		t.Error("an MCP tool replaced a registered tool")
	}
	// The SDK lists tools sorted by name, so "a.b" wins mcp_s_a_b.
	if r := h.exec("mcp_s_a_b", `{}`); r.Content != "a.b" {
		t.Errorf("mcp_s_a_b = %+v", r)
	}
	var long []string
	for _, n := range names {
		if strings.HasPrefix(n, "mcp_s_long_name") {
			long = append(long, n)
			if len(n) > 64 {
				t.Errorf("%s is longer than 64", n)
			}
		}
	}
	if len(long) != 2 {
		t.Errorf("long names = %v, want 2 distinct", long)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "name collision") || !strings.Contains(logs, "already registered") {
		t.Errorf("collisions not logged:\n%s", logs)
	}
	if st := h.status(t, "s"); len(st.Tools) != 3 || slices.Contains(st.Tools, "mcp_s_taken") {
		t.Errorf("status tools = %v", st.Tools)
	}
}

type stubTool struct{ name string }

func (s *stubTool) Name() string                                          { return s.name }
func (s *stubTool) Description() string                                   { return "stub" }
func (s *stubTool) Parameters() map[string]any                            { return tools.Object(nil) }
func (s *stubTool) ReadOnly() bool                                        { return true }
func (s *stubTool) Execute(context.Context, json.RawMessage) tools.Result { return tools.Text("stub") }

func TestOneServerFailingDoesNotStopOthers(t *testing.T) {
	cfgs := map[string]config.MCPServerConfig{
		"good":     stdio(),
		"broken":   stdio(),
		"hung":     stdio(),
		"disabled": {Command: "x", Enabled: new(false)},
		"needsenv": {Command: "x", Env: map[string]string{"K": "${NANOBOT_TEST_SURELY_UNSET}"}},
	}
	h := newHarness(t, cfgs, WithConnectTimeout(300*time.Millisecond), WithLookupEnv(func(string) (string, bool) { return "", false }))
	h.fakes.fail["broken"] = errors.New("spawn failed")
	h.fakes.hang["hung"] = true

	start := time.Now()
	h.connect(t)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Connect took %s; servers did not connect concurrently or the timeout was ignored", d)
	}
	want := map[string]struct {
		state State
		err   string
	}{
		"good":     {StateConnected, ""},
		"broken":   {StateFailed, "spawn failed"},
		"hung":     {StateFailed, "timed out after 300ms"},
		"disabled": {StateDisabled, ""},
		"needsenv": {StateFailed, "NANOBOT_TEST_SURELY_UNSET"},
	}
	for _, st := range h.mgr.Status() {
		w := want[st.Name]
		errText := ""
		if st.Err != nil {
			errText = st.Err.Error()
		}
		if st.State != w.state || (w.err == "") != (st.Err == nil) || !strings.Contains(errText, w.err) {
			t.Errorf("%s: state %s err %v, want %s %q", st.Name, st.State, st.Err, w.state, w.err)
		}
	}
	if h.fakes.dialCount("disabled") != 0 {
		t.Error("a disabled server was dialed")
	}
	if got := h.reg.Names(); len(got) != 4 || !strings.HasPrefix(got[0], "mcp_good_") {
		t.Errorf("registered %v, want only good's tools", got)
	}
}

func TestErrorsHideSecrets(t *testing.T) {
	env := map[string]string{"TOK": "query-secret-1", "KEY": "header-secret-2"}
	cfg := config.MCPServerConfig{
		URL:     "https://user:pw-secret-3@mcp.example.com/mcp?token=${TOK}",
		Headers: map[string]string{"Authorization": "Bearer ${KEY}"},
	}
	failing := func(_ string, cfg config.MCPServerConfig, _ io.Writer) (sdk.Transport, error) {
		return nil, fmt.Errorf("cannot reach %s (auth %q)", cfg.URL, cfg.Headers["Authorization"])
	}
	h := newHarness(t, map[string]config.MCPServerConfig{"remote": cfg}, WithTransport(failing),
		WithLookupEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }))
	h.connect(t)
	st := h.status(t, "remote")
	if st.Err == nil {
		t.Fatal("no error")
	}
	msg := st.Err.Error()
	for _, secret := range []string{"query-secret-1", "header-secret-2", "pw-secret-3"} {
		if strings.Contains(msg, secret) || strings.Contains(h.logs.String(), secret) {
			t.Errorf("secret %q leaked: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "mcp.example.com/mcp?[redacted]") {
		t.Errorf("error = %s", msg)
	}
}

func TestConnectCanceled(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"hung": stdio()})
	h.fakes.hang["hung"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.mgr.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Connect = %v, want deadline exceeded", err)
	}
	if st := h.status(t, "hung"); st.State != StateFailed {
		t.Errorf("status = %+v", st)
	}
}

func TestReconnectAfterSessionDies(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio(), "beta": stdio()})
	h.connect(t)
	tool, _ := h.reg.Get("mcp_alpha_echo")

	for round := 1; round <= 2; round++ {
		h.fakes.dropSessions("alpha")
		if r := h.exec("mcp_alpha_echo", `{"text": "back"}`); r.IsError || r.Content != "back" {
			t.Fatalf("round %d: echo after the session died = %+v", round, r)
		}
		if n := h.fakes.dialCount("alpha"); n != round+1 {
			t.Errorf("round %d: %d dials", round, n)
		}
	}
	// The tools stayed registered as the same objects; beta was untouched.
	if again, _ := h.reg.Get("mcp_alpha_echo"); again != tool {
		t.Error("reconnect re-registered the tools")
	}
	if n := h.fakes.dialCount("beta"); n != 1 {
		t.Errorf("beta dialed %d times", n)
	}
	if st := h.status(t, "alpha"); st.State != StateConnected || st.Err != nil {
		t.Errorf("alpha status = %+v", st)
	}
}

func TestReconnectFails(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.connect(t)
	h.fakes.mu.Lock()
	h.fakes.fail["alpha"] = errors.New("server gone for good")
	h.fakes.mu.Unlock()
	h.fakes.dropSessions("alpha")

	r := h.exec("mcp_alpha_echo", `{"text": "x"}`)
	if !r.IsError || !strings.Contains(r.Content, "server gone for good") {
		t.Errorf("echo = %+v", r)
	}
	// Within the backoff, calls fail fast without dialing again.
	dials := h.fakes.dialCount("alpha")
	if r := h.exec("mcp_alpha_echo", `{"text": "x"}`); !r.IsError || !strings.Contains(r.Content, "unavailable") {
		t.Errorf("second echo = %+v", r)
	}
	if n := h.fakes.dialCount("alpha"); n != dials {
		t.Errorf("dialed during backoff (%d -> %d)", dials, n)
	}
	if st := h.status(t, "alpha"); st.State != StateFailed {
		t.Errorf("status = %+v", st)
	}
}

func TestConcurrentCallsShareOneReconnect(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.connect(t)
	h.fakes.dropSessions("alpha")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if r := h.exec("mcp_alpha_echo", `{"text": "ok"}`); r.Content != "ok" {
				t.Errorf("echo = %+v", r)
			}
		})
	}
	wg.Wait()
	if n := h.fakes.dialCount("alpha"); n != 2 {
		t.Errorf("%d dials, want 2", n)
	}
}

func TestClose(t *testing.T) {
	base := runtime.NumGoroutine()
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio(), "beta": stdio(), "hung": stdio()},
		WithConnectTimeout(200*time.Millisecond))
	h.fakes.hang["hung"] = true
	h.connect(t)
	echo, _ := h.reg.Get("mcp_alpha_echo")

	// Calls in flight are canceled by Close (the SDK would otherwise wait
	// for them) and end with an error result.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if r := h.exec("mcp_alpha_sleep", `{}`); !r.IsError || !strings.Contains(r.Content, "shut down") {
				t.Errorf("in-flight call = %+v", r)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	for range 3 { // concurrent and repeated Close
		wg.Go(func() {
			if err := h.mgr.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	wg.Wait()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Close took %s with calls in flight", d)
	}
	if err := h.mgr.Close(); err != nil {
		t.Errorf("Close again: %v", err)
	}

	if names := h.reg.Names(); len(names) != 0 {
		t.Errorf("tools still registered after Close: %v", names)
	}
	if r := echo.Execute(context.Background(), json.RawMessage(`{"text":"x"}`)); !r.IsError || !strings.Contains(r.Content, "shut down") {
		t.Errorf("call after Close = %+v", r)
	}
	for _, st := range h.mgr.Status() {
		if st.State != StateClosed || len(st.Tools) != 0 {
			t.Errorf("%s after Close: %+v", st.Name, st)
		}
	}
	if err := h.mgr.Connect(context.Background()); err != nil {
		t.Errorf("Connect after Close = %v", err)
	}
	if names := h.reg.Names(); len(names) != 0 {
		t.Errorf("Connect after Close registered %v", names)
	}
	waitForGoroutines(t, base)
}

func TestCloseWhileConnecting(t *testing.T) {
	base := runtime.NumGoroutine()
	h := newHarness(t, map[string]config.MCPServerConfig{"hung": stdio(), "alpha": stdio()})
	h.fakes.hang["hung"] = true
	done := make(chan error)
	go func() { done <- h.mgr.Connect(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := h.mgr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("Connect = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Close took %s with a connection attempt in flight", d)
	}
	if names := h.reg.Names(); len(names) != 0 {
		t.Errorf("tools registered after Close: %v", names)
	}
	waitForGoroutines(t, base)
}

// waitForGoroutines fails if the goroutine count does not drop back to
// base, which would mean the Manager leaked some.
func waitForGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d, want <= %d\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRegistryRaceWithAgent exercises registration, calls and Definitions
// concurrently; run with -race.
func TestRegistryRaceWithAgent(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"a": stdio(), "b": stdio(), "c": stdio()})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, d := range h.reg.Definitions() {
				_ = d.Parameters["type"]
			}
			_ = h.exec("mcp_a_echo", `{"text": "x"}`)
			_ = h.mgr.Status()
		}
	})
	h.connect(t)
	h.fakes.dropSessions("b")
	_ = h.exec("mcp_b_echo", `{"text": "x"}`)
	_ = h.mgr.Close()
	close(stop)
	wg.Wait()
}
