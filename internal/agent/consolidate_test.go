package agent

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yedamao/nanobot-go/internal/memory"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
)

// summarizer routes consolidation requests (any request whose system
// prompt is not the agent's) to a canned summary or error, and the rest to
// chat.
type summarizer struct {
	chat providers.Provider

	mu      sync.Mutex
	summary string
	err     error
	reqs    []providers.ChatRequest
}

func (s *summarizer) Name() string { return "summarizer" }

func (s *summarizer) Chat(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	if strings.HasPrefix(req.Messages[0].Content, "# nanobot-go") {
		return s.chat.Chat(ctx, req)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if s.err != nil {
		return nil, s.err
	}
	return &providers.ChatResponse{Content: s.summary, FinishReason: providers.FinishStop}, nil
}

func (s *summarizer) set(summary string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary, s.err = summary, err
}

func (s *summarizer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

type consolidationEnv struct {
	loop     *Loop
	chat     *scripted
	sum      *summarizer
	store    *memory.Store
	sessions *session.Manager
	dir      string
}

// newConsolidationLoop returns a loop that consolidates past maxHistory
// messages (nil Consolidator when maxHistory < 0).
func newConsolidationLoop(t *testing.T, maxHistory int) *consolidationEnv {
	t.Helper()
	chat := &scripted{steps: []step{reply("ok")}}
	sum := &summarizer{chat: chat, summary: "- [ephemeral] summary 1"}
	ws, dir := t.TempDir(), t.TempDir()
	store := memory.NewStore(ws)
	var c *memory.Consolidator
	if maxHistory >= 0 {
		var err error
		c, err = memory.NewConsolidator(memory.Config{Provider: sum, Store: store, MaxMessages: maxHistory, ContextWindowTokens: 200_000, ReplyMaxTokens: 1000})
		if err != nil {
			t.Fatal(err)
		}
	}
	sessions := session.NewManager(dir)
	l, err := NewLoop(LoopConfig{
		Provider:           sum,
		Sessions:           sessions,
		Context:            NewContextBuilder(ws),
		MaxHistoryMessages: max(0, maxHistory),
		Consolidator:       c,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &consolidationEnv{loop: l, chat: chat, sum: sum, store: store, sessions: sessions, dir: dir}
}

func (e *consolidationEnv) send(t *testing.T, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if _, err := e.loop.ProcessDirect(context.Background(), m, "cli:direct"); err != nil {
			t.Fatalf("ProcessDirect(%q): %v", m, err)
		}
	}
}

// lastChat returns the last request that reached the chat model.
func (e *consolidationEnv) lastChat() providers.ChatRequest {
	e.chat.mu.Lock()
	defer e.chat.mu.Unlock()
	return e.chat.reqs[len(e.chat.reqs)-1]
}

func (e *consolidationEnv) stored(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.NewStore(e.dir).Load("cli:direct")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestLoopConsolidatesInsteadOfDropping(t *testing.T) {
	e := newConsolidationLoop(t, 4)
	e.send(t, "u1", "u2", "u3")
	if e.sum.calls() != 0 {
		t.Fatalf("consolidated at %d messages with a limit of 4", 6)
	}
	e.send(t, "u4") // 6 stored messages > 4: archive turns 1-2, keep turn 3

	if e.sum.calls() != 1 {
		t.Fatalf("summarizer calls = %d, want 1", e.sum.calls())
	}
	if in := e.sum.reqs[0].Messages[1].Content; !strings.Contains(in, "USER: u1") || !strings.Contains(in, "USER: u2") || strings.Contains(in, "u3") {
		t.Errorf("summarizer input:\n%s", in)
	}

	req := e.lastChat().Messages
	if !strings.Contains(req[0].Content, "# Archived Context Summary") || !strings.HasSuffix(req[0].Content, "- [ephemeral] summary 1") {
		t.Errorf("system prompt lacks the summary: ...%s", req[0].Content[max(0, len(req[0].Content)-200):])
	}
	if got, want := summarize(req[1:]), []string{"user:u3", "assistant:ok", "user:u4"}; !slices.Equal(got, want) {
		t.Errorf("replayed = %v, want %v", got, want)
	}

	entries, err := e.store.Entries()
	if err != nil || len(entries) != 1 || entries[0].Content != "- [ephemeral] summary 1" || entries[0].SessionKey != "cli:direct" {
		t.Errorf("history.jsonl = %+v, %v", entries, err)
	}
	// The full transcript is kept on disk with the marker.
	sess := e.stored(t)
	if len(sess.Messages) != 8 || sess.LastConsolidated != 4 || sess.Summary != "- [ephemeral] summary 1" {
		t.Errorf("stored session: %d messages, marker %d, summary %q", len(sess.Messages), sess.LastConsolidated, sess.Summary)
	}

	// Consolidation does not repeat every turn: the next one fits.
	e.send(t, "u5")
	if e.sum.calls() != 1 {
		t.Errorf("summarizer calls = %d after a turn under the limit", e.sum.calls())
	}
	// Once over again, the previous summary is merged into the next one.
	e.sum.set("- [ephemeral] summary 2", nil)
	e.send(t, "u6")
	if e.sum.calls() != 2 || !strings.Contains(e.sum.reqs[1].Messages[1].Content, "summary 1") {
		t.Fatalf("second consolidation did not see the previous summary")
	}
	if sess := e.stored(t); sess.LastConsolidated != 8 || sess.Summary != "- [ephemeral] summary 2" {
		t.Errorf("after second consolidation: marker %d summary %q", sess.LastConsolidated, sess.Summary)
	}
	if entries, _ := e.store.Entries(); len(entries) != 2 || entries[1].Cursor != 2 {
		t.Errorf("history.jsonl = %+v", entries)
	}
}

func TestLoopFallsBackWhenConsolidationFails(t *testing.T) {
	e := newConsolidationLoop(t, 4)
	e.sum.set("", errors.New("summarizer down"))
	e.send(t, "u1", "u2", "u3", "u4")

	if e.sum.calls() != 1 {
		t.Fatalf("summarizer calls = %d, want 1", e.sum.calls())
	}
	// Old behaviour for this turn: the window of the last 4 messages, no
	// summary.
	req := e.lastChat().Messages
	if strings.Contains(req[0].Content, "Archived Context Summary") {
		t.Error("summary section after a failed consolidation")
	}
	if got, want := summarize(req[1:]), []string{"user:u2", "assistant:ok", "user:u3", "assistant:ok", "user:u4"}; !slices.Equal(got, want) {
		t.Errorf("replayed = %v, want %v", got, want)
	}
	sess := e.stored(t)
	if len(sess.Messages) != 8 || sess.LastConsolidated != 0 || sess.Summary != "" {
		t.Errorf("stored: %d messages, marker %d, summary %q", len(sess.Messages), sess.LastConsolidated, sess.Summary)
	}
	if _, err := os.Stat(e.store.HistoryPath()); !os.IsNotExist(err) {
		t.Errorf("history.jsonl written after failure: %v", err)
	}

	// The next turn retries and archives everything that was pending.
	e.sum.set("- [ephemeral] recovered", nil)
	e.send(t, "u5")
	if in := e.sum.reqs[1].Messages[1].Content; !strings.Contains(in, "USER: u1") || !strings.Contains(in, "USER: u3") {
		t.Errorf("retry lost messages:\n%s", in)
	}
	if sess := e.stored(t); sess.LastConsolidated != 6 || sess.Summary != "- [ephemeral] recovered" {
		t.Errorf("after retry: marker %d summary %q", sess.LastConsolidated, sess.Summary)
	}
}

func TestCompactCommand(t *testing.T) {
	e := newConsolidationLoop(t, 100)
	ctx := context.Background()
	if got, err := e.loop.ProcessDirect(ctx, "/compact", "cli:direct"); err != nil || got != compactNothingText {
		t.Errorf("/compact on empty session = %q, %v", got, err)
	}
	e.send(t, "u1", "u2")
	got, err := e.loop.ProcessDirect(ctx, " /Compact ", "cli:direct")
	if err != nil || got != "Compacted 4 messages into a summary." {
		t.Fatalf("/compact = %q, %v", got, err)
	}
	if sess := e.stored(t); sess.LastConsolidated != 4 || len(sess.Messages) != 4 {
		t.Errorf("stored: marker %d, %d messages (commands are not stored)", sess.LastConsolidated, len(sess.Messages))
	}
	if got, _ := e.loop.ProcessDirect(ctx, "/compact", "cli:direct"); got != compactNothingText {
		t.Errorf("second /compact = %q", got)
	}

	e.send(t, "u3")
	req := e.lastChat().Messages
	if got, want := summarize(req[1:]), []string{"user:u3"}; !slices.Equal(got, want) {
		t.Errorf("replayed after /compact = %v, want %v", got, want)
	}
	if !strings.Contains(req[0].Content, "summary 1") {
		t.Error("summary missing after /compact")
	}

	// /new drops the summary too.
	if _, err := e.loop.ProcessDirect(ctx, "/new", "cli:direct"); err != nil {
		t.Fatal(err)
	}
	if sess := e.stored(t); sess.Summary != "" || sess.LastConsolidated != 0 {
		t.Errorf("after /new: marker %d summary %q", sess.LastConsolidated, sess.Summary)
	}
	e.send(t, "fresh")
	if strings.Contains(e.lastChat().Messages[0].Content, "Archived Context Summary") {
		t.Error("summary survived /new")
	}
}

func TestCompactCommandFailure(t *testing.T) {
	e := newConsolidationLoop(t, 100)
	e.send(t, "u1")
	e.sum.set("", errors.New("summarizer down"))
	if _, err := e.loop.ProcessDirect(context.Background(), "/compact", "cli:direct"); err == nil || !strings.Contains(err.Error(), "summarizer down") {
		t.Errorf("/compact error = %v", err)
	}
	if sess := e.stored(t); sess.LastConsolidated != 0 || len(sess.Messages) != 2 {
		t.Errorf("failed /compact changed the session: marker %d, %d messages", sess.LastConsolidated, len(sess.Messages))
	}
}

func TestCompactCommandDisabled(t *testing.T) {
	e := newConsolidationLoop(t, -1)
	e.send(t, "u1", "u2", "u3")
	if got, err := e.loop.ProcessDirect(context.Background(), "/compact", "cli:direct"); err != nil || got != compactDisabledText {
		t.Errorf("/compact = %q, %v", got, err)
	}
	if e.sum.calls() != 0 {
		t.Errorf("summarizer called %d times without a consolidator", e.sum.calls())
	}
	if help, _ := e.loop.ProcessDirect(context.Background(), "/help", "cli:direct"); !strings.Contains(help, "/compact") {
		t.Errorf("/help = %q", help)
	}
}
