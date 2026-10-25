package providers

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// Limits on the images ExpandMedia inlines into a request. An image over a
// limit is left out and named in a text part instead.
const (
	// MaxImageBytes caps one image file. Anthropic rejects images whose
	// base64 form exceeds 5 MiB, which is about 3.75 MiB of file.
	MaxImageBytes = 3_750_000
	// MaxImagesPerMessage caps the images inlined for one message.
	MaxImagesPerMessage = 8
	// MaxRequestImageBytes caps the image files inlined into one request,
	// newest messages first, keeping the request well under provider body
	// limits once base64-encoded.
	MaxRequestImageBytes = 16 << 20
)

// imageExts are the raster formats sent to models as image parts, like
// nanobot's is_image_file without SVG, which vision APIs do not accept.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// imageTypes are the MIME types the vision APIs accept, as detected from
// the file bytes.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// IsImagePath reports whether path names a raster image by its extension.
func IsImagePath(path string) bool { return imageExts[strings.ToLower(filepath.Ext(path))] }

// SplitMedia ports nanobot's reference_non_image_attachments: image paths
// are returned for vision parts, and every other path is appended to
// content as an [Attachment: path] line the model can read with its tools.
func SplitMedia(content string, media []string) (string, []string) {
	var images, refs []string
	for _, p := range media {
		if IsImagePath(p) {
			images = append(images, p)
		} else {
			refs = append(refs, "[Attachment: "+p+"]")
		}
	}
	if len(refs) > 0 {
		suffix := strings.Join(refs, "\n")
		if content != "" {
			content += "\n\n" + suffix
		} else {
			content = suffix
		}
	}
	return content, images
}

// Image is an inlined image part.
type Image struct {
	// MediaType is the MIME type detected from the bytes.
	MediaType string
	// Data is the base64-encoded file.
	Data string
}

// DataURL returns the image as a data: URL.
func (i Image) DataURL() string { return "data:" + i.MediaType + ";base64," + i.Data }

// Part is one piece of a multimodal message: an image when Image is set,
// else text.
type Part struct {
	Text  string
	Image *Image
}

// imagePlaceholder names an image path in text, like nanobot's
// image_placeholder_text; note, if set, says why the image is missing.
func imagePlaceholder(path, note string) string {
	if note != "" {
		return fmt.Sprintf("[image: %s (%s)]", path, note)
	}
	return "[image: " + path + "]"
}

// ExpandMedia returns a copy of msgs where each user message with Media
// has Parts: per image, a placeholder naming its path followed by the
// image itself, then the message text, as nanobot's build_user_content
// lays them out. Relative paths resolve against baseDir. Images are read
// now, so sessions only ever store paths; one that is missing, not a
// supported raster format or over a limit becomes a placeholder noting why,
// and never fails the request. The request budget goes to the newest
// messages first. msgs is not modified.
func ExpandMedia(msgs []Message, baseDir string) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	budget := int64(MaxRequestImageBytes)
	for i := len(out) - 1; i >= 0; i-- {
		m := &out[i]
		if m.Role != RoleUser || len(m.Media) == 0 {
			continue
		}
		parts := make([]Part, 0, 2*len(m.Media)+1)
		count := 0
		for _, p := range m.Media {
			img, size, note := loadImage(p, baseDir, count, budget)
			if img == nil {
				parts = append(parts, Part{Text: imagePlaceholder(p, note)})
				continue
			}
			count++
			budget -= size
			parts = append(parts, Part{Text: imagePlaceholder(p, "")}, Part{Image: img})
		}
		m.Parts = append(parts, Part{Text: m.Content})
	}
	return out
}

// loadImage reads the image at p for a message that already holds count
// images, with its file size, or returns why it is not sent.
func loadImage(p, baseDir string, count int, budget int64) (*Image, int64, string) {
	if count >= MaxImagesPerMessage {
		return nil, 0, fmt.Sprintf("not attached: over %d images", MaxImagesPerMessage)
	}
	path := p
	if !filepath.IsAbs(path) && baseDir != "" {
		path = filepath.Join(baseDir, path)
	}
	data, err := fsutil.ReadRegularFile(path, MaxImageBytes+1)
	if err != nil {
		return nil, 0, "not attached: unreadable"
	}
	size := int64(len(data))
	if size > MaxImageBytes {
		return nil, 0, "not attached: too large"
	}
	if size > budget {
		return nil, 0, "not attached: request image budget exhausted"
	}
	typ := http.DetectContentType(data)
	if !imageTypes[typ] {
		return nil, 0, "not attached: unsupported format"
	}
	return &Image{MediaType: typ, Data: base64.StdEncoding.EncodeToString(data)}, size, ""
}
