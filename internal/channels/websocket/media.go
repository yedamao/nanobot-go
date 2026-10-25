package websocket

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// MediaPath serves workspace files for the page: GET
	// /ui/media/<workspace-relative path>. Images in the inline allowlist
	// (see inlineMediaTypes) are served for <img>; anything else is a
	// download.
	MediaPath = "/ui/media/"
	// MaxMediaBytes caps a file served under MediaPath; larger ones are
	// refused with 413.
	MaxMediaBytes = 25 << 20

	// mediaCSP applies when a media file is opened directly rather than
	// through <img>: nothing loads and nothing runs, which keeps an SVG's
	// scripts dead.
	mediaCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"
)

// inlineMediaTypes maps the extensions served inline (as images the page
// previews) to their Content-Type. Every other file is served as
// application/octet-stream with Content-Disposition: attachment.
var inlineMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
}

// MediaRef is one file attached to a message frame.
type MediaRef struct {
	// URL is the file's same-origin MediaPath URL.
	URL  string `json:"url"`
	Name string `json:"name"`
	// Kind is "image" for a file the page previews inline, else "file".
	Kind string `json:"kind"`
}

// mediaKind returns "image" for a name in the inline allowlist, else
// "file".
func mediaKind(name string) string {
	if _, ok := inlineMediaTypes[strings.ToLower(path.Ext(name))]; ok {
		return "image"
	}
	return "file"
}

// mediaRel returns a slash-separated workspace-relative path cleaned of
// "." segments, or false when it is not one the media route serves: empty,
// absolute, with a ".." segment, a hidden (dot) segment, a backslash or a
// NUL. Hidden files and directories (.git, .env, …) are never served.
func mediaRel(rel string) (string, bool) {
	if rel == "" || strings.ContainsAny(rel, "\\\x00") || strings.HasPrefix(rel, "/") {
		return "", false
	}
	var parts []string
	for _, seg := range strings.Split(rel, "/") {
		switch {
		case seg == "" || seg == ".":
		case strings.HasPrefix(seg, "."):
			return "", false
		default:
			parts = append(parts, seg)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "/"), true
}

// mediaURL returns the MediaPath URL of a slash-separated workspace-relative
// path, each segment escaped.
func mediaURL(rel string) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return MediaPath + strings.Join(segs, "/")
}

// serveMedia answers GET /ui/media/{path...} with the workspace file at
// path. The file is opened through an os.Root on the workspace, so ".."
// and symlinks cannot leave it, whatever tools.restrictToWorkspace says;
// only regular files up to MaxMediaBytes are served. Range requests work
// (http.ServeContent).
func (c *Channel) serveMedia(w http.ResponseWriter, r *http.Request) {
	rel, ok := mediaRel(r.PathValue("path"))
	if !ok {
		http.Error(w, "invalid media path", http.StatusBadRequest)
		return
	}
	root, err := c.openWorkspace()
	if err != nil {
		c.log.Warn("websocket: open workspace", "error", err)
		http.Error(w, "cannot open the workspace", http.StatusInternalServerError)
		return
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		// Escaping symlinks and unreadable files alike.
		http.Error(w, "file not available", http.StatusForbidden)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusForbidden)
		return
	}
	if info.Size() > MaxMediaBytes {
		http.Error(w, "file too large to serve", http.StatusRequestEntityTooLarge)
		return
	}
	name := path.Base(rel)
	h := w.Header()
	if typ, ok := inlineMediaTypes[strings.ToLower(path.Ext(name))]; ok {
		h.Set("Content-Type", typ)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	h.Set("Content-Security-Policy", mediaCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// openWorkspace opens the workspace root; the caller closes it.
func (c *Channel) openWorkspace() (*os.Root, error) {
	return os.OpenRoot(c.workspace.Root)
}

// workspaceRel returns the media route's path for p, a file path a reply
// names (absolute or relative to the workspace), or false when p is not an
// existing regular file inside the workspace that the route would serve.
func (c *Channel) workspaceRel(p string) (string, bool) {
	if c.workspace == nil {
		return "", false
	}
	real, err := c.workspace.ResolveRead(p)
	if err != nil || !c.workspace.Contains(real) {
		return "", false
	}
	rel, ok := mediaRel(filepath.ToSlash(c.workspace.Rel(real)))
	if !ok {
		return "", false
	}
	root, err := c.openWorkspace()
	if err != nil {
		return "", false
	}
	defer root.Close()
	info, err := root.Stat(filepath.FromSlash(rel))
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return rel, true
}

// mediaRefs returns the MediaRefs of an outbound message's media paths
// that are workspace files; others (URLs, files elsewhere) are left out.
func (c *Channel) mediaRefs(paths []string) []MediaRef {
	var out []MediaRef
	for _, p := range paths {
		rel, ok := c.workspaceRel(p)
		if !ok {
			c.log.Debug("websocket: media outside the workspace not attached", "path", p)
			continue
		}
		name := path.Base(rel)
		out = append(out, MediaRef{URL: mediaURL(rel), Name: name, Kind: mediaKind(name)})
	}
	return out
}

// MaxChatMedia caps the files attached to one chat frame.
const MaxChatMedia = 16

// mediaPaths returns the absolute workspace paths of a chat frame's
// attachments, or why the frame is rejected. Only the MediaRef URLs are
// trusted, and only as MediaPath URLs naming files the media route would
// serve, so a client cannot attach a file outside the workspace.
func (c *Channel) mediaPaths(refs []MediaRef) ([]string, string) {
	if len(refs) > MaxChatMedia {
		return nil, fmt.Sprintf("too many attachments (at most %d)", MaxChatMedia)
	}
	var out []string
	for _, r := range refs {
		p, ok := c.mediaPath(r.URL)
		if !ok {
			return nil, fmt.Sprintf("attachment %q is not a workspace file", r.URL)
		}
		out = append(out, p)
	}
	return out, ""
}

// mediaPath returns the absolute path of the workspace file at the
// MediaPath URL u, or false when u is not one the media route serves.
func (c *Channel) mediaPath(u string) (string, bool) {
	escaped, ok := strings.CutPrefix(u, MediaPath)
	if !ok || c.workspace == nil {
		return "", false
	}
	rel, err := url.PathUnescape(escaped)
	if err != nil {
		return "", false
	}
	if rel, ok = mediaRel(rel); !ok {
		return "", false
	}
	if rel, ok = c.workspaceRel(filepath.FromSlash(rel)); !ok {
		return "", false
	}
	return filepath.Join(c.workspace.Root, filepath.FromSlash(rel)), true
}

// markdownImageRE matches a markdown image: ![alt](target) or
// ![alt](<target>), with an optional title.
var markdownImageRE = regexp.MustCompile(`!\[([^\]\n]*)\]\((<[^>\n]+>|[^)\s]+)(\s+(?:"[^"\n]*"|'[^'\n]*'))?\)`)

// schemeRE matches a URL scheme ("https:", "data:", "file:").
var schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// rewriteMarkdownImages points markdown images whose target is a file in
// the workspace (a relative path, or an absolute one under the workspace)
// at its MediaPath URL, so the page can show it. Other targets (URLs,
// missing files, files outside the workspace) are left as written.
func (c *Channel) rewriteMarkdownImages(text string) string {
	if c.workspace == nil || !strings.Contains(text, "![") {
		return text
	}
	return markdownImageRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := markdownImageRE.FindStringSubmatch(m)
		target := strings.TrimSuffix(strings.TrimPrefix(sub[2], "<"), ">")
		if schemeRE.MatchString(target) || strings.HasPrefix(target, "#") || strings.HasPrefix(target, MediaPath) {
			return m
		}
		if t, err := url.PathUnescape(target); err == nil {
			target = t
		}
		rel, ok := c.workspaceRel(target)
		if !ok {
			return m
		}
		return "![" + sub[1] + "](" + mediaURL(rel) + sub[3] + ")"
	})
}
