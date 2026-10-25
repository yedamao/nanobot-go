package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// streamAnthropic runs a streamed chat and returns the deltas it saw.
func streamAnthropic(t *testing.T, p *Anthropic) (*ChatResponse, []string, error) {
	t.Helper()
	var deltas []string
	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		OnDelta:  func(d string) { deltas = append(deltas, d) },
	})
	return resp, deltas, err
}

// sseEvent is a named Anthropic event and its blank-line terminator.
func sseEvent(name, data string) []string {
	return []string{"event: " + name, "data: " + data, ""}
}

func sseEvents(events ...[]string) []string { return slices.Concat(events...) }

const (
	anthropicMessageStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","stop_reason":null,"usage":{"input_tokens":12,"output_tokens":1}}}`
	anthropicMessageStop  = `{"type":"message_stop"}`
)

func TestAnthropicStreamText(t *testing.T) {
	srv := newSSEServer(t, sseLines(sseEvents(
		sseEvent("message_start", anthropicMessageStart),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		sseEvent("ping", `{"type":"ping"}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`),
		// Data-only, with no space after the colon.
		[]string{`data:{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo!"}}`, ""},
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":6}}`),
		sseEvent("message_stop", anthropicMessageStop),
	)...))
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Model: "m", Retry: noRetry})
	resp, deltas, err := streamAnthropic(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deltas, []string{"Hel", "lo!"}) {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.Content != "Hello!" || resp.FinishReason != FinishStop || len(resp.ToolCalls) != 0 {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Usage != (Usage{InputTokens: 12, OutputTokens: 6}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if srv.body["stream"] != true {
		t.Errorf("stream = %v", srv.body["stream"])
	}
}

func TestAnthropicStreamThinking(t *testing.T) {
	srv := newSSEServer(t, sseLines(sseEvents(
		sseEvent("message_start", anthropicMessageStart),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think."}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi!"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`),
		sseEvent("message_stop", anthropicMessageStop),
	)...))
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Model: "m", Retry: noRetry})
	var deltas, thinking []string
	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages:   []Message{{Role: RoleUser, Content: "hi"}},
		OnDelta:    func(d string) { deltas = append(deltas, d) },
		OnThinking: func(d string) { thinking = append(thinking, d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(thinking, []string{"Let me ", "think."}) {
		t.Errorf("thinking = %q", thinking)
	}
	if !slices.Equal(deltas, []string{"Hi!"}) || resp.Content != "Hi!" {
		t.Errorf("deltas = %q, content = %q", deltas, resp.Content)
	}
}

func TestAnthropicWithoutOnDeltaDoesNotStream(t *testing.T) {
	var c capture
	srv := newServer(t, &c, 200, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`)
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
	if _, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.body["stream"]; ok {
		t.Error("stream sent without OnDelta")
	}
}

func TestAnthropicStreamToolUse(t *testing.T) {
	srv := newSSEServer(t, sseLines(sseEvents(
		sseEvent("message_start", anthropicMessageStart),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look."}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_a","name":"read_file","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" \"a.txt\"}"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		// A tool without arguments gets {}.
		sseEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_b","name":"list","input":{}}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":2}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}`),
		sseEvent("message_stop", anthropicMessageStop),
	)...))
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
	resp, deltas, err := streamAnthropic(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deltas, []string{"Let me look."}) || resp.Content != "Let me look." {
		t.Errorf("deltas = %q, content = %q", deltas, resp.Content)
	}
	if resp.FinishReason != FinishToolCalls || len(resp.ToolCalls) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	a, b := resp.ToolCalls[0], resp.ToolCalls[1]
	if a.ID != "toolu_a" || a.Name != "read_file" || b.ID != "toolu_b" || b.Name != "list" {
		t.Errorf("calls = %+v", resp.ToolCalls)
	}
	assertJSONEqual(t, a.Arguments, `{"path":"a.txt"}`)
	assertJSONEqual(t, b.Arguments, `{}`)
	if resp.Usage != (Usage{InputTokens: 12, OutputTokens: 30}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestAnthropicStreamFinishReasons(t *testing.T) {
	toolStart := sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"x","input":{}}}`)
	textStart := sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"cut"}}`)
	tests := []struct {
		name  string
		block []string
		stop  string
		want  string
	}{
		{"max tokens", textStart, "max_tokens", FinishLength},
		{"end turn with tool", toolStart, "end_turn", FinishToolCalls},
		{"stop sequence", textStart, "stop_sequence", FinishStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newSSEServer(t, sseLines(sseEvents(
				sseEvent("message_start", anthropicMessageStart),
				tt.block,
				sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+tt.stop+`"},"usage":{"output_tokens":2}}`),
				sseEvent("message_stop", anthropicMessageStop),
			)...))
			p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
			resp, _, err := streamAnthropic(t, p)
			if err != nil || resp.FinishReason != tt.want {
				t.Fatalf("resp = %+v, err = %v", resp, err)
			}
		})
	}
}

func TestAnthropicStreamIgnoredByServer(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\n  \"content\": [{\"type\": \"text\", \"text\": \"whole\"}],\n  \"stop_reason\": \"end_turn\",\n  \"usage\": {\"input_tokens\": 1, \"output_tokens\": 1}\n}\n")
	})
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
	resp, deltas, err := streamAnthropic(t, p)
	if err != nil || resp.Content != "whole" || !slices.Equal(deltas, []string{"whole"}) || resp.Usage.InputTokens != 1 {
		t.Fatalf("resp = %+v, deltas = %q, err = %v", resp, deltas, err)
	}
}

func TestAnthropicStreamErrors(t *testing.T) {
	text := sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`)
	tests := []struct {
		name  string
		lines []string
		kind  Kind
		msg   string
	}{
		{"cut off", sseEvents(sseEvent("message_start", anthropicMessageStart), text), KindConnection, "stream ended"},
		{"overloaded", sseEvents(text, sseEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)), KindServerError, "overloaded_error: Overloaded"},
		{"rate limited", sseEvent("error", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`), KindRateLimit, "slow down"},
		{"bad event", []string{`data: {"type":`}, KindServerError, "decode stream event"},
		{"empty body", nil, KindServerError, "decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newSSEServer(t, sseLines(tt.lines...))
			// Retries are enabled: a failure after the reply started
			// must not be retried, or deltas would repeat.
			p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: RetryPolicy{
				Delays: []time.Duration{time.Millisecond},
				Sleep:  func(context.Context, time.Duration) error { return nil },
			}})
			_, _, err := streamAnthropic(t, p)
			var e *APIError
			if !errors.As(err, &e) || e.Kind != tt.kind || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("err = %v, want %s containing %q", err, tt.kind, tt.msg)
			}
			if n := srv.calls.Load(); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	}
}

func TestAnthropicStreamRetriesBeforeTheReply(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.WriteHeader(529) // overloaded
			return
		}
		writeSSE(w, sseEvents(
			sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			sseEvent("message_stop", anthropicMessageStop),
		)...)
	})
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: RetryPolicy{
		Delays: []time.Duration{time.Millisecond},
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}})
	resp, deltas, err := streamAnthropic(t, p)
	if err != nil || resp.Content != "ok" || !slices.Equal(deltas, []string{"ok"}) || srv.calls.Load() != 2 {
		t.Fatalf("resp = %+v, deltas = %q, err = %v, calls = %d", resp, deltas, err, srv.calls.Load())
	}
}

func TestAnthropicStreamCancel(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		writeSSE(w, sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}`)...)
		<-r.Context().Done() // a stalled stream, until the client hangs up
	})
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.Chat(ctx, ChatRequest{
			Messages: []Message{{Role: RoleUser, Content: "hi"}},
			OnDelta:  func(string) { cancel() },
		})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Chat did not return after cancel")
	}
}
