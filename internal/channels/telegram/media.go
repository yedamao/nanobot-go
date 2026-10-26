package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yedamao/nanobot-go/internal/fsutil"
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
	if err := fsutil.WriteInRoot(c.workspace, rel, data); err != nil {
		return "", err
	}
	return rel, nil
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

const (
	// maxCaptionLen is Telegram's limit for a media caption, in UTF-16
	// code units like maxMessageLen.
	maxCaptionLen = 1024
	// maxOutboundFiles caps the files one reply sends; the rest are
	// logged and left out.
	maxOutboundFiles = 10
	// maxUploadPhotoBytes and maxUploadBytes are the Bot API's upload
	// limits for sendPhoto and sendDocument.
	maxUploadPhotoBytes = 10 << 20
	maxUploadBytes      = 50 << 20
)

// sendFile uploads one local file of an outbound message: an image
// (by extension, like nanobot's _get_media_type) through sendPhoto, and
// anything else, or a photo Telegram refuses (dimensions, size), through
// sendDocument. Relative paths resolve against the workspace. URLs are
// not fetched.
func (c *Channel) sendFile(ctx context.Context, chatID, threadID int64, reply *replyParameters, p, caption string) error {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return errors.New("remote media URLs are not supported")
	}
	full := p
	if !filepath.IsAbs(full) {
		if c.workspace == "" {
			return errors.New("no workspace to resolve a relative path against")
		}
		full = filepath.Join(c.workspace, filepath.FromSlash(p))
	}
	data, err := fsutil.ReadRegularFile(full, maxUploadBytes+1)
	if err != nil {
		return err
	}
	if len(data) > maxUploadBytes {
		return fmt.Errorf("%w (limit %d bytes)", errTooLarge, maxUploadBytes)
	}
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10)}
	if threadID != 0 {
		fields["message_thread_id"] = strconv.FormatInt(threadID, 10)
	}
	if reply != nil {
		rp, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		fields["reply_parameters"] = string(rp)
	}
	if caption != "" {
		fields["caption"] = caption
	}
	name := filepath.Base(full)
	if providers.IsImagePath(p) && len(data) <= maxUploadPhotoBytes {
		err := c.retrySend(ctx, chatID, func(ctx context.Context) error {
			return c.api.upload(ctx, "sendPhoto", fields, "photo", name, data, nil)
		})
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != http.StatusBadRequest {
			return err
		}
		c.log.Info("telegram: photo refused; sending it as a file", "chat_id", chatID, "error", err)
	}
	return c.retrySend(ctx, chatID, func(ctx context.Context) error {
		return c.api.upload(ctx, "sendDocument", fields, "document", name, data, nil)
	})
}
