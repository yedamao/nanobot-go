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
