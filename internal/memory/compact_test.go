package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendN(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := range n {
		if _, err := s.Append(fmt.Sprintf("entry %d", i+1), ""); err != nil {
			t.Fatal(err)
		}
	}
}

func cursors(t *testing.T, s *Store) []int {
	t.Helper()
	entries, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int, len(entries))
	for i, e := range entries {
		out[i] = e.Cursor
	}
	return out
}

func TestDreamCursor(t *testing.T) {
	s := newTestStore(t)
	if c, err := s.DreamCursor(); err != nil || c != 0 {
		t.Fatalf("missing cursor = %d, %v", c, err)
	}
	if err := s.SetDreamCursor(42); err != nil {
		t.Fatal(err)
	}
	if c, err := s.DreamCursor(); err != nil || c != 42 {
		t.Fatalf("cursor = %d, %v", c, err)
	}
	info, err := os.Stat(s.DreamCursorPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("stat = %v, %v", info, err)
	}
	if err := s.SetDreamCursor(-1); err == nil {
		t.Error("negative cursor accepted")
	}
	// Hand-edited whitespace is fine; garbage is an error, not 0.
	for content, want := range map[string]int{" 7 \n": 7, "0": 0} {
		if err := os.WriteFile(s.DreamCursorPath(), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if c, err := s.DreamCursor(); err != nil || c != want {
			t.Errorf("cursor %q = %d, %v", content, c, err)
		}
	}
	for _, bad := range []string{"", "abc", "-3", "12abc", strings.Repeat("9", 100)} {
		if err := os.WriteFile(s.DreamCursorPath(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DreamCursor(); err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Errorf("cursor %q: err = %v", bad, err)
		}
	}
	// A directory in its place is an error too.
	if err := os.Remove(s.DreamCursorPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.DreamCursorPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DreamCursor(); err == nil {
		t.Error("directory cursor accepted")
	}
}

func TestCompactKeepsNewest(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.Compact(3); err != nil || n != 0 {
		t.Fatalf("compact without a memory dir = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Dir(s.HistoryPath())); !os.IsNotExist(err) {
		t.Errorf("Compact created the memory dir: %v", err)
	}
	appendN(t, s, 5)
	if err := s.SetDreamCursor(5); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Compact(0); err != nil || n != 0 || len(cursors(t, s)) != 5 {
		t.Errorf("Compact(0) = %d, %v", n, err)
	}
	if n, err := s.Compact(5); err != nil || n != 0 {
		t.Errorf("Compact at the limit = %d, %v", n, err)
	}
	n, err := s.Compact(3)
	if err != nil || n != 2 {
		t.Fatalf("Compact(3) = %d, %v", n, err)
	}
	if got := fmt.Sprint(cursors(t, s)); got != "[3 4 5]" {
		t.Errorf("cursors = %s", got)
	}
	// New entries continue from the highest cursor, never reusing one.
	appendN(t, s, 1)
	if got := fmt.Sprint(cursors(t, s)); got != "[3 4 5 6]" {
		t.Errorf("cursors after append = %s", got)
	}
	info, err := os.Stat(s.HistoryPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("stat = %v, %v", info, err)
	}
}

func TestCompactNeverDropsUnprocessedEntries(t *testing.T) {
	s := newTestStore(t)
	appendN(t, s, 10)
	// Dream has seen entries up to 4: 5..10 must survive a limit of 3.
	if err := s.SetDreamCursor(4); err != nil {
		t.Fatal(err)
	}
	n, err := s.Compact(3)
	if err != nil || n != 4 {
		t.Fatalf("Compact = %d, %v", n, err)
	}
	if got := fmt.Sprint(cursors(t, s)); got != "[5 6 7 8 9 10]" {
		t.Errorf("cursors = %s", got)
	}
	// Dream never ran: nothing is dropped.
	s = newTestStore(t)
	appendN(t, s, 4)
	if n, err := s.Compact(1); err != nil || n != 0 || len(cursors(t, s)) != 4 {
		t.Errorf("Compact before any Dream run = %d, %v", n, err)
	}
	// A corrupt cursor stops compaction instead of guessing.
	if err := os.WriteFile(s.DreamCursorPath(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(1); err == nil {
		t.Error("Compact with a corrupt cursor succeeded")
	}
	if len(cursors(t, s)) != 4 {
		t.Error("entries dropped with a corrupt cursor")
	}
}

func TestCompactDropsCorruptLines(t *testing.T) {
	s := newTestStore(t)
	appendN(t, s, 3)
	f, err := os.OpenFile(s.HistoryPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not json\n{\"cursor\":0}\n")
	f.Close()
	appendN(t, s, 1)
	if err := s.SetDreamCursor(4); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Compact(2); err != nil || n != 2 {
		t.Fatalf("Compact = %d, %v", n, err)
	}
	data, _ := os.ReadFile(s.HistoryPath())
	if strings.Contains(string(data), "not json") || strings.Count(string(data), "\n") != 2 {
		t.Errorf("file:\n%s", data)
	}
}
