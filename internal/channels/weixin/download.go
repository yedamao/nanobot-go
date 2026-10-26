package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// Inbound media (nanobot: _download_media_item, after openclaw-weixin's
// media-download.ts and pic-decrypt.ts): an image, voice, file or video
// item points at an AES-128-ECB encrypted file on the WeChat CDN, which
// is downloaded, decrypted and saved to MediaDir in the workspace. The
// inbound message lists the saved files in Media.
const (
	// MediaDir is the workspace directory inbound media are saved to
	// (nanobot: get_media_dir("weixin")).
	MediaDir = "media/weixin"
	// maxImageDownloadBytes caps an image at what the providers inline;
	// a larger one could never reach the model.
	maxImageDownloadBytes = providers.MaxImageBytes
	// maxDownloadBytes caps any other file, like maxUploadBytes.
	maxDownloadBytes = 50 << 20
	// downloadTimeout bounds one CDN download.
	downloadTimeout = 2 * time.Minute

	mediaImage = "image"
	mediaVoice = "voice"
	mediaFile  = "file"
	mediaVideo = "video"
)

// mediaExts are the extensions of saved media without a file name of
// their own (nanobot: _ext_for_type); images go by their detected type.
var mediaExts = map[string]string{mediaImage: ".jpg", mediaVoice: ".silk", mediaVideo: ".mp4"}

var imageTypeExts = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// inboundMedia is where one inbound media item lives on the CDN.
type inboundMedia struct {
	kind   string // mediaImage, mediaVoice, mediaFile or mediaVideo
	media  *cdnMedia
	hexKey string // image_item.aeskey
	name   string // file_item.file_name
}

// mediaOf returns the media of an image, voice, file or video item.
func mediaOf(it *wireItem) (inboundMedia, bool) {
	switch {
	case it.Type == itemImage && it.ImageItem != nil:
		return inboundMedia{kind: mediaImage, media: it.ImageItem.Media, hexKey: it.ImageItem.AESKey}, true
	case it.Type == itemVoice && it.VoiceItem != nil:
		return inboundMedia{kind: mediaVoice, media: it.VoiceItem.Media}, true
	case it.Type == itemFile && it.FileItem != nil:
		return inboundMedia{kind: mediaFile, media: it.FileItem.Media, name: it.FileItem.FileName}, true
	case it.Type == itemVideo && it.VideoItem != nil:
		return inboundMedia{kind: mediaVideo, media: it.VideoItem.Media}, true
	}
	return inboundMedia{}, false
}

// located reports whether the item says where to download it from
// (nanobot: _has_downloadable_media_locator).
func (m inboundMedia) located() bool {
	return m.media != nil && (m.media.EncryptQueryParam != "" || strings.TrimSpace(m.media.FullURL) != "")
}

// key returns the item's AES key, or nil if it has none: image_item.aeskey
// (hex) first, else media.aes_key (see parseAESKey).
func (m inboundMedia) key() ([]byte, error) {
	if m.hexKey != "" {
		k, err := hex.DecodeString(m.hexKey)
		if err != nil || len(k) != 16 {
			return nil, errors.New("image aeskey is not 16 hex-encoded bytes")
		}
		return k, nil
	}
	if m.media.AESKey != "" {
		return parseAESKey(m.media.AESKey)
	}
	return nil, nil
}

// downloadMedia saves the media of a message's items and returns their
// workspace-relative paths, in item order. When no item has media to
// download, the media of the first quoted message that is media are
// tried instead (nanobot: the ref_msg fallback); quote then labels them,
// as "[image]" and the like, for the message text. A download that fails
// is logged and left out.
func (c *Channel) downloadMedia(ctx context.Context, items []wireItem) (paths []string, quote string) {
	located := false
	for i := range items {
		m, ok := mediaOf(&items[i])
		if !ok || !m.located() {
			continue
		}
		located = true
		if p := c.fetchMedia(ctx, m); p != "" {
			paths = append(paths, p)
		}
	}
	if located {
		return paths, ""
	}
	for _, it := range items {
		if it.Type != itemText || it.RefMsg == nil || it.RefMsg.MessageItem == nil || !isMedia(it.RefMsg.MessageItem.Type) {
			continue
		}
		ref := it.RefMsg.MessageItem
		if m, ok := mediaOf(ref); ok && m.located() {
			if p := c.fetchMedia(ctx, m); p != "" {
				return []string{p}, mediaLabel(*ref)
			}
		}
		break
	}
	return nil, ""
}

// fetchMedia downloads one item, logging why it could not.
func (c *Channel) fetchMedia(ctx context.Context, m inboundMedia) string {
	p, err := c.saveMedia(ctx, m)
	if err != nil {
		c.log.Warn("weixin: cannot download inbound media", "type", m.kind, "error", err)
		return ""
	}
	return p
}

// saveMedia downloads and decrypts one item to MediaDir (mode 0600) and
// returns its workspace-relative path. Images may come unencrypted;
// voice, files and video need a key.
func (c *Channel) saveMedia(ctx context.Context, m inboundMedia) (string, error) {
	if c.workspace == "" {
		return "", errors.New("no workspace to save it to")
	}
	key, err := m.key()
	if err != nil {
		return "", err
	}
	if key == nil && m.kind != mediaImage {
		return "", errors.New("no AES key")
	}
	limit := maxDownloadBytes
	if m.kind == mediaImage {
		limit = maxImageDownloadBytes
	}
	// The ciphertext is up to one block longer than the file.
	data, err := c.downloadCDN(ctx, m, int64(limit+aes.BlockSize))
	if err != nil {
		return "", err
	}
	if key != nil {
		if data, err = decryptECB(data, key); err != nil {
			return "", err
		}
	}
	if len(data) == 0 {
		return "", errors.New("empty file")
	}
	if len(data) > limit {
		return "", fmt.Errorf("file too large (limit %d bytes)", limit)
	}
	rel := path.Join(MediaDir, mediaFileName(m, data))
	if err := fsutil.WriteInRoot(c.workspace, rel, data); err != nil {
		return "", err
	}
	return rel, nil
}

// downloadCDN fetches the item from its full_url, or when that is
// missing or fails, from the CDN's download endpoint with its
// encrypt_query_param.
func (c *Channel) downloadCDN(ctx context.Context, m inboundMedia, limit int64) ([]byte, error) {
	var urls []string
	if u := strings.TrimSpace(m.media.FullURL); u != "" {
		urls = append(urls, u)
	}
	if p := m.media.EncryptQueryParam; p != "" {
		urls = append(urls, c.cdnBaseURL+"/download?encrypted_query_param="+url.QueryEscape(p))
	}
	var err error
	for i, u := range urls {
		var data []byte
		if data, err = c.getCDN(ctx, u, limit); err == nil {
			return data, nil
		}
		if i+1 < len(urls) {
			c.log.Warn("weixin: media download via full_url failed; trying encrypt_query_param", "type", m.kind, "error", err)
		}
	}
	return nil, err
}

// getCDN downloads u, at most limit bytes. The bot token is not sent,
// and errors leave out the URL (it carries the download's credentials).
func (c *Channel) getCDN(ctx context.Context, u string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("weixin cdn download: invalid download URL")
	}
	resp, err := c.api.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, redact(fmt.Errorf("weixin cdn download: %w", err), "")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil, &httpError{Endpoint: "cdn download", Status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, redact(fmt.Errorf("weixin cdn download: %w", err), "")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("weixin cdn download: file too large (limit %d bytes)", limit)
	}
	return data, nil
}

// mediaFileName names a saved item: a file by its own name, made safe,
// anything else as <kind>_<random><ext>. A random part keeps files of the
// same name apart.
func mediaFileName(m inboundMedia, data []byte) string {
	uniq := strings.ToLower(rand.Text()[:10])
	if m.kind == mediaFile {
		if name := safeFileName(m.name); name != "" {
			return uniq + "_" + name
		}
		return "file_" + uniq
	}
	ext := mediaExts[m.kind]
	if m.kind == mediaImage {
		if e, ok := imageTypeExts[http.DetectContentType(data)]; ok {
			ext = e
		}
	}
	return m.kind + "_" + uniq + ext
}

// safeFileName returns the last element of a sender-supplied file name
// with only letters, digits, '.', '-' and '_' (others become '_'), no
// leading dots, and at most 100 characters, keeping a short extension.
func safeFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, name)
	name = strings.TrimLeft(name, ".")
	if r := []rune(name); len(r) > 100 {
		ext := path.Ext(name)
		if len([]rune(ext)) > 16 {
			ext = ""
		}
		name = string(r[:100-len([]rune(ext))]) + ext
	}
	return name
}

// parseAESKey decodes media.aes_key (nanobot: _parse_aes_key): base64 of
// the 16-byte key (images), or base64 of the key as 32 hex characters
// (voice, files, video).
func parseAESKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err != nil {
			return nil, errors.New("aes_key is not base64")
		}
	}
	switch len(b) {
	case 16:
		return b, nil
	case 32:
		if k, err := hex.DecodeString(string(b)); err == nil {
			return k, nil
		}
	}
	return nil, fmt.Errorf("aes_key decodes to %d bytes, not a 16-byte key or 32 hex characters", len(b))
}

// decryptECB decrypts AES-128-ECB data and removes its PKCS7 padding.
func decryptECB(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext of %d bytes is not whole AES blocks", len(data))
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("bad PKCS7 padding (wrong key?)")
	}
	return out[:len(out)-pad], nil
}
