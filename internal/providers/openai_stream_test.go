package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sseServer answers every request with status and the given lines, each
// flushed on its own. It records the last request body and counts calls.
type sseServer struct {
	*httptest.Server
	body  map[string]any
	calls atomic.Int32
}

func newSSEServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, call int)) *sseServer {
	t.Helper()
	s := &sseServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.calls.Add(1))
		raw, _ := io.ReadAll(r.Body)
		s.body = nil
		if err := json.Unmarshal(raw, &s.body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		handle(w, r, n)
	}))
	t.Cleanup(s.Close)
	return s
}

// writeSSE sends lines as an event stream.
func writeSSE(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	f := w.(http.Flusher)
	for _, l := range lines {
		io.WriteString(w, l+"\n")
		f.Flush()
	}
}

func sseLines(lines ...string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) { writeSSE(w, lines...) }
}

// streamChat runs a streamed chat and returns the deltas it saw.
func streamChat(t *testing.T, p *OpenAI) (*ChatResponse, []string, error) {
	t.Helper()
	var deltas []string
	resp, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		OnDelta:  func(d string) { deltas = append(deltas, d) },
	})
	return resp, deltas, err
}

func TestOpenAIStreamText(t *testing.T) {
	srv := newSSEServer(t, sseLines(
		": keep-alive",
		"",
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}],"usage":null}`,
		"",
		// No space after the colon, and a DeepSeek-style reasoning delta.
		`data:{"choices":[{"index":0,"delta":{"reasoning_content":"hmm","content":null}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"lo!"},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
		"",
		"data: [DONE]",
		"",
	))
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Model: "m", Retry: noRetry})
	resp, deltas, err := streamChat(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deltas, []string{"Hel", "lo!"}) {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.Content != "Hello!" || resp.FinishReason != FinishStop || len(resp.ToolCalls) != 0 {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Usage != (Usage{InputTokens: 9, OutputTokens: 2}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if srv.body["stream"] != true {
		t.Errorf("stream = %v", srv.body["stream"])
	}
	assertJSONEqual(t, srv.body["stream_options"], `{"include_usage": true}`)
}

func TestOpenAIStreamReasoning(t *testing.T) {
	srv := newSSEServer(t, sseLines(
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"Let me "}}]}`,
		"",
		`data: {"choices":[{"delta":{"reasoning_content":"think.","content":null}}]}`,
		"",
		`data: {"choices":[{"delta":{"reasoning_content":"","content":"Hi"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	))
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Model: "m", Retry: noRetry})
	var deltas, thinking []string
	resp, err := p.Chat(context.Background(), ChatRequest{
		OnDelta:    func(d string) { deltas = append(deltas, d) },
		OnThinking: func(d string) { thinking = append(thinking, d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(thinking, []string{"Let me ", "think."}) {
		t.Errorf("thinking = %q", thinking)
	}
	if !slices.Equal(deltas, []string{"Hi", "!"}) || resp.Content != "Hi!" {
		t.Errorf("deltas = %q, content = %q", deltas, resp.Content)
	}
}

func TestOpenAIWithoutOnDeltaDoesNotStream(t *testing.T) {
	var c capture
	srv := newServer(t, &c, 200, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
	if _, err := p.Chat(context.Background(), ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.body["stream"]; ok {
		t.Error("stream sent without OnDelta")
	}
	if _, ok := c.body["stream_options"]; ok {
		t.Error("stream_options sent without OnDelta")
	}
}

func TestOpenAIStreamToolCalls(t *testing.T) {
	srv := newSSEServer(t, sseLines(
		`data: {"choices":[{"delta":{"content":"Let me look."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"exec","arguments":"{\"command\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		// A server repeating the name must not double it.
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"exec","arguments":":\"ls\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
		"data: [DONE]",
	))
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
	resp, deltas, err := streamChat(t, p)
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
	if a.ID != "call_a" || a.Name != "read_file" || b.ID != "call_b" || b.Name != "exec" {
		t.Errorf("calls = %+v", resp.ToolCalls)
	}
	assertJSONEqual(t, a.Arguments, `{"path":"a.txt"}`)
	assertJSONEqual(t, b.Arguments, `{"command":"ls"}`)
	if resp.Usage != (Usage{InputTokens: 5, OutputTokens: 7}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestOpenAIStreamToolCallsWithoutIndex(t *testing.T) {
	srv := newSSEServer(t, sseLines(
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"x","function":{"name":"a","arguments":"{\"n\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"y","function":{"name":"b","arguments":"{}"}}]}}]}`,
		// finish_reason "stop" with tool calls, and no [DONE].
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	))
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
	resp, _, err := streamChat(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != FinishToolCalls || len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "x" || resp.ToolCalls[1].Name != "b" {
		t.Fatalf("resp = %+v", resp)
	}
	assertJSONEqual(t, resp.ToolCalls[0].Arguments, `{"n":1}`)
}

func TestOpenAIStreamLength(t *testing.T) {
	srv := newSSEServer(t, sseLines(
		`data: {"choices":[{"delta":{"content":"cut"},"finish_reason":"length"}]}`,
		"data: [DONE]",
	))
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
	if resp, _, err := streamChat(t, p); err != nil || resp.FinishReason != FinishLength || resp.Content != "cut" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

func TestOpenAIStreamIgnoredByServer(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\n  \"choices\": [{\"message\": {\"content\": \"whole\"}, \"finish_reason\": \"stop\"}],\n  \"usage\": {\"prompt_tokens\": 1, \"completion_tokens\": 1}\n}\n")
	})
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
	resp, deltas, err := streamChat(t, p)
	if err != nil || resp.Content != "whole" || !slices.Equal(deltas, []string{"whole"}) || resp.Usage.InputTokens != 1 {
		t.Fatalf("resp = %+v, deltas = %q, err = %v", resp, deltas, err)
	}
}

func TestOpenAIStreamErrors(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		kind  Kind
		msg   string
	}{
		{"cut off", []string{`data: {"choices":[{"delta":{"content":"par"}}]}`}, KindConnection, "stream ended"},
		{"error event", []string{`data: {"choices":[{"delta":{"content":"par"}}]}`, `data: {"error":{"message":"overloaded","type":"server_error"}}`}, KindServerError, "server_error: overloaded"},
		{"bad event", []string{`data: {"choices":`}, KindServerError, "decode stream event"},
		{"no choices", []string{`data: {"choices":[]}`, "data: [DONE]"}, KindServerError, "empty choices"},
		{"empty body", nil, KindServerError, "decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newSSEServer(t, sseLines(tt.lines...))
			// Retries are enabled: a failure after the reply started
			// must not be retried, or deltas would repeat.
			p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: RetryPolicy{
				Delays: []time.Duration{time.Millisecond},
				Sleep:  func(context.Context, time.Duration) error { return nil },
			}})
			_, _, err := streamChat(t, p)
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

func TestOpenAIStreamRetriesBeforeTheReply(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeSSE(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, "data: [DONE]")
	})
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: RetryPolicy{
		Delays: []time.Duration{time.Millisecond},
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}})
	resp, deltas, err := streamChat(t, p)
	if err != nil || resp.Content != "ok" || !slices.Equal(deltas, []string{"ok"}) || srv.calls.Load() != 2 {
		t.Fatalf("resp = %+v, deltas = %q, err = %v, calls = %d", resp, deltas, err, srv.calls.Load())
	}
}

func TestOpenAIStreamCancel(t *testing.T) {
	srv := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		writeSSE(w, `data: {"choices":[{"delta":{"content":"first"}}]}`)
		<-r.Context().Done() // a stalled stream, until the client hangs up
	})
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.Chat(ctx, ChatRequest{OnDelta: func(string) { cancel() }})
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
