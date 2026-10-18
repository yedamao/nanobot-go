package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// fakeProvider answers "echo: <last user message>" unless chat is set.
type fakeProvider struct {
	chat func(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error)
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Chat(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	if f.chat != nil {
		return f.chat(ctx, req)
	}
	last := req.Messages[len(req.Messages)-1].Content
	return &providers.ChatResponse{Content: "echo: " + last, FinishReason: providers.FinishStop}, nil
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
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

type testEnv struct {
	dir, configPath, workspace string
	env                        map[string]string
	stdout, stderr             *syncBuffer
	interrupts                 chan os.Signal
	provider                   *fakeProvider
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	// Default paths such as ~/.nanobot-go/workspace must never touch the
	// real home directory.
	t.Setenv("HOME", filepath.Join(dir, "fake-home"))
	e := &testEnv{
		dir:        dir,
		configPath: filepath.Join(dir, "home", "config.json"),
		workspace:  filepath.Join(dir, "ws"),
		stdout:     &syncBuffer{},
		stderr:     &syncBuffer{},
		interrupts: make(chan os.Signal, 1),
		provider:   &fakeProvider{},
	}
	e.env = map[string]string{config.EnvConfigPath: e.configPath}
	return e
}

func (e *testEnv) app(stdin io.Reader) *app {
	return &app{
		stdin:       stdin,
		stdout:      e.stdout,
		stderr:      e.stderr,
		getenv:      func(k string) string { return e.env[k] },
		interrupts:  e.interrupts,
		newProvider: func(*config.Config) (providers.Provider, error) { return e.provider, nil },
	}
}

func (e *testEnv) run(t *testing.T, stdin io.Reader, args ...string) int {
	t.Helper()
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	return e.app(stdin).run(context.Background(), args)
}

func TestUsageAndVersion(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run(t, nil); code != 2 {
		t.Errorf("no args exit = %d, want 2", code)
	}
	if code := e.run(t, nil, "bogus"); code != 2 || !strings.Contains(e.stderr.String(), `unknown command "bogus"`) {
		t.Errorf("unknown command exit = %d, stderr %q", code, e.stderr)
	}
	if code := e.run(t, nil, "version"); code != 0 || !strings.Contains(e.stdout.String(), "nanobot-go "+version) {
		t.Errorf("version exit = %d, stdout %q", code, e.stdout)
	}
	if code := e.run(t, nil, "agent", "--nope"); code != 2 {
		t.Errorf("bad flag exit = %d, want 2", code)
	}
	if code := e.run(t, nil, "status", "-h"); code != 0 {
		t.Errorf("-h exit = %d, want 0", code)
	}
}

func TestOnboard(t *testing.T) {
	e := newTestEnv(t)
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	// Onboard with --config pointing at a fresh path writes the defaults.
	path := filepath.Join(e.dir, "other", "config.json")
	if code := e.run(t, nil, "onboard", "--config", path); code != 0 {
		t.Fatalf("onboard exit = %d, stderr %s", code, e.stderr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config perm = %o", info.Mode().Perm())
	}

	// With an existing config, onboard keeps it and creates its workspace.
	cfg.Providers.Anthropic.APIKey = "sk-keep-me"
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
	if code := e.run(t, nil, "onboard"); code != 0 {
		t.Fatalf("onboard exit = %d, stderr %s", code, e.stderr)
	}
	got, err := config.Load(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Providers.Anthropic.APIKey != "sk-keep-me" {
		t.Error("onboard overwrote an existing config without --force")
	}
	for _, f := range []string{"AGENTS.md", "SOUL.md", "USER.md"} {
		if _, err := os.Stat(filepath.Join(e.workspace, f)); err != nil {
			t.Errorf("workspace file %s: %v", f, err)
		}
	}
	if strings.Contains(e.stdout.String(), "sk-keep-me") {
		t.Error("onboard printed an API key")
	}

	// --force resets to defaults.
	if code := e.run(t, nil, "onboard", "--force"); code != 0 {
		t.Fatalf("onboard --force exit = %d", code)
	}
	if got, _ := config.Load(e.configPath); got.Providers.Anthropic.APIKey != "" {
		t.Error("--force did not reset the config")
	}
}

func TestStatusNeverPrintsKeys(t *testing.T) {
	e := newTestEnv(t)
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	cfg.Providers.Anthropic.APIKey = "sk-ant-secret-file"
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
	e.env["NANOBOT_GO_OPENAI_API_KEY"] = "sk-openai-secret-env"
	if code := e.run(t, nil, "status"); code != 0 {
		t.Fatalf("status exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	if strings.Contains(out, "secret") {
		t.Fatalf("status printed a key:\n%s", out)
	}
	for _, want := range []string{e.configPath, e.workspace + " (missing)", config.DefaultModel, "auto -> anthropic",
		"set (config)", "set (env)", "not set"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestAgentOneShot(t *testing.T) {
	e := newTestEnv(t)
	code := e.run(t, nil, "agent", "-m", "hello", "--workspace", e.workspace)
	if code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if got := e.stdout.String(); got != "echo: hello\n" {
		t.Errorf("stdout = %q", got)
	}
	// The turn was saved under the default session key next to the config.
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.configPath), "sessions", "cli_direct.jsonl")); err != nil {
		t.Errorf("session file: %v", err)
	}
	if _, err := os.Stat(e.workspace); err != nil {
		t.Errorf("workspace not created: %v", err)
	}

	// -s selects another session.
	if code := e.run(t, nil, "agent", "--message", "x", "-s", "cli:other", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.configPath), "sessions", "cli_other.jsonl")); err != nil {
		t.Errorf("session file for -s: %v", err)
	}
}

func TestAgentOneShotRunsTools(t *testing.T) {
	e := newTestEnv(t)
	var mu sync.Mutex
	var reqs []providers.ChatRequest
	e.provider.chat = func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		reqs = append(reqs, req)
		if len(reqs) == 1 {
			return &providers.ChatResponse{FinishReason: providers.FinishToolCalls, ToolCalls: []providers.ToolCall{
				{ID: "c1", Name: "write_file", Arguments: json.RawMessage(`{"path":"note.txt","content":"hi"}`)},
				{ID: "c2", Name: "write_file", Arguments: json.RawMessage(`{"path":"../escape.txt","content":"x"}`)},
			}}, nil
		}
		return &providers.ChatResponse{Content: "done", FinishReason: providers.FinishStop}, nil
	}
	if code := e.run(t, nil, "agent", "-m", "write a note", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if got := e.stdout.String(); got != "done\n" {
		t.Errorf("stdout = %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(e.workspace, "note.txt")); err != nil || string(data) != "hi" {
		t.Errorf("note.txt = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "escape.txt")); err == nil {
		t.Error("write_file escaped the workspace")
	}
	if !strings.Contains(e.stderr.String(), "↳ write_file") {
		t.Errorf("no tool hint on stderr: %q", e.stderr)
	}
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	msgs := reqs[1].Messages
	ok, escaped := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if ok.Role != providers.RoleTool || ok.IsError || escaped.ToolCallID != "c2" || !escaped.IsError ||
		!strings.Contains(escaped.Content, "outside the workspace") {
		t.Errorf("tool results = %+v / %+v", ok, escaped)
	}
}

func TestAgentOneShotProviderError(t *testing.T) {
	e := newTestEnv(t)
	e.provider.chat = func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
		return nil, &providers.APIError{Provider: "fake", Kind: providers.KindAuth, Message: "bad key"}
	}
	if code := e.run(t, nil, "agent", "-m", "hi", "--workspace", e.workspace); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(e.stderr.String(), "bad key") {
		t.Errorf("stderr = %q", e.stderr)
	}
}

func TestAgentREPL(t *testing.T) {
	e := newTestEnv(t)
	in := strings.NewReader("hello\n\n/help\nsecond\nexit\nnot reached\n")
	if code := e.run(t, in, "agent", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	for _, want := range []string{"echo: hello", "/new", "echo: second"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not reached") {
		t.Error("REPL kept reading after exit")
	}
}

func TestAgentREPLCompact(t *testing.T) {
	e := newTestEnv(t)
	in := strings.NewReader("hello\n/compact\nexit\n")
	if code := e.run(t, in, "agent", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "Compacted 2 messages into a summary.") {
		t.Errorf("output:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(e.workspace, "memory", "history.jsonl"))
	if err != nil || strings.Count(string(data), "\n") != 1 || !strings.Contains(string(data), `"cursor":1`) {
		t.Errorf("history.jsonl = %q, %v", data, err)
	}

	// Disabled in the config: /compact says so and writes nothing.
	e = newTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(e.configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.configPath, []byte(`{"agents": {"defaults": {"consolidation": {"enable": false}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in = strings.NewReader("hello\n/compact\nexit\n")
	if code := e.run(t, in, "agent", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "History consolidation is disabled.") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.workspace, "memory")); !os.IsNotExist(err) {
		t.Errorf("memory dir created while disabled: %v", err)
	}
}

func TestAgentREPLExitStopsReader(t *testing.T) {
	e := newTestEnv(t)
	before := runtime.NumGoroutine()
	// Input remains after "exit": the reader goroutine must not stay
	// blocked trying to hand it over.
	in := strings.NewReader("exit\nleft\nover\n")
	if code := e.run(t, in, "agent", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after the REPL returned", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentREPLInterruptCancelsTurn(t *testing.T) {
	e := newTestEnv(t)
	started := make(chan struct{}, 1)
	e.provider.chat = func(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		last := req.Messages[len(req.Messages)-1].Content
		if last == "slow" {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &providers.ChatResponse{Content: "echo: " + last, FinishReason: providers.FinishStop}, nil
	}
	inR, inW := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- e.run(t, inR, "agent", "--workspace", e.workspace) }()

	io.WriteString(inW, "slow\n")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	e.interrupts <- os.Interrupt
	io.WriteString(inW, "fast\n")
	inW.Close()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, stderr %s", code, e.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("REPL did not exit")
	}
	out := e.stdout.String()
	if !strings.Contains(out, "(canceled)") || !strings.Contains(out, "echo: fast") {
		t.Errorf("output:\n%s", out)
	}
	// The canceled turn is not in the session; the next one is.
	data, err := os.ReadFile(filepath.Join(filepath.Dir(e.configPath), "sessions", "cli_direct.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "slow") || !strings.Contains(string(data), "fast") {
		t.Errorf("session file:\n%s", data)
	}
}

func TestAgentMissingKeyFailsClearly(t *testing.T) {
	e := newTestEnv(t)
	a := e.app(strings.NewReader(""))
	a.newProvider = nil // use the real factory: no key configured
	if code := a.run(context.Background(), []string{"agent", "-m", "hi", "--workspace", e.workspace}); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(e.stderr.String(), "API key") {
		t.Errorf("stderr = %q", e.stderr)
	}
}
