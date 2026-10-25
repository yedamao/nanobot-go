package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// sseDone is the payload that ends an OpenAI event stream.
const sseDone = "[DONE]"

// chatStream posts a "stream": true request and assembles the response
// from its server-sent events, passing each piece of content to onDelta as
// it arrives. Canceling ctx aborts the stream. Only failures before the
// first event are retried (see httpEndpoint.stream).
func (o *OpenAI) chatStream(ctx context.Context, url string, h http.Header, body []byte, onDelta func(string)) (*ChatResponse, error) {
	var acc openAIStream
	err := o.ep.stream(ctx, url, h, body, func(r io.Reader) error {
		acc = openAIStream{provider: o.cfg.Name, onDelta: onDelta}
		return acc.read(r)
	})
	if err != nil {
		return nil, err
	}
	return acc.response(), nil
}

// openAIChunk is one chat.completion.chunk event.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				// Index is missing on some compatible servers.
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Error is how some servers report a failure after the 200 status.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// streamToolCall is a tool call being assembled from chunks.
type streamToolCall struct {
	index     int
	id, name  string
	arguments strings.Builder
}

// openAIStream assembles a streamed response.
type openAIStream struct {
	provider string
	onDelta  func(string)

	content strings.Builder
	calls   []*streamToolCall
	finish  string
	usage   Usage
	choices bool          // a chunk with a choice arrived
	whole   *ChatResponse // set when the server did not stream
}

// read consumes the event stream until [DONE] or EOF. It is lenient about
// framing: every "data:" line is taken as a whole event, comments (such as
// keep-alives) and other fields are skipped, and a stream that ends after
// a finish_reason without [DONE] is accepted. A server that ignored
// "stream" and sent a plain JSON response is accepted too.
func (s *openAIStream) read(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxResponseBytes)
	var (
		events bool
		plain  []byte // the body so far while no event has arrived
	)
	for sc.Scan() {
		line := sc.Text()
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			if !events && len(plain) < maxResponseBytes {
				plain = append(append(plain, line...), '\n')
			}
			continue
		}
		events = true
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if value == sseDone {
			if !s.choices {
				return s.emptyError()
			}
			return nil
		}
		if err := s.event([]byte(value)); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return transportError(s.provider, err)
	}
	if !events {
		return s.plain(plain)
	}
	if !s.choices {
		return s.emptyError()
	}
	if s.finish == "" {
		return &APIError{Provider: s.provider, StatusCode: http.StatusOK, Kind: KindConnection, Message: "stream ended before the response was complete"}
	}
	return nil
}

// plain takes a non-streamed response body, passing its content on as one
// delta.
func (s *openAIStream) plain(body []byte) error {
	var resp openAIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return &APIError{Provider: s.provider, StatusCode: http.StatusOK, Kind: KindServerError, Message: "decode response", Err: err}
	}
	if len(resp.Choices) == 0 {
		return s.emptyError()
	}
	s.whole = resp.toChatResponse()
	if s.whole.Content != "" {
		s.onDelta(s.whole.Content)
	}
	return nil
}

// emptyError reports a response without choices, which some compatible
// servers send under load; like Chat, it is a transient server error so
// fallbacks can take over.
func (s *openAIStream) emptyError() error {
	return &APIError{Provider: s.provider, StatusCode: http.StatusOK, Kind: KindServerError, Message: "empty choices in response"}
}

func (s *openAIStream) event(data []byte) error {
	var c openAIChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return &APIError{Provider: s.provider, StatusCode: http.StatusOK, Kind: KindServerError, Message: "decode stream event", Err: err}
	}
	if c.Error != nil {
		msg := c.Error.Message
		if c.Error.Type != "" {
			msg = c.Error.Type + ": " + msg
		}
		return &APIError{Provider: s.provider, StatusCode: http.StatusOK, Kind: KindServerError, Message: "stream error: " + msg}
	}
	if c.Usage != nil {
		s.usage = Usage{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens}
	}
	if len(c.Choices) == 0 {
		return nil // the usage chunk
	}
	s.choices = true
	ch := c.Choices[0]
	if d := ch.Delta.Content; d != "" {
		s.content.WriteString(d)
		s.onDelta(d)
	}
	for _, tc := range ch.Delta.ToolCalls {
		call := s.toolCall(tc.Index, tc.ID)
		if call.id == "" {
			call.id = tc.ID
		}
		// The name comes once; a server repeating it must not double it.
		if call.name == "" {
			call.name = tc.Function.Name
		}
		call.arguments.WriteString(tc.Function.Arguments)
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		s.finish = *ch.FinishReason
	}
	return nil
}

// toolCall returns the call a tool-call delta continues, adding it if new.
// Calls are matched by index; without one, by id, and a delta with
// neither continues the last call.
func (s *openAIStream) toolCall(index *int, id string) *streamToolCall {
	for _, c := range s.calls {
		if (index != nil && c.index == *index) || (index == nil && id != "" && c.id == id) {
			return c
		}
	}
	if index == nil && id == "" && len(s.calls) > 0 {
		return s.calls[len(s.calls)-1]
	}
	c := &streamToolCall{index: len(s.calls)}
	if index != nil {
		c.index = *index
	}
	s.calls = append(s.calls, c)
	return c
}

func (s *openAIStream) response() *ChatResponse {
	if s.whole != nil {
		return s.whole
	}
	out := &ChatResponse{Content: s.content.String(), Usage: s.usage}
	for _, c := range s.calls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: c.id, Name: c.name, Arguments: rawArguments(c.arguments.String())})
	}
	out.FinishReason = openAIFinishReason(s.finish, len(out.ToolCalls) > 0)
	return out
}
