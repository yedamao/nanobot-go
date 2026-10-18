package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// DefaultOpenAIBaseURL is the OpenAI API root; /chat/completions is appended.
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

// OpenAIConfig configures an OpenAI-compatible provider.
type OpenAIConfig struct {
	// Name identifies the provider (e.g. "openai", "deepseek"); defaults to
	// "openai".
	Name   string
	APIKey string
	// BaseURL defaults to DefaultOpenAIBaseURL.
	BaseURL string
	// Model is used when a request does not name one.
	Model string
	// MaxTokens is used when a request does not set one; zero omits it.
	MaxTokens  int
	HTTPClient *http.Client
	Retry      RetryPolicy
}

// OpenAI talks to OpenAI-compatible chat completions endpoints.
type OpenAI struct {
	cfg OpenAIConfig
	ep  httpEndpoint
}

// NewOpenAI returns an OpenAI-compatible provider.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	cfg.Name = cmp.Or(cfg.Name, "openai")
	cfg.BaseURL = strings.TrimRight(cmp.Or(cfg.BaseURL, DefaultOpenAIBaseURL), "/")
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &OpenAI{
		cfg: cfg,
		ep:  httpEndpoint{provider: cfg.Name, client: client, retry: cfg.Retry, errorMessage: openAIErrorMessage},
	}
}

// Name implements Provider.
func (o *OpenAI) Name() string { return o.cfg.Name }

// Chat implements Provider.
func (o *OpenAI) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(o.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", o.cfg.Name, err)
	}
	h := http.Header{}
	if o.cfg.APIKey != "" {
		h.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	out, err := o.ep.post(ctx, o.cfg.BaseURL+"/chat/completions", h, body)
	if err != nil {
		return nil, err
	}
	var resp openAIResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, &APIError{Provider: o.cfg.Name, StatusCode: http.StatusOK, Kind: KindServerError, Message: "decode response", Err: err}
	}
	if len(resp.Choices) == 0 {
		// Some compatible servers return no choices under load; treat it as
		// a transient server error so fallbacks can take over.
		return nil, &APIError{Provider: o.cfg.Name, StatusCode: http.StatusOK, Kind: KindServerError, Message: "empty choices in response"}
	}
	return resp.toChatResponse(), nil
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Tools       []openAITool    `json:"tools,omitempty"`
	ToolChoice  string          `json:"tool_choice,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
}

type openAIMessage struct {
	Role string `json:"role"`
	// Content is a pointer so assistant tool-call messages can send null.
	Content    *string          `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content   *string          `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (o *OpenAI) buildRequest(req ChatRequest) openAIRequest {
	out := openAIRequest{
		Model:       cmp.Or(req.Model, o.cfg.Model),
		MaxTokens:   cmp.Or(req.MaxTokens, o.cfg.MaxTokens),
		Temperature: req.Temperature,
		Messages:    make([]openAIMessage, 0, len(req.Messages)),
	}
	for _, m := range req.Messages {
		msg := openAIMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		if m.Role == RoleTool {
			msg.Name = m.Name
		}
		if m.Content != "" || len(m.ToolCalls) == 0 {
			msg.Content = &m.Content
		}
		for _, tc := range m.ToolCalls {
			var wire openAIToolCall
			wire.ID, wire.Type = tc.ID, "function"
			wire.Function.Name = tc.Name
			wire.Function.Arguments = argumentString(tc.Arguments)
			msg.ToolCalls = append(msg.ToolCalls, wire)
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range req.Tools {
		var wire openAITool
		wire.Type = "function"
		wire.Function.Name = t.Name
		wire.Function.Description = t.Description
		wire.Function.Parameters = t.Parameters
		if wire.Function.Parameters == nil {
			wire.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out.Tools = append(out.Tools, wire)
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = string(req.ToolChoice)
	}
	return out
}

// argumentString is the inverse of rawArguments: a JSON string holding
// malformed text is sent back as that text.
func argumentString(args json.RawMessage) string {
	if len(args) == 0 {
		return "{}"
	}
	var s string
	if json.Unmarshal(args, &s) == nil {
		return s
	}
	return string(args)
}

func (r *openAIResponse) toChatResponse() *ChatResponse {
	c := r.Choices[0]
	out := &ChatResponse{Usage: Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens}}
	if c.Message.Content != nil {
		out.Content = *c.Message.Content
	}
	for _, tc := range c.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: rawArguments(tc.Function.Arguments)})
	}
	switch c.FinishReason {
	case "length":
		out.FinishReason = FinishLength
	case "tool_calls", "function_call":
		out.FinishReason = FinishToolCalls
	default:
		out.FinishReason = FinishStop
		// Some compatible servers report "stop" alongside tool calls.
		if len(out.ToolCalls) > 0 {
			out.FinishReason = FinishToolCalls
		}
	}
	return out
}

func openAIErrorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
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
