package websocket

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// uploadRequest returns a POST /ui/upload request whose "file" part is
// data named filename, from this device unless edit changes it.
func uploadRequest(t *testing.T, filename string, data []byte, edit func(*http.Request)) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("note", "ignored")
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, UploadPath, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Host = "127.0.0.1:18790"
	req.RemoteAddr = "127.0.0.1:5555"
	if edit != nil {
		edit(req)
	}
	return req
}

func serve(mux http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestUpload(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	_, mux := mediaChannel(t, Config{}, "127.0.0.1", ws)
	rec := serve(mux, uploadRequest(t, "../../My Chart.PNG", pngBytes, nil))
	if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("upload: %d %q", rec.Code, rec.Body.String())
	}
	var res UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^uploads/[a-z0-9]{8}-My_Chart\.PNG$`).MatchString(res.Path) ||
		res.URL != MediaPath+res.Path || res.Name != "My_Chart.PNG" || res.Kind != "image" {
		t.Errorf("result = %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(res.Path)))
	if err != nil || !bytes.Equal(data, pngBytes) {
		t.Errorf("saved file = %q, %v", data, err)
	}
	// The saved file is served back.
	if got := do(mux, http.MethodGet, res.URL, "127.0.0.1:18790", "127.0.0.1:5555", nil); got.Code != http.StatusOK || got.Body.String() != string(pngBytes) {
		t.Errorf("serve upload: %d", got.Code)
	}
	// Markdown naming the saved path points at it.
	ch, _ := mediaChannel(t, Config{}, "127.0.0.1", ws)
	if got := ch.rewriteMarkdownImages("![c](" + res.Path + ")"); got != "![c]("+res.URL+")" {
		t.Errorf("rewrite = %q", got)
	}
}

func TestUploadRefused(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	_, mux := mediaChannel(t, Config{Token: testToken}, "0.0.0.0", ws)

	if rec := serve(mux, uploadRequest(t, "a.txt", []byte("x"), func(r *http.Request) {
		r.Host, r.RemoteAddr = lanHost, "192.168.1.9:5555"
	})); rec.Code != http.StatusUnauthorized {
		t.Errorf("LAN without sign-in: %d", rec.Code)
	}
	if rec := serve(mux, uploadRequest(t, "a.txt", []byte("x"), func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin: %d", rec.Code)
	}
	if rec := serve(mux, uploadRequest(t, "big.bin", make([]byte, MaxUploadBytes+1), nil)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, UploadPath, strings.NewReader("{}"))
	req.Host, req.RemoteAddr = "127.0.0.1:18790", "127.0.0.1:5555"
	req.Header.Set("Content-Type", "application/json")
	if rec := serve(mux, req); rec.Code != http.StatusBadRequest {
		t.Errorf("not multipart: %d", rec.Code)
	}
	entries, _ := os.ReadDir(filepath.Join(ws.Root, UploadDir))
	if len(entries) != 0 {
		t.Errorf("refused uploads left files: %v", entries)
	}

	_, none := mediaChannel(t, Config{}, "127.0.0.1", nil)
	if rec := serve(none, uploadRequest(t, "a.txt", []byte("x"), nil)); rec.Code == http.StatusCreated {
		t.Error("upload without a workspace succeeded")
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":                      "report.pdf",
		"../../etc/passwd":                "passwd",
		`C:\Users\me\photo.jpg`:           "photo.jpg",
		".env":                            "env",
		"...":                             "file",
		"":                                "file",
		"résumé 2026.pdf":                 "r_sum__2026.pdf",
		"a\x00b.png":                      "a_b.png",
		strings.Repeat("x", 200) + ".png": strings.Repeat("x", 76) + ".png",
	} {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
