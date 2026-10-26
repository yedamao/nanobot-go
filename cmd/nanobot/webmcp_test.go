package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// runningMCP returns an MCP store for path whose running servers are the
// file's, with live reporting states.
func runningMCP(t *testing.T, path string, live func() []mcp.ServerStatus) *webMCP {
	t.Helper()
	s := runningSettings(t, path, nil)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "MCP_TOKEN" {
			return envSecret
		}
		return ""
	}
	return newWebMCP(s, cfg.Tools.MCPServers, live, getenv, nil)
}

func assertNoSecrets(t *testing.T, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fileSecret, envSecret, "argsecret", "querysecret", "userpass"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("JSON holds secret %q: %s", secret, data)
		}
	}
}

func TestWebMCPListRedacts(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningMCP(t, path, func() []mcp.ServerStatus {
		return []mcp.ServerStatus{{Name: "srv", State: mcp.StateFailed,
			Err: errors.New(`Post "https://mcp.example.test": header Bearer ` + fileSecret + `-mcp rejected`)}}
	})
	_, created, err := s.PutMCPServer("fs", websocket.MCPServerInput{
		Command: "npx", Args: []string{"-y", "server", "--api-key", "argsecret", "--token=argsecret", "sk-argsecret"},
		Env: &map[string]string{"GITHUB_TOKEN": fileSecret + "-env", "REF": "${MCP_TOKEN}"},
	})
	if err != nil || !created {
		t.Fatalf("put = %v, created %v", err, created)
	}
	if _, _, err := s.PutMCPServer("web", websocket.MCPServerInput{
		URL: "https://user:userpass@mcp2.example.test/v1/mcp?key=querysecret", Enabled: ptr(false),
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.MCPServers()
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, list)
	if !list.Live || len(list.Servers) != 3 {
		t.Fatalf("list = %+v", list)
	}
	byName := map[string]websocket.MCPServerView{}
	for _, v := range list.Servers {
		byName[v.Name] = v
	}
	fs := byName["fs"]
	if !slices.Equal(fs.Args, []string{"-y", "server", "--api-key", redacted, "--token=" + redacted, redacted}) ||
		!slices.Equal(fs.EnvKeys, []string{"GITHUB_TOKEN", "REF"}) || fs.State != "not_running" || !fs.RestartRequired || !fs.Enabled {
		t.Errorf("fs = %+v", fs)
	}
	web := byName["web"]
	if web.URL != "https://mcp2.example.test/v1/mcp?"+redacted || web.Enabled || web.RestartRequired {
		t.Errorf("web = %+v", web)
	}
	srv := byName["srv"]
	if !slices.Equal(srv.HeaderKeys, []string{"Authorization"}) || srv.State != "failed" || srv.RestartRequired ||
		!strings.Contains(srv.Error, "header "+redacted+" rejected") {
		t.Errorf("srv = %+v", srv)
	}
	if !slices.Equal(list.RestartRequired, []string{"fs"}) {
		t.Errorf("restart_required = %v", list.RestartRequired)
	}

	// The file keeps every value.
	data, _ := os.ReadFile(path)
	for _, kept := range []string{fileSecret + "-env", fileSecret + "-mcp", "querysecret", "argsecret"} {
		if !strings.Contains(string(data), kept) {
			t.Errorf("file lost %q", kept)
		}
	}
}

func TestWebMCPEnableReplaceDelete(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningMCP(t, path, nil)

	v, err := s.SetMCPServerEnabled("srv", false)
	if err != nil || v.Enabled || !v.RestartRequired || v.State != "unknown" {
		t.Fatalf("disable = %+v, %v", v, err)
	}
	v, err = s.SetMCPServerEnabled("srv", true)
	if err != nil || !v.Enabled || v.RestartRequired {
		t.Fatalf("enable = %+v, %v", v, err)
	}
	if _, err := s.SetMCPServerEnabled("nope", true); !errors.Is(err, websocket.ErrMCPNotFound) {
		t.Errorf("missing = %v", err)
	}

	// Replacing without headers keeps them; without enabled keeps it.
	s.SetMCPServerEnabled("srv", false)
	v, created, err := s.PutMCPServer("srv", websocket.MCPServerInput{URL: "https://mcp.example.test/other", ToolTimeout: 5})
	if err != nil || created || v.Enabled || !slices.Equal(v.HeaderKeys, []string{"Authorization"}) {
		t.Fatalf("replace = %+v, %v, %v", v, created, err)
	}
	cfg, _ := config.Load(path)
	if got := cfg.Tools.MCPServers["srv"]; got.Headers["Authorization"] != "Bearer "+fileSecret+"-mcp" || got.ToolTimeout != 5 {
		t.Errorf("saved = %+v", got)
	}

	// Invalid input is refused and nothing is saved.
	for name, in := range map[string]websocket.MCPServerInput{
		"bad name!": {Command: "x"},
		"both":      {Command: "x", URL: "https://a.test"},
		"neither":   {},
		"badurl":    {URL: "ftp://a.test"},
		"redacted":  {Command: "x", Args: []string{redacted}},
	} {
		if _, _, err := s.PutMCPServer(name, in); !errors.Is(err, websocket.ErrInvalidMCP) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	if err := s.DeleteMCPServer("srv"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMCPServer("srv"); !errors.Is(err, websocket.ErrMCPNotFound) {
		t.Errorf("second delete = %v", err)
	}
	list, err := s.MCPServers()
	if err != nil {
		t.Fatal(err)
	}
	// The running server is still listed, as removed.
	if len(list.Servers) != 1 || !list.Servers[0].Removed || !slices.Equal(list.RestartRequired, []string{"srv"}) {
		t.Errorf("after delete = %+v", list)
	}
	assertNoSecrets(t, list)
	// Other secrets survive.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), fileSecret+"-telegram") || !strings.Contains(string(data), fileSecret+"-anthropic") {
		t.Error("saving MCP servers lost other secrets")
	}
}

func TestRedactMCPURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"https://h.test":                  "https://h.test",
		"https://h.test/":                 "https://h.test/",
		"https://u:p@h.test:8080/a/b?x=1": "https://h.test:8080/a/b?***",
		"https://${HOST}/mcp#frag":        "https://${HOST}/mcp?***",
	} {
		if got := redactMCPURL(in); got != want {
			t.Errorf("redactMCPURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// hangTransport connects only when its context ends, failing.
type hangTransport struct{}

func (hangTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// probeTransport serves in-memory "demo" (an echo tool) and "empty" (no
// tools) servers, never answers "slow", rejects every HTTP server with
// an error quoting its headers, and builds real transports otherwise.
func probeTransport(t *testing.T) mcp.TransportFactory {
	return func(name string, cfg config.MCPServerConfig, stderr io.Writer) (sdk.Transport, error) {
		switch {
		case name == "demo" || name == "empty":
			srv := sdk.NewServer(&sdk.Implementation{Name: name, Version: "1"}, nil)
			if name == "demo" {
				srv.AddTool(&sdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
					func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
						return &sdk.CallToolResult{}, nil
					})
			}
			client, server := sdk.NewInMemoryTransports()
			ss, err := srv.Connect(context.Background(), server, nil)
			if err != nil {
				return nil, err
			}
			t.Cleanup(func() { _ = ss.Close() })
			return client, nil
		case name == "slow":
			return hangTransport{}, nil
		case cfg.URL != "":
			return nil, fmt.Errorf("POST %q: header %v: Unauthorized", cfg.URL, cfg.Headers)
		}
		return mcp.DefaultTransport(name, cfg, stderr)
	}
}

func TestWebMCPTestServer(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningMCP(t, path, nil)
	var dialed []string
	s.probe = func(cfg *config.Config) *mcp.Manager {
		dialed = slices.AppendSeq(dialed, maps.Keys(cfg.Tools.MCPServers))
		return mcp.NewManager(cfg.Tools.MCPServers, tools.NewRegistry(), nil,
			mcp.WithTransport(probeTransport(t)), mcp.WithConnectTimeout(200*time.Millisecond), mcp.WithLookupEnv(s.lookupEnv))
	}
	for name, in := range map[string]websocket.MCPServerInput{
		// The whitelist is ignored: the test reports every tool.
		"demo":    {Command: "demo", Args: []string{"--token", "argsecret"}, EnabledTools: []string{"other"}},
		"empty":   {Command: "empty"},
		"slow":    {Command: "slow"},
		"missing": {Command: "nanobot-test-no-such-binary", Env: &map[string]string{"KEY": fileSecret}},
		"needvar": {Command: "demo", Env: &map[string]string{"KEY": "${NANOBOT_TEST_UNSET}"}},
		"off":     {Command: "demo", Enabled: ptr(false)},
		"web":     {URL: "https://user:userpass@mcp.example.test/mcp?key=querysecret", Headers: &map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"}},
	} {
		if _, _, err := s.PutMCPServer(name, in); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, status, msg string
		tools             []string
		dials             bool
	}{
		{"demo", websocket.MCPTestOK, "demo connected with 1 tool.", []string{"mcp_demo_echo"}, true},
		{"empty", websocket.MCPTestOK, "empty connected, but reported no tools.", nil, true},
		{"slow", websocket.MCPTestError, "slow did not answer within", nil, true},
		{"missing", websocket.MCPTestError, `missing needs "nanobot-test-no-such-binary", which is not on PATH.`, nil, true},
		{"needvar", websocket.MCPTestNotConfigured, "NANOBOT_TEST_UNSET", nil, false},
		{"off", websocket.MCPTestNotConfigured, "off is disabled", nil, false},
		{"web", websocket.MCPTestRejected, "web refused access", nil, true},
		{"srv", websocket.MCPTestRejected, "srv refused access", nil, true},
	} {
		dialed = nil
		res, err := s.TestMCPServer(context.Background(), tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		assertNoSecrets(t, res)
		if res.OK != (tc.status == websocket.MCPTestOK) || res.Status != tc.status || !strings.Contains(res.Message, tc.msg) ||
			!slices.Equal(res.Tools, tc.tools) || (len(dialed) > 0) != tc.dials {
			t.Errorf("%s = %+v, dialed %v", tc.name, res, dialed)
		}
		if tc.dials && !slices.Equal(dialed, []string{tc.name}) {
			t.Errorf("%s dialed %v, want only itself", tc.name, dialed)
		}
	}
	if _, err := s.TestMCPServer(context.Background(), "nope"); !errors.Is(err, websocket.ErrMCPNotFound) {
		t.Errorf("unknown = %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("testing changed the config file")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res, err := s.TestMCPServer(ctx, "slow"); err != nil || res.OK || res.Status != websocket.MCPTestError {
		t.Errorf("canceled = %+v, %v", res, err)
	}
	s.probe = nil
	if _, err := s.TestMCPServer(context.Background(), "demo"); err == nil {
		t.Error("without a probe: no error")
	}
}
