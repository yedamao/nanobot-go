package websocket

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/session"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\nfake")

// mediaWorkspace returns a workspace holding a few files, and a file
// outside it.
func mediaWorkspace(t *testing.T) (*security.Workspace, string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "ws")
	outside := filepath.Join(base, "secret.png")
	files := map[string][]byte{
		filepath.Join(dir, "out", "chart.png"):   pngBytes,
		filepath.Join(dir, "out", "my file.png"): pngBytes,
		filepath.Join(dir, "notes.txt"):          []byte("notes"),
		filepath.Join(dir, "report.pdf"):         []byte("%PDF"),
		filepath.Join(dir, "draw.svg"):           []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"),
		filepath.Join(dir, ".env"):               []byte("KEY=1"),
		filepath.Join(dir, ".git", "config.png"): pngBytes,
		outside:                                  pngBytes,
	}
	for p, data := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.png")); err != nil {
		t.Fatal(err)
	}
	// os.Root follows relative symlinks that stay inside the root.
	if err := os.Symlink(filepath.Join("out", "chart.png"), filepath.Join(dir, "inside.png")); err != nil {
		t.Fatal(err)
	}
	ws, err := security.NewWorkspace(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return ws, outside
}

func mediaChannel(t *testing.T, cfg Config, host string, ws *security.Workspace) (*Channel, *http.ServeMux) {
	t.Helper()
	ch, err := New(cfg, channels.Deps{Bus: bus.NewMessageBus(8)}, Options{Host: host, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ch.Mount(mux)
	return ch, mux
}

func TestServeMedia(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	_, mux := mediaChannel(t, Config{}, "127.0.0.1", ws)
	get := func(target string) (int, http.Header, string) {
		rec := do(mux, http.MethodGet, target, "127.0.0.1:18790", "127.0.0.1:5555", nil)
		return rec.Code, rec.Header(), rec.Body.String()
	}

	code, h, body := get("/ui/media/out/chart.png")
	if code != http.StatusOK || h.Get("Content-Type") != "image/png" || body != string(pngBytes) {
		t.Errorf("png: %d %q %q", code, h.Get("Content-Type"), body)
	}
	if h.Get("Content-Disposition") != "" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("png headers: %v", h)
	}
	if code, _, _ := get("/ui/media/out/my%20file.png"); code != http.StatusOK {
		t.Errorf("escaped name: %d", code)
	}
	if code, h, _ := get("/ui/media/draw.svg"); code != http.StatusOK || h.Get("Content-Type") != "image/svg+xml" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("svg: %d %v", code, h)
	}
	for _, name := range []string{"notes.txt", "report.pdf"} {
		code, h, _ := get("/ui/media/" + name)
		if code != http.StatusOK || h.Get("Content-Type") != "application/octet-stream" || h.Get("Content-Disposition") != `attachment; filename=`+name {
			t.Errorf("%s: %d %q %q", name, code, h.Get("Content-Type"), h.Get("Content-Disposition"))
		}
	}
	if code, _, _ := get("/ui/media/inside.png"); code != http.StatusOK {
		t.Errorf("symlink within the workspace: %d", code)
	}
	if code, _, _ := get("/ui/media/missing.png"); code != http.StatusNotFound {
		t.Errorf("missing: %d", code)
	}
	for target, want := range map[string]int{
		"/ui/media/escape.png":               http.StatusForbidden, // symlink out of the workspace
		"/ui/media/out":                      http.StatusForbidden, // a directory
		"/ui/media/..%2fsecret.png":          http.StatusBadRequest,
		"/ui/media/out/..%2f..%2fsecret.png": http.StatusBadRequest,
		"/ui/media/%2ftmp%2fx.png":           http.StatusBadRequest,
		"/ui/media/.env":                     http.StatusBadRequest,
		"/ui/media/.git/config.png":          http.StatusBadRequest,
		"/ui/media/out%5c..%5cchart.png":     http.StatusBadRequest,
	} {
		if code, _, body := get(target); code != want {
			t.Errorf("%s: %d %q, want %d", target, code, body, want)
		}
	}
}

func TestServeMediaRange(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	_, mux := mediaChannel(t, Config{}, "127.0.0.1", ws)
	rec := do(mux, http.MethodGet, "/ui/media/notes.txt", "127.0.0.1:18790", "127.0.0.1:5555", func(r *http.Request) {
		r.Header.Set("Range", "bytes=0-1")
	})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "no" {
		t.Errorf("range: %d %q", rec.Code, rec.Body.String())
	}
}

func TestServeMediaTooLarge(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	big := filepath.Join(ws.Root, "big.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxMediaBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, mux := mediaChannel(t, Config{}, "127.0.0.1", ws)
	if rec := do(mux, http.MethodGet, "/ui/media/big.png", "127.0.0.1:18790", "127.0.0.1:5555", nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", rec.Code)
	}
}

func TestServeMediaAuth(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	ch, mux := mediaChannel(t, Config{Token: testToken}, "0.0.0.0", ws)
	const target = "/ui/media/out/chart.png"
	if rec := do(mux, http.MethodGet, target, lanHost, "192.168.1.9:5555", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("LAN without sign-in: %d", rec.Code)
	}
	if rec := do(mux, http.MethodGet, target, lanHost, "192.168.1.9:5555", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}); rec.Code != http.StatusOK {
		t.Errorf("LAN with token: %d", rec.Code)
	}
	id := ch.auth.newSession()
	if rec := do(mux, http.MethodGet, target, lanHost, "192.168.1.9:5555", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: id})
	}); rec.Code != http.StatusOK {
		t.Errorf("LAN with session cookie: %d", rec.Code)
	}
	if rec := do(mux, http.MethodGet, target+"?token="+testToken, lanHost, "192.168.1.9:5555", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("LAN with ?token=: %d (only /ws takes it)", rec.Code)
	}

	_, local := mediaChannel(t, Config{}, "127.0.0.1", ws)
	if rec := do(local, http.MethodGet, target, lanHost, "192.168.1.9:5555", nil); rec.Code != http.StatusForbidden {
		t.Errorf("LAN with no secret configured: %d", rec.Code)
	}
}

func TestMediaUnmountedWithoutWorkspace(t *testing.T) {
	_, mux := mediaChannel(t, Config{}, "127.0.0.1", nil)
	rec := do(mux, http.MethodGet, "/ui/media/x.png", "127.0.0.1:18790", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("media without a workspace: %d, want 404", rec.Code)
	}
}

func TestRewriteMarkdownImages(t *testing.T) {
	ws, outside := mediaWorkspace(t)
	ch, _ := mediaChannel(t, Config{}, "127.0.0.1", ws)
	abs := filepath.Join(ws.Root, "out", "chart.png")
	for in, want := range map[string]string{
		"see ![chart](out/chart.png) here":        "see ![chart](/ui/media/out/chart.png) here",
		"![c](./out/chart.png)":                   "![c](/ui/media/out/chart.png)",
		"![c](" + abs + ")":                       "![c](/ui/media/out/chart.png)",
		"![c](<out/my file.png>)":                 "![c](/ui/media/out/my%20file.png)",
		"![c](out/my%20file.png)":                 "![c](/ui/media/out/my%20file.png)",
		`![c](out/chart.png "Title")`:             `![c](/ui/media/out/chart.png "Title")`,
		"![a](out/chart.png) and ![b](notes.txt)": "![a](/ui/media/out/chart.png) and ![b](/ui/media/notes.txt)",
		"![c](inside.png)":                        "![c](/ui/media/out/chart.png)",
		"![c](" + outside + ")":                   "![c](" + outside + ")",
		"![c](../secret.png)":                     "![c](../secret.png)",
		"![c](escape.png)":                        "![c](escape.png)",
		"![c](missing.png)":                       "![c](missing.png)",
		"![c](.env)":                              "![c](.env)",
		"![c](out)":                               "![c](out)",
		"![c](https://example.com/c.png)":         "![c](https://example.com/c.png)",
		"![c](data:image/png;base64,AAAA)":        "![c](data:image/png;base64,AAAA)",
		"![c](/ui/media/out/chart.png)":           "![c](/ui/media/out/chart.png)",
		"[link](out/chart.png)":                   "[link](out/chart.png)",
		"`![c](out/chart.png)` is markdown":       "`![c](/ui/media/out/chart.png)` is markdown",
		"no images":                               "no images",
	} {
		if got := ch.rewriteMarkdownImages(in); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", in, got, want)
		}
	}
	none, _ := mediaChannel(t, Config{}, "127.0.0.1", nil)
	if got := none.rewriteMarkdownImages("![c](out/chart.png)"); got != "![c](out/chart.png)" {
		t.Errorf("without workspace: %q", got)
	}
}

func TestMessageFrameMedia(t *testing.T) {
	ws, outside := mediaWorkspace(t)
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Workspace: ws})
	conn, chatID := h.dial(t, "")
	err := h.ch.Send(testCtx(t), bus.OutboundMessage{
		Channel: Name, ChatID: chatID,
		Content: "here ![c](out/chart.png)",
		Media:   []string{"out/chart.png", filepath.Join(ws.Root, "report.pdf"), outside, "https://example.com/x.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := readFrame(t, conn)
	if f.Type != TypeMessage || f.Content != "here ![c](/ui/media/out/chart.png)" {
		t.Errorf("frame = %+v", f)
	}
	want := []MediaRef{
		{URL: "/ui/media/out/chart.png", Name: "chart.png", Kind: "image"},
		{URL: "/ui/media/report.pdf", Name: "report.pdf", Kind: "file"},
	}
	if len(f.Media) != len(want) || f.Media[0] != want[0] || f.Media[1] != want[1] {
		t.Errorf("media = %+v, want %+v", f.Media, want)
	}
}

func TestHistoryRewritesImages(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	store := session.NewJSONLStore(t.TempDir())
	sess := session.New(Name + ":c1")
	sess.Append(
		providers.Message{Role: providers.RoleUser, Content: "look ![u](out/chart.png)"},
		providers.Message{Role: providers.RoleAssistant, Content: "ok ![a](notes.txt)"},
	)
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	ch, err := New(Config{}, channels.Deps{Bus: bus.NewMessageBus(1)}, Options{Host: "127.0.0.1", Sessions: store, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	got := ch.history("c1")
	if len(got) != 2 || got[0].Content != "look ![u](/ui/media/out/chart.png)" || got[1].Content != "ok ![a](/ui/media/notes.txt)" {
		t.Errorf("history = %+v", got)
	}
}
