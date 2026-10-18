package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools"
)

func newTestLoop(t *testing.T, p providers.Provider, reg *tools.Registry, b *bus.MessageBus) (*Loop, *session.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	sessions := session.NewManager(session.NewJSONLStore(dir))
	l, err := NewLoop(LoopConfig{
		Bus:      b,
		Provider: p,
		Tools:    reg,
		Sessions: sessions,
		Context:  NewContextBuilder(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return l, sessions, dir
}

func TestProcessDirectPersistsTurns(t *testing.T) {
	p := &scripted{steps: []step{reply("first"), reply("second")}}
	l, _, dir := newTestLoop(t, p, nil, nil)
	ctx := context.Background()

	got, err := l.ProcessDirect(ctx, "hi", "cli:direct")
	if err != nil || got != "first" {
		t.Fatalf("ProcessDirect = %q, %v", got, err)
	}
	if got, err = l.ProcessDirect(ctx, "again", "cli:direct"); err != nil || got != "second" {
		t.Fatalf("ProcessDirect = %q, %v", got, err)
	}

	// The second request replays the first turn after the system prompt.
	req := p.reqs[1].Messages
	if len(req) != 4 || req[0].Role != providers.RoleSystem || req[1].Content != "hi" || req[2].Content != "first" || req[3].Content != "again" {
		t.Errorf("second request = %+v", req)
	}

	// A fresh manager reads the turns back from disk, without system prompts.
	sess, err := session.NewManager(session.NewJSONLStore(dir)).GetOrCreate("cli:direct")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user:hi", "assistant:first", "user:again", "assistant:second"}
	if got := summarize(sess.Messages); !slices.Equal(got, want) {
		t.Errorf("persisted = %v, want %v", got, want)
	}
}

func TestWaitConcurrentWithNewSessions(t *testing.T) {
	// Wait may run while turns for new sessions are being enqueued (for
	// example ProcessDirect during Run's shutdown); neither may race.
	p := &scripted{steps: []step{reply("ok")}}
	l, _, _ := newTestLoop(t, p, nil, nil)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if _, err := l.ProcessDirect(context.Background(), "hi", fmt.Sprintf("cli:%d", i)); err != nil {
				t.Error(err)
			}
		})
		wg.Go(l.Wait)
	}
	wg.Wait()
	l.Wait()
}

func TestProcessDirectPersistsToolExchange(t *testing.T) {
	log := &execLog{}
	tool := &fakeTool{name: "probe", readOnly: true, log: log}
	p := &scripted{steps: []step{callTools(call("c1", "probe", `{"v":"x"}`)), reply("done")}}
	l, sessions, _ := newTestLoop(t, p, registry(t, tool), nil)

	if _, err := l.ProcessDirect(context.Background(), "use the tool", "cli:direct"); err != nil {
		t.Fatal(err)
	}
	sess, _ := sessions.GetOrCreate("cli:direct")
	want := []string{"user:use the tool", "assistant:[probe]", "tool:c1", "assistant:done"}
	if got := summarize(sess.Messages); !slices.Equal(got, want) {
		t.Errorf("persisted = %v, want %v", got, want)
	}
}

func TestFailedTurnIsNotPersisted(t *testing.T) {
	boom := &providers.APIError{Provider: "scripted", Kind: providers.KindServerError, Message: "boom"}
	p := &scripted{steps: []step{reply("ok"), callTools(call("c1", "missing", `{}`)), fail(boom)}}
	l, sessions, dir := newTestLoop(t, p, nil, nil)
	ctx := context.Background()

	if _, err := l.ProcessDirect(ctx, "first", "s:1"); err != nil {
		t.Fatal(err)
	}
	// The second turn makes a tool call and then the provider fails.
	if _, err := l.ProcessDirect(ctx, "second", "s:1"); !errors.As(err, new(*providers.APIError)) {
		t.Fatalf("ProcessDirect error = %v, want APIError", err)
	}
	for _, m := range []*session.Manager{sessions, session.NewManager(session.NewJSONLStore(dir))} {
		sess, _ := m.GetOrCreate("s:1")
		if got, want := summarize(sess.Messages), []string{"user:first", "assistant:ok"}; !slices.Equal(got, want) {
			t.Errorf("session after failed turn = %v, want %v", got, want)
		}
	}
}

func TestCanceledTurnIsNotPersisted(t *testing.T) {
	started := make(chan struct{})
	p := &scripted{steps: []step{func(ctx context.Context, _ providers.ChatRequest) (*providers.ChatResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	l, sessions, _ := newTestLoop(t, p, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	if _, err := l.ProcessDirect(ctx, "slow", "s:1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	l.Wait()
	sess, _ := sessions.GetOrCreate("s:1")
	if len(sess.Messages) != 0 {
		t.Errorf("canceled turn persisted: %v", summarize(sess.Messages))
	}
}

func TestCommands(t *testing.T) {
	p := &scripted{steps: []step{reply("answer")}}
	l, sessions, dir := newTestLoop(t, p, nil, nil)
	ctx := context.Background()

	help, err := l.ProcessDirect(ctx, " /HELP ", "cli:direct")
	if err != nil || !strings.Contains(help, "/new") {
		t.Errorf("/help = %q, %v", help, err)
	}
	if _, err := l.ProcessDirect(ctx, "hello", "cli:direct"); err != nil {
		t.Fatal(err)
	}
	got, err := l.ProcessDirect(ctx, "/new", "cli:direct")
	if err != nil || got != newSessionText {
		t.Errorf("/new = %q, %v", got, err)
	}
	if len(p.reqs) != 1 {
		t.Errorf("provider called %d times, want 1 (commands must not reach the model)", len(p.reqs))
	}
	for _, m := range []*session.Manager{sessions, session.NewManager(session.NewJSONLStore(dir))} {
		if sess, _ := m.GetOrCreate("cli:direct"); len(sess.Messages) != 0 {
			t.Errorf("history after /new = %v", summarize(sess.Messages))
		}
	}
}

// echo replies with the last user message. Before replying to a message
// starting with "wait-for:<name>" it blocks until a message with content
// <name> has reached the provider, proving that sessions run in parallel.
type echo struct {
	mu      sync.Mutex
	arrived map[string]chan struct{}
	history map[string][]int // session marker -> history lengths seen
}

func (e *echo) Name() string { return "echo" }

func (e *echo) signal(name string) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.arrived[name]
	if !ok {
		ch = make(chan struct{})
		e.arrived[name] = ch
	}
	return ch
}

func (e *echo) Chat(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	last := req.Messages[len(req.Messages)-1].Content
	e.mu.Lock()
	e.history[last[:1]] = append(e.history[last[:1]], len(req.Messages))
	e.mu.Unlock()
	close(e.signal(last))
	if name, ok := strings.CutPrefix(last, "a-wait-for:"); ok {
		select {
		case <-e.signal(name):
		case <-time.After(5 * time.Second):
			return nil, errors.New("sessions did not run in parallel")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &providers.ChatResponse{Content: "re:" + last, FinishReason: providers.FinishStop}, nil
}

func TestRunOrdersPerSessionAndParallelizesAcrossSessions(t *testing.T) {
	b := bus.NewMessageBus(64)
	p := &echo{arrived: map[string]chan struct{}{}, history: map[string][]int{}}
	l, _, _ := newTestLoop(t, p, nil, b)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	const n = 5
	// Session A's first turn blocks until session B's first message reaches
	// the provider; with a single global worker this would time out.
	inputs := map[string][]string{"A": {"a-wait-for:b0"}, "B": {}}
	for i := range n {
		if i > 0 {
			inputs["A"] = append(inputs["A"], fmt.Sprintf("a%d", i))
		}
		inputs["B"] = append(inputs["B"], fmt.Sprintf("b%d", i))
	}
	for i := range n {
		for _, chat := range []string{"A", "B"} {
			msg := bus.InboundMessage{Channel: "test", ChatID: chat, Content: inputs[chat][i]}
			if err := b.PublishInbound(ctx, msg); err != nil {
				t.Fatal(err)
			}
		}
	}

	got := map[string][]string{}
	for range 2 * n {
		out, err := consumeWithTimeout(b)
		if err != nil {
			t.Fatal(err)
		}
		if out.Channel != "test" {
			t.Errorf("outbound channel = %q", out.Channel)
		}
		got[out.ChatID] = append(got[out.ChatID], out.Content)
	}
	for _, chat := range []string{"A", "B"} {
		var want []string
		for _, in := range inputs[chat] {
			want = append(want, "re:"+in)
		}
		if !slices.Equal(got[chat], want) {
			t.Errorf("session %s replies = %v, want %v", chat, got[chat], want)
		}
	}
	// Within a session each turn sees all previous turns: system + 2 per
	// earlier turn + the new user message.
	for _, marker := range []string{"a", "b"} {
		want := []int{2, 4, 6, 8, 10}
		if !slices.Equal(p.history[marker], want) {
			t.Errorf("session %s request sizes = %v, want %v", marker, p.history[marker], want)
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunShutdownCancelsInFlightTurn(t *testing.T) {
	b := bus.NewMessageBus(4)
	started := make(chan struct{})
	p := &scripted{steps: []step{func(ctx context.Context, _ providers.ChatRequest) (*providers.ChatResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	l, sessions, _ := newTestLoop(t, p, nil, b)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	if err := b.PublishInbound(ctx, bus.InboundMessage{Channel: "test", ChatID: "1", Content: "slow"}); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if b.OutboundLen() != 0 {
		t.Error("canceled turn published a reply")
	}
	if sess, _ := sessions.GetOrCreate("test:1"); len(sess.Messages) != 0 {
		t.Error("canceled turn persisted")
	}
}

func TestRunPublishesErrorReply(t *testing.T) {
	b := bus.NewMessageBus(4)
	p := &scripted{steps: []step{fail(errors.New("provider down"))}}
	l, _, _ := newTestLoop(t, p, nil, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	meta := map[string]any{"thread": "t1"}
	if err := b.PublishInbound(ctx, bus.InboundMessage{Channel: "test", ChatID: "1", Content: "hi", Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	out, err := consumeWithTimeout(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "provider down") || out.Metadata["thread"] != "t1" {
		t.Errorf("outbound = %+v", out)
	}
}

func TestNewLoopValidates(t *testing.T) {
	if _, err := NewLoop(LoopConfig{}); err == nil {
		t.Error("NewLoop without provider succeeded")
	}
	l, _, _ := newTestLoop(t, &scripted{steps: []step{reply("x")}}, nil, nil)
	if err := l.Run(context.Background()); err == nil {
		t.Error("Run without bus succeeded")
	}
}

func consumeWithTimeout(b *bus.MessageBus) (bus.OutboundMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.ConsumeOutbound(ctx)
}

// summarize renders messages as role:content, role:[tool names] or
// tool:call-id for compact comparisons.
func summarize(msgs []providers.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		switch {
		case len(m.ToolCalls) > 0:
			var names []string
			for _, c := range m.ToolCalls {
				names = append(names, c.Name)
			}
			out[i] = fmt.Sprintf("%s:[%s]", m.Role, strings.Join(names, ","))
		case m.Role == providers.RoleTool:
			out[i] = "tool:" + m.ToolCallID
		default:
			out[i] = fmt.Sprintf("%s:%s", m.Role, m.Content)
		}
	}
	return out
}
