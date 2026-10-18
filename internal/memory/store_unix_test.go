//go:build unix

package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendRefusesSymlink(t *testing.T) {
	// The agent could plant a symlink at history.jsonl; appends must not
	// follow it out of the workspace.
	s := newTestStore(t)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.HistoryPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.HistoryPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("x", ""); err == nil {
		t.Error("Append followed a symlink")
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep\n" {
		t.Errorf("symlink target modified: %q", data)
	}
}

func TestDreamCursorRefusesSymlink(t *testing.T) {
	s := newTestStore(t)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("99"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.DreamCursorPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.DreamCursorPath()); err != nil {
		t.Fatal(err)
	}
	if c, err := s.DreamCursor(); err == nil {
		t.Errorf("DreamCursor followed a symlink: %d", c)
	}
	// Setting it replaces the link instead of writing through it.
	if err := s.SetDreamCursor(3); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "99" {
		t.Errorf("symlink target modified: %q", data)
	}
	if c, err := s.DreamCursor(); err != nil || c != 3 {
		t.Errorf("cursor = %d, %v", c, err)
	}
}
