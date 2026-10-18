package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

func newWS(t *testing.T) *security.Workspace {
	t.Helper()
	ws, err := security.NewWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func writeFile(t *testing.T, ws *security.Workspace, rel, content string) string {
	t.Helper()
	p := filepath.Join(ws.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, tool tools.Tool, args any) tools.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw)
}

func TestReadFile(t *testing.T) {
	ws := newWS(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	writeFile(t, ws, "ten.txt", strings.Join(lines, "\n")+"\n")
	writeFile(t, ws, "crlf.txt", "a\r\nb\r\n")
	writeFile(t, ws, "empty.txt", "")
	writeFile(t, ws, "bin.dat", "abc\x00def")
	writeFile(t, ws, "long.txt", strings.Repeat("x", maxLineChars+10))
	tool := NewReadFile(ws)

	tests := []struct {
		name    string
		args    map[string]any
		want    string // exact unless contains is set
		has     []string
		wantErr string
	}{
		{name: "whole file", args: map[string]any{"path": "crlf.txt"}, want: "1| a\n2| b"},
		{name: "offset limit", args: map[string]any{"path": "ten.txt", "offset": 3, "limit": 2},
			want: "3| line 3\n4| line 4\n\n(showing lines 3-4 of 10; use offset=5 to continue)"},
		{name: "tail", args: map[string]any{"path": "ten.txt", "offset": 9},
			want: "9| line 9\n10| line 10\n\n(showing lines 9-10 of 10)"},
		{name: "empty", args: map[string]any{"path": "empty.txt"}, want: "(empty file)"},
		{name: "long line", args: map[string]any{"path": "long.txt"}, has: []string{"(line truncated)"}},
		{name: "past end", args: map[string]any{"path": "ten.txt", "offset": 50}, wantErr: "past the end"},
		{name: "binary", args: map[string]any{"path": "bin.dat"}, wantErr: "binary"},
		{name: "missing", args: map[string]any{"path": "nope.txt"}, wantErr: "no such file"},
		{name: "directory", args: map[string]any{"path": "."}, wantErr: "is a directory"},
		{name: "escape", args: map[string]any{"path": "../x"}, wantErr: "outside the workspace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := run(t, tool, tt.args)
			if tt.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Content, tt.wantErr) {
					t.Fatalf("got %+v, want error with %q", res, tt.wantErr)
				}
				return
			}
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			if tt.want != "" && res.Content != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", res.Content, tt.want)
			}
			for _, h := range tt.has {
				if !strings.Contains(res.Content, h) {
					t.Errorf("missing %q in %q", h, res.Content)
				}
			}
		})
	}
}

func TestWriteFile(t *testing.T) {
	ws := newWS(t)
	tool := NewWriteFile(ws)

	res := run(t, tool, map[string]any{"path": "a/b/c.txt", "content": "hello"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	if !strings.Contains(res.Content, "Wrote 5 bytes to "+filepath.Join("a", "b", "c.txt")) {
		t.Errorf("content = %q", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(ws.Root, "a/b/c.txt"))
	if string(got) != "hello" {
		t.Errorf("file = %q", got)
	}

	// Overwrite preserves mode and leaves no temp files behind.
	p := writeFile(t, ws, "script.sh", "old")
	if err := os.Chmod(p, 0o750); err != nil {
		t.Fatal(err)
	}
	if res := run(t, tool, map[string]any{"path": "script.sh", "content": "new"}); res.IsError {
		t.Fatal(res.Content)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, want 0750", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(ws.Root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	for _, bad := range []map[string]any{
		{"path": "../escape.txt", "content": "x"},
		{"path": ".", "content": "x"},
	} {
		if res := run(t, tool, bad); !res.IsError {
			t.Errorf("write %v succeeded, want error", bad)
		}
	}
}

func TestEditFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		args    map[string]any
		want    string // resulting file content
		wantMsg string
		wantErr string
	}{
		{
			name:    "single exact",
			content: "foo\nbar\nbaz\n",
			args:    map[string]any{"old_string": "bar", "new_string": "BAR"},
			want:    "foo\nBAR\nbaz\n",
			wantMsg: "replaced 1 occurrence",
		},
		{
			name:    "ambiguous",
			content: "x = 1\nx = 1\n",
			args:    map[string]any{"old_string": "x = 1", "new_string": "x = 2"},
			wantErr: "matches 2 locations in f.txt (lines 1, 2)",
		},
		{
			name:    "replace all",
			content: "x = 1\ny\nx = 1\n",
			args:    map[string]any{"old_string": "x = 1", "new_string": "x = 2", "replace_all": true},
			want:    "x = 2\ny\nx = 2\n",
			wantMsg: "replaced 2 occurrence",
		},
		{
			name:    "not found",
			content: "abc\n",
			args:    map[string]any{"old_string": "xyz", "new_string": "q"},
			wantErr: "not found",
		},
		{
			name:    "whitespace fallback",
			content: "func f() {\n\t\treturn 1\n}\n",
			args:    map[string]any{"old_string": "  return 1  ", "new_string": "\t\treturn 2"},
			want:    "func f() {\n\t\treturn 2\n}\n",
			wantMsg: "ignoring leading/trailing whitespace",
		},
		{
			name:    "multiline fallback keeps trailing newline",
			content: "a\n    b\n    c\nd\n",
			args:    map[string]any{"old_string": "b\nc\n", "new_string": "    B\n"},
			want:    "a\n    B\nd\n",
		},
		{
			name:    "crlf fallback",
			content: "one\r\ntwo\r\nthree\r\n",
			args:    map[string]any{"old_string": "one\ntwo", "new_string": "1\n2"},
			want:    "1\r\n2\r\nthree\r\n",
		},
		{
			name:    "fallback ambiguous",
			content: "  x\n    y\nb\n x\n y \n",
			args:    map[string]any{"old_string": "x\ny", "new_string": "c"},
			wantErr: "matches 2 locations in f.txt (lines 1, 4)",
		},
		{
			name:    "empty old",
			content: "abc",
			args:    map[string]any{"old_string": "", "new_string": "x"},
			wantErr: "must not be empty",
		},
		{
			name:    "identical",
			content: "abc",
			args:    map[string]any{"old_string": "a", "new_string": "a"},
			wantErr: "identical",
		},
		{
			name:    "whitespace-only needle",
			content: "a\n\nb\n",
			args:    map[string]any{"old_string": "   \n", "new_string": "x"},
			wantErr: "not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := newWS(t)
			p := writeFile(t, ws, "f.txt", tt.content)
			tt.args["path"] = "f.txt"
			res := run(t, NewEditFile(ws), tt.args)
			got, _ := os.ReadFile(p)
			if tt.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Content, tt.wantErr) {
					t.Fatalf("got %+v, want error %q", res, tt.wantErr)
				}
				if string(got) != tt.content {
					t.Errorf("file modified on error: %q", got)
				}
				return
			}
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			if string(got) != tt.want {
				t.Errorf("file = %q, want %q", got, tt.want)
			}
			if !strings.Contains(res.Content, tt.wantMsg) {
				t.Errorf("message %q missing %q", res.Content, tt.wantMsg)
			}
		})
	}
}

func TestEditFileMissingAndEscape(t *testing.T) {
	ws := newWS(t)
	tool := NewEditFile(ws)
	if res := run(t, tool, map[string]any{"path": "nope.txt", "old_string": "a", "new_string": "b"}); !res.IsError {
		t.Error("expected error for missing file")
	}
	if res := run(t, tool, map[string]any{"path": "/etc/hosts", "old_string": "a", "new_string": "b"}); !res.IsError ||
		!strings.Contains(res.Content, "outside the workspace") {
		t.Errorf("got %+v", res)
	}
}

func TestListDir(t *testing.T) {
	ws := newWS(t)
	writeFile(t, ws, "b.txt", "")
	writeFile(t, ws, "sub/a.txt", "")
	writeFile(t, ws, ".hidden", "")
	if err := os.Mkdir(filepath.Join(ws.Root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws.Root, "sub"), filepath.Join(ws.Root, "link")); err != nil {
		t.Fatal(err)
	}
	tool := NewListDir(ws)

	res := run(t, tool, map[string]any{})
	if want := ".hidden\nb.txt\nemptydir/\nlink/\nsub/"; res.Content != want {
		t.Errorf("got %q, want %q", res.Content, want)
	}
	if res := run(t, tool, map[string]any{"path": "emptydir"}); res.Content != "(empty directory)" {
		t.Errorf("got %q", res.Content)
	}
	if res := run(t, tool, map[string]any{"path": "sub"}); res.Content != "a.txt" {
		t.Errorf("got %q", res.Content)
	}
	if res := run(t, tool, map[string]any{"path": ".."}); !res.IsError {
		t.Error("expected escape error")
	}
	if res := run(t, tool, map[string]any{"path": "b.txt"}); !res.IsError {
		t.Error("expected error listing a file")
	}
}

func TestListDirCapsEntries(t *testing.T) {
	ws := newWS(t)
	for i := range maxDirEntries + 5 {
		writeFile(t, ws, fmt.Sprintf("d/f%04d", i), "")
	}
	res := run(t, NewListDir(ws), map[string]any{"path": "d"})
	if !strings.HasSuffix(res.Content, "... (5 more entries)") {
		t.Errorf("tail = %q", res.Content[len(res.Content)-40:])
	}
}
