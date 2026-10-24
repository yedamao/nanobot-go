package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/security/sandbox"
	"github.com/yedamao/nanobot-go/internal/tools"
)

func TestExecSandboxFailsClosedAtConstruction(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/bwrap", nil }
	missing := func(string) (string, error) { return "", errors.New("executable file not found") }
	probeFails := func(context.Context, []string) error { return errors.New("no user namespaces") }
	otherOS := sandbox.Bwrap
	if runtime.GOOS == "linux" {
		otherOS = sandbox.Seatbelt
	}

	tests := []struct {
		name    string
		sandbox string
		host    sandbox.Host
		want    string
	}{
		{"unknown backend", "jail", sandbox.LocalHost(), `unknown sandbox backend "jail"`},
		{"wrong os on this host", otherOS, sandbox.LocalHost(), "refusing to run commands unsandboxed"},
		{"wrong os", sandbox.Bwrap, sandbox.Host{GOOS: "windows", LookPath: found}, "requires linux, this host runs windows"},
		{"missing binary", sandbox.Bwrap, sandbox.Host{GOOS: "linux", LookPath: missing}, "bwrap not found"},
		{"unusable binary", sandbox.Bwrap, sandbox.Host{GOOS: "linux", LookPath: found, Run: probeFails}, "no user namespaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildExec(newWS(t), ExecConfig{Sandbox: tt.sandbox}, tt.host)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}

	// RegisterAll refuses to start without the exec tool rather than
	// registering an unsandboxed one.
	reg := tools.NewRegistry()
	err := RegisterAll(reg, Config{Workspace: newWS(t), Exec: ExecConfig{Sandbox: otherOS}})
	if err == nil || !strings.Contains(err.Error(), "refusing to run commands unsandboxed") {
		t.Errorf("RegisterAll err = %v", err)
	}
}

func TestExecSandboxNeverRunsUnsandboxed(t *testing.T) {
	ws := newWS(t)
	// The host check passes, but the sandbox binary cannot be started.
	host := sandbox.Host{GOOS: "linux", LookPath: func(string) (string, error) {
		return filepath.Join(t.TempDir(), "no-such-bwrap"), nil
	}}
	e, err := buildExec(ws, ExecConfig{Sandbox: sandbox.Bwrap}, host)
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, e, map[string]any{"command": "touch marker"})
	if !res.IsError {
		t.Errorf("got %+v, want an error", res)
	}
	if _, err := os.Stat(filepath.Join(ws.Root, "marker")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command ran outside the sandbox (stat err = %v)", err)
	}
	if !strings.Contains(e.Description(), "OS sandbox") {
		t.Errorf("description does not mention the sandbox: %q", e.Description())
	}
}

func TestExecSandboxBindsPassShellGuard(t *testing.T) {
	ws := newWS(t)
	toolDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := sandbox.Host{GOOS: "linux", LookPath: func(string) (string, error) { return "/usr/bin/bwrap", nil }}
	e, err := buildExec(ws, ExecConfig{Sandbox: sandbox.Bwrap, SandboxROBinds: []string{toolDir, filepath.Dir(ws.Root)}}, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.guard.Check(filepath.Join(toolDir, "bin", "tool"), ws.Root); err != nil {
		t.Errorf("bind root blocked by the guard: %v", err)
	}
	// The workspace's parent was dropped from the binds, so it stays blocked.
	if err := e.guard.Check("ls "+filepath.Dir(ws.Root), ws.Root); err == nil {
		t.Error("workspace parent passed the guard")
	}
}

// TestExecSeatbelt runs commands under the real macOS sandbox.
func TestExecSeatbelt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// <root> plays ~/.nanobot-go: secret.txt sits next to the workspace.
	wsDir := filepath.Join(root, "workspace")
	sibling := filepath.Join(root, "sessions")
	for _, d := range []string{filepath.Join(wsDir, "sub"), sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(root, "secret.txt")
	siblingSecret := filepath.Join(sibling, "key.txt")
	for p, data := range map[string]string{
		secret:                               "TOP-SECRET",
		siblingSecret:                        "SIBLING-SECRET",
		filepath.Join(wsDir, "inside.txt"):   "hello from inside",
		filepath.Join(wsDir, "sub", "n.txt"): "nested",
	} {
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Unrestricted, so the shell guard lets absolute paths through and
	// only the sandbox stands in the way.
	ws, err := security.NewWorkspace(wsDir, false)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewExec(ws, ExecConfig{Sandbox: sandbox.Seatbelt})
	if err != nil {
		if strings.Contains(err.Error(), "cannot start a sandboxed process") {
			t.Skipf("sandbox-exec cannot run here (nested sandbox?): %v", err)
		}
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    map[string]any
		want    string // substring of the output
		notWant string
	}{
		{name: "read inside", args: map[string]any{"command": "cat inside.txt"}, want: "hello from inside\nExit code: 0"},
		{name: "read inside abs", args: map[string]any{"command": "cat " + filepath.Join(wsDir, "inside.txt")}, want: "hello from inside"},
		{name: "working dir", args: map[string]any{"command": "pwd; cat n.txt", "working_dir": "sub"}, want: filepath.Join(wsDir, "sub") + "\nnested"},
		{name: "read parent secret", args: map[string]any{"command": "cat " + secret}, want: "Operation not permitted", notWant: "TOP-SECRET"},
		{name: "read via dotdot", args: map[string]any{"command": "cat ../secret.txt"}, want: "Operation not permitted", notWant: "TOP-SECRET"},
		{name: "read sibling", args: map[string]any{"command": "cat " + siblingSecret}, want: "Operation not permitted", notWant: "SIBLING-SECRET"},
		{name: "list parent", args: map[string]any{"command": "ls " + root}, want: "Operation not permitted", notWant: "secret.txt"},
		{name: "write parent", args: map[string]any{"command": "echo x > " + filepath.Join(root, "new.txt")}, want: "Operation not permitted"},
		{name: "write inside", args: map[string]any{"command": "echo made > made.txt && cat made.txt"}, want: "made\nExit code: 0"},
		{name: "home is workspace", args: map[string]any{"command": "echo $HOME; echo $TMPDIR"}, want: wsDir + "\n" + wsDir + "\nExit code: 0"},
		{name: "system tools work", args: map[string]any{"command": "ls / >/dev/null && /usr/bin/true && echo ok"}, want: "ok\nExit code: 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := run(t, e, tt.args)
			if res.IsError || !strings.Contains(res.Content, tt.want) {
				t.Errorf("got %+v, want output containing %q", res, tt.want)
			}
			if tt.notWant != "" && strings.Contains(res.Content, tt.notWant) {
				t.Errorf("sandbox leaked %q: %s", tt.notWant, res.Content)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write outside the workspace succeeded (stat err = %v)", err)
	}
	// Timeouts still kill the sandboxed process.
	if res := run(t, e, map[string]any{"command": "sleep 5", "timeout": 1}); !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Errorf("timeout: got %+v", res)
	}
}
