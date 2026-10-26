package imagegen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
)

// OpenAI generates images through the OpenAI Images API:
// POST /images/generations, or /images/edits (multipart) with reference
// images, which only GPT Image models accept.
type OpenAI struct{ cfg Config }

func (o *OpenAI) Name() string { return "openai" }

// openAIModel strips an "openai/" prefix (OpenRouter's naming).
func openAIModel(model string) string {
	for _, p := range []string{"openai/", "openai_codex/"} {
		if after, ok := strings.CutPrefix(model, p); ok {
			return after
		}
	}
	return model
}

func isGPTImageModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "gpt-image") || strings.HasPrefix(m, "chatgpt-image")
}

var (
	dalle2Sizes   = map[string]bool{"256x256": true, "512x512": true, "1024x1024": true}
	dalle3Sizes   = map[string]bool{"1024x1024": true, "1792x1024": true, "1024x1792": true}
	gptImageSizes = map[string]bool{"1024x1024": true, "1536x1024": true, "1024x1536": true, "auto": true}

	dalle2Ratios = map[string]string{"1:1": "1024x1024", "16:9": "1024x1024", "9:16": "1024x1024", "3:4": "1024x1024", "4:3": "1024x1024"}
	dalle3Ratios = map[string]string{"1:1": "1024x1024", "16:9": "1792x1024", "9:16": "1024x1792", "3:4": "1024x1792", "4:3": "1792x1024"}
	gptRatios    = map[string]string{"1:1": "1024x1024", "16:9": "1536x1024", "9:16": "1024x1536", "3:4": "1024x1536", "4:3": "1536x1024"}
)

// openAISize resolves image_size or aspect_ratio to an Images API size,
// like nanobot's _openai_size: an explicit size the model supports wins,
// then the aspect ratio, then 1024x1024. gpt-image-2 takes any WxH.
func openAISize(model, aspectRatio, imageSize string) string {
	m := strings.ToLower(model)
	ratios, sizes := gptRatios, gptImageSizes
	switch {
	case strings.HasPrefix(m, "dall-e-2"):
		ratios, sizes = dalle2Ratios, dalle2Sizes
	case strings.HasPrefix(m, "dall-e-3"):
		ratios, sizes = dalle3Ratios, dalle3Sizes
	case strings.HasPrefix(m, "gpt-image-2"):
		sizes = nil
	}
	if s := strings.ToLower(strings.TrimSpace(imageSize)); s != "" {
		if sizes != nil && sizes[s] {
			return s
		}
		if w, h, ok := strings.Cut(s, "x"); sizes == nil && ok && isDigits(w) && isDigits(h) {
			return s
		}
	}
	if s, ok := ratios[aspectRatio]; ok {
		return s
	}
	return "1024x1024"
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (o *OpenAI) Generate(ctx context.Context, req Request) (Response, error) {
	if o.cfg.APIKey == "" {
		return Response{}, errors.New("OpenAI API key is not configured. Set providers.openai.apiKey")
	}
	model := openAIModel(req.Model)
	body := map[string]any{"model": model, "prompt": req.Prompt}
	if !isGPTImageModel(model) {
		body["response_format"] = "b64_json"
		body["n"] = 1
	}
	body["size"] = openAISize(model, req.AspectRatio, req.ImageSize)
	body = mergeBody(body, o.cfg.ExtraBody)

	var out []byte
	var err error
	if len(req.ReferenceImages) > 0 {
		if !isGPTImageModel(model) {
			return Response{}, fmt.Errorf("OpenAI model %q does not support reference images; use a GPT Image model", model)
		}
		ct, data, ferr := editForm(body, req.ReferenceImages)
		if ferr != nil {
			return Response{}, ferr
		}
		out, err = post(ctx, o.cfg, "OpenAI", o.cfg.APIBase+"/images/edits", ct, data, nil)
	} else {
		data, merr := json.Marshal(body)
		if merr != nil {
			return Response{}, merr
		}
		out, err = post(ctx, o.cfg, "OpenAI", o.cfg.APIBase+"/images/generations", "application/json", data, nil)
	}
	if err != nil {
		return Response{}, err
	}
	images, err := o.parseImages(ctx, out)
	if err != nil {
		return Response{}, err
	}
	if len(images) == 0 {
		return Response{}, noImages("openai", out)
	}
	return Response{Images: images}, nil
}

// parseImages reads data[].b64_json, or downloads data[].url through the
// SSRF policy.
func (o *OpenAI) parseImages(ctx context.Context, out []byte) ([]string, error) {
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("OpenAI image generation: decode response: %w", err)
	}
	var images []string
	for _, d := range resp.Data {
		switch {
		case d.B64JSON != "":
			u, err := b64DataURL(d.B64JSON)
			if err != nil {
				return nil, err
			}
			images = append(images, u)
		case d.URL != "":
			raw, err := o.cfg.Policy.Download(ctx, d.URL, MaxImageBytes)
			if err != nil {
				return nil, fmt.Errorf("download generated image: %w", err)
			}
			u, err := bytesDataURL(raw)
			if err != nil {
				return nil, err
			}
			images = append(images, u)
		}
	}
	return images, nil
}

// editForm builds the multipart body of /images/edits: scalar fields of
// body as form fields (others are dropped, like nanobot) and each
// reference image as image[].
func editForm(body map[string]any, refs []string) (string, []byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range body {
		var s string
		switch v := v.(type) {
		case string:
			s = v
		case bool:
			s = strconv.FormatBool(v)
		case int:
			s = strconv.Itoa(v)
		case float64:
			s = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			continue
		}
		if err := w.WriteField(k, s); err != nil {
			return "", nil, err
		}
	}
	for _, p := range refs {
		raw, mime, err := ReadImage(p)
		if err != nil {
			return "", nil, err
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", multipart.FileContentDisposition("image[]", filepath.Base(p)))
		h.Set("Content-Type", mime)
		part, err := w.CreatePart(h)
		if err != nil {
			return "", nil, err
		}
		if _, err := part.Write(raw); err != nil {
			return "", nil, err
		}
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}
	return w.FormDataContentType(), buf.Bytes(), nil
}
