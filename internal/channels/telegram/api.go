package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// apiTimeout bounds every Bot API call except getUpdates.
	apiTimeout = 30 * time.Second
	// pollSlack is added to the long-poll timeout for the getUpdates
	// request, so the server answers before the client gives up.
	pollSlack = 15 * time.Second
	// maxResponseBytes caps a Bot API response body.
	maxResponseBytes = 8 << 20
	// redacted replaces the bot token wherever it could appear.
	redacted = "<redacted>"
)

// client calls the Telegram Bot API over HTTPS with JSON bodies. The bot
// token is part of every request URL (https://api.telegram.org/bot<token>/
// <method>), so the client keeps it out of every error it returns.
type client struct {
	base  string // API base without a trailing slash
	token string
	http  *http.Client
}

// apiError is a Bot API error response ({"ok": false, ...}).
type apiError struct {
	Method      string
	Code        int // error_code, usually the HTTP status
	Description string
	// RetryAfter is set on 429 Too Many Requests (flood control).
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return s
}

// envelope is every Bot API response.
type envelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call posts params as JSON to method and decodes the result into out
// (when non-nil). Errors never contain the token.
func (c *client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return c.redact(fmt.Errorf("telegram %s: %w", method, err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error quotes the full URL, token included.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = c.base + "/bot" + redacted + "/" + method
		}
		return c.redact(fmt.Errorf("telegram %s: %w", method, err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return c.redact(fmt.Errorf("telegram %s: read response: %w", method, err))
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d with an unreadable body", method, resp.StatusCode)
	}
	if !env.OK {
		e := &apiError{Method: method, Code: env.ErrorCode, Description: c.scrub(env.Description)}
		if e.Code == 0 {
			e.Code = resp.StatusCode
		}
		if env.Parameters != nil && env.Parameters.RetryAfter > 0 {
			e.RetryAfter = time.Duration(env.Parameters.RetryAfter) * time.Second
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

// scrub removes the token from s.
func (c *client) scrub(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, redacted)
}

// redact returns err with the token removed from its message. Only a
// context error stays in the chain (for errors.Is): the rest of the
// chain may quote the request URL, token included.
func (c *client) redact(err error) error {
	e := &redactedError{msg: c.scrub(err.Error())}
	switch {
	case errors.Is(err, context.Canceled):
		e.err = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		e.err = context.DeadlineExceeded
	}
	return e
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// Bot API types: only the fields this channel reads.

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type chat struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"` // private, group, supergroup, channel
	IsForum bool   `json:"is_forum"`
}

type entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"` // in UTF-16 code units
	Length int    `json:"length"`
	User   *user  `json:"user"`
}

type message struct {
	MessageID       int64    `json:"message_id"`
	MessageThreadID int64    `json:"message_thread_id"`
	IsTopicMessage  bool     `json:"is_topic_message"`
	From            *user    `json:"from"`
	Chat            chat     `json:"chat"`
	Text            string   `json:"text"`
	Caption         string   `json:"caption"`
	Entities        []entity `json:"entities"`
	CaptionEntities []entity `json:"caption_entities"`
	ReplyToMessage  *message `json:"reply_to_message"`
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

// replyParameters makes a message a reply (sendMessage's
// reply_parameters).
type replyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
}

type sendMessageParams struct {
	ChatID          int64            `json:"chat_id"`
	Text            string           `json:"text"`
	MessageThreadID int64            `json:"message_thread_id,omitempty"`
	ReplyParameters *replyParameters `json:"reply_parameters,omitempty"`
}

type chatActionParams struct {
	ChatID          int64  `json:"chat_id"`
	Action          string `json:"action"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
}

type getUpdatesParams struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}
