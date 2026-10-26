// Package imagegen generates images through an image provider and stores
// them as workspace artifacts, porting nanobot/providers/image_generation.py
// (the OpenRouter and OpenAI clients) and nanobot/utils/artifacts.py.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/security"
)

const (
	// DefaultTimeout bounds one provider request, like nanobot's 120 s.
	DefaultTimeout = 120 * time.Second
	// MaxImageBytes caps one generated, downloaded or reference image.
	MaxImageBytes = 32 << 20
	// maxResponseBytes caps a provider response, which holds base64 images.
	maxResponseBytes = 4 * MaxImageBytes
	// errorBodyLimit caps the response body quoted in an error.
	errorBodyLimit = 500
)

// Request is one image generation call.
type Request struct {
	Prompt string
	Model  string
	// ReferenceImages are absolute paths of images to edit or follow;
	// the caller has checked them.
	ReferenceImages []string
	AspectRatio     string
	ImageSize       string
}

// Response holds the generated images as data URLs.
type Response struct {
	Images []string
	// Content is the text the provider returned alongside, if any.
	Content string
}

// Provider generates images.
type Provider interface {
	Name() string
	Generate(ctx context.Context, req Request) (Response, error)
}

// Config configures a client.
type Config struct {
	APIKey string
	// APIBase overrides the provider's default endpoint.
	APIBase      string
	ExtraHeaders map[string]string
	ExtraBody    map[string]any
	// HTTPClient sends the provider requests; nil means a client with
	// DefaultTimeout.
	HTTPClient *http.Client
	// Policy checks image URLs a provider returns instead of base64
	// before they are downloaded; nil means the default policy.
	Policy *security.Policy
}

// New returns the client for the named provider ("openrouter" or "openai").
func New(name string, cfg Config) (Provider, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: DefaultTimeout}
	}
	if cfg.Policy == nil {
		cfg.Policy = &security.Policy{}
	}
	switch name {
	case "openrouter":
		cfg.APIBase = baseURL(cfg.APIBase, "https://openrouter.ai/api/v1")
		return &OpenRouter{cfg: cfg}, nil
	case "openai":
		cfg.APIBase = baseURL(cfg.APIBase, "https://api.openai.com/v1")
		return &OpenAI{cfg: cfg}, nil
	}
	return nil, fmt.Errorf("unsupported image generation provider %q", name)
}

func baseURL(v, def string) string {
	if v = strings.TrimSpace(v); v == "" {
		v = def
	}
	return strings.TrimRight(v, "/")
}

// post sends body as JSON (or the given content type) and returns the
// response body of a 2xx answer. label names the provider in errors.
func post(ctx context.Context, cfg Config, label, url, contentType string, body []byte, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range cfg.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s image generation failed: %w", label, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s image generation failed: read response: %w", label, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s image generation failed (HTTP %d): %s", label, resp.StatusCode, errorDetail(data))
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%s image generation failed: response larger than %d bytes", label, maxResponseBytes)
	}
	return data, nil
}

// errorDetail returns the error message of an error body, or the body
// truncated.
func errorDetail(body []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil && len(v.Error) > 0 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &e) == nil && e.Message != "" {
			return e.Message
		}
		var s string
		if json.Unmarshal(v.Error, &s) == nil && s != "" {
			return s
		}
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "<empty response body>"
	}
	return truncate(s, errorBodyLimit)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// mergeBody adds extra to body, like nanobot's body.update(extra_body).
// A null value removes the key, so extraBody can opt out of a default.
func mergeBody(body, extra map[string]any) map[string]any {
	maps.Copy(body, extra)
	for k, v := range body {
		if v == nil {
			delete(body, k)
		}
	}
	return body
}

// noImages is the error for a response without images.
func noImages(label string, data []byte) error {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &v) == nil && len(v.Error) > 0 && string(v.Error) != "null" {
		return fmt.Errorf("%s returned no images: %s", label, truncate(string(v.Error), errorBodyLimit))
	}
	return fmt.Errorf("%s returned no images for this request", label)
}

// imageMIMEs maps the image types accepted to file extensions.
var imageMIMEs = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// DetectImageMIME returns the MIME type of a PNG, JPEG, WebP or GIF image
// sniffed from its bytes, or "" for anything else.
func DetectImageMIME(data []byte) string {
	mime := http.DetectContentType(data)
	if _, ok := imageMIMEs[mime]; ok {
		return mime
	}
	return ""
}

// ErrNotImage is returned (wrapped) for data that is not a supported image.
var ErrNotImage = errors.New("unsupported or unrecognized image data")

// DecodeDataURL decodes a base64 image data URL and returns the bytes and
// their sniffed MIME type, which wins over the declared one.
func DecodeDataURL(dataURL string) ([]byte, string, error) {
	s := strings.TrimSpace(dataURL)
	header, payload, ok := strings.Cut(s, ",")
	if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
		return nil, "", errors.New("expected a base64 image data URL")
	}
	raw, err := decodeBase64(payload)
	if err != nil {
		return nil, "", errors.New("invalid base64 image payload")
	}
	if len(raw) > MaxImageBytes {
		return nil, "", fmt.Errorf("image larger than %d bytes", MaxImageBytes)
	}
	mime := DetectImageMIME(raw)
	if mime == "" {
		return nil, "", ErrNotImage
	}
	return raw, mime, nil
}

func decodeBase64(s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "")
	return base64.StdEncoding.Strict().DecodeString(s)
}

// b64DataURL turns raw base64 image data into a data URL.
func b64DataURL(b64 string) (string, error) {
	raw, err := decodeBase64(b64)
	if err != nil {
		return "", errors.New("generated image payload was not valid base64")
	}
	return bytesDataURL(raw)
}

func bytesDataURL(raw []byte) (string, error) {
	mime := DetectImageMIME(raw)
	if mime == "" {
		return "", errors.New("generated image payload was not a supported image")
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// ReadImage reads the image file at path (at most MaxImageBytes) and
// returns its bytes and MIME type.
func ReadImage(path string) ([]byte, string, error) {
	raw, err := fsutil.ReadRegularFile(path, MaxImageBytes+1)
	if err != nil {
		return nil, "", err
	}
	if len(raw) > MaxImageBytes {
		return nil, "", fmt.Errorf("image %s is larger than %d bytes", path, MaxImageBytes)
	}
	mime := DetectImageMIME(raw)
	if mime == "" {
		return nil, "", fmt.Errorf("unsupported reference image: %s", path)
	}
	return raw, mime, nil
}

func pathDataURL(path string) (string, error) {
	raw, mime, err := ReadImage(path)
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}
