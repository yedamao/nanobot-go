package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// openRouterHeaders are OpenRouter's app attribution headers.
var openRouterHeaders = map[string]string{
	"HTTP-Referer":       "https://github.com/yedamao/nanobot-go",
	"X-OpenRouter-Title": "nanobot-go",
}

// OpenRouter generates images through OpenRouter's chat completions with
// modalities ["image", "text"].
type OpenRouter struct{ cfg Config }

func (o *OpenRouter) Name() string { return "openrouter" }

func (o *OpenRouter) Generate(ctx context.Context, req Request) (Response, error) {
	if o.cfg.APIKey == "" {
		return Response{}, errors.New("OpenRouter API key is not configured. Set providers.openrouter.apiKey")
	}
	var content any = req.Prompt
	if len(req.ReferenceImages) > 0 {
		blocks := []map[string]any{{"type": "text", "text": req.Prompt}}
		for _, p := range req.ReferenceImages {
			u, err := pathDataURL(p)
			if err != nil {
				return Response{}, err
			}
			blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
		content = blocks
	}
	body := map[string]any{
		"model":      req.Model,
		"messages":   []map[string]any{{"role": "user", "content": content}},
		"modalities": []string{"image", "text"},
		"stream":     false,
	}
	imageConfig := map[string]string{}
	if req.AspectRatio != "" {
		imageConfig["aspect_ratio"] = req.AspectRatio
	}
	if req.ImageSize != "" {
		imageConfig["image_size"] = req.ImageSize
	}
	if len(imageConfig) > 0 {
		body["image_config"] = imageConfig
	}
	data, err := json.Marshal(mergeBody(body, o.cfg.ExtraBody))
	if err != nil {
		return Response{}, err
	}
	out, err := post(ctx, o.cfg, "OpenRouter", o.cfg.APIBase+"/chat/completions", "application/json", data, openRouterHeaders)
	if err != nil {
		return Response{}, err
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
				Images  []struct {
					ImageURL      *struct{ URL string } `json:"image_url"`
					ImageURLCamel *struct{ URL string } `json:"imageUrl"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return Response{}, fmt.Errorf("OpenRouter image generation: decode response: %w", err)
	}
	var res Response
	var text []string
	for _, c := range resp.Choices {
		if s, ok := c.Message.Content.(string); ok && s != "" {
			text = append(text, s)
		}
		for _, img := range c.Message.Images {
			u := img.ImageURL
			if u == nil {
				u = img.ImageURLCamel
			}
			if u != nil && strings.HasPrefix(u.URL, "data:image/") {
				res.Images = append(res.Images, u.URL)
			}
		}
	}
	if len(res.Images) == 0 {
		return Response{}, noImages("openrouter", out)
	}
	res.Content = strings.TrimSpace(strings.Join(text, "\n"))
	return res, nil
}
