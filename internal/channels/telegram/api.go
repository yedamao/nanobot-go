package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
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

// client calls the Telegram Bot API over HTTPS with JSON (or, for file
// uploads, multipart) bodies. The bot
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
	return c.post(ctx, method, "application/json", bytes.NewReader(body), out)
}

// upload posts fields and one file (field name, file name, contents) as
// multipart/form-data to method, the way the Bot API takes local files
// (sendPhoto, sendDocument). An empty field sends the fields alone.
// Errors never contain the token.
func (c *client) upload(ctx context.Context, method string, fields map[string]string, field, name string, data []byte, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := w.WriteField(k, fields[k]); err != nil {
			return fmt.Errorf("telegram %s: %w", method, err)
		}
	}
	var err error
	if field != "" {
		var part io.Writer
		if part, err = w.CreateFormFile(field, name); err == nil {
			_, err = part.Write(data)
		}
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	return c.post(ctx, method, w.FormDataContentType(), &buf, out)
}

// post sends body to method and decodes the Bot API envelope.
func (c *client) post(ctx context.Context, method, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, body)
	if err != nil {
		return c.redact(fmt.Errorf("telegram %s: %w", method, err))
	}
	req.Header.Set("Content-Type", contentType)
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

// errTooLarge marks a download over its size limit.
var errTooLarge = errors.New("file too large")

// download fetches a file getFile returned (GET {base}/file/bot<token>/
// <file_path>) into memory, refusing one over limit bytes. Errors never
// contain the token.
func (c *client) download(ctx context.Context, filePath string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+filePath, nil)
	if err != nil {
		return nil, c.redact(fmt.Errorf("telegram download: %w", err))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = c.base + "/file/bot" + redacted + "/" + filePath
		}
		return nil, c.redact(fmt.Errorf("telegram download: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("telegram download: %w (%d bytes, limit %d)", errTooLarge, resp.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, c.redact(fmt.Errorf("telegram download: %w", err))
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("telegram download: %w (limit %d bytes)", errTooLarge, limit)
	}
	return data, nil
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
	MessageID int64 `json:"message_id"`
	// Date is 0 for an InaccessibleMessage (a callback query's message
	// the bot can no longer see).
	Date            int64    `json:"date"`
	MessageThreadID int64    `json:"message_thread_id"`
	IsTopicMessage  bool     `json:"is_topic_message"`
	From            *user    `json:"from"`
	Chat            chat     `json:"chat"`
	Text            string   `json:"text"`
	Caption         string   `json:"caption"`
	Entities        []entity `json:"entities"`
	CaptionEntities []entity `json:"caption_entities"`
	// Photo lists the sizes Telegram made of a photo, smallest first.
	Photo []photoSize `json:"photo"`
	// Voice is a voice note; Audio is a music file; VideoNote is a round
	// video message; Document is any other file.
	Voice          *voice     `json:"voice"`
	Audio          *audio     `json:"audio"`
	Document       *document  `json:"document"`
	Video          *video     `json:"video"`
	VideoNote      *videoNote `json:"video_note"`
	ReplyToMessage *message   `json:"reply_to_message"`
}

// document is a general file; FileName is the sender's, untrusted.
type document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type video struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Duration     int    `json:"duration"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// videoNote has no MIME type; Telegram records them as MPEG-4.
type videoNote struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Length       int    `json:"length"` // width and height
	Duration     int    `json:"duration"`
	FileSize     int64  `json:"file_size"`
}

type voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type photoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

// file is getFile's result; FilePath is valid for an hour.
type file struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	FilePath     string `json:"file_path"`
}

type getFileParams struct {
	FileID string `json:"file_id"`
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

// callbackQuery is a tap on an inline keyboard button. Message is the
// message the keyboard is on, nil when it is unknown.
type callbackQuery struct {
	ID      string   `json:"id"`
	From    *user    `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

type answerCallbackQueryParams struct {
	CallbackQueryID string `json:"callback_query_id"`
}

// editMessageReplyMarkupParams without a reply_markup removes the
// message's inline keyboard.
type editMessageReplyMarkupParams struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

// replyParameters makes a message a reply (sendMessage's
// reply_parameters).
type replyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
}

type sendMessageParams struct {
	ChatID          int64                 `json:"chat_id"`
	Text            string                `json:"text"`
	MessageThreadID int64                 `json:"message_thread_id,omitempty"`
	ReplyParameters *replyParameters      `json:"reply_parameters,omitempty"`
	ReplyMarkup     *inlineKeyboardMarkup `json:"reply_markup,omitempty"`
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
