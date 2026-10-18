package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Anthropic defaults.
const (
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	DefaultAnthropicVersion = "2023-06-01"
	// DefaultMaxTokens is used when a request does not set MaxTokens and the
	// API requires a value.
	DefaultMaxTokens = 8192
)

// AnthropicConfig configures an Anthropic provider.
type AnthropicConfig struct {
	APIKey string
	// BaseURL defaults to DefaultAnthropicBaseURL; /v1/messages is appended.
	BaseURL string
	// Version is the anthropic-version header; defaults to
	// DefaultAnthropicVersion.
	Version string
	// Model is used when a request does not name one.
	Model string
	// MaxTokens is used when a request does not set one; defaults to
	// DefaultMaxTokens.
	MaxTokens  int
	HTTPClient *http.Client
	Retry      RetryPolicy
}

// Anthropic talks to the Anthropic Messages API.
type Anthropic struct {
	cfg AnthropicConfig
	ep  httpEndpoint
}

// NewAnthropic returns an Anthropic provider.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	cfg.BaseURL = strings.TrimRight(cmp.Or(cfg.BaseURL, DefaultAnthropicBaseURL), "/")
	cfg.Version = cmp.Or(cfg.Version, DefaultAnthropicVersion)
	cfg.MaxTokens = cmp.Or(cfg.MaxTokens, DefaultMaxTokens)
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Anthropic{
		cfg: cfg,
		ep:  httpEndpoint{provider: "anthropic", client: client, retry: cfg.Retry, errorMessage: anthropicErrorMessage},
	}
}

// Name implements Provider.
func (a *Anthropic) Name() string { return "anthropic" }

// Chat implements Provider.
func (a *Anthropic) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(a.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode request: %w", err)
	}
	h := http.Header{}
	h.Set("x-api-key", a.cfg.APIKey)
	h.Set("anthropic-version", a.cfg.Version)
	out, err := a.ep.post(ctx, a.cfg.BaseURL+"/v1/messages", h, body)
	if err != nil {
		return nil, err
	}
	var resp anthropicResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, &APIError{Provider: "anthropic", StatusCode: http.StatusOK, Kind: KindServerError, Message: "decode response", Err: err}
	}
	return resp.toChatResponse(), nil
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  *anthropicChoice   `json:"tool_choice,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicChoice struct {
	Type string `json:"type"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicResponse struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) buildRequest(req ChatRequest) anthropicRequest {
	out := anthropicRequest{
		Model:       cmp.Or(req.Model, a.cfg.Model),
		MaxTokens:   cmp.Or(req.MaxTokens, a.cfg.MaxTokens),
		Temperature: req.Temperature,
	}
	var system []string
	for _, m := range req.Messages {
		var role string
		var blocks []anthropicBlock
		switch m.Role {
		case RoleSystem:
			if m.Content != "" {
				system = append(system, m.Content)
			}
			continue
		case RoleAssistant:
			role = "assistant"
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: objectInput(tc.Arguments)})
			}
		case RoleTool:
			role = "user"
			blocks = []anthropicBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content, IsError: m.IsError}}
		default:
			role = "user"
			if m.Content != "" { // empty text blocks are rejected
				blocks = []anthropicBlock{{Type: "text", Text: m.Content}}
			}
		}
		if len(blocks) == 0 {
			continue
		}
		// The API requires alternating roles: merge consecutive same-role
		// messages (e.g. several tool results, or tool results followed by
		// a user nudge) into one message.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			continue
		}
		out.Messages = append(out.Messages, anthropicMessage{Role: role, Content: blocks})
	}
	out.System = strings.Join(system, "\n\n")
	for _, t := range req.Tools {
		schema := t.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out.Tools = append(out.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if len(out.Tools) > 0 && req.ToolChoice != ToolChoiceAuto {
		out.ToolChoice = &anthropicChoice{Type: string(req.ToolChoice)}
	}
	return out
}

// objectInput returns args if it is a JSON object, else {}: tool_use input
// must be an object, and malformed arguments were already answered with an
// error result.
func objectInput(args json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) != nil || obj == nil {
		return json.RawMessage("{}")
	}
	return args
}

func (r *anthropicResponse) toChatResponse() *ChatResponse {
	out := &ChatResponse{Usage: Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}}
	var text strings.Builder
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	out.Content = text.String()
	switch r.StopReason {
	case "max_tokens":
		out.FinishReason = FinishLength
	case "tool_use":
		out.FinishReason = FinishToolCalls
	default:
		out.FinishReason = FinishStop
		if len(out.ToolCalls) > 0 {
			out.FinishReason = FinishToolCalls
		}
	}
	return out
}

func anthropicErrorMessage(body []byte) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Message == "" {
		return ""
	}
	if e.Error.Type != "" {
		return e.Error.Type + ": " + e.Error.Message
	}
	return e.Error.Message
}
