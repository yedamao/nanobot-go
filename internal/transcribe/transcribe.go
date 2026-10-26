// Package transcribe turns audio into text through an OpenAI-compatible
// POST {apiBase}/audio/transcriptions, the thin Go counterpart of
// nanobot's transcription service (nanobot/audio/transcription) with only
// its "openai" provider (Whisper).
package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultAPIBase is OpenAI's API.
	DefaultAPIBase = "https://api.openai.com/v1"
	// DefaultModel is nanobot's default for the openai provider.
	DefaultModel = "whisper-1"
	// DefaultTimeout bounds one transcription request.
	DefaultTimeout = 120 * time.Second
	// maxResponseBytes caps the response body read.
	maxResponseBytes = 1 << 20
	path             = "audio/transcriptions"
)

// mimeTypes are the content types of the audio file part by extension,
// like nanobot's _AUDIO_MIME_OVERRIDES.
var mimeTypes = map[string]string{
	".m4a":  "audio/mp4",
	".mp3":  "audio/mpeg",
	".mpga": "audio/mpeg",
	".oga":  "audio/ogg",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
	".weba": "audio/webm",
	".webm": "audio/webm",
	".flac": "audio/flac",
}

// OpenAI is a Whisper client for an OpenAI-compatible API.
type OpenAI struct {
	APIKey string
	// APIBase is the API root ("…/v1") or the full transcriptions URL;
	// empty means DefaultAPIBase.
	APIBase string
	// Model defaults to DefaultModel.
	Model string
	// HTTPClient defaults to a client with DefaultTimeout.
	HTTPClient *http.Client
}

// endpoint resolves the transcriptions URL like nanobot's
// _resolve_transcription_url: a chat-style base gets the path appended.
func (o *OpenAI) endpoint() string {
	base := strings.TrimRight(strings.TrimSpace(o.APIBase), "/")
	if base == "" {
		base = DefaultAPIBase
	}
	if strings.HasSuffix(base, "/"+path) {
		return base
	}
	return base + "/" + path
}

// Transcribe sends the audio data, named name (its extension picks the
// content type), and returns the trimmed transcript. Errors never
// contain the API key.
func (o *OpenAI) Transcribe(ctx context.Context, name string, data []byte) (string, error) {
	if o.APIKey == "" {
		return "", errors.New("transcribe: no API key")
	}
	model := o.Model
	if model == "" {
		model = DefaultModel
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("model", model); err != nil {
		return "", err
	}
	ct, ok := mimeTypes[strings.ToLower(filepath.Ext(name))]
	if !ok {
		ct = "application/octet-stream"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(name)))
	h.Set("Content-Type", ct)
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint(), &body)
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return "", fmt.Errorf("transcribe: HTTP %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("transcribe: bad response: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}
