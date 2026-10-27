package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadMCPServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"tools": {"mcpServers": {
	  "fs": {"command": "npx", "args": ["-y", "server-fs", "/tmp"], "env": {"TOKEN": "${FS_TOKEN}"}},
	  "remote": {"url": "https://mcp.example.com/mcp", "headers": {"Authorization": "Bearer ${API_TOKEN}"},
	    "toolTimeout": 60, "enabledTools": ["search", "mcp_remote_fetch"]},
	  "legacy": {"url": "https://mcp.example.com/sse/", "enabledTools": []},
	  "off": {"type": "stdio", "command": "srv", "enabled": false}
	}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	servers := cfg.Tools.MCPServers
	if len(servers) != 4 {
		t.Fatalf("got %d servers, want 4", len(servers))
	}
	tests := []struct {
		name      string
		transport string
		timeout   time.Duration
		tools     []string
		enabled   bool
	}{
		{"fs", MCPStdio, 30 * time.Second, []string{"*"}, true},
		{"remote", MCPStreamableHTTP, 60 * time.Second, []string{"search", "mcp_remote_fetch"}, true},
		{"legacy", MCPSSE, 30 * time.Second, []string{}, true},
		{"off", MCPStdio, 30 * time.Second, []string{"*"}, false},
	}
	for _, tt := range tests {
		s := servers[tt.name]
		if s.Transport() != tt.transport || s.Timeout() != tt.timeout || s.IsEnabled() != tt.enabled ||
			!reflect.DeepEqual(s.AllowedTools(), tt.tools) {
			t.Errorf("%s: transport %s timeout %s enabled %t tools %q, want %s %s %t %q", tt.name,
				s.Transport(), s.Timeout(), s.IsEnabled(), s.AllowedTools(), tt.transport, tt.timeout, tt.enabled, tt.tools)
		}
	}
	// ${VAR} references stay unexpanded in the loaded config.
	if servers["fs"].Env["TOKEN"] != "${FS_TOKEN}" {
		t.Errorf("env expanded at load time: %q", servers["fs"].Env["TOKEN"])
	}

	// The config survives a save and reload, including enabledTools: [].
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tools.MCPServers, servers) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got.Tools.MCPServers, servers)
	}
}

func TestLoadMCPUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"tools": {"mcpServers": {"fs": {"command": "x", "tool_timeout": 5}}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), `unknown field "tool_timeout"`) {
		t.Errorf("Load error = %v, want unknown field", err)
	}
}

func TestValidateMCP(t *testing.T) {
	tests := []struct {
		name   string
		server MCPServerConfig
		key    string // server name; default "srv"
		want   string // empty means valid
	}{
		{"stdio", MCPServerConfig{Command: "srv", Args: []string{"-v"}, Env: map[string]string{"A": "b"}, Cwd: "~/x"}, "", ""},
		{"http", MCPServerConfig{URL: "http://localhost:8080/mcp", Headers: map[string]string{"X-Key": "v"}}, "", ""},
		{"url from env", MCPServerConfig{URL: "${MCP_URL}"}, "", ""},
		{"explicit sse", MCPServerConfig{Type: MCPSSE, URL: "https://h/events"}, "", ""},
		{"bad name", MCPServerConfig{Command: "srv"}, "my server", "server name"},
		{"long name", MCPServerConfig{Command: "srv"}, strings.Repeat("a", 65), "server name"},
		{"neither", MCPServerConfig{}, "", "command or url is required"},
		{"both", MCPServerConfig{Command: "srv", URL: "https://h"}, "", "not both"},
		{"unknown type", MCPServerConfig{Type: "websocket", URL: "https://h"}, "", `type "websocket"`},
		{"stdio with url", MCPServerConfig{Type: MCPStdio, URL: "https://h"}, "", "type stdio needs a command"},
		{"http with command", MCPServerConfig{Type: MCPStreamableHTTP, Command: "srv"}, "", "needs a url"},
		{"bad scheme", MCPServerConfig{URL: "ftp://h/x"}, "", "http(s) URL"},
		{"no host", MCPServerConfig{URL: "http:///x"}, "", "http(s) URL"},
		{"args on http", MCPServerConfig{URL: "https://h", Args: []string{"x"}}, "", "only apply to stdio"},
		{"headers on stdio", MCPServerConfig{Command: "srv", Headers: map[string]string{"A": "b"}}, "", "only apply to HTTP"},
		{"bad env name", MCPServerConfig{Command: "srv", Env: map[string]string{"A=B": "c"}}, "", "invalid variable name"},
		{"bad header name", MCPServerConfig{URL: "https://h", Headers: map[string]string{"Bad Header": "v"}}, "", "invalid header name"},
		{"negative timeout", MCPServerConfig{Command: "srv", ToolTimeout: -1}, "", "toolTimeout"},
		{"empty tool name", MCPServerConfig{Command: "srv", EnabledTools: []string{""}}, "", "enabledTools[0]"},
		{"oauth http", MCPServerConfig{URL: "https://h/mcp", Auth: MCPAuthOAuth}, "", ""},
		{"oauth sse", MCPServerConfig{Type: MCPSSE, URL: "https://h/sse", Auth: MCPAuthOAuth}, "", ""},
		{"oauth with headers", MCPServerConfig{URL: "https://h/mcp", Auth: MCPAuthOAuth, Headers: map[string]string{"X-Team": "a"}}, "", ""},
		{"oauth stdio", MCPServerConfig{Command: "srv", Auth: MCPAuthOAuth}, "", "auth oauth needs a url"},
		{"oauth typed stdio", MCPServerConfig{Type: MCPStdio, Command: "srv", Auth: MCPAuthOAuth}, "", "auth oauth needs a url"},
		{"unknown auth", MCPServerConfig{URL: "https://h/mcp", Auth: "bearer"}, "", `auth "bearer" must be empty or "oauth"`},
		{"disabled is still validated", MCPServerConfig{Enabled: new(false)}, "", "command or url is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := tt.key
			if key == "" {
				key = "srv"
			}
			cfg := Default()
			cfg.Tools.MCPServers = map[string]MCPServerConfig{key: tt.server}
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadMCPAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"tools":{"mcpServers":{"linear":{"url":"https://mcp.linear.app/mcp","auth":"oauth"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Tools.MCPServers["linear"]
	if s.Auth != MCPAuthOAuth || !s.UsesOAuth() {
		t.Errorf("auth = %q, want oauth", s.Auth)
	}
	if got := MCPOAuthPath(path); got != filepath.Join(filepath.Dir(path), "auth", "mcp.json") {
		t.Errorf("MCPOAuthPath = %q", got)
	}
}

func TestMCPExpand(t *testing.T) {
	env := map[string]string{"TOKEN": "s3cret", "HOST": "mcp.example.com", "EMPTY": ""}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	in := MCPServerConfig{
		Command: "srv",
		Args:    []string{"--token=${TOKEN}", "$TOKEN", "${EMPTY}x"},
		Env:     map[string]string{"A": "${TOKEN}-${TOKEN}"},
		URL:     "https://${HOST}/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"},
	}
	got, err := in.Expand(getenv)
	if err != nil {
		t.Fatal(err)
	}
	want := MCPServerConfig{
		Command: "srv",
		Args:    []string{"--token=s3cret", "$TOKEN", "x"}, // only ${VAR} form
		Env:     map[string]string{"A": "s3cret-s3cret"},
		URL:     "https://mcp.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer s3cret"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand = %+v, want %+v", got, want)
	}
	if in.Env["A"] != "${TOKEN}-${TOKEN}" || in.Args[0] != "--token=${TOKEN}" {
		t.Error("Expand modified its receiver")
	}

	_, err = MCPServerConfig{Env: map[string]string{"A": "${NOPE}", "B": "${ALSO_NOPE}${NOPE}"}}.Expand(getenv)
	if err == nil || !strings.Contains(err.Error(), "ALSO_NOPE, NOPE") {
		t.Errorf("Expand with unset vars = %v", err)
	}
}
