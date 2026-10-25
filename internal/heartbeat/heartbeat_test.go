package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
)

const template = `# Heartbeat Tasks

<!--
This file is checked periodically.
- not a task: inside a comment
-->

## Active Tasks

<!-- Add your periodic tasks below this line -->
`

func TestHasActiveTasks(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          bool
	}{
		{"empty", "", false},
		{"template", template, false},
		{"task", template + "- check the build\n", true},
		{"plain line", "## Active Tasks\nwater the plants\n", true},
		{"heading case", "## active tasks (daily)\n- x\n", true},
		{"other section", "## Notes\n- not a task\n", false},
		{"before any section", "- a task outside every section\n## Active Tasks\n", false},
		{"section ends", "## Active Tasks\n## Done\n- old task\n", false},
		{"subheading stays", "## Active Tasks\n### Morning\n- check mail\n", true},
		{"subheading only", "## Active Tasks\n### Morning\n", false},
		{"one-line comment", "## Active Tasks\n<!-- - commented out -->\n", false},
		{"multi-line comment", "## Active Tasks\n<!--\n- commented out\n-->\n", false},
		{"after comment", "## Active Tasks\n<!--\nnote\n-->\n- real\n", true},
		{"crlf", "## Active Tasks\r\n\r\n- real\r\n", true},
	} {
		if got := HasActiveTasks(tt.content); got != tt.want {
			t.Errorf("%s: HasActiveTasks = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func infos(keys ...string) []session.Info {
	out := make([]session.Info, len(keys))
	for i, k := range keys {
		out[i] = session.Info{Key: k}
	}
	return out
}

func TestPickTarget(t *testing.T) {
	for _, tt := range []struct {
		name     string
		sessions []session.Info
		enabled  []string
		want     string // channel:chat, "" for none
	}{
		{"most recent", infos("telegram:1", "websocket:abc"), []string{"telegram", "websocket"}, "telegram:1"},
		{"skips cli and system", infos("cli:direct", "system:x", "heartbeat", "websocket:abc"), []string{"websocket", "cli", "system"}, "websocket:abc"},
		{"skips disabled channels", infos("weixin:u1", "telegram:1"), []string{"telegram"}, "telegram:1"},
		{"skips topics", infos("telegram:1:topic:9", "telegram:2"), []string{"telegram"}, "telegram:2"},
		{"skips empty chat", infos("telegram:", "telegram:3"), []string{"telegram"}, "telegram:3"},
		{"cli only", infos("cli:direct", "heartbeat"), []string{"telegram"}, ""},
		{"no channel running", infos("telegram:1"), []string{}, ""},
		{"nil allows any", infos("cli:direct", "fake:1"), nil, "fake:1"},
	} {
		ch, chat, ok := PickTarget(tt.sessions, tt.enabled)
		got := ""
		if ok {
			got = ch + ":" + chat
		}
		if got != tt.want {
			t.Errorf("%s: PickTarget = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// fakeProvider answers the agent turn with reply and the notification
// check with eval.
type fakeProvider struct {
	mu    sync.Mutex
	evals []providers.ChatRequest
	eval  func() (*providers.ChatResponse, error)
}

func (p *fakeProvider) Name() string { return "fake" }
func (p *fakeProvider) Chat(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	p.mu.Lock()
	p.evals = append(p.evals, req)
	p.mu.Unlock()
	return p.eval()
}

func notify(v any) func() (*providers.ChatResponse, error) {
	return func() (*providers.ChatResponse, error) {
		args, _ := json.Marshal(map[string]any{"should_notify": v, "reason": "test"})
		return &providers.ChatResponse{FinishReason: providers.FinishToolCalls, ToolCalls: []providers.ToolCall{
			{ID: "e1", Name: evaluateToolName, Arguments: args},
		}}, nil
	}
}

type harness struct {
	ws        string
	provider  *fakeProvider
	turns     []bus.InboundMessage
	delivered []bus.OutboundMessage
	recorded  map[string][]providers.Message
	reply     string
	runner    *Runner
}

func newHarness(t *testing.T, heartbeat string, sessions ...string) *harness {
	t.Helper()
	h := &harness{ws: t.TempDir(), provider: &fakeProvider{eval: notify(true)}, recorded: map[string][]providers.Message{}, reply: "The build is red."}
	if heartbeat != "" {
		if err := os.WriteFile(filepath.Join(h.ws, File), []byte(heartbeat), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.runner = New(Config{
		Workspace: h.ws,
		Provider:  h.provider,
		Sessions:  func() ([]session.Info, error) { return infos(sessions...), nil },
		Channels:  func() []string { return []string{"telegram", "websocket"} },
		Turn: func(_ context.Context, msg bus.InboundMessage) (string, error) {
			h.turns = append(h.turns, msg)
			return h.reply, nil
		},
		Record: func(_ context.Context, key string, msgs ...providers.Message) error {
			h.recorded[key] = append(h.recorded[key], msgs...)
			return nil
		},
		Deliver: func(_ context.Context, msg bus.OutboundMessage) error {
			h.delivered = append(h.delivered, msg)
			return nil
		},
	})
	return h
}

func TestRunDeliversWhenEvaluatorSaysSo(t *testing.T) {
	h := newHarness(t, template+"- check the build\n", "cli:direct", "telegram:42", "websocket:abc")
	res, err := h.runner.Run(context.Background())
	if err != nil || !res.Notified || res.Response != "The build is red." {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(h.turns) != 1 {
		t.Fatalf("turns = %+v", h.turns)
	}
	turn := h.turns[0]
	if turn.SessionKey() != SessionKey || turn.Channel != "telegram" || turn.ChatID != "42" || turn.SenderID != SenderID ||
		!strings.HasPrefix(turn.Content, preamble) || !strings.HasSuffix(turn.Content, "- check the build\n") {
		t.Errorf("turn = %+v", turn)
	}
	if len(h.delivered) != 1 || h.delivered[0].Channel != "telegram" || h.delivered[0].ChatID != "42" || h.delivered[0].Content != "The build is red." {
		t.Errorf("delivered = %+v", h.delivered)
	}
	if got := h.recorded["telegram:42"]; len(got) != 1 || got[0].Role != providers.RoleAssistant || got[0].Content != "The build is red." {
		t.Errorf("recorded = %+v", h.recorded)
	}
	// The check sees the task and the reply, with the evaluate tool.
	req := h.provider.evals[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != evaluateToolName || req.Messages[0].Content != DefaultEvaluatorPrompt ||
		!strings.Contains(req.Messages[1].Content, "## Original task\n"+turn.Content) ||
		!strings.HasSuffix(req.Messages[1].Content, "## Agent response\nThe build is red.") ||
		req.Temperature == nil || *req.Temperature != 0 {
		t.Errorf("evaluation request = %+v", req)
	}
}

func TestRunSilencedAndFailClosed(t *testing.T) {
	for _, tt := range []struct {
		name string
		eval func() (*providers.ChatResponse, error)
	}{
		{"evaluator says no", notify(false)},
		{"provider error", func() (*providers.ChatResponse, error) { return nil, errors.New("boom") }},
		{"no tool call", func() (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Content: "yes, notify", FinishReason: providers.FinishStop}, nil
		}},
		{"truncated", func() (*providers.ChatResponse, error) {
			r, _ := notify(true)()
			r.FinishReason = providers.FinishLength
			return r, nil
		}},
		{"not a boolean", notify("true")},
		{"bad arguments", func() (*providers.ChatResponse, error) {
			return &providers.ChatResponse{FinishReason: providers.FinishToolCalls, ToolCalls: []providers.ToolCall{
				{ID: "e1", Name: evaluateToolName, Arguments: json.RawMessage(`"oops"`)},
			}}, nil
		}},
	} {
		h := newHarness(t, "## Active Tasks\n- check\n", "telegram:42")
		h.provider.eval = tt.eval
		res, err := h.runner.Run(context.Background())
		if err != nil || res.Notified || len(h.delivered) != 0 || len(h.recorded) != 0 {
			t.Errorf("%s: Run = %+v, %v; delivered %+v", tt.name, res, err, h.delivered)
		}
		if len(h.turns) != 1 || len(h.provider.evals) != 1 {
			t.Errorf("%s: turns %d, evaluations %d", tt.name, len(h.turns), len(h.provider.evals))
		}
	}
}

func TestRunSkipsWithoutModelCalls(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		sessions      []string
	}{
		{"missing file", "", []string{"telegram:42"}},
		{"no active tasks", template, []string{"telegram:42"}},
		{"cli only", "## Active Tasks\n- check\n", []string{"cli:direct", "heartbeat"}},
		{"disabled channel", "## Active Tasks\n- check\n", []string{"weixin:u1"}},
	} {
		h := newHarness(t, tt.content, tt.sessions...)
		res, err := h.runner.Run(context.Background())
		if err != nil || res.Skipped == "" || len(h.turns) != 0 || len(h.provider.evals) != 0 || len(h.delivered) != 0 {
			t.Errorf("%s: Run = %+v, %v; turns %d, evaluations %d", tt.name, res, err, len(h.turns), len(h.provider.evals))
		}
	}
}

func TestRunEmptyReplyIsNotEvaluated(t *testing.T) {
	h := newHarness(t, "## Active Tasks\n- check\n", "telegram:42")
	h.reply = "  "
	if res, err := h.runner.Run(context.Background()); err != nil || res.Notified || len(h.provider.evals) != 0 {
		t.Errorf("Run = %+v, %v; evaluations %d", res, err, len(h.provider.evals))
	}
}

func TestEvaluatorPromptOverride(t *testing.T) {
	ws := t.TempDir()
	if got := EvaluatorPrompt(ws, nil); got != DefaultEvaluatorPrompt {
		t.Errorf("default = %q", got)
	}
	path := filepath.Join(ws, filepath.FromSlash(EvaluatorPromptFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(" \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := EvaluatorPrompt(ws, nil); got != DefaultEvaluatorPrompt {
		t.Errorf("blank override = %q", got)
	}
	if err := os.WriteFile(path, []byte("Only notify about failures.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := EvaluatorPrompt(ws, nil); got != "Only notify about failures." {
		t.Errorf("override = %q", got)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("é", maxEvaluatorPromptChars+10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := EvaluatorPrompt(ws, nil); len([]rune(got)) != maxEvaluatorPromptChars {
		t.Errorf("oversized override has %d chars", len([]rune(got)))
	}
}
