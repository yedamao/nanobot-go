package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newExec(t *testing.T, cfg ExecConfig) (*Exec, string) {
	t.Helper()
	ws := newWS(t)
	e, err := NewExec(ws, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e, ws.Root
}

func TestExec(t *testing.T) {
	e, root := newExec(t, ExecConfig{})
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr string
	}{
		{name: "echo", args: map[string]any{"command": "echo hello"}, want: "hello\nExit code: 0"},
		{name: "stderr", args: map[string]any{"command": "echo out; echo err >&2"}, want: "out\nSTDERR:\nerr\nExit code: 0"},
		{name: "exit code", args: map[string]any{"command": "exit 3"}, want: "(no output)\nExit code: 3"},
		{name: "cwd default", args: map[string]any{"command": "pwd"}, want: root + "\nExit code: 0"},
		{name: "working dir", args: map[string]any{"command": "pwd", "working_dir": "sub"}, want: filepath.Join(root, "sub") + "\nExit code: 0"},
		{name: "bad working dir", args: map[string]any{"command": "pwd", "working_dir": "missing"}, wantErr: "not a directory"},
		{name: "working dir escape", args: map[string]any{"command": "pwd", "working_dir": ".."}, wantErr: "outside the workspace"},
		{name: "guard deny", args: map[string]any{"command": "rm -rf sub"}, wantErr: "deny pattern"},
		{name: "guard path", args: map[string]any{"command": "cat /etc/passwd"}, wantErr: "path outside working dir"},
		{name: "timeout", args: map[string]any{"command": "sleep 5", "timeout": 1}, wantErr: "timed out after 1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := run(t, e, tt.args)
			if tt.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Content, tt.wantErr) {
					t.Fatalf("got %+v, want error %q", res, tt.wantErr)
				}
				return
			}
			if res.IsError || res.Content != tt.want {
				t.Errorf("got %+v, want %q", res, tt.want)
			}
		})
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	e, _ := newExec(t, ExecConfig{Timeout: 200 * time.Millisecond})
	start := time.Now()
	// The background child keeps stdout open; without killing the group
	// and WaitDelay this would block for the full sleep.
	res := run(t, e, map[string]any{"command": "sleep 30 & sleep 30"})
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Fatalf("got %+v", res)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v; process group not killed", d)
	}
}

func TestExecCancelled(t *testing.T) {
	e, _ := newExec(t, ExecConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	res := e.Execute(ctx, json.RawMessage(`{"command":"sleep 10"}`))
	if !res.IsError || !strings.Contains(res.Content, "cancelled") {
		t.Errorf("got %+v", res)
	}
}

func TestExecOutputCap(t *testing.T) {
	e, _ := newExec(t, ExecConfig{MaxOutput: 100})
	res := run(t, e, map[string]any{"command": "i=0; while [ $i -lt 200 ]; do echo line$i; i=$((i+1)); done"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	if !strings.HasPrefix(res.Content, "line0\n") || !strings.Contains(res.Content, "bytes omitted") ||
		!strings.Contains(res.Content, "line199\nExit code: 0") {
		t.Errorf("got %q", res.Content)
	}
}

func TestExecAllowPatterns(t *testing.T) {
	e, _ := newExec(t, ExecConfig{AllowPatterns: []string{`echo .*`}})
	if res := run(t, e, map[string]any{"command": "echo ok"}); res.IsError {
		t.Errorf("allowed command failed: %s", res.Content)
	}
	if res := run(t, e, map[string]any{"command": "ls"}); !res.IsError || !strings.Contains(res.Content, "allowlist") {
		t.Errorf("got %+v", res)
	}
	if _, err := NewExec(newWS(t), ExecConfig{DenyPatterns: []string{"("}}); err == nil {
		t.Error("expected error for invalid pattern")
	}
}

func TestHeadTail(t *testing.T) {
	h := newHeadTail(10)
	for _, s := range []string{"abc", "defgh", "ijklmno", "pq"} {
		if n, err := h.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	// 17 bytes in: head "abcde", tail "mnopq", 7 dropped.
	if got, want := h.String(), "abcde\n... (7 bytes omitted) ...\nmnopq"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	small := newHeadTail(100)
	small.Write([]byte("hi"))
	if small.String() != "hi" {
		t.Errorf("got %q", small.String())
	}
}
