//go:build unix

package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// Symlinks at or above an allowed name are resolved before the check, so
// they cannot redirect a Dream write to a protected file or out of the
// workspace.
func TestDreamToolsRefuseSymlinkEscapes(t *testing.T) {
	ws, reg := dreamWorkspace(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "target.md")
	if err := os.WriteFile(outsideFile, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(ws, "memory", "history.jsonl")
	before, _ := os.ReadFile(history)

	// SOUL.md -> memory/history.jsonl, USER.md -> a file outside.
	if err := os.Remove(filepath.Join(ws, "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"SOUL.md":          history,
		"USER.md":          outsideFile,
		"memory/MEMORY.md": filepath.Join("..", "AGENTS.md"), // relative link
	}
	if err := os.Remove(filepath.Join(ws, "memory", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	// skills -> memory, so skills/history.jsonl is the real history file.
	if err := os.RemoveAll(filepath.Join(ws, "skills")); err != nil {
		t.Fatal(err)
	}
	links["skills"] = filepath.Join(ws, "memory")
	if err := os.MkdirAll(filepath.Join(ws, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"SOUL.md", "USER.md", "memory/MEMORY.md", "skills/history.jsonl", "skills/new/SKILL.md"} {
		for _, tool := range []string{"write_file", "edit_file"} {
			args := map[string]any{"path": path, "content": "pwned", "old_string": "o", "new_string": "pwned"}
			if res := call(reg, tool, args); !res.IsError {
				t.Errorf("%s %s through a symlink succeeded: %s", tool, path, res.Content)
			}
		}
	}
	if after, _ := os.ReadFile(history); string(after) != string(before) {
		t.Errorf("history.jsonl modified: %q", after)
	}
	if data, _ := os.ReadFile(outsideFile); string(data) != "outside\n" {
		t.Errorf("outside file modified: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "AGENTS.md")); string(data) != "agents\n" {
		t.Errorf("AGENTS.md modified: %q", data)
	}
	if _, err := os.Stat(filepath.Join(ws, "memory", "new")); !os.IsNotExist(err) {
		t.Errorf("skill created through the skills symlink: %v", err)
	}

	// skills -> a directory outside the workspace.
	if err := os.Remove(filepath.Join(ws, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(ws, "skills")); err != nil {
		t.Fatal(err)
	}
	if res := call(reg, "write_file", map[string]any{"path": "skills/x/SKILL.md", "content": "pwned"}); !res.IsError {
		t.Errorf("write through an outside skills symlink succeeded: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "x")); !os.IsNotExist(err) {
		t.Errorf("file created outside the workspace: %v", err)
	}
}
