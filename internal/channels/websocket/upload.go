package websocket

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	// UploadPath takes a multipart form whose "file" part is saved to the
	// workspace's UploadDir; it answers 201 with an UploadResult.
	UploadPath = "/ui/upload"
	// UploadDir is the workspace directory uploads are saved to.
	UploadDir = "uploads"
	// MaxUploadBytes caps one uploaded file; larger ones are refused with
	// 413 and nothing is kept.
	MaxUploadBytes = 10 << 20
	// uploadOverhead allows for the multipart framing around the file.
	uploadOverhead = 64 << 10
	// maxUploadNameRunes caps a saved file's name, before its random
	// prefix.
	maxUploadNameRunes = 80
)

// UploadResult is the body of a successful POST /ui/upload.
type UploadResult struct {
	// Path is the saved file's path relative to the workspace, which the
	// agent's file tools take.
	Path string `json:"path"`
	// URL is its media route URL.
	URL  string `json:"url"`
	Name string `json:"name"`
	// Kind is "image" or "file", as in MediaRef.
	Kind string `json:"kind"`
}

// safeFilename returns name's base with every character but ASCII letters,
// digits, '.', '-' and '_' replaced by '_', leading dots dropped (no
// hidden files) and cut to maxUploadNameRunes, keeping the extension; an
// empty result is "file".
func safeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if n := utf8.RuneCountInString(s); n > maxUploadNameRunes {
		ext := path.Ext(s)
		if len(ext) > 16 {
			ext = ""
		}
		s = s[:maxUploadNameRunes-len(ext)] + ext
	}
	if strings.Trim(s, "._-") == "" {
		return "file"
	}
	return s
}

// upload answers POST /ui/upload: it saves the form's "file" part as
// UploadDir/<random>-<safe name> in the workspace, through an os.Root so
// it cannot be written elsewhere, and answers 201 with an UploadResult.
func (c *Channel) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+uploadOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "body must be multipart/form-data with a file part", http.StatusBadRequest)
		return
	}
	var part io.ReadCloser
	var filename string
	for {
		p, err := mr.NextPart()
		if err != nil {
			if tooLarge(err) {
				http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "body must be multipart/form-data with a file part", http.StatusBadRequest)
			return
		}
		if p.FormName() == "file" {
			part, filename = p, p.FileName()
			break
		}
		p.Close()
	}
	defer part.Close()

	root, err := c.openWorkspace()
	if err != nil {
		c.log.Warn("websocket: open workspace", "error", err)
		http.Error(w, "cannot open the workspace", http.StatusInternalServerError)
		return
	}
	defer root.Close()
	if err := root.MkdirAll(UploadDir, 0o700); err != nil {
		c.log.Warn("websocket: create upload directory", "error", err)
		http.Error(w, "cannot save the file", http.StatusInternalServerError)
		return
	}
	name := safeFilename(filename)
	rel := UploadDir + "/" + strings.ToLower(rand.Text()[:8]) + "-" + name
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		c.log.Warn("websocket: create upload", "error", err)
		http.Error(w, "cannot save the file", http.StatusInternalServerError)
		return
	}
	n, err := io.Copy(f, io.LimitReader(part, MaxUploadBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || n > MaxUploadBytes {
		root.Remove(rel)
		switch {
		case n > MaxUploadBytes || tooLarge(err):
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		default:
			c.log.Warn("websocket: save upload", "error", err)
			http.Error(w, "cannot save the file", http.StatusBadRequest)
		}
		return
	}
	c.log.Info("websocket: file uploaded", "path", rel, "bytes", n)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(UploadResult{Path: rel, URL: mediaURL(rel), Name: name, Kind: mediaKind(name)})
}

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
