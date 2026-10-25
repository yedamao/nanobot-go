package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// chatStream posts a "stream": true request and assembles the response
// from its server-sent events, passing each piece of text to onDelta and
// each piece of thinking to onThinking (when non-nil) as it arrives.
// Canceling ctx aborts the stream. Only failures before the first event
// are retried (see httpEndpoint.stream).
func (a *Anthropic) chatStream(ctx context.Context, url string, h http.Header, body []byte, onDelta, onThinking func(string)) (*ChatResponse, error) {
	var acc anthropicStream
	err := a.ep.stream(ctx, url, h, body, func(r io.Reader) error {
		acc = anthropicStream{onDelta: onDelta, onThinking: onThinking}
		return acc.read(r)
	})
	if err != nil {
		return nil, err
	}
	return acc.response(), nil
}

// anthropicEvent is one Messages stream event. Which fields are set
// depends on Type.
type anthropicEvent struct {
	Type string `json:"type"`
	// message_start
	Message *struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	// content_block_start, content_block_delta
	Index        int             `json:"index"`
	ContentBlock *anthropicBlock `json:"content_block"`
	// content_block_delta, message_delta
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	// message_delta
	Usage *anthropicUsage `json:"usage"`
	// error
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// anthropicStream assembles a streamed response.
type anthropicStream struct {
	onDelta    func(string)
	onThinking func(string) // may be nil

	content strings.Builder
	calls   []*streamToolCall // tool_use blocks, by content block index
	stop    string
	usage   Usage
	done    bool          // message_stop arrived
	whole   *ChatResponse // set when the server did not stream
}

// read consumes the event stream until message_stop or EOF. Events are
// told apart by the "type" in their data, so "event:" lines are not
// needed; comments and other fields are skipped. A server that ignored
// "stream" and sent a plain JSON response is accepted too.
func (s *anthropicStream) read(r io.Reader) error {
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
			if !events && field != "event" && len(plain) < maxResponseBytes {
				plain = append(append(plain, line...), '\n')
			}
			continue
		}
		events = true
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if err := s.event([]byte(value)); err != nil {
			return err
		}
		if s.done {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return transportError("anthropic", err)
	}
	if !events {
		return s.plain(plain)
	}
	return &APIError{Provider: "anthropic", StatusCode: http.StatusOK, Kind: KindConnection, Message: "stream ended before the response was complete"}
}

// plain takes a non-streamed response body, passing its text on as one
// delta.
func (s *anthropicStream) plain(body []byte) error {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return &APIError{Provider: "anthropic", StatusCode: http.StatusOK, Kind: KindServerError, Message: "decode response", Err: err}
	}
	s.whole = resp.toChatResponse()
	if s.whole.Content != "" {
		s.onDelta(s.whole.Content)
	}
	return nil
}

func (s *anthropicStream) event(data []byte) error {
	var e anthropicEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return &APIError{Provider: "anthropic", StatusCode: http.StatusOK, Kind: KindServerError, Message: "decode stream event", Err: err}
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.usage = Usage{InputTokens: e.Message.Usage.InputTokens, OutputTokens: e.Message.Usage.OutputTokens}
		}
	case "content_block_start":
		if b := e.ContentBlock; b != nil {
			switch b.Type {
			case "text":
				s.text(b.Text)
			case "tool_use":
				// The start carries an empty input; the arguments follow
				// as input_json_delta pieces.
				s.calls = append(s.calls, &streamToolCall{index: e.Index, id: b.ID, name: b.Name})
			}
		}
	case "content_block_delta":
		if e.Delta == nil {
			return nil
		}
		switch e.Delta.Type {
		case "text_delta":
			s.text(e.Delta.Text)
		case "input_json_delta":
			if c := s.toolCall(e.Index); c != nil {
				c.arguments.WriteString(e.Delta.PartialJSON)
			}
		case "thinking_delta":
			// Thinking is passed on but kept out of the reply.
			if e.Delta.Thinking != "" && s.onThinking != nil {
				s.onThinking(e.Delta.Thinking)
			}
		}
		// signature_delta is not exposed.
	case "message_delta":
		if e.Delta != nil && e.Delta.StopReason != "" {
			s.stop = e.Delta.StopReason
		}
		if u := e.Usage; u != nil {
			// The counts are cumulative.
			if u.InputTokens > 0 {
				s.usage.InputTokens = u.InputTokens
			}
			s.usage.OutputTokens = u.OutputTokens
		}
	case "message_stop":
		s.done = true
	case "error":
		return s.streamError(e)
	}
	// ping and unknown events are ignored.
	return nil
}

func (s *anthropicStream) text(t string) {
	if t == "" {
		return
	}
	s.content.WriteString(t)
	s.onDelta(t)
}

// toolCall returns the tool_use block at content block index i, or nil.
func (s *anthropicStream) toolCall(i int) *streamToolCall {
	for _, c := range s.calls {
		if c.index == i {
			return c
		}
	}
	return nil
}

// streamError reports an error event, which arrives after the 200 status
// (overloaded_error mid-stream, for example).
func (s *anthropicStream) streamError(e anthropicEvent) error {
	msg, typ := "unknown error", ""
	if e.Error != nil {
		msg, typ = e.Error.Message, e.Error.Type
		if typ != "" {
			msg = typ + ": " + msg
		}
	}
	kind := KindServerError
	switch typ {
	case "rate_limit_error":
		kind = KindRateLimit
	case "invalid_request_error", "request_too_large":
		kind = KindBadRequest
	case "authentication_error", "permission_error":
		kind = KindAuth
	}
	return &APIError{Provider: "anthropic", StatusCode: http.StatusOK, Kind: kind, Message: "stream error: " + msg}
}

func (s *anthropicStream) response() *ChatResponse {
	if s.whole != nil {
		return s.whole
	}
	// Build the non-streamed shape so both paths share toChatResponse.
	resp := anthropicResponse{StopReason: s.stop}
	resp.Usage.InputTokens, resp.Usage.OutputTokens = s.usage.InputTokens, s.usage.OutputTokens
	if s.content.Len() > 0 {
		resp.Content = append(resp.Content, anthropicBlock{Type: "text", Text: s.content.String()})
	}
	for _, c := range s.calls {
		resp.Content = append(resp.Content, anthropicBlock{Type: "tool_use", ID: c.id, Name: c.name, Input: rawArguments(c.arguments.String())})
	}
	return resp.toChatResponse()
}
