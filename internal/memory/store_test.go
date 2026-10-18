package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	s.now = func() time.Time { return time.Date(2026, 10, 18, 9, 30, 59, 0, time.UTC) }
	return s
}

func TestAppendAndEntries(t *testing.T) {
	s := newTestStore(t)
	if got, err := s.Entries(); err != nil || got != nil {
		t.Fatalf("Entries on missing file = %v, %v", got, err)
	}
	e1, err := s.Append("first <b>&</b>\n\n", "cli:direct")
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.Append("second", "")
	if err != nil {
		t.Fatal(err)
	}
	want1 := Entry{Cursor: 1, Timestamp: "2026-10-18 09:30", Content: "first <b>&</b>", SessionKey: "cli:direct"}
	if e1 != want1 || e2.Cursor != 2 || e2.SessionKey != "" {
		t.Errorf("entries = %+v, %+v", e1, e2)
	}
	got, err := s.Entries()
	if err != nil || len(got) != 2 || got[0] != want1 || got[1] != e2 {
		t.Errorf("Entries = %+v, %v", got, err)
	}

	data, _ := os.ReadFile(s.HistoryPath())
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"content":"first <b>&</b>"`) || strings.Contains(lines[1], "session_key") {
		t.Errorf("file:\n%s", data)
	}
	info, err := os.Stat(s.HistoryPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("stat = %v, %v", info, err)
	}
	if filepath.Base(filepath.Dir(s.HistoryPath())) != Dir || filepath.Base(s.HistoryPath()) != HistoryFile {
		t.Errorf("HistoryPath = %s", s.HistoryPath())
	}
}

func TestAppendSurvivesCorruptAndTornLines(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(s.HistoryPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	// Out-of-order cursors, garbage, a non-positive cursor, and a torn
	// last line from a crash mid-append.
	content := `{"cursor":7,"timestamp":"t","content":"a"}` + "\n" +
		"garbage\n" +
		`{"cursor":-3,"timestamp":"t","content":"bad"}` + "\n" +
		`{"cursor":4,"timestamp":"t","content":"b"}` + "\n" +
		`{"cursor":99,"timestamp":"t","con`
	if err := os.WriteFile(s.HistoryPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := s.Append("new", "k")
	if err != nil {
		t.Fatal(err)
	}
	if e.Cursor != 8 {
		t.Errorf("cursor = %d, want 8 (max valid + 1)", e.Cursor)
	}
	got, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	var cursors []int
	for _, e := range got {
		cursors = append(cursors, e.Cursor)
	}
	if fmt.Sprint(cursors) != "[7 4 8]" {
		t.Errorf("cursors = %v, want [7 4 8]", cursors)
	}
	data, _ := os.ReadFile(s.HistoryPath())
	if !strings.HasSuffix(string(data), "\"con\n"+`{"cursor":8,"timestamp":"2026-10-18 09:30","content":"new","session_key":"k"}`+"\n") {
		t.Errorf("new entry not on its own line:\n%s", data)
	}
}

func TestAppendCapsHugeEntries(t *testing.T) {
	s := newTestStore(t)
	e, err := s.Append(strings.Repeat("x", maxEntryChars+500), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.Content, strings.Repeat("x", maxEntryChars)) || !strings.Contains(e.Content, "truncated, 500 more") {
		t.Errorf("content not capped: %d chars", len(e.Content))
	}
}

func TestAppendConcurrentCursorsAreUnique(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if _, err := s.Append(fmt.Sprint(i), ""); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := s.Entries()
	if err != nil || len(got) != 20 {
		t.Fatalf("Entries = %d, %v", len(got), err)
	}
	seen := map[int]bool{}
	for _, e := range got {
		if seen[e.Cursor] || e.Cursor < 1 || e.Cursor > 20 {
			t.Errorf("bad or duplicate cursor %d", e.Cursor)
		}
		seen[e.Cursor] = true
	}
}

func TestAppendErrors(t *testing.T) {
	// The memory directory cannot be created over a file.
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, Dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(ws).Append("x", ""); err == nil {
		t.Error("Append succeeded without a memory directory")
	}

	// history.jsonl is a directory.
	s := newTestStore(t)
	if err := os.MkdirAll(s.HistoryPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("x", ""); err == nil {
		t.Error("Append into a directory succeeded")
	}
	if _, err := s.Entries(); err == nil {
		t.Error("Entries of a directory succeeded")
	}
}

func TestEntryJSONShape(t *testing.T) {
	// nanobot's Dream reads these keys.
	data, err := json.Marshal(Entry{Cursor: 1, Timestamp: "t", Content: "c"})
	if err != nil || string(data) != `{"cursor":1,"timestamp":"t","content":"c"}` {
		t.Errorf("json = %s, %v", data, err)
	}
}
