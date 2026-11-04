package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// fakeMCP serves an in-memory "demo" server with an echo tool and fails
// every other server.
type fakeMCP struct {
	mu       sync.Mutex
	dials    int
	sessions []*sdk.ServerSession
}

func (f *fakeMCP) transport(name string, _ config.MCPServerConfig, _ io.Writer) (sdk.Transport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	if name != "demo" {
		return nil, errors.New("server " + name + " is down")
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "demo", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{
		Name:        "echo",
		Description: "Echo text.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ Text string }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "demo says " + args.Text}}}, nil
	})
	client, server := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), server, nil)
	if err != nil {
		return nil, err
	}
	f.sessions = append(f.sessions, ss)
	return client, nil
}

// allClosed reports whether every server session has ended.
func (f *fakeMCP) allClosed(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	sessions := slices.Clone(f.sessions)
	f.mu.Unlock()
	for _, ss := range sessions {
		done := make(chan struct{})
		go func() { _ = ss.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("an MCP session is still open after the command returned")
		}
	}
}

func (e *testEnv) saveMCPConfig(t *testing.T, servers map[string]config.MCPServerConfig) {
	t.Helper()
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	cfg.Tools.MCPServers = servers
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}
}

func TestStatusListsMCPServers(t *testing.T) {
	e := newTestEnv(t)
	fake := &fakeMCP{}
	e.mcpTransport = fake.transport
	e.saveMCPConfig(t, map[string]config.MCPServerConfig{
		"fs": {Command: "npx", Args: []string{"-y", "--token=sk-arg-secret"}, Env: map[string]string{"FS_TOKEN": "sk-env-secret"}},
		"remote": {
			URL:          "https://user:pw-secret@mcp.example.com/v1/sk-path-secret?key=sk-query-secret",
			Headers:      map[string]string{"Authorization": "Bearer sk-header-secret"},
			EnabledTools: []string{"search", "fetch"},
			ToolTimeout:  60,
		},
		"none": {URL: "http://localhost:9000", EnabledTools: []string{}},
		"off":  {Command: "srv", Enabled: new(false)},
	})
	if code := e.run(t, nil, "status"); code != 0 {
		t.Fatalf("status exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	if strings.Contains(out, "secret") {
		t.Fatalf("status printed a secret:\n%s", out)
	}
	for _, want := range []string{
		"MCP servers:", "4 configured",
		"stdio npx (2 args); env FS_TOKEN; tools *; configured",
		"streamableHttp https://mcp.example.com/...; headers Authorization; tools search, fetch; timeout 1m0s; configured",
		"streamableHttp http://localhost:9000; tools none; configured",
		"stdio srv; tools *; disabled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	if fake.dials != 0 {
		t.Error("status connected without --mcp")
	}
}

func TestStatusNoMCPServers(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run(t, nil, "status"); code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	if !strings.Contains(e.stdout.String(), "MCP servers:") || !strings.Contains(e.stdout.String(), "none") {
		t.Errorf("stdout = %s", e.stdout)
	}
}

func TestStatusMCPConnects(t *testing.T) {
	e := newTestEnv(t)
	fake := &fakeMCP{}
	e.mcpTransport = fake.transport
	e.saveMCPConfig(t, map[string]config.MCPServerConfig{
		"demo":   {Command: "demo"},
		"broken": {Command: "broken"},
		"off":    {Command: "off", Enabled: new(false)},
	})
	if code := e.run(t, nil, "status", "--mcp"); code != 0 {
		t.Fatalf("status exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	for _, want := range []string{
		"stdio demo; tools *; connected (1 tools)",
		"stdio broken; tools *; failed: server broken is down",
		"stdio off; tools *; disabled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status --mcp output missing %q:\n%s", want, out)
		}
	}
	if fake.dials != 2 {
		t.Errorf("dials = %d, want 2", fake.dials)
	}
	fake.allClosed(t)
}

func TestAgentUsesMCPTools(t *testing.T) {
	e := newTestEnv(t)
	fake := &fakeMCP{}
	e.mcpTransport = fake.transport
	e.saveMCPConfig(t, map[string]config.MCPServerConfig{"demo": {Command: "demo"}, "broken": {Command: "broken"}})

	var mu sync.Mutex
	var reqs []providers.ChatRequest
	e.provider.chat = func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		reqs = append(reqs, req)
		if len(reqs) == 1 {
			return &providers.ChatResponse{FinishReason: providers.FinishToolCalls, ToolCalls: []providers.ToolCall{
				{ID: "c1", Name: "mcp_demo_echo", Arguments: json.RawMessage(`{"text":"hi"}`)},
			}}, nil
		}
		return &providers.ChatResponse{Content: "done", FinishReason: providers.FinishStop}, nil
	}
	if code := e.run(t, nil, "agent", "-m", "use the demo"); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	var names []string
	for _, d := range reqs[0].Tools {
		names = append(names, d.Name)
	}
	if !slices.Contains(names, "mcp_demo_echo") || names[len(names)-1] != "mcp_demo_echo" {
		t.Errorf("tools offered = %v, want mcp_demo_echo last", names)
	}
	msgs := reqs[1].Messages
	if res := msgs[len(msgs)-1]; res.Role != providers.RoleTool || res.IsError || res.Content != "demo says hi" {
		t.Errorf("tool result = %+v", res)
	}
	// One-shot mode stays quiet about MCP unless --verbose.
	if strings.Contains(e.stderr.String(), "MCP:") {
		t.Errorf("one-shot printed an MCP summary: %q", e.stderr)
	}
	fake.allClosed(t)
}

func TestAgentREPLReportsMCP(t *testing.T) {
	e := newTestEnv(t)
	fake := &fakeMCP{}
	e.mcpTransport = fake.transport
	e.saveMCPConfig(t, map[string]config.MCPServerConfig{"demo": {Command: "demo"}, "broken": {Command: "broken"}})
	if code := e.run(t, strings.NewReader("hello\nexit\n"), "agent"); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	want := "MCP: broken failed: server broken is down, demo connected (1 tools)\n"
	if !strings.Contains(e.stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", e.stderr, want)
	}
	fake.allClosed(t)
}

func TestDreamGetsNoMCPTools(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	fake := &fakeMCP{}
	e.mcpTransport = fake.transport
	e.saveMCPConfig(t, map[string]config.MCPServerConfig{"demo": {Command: "demo"}})
	appendHistory(t, e.workspace, "- [durable] Ada likes tea.")
	var tools []string
	e.provider.chat = func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		for _, d := range req.Tools {
			tools = append(tools, d.Name)
		}
		return &providers.ChatResponse{Content: "nothing to save", FinishReason: providers.FinishStop}, nil
	}
	if code := e.run(t, nil, "dream"); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if len(tools) == 0 {
		t.Fatal("dream offered no tools; the test would prove nothing")
	}
	for _, n := range tools {
		if strings.HasPrefix(n, "mcp_") {
			t.Errorf("dream offered MCP tool %s", n)
		}
	}
	if fake.dials != 0 {
		t.Errorf("dream connected to MCP servers (%d dials)", fake.dials)
	}
}

func TestRedactURL(t *testing.T) {
	tests := map[string]string{
		"https://mcp.example.com/mcp":               "https://mcp.example.com/...",
		"http://localhost:8080":                     "http://localhost:8080",
		"http://localhost:8080/":                    "http://localhost:8080",
		"https://u:p@h.example/x?token=1":           "https://h.example/...",
		"https://h.example?token=1":                 "https://h.example/...",
		"${MCP_URL}":                                "<url>",
		"https://${HOST}/mcp":                       "<url>",
		"http://[::1]:9000/sse":                     "http://[::1]:9000/...",
		"not a url at all, with spaces and %zz fun": "<url>",
	}
	for in, want := range tests {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
