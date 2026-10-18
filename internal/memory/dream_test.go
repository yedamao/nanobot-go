package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// fakeDreamAgent records each prompt and, unless edit is nil, lets the
// test act through the restricted registry like a model would.
type fakeDreamAgent struct {
	mu      sync.Mutex
	prompts []string
	outcome DreamOutcome
	err     error
	edit    func(reg *tools.Registry)
}

func (f *fakeDreamAgent) run(_ context.Context, prompt string, reg *tools.Registry) (DreamOutcome, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.mu.Unlock()
	if f.edit != nil {
		f.edit(reg)
	}
	return f.outcome, f.err
}

func newTestDream(t *testing.T, cfg DreamConfig) (*Dream, *fakeDreamAgent) {
	t.Helper()
	if cfg.Workspace == "" {
		cfg.Workspace = t.TempDir()
	}
	if cfg.Store == nil {
		cfg.Store = NewStore(cfg.Workspace)
		cfg.Store.now = func() time.Time { return time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC) }
	}
	if cfg.Git == "" {
		cfg.Git = "nanobot-test-no-such-git" // git tests opt in
	}
	agent := &fakeDreamAgent{outcome: DreamOutcome{Completed: true, StopReason: "completed", Reply: "done"}}
	cfg.Run = agent.run
	d, err := NewDream(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC) }
	return d, agent
}

func TestNewDreamValidates(t *testing.T) {
	run := func(context.Context, string, *tools.Registry) (DreamOutcome, error) { return DreamOutcome{}, nil }
	store := NewStore(t.TempDir())
	for _, cfg := range []DreamConfig{
		{Store: store, Run: run},
		{Workspace: "ws", Run: run},
		{Workspace: "ws", Store: store},
		{Workspace: "ws", Store: store, Run: run, BatchEntries: -1},
	} {
		if _, err := NewDream(cfg); err == nil {
			t.Errorf("NewDream(%+v) succeeded", cfg)
		}
	}
	d, err := NewDream(DreamConfig{Workspace: "ws", Store: store, Run: run})
	if err != nil || d.cfg.BatchEntries != DefaultDreamBatchEntries || d.cfg.MaxHistoryEntries != DefaultMaxHistoryEntries {
		t.Errorf("defaults = %+v, %v", d, err)
	}
}

func TestDreamNoHistory(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{})
	res, err := d.Run(context.Background())
	if err != nil || res != (DreamResult{}) || len(agent.prompts) != 0 {
		t.Errorf("Run = %+v, %v; prompts %d", res, err, len(agent.prompts))
	}
	if _, err := os.Stat(filepath.Join(d.cfg.Workspace, Dir)); !os.IsNotExist(err) {
		t.Errorf("memory dir created: %v", err)
	}
}

func TestDreamProcessesBatchesAndAdvancesCursor(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{BatchEntries: 3})
	store := d.cfg.Store
	appendN(t, store, 4)
	if _, err := store.Append("from telegram", "telegram:42"); err != nil {
		t.Fatal(err)
	}
	agent.edit = func(reg *tools.Registry) {
		raw, _ := json.Marshal(map[string]string{"path": "memory/MEMORY.md", "content": "- fact"})
		if res := reg.Execute(context.Background(), "write_file", raw); res.IsError {
			t.Errorf("write MEMORY.md: %s", res.Content)
		}
	}

	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Pending != 5 || res.Processed != 3 || res.Cursor != 3 || !res.Outcome.Completed || res.Commit != "" {
		t.Errorf("first run = %+v", res)
	}
	if c, _ := store.DreamCursor(); c != 3 {
		t.Errorf("cursor = %d", c)
	}
	p := agent.prompts[0]
	if !strings.HasPrefix(p, "You are running Dream.") || !strings.Contains(p, "## Conversation History\n[2026-10-18 09:30] entry 1\n[2026-10-18 09:30] entry 2\n") ||
		!strings.Contains(p, "entry 3") || strings.Contains(p, "entry 4") {
		t.Errorf("first prompt:\n%s", p)
	}
	if data, _ := os.ReadFile(filepath.Join(d.cfg.Workspace, "memory", "MEMORY.md")); string(data) != "- fact" {
		t.Errorf("MEMORY.md = %q", data)
	}

	res, err = d.Run(context.Background())
	if err != nil || res.Pending != 2 || res.Processed != 2 || res.Cursor != 5 {
		t.Errorf("second run = %+v, %v", res, err)
	}
	if p := agent.prompts[1]; strings.Contains(p, "entry 3") || !strings.Contains(p, "entry 4") ||
		!strings.Contains(p, "(telegram:42) from telegram") {
		t.Errorf("second prompt:\n%s", p)
	}

	res, err = d.Run(context.Background())
	if err != nil || res.Pending != 0 || res.Processed != 0 || res.Cursor != 5 || len(agent.prompts) != 2 {
		t.Errorf("third run = %+v, %v; prompts %d", res, err, len(agent.prompts))
	}
}

func TestDreamIncompleteRunKeepsCursor(t *testing.T) {
	for name, set := range map[string]func(*fakeDreamAgent){
		"max iterations": func(a *fakeDreamAgent) { a.outcome = DreamOutcome{StopReason: "max_iterations"} },
		"provider error": func(a *fakeDreamAgent) { a.outcome, a.err = DreamOutcome{StopReason: "error"}, errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			d, agent := newTestDream(t, DreamConfig{})
			appendN(t, d.cfg.Store, 2)
			if err := d.cfg.Store.SetDreamCursor(1); err != nil {
				t.Fatal(err)
			}
			set(agent)
			res, err := d.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "cursor stays at 1") {
				t.Errorf("err = %v", err)
			}
			if c, _ := d.cfg.Store.DreamCursor(); c != 1 || res.Cursor != 1 || res.Processed != 1 {
				t.Errorf("cursor = %d, result %+v", c, res)
			}
			// The next run retries the same entry.
			agent.outcome, agent.err = DreamOutcome{Completed: true}, nil
			if res, err := d.Run(context.Background()); err != nil || res.Cursor != 2 {
				t.Errorf("retry = %+v, %v", res, err)
			}
			if !strings.Contains(agent.prompts[1], "entry 2") {
				t.Errorf("retry prompt:\n%s", agent.prompts[1])
			}
		})
	}
}

func TestDreamCorruptCursorStopsBeforeRunning(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{MaxHistoryEntries: 1})
	appendN(t, d.cfg.Store, 3)
	if err := os.WriteFile(d.cfg.Store.DreamCursorPath(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("err = %v", err)
	}
	if len(agent.prompts) != 0 || len(cursors(t, d.cfg.Store)) != 3 {
		t.Error("ran or trimmed with a corrupt cursor")
	}
}

func TestDreamTrimsHistoryKeepingUnprocessed(t *testing.T) {
	d, _ := newTestDream(t, DreamConfig{BatchEntries: 4, MaxHistoryEntries: 3})
	appendN(t, d.cfg.Store, 10)
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Entries 5..10 are unprocessed and survive the limit of 3.
	if res.Cursor != 4 || res.Dropped != 4 || fmt.Sprint(cursors(t, d.cfg.Store)) != "[5 6 7 8 9 10]" {
		t.Errorf("result %+v, cursors %v", res, cursors(t, d.cfg.Store))
	}
	// A failed run trims nothing it has not processed.
	d2, agent := newTestDream(t, DreamConfig{Workspace: d.cfg.Workspace, BatchEntries: 4, MaxHistoryEntries: 3})
	agent.err = errors.New("boom")
	if res, err := d2.Run(context.Background()); err == nil || res.Dropped != 0 {
		t.Errorf("failed run = %+v, %v", res, err)
	}
	if got := fmt.Sprint(cursors(t, d.cfg.Store)); got != "[5 6 7 8 9 10]" {
		t.Errorf("cursors after failed run = %s", got)
	}
	// Negative disables trimming.
	d3, _ := newTestDream(t, DreamConfig{Workspace: d.cfg.Workspace, BatchEntries: 10, MaxHistoryEntries: -1})
	if res, err := d3.Run(context.Background()); err != nil || res.Cursor != 10 || res.Dropped != 0 || len(cursors(t, d.cfg.Store)) != 6 {
		t.Errorf("untrimmed run = %+v, %v", res, err)
	}
}

func TestDreamPromptOverride(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{})
	appendN(t, d.cfg.Store, 1)
	override := filepath.Join(d.cfg.Workspace, "prompts", "dream.md")
	if err := os.MkdirAll(filepath.Dir(override), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("\n  Custom dream rules.  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := agent.prompts[0]; !strings.HasPrefix(p, "Custom dream rules.\n\n## Conversation History\n") || strings.Contains(p, "running Dream") {
		t.Errorf("prompt:\n%s", p)
	}
	// Empty, oversized and non-regular overrides.
	if err := os.WriteFile(override, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.template(), "You are running Dream.") {
		t.Error("empty override not ignored")
	}
	if err := os.WriteFile(override, []byte(strings.Repeat("é", maxDreamPromptChars+10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := d.template(); utf8Len(got) != maxDreamPromptChars {
		t.Errorf("override length = %d", utf8Len(got))
	}
	if err := os.Remove(override); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(override, 0o700); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.template(), "You are running Dream.") {
		t.Error("directory override not ignored")
	}
}

func utf8Len(s string) int { return len([]rune(s)) }

func TestDreamPromptCapsEntries(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{})
	if _, err := d.cfg.Store.Append(strings.Repeat("x", 3*maxDreamEntryChars), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := agent.prompts[0]; strings.Contains(p, strings.Repeat("x", maxDreamEntryChars+1)) || !strings.Contains(p, strings.Repeat("x", maxDreamEntryChars/2)) {
		t.Errorf("entry not capped: prompt has %d bytes", len(p))
	}
}

func TestDreamRunGetsRestrictedTools(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{})
	appendN(t, d.cfg.Store, 1)
	agent.edit = func(reg *tools.Registry) {
		if _, ok := reg.Get("exec"); ok {
			t.Error("exec offered to Dream")
		}
		raw, _ := json.Marshal(map[string]string{"path": "memory/history.jsonl", "content": "wiped"})
		if res := reg.Execute(context.Background(), "write_file", raw); !res.IsError {
			t.Errorf("history.jsonl writable during Dream: %s", res.Content)
		}
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cursors(t, d.cfg.Store)) != 1 {
		t.Error("history.jsonl damaged")
	}
}

// --- git ---

// gitEnv skips the test without git and isolates it from the user's git
// configuration.
func gitEnv(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return bin
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeMemory(content string) func(*tools.Registry) {
	return func(reg *tools.Registry) {
		raw, _ := json.Marshal(map[string]string{"path": "memory/MEMORY.md", "content": content})
		reg.Execute(context.Background(), "write_file", raw)
	}
}

func TestDreamCommitsToNewRepository(t *testing.T) {
	bin := gitEnv(t)
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("not versioned"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, agent := newTestDream(t, DreamConfig{Workspace: ws, Git: bin})
	appendN(t, d.cfg.Store, 2)
	agent.edit = writeMemory("- Ada likes Go")

	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit == "" || res.Cursor != 2 {
		t.Fatalf("result = %+v", res)
	}
	if got := git(t, ws, "log", "-1", "--format=%s|%an <%ae>"); got != "dream: 2026-10-18T03:00:00Z|nanobot <nanobot@dream>" {
		t.Errorf("commit = %q", got)
	}
	if body := git(t, ws, "log", "-1", "--format=%b"); !strings.Contains(body, "memory/MEMORY.md") {
		t.Errorf("commit body = %q", body)
	}
	if got := git(t, ws, "rev-parse", "--short", "HEAD"); got != res.Commit {
		t.Errorf("HEAD = %s, result %s", got, res.Commit)
	}
	// Only long-term memory is versioned, never the history or cursor.
	if files := git(t, ws, "ls-files"); files != ".gitignore\nmemory/MEMORY.md" {
		t.Errorf("tracked files:\n%s", files)
	}
	if status := git(t, ws, "status", "--porcelain"); status != "" {
		t.Errorf("status after commit:\n%s", status)
	}

	// No changes: no commit.
	appendN(t, d.cfg.Store, 1)
	agent.edit = nil
	if res, err := d.Run(context.Background()); err != nil || res.Commit != "" || res.Cursor != 3 {
		t.Errorf("no-op run = %+v, %v", res, err)
	}
	if n := git(t, ws, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("commits = %s", n)
	}

	// A skill created later, and a profile file, are committed; an
	// incomplete run's edits are committed too, but the cursor stays.
	appendN(t, d.cfg.Store, 1)
	agent.edit = func(reg *tools.Registry) {
		for _, p := range []string{"skills/deploy/SKILL.md", "USER.md"} {
			raw, _ := json.Marshal(map[string]string{"path": p, "content": "x"})
			reg.Execute(context.Background(), "write_file", raw)
		}
	}
	agent.outcome = DreamOutcome{StopReason: "max_iterations"}
	res, err = d.Run(context.Background())
	if err == nil || res.Commit == "" || res.Cursor != 3 {
		t.Errorf("incomplete run = %+v, %v", res, err)
	}
	if files := git(t, ws, "show", "--name-only", "--format=", "HEAD"); files != "USER.md\nskills/deploy/SKILL.md" {
		t.Errorf("committed files:\n%s", files)
	}
}

func TestDreamCommitLeavesOtherStagedChanges(t *testing.T) {
	bin := gitEnv(t)
	ws := t.TempDir()
	git(t, ws, "init", "-q")
	for name, content := range map[string]string{"notes.txt": "v1", "SOUL.md": "soul", ".gitignore": "memory/history.jsonl\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, ws, "add", ".")
	git(t, ws, "-c", "user.name=u", "-c", "user.email=u@x", "commit", "-q", "-m", "user commit")
	// The user has an unrelated change staged.
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "notes.txt")

	d, agent := newTestDream(t, DreamConfig{Workspace: ws, Git: bin})
	appendN(t, d.cfg.Store, 1)
	agent.edit = func(reg *tools.Registry) {
		writeMemory("- fact")(reg)
		if err := os.Remove(filepath.Join(ws, "SOUL.md")); err != nil { // deletion is committed too
			t.Error(err)
		}
	}
	res, err := d.Run(context.Background())
	if err != nil || res.Commit == "" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if files := git(t, ws, "show", "--name-status", "--format=", "HEAD"); files != "D\tSOUL.md\nA\tmemory/MEMORY.md" {
		t.Errorf("committed:\n%s", files)
	}
	if staged := git(t, ws, "diff", "--cached", "--name-only"); staged != "notes.txt" {
		t.Errorf("staged after Dream = %q", staged)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, ".gitignore")); string(data) != "memory/history.jsonl\n" {
		t.Errorf("user .gitignore changed: %q", data)
	}
}

func TestDreamSkipsCommitInsideAnotherRepository(t *testing.T) {
	bin := gitEnv(t)
	parent := t.TempDir()
	git(t, parent, "init", "-q")
	ws := filepath.Join(parent, "workspace")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	d, agent := newTestDream(t, DreamConfig{Workspace: ws, Git: bin})
	appendN(t, d.cfg.Store, 1)
	agent.edit = writeMemory("- fact")
	res, err := d.Run(context.Background())
	if err != nil || res.Commit != "" || res.Cursor != 1 {
		t.Errorf("Run = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git")); !os.IsNotExist(err) {
		t.Errorf("nested repository created: %v", err)
	}
	if out, err := exec.Command("git", "-C", parent, "rev-parse", "HEAD").CombinedOutput(); err == nil {
		t.Errorf("committed to the parent repository: %s", out)
	}
}

func TestDreamWithoutGitIsANoop(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{Git: "nanobot-test-no-such-git"})
	appendN(t, d.cfg.Store, 1)
	agent.edit = writeMemory("- fact")
	res, err := d.Run(context.Background())
	if err != nil || res.Commit != "" || res.Cursor != 1 {
		t.Errorf("Run = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(d.cfg.Workspace, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git created: %v", err)
	}
}
