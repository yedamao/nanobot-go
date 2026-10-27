package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/security"
)

const (
	// MediaDir is the workspace directory inbound photos are saved to
	// (nanobot: get_media_dir("telegram")).
	MediaDir = "media/telegram"
	// maxPhotoBytes caps a downloaded photo at what the providers inline;
	// a larger one could never reach the model.
	maxPhotoBytes = providers.MaxImageBytes
	// maxClipBytes caps a downloaded voice note, audio file, document,
	// video, video note or sticker at the Bot API's getFile limit (20 MB; bots
	// cannot download larger files, so nanobot has no cap of its own).
	maxClipBytes = 20 << 20
	// transcribeTimeout bounds the transcription of one clip; the poll
	// loop waits for it.
	transcribeTimeout = 60 * time.Second
	// maxFileExtLen caps the extension a document takes from its file
	// name.
	maxFileExtLen = 32
)

// audioExts maps the MIME type of a voice note or audio file to its file
// extension.
var audioExts = map[string]string{
	"audio/ogg":   ".ogg",
	"audio/mpeg":  ".mp3",
	"audio/mp4":   ".m4a",
	"audio/x-m4a": ".m4a",
	"audio/wav":   ".wav",
	"audio/x-wav": ".wav",
	"audio/flac":  ".flac",
}

// videoExts maps the MIME type of a video to its file extension
// (nanobot's _get_extension).
var videoExts = map[string]string{
	"video/mp4":        ".mp4",
	"video/quicktime":  ".mov",
	"video/webm":       ".webm",
	"video/x-matroska": ".mkv",
	"video/3gpp":       ".3gp",
}

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
	data, err := c.fetchFile(ctx, p.FileID, maxPhotoBytes)
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

// fetchFile resolves fileID with getFile and downloads it, refusing a
// file over limit bytes.
func (c *Channel) fetchFile(ctx context.Context, fileID string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	var f file
	if err := c.api.call(ctx, "getFile", getFileParams{FileID: fileID}, &f); err != nil {
		return nil, err
	}
	if f.FilePath == "" {
		return nil, errors.New("telegram getFile: no file_path")
	}
	if f.FileSize > limit {
		return nil, fmt.Errorf("%w (%d bytes, limit %d)", errTooLarge, f.FileSize, limit)
	}
	return c.api.download(ctx, f.FilePath, limit)
}

// clip is the voice note, audio file, document, video, video note or
// sticker of a message.
type clip struct {
	// kind is "voice", "audio", "file" (a document, as nanobot tags it),
	// "video", "video_note" or "sticker", the tag of its content part.
	kind     string
	fileID   string
	uniqueID string
	mimeType string
	// fileName is the sender's name for a document, used only for its
	// extension.
	fileName string
	// stickerExt is the extension of a sticker, which has no MIME type.
	stickerExt string
	size       int64
}

// clipOf returns the voice note, audio file, document, video, video note
// or sticker of m, if it has one, in nanobot's order (stickers, which
// nanobot does not handle, last).
func clipOf(m *message) (clip, bool) {
	switch {
	case m.Voice != nil:
		v := m.Voice
		return clip{kind: "voice", fileID: v.FileID, uniqueID: v.FileUniqueID, mimeType: v.MimeType, size: v.FileSize}, true
	case m.Audio != nil:
		a := m.Audio
		return clip{kind: "audio", fileID: a.FileID, uniqueID: a.FileUniqueID, mimeType: a.MimeType, size: a.FileSize}, true
	case m.Document != nil:
		d := m.Document
		return clip{kind: "file", fileID: d.FileID, uniqueID: d.FileUniqueID, mimeType: d.MimeType, fileName: d.FileName, size: d.FileSize}, true
	case m.Video != nil:
		v := m.Video
		return clip{kind: "video", fileID: v.FileID, uniqueID: v.FileUniqueID, mimeType: v.MimeType, size: v.FileSize}, true
	case m.VideoNote != nil:
		v := m.VideoNote
		return clip{kind: "video_note", fileID: v.FileID, uniqueID: v.FileUniqueID, size: v.FileSize}, true
	case m.Sticker != nil:
		s := m.Sticker
		ext := ".webp"
		switch {
		case s.IsVideo:
			ext = ".webm"
		case s.IsAnimated:
			ext = ".tgs"
		}
		return clip{kind: "sticker", fileID: s.FileID, uniqueID: s.FileUniqueID, stickerExt: ext, size: s.FileSize}, true
	}
	return clip{}, false
}

// ext is the file extension of a clip: by MIME type, then by kind (like
// nanobot's _get_extension); a document without a known MIME type takes
// the suffixes of its file name, else none; a sticker's comes from its
// type.
func (k clip) ext() string {
	mime := strings.ToLower(k.mimeType)
	switch k.kind {
	case "sticker":
		return k.stickerExt
	case "file":
		for _, exts := range []map[string]string{photoExts, audioExts, videoExts} {
			if ext, ok := exts[mime]; ok {
				return ext
			}
		}
		return nameExt(k.fileName)
	case "video", "video_note":
		if ext, ok := videoExts[mime]; ok {
			return ext
		}
		return ".mp4"
	}
	if ext, ok := audioExts[mime]; ok {
		return ext
	}
	if k.kind == "voice" {
		return ".ogg"
	}
	return ".mp3"
}

// nameExt returns the suffixes of a file name, like nanobot's
// "".join(Path(filename).suffixes) ("report.tar.gz" gives ".tar.gz"),
// but only the trailing ones made of letters, digits, '-' and '_', at
// most maxFileExtLen bytes in all, so a sender's name can never add a
// separator, "..", or anything else to the saved path. "" when there is
// none.
func nameExt(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	if strings.HasSuffix(name, ".") {
		return "" // Path("a.").suffixes is []
	}
	parts := strings.Split(strings.TrimLeft(name, "."), ".")[1:]
	ext := ""
	for i := len(parts) - 1; i >= 0; i-- {
		if !safeID(parts[i]) || len(ext)+1+len(parts[i]) > maxFileExtLen {
			break
		}
		ext = "." + parts[i] + ext
	}
	return ext
}

// transcribable reports whether a clip is sent to the Transcriber:
// voice notes and audio files are, videos are not (as in nanobot).
func (k clip) transcribable() bool {
	return k.kind == "voice" || k.kind == "audio"
}

// downloadClip saves a voice note, audio file, document, video, video
// note or sticker to MediaDir in the workspace as <file_unique_id>.<ext> (mode 0600)
// and returns its workspace-relative path with the content part that
// stands for it: for audio, the transcript as "[transcription: …]" when
// a Transcriber is set and returns text; else "[<kind>: <path>]", e.g.
// "[voice: <path>]", "[file: <path>]", "[video_note: <path>]" or
// "[sticker: <path>]" (nanobot's _download_message_media).
func (c *Channel) downloadClip(ctx context.Context, k clip) (rel, part string, err error) {
	if c.workspace == "" {
		return "", "", errors.New("no workspace to save it to")
	}
	if !safeID(k.uniqueID) {
		return "", "", fmt.Errorf("unusable file_unique_id %q", k.uniqueID)
	}
	if k.size > maxClipBytes {
		return "", "", fmt.Errorf("%w (%d bytes, limit %d)", errTooLarge, k.size, maxClipBytes)
	}
	data, err := c.fetchFile(ctx, k.fileID, maxClipBytes)
	if err != nil {
		return "", "", err
	}
	rel = path.Join(MediaDir, k.uniqueID+k.ext())
	if err := fsutil.WriteInRoot(c.workspace, rel, data); err != nil {
		return "", "", err
	}
	if k.transcribable() {
		if text := c.transcribe(ctx, rel, data); text != "" {
			return rel, "[transcription: " + text + "]", nil
		}
	}
	return rel, "[" + k.kind + ": " + rel + "]", nil
}

// transcribe returns the transcript of an audio file, or "" without a
// Transcriber or when it fails (logged).
func (c *Channel) transcribe(ctx context.Context, name string, data []byte) string {
	if c.transcriber == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()
	text, err := c.transcriber.Transcribe(ctx, name, data)
	if err != nil {
		c.log.Warn("telegram: transcription failed", "file", name, "error", err)
		return ""
	}
	return strings.TrimSpace(text)
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
	// limits for sendPhoto and for the other methods (sendDocument,
	// sendAnimation, sendVideo, sendVoice, sendAudio).
	maxUploadPhotoBytes = 10 << 20
	maxUploadBytes      = 50 << 20
)

// outboundMedia is how an outbound file is sent: its Bot API method and
// the form field that carries the file.
type outboundMedia struct{ method, field string }

var (
	asPhoto     = outboundMedia{"sendPhoto", "photo"}
	asAnimation = outboundMedia{"sendAnimation", "animation"}
	asVideo     = outboundMedia{"sendVideo", "video"}
	asVoice     = outboundMedia{"sendVoice", "voice"}
	asAudio     = outboundMedia{"sendAudio", "audio"}
	asDocument  = outboundMedia{"sendDocument", "document"}
)

// outboundExts maps the extension of an outbound file other than a still
// image to how it is sent (nanobot's _get_media_type, except that a GIF
// goes as an animation rather than a photo). It is checked before
// providers.IsImagePath, which also counts .gif as an image.
var outboundExts = map[string]outboundMedia{
	".gif": asAnimation,
	".mp4": asVideo, ".mov": asVideo, ".avi": asVideo, ".mkv": asVideo, ".webm": asVideo, ".3gp": asVideo,
	".ogg": asVoice,
	".mp3": asAudio, ".m4a": asAudio, ".wav": asAudio, ".aac": asAudio,
}

// outboundMediaOf returns how the file named name (a path or a URL's base
// name) is sent, by its extension: GIFs as animations, other images as
// photos, videos, .ogg as voice notes, audio files, and anything else as
// a document.
func outboundMediaOf(name string) outboundMedia {
	if m, ok := outboundExts[strings.ToLower(path.Ext(name))]; ok {
		return m
	}
	if providers.IsImagePath(name) {
		return asPhoto
	}
	return asDocument
}

// outboundFile is an outbound file ready to send: a local file's contents
// or a checked remote URL, with how it goes.
type outboundFile struct {
	// name is the file name of a local file, for the upload.
	name string
	kind outboundMedia
	data []byte
	// url is the http(s) URL of a remote file, "" for a local one.
	url string
}

// loadFile prepares the outbound file p by its extension (like nanobot's
// _get_media_type): GIFs go through sendAnimation, other images through
// sendPhoto (a local one over the photo upload limit as a document),
// videos through sendVideo, .ogg through sendVoice, audio files through
// sendAudio and anything else through sendDocument.
// Local files are read; relative paths resolve against the workspace.
// An http(s) URL is checked against the SSRF policy, to be handed to the
// Bot API, which fetches it (nanobot's validate_url_target, then
// send_photo(url)).
func (c *Channel) loadFile(ctx context.Context, p string) (outboundFile, error) {
	if security.IsHTTPURL(p) {
		u := strings.TrimSpace(p)
		if err := security.CheckURL(ctx, u, c.ssrf); err != nil {
			return outboundFile{}, err
		}
		return outboundFile{kind: outboundMediaOf(security.URLName(u)), url: u}, nil
	}
	full := p
	if !filepath.IsAbs(full) {
		if c.workspace == "" {
			return outboundFile{}, errors.New("no workspace to resolve a relative path against")
		}
		full = filepath.Join(c.workspace, filepath.FromSlash(p))
	}
	data, err := fsutil.ReadRegularFile(full, maxUploadBytes+1)
	if err != nil {
		return outboundFile{}, err
	}
	if len(data) > maxUploadBytes {
		return outboundFile{}, fmt.Errorf("%w (limit %d bytes)", errTooLarge, maxUploadBytes)
	}
	name := filepath.Base(full)
	kind := outboundMediaOf(name)
	if kind == asPhoto && len(data) > maxUploadPhotoBytes {
		kind = asDocument
	}
	return outboundFile{name: name, kind: kind, data: data}, nil
}

// sendFields returns the form fields every outbound file method shares;
// markup, when set, is the inline keyboard of the message.
func sendFields(chatID, threadID int64, reply *replyParameters, markup *inlineKeyboardMarkup) (map[string]string, error) {
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10)}
	if threadID != 0 {
		fields["message_thread_id"] = strconv.FormatInt(threadID, 10)
	}
	if reply != nil {
		rp, err := json.Marshal(reply)
		if err != nil {
			return nil, err
		}
		fields["reply_parameters"] = string(rp)
	}
	if markup != nil {
		rm, err := json.Marshal(markup)
		if err != nil {
			return nil, err
		}
		fields["reply_markup"] = string(rm)
	}
	return fields, nil
}

// sendFile sends one loaded file as its kind (a video with
// supports_streaming), or through sendDocument when Telegram refuses it
// as that (400: dimensions, size, format). A local file is uploaded; a
// remote one goes by reference, the file field (photo, animation, video,
// document, ...) being the URL itself. markup, when set, is the inline
// keyboard of the message.
func (c *Channel) sendFile(ctx context.Context, chatID, threadID int64, reply *replyParameters, markup *inlineKeyboardMarkup, f outboundFile, caption string) error {
	fields, err := sendFields(chatID, threadID, reply, markup)
	if err != nil {
		return err
	}
	if caption != "" {
		fields["caption"] = caption
	}
	return c.sendMedia(ctx, chatID, f.kind, func(ctx context.Context, m outboundMedia) error {
		mf := mediaFields(fields, m)
		if f.url != "" {
			mf[m.field] = f.url
			return c.api.upload(ctx, m.method, mf, "", "", nil, nil)
		}
		return c.api.upload(ctx, m.method, mf, m.field, f.name, f.data, nil)
	})
}

// albumGroup returns the album a file sent as m can share with others
// (sendMediaGroup): photos and videos mix, documents and audio files
// each group only with their own kind, and voice notes and animations
// never go in one (""), as the Bot API has it.
func albumGroup(m outboundMedia) string {
	switch m {
	case asPhoto, asVideo:
		return "visual"
	case asDocument, asAudio:
		return m.field
	}
	return ""
}

// albumLen returns how many of files, from the first, can go as one
// album: the run of loaded ones (no error in errs) of one albumGroup.
func albumLen(files []outboundFile, errs []error) int {
	if len(files) == 0 || errs[0] != nil {
		return 0
	}
	g := albumGroup(files[0].kind)
	if g == "" {
		return 0
	}
	n := 1
	for n < len(files) && errs[n] == nil && albumGroup(files[n].kind) == g {
		n++
	}
	return n
}

// inputMedia is one item of a sendMediaGroup album (InputMediaPhoto,
// InputMediaVideo, InputMediaAudio, InputMediaDocument).
type inputMedia struct {
	Type              string `json:"type"`
	Media             string `json:"media"`
	Caption           string `json:"caption,omitempty"`
	SupportsStreaming bool   `json:"supports_streaming,omitempty"`
}

// sendAlbum sends 2-10 files of one albumGroup as one album through
// sendMediaGroup, the caption on the first item (where Telegram shows an
// album's caption). Local files are uploaded as attach://file<i>; remote
// ones go by URL. An album takes no inline keyboard.
func (c *Channel) sendAlbum(ctx context.Context, chatID, threadID int64, reply *replyParameters, files []outboundFile, caption string) error {
	fields, err := sendFields(chatID, threadID, reply, nil)
	if err != nil {
		return err
	}
	items := make([]inputMedia, len(files))
	var uploads []formFile
	for i, f := range files {
		items[i] = inputMedia{Type: f.kind.field, Media: f.url, SupportsStreaming: f.kind == asVideo}
		if f.url == "" {
			attach := "file" + strconv.Itoa(i)
			items[i].Media = "attach://" + attach
			uploads = append(uploads, formFile{field: attach, name: f.name, data: f.data})
		}
	}
	items[0].Caption = caption
	media, err := json.Marshal(items)
	if err != nil {
		return err
	}
	fields["media"] = string(media)
	return c.retrySend(ctx, chatID, func(ctx context.Context) error {
		return c.api.uploadFiles(ctx, "sendMediaGroup", fields, uploads, nil)
	})
}

// mediaName is the file name of an outbound media path or URL.
func mediaName(p string) string {
	if security.IsHTTPURL(p) {
		return security.URLName(p)
	}
	return path.Base(filepath.ToSlash(p))
}

// mediaFields returns a copy of the form fields of a file sent as m, with
// supports_streaming set for a video (as nanobot does).
func mediaFields(fields map[string]string, m outboundMedia) map[string]string {
	f := maps.Clone(fields)
	if m == asVideo {
		f["supports_streaming"] = "true"
	}
	return f
}

// sendMedia sends a file as kind, falling back to sendDocument when
// Telegram refuses a photo, animation, video, voice note or audio file
// (400). send makes one attempt as the given kind.
func (c *Channel) sendMedia(ctx context.Context, chatID int64, kind outboundMedia, send func(ctx context.Context, m outboundMedia) error) error {
	if kind != asDocument {
		err := c.retrySend(ctx, chatID, func(ctx context.Context) error {
			return send(ctx, kind)
		})
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != http.StatusBadRequest {
			return err
		}
		c.log.Info("telegram: "+kind.field+" refused; sending it as a file", "chat_id", chatID, "error", err)
	}
	return c.retrySend(ctx, chatID, func(ctx context.Context) error {
		return send(ctx, asDocument)
	})
}
