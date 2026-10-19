package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// envTestServer makes the test binary act as an MCP stdio server.
const envTestServer = "NANOBOT_MCP_TEST_SERVER"

func TestMain(m *testing.M) {
	switch os.Getenv(envTestServer) {
	case "":
		os.Exit(m.Run())
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: cannot start")
		os.Exit(3)
	default:
		runStdioServer()
		os.Exit(0)
	}
}

// runStdioServer serves pid and getenv over stdin/stdout.
func runStdioServer() {
	srv := sdk.NewServer(&sdk.Implementation{Name: "stdio-test", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{Name: "pid", InputSchema: objectSchema},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return textResult(strconv.Itoa(os.Getpid())), nil
		})
	srv.AddTool(&sdk.Tool{
		Name:        "getenv",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ Name string }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return textResult(os.Getenv(args.Name) + "|" + mustGetwd()), nil
	})
	fmt.Fprintln(os.Stderr, "stdio test server ready")
	_ = srv.Run(context.Background(), &sdk.StdioTransport{})
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

func TestStdioServer(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	t.Setenv("MCP_TEST_GREETING", "hello")
	t.Setenv("NANOBOT_GO_ANTHROPIC_API_KEY", "sk-must-not-leak")
	cfgs := map[string]config.MCPServerConfig{
		"local": {
			Command: exe,
			Env:     map[string]string{envTestServer: "1", "GREETING": "${MCP_TEST_GREETING}"},
			Cwd:     dir,
		},
		"crashy":  {Command: exe, Env: map[string]string{envTestServer: "fail"}},
		"missing": {Command: dir + "/no-such-binary"},
	}
	reg := tools.NewRegistry()
	stderr := &syncBuffer{}
	mgr := NewManager(cfgs, reg, nil, WithConnectTimeout(20*time.Second), WithStderr(stderr))
	defer mgr.Close()
	if err := mgr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, st := range mgr.Status() {
		switch st.Name {
		case "local":
			if st.State != StateConnected || len(st.Tools) != 2 {
				t.Fatalf("local: %+v", st)
			}
		case "crashy":
			if st.State != StateFailed || st.Err == nil || !strings.Contains(st.Err.Error(), "boom: cannot start") {
				t.Errorf("crashy: %+v (want its stderr in the error)", st)
			}
		case "missing":
			if st.State != StateFailed || st.Err == nil {
				t.Errorf("missing: %+v", st)
			}
		}
	}
	if !strings.Contains(stderr.String(), "stdio test server ready") {
		t.Errorf("server stderr not forwarded: %q", stderr)
	}

	exec := func(name, args string) string {
		t.Helper()
		r := reg.Execute(context.Background(), name, json.RawMessage(args))
		if r.IsError {
			t.Fatalf("%s: %s", name, r.Content)
		}
		return r.Content
	}
	// ${VAR} was expanded, cwd applied, and unlisted variables withheld.
	if got := exec("mcp_local_getenv", `{"name": "GREETING"}`); got != "hello|"+dir && !sameDir(got, "hello|", dir) {
		t.Errorf("GREETING|cwd = %q, want hello|%s", got, dir)
	}
	if got := exec("mcp_local_getenv", `{"name": "NANOBOT_GO_ANTHROPIC_API_KEY"}`); strings.HasPrefix(got, "sk-") {
		t.Error("an API key leaked into the MCP server's environment")
	}

	// Kill the server: the next call reconnects to a new process.
	pid1, _ := strconv.Atoi(exec("mcp_local_pid", `{}`))
	proc, err := os.FindProcess(pid1)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	pid2, _ := strconv.Atoi(exec("mcp_local_pid", `{}`))
	if pid2 == 0 || pid2 == pid1 {
		t.Errorf("pid after reconnect = %d (was %d)", pid2, pid1)
	}

	// Close terminates the server process.
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" { // signal 0 probes a process on Unix only
		p, _ := os.FindProcess(pid2)
		if err := p.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("server process %d still exists after Close (signal 0: %v)", pid2, err)
		}
	}
}

// sameDir handles /tmp vs /private/tmp style symlinks.
func sameDir(got, prefix, dir string) bool {
	a, err1 := os.Stat(strings.TrimPrefix(got, prefix))
	b, err2 := os.Stat(dir)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}
