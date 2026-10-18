package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// dreamWorkspace returns a resolved workspace with the memory files in
// place and the Dream registry for it.
func dreamWorkspace(t *testing.T) (string, *tools.Registry) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"AGENTS.md":                  "agents\n",
		"SOUL.md":                    "soul\n",
		"memory/history.jsonl":       `{"cursor":1,"timestamp":"t","content":"c"}` + "\n",
		"memory/" + DreamCursorFile:  "0\n",
		"memory/" + LongTermFile:     "memory\n",
		"notes/keep.txt":             "keep\n",
		"skills/existing/SKILL.md":   "skill\n",
		"skills/existing/extra.json": "{}\n",
	} {
		p := filepath.Join(ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := DreamTools(ws)
	if err != nil {
		t.Fatal(err)
	}
	return ws, reg
}

func call(reg *tools.Registry, name string, args map[string]any) tools.Result {
	raw, _ := json.Marshal(args)
	return reg.Execute(context.Background(), name, raw)
}

func TestDreamToolsAreRestricted(t *testing.T) {
	_, reg := dreamWorkspace(t)
	if got := reg.Names(); !slices.Equal(got, []string{"edit_file", "list_dir", "read_file", "write_file"}) {
		t.Errorf("tools = %v", got)
	}
	for _, d := range reg.Definitions() {
		if (d.Name == "write_file" || d.Name == "edit_file") != strings.Contains(d.Description, "During Dream") {
			t.Errorf("%s description = %q", d.Name, d.Description)
		}
	}
	if _, err := DreamTools(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("DreamTools accepted a missing workspace")
	}
}

func TestDreamToolsWriteAllowedFiles(t *testing.T) {
	ws, reg := dreamWorkspace(t)
	for _, path := range []string{
		"SOUL.md",
		"USER.md", // created
		"memory/MEMORY.md",
		"./memory/../memory/MEMORY.md",
		filepath.Join(ws, "SOUL.md"),
		"skills/new-skill/SKILL.md", // parents created
		"skills/existing/extra.json",
	} {
		if res := call(reg, "write_file", map[string]any{"path": path, "content": "new: " + path}); res.IsError {
			t.Errorf("write %s: %s", path, res.Content)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "skills", "new-skill", "SKILL.md")); string(data) != "new: skills/new-skill/SKILL.md" {
		t.Errorf("skill = %q", data)
	}
	if res := call(reg, "edit_file", map[string]any{"path": "USER.md", "old_string": "new: USER.md", "new_string": "Name: Ada"}); res.IsError {
		t.Errorf("edit USER.md: %s", res.Content)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "USER.md")); string(data) != "Name: Ada" {
		t.Errorf("USER.md = %q", data)
	}
	// Reads are not limited to the writable files.
	for _, path := range []string{"AGENTS.md", "memory/history.jsonl", "notes/keep.txt"} {
		if res := call(reg, "read_file", map[string]any{"path": path}); res.IsError {
			t.Errorf("read %s: %s", path, res.Content)
		}
	}
	if res := call(reg, "list_dir", map[string]any{"path": "skills"}); res.IsError || !strings.Contains(res.Content, "new-skill/") {
		t.Errorf("list skills = %+v", res)
	}
}

func TestDreamToolsRefuseOtherTargets(t *testing.T) {
	ws, reg := dreamWorkspace(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	for _, path := range []string{
		"AGENTS.md",
		"memory/history.jsonl",
		"memory/" + DreamCursorFile,
		"memory/.history.lock",
		"memory/OTHER.md",
		"notes/keep.txt",
		"new.md",
		"skills",
		"skills/../memory/history.jsonl",
		"skills/../AGENTS.md",
		"memory/MEMORY.md/../../AGENTS.md",
		"../SOUL.md",
		outside,
		filepath.Join(ws, "..", "SOUL.md"),
		"",
		"soul\x00.md",
	} {
		for _, tool := range []string{"write_file", "edit_file"} {
			args := map[string]any{"path": path, "content": "pwned", "old_string": "a", "new_string": "pwned"}
			if res := call(reg, tool, args); !res.IsError {
				t.Errorf("%s %q succeeded: %s", tool, path, res.Content)
			}
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("file written outside the workspace: %v", err)
	}
	for name, want := range map[string]string{
		"AGENTS.md":            "agents\n",
		"memory/history.jsonl": `{"cursor":1,"timestamp":"t","content":"c"}` + "\n",
		"memory/.dream_cursor": "0\n",
		"notes/keep.txt":       "keep\n",
	} {
		if data, _ := os.ReadFile(filepath.Join(ws, name)); string(data) != want {
			t.Errorf("%s modified: %q", name, data)
		}
	}
	for _, name := range []string{"new.md", "memory/OTHER.md"} {
		if _, err := os.Stat(filepath.Join(ws, name)); !os.IsNotExist(err) {
			t.Errorf("%s created: %v", name, err)
		}
	}
	// Reads stay inside the workspace even though the agent config may not
	// restrict it.
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := call(reg, "read_file", map[string]any{"path": outside}); !res.IsError {
		t.Errorf("read outside the workspace: %s", res.Content)
	}
	// Malformed arguments are rejected before touching anything.
	if res := reg.Execute(context.Background(), "write_file", json.RawMessage(`{"path": 3, "content": "x"}`)); !res.IsError {
		t.Errorf("non-string path accepted: %s", res.Content)
	}
}
