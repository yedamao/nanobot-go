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
	return &OpenAI{
		cfg: cfg,
		ep:  httpEndpoint{provider: cfg.Name, client: clientOrDefault(cfg.HTTPClient), retry: cfg.Retry, errorMessage: openAIErrorMessage},
	}
}

// Name implements Provider.
func (o *OpenAI) Name() string { return o.cfg.Name }

// Chat implements Provider. With req.OnDelta set the response is streamed
// as server-sent events (see chatStream).
func (o *OpenAI) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	wire := o.buildRequest(req)
	if req.OnDelta != nil {
		wire.Stream = true
		wire.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", o.cfg.Name, err)
	}
	h := http.Header{}
	if o.cfg.APIKey != "" {
		h.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	url := o.cfg.BaseURL + "/chat/completions"
	if req.OnDelta != nil {
		return o.chatStream(ctx, url, h, body, req.OnDelta)
	}
	out, err := o.ep.post(ctx, url, h, body)
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
	Stream      bool            `json:"stream,omitempty"`
	// StreamOptions asks for a last chunk carrying the usage.
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role string `json:"role"`
	// Content is a *string, so assistant tool-call messages can send
	// null, or []openAIPart for a multimodal user message.
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

type openAIPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

// openAIParts maps multimodal parts to content parts, leaving out blank
// text.
func openAIParts(parts []Part) []openAIPart {
	out := make([]openAIPart, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.Image != nil:
			out = append(out, openAIPart{Type: "image_url", ImageURL: &openAIImageURL{URL: p.Image.DataURL()}})
		case hasText(p.Text):
			out = append(out, openAIPart{Type: "text", Text: p.Text})
		}
	}
	return out
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
		switch {
		case m.Role == RoleUser && len(m.Parts) > 0:
			msg.Content = openAIParts(m.Parts)
		case m.Content != "" || len(m.ToolCalls) == 0:
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
	out.FinishReason = openAIFinishReason(c.FinishReason, len(out.ToolCalls) > 0)
	return out
}

// openAIFinishReason normalizes a wire finish_reason.
func openAIFinishReason(reason string, hasToolCalls bool) string {
	switch reason {
	case "length":
		return FinishLength
	case "tool_calls", "function_call":
		return FinishToolCalls
	}
	// Some compatible servers report "stop" alongside tool calls.
	if hasToolCalls {
		return FinishToolCalls
	}
	return FinishStop
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
