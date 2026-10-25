// Package providers defines a provider-neutral chat API and implements it
// for Anthropic's Messages API and OpenAI-compatible chat completions over
// net/http. It mirrors nanobot/providers: retry with backoff lives in each
// HTTP provider, and FallbackProvider fails over between providers.
package providers

import (
	"context"
	"encoding/json"
)

// Role is the author of a message.
type Role string

// Message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Normalized finish reasons reported in ChatResponse.FinishReason.
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"
	FinishError     = "error"
)

// ToolChoice controls whether the model may call the offered tools.
type ToolChoice string

// Tool choices.
const (
	// ToolChoiceAuto lets the model decide; the field is omitted on the wire.
	ToolChoiceAuto ToolChoice = ""
	// ToolChoiceNone keeps the tool definitions in the request but forbids
	// calling them. Use it instead of dropping Tools when the history holds
	// tool calls: Anthropic rejects tool_use/tool_result blocks in a request
	// that defines no tools.
	ToolChoiceNone ToolChoice = "none"
)

// Message is one entry of a conversation.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls are the calls requested by an assistant message.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool message to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name is the tool name on tool messages.
	Name string `json:"name,omitempty"`
	// IsError marks a tool message whose call failed. Anthropic receives it
	// as tool_result.is_error; OpenAI has no equivalent field, so the
	// content itself must describe the failure.
	IsError bool `json:"is_error,omitempty"`
	// Media lists the image files attached to a user message, as absolute
	// or workspace-relative paths. Sessions store only these paths;
	// ExpandMedia reads the files into Parts for each request.
	Media []string `json:"media,omitempty"`
	// Parts, when set, replaces Content on the wire with multimodal
	// parts (see ExpandMedia). It is never persisted.
	Parts []Part `json:"-"`
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is the JSON value the model sent. Providers store arguments
	// that are not valid JSON as a JSON string holding the raw text, so the
	// field always marshals.
	Arguments json.RawMessage `json:"arguments"`
}

// ToolDef describes a tool offered to the model.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest is a provider-neutral chat request.
type ChatRequest struct {
	// Model overrides the provider's default model when set.
	Model    string
	Messages []Message
	Tools    []ToolDef
	// ToolChoice is ToolChoiceAuto (the default) or ToolChoiceNone. It is
	// only sent when Tools is non-empty.
	ToolChoice ToolChoice
	// MaxTokens caps the output; zero uses the provider default.
	MaxTokens int
	// Temperature is omitted from the request when nil.
	Temperature *float64
	// OnDelta, when set, asks for a streamed response: it is called with
	// each piece of assistant text as it arrives, in order and on the
	// goroutine that called Chat, before Chat returns the assembled
	// response. The Anthropic and OpenAI-compatible providers both
	// stream; a provider that cannot ignores it and never calls it.
	OnDelta func(delta string)
	// OnThinking, when set alongside OnDelta, is called the same way with
	// each piece of the model's thinking as it streams: Anthropic
	// thinking_delta and OpenAI-compatible reasoning_content. Thinking is
	// never part of the response Content. Without it thinking is dropped.
	OnThinking func(delta string)
}

// ChatResponse is a provider-neutral chat response.
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	// FinishReason is one of FinishStop, FinishToolCalls, FinishLength or
	// FinishError.
	FinishReason string
	Usage        Usage
}

// Usage counts tokens for one or more requests.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Add returns the sum of u and o.
func (u Usage) Add(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens}
}

// Provider is a chat model backend.
type Provider interface {
	// Chat sends one request. Implementations retry transient failures
	// themselves and report final failures as *APIError (or a wrapped
	// context error when ctx ends).
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// Name identifies the provider in logs and errors.
	Name() string
}

// rawArguments converts a tool-call argument string from the wire into a
// RawMessage that always marshals: valid JSON is kept as-is, blank becomes
// {}, and anything else is wrapped in a JSON string.
func rawArguments(s string) json.RawMessage {
	if len(s) == 0 {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}
