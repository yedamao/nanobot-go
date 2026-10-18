package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// capture records the last request a test server received.
type capture struct {
	path   string
	header http.Header
	body   map[string]any
}

func newServer(t *testing.T, c *capture, status int, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.path = r.URL.Path
		c.header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		c.body = nil
		if err := json.Unmarshal(raw, &c.body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// roundTrip decodes v through JSON so tests can compare against generic maps.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertJSONEqual(t *testing.T, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("bad want JSON: %v", err)
	}
	g, w := roundTrip(t, got), roundTrip(t, want)
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", gb, wb)
	}
}

var noRetry = RetryPolicy{Delays: []time.Duration{}}

func TestAnthropicToolCallRoundTrip(t *testing.T) {
	var c capture
	srv := newServer(t, &c, 200, `{
		"content": [
			{"type": "text", "text": "Let me look."},
			{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": {"path": "a.txt"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 12, "output_tokens": 7}
	}`)
	p := NewAnthropic(AnthropicConfig{APIKey: "sk-test", BaseURL: srv.URL + "/", Model: "claude-x", Retry: noRetry})

	temp := 0.5
	req := ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "read a.txt"},
		},
		Tools:       []ToolDef{{Name: "read_file", Description: "Read", Parameters: map[string]any{"type": "object"}}},
		MaxTokens:   100,
		Temperature: &temp,
	}
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if c.path != "/v1/messages" {
		t.Errorf("path = %q", c.path)
	}
	if got := c.header.Get("x-api-key"); got != "sk-test" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := c.header.Get("anthropic-version"); got != DefaultAnthropicVersion {
		t.Errorf("anthropic-version = %q", got)
	}
	assertJSONEqual(t, c.body, `{
		"model": "claude-x", "max_tokens": 100, "temperature": 0.5, "system": "be brief",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "read a.txt"}]}],
		"tools": [{"name": "read_file", "description": "Read", "input_schema": {"type": "object"}}]
	}`)

	if resp.FinishReason != FinishToolCalls || resp.Content != "Let me look." {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_1" || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	assertJSONEqual(t, resp.ToolCalls[0].Arguments, `{"path": "a.txt"}`)
	if resp.Usage != (Usage{InputTokens: 12, OutputTokens: 7}) {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// Send the tool call and two results back, followed by a user nudge.
	req.Messages = append(req.Messages,
		Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: append(resp.ToolCalls,
			ToolCall{ID: "toolu_2", Name: "broken", Arguments: json.RawMessage(`"{not json"`)})},
		Message{Role: RoleTool, ToolCallID: "toolu_1", Name: "read_file", Content: "hello"},
		Message{Role: RoleTool, ToolCallID: "toolu_2", Name: "broken", Content: "Error: bad args", IsError: true},
		Message{Role: RoleUser, Content: "go on"},
	)
	req.Temperature = nil
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, c.body["messages"], `[
		{"role": "user", "content": [{"type": "text", "text": "read a.txt"}]},
		{"role": "assistant", "content": [
			{"type": "text", "text": "Let me look."},
			{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": {"path": "a.txt"}},
			{"type": "tool_use", "id": "toolu_2", "name": "broken", "input": {}}
		]},
		{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "toolu_1", "content": "hello"},
			{"type": "tool_result", "tool_use_id": "toolu_2", "content": "Error: bad args", "is_error": true},
			{"type": "text", "text": "go on"}
		]}
	]`)
	if _, ok := c.body["temperature"]; ok {
		t.Error("temperature sent although unset")
	}
}

func TestAnthropicSkipsBlankText(t *testing.T) {
	p := NewAnthropic(AnthropicConfig{Model: "m"})
	req := p.buildRequest(ChatRequest{Messages: []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, Content: " \n", ToolCalls: []ToolCall{{ID: "t1", Name: "x", Arguments: json.RawMessage(`{}`)}}},
		{Role: RoleTool, ToolCallID: "t1", Content: "ok"},
		{Role: RoleUser, Content: "\t"},
		{Role: RoleAssistant, Content: "  "},
		{Role: RoleUser, Content: "next"},
	}})
	assertJSONEqual(t, req.Messages, `[
		{"role": "user", "content": [{"type": "text", "text": "hi"}]},
		{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "x", "input": {}}]},
		{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
			{"type": "text", "text": "next"}
		]}
	]`)
}

func TestAnthropicToolChoice(t *testing.T) {
	tools := []ToolDef{{Name: "echo"}}
	tests := []struct {
		name   string
		tools  []ToolDef
		choice ToolChoice
		want   string // tool_choice JSON, "" when absent
	}{
		{"auto omitted", tools, ToolChoiceAuto, ""},
		{"none", tools, ToolChoiceNone, `{"type":"none"}`},
		{"none without tools omitted", nil, ToolChoiceNone, ""},
	}
	p := NewAnthropic(AnthropicConfig{Model: "m"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := roundTrip(t, p.buildRequest(ChatRequest{
				Messages:   []Message{{Role: RoleUser, Content: "x"}},
				Tools:      tt.tools,
				ToolChoice: tt.choice,
			})).(map[string]any)
			got, ok := body["tool_choice"]
			if tt.want == "" {
				if ok {
					t.Errorf("tool_choice = %v, want absent", got)
				}
				return
			}
			assertJSONEqual(t, got, tt.want)
		})
	}
}

func TestAnthropicFinishReasons(t *testing.T) {
	tests := []struct {
		stop, content, want string
	}{
		{"end_turn", `[{"type":"text","text":"hi"}]`, FinishStop},
		{"stop_sequence", `[{"type":"text","text":"hi"}]`, FinishStop},
		{"max_tokens", `[{"type":"text","text":"hi"}]`, FinishLength},
		{"max_tokens", `[{"type":"tool_use","id":"t","name":"x","input":{}}]`, FinishLength},
		{"tool_use", `[{"type":"tool_use","id":"t","name":"x","input":{}}]`, FinishToolCalls},
	}
	for _, tt := range tests {
		t.Run(tt.stop, func(t *testing.T) {
			var c capture
			srv := newServer(t, &c, 200, `{"content":`+tt.content+`,"stop_reason":"`+tt.stop+`"}`)
			p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
			resp, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.FinishReason != tt.want {
				t.Errorf("finish = %q, want %q", resp.FinishReason, tt.want)
			}
			if got := c.body["max_tokens"]; got != float64(DefaultMaxTokens) {
				t.Errorf("max_tokens = %v, want default", got)
			}
		})
	}
}

func TestAnthropicErrorClassification(t *testing.T) {
	tests := []struct {
		status int
		kind   Kind
	}{
		{400, KindBadRequest},
		{401, KindAuth},
		{403, KindAuth},
		{404, KindBadRequest},
		{408, KindTimeout},
		{409, KindServerError},
		{429, KindRateLimit},
		{500, KindServerError},
		{529, KindServerError},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var c capture
			srv := newServer(t, &c, tt.status, `{"type":"error","error":{"type":"some_error","message":"nope"}}`)
			p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
			_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.StatusCode != tt.status || apiErr.Kind != tt.kind || apiErr.Message != "some_error: nope" {
				t.Errorf("err = %+v", apiErr)
			}
		})
	}
}
