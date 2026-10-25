package main

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
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
	return newWebMCP(s, cfg.Tools.MCPServers, live, getenv)
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
