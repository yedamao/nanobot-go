package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/providers"
)

func openSQLite(t *testing.T, path, importDir string) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(path, importDir)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	return openSQLite(t, filepath.Join(t.TempDir(), "ws", SQLiteFileName), "")
}

func TestSQLiteRoundTrip(t *testing.T) {
	store := newSQLite(t)
	s := New("telegram:12345")
	s.Metadata["title"] = "hello"
	failed := toolResult("c2")
	failed.IsError = true
	s.Append(user("hi <b>&</b>"), toolCall("c1", "c2"), toolResult("c1"), failed, assistant("multi\nline"))
	if err := s.CommitSummary("- earlier", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if s.Generation != 1 {
		t.Errorf("Generation after first save = %d, want 1", s.Generation)
	}
	got, err := store.Load(s.Key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Messages, s.Messages) {
		t.Errorf("messages =\n%+v\nwant\n%+v", got.Messages, s.Messages)
	}
	if got.Key != s.Key || !got.CreatedAt.Equal(s.CreatedAt) || !got.UpdatedAt.Equal(s.UpdatedAt) {
		t.Errorf("header = %+v", got)
	}
	if got.Metadata["title"] != "hello" || got.LastConsolidated != 1 || got.Summary != "- earlier" || got.Generation != 1 {
		t.Errorf("loaded metadata %v marker %d summary %q generation %d", got.Metadata, got.LastConsolidated, got.Summary, got.Generation)
	}

	// Saving the loaded copy advances the generation; shrinking the
	// transcript removes the dropped rows.
	got.Clear()
	got.Append(user("fresh"))
	if err := store.Save(got); err != nil {
		t.Fatal(err)
	}
	again, err := store.Load(s.Key)
	if err != nil {
		t.Fatal(err)
	}
	if want := []providers.Message{user("fresh")}; !reflect.DeepEqual(again.Messages, want) || again.Summary != "" || again.Generation != 2 {
		t.Errorf("after clear: %+v", again)
	}
}

func TestSQLiteLoadMissing(t *testing.T) {
	store := newSQLite(t)
	if _, err := store.Load("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load(missing) = %v, want ErrNotFound", err)
	}
}

func TestSQLiteSaveRejectsBadMarker(t *testing.T) {
	store := newSQLite(t)
	s := New("k")
	s.Append(user("one"))
	s.LastConsolidated = 2
	if err := store.Save(s); err == nil {
		t.Fatal("Save with marker past the end succeeded")
	}
	if _, err := store.Load("k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("failed save stored something: %v", err)
	}
}

func TestSQLiteConflict(t *testing.T) {
	store := newSQLite(t)
	s := New("cli:direct")
	s.Append(user("u1"))
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	a, _ := store.Load("cli:direct")
	b, _ := store.Load("cli:direct")

	a.Append(assistant("from a"))
	if err := store.Save(a); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	b.Append(assistant("from b"))
	err := store.Save(b)
	if !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("stale save = %v, want ErrSessionConflict", err)
	}
	if b.Generation != 1 {
		t.Errorf("failed save changed Generation to %d", b.Generation)
	}
	got, _ := store.Load("cli:direct")
	if want := []providers.Message{user("u1"), assistant("from a")}; !reflect.DeepEqual(got.Messages, want) {
		t.Errorf("stored = %+v, want the first writer's", got.Messages)
	}

	// A new, never-saved session must not overwrite a stored one.
	if err := store.Save(New("cli:direct")); !errors.Is(err, ErrSessionConflict) {
		t.Errorf("save of a fresh session over a stored one = %v, want ErrSessionConflict", err)
	}

	// Nor may a session that was deleted meanwhile be resurrected.
	if err := store.Delete("cli:direct"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(got); !errors.Is(err, ErrSessionConflict) {
		t.Errorf("save after delete = %v, want ErrSessionConflict", err)
	}
	// Reloading (a fresh session after the delete) works.
	if err := store.Save(New("cli:direct")); err != nil {
		t.Errorf("save after reload: %v", err)
	}
}

func TestSQLiteListAndDelete(t *testing.T) {
	store := newSQLite(t)
	if infos, err := store.List(); err != nil || len(infos) != 0 {
		t.Fatalf("List(empty) = %v, %v", infos, err)
	}
	older, newer := New("a:1"), New("b:2")
	older.Append(user("x"), assistant("y"))
	older.UpdatedAt = time.Now().Add(-time.Hour) // after Append, which bumps it
	for _, s := range []*Session{older, newer} {
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Key != "b:2" || infos[1].Key != "a:1" || infos[0].Path != store.Path() {
		t.Errorf("List = %+v, want b:2 then a:1", infos)
	}
	if err := store.Delete("a:1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("a:1"); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
	if infos, _ := store.List(); len(infos) != 1 {
		t.Errorf("List after delete = %+v", infos)
	}
	// The cascade removed the messages too.
	var n int
	if err := store.db.QueryRow(`SELECT count(*) FROM messages WHERE session_key = 'a:1'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("messages left after delete: %d, %v", n, err)
	}
}

func TestSQLiteSkipsCorruptRowsAndResetsMarker(t *testing.T) {
	store := newSQLite(t)
	s := New("k")
	s.Append(user("one"), assistant("two"), user("three"))
	if err := s.CommitSummary("sum", 3); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE messages SET record = '{truncated' WHERE position = 1`); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("k")
	if err != nil {
		t.Fatal(err)
	}
	if want := []providers.Message{user("one"), user("three")}; !reflect.DeepEqual(got.Messages, want) {
		t.Errorf("messages = %+v, want %+v", got.Messages, want)
	}
	// The marker now points past the end: replay everything.
	if got.LastConsolidated != 0 || got.Summary != "" {
		t.Errorf("marker %d summary %q, want reset", got.LastConsolidated, got.Summary)
	}
}

func TestSQLiteReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd dir?#%")
	path := filepath.Join(dir, SQLiteFileName)
	store := openSQLite(t, path, "")
	s := New("cli:direct")
	s.Append(user("hi"), assistant("hello"))
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The file is where we asked, not at a mangled URI path.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("database perm = %o, want 600", perm)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("directory: %v, %v", info, err)
	}

	store = openSQLite(t, path, "")
	got, err := store.Load("cli:direct")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Messages, s.Messages) || got.Generation != 1 {
		t.Errorf("reopened: %+v", got)
	}
	var (
		version, busy int
		mode          string
	)
	if err := store.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != len(sqliteMigrations()) {
		t.Errorf("schema version = %d, %v", version, err)
	}
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v", mode, err)
	}
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != int(sqliteBusyTimeout.Milliseconds()) {
		t.Errorf("busy_timeout = %d, %v", busy, err)
	}
}

func TestSQLiteRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), SQLiteFileName)
	store := openSQLite(t, path, "")
	if _, err := store.db.Exec(`UPDATE schema_version SET version = 99`); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := OpenSQLite(path, ""); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("open of a newer schema = %v, want an error", err)
	}
}

func TestSQLiteRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.sqlite3")
	link := filepath.Join(dir, SQLiteFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if _, err := OpenSQLite(link, ""); err == nil {
		t.Error("opened a database through a symlink")
	}
}

func writeJSONL(t *testing.T, dir string, sessions ...*Session) {
	t.Helper()
	src := NewJSONLStore(dir)
	for _, s := range sessions {
		if err := src.Save(s); err != nil {
			t.Fatal(err)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSQLiteImportsJSONLOnce(t *testing.T) {
	legacy := t.TempDir()
	a := New("cli:direct")
	a.Metadata["title"] = "a"
	a.Append(user("u1"), toolCall("c1"), toolResult("c1"), assistant("a1"), user("u2"), assistant("a2"))
	if err := a.CommitSummary("summary of u1", 4); err != nil {
		t.Fatal(err)
	}
	b := New("telegram:42")
	b.Append(user("hello"))
	writeJSONL(t, legacy, a, b)
	// Not session files: left alone.
	for name, content := range map[string]string{".hidden.jsonl": "x", "notes.txt": "x"} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(legacy, "ws", SQLiteFileName)
	store := openSQLite(t, path, legacy)
	for _, want := range []*Session{a, b} {
		got, err := store.Load(want.Key)
		if err != nil {
			t.Fatalf("Load(%q): %v", want.Key, err)
		}
		if !reflect.DeepEqual(got.Messages, want.Messages) || got.Summary != want.Summary ||
			got.LastConsolidated != want.LastConsolidated || !got.UpdatedAt.Equal(want.UpdatedAt) ||
			!reflect.DeepEqual(got.Metadata, want.Metadata) {
			t.Errorf("imported %q = %+v, want %+v", want.Key, got, want)
		}
	}
	wantNames := []string{".hidden.jsonl", "cli_direct.jsonl" + MigratedSuffix, "notes.txt", "telegram_42.jsonl" + MigratedSuffix, "ws"}
	if names := dirNames(t, legacy); !reflect.DeepEqual(names, wantNames) {
		t.Errorf("legacy dir = %v, want %v", names, wantNames)
	}
	// The JSONL store no longer sees the migrated files.
	if infos, _ := NewJSONLStore(legacy).List(); len(infos) != 0 {
		t.Errorf("JSONL store still lists %+v", infos)
	}

	// Later JSONL files are not imported: the import runs once.
	writeJSONL(t, legacy, New("late:1"))
	store.Close()
	store = openSQLite(t, path, legacy)
	if _, err := store.Load("late:1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second open imported again: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "late_1.jsonl")); err != nil {
		t.Errorf("unimported file was touched: %v", err)
	}
}

func TestSQLiteImportFailureLeavesSourcesUnchanged(t *testing.T) {
	legacy := t.TempDir()
	good := New("cli:direct")
	good.Append(user("keep me"))
	writeJSONL(t, legacy, good)
	bad := filepath.Join(legacy, "broken.jsonl")
	if err := os.WriteFile(bad, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := dirNames(t, legacy)

	path := filepath.Join(t.TempDir(), SQLiteFileName)
	_, err := OpenSQLite(path, legacy)
	if err == nil || !strings.Contains(err.Error(), "broken.jsonl") {
		t.Fatalf("open with a corrupt JSONL file = %v, want an error naming it", err)
	}
	if after := dirNames(t, legacy); !reflect.DeepEqual(after, before) {
		t.Errorf("legacy dir changed: %v -> %v", before, after)
	}

	// Once the file is fixed the import runs; nothing half-imported is in
	// the way.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	store := openSQLite(t, path, legacy)
	if got, err := store.Load("cli:direct"); err != nil || len(got.Messages) != 1 {
		t.Errorf("Load after fix = %+v, %v", got, err)
	}
}

func TestSQLiteImportRejectsDuplicateKeys(t *testing.T) {
	legacy := t.TempDir()
	s := New("cli:direct")
	writeJSONL(t, legacy, s)
	data, err := os.ReadFile(filepath.Join(legacy, "cli_direct.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "copy.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(filepath.Join(t.TempDir(), SQLiteFileName), legacy); err == nil || !strings.Contains(err.Error(), "both hold") {
		t.Errorf("open with duplicate keys = %v", err)
	}
}

func TestSQLiteFinishesInterruptedRename(t *testing.T) {
	// A crash after the import committed but before the files were
	// renamed: the next open renames them.
	legacy := t.TempDir()
	path := filepath.Join(t.TempDir(), SQLiteFileName)
	store := openSQLite(t, path, legacy)
	writeJSONL(t, legacy, New("cli:direct"))
	src := filepath.Join(legacy, "cli_direct.jsonl")
	missing := filepath.Join(legacy, "gone.jsonl")
	if _, err := store.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, metaJSONLRename,
		fmt.Sprintf(`[%q, %q]`, src, missing)); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store = openSQLite(t, path, legacy)
	if _, err := os.Stat(src + MigratedSuffix); err != nil {
		t.Errorf("not renamed: %v", err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT count(*) FROM meta WHERE key = ?`, metaJSONLRename).Scan(&n); err != nil || n != 0 {
		t.Errorf("rename list not cleared: %d, %v", n, err)
	}
}

func TestSQLiteRejectsBadMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), SQLiteFileName)
	store := openSQLite(t, path, "")
	if _, err := store.db.Exec(`UPDATE meta SET value = 'half' WHERE key = ?`, metaJSONLImport); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := OpenSQLite(path, ""); err == nil {
		t.Error("open with an invalid import marker succeeded")
	}

	path = filepath.Join(t.TempDir(), SQLiteFileName)
	store = openSQLite(t, path, "")
	if _, err := store.db.Exec(`INSERT INTO meta (key, value) VALUES (?, '["relative.jsonl"]')`, metaJSONLRename); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := OpenSQLite(path, ""); err == nil {
		t.Error("open with a relative rename entry succeeded")
	}
}

// appendWithRetry appends one message to key, reloading and retrying on
// conflict like a caller that lost a race.
func appendWithRetry(store *SQLiteStore, key, text string) (conflicts int, err error) {
	for {
		s, err := store.Load(key)
		if errors.Is(err, ErrNotFound) {
			s, err = New(key), nil
		}
		if err != nil {
			return conflicts, err
		}
		s.Append(user(text))
		err = store.Save(s)
		if !errors.Is(err, ErrSessionConflict) {
			return conflicts, err
		}
		conflicts++
	}
}

func TestSQLiteConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), SQLiteFileName)
	// Two stores on one file stand in for two processes.
	stores := []*SQLiteStore{openSQLite(t, path, ""), openSQLite(t, path, "")}
	const (
		writers = 8
		appends = 15
	)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		conflicts int
		errs      []error
	)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := stores[w%len(stores)]
			for i := range appends {
				// Half the writers share one key; the others have their own.
				key := "shared"
				if w%2 == 1 {
					key = fmt.Sprintf("own:%d", w)
				}
				c, err := appendWithRetry(store, key, fmt.Sprintf("w%d-%d", w, i))
				mu.Lock()
				conflicts += c
				if err != nil {
					errs = append(errs, err)
				}
				mu.Unlock()
			}
			if _, err := store.List(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}

	// No append was lost: every successful save built on the previous one.
	shared, err := stores[0].Load("shared")
	if err != nil {
		t.Fatal(err)
	}
	if want := (writers / 2) * appends; len(shared.Messages) != want || shared.Generation != int64(want) {
		t.Errorf("shared session has %d messages, generation %d; want %d", len(shared.Messages), shared.Generation, want)
	}
	for w := 1; w < writers; w += 2 {
		own, err := stores[1].Load(fmt.Sprintf("own:%d", w))
		if err != nil || len(own.Messages) != appends {
			t.Errorf("own:%d = %v, %v", w, own, err)
		}
	}
	t.Logf("%d conflicts retried", conflicts)
}

func TestSQLiteConcurrentOpenImportsOnce(t *testing.T) {
	legacy := t.TempDir()
	s := New("cli:direct")
	s.Append(user("once"))
	writeJSONL(t, legacy, s)
	path := filepath.Join(t.TempDir(), SQLiteFileName)

	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := OpenSQLite(path, legacy)
			if err == nil {
				err = store.Close()
			}
			results[i] = err
		}()
	}
	wg.Wait()
	for _, err := range results {
		if err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}
	store := openSQLite(t, path, legacy)
	got, err := store.Load("cli:direct")
	if err != nil || len(got.Messages) != 1 || got.Generation != 1 {
		t.Errorf("imported session = %+v, %v", got, err)
	}
}

func TestSQLiteManager(t *testing.T) {
	path := filepath.Join(t.TempDir(), SQLiteFileName)
	m1 := NewManager(openSQLite(t, path, ""))
	m2 := NewManager(openSQLite(t, path, ""))
	s1, _ := m1.GetOrCreate("k")
	s1.Append(user("from 1"))
	if err := m1.Save(s1); err != nil {
		t.Fatal(err)
	}
	s2, _ := m2.GetOrCreate("k")
	s2.Append(user("from 2"))
	if err := m2.Save(s2); err != nil {
		t.Fatal(err)
	}
	// m1's cached copy is now stale.
	s1.Append(user("lost"))
	if err := m1.Save(s1); !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("stale manager save = %v, want ErrSessionConflict", err)
	}
	fresh, err := m1.GetOrCreate("k")
	if err != nil {
		t.Fatal(err)
	}
	if want := []providers.Message{user("from 1"), user("from 2")}; !reflect.DeepEqual(fresh.Messages, want) {
		t.Errorf("reloaded = %+v, want %+v", fresh.Messages, want)
	}
	if err := m1.Close(); err != nil {
		t.Error(err)
	}
}

func TestWorkspaceIDAndSQLitePath(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	id, err := WorkspaceID(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("WorkspaceID = %q, want 32 hex digits", id)
	}
	// Spelling does not matter, and creating the directory does not
	// change the identity.
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ws + "/", filepath.Join(ws, "sub", ".."), ws} {
		if got, _ := WorkspaceID(p); got != id {
			t.Errorf("WorkspaceID(%q) = %q, want %q", p, got, id)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(ws, link); err == nil {
		if got, _ := WorkspaceID(link); got != id {
			t.Errorf("WorkspaceID(symlink) = %q, want %q", got, id)
		}
	}
	if other, _ := WorkspaceID(filepath.Join(root, "other")); other == id {
		t.Error("different workspaces share an id")
	}

	sessions := filepath.Join(root, "sessions")
	path, err := SQLitePath(sessions, ws)
	if err != nil {
		t.Fatal(err)
	}
	canonRoot, _ := filepath.EvalSymlinks(root)
	if want := filepath.Join(canonRoot, "sessions", id, SQLiteFileName); path != want {
		t.Errorf("SQLitePath = %q, want %q", path, want)
	}
	if _, err := SQLitePath(filepath.Join(ws, "sessions"), ws); err == nil {
		t.Error("SQLitePath accepted a location inside the workspace")
	}
	if _, err := SQLitePath(sessions, root); err == nil {
		t.Error("SQLitePath accepted a workspace containing the sessions dir")
	}
}

func TestOpenImportsSessionsDir(t *testing.T) {
	root := t.TempDir()
	sessions, ws := filepath.Join(root, "sessions"), filepath.Join(root, "workspace")
	legacy := New("cli:direct")
	legacy.Append(user("from jsonl"))
	writeJSONL(t, sessions, legacy)

	store, err := Open(sessions, ws)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	want, _ := SQLitePath(sessions, ws)
	if s, ok := store.(*SQLiteStore); !ok || s.Path() != want {
		t.Fatalf("Open = %T, want *SQLiteStore at %s", store, want)
	}
	got, err := store.Load("cli:direct")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Messages, legacy.Messages) {
		t.Errorf("imported messages = %+v, want %+v", got.Messages, legacy.Messages)
	}
	if _, err := os.Stat(filepath.Join(sessions, "cli_direct.jsonl"+MigratedSuffix)); err != nil {
		t.Errorf("JSONL file not marked migrated: %v", err)
	}

	if _, err := Open(filepath.Join(ws, "sessions"), ws); err == nil {
		t.Error("Open accepted a sessions dir inside the workspace")
	}
}
