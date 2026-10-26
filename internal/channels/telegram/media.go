package telegram

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/yedamao/nanobot-go/internal/providers"
)

const (
	// MediaDir is the workspace directory inbound photos are saved to
	// (nanobot: get_media_dir("telegram")).
	MediaDir = "media/telegram"
	// maxPhotoBytes caps a downloaded photo at what the providers inline;
	// a larger one could never reach the model.
	maxPhotoBytes = providers.MaxImageBytes
)

// photoExts maps the detected type of a photo to its file extension.
var photoExts = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// largestPhoto returns the biggest of a photo's sizes, by pixels and then
// file size (Telegram lists them smallest first, like nanobot's
// msg.photo[-1]).
func largestPhoto(sizes []photoSize) photoSize {
	best := sizes[0]
	for _, s := range sizes[1:] {
		pa, pb := int64(s.Width)*int64(s.Height), int64(best.Width)*int64(best.Height)
		if pa > pb || pa == pb && s.FileSize > best.FileSize {
			best = s
		}
	}
	return best
}

// downloadPhoto saves the largest size of a photo to MediaDir in the
// workspace as <file_unique_id>.<ext> (mode 0600) and returns its
// workspace-relative path, the form websocket uploads use too.
func (c *Channel) downloadPhoto(ctx context.Context, sizes []photoSize) (string, error) {
	if c.workspace == "" {
		return "", errors.New("no workspace to save it to")
	}
	p := largestPhoto(sizes)
	if !safeID(p.FileUniqueID) {
		return "", fmt.Errorf("unusable file_unique_id %q", p.FileUniqueID)
	}
	if p.FileSize > maxPhotoBytes {
		return "", fmt.Errorf("%w (%d bytes, limit %d)", errTooLarge, p.FileSize, maxPhotoBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	var f file
	if err := c.api.call(ctx, "getFile", getFileParams{FileID: p.FileID}, &f); err != nil {
		return "", err
	}
	if f.FilePath == "" {
		return "", errors.New("telegram getFile: no file_path")
	}
	if f.FileSize > maxPhotoBytes {
		return "", fmt.Errorf("%w (%d bytes, limit %d)", errTooLarge, f.FileSize, maxPhotoBytes)
	}
	data, err := c.api.download(ctx, f.FilePath, maxPhotoBytes)
	if err != nil {
		return "", err
	}
	ext, ok := photoExts[http.DetectContentType(data)]
	if !ok {
		return "", errors.New("not a supported image format")
	}
	rel := path.Join(MediaDir, p.FileUniqueID+ext)
	if err := writeWorkspaceFile(c.workspace, rel, data); err != nil {
		return "", err
	}
	return rel, nil
}

// writeWorkspaceFile writes data to rel inside the workspace dir through
// an os.Root, via a temporary file renamed into place.
func writeWorkspaceFile(dir, rel string, data []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(path.Dir(rel), 0o700); err != nil {
		return err
	}
	tmp := rel + ".tmp-" + strings.ToLower(rand.Text()[:8])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		root.Remove(tmp)
	}
	return err
}

// safeID reports whether a file_unique_id can be used as a file name:
// Telegram's are short base64url strings.
func safeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
