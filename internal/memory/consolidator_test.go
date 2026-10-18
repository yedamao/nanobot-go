package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
)

// fakeProvider returns resp (or err) and records requests.
type fakeProvider struct {
	mu   sync.Mutex
	resp *providers.ChatResponse
	err  error
	reqs []providers.ChatRequest
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Chat(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func summaryResp(s string) *providers.ChatResponse {
	return &providers.ChatResponse{Content: s, FinishReason: providers.FinishStop}
}

func user(s string) providers.Message { return providers.Message{Role: providers.RoleUser, Content: s} }
func assistant(s string) providers.Message {
	return providers.Message{Role: providers.RoleAssistant, Content: s}
}
func toolCall(id string) providers.Message {
	return providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{ID: id, Name: "exec", Arguments: json.RawMessage(`{"command":"ls"}`)}}}
}
func toolResult(id string) providers.Message {
	return providers.Message{Role: providers.RoleTool, ToolCallID: id, Name: "exec", Content: "out " + id, IsError: true}
}

// turns returns a session with n user/assistant turns.
func turns(n int) *session.Session {
	s := session.New("cli:direct")
	for i := range n {
		s.Append(user("u"+string(rune('a'+i))), assistant("a"+string(rune('a'+i))))
	}
	return s
}

func newTestConsolidator(t *testing.T, p providers.Provider, cfg Config) (*Consolidator, *Store) {
	t.Helper()
	store := newTestStore(t)
	cfg.Provider, cfg.Store = p, store
	c, err := NewConsolidator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, store
}

func TestNewConsolidatorValidates(t *testing.T) {
	store := NewStore(t.TempDir())
	p := &fakeProvider{}
	for _, cfg := range []Config{
		{Store: store},
		{Provider: p},
		{Provider: p, Store: store, ContextWindowTokens: -1},
		{Provider: p, Store: store, SummaryMaxTokens: -1},
	} {
		if _, err := NewConsolidator(cfg); err == nil {
			t.Errorf("NewConsolidator(%+v) succeeded", cfg)
		}
	}
	c, err := NewConsolidator(Config{Provider: p, Store: store})
	if err != nil || c.cfg.SummaryMaxTokens != DefaultSummaryMaxTokens {
		t.Errorf("defaults: %+v, %v", c, err)
	}
}

func TestPlanByMessageCount(t *testing.T) {
	c, _ := newTestConsolidator(t, &fakeProvider{}, Config{MaxMessages: 8})
	if _, ok := c.Plan(turns(4), 0); ok {
		t.Error("8 messages with a limit of 8 should not consolidate")
	}
	s := turns(5) // 10 messages
	b, ok := c.Plan(s, 0)
	if !ok || b != 6 {
		t.Fatalf("Plan = %d, %v; want 6 (keep the last 4 messages)", b, ok)
	}

	// The plan is relative to the existing marker.
	s.Append(user("x"), assistant("y"), user("z"), assistant("w")) // 14 messages
	if err := s.CommitSummary("old", 4); err != nil {
		t.Fatal(err)
	}
	if b, ok = c.Plan(s, 0); !ok || b != 10 {
		t.Errorf("Plan after marker = %d, %v; want 10", b, ok)
	}
}

func TestPlanByTokens(t *testing.T) {
	// Window 2000 - reply 0 - safety 1024 = budget 976 tokens.
	c, _ := newTestConsolidator(t, &fakeProvider{}, Config{ContextWindowTokens: 2000, SummaryMaxTokens: 100})
	s := session.New("k")
	big := strings.Repeat("x", 800) // ~200 tokens
	for range 6 {
		s.Append(user(big), assistant("ok"))
	}
	if _, ok := c.Plan(s, 0); !ok {
		t.Fatal("~1250 tokens over a 976 budget should consolidate")
	}
	b, _ := c.Plan(s, 0)
	// Keep (976-100)/2 = 438 tokens: the last two turns (~420 tokens).
	if b != 8 {
		t.Errorf("boundary = %d, want 8", b)
	}
	if _, ok := c.Plan(turns(2), 0); ok {
		t.Error("small history consolidated")
	}
	// Overhead counts toward the budget.
	if _, ok := c.Plan(turns(2), 2000); !ok {
		t.Error("overhead ignored")
	}
}

func TestPlanKeepsLatestTurnAndTurnBoundaries(t *testing.T) {
	c, _ := newTestConsolidator(t, &fakeProvider{}, Config{MaxMessages: 2})
	// One long turn cannot be split.
	s := session.New("k")
	s.Append(user("u"), toolCall("1"), toolResult("1"), toolCall("2"), toolResult("2"), assistant("a"))
	if b, ok := c.Plan(s, 0); ok {
		t.Errorf("single turn planned to %d", b)
	}
	// Two long turns: keep the latest even though it exceeds the target.
	s.Append(user("v"), toolCall("3"), toolResult("3"), assistant("b"))
	b, ok := c.Plan(s, 0)
	if !ok || b != 6 || s.Messages[b].Role != providers.RoleUser {
		t.Errorf("Plan = %d, %v; want 6 (start of the latest turn)", b, ok)
	}
}

func TestConsolidateWritesHistoryThenCommits(t *testing.T) {
	p := &fakeProvider{resp: summaryResp("  - [ephemeral] user is testing  \n")}
	c, store := newTestConsolidator(t, p, Config{Model: "m", SummaryMaxTokens: 300})
	s := session.New("cli:direct")
	s.Append(user("u1"), toolCall("c1"), toolResult("c1"), assistant("a1"), user("u2"), assistant("a2"))
	if err := s.CommitSummary("- [durable] old fact", 0); err != nil {
		t.Fatal(err)
	}

	res, err := c.Consolidate(context.Background(), s, 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archived != 4 || res.Entry.Cursor != 1 || res.Summary != "- [ephemeral] user is testing" {
		t.Errorf("result = %+v", res)
	}
	if s.LastConsolidated != 4 || s.Summary != res.Summary || len(s.Messages) != 6 {
		t.Errorf("session marker %d summary %q messages %d", s.LastConsolidated, s.Summary, len(s.Messages))
	}
	entries, err := store.Entries()
	if err != nil || len(entries) != 1 || entries[0].Content != res.Summary || entries[0].SessionKey != "cli:direct" {
		t.Errorf("history = %+v, %v", entries, err)
	}

	req := p.reqs[0]
	if req.Model != "m" || req.MaxTokens != 300 || len(req.Tools) != 0 || len(req.Messages) != 2 || req.Messages[0].Role != providers.RoleSystem {
		t.Errorf("request = %+v", req)
	}
	in := req.Messages[1].Content
	for _, want := range []string{"## Previous checkpoint", "old fact", "## Archived messages (4)", "USER: u1", `[tool call] exec {"command":"ls"}`, "TOOL exec (error): out c1", "ASSISTANT: a1"} {
		if !strings.Contains(in, want) {
			t.Errorf("summarizer input lacks %q:\n%s", want, in)
		}
	}
	if strings.Contains(in, "u2") {
		t.Error("summarizer saw messages after the boundary")
	}
}

func TestConsolidateFailuresLeaveSessionUnchanged(t *testing.T) {
	cases := []struct {
		name string
		p    *fakeProvider
	}{
		{"provider error", &fakeProvider{err: errors.New("boom")}},
		{"truncated", &fakeProvider{resp: &providers.ChatResponse{Content: "- partial", FinishReason: providers.FinishLength}}},
		{"error finish", &fakeProvider{resp: &providers.ChatResponse{Content: "x", FinishReason: providers.FinishError}}},
		{"tool calls", &fakeProvider{resp: &providers.ChatResponse{ToolCalls: []providers.ToolCall{{ID: "1", Name: "exec"}}, FinishReason: providers.FinishToolCalls}}},
		{"empty", &fakeProvider{resp: summaryResp("  ")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, store := newTestConsolidator(t, tc.p, Config{})
			s := turns(3)
			if _, err := c.Consolidate(context.Background(), s, 4); err == nil {
				t.Fatal("Consolidate succeeded")
			}
			if s.LastConsolidated != 0 || s.Summary != "" || len(s.Messages) != 6 {
				t.Errorf("session changed: marker %d summary %q", s.LastConsolidated, s.Summary)
			}
			if _, err := os.Stat(store.HistoryPath()); !os.IsNotExist(err) {
				t.Errorf("history written: %v", err)
			}
		})
	}
}

func TestConsolidateHistoryWriteFailureKeepsMarker(t *testing.T) {
	c, store := newTestConsolidator(t, &fakeProvider{resp: summaryResp("- [ephemeral] x")}, Config{})
	if err := os.MkdirAll(store.HistoryPath(), 0o700); err != nil { // unwritable
		t.Fatal(err)
	}
	s := turns(3)
	if _, err := c.Consolidate(context.Background(), s, 4); err == nil {
		t.Fatal("Consolidate succeeded without writing history")
	}
	if s.LastConsolidated != 0 || s.Summary != "" {
		t.Errorf("marker advanced before the summary was stored: %d %q", s.LastConsolidated, s.Summary)
	}
}

func TestConsolidateNothingWorthKeeping(t *testing.T) {
	c, store := newTestConsolidator(t, &fakeProvider{resp: summaryResp("(nothing)")}, Config{})
	s := turns(3)
	res, err := c.Consolidate(context.Background(), s, 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archived != 4 || res.Entry.Cursor != 0 || s.LastConsolidated != 4 || s.Summary != "" {
		t.Errorf("result %+v, marker %d, summary %q", res, s.LastConsolidated, s.Summary)
	}
	if entries, _ := store.Entries(); len(entries) != 0 {
		t.Errorf("history = %+v", entries)
	}
}

func TestConsolidateNoop(t *testing.T) {
	p := &fakeProvider{resp: summaryResp("x")}
	c, _ := newTestConsolidator(t, p, Config{})
	s := turns(2)
	if err := s.CommitSummary("old", 2); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Consolidate(context.Background(), s, 2); err != nil || res.Archived != 0 {
		t.Errorf("Consolidate at marker = %+v, %v", res, err)
	}
	if _, err := c.Consolidate(context.Background(), s, 9); err == nil {
		t.Error("boundary past the end accepted")
	}
	if len(p.reqs) != 0 {
		t.Errorf("provider called %d times", len(p.reqs))
	}
}

func TestConsolidateHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := newTestConsolidator(t, &fakeProvider{err: context.Canceled}, Config{})
	s := turns(3)
	if _, err := c.Consolidate(ctx, s, 4); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFormatTranscriptOmitsMiddle(t *testing.T) {
	var msgs []providers.Message
	for i := range 10 {
		msgs = append(msgs, user(strings.Repeat(string(rune('a'+i)), 100)))
	}
	out, omitted := formatTranscript(msgs, 500)
	if omitted == 0 || !strings.Contains(out, "messages omitted") {
		t.Fatalf("nothing omitted: %d\n%s", omitted, out)
	}
	if !strings.Contains(out, strings.Repeat("a", 100)) || !strings.Contains(out, strings.Repeat("j", 100)) {
		t.Error("oldest or newest message dropped")
	}
	if len(out) > 600 {
		t.Errorf("transcript is %d chars, limit 500", len(out))
	}
	if out, omitted := formatTranscript(msgs[:2], 500); omitted != 0 || strings.Contains(out, "omitted") {
		t.Errorf("short transcript trimmed: %s", out)
	}
}

func TestEstimates(t *testing.T) {
	if EstimateTokens("") != 0 || EstimateTokens("abcd") != 1 || EstimateTokens("abcde") != 2 {
		t.Error("EstimateTokens")
	}
	if got := EstimateMessages([]providers.Message{user("abcd"), toolCall("1")}); got <= EstimateTokens("abcd")+2*messageOverheadTokens {
		t.Errorf("EstimateMessages = %d ignores tool calls", got)
	}
	if EstimateJSON(map[string]string{"a": "b"}) != 3 || EstimateJSON(make(chan int)) != 0 {
		t.Error("EstimateJSON")
	}
}
