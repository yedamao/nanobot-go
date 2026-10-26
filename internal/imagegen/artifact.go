package imagegen

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// MediaDir is the workspace directory generated images are saved under,
// next to the channels' media/telegram and media/weixin.
const MediaDir = "media"

// Artifact describes a stored image; it is also its sidecar JSON.
type Artifact struct {
	ID string `json:"id"`
	// Path is the absolute path of the image, inside the workspace, which
	// the message tool accepts as media.
	Path         string   `json:"path"`
	MIME         string   `json:"mime"`
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model"`
	Provider     string   `json:"provider"`
	SourceImages []string `json:"source_images"`
	CreatedAt    string   `json:"created_at"`
}

// StoreOptions describe the image being stored.
type StoreOptions struct {
	// Workspace is the absolute workspace root.
	Workspace string
	// SaveDir is a safe relative directory under media/.
	SaveDir      string
	Prompt       string
	Model        string
	Provider     string
	SourceImages []string
	// Now defaults to time.Now.
	Now time.Time
}

// Store decodes a data URL image and saves it, with a sidecar
// <id>.json, as <workspace>/media/<saveDir>/<YYYY-MM-DD>/img_<12 hex>.<ext>
// (mode 0600). Writes go through an os.Root on the workspace, so a
// symlink cannot redirect them outside it.
func Store(dataURL string, o StoreOptions) (Artifact, error) {
	raw, mime, err := DecodeDataURL(dataURL)
	if err != nil {
		return Artifact{}, err
	}
	ext, ok := imageMIMEs[mime]
	if !ok {
		return Artifact{}, fmt.Errorf("unsupported image MIME type: %s", mime)
	}
	saveDir, err := config.SafeRelDir(o.SaveDir)
	if err != nil {
		return Artifact{}, fmt.Errorf("saveDir %w", err)
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Artifact{}, err
	}
	a := Artifact{
		ID:           "img_" + hex.EncodeToString(id[:]),
		MIME:         mime,
		Prompt:       o.Prompt,
		Model:        o.Model,
		Provider:     o.Provider,
		SourceImages: append([]string{}, o.SourceImages...),
		CreatedAt:    now.Format(time.RFC3339),
	}
	dir := path.Join(MediaDir, saveDir, now.Format("2006-01-02"))
	rel := path.Join(dir, a.ID+ext)
	a.Path = filepath.Join(o.Workspace, filepath.FromSlash(rel))
	if err := fsutil.WriteInRoot(o.Workspace, rel, raw); err != nil {
		return Artifact{}, fmt.Errorf("save image: %w", err)
	}
	meta, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return Artifact{}, err
	}
	if err := fsutil.WriteInRoot(o.Workspace, path.Join(dir, a.ID+".json"), meta); err != nil {
		return Artifact{}, fmt.Errorf("save image metadata: %w", err)
	}
	return a, nil
}

// NextStep is the guidance returned with the artifacts, like nanobot's
// generated_image_tool_result.
const NextStep = "Use these artifact paths as reference_images for follow-up edits. " +
	"Call the message tool with the artifact paths in the media parameter " +
	"to deliver the images to the user. Keep raw paths internal unless the " +
	"user asks for debug details."

// ToolResult returns the JSON the generate_image tool answers with.
func ToolResult(artifacts []Artifact) string {
	data, _ := json.Marshal(struct {
		Artifacts []Artifact `json:"artifacts"`
		NextStep  string     `json:"next_step"`
	}{artifacts, NextStep})
	return string(data)
}
