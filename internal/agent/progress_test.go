package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/providers"
)

func TestToolHint(t *testing.T) {
	long := strings.Repeat("x", 60)
	tests := []struct {
		name, args, want string
	}{
		{"read_file", `{"path":"notes/todo.md"}`, "read notes/todo.md"},
		{"read_file", `{"path":"/very/long/path/to/some/deeply/nested/project/notes/todo.md"}`, "read …/deeply/nested/project/notes/todo.md"},
		{"write_file", `{"path":"` + long + `.txt"}`, "write " + strings.Repeat("x", 39) + "…"},
		{"edit_file", `{"path":"a.go","old_text":"x"}`, "edit a.go"},
		{"list_dir", `{"path":"."}`, "ls ."},
		{"exec", `{"command":"go test\n  ./..."}`, "$ go test ./..."},
		{"exec", `{"command":"` + long + `"}`, "$ " + strings.Repeat("x", 39) + "…"},
		{"web_fetch", `{"url":"https://example.com/a"}`, "fetch https://example.com/a"},
		{"web_search", `{"query":"go generics"}`, `search "go generics"`},
		{"read_file", `{}`, "read_file"},
		{"cron", `{"action":"list"}`, `cron("list")`},
		{"cron", `{"z":"last","a":"first"}`, `cron("first")`},
		{"cron", `{"n":3}`, "cron"},
		{"mcp_github__list_issues", `{"repo":"a/b"}`, `github::list_issues("a/b")`},
		{"mcp_plain", `{}`, "mcp_plain"},
		{"broken", `not json`, "broken"},
	}
	for _, tt := range tests {
		got := ToolHint(providers.ToolCall{Name: tt.name, Arguments: []byte(tt.args)})
		if got != tt.want {
			t.Errorf("ToolHint(%s %s) = %q, want %q", tt.name, tt.args, got, tt.want)
		}
	}
}

func TestProgressHooksPublishToTheChat(t *testing.T) {
	b := bus.NewMessageBus(16)
	meta := map[string]any{"message_thread_id": int64(7)}
	h := newProgressHooks(b, bus.InboundMessage{Channel: "websocket", ChatID: "c1", Metadata: meta})
	var wg sync.WaitGroup
	for _, p := range []string{"a.txt", "b.txt"} {
		wg.Go(func() { h.OnToolStart(context.Background(), call("id", "read_file", `{"path":"`+p+`"}`)) })
	}
	wg.Wait()
	var hints []string
	for range 2 {
		out, err := consumeWithTimeout(b)
		if err != nil {
			t.Fatal(err)
		}
		if out.Channel != "websocket" || out.ChatID != "c1" || !bus.IsProgress(out) || out.Metadata["message_thread_id"] != int64(7) {
			t.Errorf("outbound = %+v", out)
		}
		hints = append(hints, out.Content)
	}
	if slices.Sort(hints); !slices.Equal(hints, []string{"read a.txt", "read b.txt"}) {
		t.Errorf("hints = %v", hints)
	}
	if _, marked := meta[bus.MetaProgress]; marked {
		t.Error("the inbound message's metadata was modified")
	}
}

func TestJoinHooks(t *testing.T) {
	if joinHooks(nil, nil) != nil {
		t.Error("joinHooks(nil, nil) != nil")
	}
	a := &recordingHooks{}
	if joinHooks(nil, a) != Hooks(a) {
		t.Error("a single hook is not returned as is")
	}
	c := &recordingHooks{}
	h := joinHooks(a, c)
	h.OnToolStart(context.Background(), call("1", "probe", `{}`))
	h.OnContent(context.Background(), "hi")
	for _, r := range []*recordingHooks{a, c} {
		if !slices.Equal(r.started, []string{"1"}) || !slices.Equal(r.content, []string{"hi"}) {
			t.Errorf("started %v, content %v", r.started, r.content)
		}
	}
}

func TestRunPublishesProgressBeforeTheReply(t *testing.T) {
	b := bus.NewMessageBus(8)
	tool := &fakeTool{name: "probe", readOnly: true, log: &execLog{}}
	p := &scripted{steps: []step{callTools(call("c1", "probe", `{"v":"x"}`)), reply("done")}}
	l, _, _ := newTestLoop(t, p, registry(t, tool), b)
	hooks := &recordingHooks{}
	l.cfg.Hooks = hooks
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	if err := b.PublishInbound(ctx, bus.InboundMessage{Channel: "test", ChatID: "1", Content: "go"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for range 2 {
		out, err := consumeWithTimeout(b)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out.Content)
		if bus.IsProgress(out) != (len(got) == 1) || out.ChatID != "1" {
			t.Errorf("outbound %d = %+v", len(got), out)
		}
	}
	if !slices.Equal(got, []string{`probe("x")`, "done"}) {
		t.Errorf("outbound = %v", got)
	}
	// LoopConfig.Hooks still sees the run.
	hooks.mu.Lock()
	if !slices.Equal(hooks.started, []string{"c1"}) {
		t.Errorf("configured hooks saw starts %v", hooks.started)
	}
	hooks.mu.Unlock()

	// Process callers deliver themselves: no progress on the bus.
	p.mu.Lock()
	p.steps = append(p.steps, callTools(call("c2", "probe", `{}`)), reply("direct"))
	p.mu.Unlock()
	if r, err := l.Process(ctx, bus.InboundMessage{Channel: "test", ChatID: "2", Content: "go"}); err != nil || r.Content != "direct" {
		t.Fatalf("Process = %+v, %v", r, err)
	}
	if n := b.OutboundLen(); n != 0 {
		t.Errorf("Process published %d outbound messages", n)
	}
}
