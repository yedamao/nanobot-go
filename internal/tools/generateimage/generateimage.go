// Package generateimage is the generate_image tool
// (nanobot/agent/tools/image_generation.py): it generates images through
// the configured image provider and saves them as workspace artifacts
// under media/<saveDir>/. Like nanobot, the images are not attached to the
// final reply; the result tells the model to deliver them with the
// message tool's media.
package generateimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/imagegen"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// maxReferenceImages caps reference_images.
const maxReferenceImages = 8

// Tool generates images.
type Tool struct {
	provider imagegen.Provider
	ws       *security.Workspace
	cfg      config.ImageGenerationConfig
}

// New returns the tool generating with p and saving in ws.
func New(p imagegen.Provider, ws *security.Workspace, cfg config.ImageGenerationConfig) *Tool {
	return &Tool{provider: p, ws: ws, cfg: cfg}
}

func (t *Tool) Name() string { return "generate_image" }

func (t *Tool) Description() string {
	return "Generate or edit images as persistent artifacts. Returns artifact IDs and local paths."
}

func (t *Tool) Parameters() map[string]any {
	count := tools.Integer("Number of images to generate in this turn.")
	count["minimum"] = 1
	count["maximum"] = config.MaxImagesPerTurnLimit
	prompt := tools.String("Detailed image generation or edit instructions.")
	prompt["minLength"] = 1
	return tools.Object(map[string]any{
		"prompt": prompt,
		"reference_images": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Existing local image paths for edits, including prior generated artifacts.",
		},
		"aspect_ratio": tools.String("Output aspect ratio, e.g. 1:1 or 16:9."),
		"image_size":   tools.String("Provider-supported output size hint, e.g. 1K or 1024x1024."),
		"count":        count,
	}, "prompt")
}

func (t *Tool) ReadOnly() bool { return false }

type args struct {
	Prompt          string   `json:"prompt"`
	ReferenceImages []string `json:"reference_images"`
	AspectRatio     string   `json:"aspect_ratio"`
	ImageSize       string   `json:"image_size"`
	Count           *int     `json:"count"`
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	a, err := tools.DecodeArgs[args](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return tools.Errorf("prompt must not be empty")
	}
	if t.provider == nil {
		return tools.Errorf("unsupported image generation provider '%s'", t.cfg.Provider)
	}
	n := 1
	if a.Count != nil {
		n = *a.Count
	}
	if n < 1 {
		return tools.Errorf("count must be at least 1")
	}
	if n > t.cfg.MaxImagesPerTurn {
		return tools.Errorf("count exceeds tools.imageGeneration.maxImagesPerTurn (%d)", t.cfg.MaxImagesPerTurn)
	}
	refs, err := t.resolveReferences(a.ReferenceImages)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	req := imagegen.Request{
		Prompt:          a.Prompt,
		Model:           t.cfg.Model,
		ReferenceImages: refs,
		AspectRatio:     orDefault(a.AspectRatio, t.cfg.DefaultAspectRatio),
		ImageSize:       orDefault(a.ImageSize, t.cfg.DefaultImageSize),
	}
	var artifacts []imagegen.Artifact
	// Providers return at least one image or an error; the call cap is a
	// guard against one that keeps answering with nothing usable.
	for calls := 0; len(artifacts) < n && calls < n; calls++ {
		resp, err := t.provider.Generate(ctx, req)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		for _, img := range resp.Images {
			art, err := imagegen.Store(img, imagegen.StoreOptions{
				Workspace:    t.ws.Root,
				SaveDir:      t.cfg.SaveDir,
				Prompt:       a.Prompt,
				Model:        t.cfg.Model,
				Provider:     t.cfg.Provider,
				SourceImages: refs,
			})
			if err != nil {
				return tools.Errorf("%v", err)
			}
			artifacts = append(artifacts, art)
			if len(artifacts) >= n {
				break
			}
		}
	}
	if len(artifacts) < n {
		return tools.Errorf("%s returned %d of %d images", t.provider.Name(), len(artifacts), n)
	}
	return tools.Text(imagegen.ToolResult(artifacts))
}

// resolveReferences resolves reference images to existing image files in
// the workspace, whether or not the workspace restricts the other tools:
// they are uploaded to the provider.
func (t *Tool) resolveReferences(paths []string) ([]string, error) {
	if len(paths) > maxReferenceImages {
		return nil, fmt.Errorf("too many reference_images: %d (at most %d)", len(paths), maxReferenceImages)
	}
	var out []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs, err := t.ws.ResolveRead(p)
		if err == nil && !t.ws.Contains(abs) {
			err = security.ErrOutsideWorkspace
		}
		if err != nil {
			return nil, fmt.Errorf("reference_images must be inside the workspace: %s: %w", p, err)
		}
		info, err := os.Stat(abs)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("reference image not found: %s", p)
		case err != nil:
			return nil, fmt.Errorf("reference image %s: %w", p, err)
		case !info.Mode().IsRegular():
			return nil, fmt.Errorf("reference image is not a file: %s", p)
		}
		if _, _, err := imagegen.ReadImage(abs); err != nil {
			return nil, fmt.Errorf("unsupported reference image: %s", p)
		}
		out = append(out, abs)
	}
	return out, nil
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}
