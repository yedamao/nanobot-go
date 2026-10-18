package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/providers"
)

func user(s string) providers.Message { return providers.Message{Role: providers.RoleUser, Content: s} }
func assistant(s string) providers.Message {
	return providers.Message{Role: providers.RoleAssistant, Content: s}
}
func toolCall(ids ...string) providers.Message {
	m := providers.Message{Role: providers.RoleAssistant}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, providers.ToolCall{ID: id, Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)})
	}
	return m
}
func toolResult(id string) providers.Message {
	return providers.Message{Role: providers.RoleTool, ToolCallID: id, Name: "read_file", Content: "result " + id}
}

func TestHistory(t *testing.T) {
	turn1 := []providers.Message{user("u1"), toolCall("c1"), toolResult("c1"), assistant("a1")}
	turn2 := []providers.Message{user("u2"), toolCall("c2", "c3"), toolResult("c2"), toolResult("c3"), assistant("a2")}
	all := append(append([]providers.Message{}, turn1...), turn2...)

	tests := []struct {
		name string
		msgs []providers.Message
		max  int
		want []providers.Message
	}{
		{"all", all, 0, all},
		{"window larger than history", all, 100, all},
		{"window cuts mid-turn: skip to next user", all, 7, turn2},
		{"window starting at orphan tool results", all, 3, []providers.Message{assistant("a2")}},
		{
			"no user in window: orphans dropped",
			[]providers.Message{toolCall("c1"), toolResult("c1"), toolResult("c2"), toolCall("c3"), toolResult("c3"), assistant("a")},
			0,
			[]providers.Message{toolCall("c3"), toolResult("c3"), assistant("a")},
		},
		{
			"orphan tool result after user is dropped with prefix",
			[]providers.Message{user("u"), toolResult("gone"), assistant("a"), user("u2")},
			0,
			[]providers.Message{assistant("a"), user("u2")},
		},
		{"empty", nil, 10, []providers.Message{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New("k")
			s.Messages = tt.msgs
			got := s.History(tt.max)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("History(%d) =\n%+v\nwant\n%+v", tt.max, got, tt.want)
			}
		})
	}
}

func TestHistoryReturnsCopy(t *testing.T) {
	s := New("k")
	s.Append(user("u"), assistant("a"))
	h := s.History(0)
	h[0].Content = "changed"
	if s.Messages[0].Content != "u" {
		t.Error("History aliases session messages")
	}
}

func TestHistoryNeverStartsWithToolResult(t *testing.T) {
	// Exhaustive over window sizes: the first message must never be a
	// tool result, and every tool result must follow its call.
	s := New("k")
	for i := range 5 {
		id := string(rune('a' + i))
		s.Append(user("u"+id), toolCall(id+"1", id+"2"), toolResult(id+"1"), toolResult(id+"2"), assistant("done"))
	}
	for n := 1; n <= len(s.Messages); n++ {
		h := s.History(n)
		seen := map[string]bool{}
		for i, m := range h {
			for _, c := range m.ToolCalls {
				seen[c.ID] = true
			}
			if m.Role == providers.RoleTool && !seen[m.ToolCallID] {
				t.Fatalf("History(%d)[%d] is an orphan tool result %q", n, i, m.ToolCallID)
			}
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	s := New("telegram:12345")
	s.Metadata["title"] = "hello"
	failed := toolResult("c2")
	failed.IsError = true
	s.Append(user("hi <b>&</b>"), toolCall("c1", "c2"), toolResult("c1"), failed, assistant("multi\nline"))
	if err := store.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load("telegram:12345")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Messages, s.Messages) {
		t.Errorf("messages =\n%+v\nwant\n%+v", got.Messages, s.Messages)
	}
	if got.Key != s.Key || !got.CreatedAt.Equal(s.CreatedAt) || !got.UpdatedAt.Equal(s.UpdatedAt) {
		t.Errorf("metadata = %+v", got)
	}
	if got.Metadata["title"] != "hello" {
		t.Errorf("Metadata = %v", got.Metadata)
	}

	info, err := os.Stat(store.Path(s.Key))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	data, _ := os.ReadFile(store.Path(s.Key))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1+len(s.Messages) || !strings.Contains(lines[0], `"_type":"metadata"`) {
		t.Errorf("file layout:\n%s", data)
	}
}

func TestStoreAtomicSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	s := New("cli:direct")
	for i := range 10 {
		s.Append(user(strings.Repeat("x", i*1000)))
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{"cli_direct.jsonl"}) {
		t.Errorf("directory = %v, want only cli_direct.jsonl", names)
	}
}

func TestStoreLoadErrors(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if _, err := store.Load("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load(missing) = %v, want ErrNotFound", err)
	}
	if err := os.WriteFile(store.Path("bad"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("bad"); err == nil {
		t.Error("Load(bad metadata) succeeded")
	}
}

func TestStoreSkipsCorruptMessageLines(t *testing.T) {
	store := NewStore(t.TempDir())
	s := New("k")
	s.Append(user("one"), assistant("two"))
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(store.Path("k"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{truncated\n" + `{"role":"bogus","content":"x"}` + "\n" + `{"role":"user","content":"three"}`)
	f.Close()

	got, err := store.Load("k")
	if err != nil {
		t.Fatal(err)
	}
	want := []providers.Message{user("one"), assistant("two"), user("three")}
	if !reflect.DeepEqual(got.Messages, want) {
		t.Errorf("messages = %+v, want %+v", got.Messages, want)
	}
}

func TestFileName(t *testing.T) {
	tests := []struct{ key, want string }{
		{"cli:direct", "cli_direct.jsonl"},
		{"cli_direct", "cli%5Fdirect.jsonl"},
		{"a/../b", "a%2F..%2Fb.jsonl"},
		{"..", "%2E..jsonl"},
		{"", "%.jsonl"},
		{"slack:C0ab", "slack_%430ab.jsonl"},
		{"telegram:-100.5", "telegram_-100.5.jsonl"},
	}
	for _, tt := range tests {
		if got := fileName(tt.key); got != tt.want {
			t.Errorf("fileName(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
	long := strings.Repeat("k", 500)
	if got := fileName(long); len(got) > maxFileNameBytes+len(fileExt) || got == fileName(long+"x") {
		t.Errorf("long key name %q (len %d) not bounded or not unique", got, len(got))
	}
	for _, k := range []string{"a/b", "..", `a\b`} {
		if strings.ContainsAny(fileName(k), `/\`) || filepath.Base(fileName(k)) != fileName(k) {
			t.Errorf("fileName(%q) = %q escapes the directory", k, fileName(k))
		}
	}
	// Names must stay distinct on case-insensitive filesystems (the macOS
	// and Windows defaults), and for keys that only differ by a NUL byte.
	pairs := [][2]string{{"cli:Work", "cli:work"}, {"slack:C0ABC", "slack:c0abc"}, {"", "\x00"}, {"%41", "A"}}
	for _, p := range pairs {
		if a, b := fileName(p[0]), fileName(p[1]); strings.EqualFold(a, b) {
			t.Errorf("fileName(%q) = %q and fileName(%q) = %q collide ignoring case", p[0], a, p[1], b)
		}
	}
}

func TestStoreKeysDifferingOnlyInCase(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, key := range []string{"cli:Work", "cli:work"} {
		s := New(key)
		s.Append(user("hello from " + key))
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"cli:Work", "cli:work"} {
		s, err := store.Load(key)
		if err != nil {
			t.Fatalf("Load(%q): %v", key, err)
		}
		if len(s.Messages) != 1 || s.Messages[0].Content != "hello from "+key {
			t.Errorf("Load(%q) = %+v", key, s.Messages)
		}
	}
}

func TestStoreListAndDelete(t *testing.T) {
	store := NewStore(t.TempDir())
	if infos, err := store.List(); err != nil || len(infos) != 0 {
		t.Fatalf("List(empty) = %v, %v", infos, err)
	}
	older, newer := New("a:1"), New("b:2")
	older.UpdatedAt = time.Now().Add(-time.Hour)
	for _, s := range []*Session{older, newer} {
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Key != "b:2" || infos[1].Key != "a:1" {
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
}

func TestManager(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	s, err := m.GetOrCreate("cli:direct")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 0 {
		t.Fatalf("new session has messages")
	}
	// GetOrCreate does not write anything by itself.
	if infos, _ := m.List(); len(infos) != 0 {
		t.Errorf("unsaved session listed: %+v", infos)
	}
	again, _ := m.GetOrCreate("cli:direct")
	if again != s {
		t.Error("GetOrCreate did not return the cached session")
	}
	s.Append(user("hi"), assistant("hello"))
	if err := m.Save(s); err != nil {
		t.Fatal(err)
	}

	// A fresh manager (new process) sees the saved session.
	loaded, err := NewManager(dir).GetOrCreate("cli:direct")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Messages, s.Messages) {
		t.Errorf("reloaded messages = %+v", loaded.Messages)
	}

	if err := m.Delete("cli:direct"); err != nil {
		t.Fatal(err)
	}
	fresh, _ := m.GetOrCreate("cli:direct")
	if fresh == s || len(fresh.Messages) != 0 {
		t.Error("Delete did not drop the cached session")
	}
}
