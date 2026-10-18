package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// step produces one scripted provider reply.
type step func(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error)

// scripted is a fake provider that replays steps and records requests.
// After the script runs out it repeats the last step.
type scripted struct {
	mu    sync.Mutex
	steps []step
	reqs  []providers.ChatRequest
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Chat(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	s.mu.Lock()
	req.Messages = slices.Clone(req.Messages)
	s.reqs = append(s.reqs, req)
	st := s.steps[min(len(s.reqs), len(s.steps))-1]
	s.mu.Unlock()
	return st(ctx, req)
}

func reply(content string) step {
	return func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
		return &providers.ChatResponse{Content: content, FinishReason: providers.FinishStop, Usage: providers.Usage{InputTokens: 10, OutputTokens: 1}}, nil
	}
}

func truncated(content string, calls ...providers.ToolCall) step {
	return func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
		return &providers.ChatResponse{Content: content, ToolCalls: calls, FinishReason: providers.FinishLength}, nil
	}
}

func callTools(calls ...providers.ToolCall) step {
	return func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
		return &providers.ChatResponse{ToolCalls: calls, FinishReason: providers.FinishToolCalls, Usage: providers.Usage{InputTokens: 10, OutputTokens: 2}}, nil
	}
}

func fail(err error) step {
	return func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) { return nil, err }
}

func call(id, name, args string) providers.ToolCall {
	return providers.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// fakeTool is a configurable tool that logs executions.
type fakeTool struct {
	name     string
	readOnly bool
	log      *execLog
	run      func(ctx context.Context, args json.RawMessage) tools.Result
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return f.name }
func (f *fakeTool) Parameters() map[string]any {
	return tools.Object(map[string]any{"v": tools.String("value")})
}
func (f *fakeTool) ReadOnly() bool { return f.readOnly }
func (f *fakeTool) Execute(ctx context.Context, args json.RawMessage) tools.Result {
	a, _ := tools.DecodeArgs[struct{ V string }](args)
	if f.log != nil {
		f.log.add(f.name + ":" + a.V)
	}
	if f.run != nil {
		return f.run(ctx, args)
	}
	return tools.Text(f.name + " got " + a.V)
}

type execLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *execLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, s)
}

func (l *execLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

func registry(t *testing.T, ts ...tools.Tool) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tl := range ts {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// recordingHooks records hook calls.
type recordingHooks struct {
	mu         sync.Mutex
	iterations []int
	started    []string
	ended      []string
	content    []string
}

func (h *recordingHooks) BeforeIteration(_ context.Context, i int, _ []providers.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.iterations = append(h.iterations, i)
}

func (h *recordingHooks) OnToolStart(_ context.Context, c providers.ToolCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = append(h.started, c.ID)
}

func (h *recordingHooks) OnToolEnd(_ context.Context, c providers.ToolCall, _ tools.Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ended = append(h.ended, c.ID)
}

func (h *recordingHooks) OnContent(_ context.Context, s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.content = append(h.content, s)
}

func userMsg(s string) []providers.Message {
	return []providers.Message{{Role: providers.RoleSystem, Content: "sys"}, {Role: providers.RoleUser, Content: s}}
}

// assertValidHistory checks that every assistant tool call is followed by
// exactly one tool result before the next assistant message, that results
// answer known calls, and that the history marshals.
func assertValidHistory(t *testing.T, msgs []providers.Message) {
	t.Helper()
	if _, err := json.Marshal(msgs); err != nil {
		t.Fatalf("history does not marshal: %v", err)
	}
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case providers.RoleAssistant:
			if len(pending) > 0 {
				t.Fatalf("message %d: assistant message while tool calls %v unanswered", i, pending)
			}
			for _, c := range m.ToolCalls {
				if c.ID == "" {
					t.Fatalf("message %d: tool call without ID", i)
				}
				pending[c.ID] = true
			}
		case providers.RoleTool:
			if !pending[m.ToolCallID] {
				t.Fatalf("message %d: tool result for unknown or answered call %q", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		default:
			if len(pending) > 0 {
				t.Fatalf("message %d: %s message while tool calls %v unanswered", i, m.Role, pending)
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("unanswered tool calls at end: %v", pending)
	}
}

func toolResults(msgs []providers.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == providers.RoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestRunSimpleAnswer(t *testing.T) {
	p := &scripted{steps: []step{reply("Hello!")}}
	hooks := &recordingHooks{}
	res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("hi"), Model: "m", MaxTokens: 50, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "Hello!" || res.StopReason != StopCompleted || res.Iterations != 1 {
		t.Errorf("res = %+v", res)
	}
	if got := res.NewMessages(); len(got) != 1 || got[0].Role != providers.RoleAssistant || got[0].Content != "Hello!" {
		t.Errorf("new messages = %+v", got)
	}
	if len(res.Messages) != 3 {
		t.Errorf("messages = %d, want 3", len(res.Messages))
	}
	if res.Usage != (providers.Usage{InputTokens: 10, OutputTokens: 1}) {
		t.Errorf("usage = %+v", res.Usage)
	}
	if req := p.reqs[0]; req.Model != "m" || req.MaxTokens != 50 || req.Tools != nil {
		t.Errorf("request = %+v", req)
	}
	if !slices.Equal(hooks.content, []string{"Hello!"}) || !slices.Equal(hooks.iterations, []int{0}) {
		t.Errorf("hooks = %+v", hooks)
	}
}

func TestRunToolRoundTrip(t *testing.T) {
	log := &execLog{}
	reg := registry(t, &fakeTool{name: "echo", log: log})
	p := &scripted{steps: []step{
		callTools(call("c1", "echo", `{"v":"x"}`)),
		reply("done"),
	}}
	hooks := &recordingHooks{}
	input := userMsg("go")
	res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: input, Tools: reg, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "done" || res.Iterations != 2 || res.StopReason != StopCompleted {
		t.Errorf("res = %+v", res)
	}
	if len(input) != 2 {
		t.Error("input messages modified")
	}
	if !slices.Equal(log.get(), []string{"echo:x"}) {
		t.Errorf("executions = %v", log.get())
	}
	assertValidHistory(t, res.Messages)
	newMsgs := res.NewMessages()
	if len(newMsgs) != 3 {
		t.Fatalf("new messages = %+v", newMsgs)
	}
	if tm := newMsgs[1]; tm.Role != providers.RoleTool || tm.ToolCallID != "c1" || tm.Name != "echo" || tm.Content != "echo got x" || tm.IsError {
		t.Errorf("tool message = %+v", tm)
	}
	// The second request carries the tool result and the tool definitions.
	second := p.reqs[1]
	if len(second.Messages) != 4 || second.Messages[3].Role != providers.RoleTool {
		t.Errorf("second request messages = %+v", second.Messages)
	}
	if len(second.Tools) != 1 || second.Tools[0].Name != "echo" {
		t.Errorf("tools = %+v", second.Tools)
	}
	if !slices.Equal(hooks.started, []string{"c1"}) || !slices.Equal(hooks.ended, []string{"c1"}) {
		t.Errorf("hooks = %+v", hooks)
	}
	if res.Usage != (providers.Usage{InputTokens: 20, OutputTokens: 3}) {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestRunParallelReadOnlyPreservesOrder(t *testing.T) {
	log := &execLog{}
	// Each read-only call blocks until all three in its batch have started,
	// proving they run concurrently.
	var started sync.WaitGroup
	started.Add(3)
	allStarted := make(chan struct{})
	go func() { started.Wait(); close(allStarted) }()
	var once sync.Map
	slow := &fakeTool{name: "read", readOnly: true, log: log, run: func(_ context.Context, args json.RawMessage) tools.Result {
		a, _ := tools.DecodeArgs[struct{ V string }](args)
		if _, dup := once.LoadOrStore(a.V, true); !dup && a.V != "last" {
			started.Done()
			select {
			case <-allStarted:
			case <-time.After(5 * time.Second):
				return tools.Errorf("read-only calls did not run concurrently")
			}
		}
		// Finish in reverse order so ordering must come from the runner.
		if a.V == "a" {
			time.Sleep(20 * time.Millisecond)
		}
		return tools.Text("read " + a.V)
	}}
	write := &fakeTool{name: "write", log: log}
	reg := registry(t, slow, write)

	p := &scripted{steps: []step{
		callTools(
			call("1", "read", `{"v":"a"}`),
			call("2", "read", `{"v":"b"}`),
			call("3", "read", `{"v":"c"}`),
			call("4", "write", `{"v":"w"}`),
			call("5", "read", `{"v":"last"}`),
		),
		reply("ok"),
	}}
	res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go"), Tools: reg})
	if err != nil {
		t.Fatal(err)
	}
	assertValidHistory(t, res.Messages)
	want := []string{"read a", "read b", "read c", "write got w", "read last"}
	if got := toolResults(res.Messages); !slices.Equal(got, want) {
		t.Errorf("results = %q, want %q", got, want)
	}
	var ids []string
	for _, m := range res.NewMessages() {
		if m.Role == providers.RoleTool {
			ids = append(ids, m.ToolCallID)
		}
	}
	if !slices.Equal(ids, []string{"1", "2", "3", "4", "5"}) {
		t.Errorf("result ids = %v", ids)
	}
	// The write runs after the whole read batch and before the last read.
	entries := log.get()
	if len(entries) != 5 || entries[3] != "write:w" || entries[4] != "read:last" {
		t.Errorf("execution order = %v", entries)
	}
}

func TestRunLengthWithToolCallsDoesNotExecute(t *testing.T) {
	log := &execLog{}
	reg := registry(t, &fakeTool{name: "write", log: log})
	p := &scripted{steps: []step{
		truncated("Writing the file", call("c1", "write", `{"v":"partial`), call("c2", "write", `{"v":"ok"}`)),
		reply("I'll write it in smaller pieces."),
	}}
	hooks := &recordingHooks{}
	res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("write"), Tools: reg, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(log.get()); n != 0 {
		t.Fatalf("tools executed %d times under finish_reason=length", n)
	}
	assertValidHistory(t, res.Messages)
	newMsgs := res.NewMessages()
	if len(newMsgs) != 4 {
		t.Fatalf("new messages = %+v", newMsgs)
	}
	if a := newMsgs[0]; a.Content != "Writing the file" || len(a.ToolCalls) != 2 {
		t.Errorf("assistant = %+v", a)
	}
	for _, m := range newMsgs[1:3] {
		if m.Content != truncatedToolResult || !m.IsError {
			t.Errorf("tool result = %+v", m)
		}
	}
	if len(hooks.started) != 0 || !slices.Equal(hooks.ended, []string{"c1", "c2"}) {
		t.Errorf("hooks started=%v ended=%v", hooks.started, hooks.ended)
	}
	if res.FinalContent != "I'll write it in smaller pieces." || res.StopReason != StopCompleted {
		t.Errorf("res = %+v", res)
	}
	// The next request offered tools again and its history was valid.
	assertValidHistory(t, p.reqs[1].Messages)
}

func TestRunLengthTextContinuation(t *testing.T) {
	p := &scripted{steps: []step{
		truncated("Hello, wo"),
		truncated("rld! How "),
		reply("are you?"),
	}}
	res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("greet")})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalContent != "Hello, world! How are you?" || res.StopReason != StopCompleted || res.Iterations != 3 {
		t.Errorf("res = %+v", res)
	}
	newMsgs := res.NewMessages()
	roles := make([]providers.Role, len(newMsgs))
	for i, m := range newMsgs {
		roles[i] = m.Role
	}
	wantRoles := []providers.Role{providers.RoleAssistant, providers.RoleUser, providers.RoleAssistant, providers.RoleUser, providers.RoleAssistant}
	if !slices.Equal(roles, wantRoles) {
		t.Fatalf("roles = %v", roles)
	}
	nudge := p.reqs[2].Messages[len(p.reqs[2].Messages)-1]
	if nudge.Role != providers.RoleUser || !strings.Contains(nudge.Content, "cut off") || !strings.Contains(nudge.Content, "Hello, world! How ") {
		t.Errorf("nudge = %q", nudge.Content)
	}
}

func TestRunLengthRecoveriesBounded(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		p := &scripted{steps: []step{truncated("x")}}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go")})
		if err != nil {
			t.Fatal(err)
		}
		if res.Iterations != maxLengthRecoveries+1 || res.StopReason != StopTruncated || res.FinalContent != "xxxx" {
			t.Errorf("res = %+v", res)
		}
		if last := res.Messages[len(res.Messages)-1]; last.Role != providers.RoleAssistant {
			t.Errorf("last message = %+v", last)
		}
	})
	t.Run("tool calls", func(t *testing.T) {
		log := &execLog{}
		reg := registry(t, &fakeTool{name: "write", log: log})
		p := &scripted{steps: []step{truncated("", call("", "write", `{"v":`))}}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go"), Tools: reg})
		if err != nil {
			t.Fatal(err)
		}
		if res.Iterations != maxLengthRecoveries+1 || res.StopReason != StopTruncated || res.FinalContent != truncatedFinalMessage {
			t.Errorf("res = %+v", res)
		}
		if len(log.get()) != 0 {
			t.Error("truncated tool calls executed")
		}
		assertValidHistory(t, res.Messages)
	})
}

func TestRunMaxIterationsForcedFinal(t *testing.T) {
	reg := registry(t, &fakeTool{name: "echo", readOnly: true})
	loop := callTools(call("", "echo", `{"v":"again"}`))

	t.Run("final answer", func(t *testing.T) {
		p := &scripted{steps: []step{loop, loop, loop, func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Content: "Summary so far.", FinishReason: providers.FinishStop}, nil
		}}}
		hooks := &recordingHooks{}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go"), Tools: reg, MaxIterations: 3, Hooks: hooks})
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != StopMaxIterations || res.FinalContent != "Summary so far." || res.Iterations != 3 {
			t.Errorf("res = %+v", res)
		}
		if len(p.reqs) != 4 {
			t.Fatalf("requests = %d, want 4", len(p.reqs))
		}
		final := p.reqs[3]
		if final.Tools != nil {
			t.Error("forced final request offered tools")
		}
		if last := final.Messages[len(final.Messages)-1]; last.Role != providers.RoleUser || !strings.Contains(last.Content, "budget for this turn is exhausted") {
			t.Errorf("final prompt = %+v", last)
		}
		assertValidHistory(t, res.Messages)
		last := res.Messages[len(res.Messages)-1]
		if last.Role != providers.RoleAssistant || last.Content != "Summary so far." {
			t.Errorf("last message = %+v", last)
		}
		// The budget prompt is not persisted.
		for _, m := range res.NewMessages() {
			if m.Role == providers.RoleUser {
				t.Errorf("unexpected user message in history: %q", m.Content)
			}
		}
		// Generated tool call IDs are unique.
		seen := map[string]bool{}
		for _, m := range res.Messages {
			for _, c := range m.ToolCalls {
				if seen[c.ID] {
					t.Errorf("duplicate tool call ID %q", c.ID)
				}
				seen[c.ID] = true
			}
		}
		if !slices.Equal(hooks.content, []string{"Summary so far."}) {
			t.Errorf("content hooks = %v", hooks.content)
		}
	})

	t.Run("fallback message", func(t *testing.T) {
		p := &scripted{steps: []step{loop, loop, fail(errors.New("boom"))}}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go"), Tools: reg, MaxIterations: 2})
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != StopMaxIterations || !strings.Contains(res.FinalContent, "maximum number of tool call iterations (2)") {
			t.Errorf("res = %+v", res)
		}
		assertValidHistory(t, res.Messages)
	})

	t.Run("default limit", func(t *testing.T) {
		p := &scripted{steps: []step{loop}}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go"), Tools: reg})
		if err != nil {
			t.Fatal(err)
		}
		if res.Iterations != DefaultMaxIterations || len(p.reqs) != DefaultMaxIterations+1 || res.StopReason != StopMaxIterations {
			t.Errorf("iterations = %d, requests = %d, stop = %s", res.Iterations, len(p.reqs), res.StopReason)
		}
	})
}

func TestRunEmptyResponseRetry(t *testing.T) {
	t.Run("recovers", func(t *testing.T) {
		p := &scripted{steps: []step{reply(""), reply("  \n"), reply("answer")}}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go")})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalContent != "answer" || res.Iterations != 3 || len(res.NewMessages()) != 1 {
			t.Errorf("res = %+v", res)
		}
	})
	t.Run("gives up", func(t *testing.T) {
		p := &scripted{steps: []step{reply("")}}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go")})
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != StopEmpty || res.FinalContent != emptyFinalMessage || res.Iterations != maxEmptyRetries+1 {
			t.Errorf("res = %+v", res)
		}
	})
}

func TestRunToolErrors(t *testing.T) {
	log := &execLog{}
	reg := registry(t, &fakeTool{name: "echo", log: log})
	tests := []struct {
		name string
		call providers.ToolCall
		want string
	}{
		{"unknown tool", call("c1", "nope", `{}`), "Tool 'nope' not found"},
		{"malformed JSON", call("c1", "echo", `{"v": "x`), "not valid JSON"},
		{"JSON string that is not an object", call("c1", "echo", `"{\"v\": \"x"`), "must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &scripted{steps: []step{callTools(tt.call), reply("sorry")}}
			res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go"), Tools: reg})
			if err != nil {
				t.Fatal(err)
			}
			if res.FinalContent != "sorry" {
				t.Errorf("final = %q", res.FinalContent)
			}
			assertValidHistory(t, res.Messages)
			results := toolResults(res.Messages)
			if len(results) != 1 || !strings.HasPrefix(results[0], "Error: ") || !strings.Contains(results[0], tt.want) {
				t.Errorf("results = %q, want %q", results, tt.want)
			}
			if tm := res.NewMessages()[1]; tm.Role != providers.RoleTool || !tm.IsError {
				t.Errorf("tool message not flagged as error: %+v", tm)
			}
		})
	}
	if n := len(log.get()); n != 0 {
		t.Errorf("tool executed %d times on bad calls", n)
	}

	t.Run("no registry", func(t *testing.T) {
		p := &scripted{steps: []step{callTools(call("c1", "echo", `{}`)), reply("ok")}}
		res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go")})
		if err != nil {
			t.Fatal(err)
		}
		assertValidHistory(t, res.Messages)
		if r := toolResults(res.Messages); len(r) != 1 || !strings.Contains(r[0], "not found") {
			t.Errorf("results = %q", r)
		}
	})
}

func TestRunProviderError(t *testing.T) {
	boom := &providers.APIError{Provider: "x", Kind: providers.KindBadRequest, Message: "bad"}
	p := &scripted{steps: []step{fail(boom)}}
	res, err := NewRunner(p).Run(context.Background(), RunSpec{Messages: userMsg("go")})
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.StopReason != StopError || len(res.NewMessages()) != 0 {
		t.Errorf("res = %+v", res)
	}
}

func TestRunCancellation(t *testing.T) {
	t.Run("during tool", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reg := registry(t, &fakeTool{name: "slow", run: func(ctx context.Context, _ json.RawMessage) tools.Result {
			cancel()
			<-ctx.Done()
			return tools.Errorf("interrupted: %v", ctx.Err())
		}})
		p := &scripted{steps: []step{callTools(call("c1", "slow", `{}`), call("c2", "slow", `{}`)), reply("never")}}
		res, err := NewRunner(p).Run(ctx, RunSpec{Messages: userMsg("go"), Tools: reg})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if res.StopReason != StopCanceled || len(p.reqs) != 1 {
			t.Errorf("stop = %s, requests = %d", res.StopReason, len(p.reqs))
		}
		assertValidHistory(t, res.Messages)
	})
	t.Run("during request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		block := func(ctx context.Context, _ providers.ChatRequest) (*providers.ChatResponse, error) {
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		}
		p := &scripted{steps: []step{block}}
		res, err := NewRunner(p).Run(ctx, RunSpec{Messages: userMsg("go")})
		if !errors.Is(err, context.Canceled) || res.StopReason != StopCanceled {
			t.Fatalf("err = %v, res = %+v", err, res)
		}
	})
	t.Run("before start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &scripted{steps: []step{reply("x")}}
		_, err := NewRunner(p).Run(ctx, RunSpec{Messages: userMsg("go")})
		if !errors.Is(err, context.Canceled) || len(p.reqs) != 0 {
			t.Fatalf("err = %v, requests = %d", err, len(p.reqs))
		}
	})
}

func TestNopHooksImplementsHooks(t *testing.T) {
	var _ Hooks = NopHooks{}
}
