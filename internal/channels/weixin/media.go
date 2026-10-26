package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// Outbound media (nanobot: _send_media_file, after openclaw-weixin): a
// file is encrypted with a random AES-128 key (ECB, PKCS7), uploaded to
// the WeChat CDN at the URL getuploadurl hands out, and sent with
// sendmessage as one image, video, voice or file item that references the
// upload.
const (
	uploadMediaImage = 1
	uploadMediaVideo = 2
	uploadMediaFile  = 3
	uploadMediaVoice = 4

	// encryptTypeAES marks a CDN item encrypted with its aes_key.
	encryptTypeAES = 1

	// defaultCDNBaseURL is where uploads go when getuploadurl returns only
	// upload_param (nanobot: cdn_base_url).
	defaultCDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

	// maxOutboundFiles caps the files one reply sends; the rest are
	// dropped with a warning (the message tool allows as many).
	maxOutboundFiles = 10
	// maxUploadBytes caps one file.
	maxUploadBytes = 50 << 20
	// uploadTimeout bounds the CDN upload.
	uploadTimeout = 2 * time.Minute
)

var (
	imageExts = extSet(".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".tiff", ".ico", ".svg")
	videoExts = extSet(".mp4", ".avi", ".mov", ".mkv", ".webm", ".flv")
	voiceExts = extSet(".mp3", ".wav", ".amr", ".silk", ".ogg", ".m4a", ".aac", ".flac")
)

func extSet(exts ...string) map[string]bool {
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		m[e] = true
	}
	return m
}

// unsendableError is a file that can never be sent as it is (missing,
// too large, refused by iLink or the CDN); the reply goes on without it.
type unsendableError struct{ err error }

func (e *unsendableError) Error() string { return e.err.Error() }
func (e *unsendableError) Unwrap() error { return e.err }

// failedNotice replaces a file that cannot be sent, like nanobot's.
func failedNotice(p string) string {
	return "[Failed to send: " + path.Base(filepath.ToSlash(p)) + "]"
}

// mediaPath resolves p against the workspace. The message tool already
// resolves attachments to absolute workspace paths, so absolute paths are
// taken as they are (like Telegram).
func (c *Channel) mediaPath(p string) (string, error) {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return "", errors.New("remote media URLs are not supported")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	if c.workspace == "" {
		return "", errors.New("no workspace to resolve a relative path against")
	}
	return filepath.Join(c.workspace, filepath.FromSlash(p)), nil
}

// sendMedia uploads part's file to the CDN and sends it to to. The upload
// keeps part's AES key and a file key derived from clientID, so a retry
// uploads the same thing. Errors about the file itself, and business
// errors other than the context token's and the bot token's, are
// *unsendableError.
func (c *Channel) sendMedia(ctx context.Context, to string, p *part, ctxToken, clientID string) error {
	full, err := c.mediaPath(p.media)
	if err != nil {
		return &unsendableError{err}
	}
	data, err := fsutil.ReadRegularFile(full, maxUploadBytes+1)
	if err != nil {
		return &unsendableError{err}
	}
	if len(data) > maxUploadBytes {
		return &unsendableError{fmt.Errorf("file too large (limit %d bytes)", maxUploadBytes)}
	}
	if p.aesKey == nil {
		p.aesKey = make([]byte, 16)
		rand.Read(p.aesKey)
	}
	uploadType, itemType := uploadMediaFile, itemFile
	switch ext := strings.ToLower(filepath.Ext(full)); {
	case imageExts[ext]:
		uploadType, itemType = uploadMediaImage, itemImage
	case videoExts[ext]:
		uploadType, itemType = uploadMediaVideo, itemVideo
	case voiceExts[ext]:
		uploadType, itemType = uploadMediaVoice, itemVoice
	}
	sum := md5.Sum(data)
	keyHex := hex.EncodeToString(p.aesKey)
	fk := sha256.Sum256([]byte(clientID + ":file"))
	fileKey := hex.EncodeToString(fk[:])[:32]
	padded := paddedSize(len(data))

	token, base, _ := c.session()
	var up getUploadURLResponse
	err = c.api.do(ctx, request{method: http.MethodPost, base: base, endpoint: "ilink/bot/getuploadurl",
		body: getUploadURLRequest{FileKey: fileKey, MediaType: uploadType, ToUserID: to, RawSize: len(data),
			RawFileMD5: hex.EncodeToString(sum[:]), FileSize: padded, NoNeedThumb: true, AESKey: keyHex,
			BaseInfo: c.api.info},
		token: token, timeout: apiTimeout}, &up)
	if err != nil {
		return mediaAPIError(err)
	}
	uploadURL := strings.TrimSpace(up.UploadFullURL)
	if uploadURL == "" {
		if up.UploadParam == "" {
			return &unsendableError{errors.New("weixin getuploadurl: no upload URL in the response")}
		}
		uploadURL = c.cdnBaseURL + "/upload?encrypted_query_param=" + url.QueryEscape(up.UploadParam) +
			"&filekey=" + url.QueryEscape(fileKey)
	}
	cipher, err := encryptECB(data, p.aesKey)
	if err != nil {
		return &unsendableError{err}
	}
	param, err := c.uploadCDN(ctx, uploadURL, cipher)
	if err != nil {
		return err
	}
	media := &cdnMedia{EncryptQueryParam: param,
		AESKey:      base64.StdEncoding.EncodeToString([]byte(keyHex)),
		EncryptType: encryptTypeAES}
	item := wireItem{Type: itemType}
	switch itemType {
	case itemImage:
		item.ImageItem = &imageItem{Media: media, MidSize: padded}
	case itemVideo:
		item.VideoItem = &videoItem{Media: media, VideoSize: padded}
	case itemVoice:
		item.VoiceItem = &voiceItem{Media: media}
	default:
		item.FileItem = &fileItem{Media: media, FileName: filepath.Base(full), Len: strconv.Itoa(len(data))}
	}
	return mediaAPIError(c.sendItem(ctx, to, item, ctxToken, clientID))
}

// mediaAPIError marks the errors of a media request that a retry would
// not fix unsendable: business errors other than the ones deliver handles
// itself (context and bot token), and non-retryable statuses.
func mediaAPIError(err error) error {
	var ae *apiError
	var he *httpError
	switch {
	case errors.As(err, &ae):
		if ae.is(errcodeContextRestricted) || ae.is(errcodeStaleToken) {
			return err
		}
	case errors.As(err, &he):
		if he.retryable() {
			return err
		}
	default:
		return err
	}
	return &unsendableError{err}
}

// uploadCDN posts the encrypted file and returns the download parameter
// from the x-encrypted-param header. The bot token is not sent, and
// errors leave out the URL (it carries the upload's credentials).
func (c *Channel) uploadCDN(ctx context.Context, u string, body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", &unsendableError{errors.New("weixin cdn upload: invalid upload URL")}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.api.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", redact(fmt.Errorf("weixin cdn upload: %w", err), "")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		he := &httpError{Endpoint: "cdn upload", Status: resp.StatusCode}
		if he.retryable() {
			return "", he
		}
		return "", &unsendableError{he}
	}
	param := resp.Header.Get("x-encrypted-param")
	if param == "" {
		return "", &unsendableError{errors.New("weixin cdn upload: no x-encrypted-param in the response")}
	}
	return param, nil
}

// paddedSize is the length of n bytes after PKCS7 padding to the AES
// block size (openclaw-weixin: aesEcbPaddedSize).
func paddedSize(n int) int { return (n + 1 + 15) / 16 * 16 }

// encryptECB encrypts data with AES-128-ECB and PKCS7 padding.
func encryptECB(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(data)%aes.BlockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	for i := 0; i < len(out); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], out[i:i+aes.BlockSize])
	}
	return out, nil
}
