package providers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestOpenAIToolCallRoundTrip(t *testing.T) {
	var c capture
	srv := newServer(t, &c, 200, `{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"a.txt\"}"}},
					{"id": "call_2", "type": "function", "function": {"name": "exec", "arguments": "{\"cmd\": \"ls"}}
				]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 20, "completion_tokens": 5}
	}`)
	p := NewOpenAI(OpenAIConfig{APIKey: "sk-oa", BaseURL: srv.URL + "/v1", Model: "gpt-x", Retry: noRetry})

	req := ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleUser, Content: "read a.txt"},
		},
		Tools:     []ToolDef{{Name: "read_file", Description: "Read"}},
		MaxTokens: 64,
	}
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if c.path != "/v1/chat/completions" {
		t.Errorf("path = %q", c.path)
	}
	if got := c.header.Get("Authorization"); got != "Bearer sk-oa" {
		t.Errorf("Authorization = %q", got)
	}
	assertJSONEqual(t, c.body, `{
		"model": "gpt-x", "max_tokens": 64,
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": "read a.txt"}
		],
		"tools": [{"type": "function", "function": {"name": "read_file", "description": "Read",
			"parameters": {"type": "object", "properties": {}}}}]
	}`)

	if resp.FinishReason != FinishToolCalls || resp.Content != "" || len(resp.ToolCalls) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	assertJSONEqual(t, resp.ToolCalls[0].Arguments, `{"path": "a.txt"}`)
	// Malformed arguments are preserved as a JSON string.
	var raw string
	if err := json.Unmarshal(resp.ToolCalls[1].Arguments, &raw); err != nil || raw != `{"cmd": "ls` {
		t.Errorf("malformed args = %s", resp.ToolCalls[1].Arguments)
	}
	if resp.Usage != (Usage{InputTokens: 20, OutputTokens: 5}) {
		t.Errorf("usage = %+v", resp.Usage)
	}

	req.Messages = append(req.Messages,
		Message{Role: RoleAssistant, ToolCalls: resp.ToolCalls},
		Message{Role: RoleTool, ToolCallID: "call_1", Name: "read_file", Content: "hello"},
		Message{Role: RoleTool, ToolCallID: "call_2", Name: "exec", Content: "Error: bad args", IsError: true},
	)
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, c.body["messages"], `[
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": "read a.txt"},
		{"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"a.txt\"}"}},
			{"id": "call_2", "type": "function", "function": {"name": "exec", "arguments": "{\"cmd\": \"ls"}}
		]},
		{"role": "tool", "tool_call_id": "call_1", "name": "read_file", "content": "hello"},
		{"role": "tool", "tool_call_id": "call_2", "name": "exec", "content": "Error: bad args"}
	]`)
}

func TestOpenAIToolChoice(t *testing.T) {
	tools := []ToolDef{{Name: "echo"}}
	tests := []struct {
		name   string
		tools  []ToolDef
		choice ToolChoice
		want   string // "" when absent
	}{
		{"auto omitted", tools, ToolChoiceAuto, ""},
		{"none", tools, ToolChoiceNone, "none"},
		{"none without tools omitted", nil, ToolChoiceNone, ""},
	}
	p := NewOpenAI(OpenAIConfig{Model: "m"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := roundTrip(t, p.buildRequest(ChatRequest{
				Messages:   []Message{{Role: RoleUser, Content: "x"}},
				Tools:      tt.tools,
				ToolChoice: tt.choice,
			})).(map[string]any)
			got, ok := body["tool_choice"]
			if tt.want == "" && ok || tt.want != "" && got != tt.want {
				t.Errorf("tool_choice = %v (present %v), want %q", got, ok, tt.want)
			}
		})
	}
}

func TestOpenAIFinishReasons(t *testing.T) {
	tests := []struct {
		name, finish, toolCalls, want string
	}{
		{"stop", "stop", `null`, FinishStop},
		{"length", "length", `null`, FinishLength},
		{"length with tools", "length", `[{"id":"c","type":"function","function":{"name":"x","arguments":"{\"a"}}]`, FinishLength},
		{"stop with tools", "stop", `[{"id":"c","type":"function","function":{"name":"x","arguments":"{}"}}]`, FinishToolCalls},
		{"content_filter", "content_filter", `null`, FinishStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c capture
			srv := newServer(t, &c, 200, `{"choices":[{"message":{"content":"hi","tool_calls":`+tt.toolCalls+`},"finish_reason":"`+tt.finish+`"}]}`)
			p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
			resp, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.FinishReason != tt.want || resp.Content != "hi" {
				t.Errorf("resp = %+v, want finish %q", resp, tt.want)
			}
			if _, ok := c.body["max_tokens"]; ok {
				t.Error("max_tokens sent although unset")
			}
			if c.header.Get("Authorization") != "" {
				t.Error("Authorization sent without an API key")
			}
		})
	}
}

func TestOpenAIErrors(t *testing.T) {
	t.Run("error body", func(t *testing.T) {
		var c capture
		srv := newServer(t, &c, 401, `{"error":{"message":"Incorrect API key","type":"invalid_request_error"}}`)
		p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
		_, err := p.Chat(context.Background(), ChatRequest{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth || apiErr.Message != "invalid_request_error: Incorrect API key" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("empty choices", func(t *testing.T) {
		var c capture
		srv := newServer(t, &c, 200, `{"choices":[]}`)
		p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
		_, err := p.Chat(context.Background(), ChatRequest{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != KindServerError {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		var c capture
		srv := newServer(t, &c, 200, `not json`)
		p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: noRetry})
		_, err := p.Chat(context.Background(), ChatRequest{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != KindServerError {
			t.Errorf("err = %v", err)
		}
	})
}
