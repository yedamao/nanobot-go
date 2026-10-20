package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/bus"
)

// chatRequest is the part of an OpenAI chat completion request the API
// reads, plus nanobot's session_id. Other fields (temperature, tools, ...)
// are ignored: the agent's config decides them.
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    *bool         `json:"stream"`
	SessionID string        `json:"session_id"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentPart is one element of an array-valued message content.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// requestError is a client error with its HTTP status.
type requestError struct {
	status  int
	message string
}

func (e *requestError) Error() string { return e.message }

func badRequest(format string, args ...any) *requestError {
	return &requestError{status: http.StatusBadRequest, message: fmt.Sprintf(format, args...)}
}

// parsedRequest is a validated chat completion request.
type parsedRequest struct {
	text       string
	sessionKey string
	stream     bool
}

// parseChatRequest reads and validates the body of r.
func (s *Server) parseChatRequest(w http.ResponseWriter, r *http.Request) (*parsedRequest, *requestError) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			if strings.HasPrefix(mt, "multipart/") {
				return nil, badRequest("File uploads (multipart/form-data) are not supported; send a JSON body")
			}
			return nil, badRequest("Content-Type must be application/json")
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			return nil, &requestError{status: http.StatusRequestEntityTooLarge, message: fmt.Sprintf("Request body exceeds %d bytes", MaxBodyBytes)}
		}
		return nil, badRequest("Could not read the request body")
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		if te := (*json.UnmarshalTypeError)(nil); errors.As(err, &te) && te.Field != "" {
			return nil, badRequest("Invalid type for %s", te.Field)
		}
		return nil, badRequest("Invalid JSON body")
	}

	if req.Model != "" && req.Model != s.cfg.Model {
		return nil, badRequest("Only configured model '%s' is available", s.cfg.Model)
	}
	if len(req.Messages) == 0 {
		return nil, badRequest("messages must contain a user message")
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return nil, badRequest("The last message must have role 'user'")
	}
	text, rerr := messageText(last.Content)
	if rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(text) == "" {
		return nil, badRequest("The user message must not be empty")
	}

	key := DefaultSessionKey
	if req.SessionID != "" {
		if len(req.SessionID) > maxSessionID || strings.IndexFunc(req.SessionID, unicode.IsControl) >= 0 {
			return nil, badRequest("session_id must be at most %d characters without control characters", maxSessionID)
		}
		key = Channel + ":" + req.SessionID
	}
	return &parsedRequest{text: text, sessionKey: key, stream: req.Stream != nil && *req.Stream}, nil
}

// messageText returns the text of a message content: a string, or an
// array of parts whose text parts are joined with spaces (as nanobot
// does). Images are rejected: the agent here is text-only.
func messageText(raw json.RawMessage) (string, *requestError) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", badRequest("Invalid content format")
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image_url", "input_audio", "file":
			return "", badRequest("Content parts of type '%s' are not supported; send text only", p.Type)
		}
	}
	return strings.Join(texts, " "), nil
}

// handleChatCompletions runs one turn and answers with a chat.completion,
// or with chat.completion.chunk events when stream is true.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	req, rerr := s.parseChatRequest(w, r)
	if rerr != nil {
		writeError(w, rerr.status, rerr.message, errInvalidRequest)
		return
	}
	s.log.Debug("api turn", "session", req.sessionKey, "stream", req.stream)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	msg := bus.InboundMessage{
		Channel:            Channel,
		ChatID:             chatID,
		SenderID:           senderID,
		Content:            req.text,
		Timestamp:          time.Now(),
		SessionKeyOverride: req.sessionKey,
	}
	if req.stream {
		s.stream(ctx, w, msg)
		return
	}

	reply, err := s.cfg.Turn(ctx, msg)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, completion(s.cfg.Model, reply))
	case r.Context().Err() != nil:
		// The client is gone; nobody reads a response.
		s.log.Info("api turn canceled: client disconnected", "session", req.sessionKey)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		s.log.Warn("api turn timed out", "session", req.sessionKey, "timeout", s.cfg.Timeout)
		writeError(w, http.StatusGatewayTimeout, fmt.Sprintf("Request timed out after %s", s.cfg.Timeout), errServer)
	default:
		s.log.Error("api turn failed", "session", req.sessionKey, "error", err)
		writeError(w, http.StatusInternalServerError, "Internal server error", errServer)
	}
}

// stream answers with Server-Sent Events. The agent does not stream
// tokens, so the whole reply arrives as one content chunk once the turn
// is done; SSE comments keep the connection alive meanwhile. Like
// nanobot, a failed or timed-out turn ends the stream without the final
// chunk and [DONE].
func (s *Server) stream(ctx context.Context, w http.ResponseWriter, msg bus.InboundMessage) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	type outcome struct {
		reply agent.Reply
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		reply, err := s.cfg.Turn(ctx, msg)
		done <- outcome{reply, err}
	}()
	tick := time.NewTicker(s.keepAlive)
	defer tick.Stop()
	var res outcome
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-tick.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				// The client is gone: the request context is canceled,
				// and with it the turn.
				res = <-done
				break wait
			}
			_ = rc.Flush()
		}
	}
	if res.err != nil {
		s.log.Warn("api streaming turn failed", "session", msg.SessionKey(), "error", res.err)
		return
	}

	id := completionID()
	created := time.Now().Unix()
	var delta map[string]string
	if res.reply.Content != "" {
		delta = map[string]string{"role": "assistant", "content": res.reply.Content}
	}
	stop := "stop"
	for _, c := range []any{chunk(id, created, s.cfg.Model, delta, nil), chunk(id, created, s.cfg.Model, nil, &stop)} {
		data, _ := json.Marshal(c)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

// completion builds a chat.completion response.
func completion(model string, reply agent.Reply) map[string]any {
	u := reply.Usage
	return map[string]any{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": reply.Content},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     u.InputTokens,
			"completion_tokens": u.OutputTokens,
			"total_tokens":      u.InputTokens + u.OutputTokens,
		},
	}
}

// chunk builds a chat.completion.chunk event. A nil delta is sent as {}.
func chunk(id string, created int64, model string, delta map[string]string, finishReason *string) map[string]any {
	if delta == nil {
		delta = map[string]string{}
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	}
}

// completionID returns a nanobot-style "chatcmpl-<12 hex>" id.
func completionID() string { return "chatcmpl-" + randomHex(6) }
