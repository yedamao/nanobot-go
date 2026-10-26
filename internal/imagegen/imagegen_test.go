package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngDataURL(t *testing.T) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t))
}

func TestDecodeDataURL(t *testing.T) {
	raw, mime, err := DecodeDataURL(pngDataURL(t))
	if err != nil || mime != "image/png" || !bytes.Equal(raw, tinyPNG(t)) {
		t.Fatalf("DecodeDataURL = %d bytes, %q, %v", len(raw), mime, err)
	}
	// The sniffed type wins over the declared one.
	jpegDeclared := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t))
	if _, mime, err := DecodeDataURL(jpegDeclared); err != nil || mime != "image/png" {
		t.Errorf("declared jpeg = %q, %v", mime, err)
	}
	for _, bad := range []string{
		"",
		"https://example.com/a.png",
		"data:text/plain;base64,aGVsbG8=",
		"data:image/png,notbase64",
		"data:image/png;base64,!!!",
		"data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("<svg></svg>")),
	} {
		if _, _, err := DecodeDataURL(bad); err == nil {
			t.Errorf("DecodeDataURL(%q) succeeded", bad)
		}
	}
}

func TestStoreWritesImageAndSidecar(t *testing.T) {
	ws := t.TempDir()
	now := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)
	a, err := Store(pngDataURL(t), StoreOptions{Workspace: ws, SaveDir: "generated", Prompt: "a cat",
		Model: "m", Provider: "openrouter", SourceImages: []string{"/x/ref.png"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ws, "media", "generated", "2026-10-26")
	if filepath.Dir(a.Path) != dir || !strings.HasPrefix(filepath.Base(a.Path), "img_") || filepath.Ext(a.Path) != ".png" {
		t.Errorf("path = %s", a.Path)
	}
	if len(a.ID) != len("img_")+12 || a.MIME != "image/png" || a.Prompt != "a cat" || a.CreatedAt != "2026-10-26T09:30:00Z" {
		t.Errorf("artifact = %+v", a)
	}
	info, err := os.Stat(a.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("image stat = %v, %v", info, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta Artifact
	if err := json.Unmarshal(data, &meta); err != nil || meta.Path != a.Path || meta.SourceImages[0] != "/x/ref.png" {
		t.Errorf("sidecar = %s (%v)", data, err)
	}
	res := ToolResult([]Artifact{a})
	if !strings.Contains(res, `"next_step"`) || !strings.Contains(res, "message tool") || !strings.Contains(res, a.ID) {
		t.Errorf("ToolResult = %s", res)
	}
}

func TestStoreRejectsUnsafeSaveDirAndBadData(t *testing.T) {
	ws := t.TempDir()
	for _, dir := range []string{"", "..", "../out", "a/../../b", "/abs", "./x"} {
		if _, err := Store(pngDataURL(t), StoreOptions{Workspace: ws, SaveDir: dir}); err == nil {
			t.Errorf("saveDir %q accepted", dir)
		}
	}
	if _, err := Store("data:image/png;base64,aGVsbG8=", StoreOptions{Workspace: ws, SaveDir: "g"}); err == nil {
		t.Error("non-image stored")
	}
	// A symlink in the save path cannot redirect the write outside.
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "media"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "media", "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := Store(pngDataURL(t), StoreOptions{Workspace: ws, SaveDir: "evil"}); err == nil {
		t.Error("write through symlink escaping the workspace succeeded")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("files written outside: %v", entries)
	}
}

func TestOpenRouterGenerate(t *testing.T) {
	dataURL := pngDataURL(t)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" ||
			r.Header.Get("X-Extra") != "1" || r.Header.Get("HTTP-Referer") == "" {
			t.Errorf("request %s %v", r.URL.Path, r.Header)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
			"content": "here",
			"images":  []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}},
		}}}})
	}))
	defer srv.Close()
	p, err := New("openrouter", Config{APIKey: "k", APIBase: srv.URL + "/api/v1/", ExtraHeaders: map[string]string{"X-Extra": "1"},
		ExtraBody: map[string]any{"provider": map[string]any{"sort": "price"}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(t.TempDir(), "ref.png")
	os.WriteFile(ref, tinyPNG(t), 0o600)
	resp, err := p.Generate(context.Background(), Request{Prompt: "a cat", Model: "openai/gpt-5.4-image-2",
		ReferenceImages: []string{ref}, AspectRatio: "16:9", ImageSize: "1K"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 1 || resp.Images[0] != dataURL || resp.Content != "here" {
		t.Errorf("resp = %+v", resp)
	}
	if got["model"] != "openai/gpt-5.4-image-2" || got["stream"] != false || got["provider"] == nil {
		t.Errorf("body = %v", got)
	}
	ic, _ := got["image_config"].(map[string]any)
	if ic["aspect_ratio"] != "16:9" || ic["image_size"] != "1K" {
		t.Errorf("image_config = %v", ic)
	}
	content := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 || !strings.HasPrefix(content[1].(map[string]any)["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,") {
		t.Errorf("content = %v", content)
	}
}

func TestOpenRouterErrors(t *testing.T) {
	p, _ := New("openrouter", Config{})
	if _, err := p.Generate(context.Background(), Request{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "providers.openrouter.apiKey") {
		t.Errorf("missing key: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "bad") {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"no can do"}}]}`)
	}))
	defer srv.Close()
	p, _ = New("openrouter", Config{APIKey: "bad", APIBase: srv.URL})
	if _, err := p.Generate(context.Background(), Request{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "invalid key") {
		t.Errorf("401: %v", err)
	}
	p, _ = New("openrouter", Config{APIKey: "k", APIBase: srv.URL})
	if _, err := p.Generate(context.Background(), Request{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "no images") {
		t.Errorf("no images: %v", err)
	}
}

func TestOpenAIGenerate(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(tinyPNG(t))
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s %v", r.URL.Path, r.Header)
		}
		got = nil
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": b64}}})
	}))
	defer srv.Close()
	p, _ := New("openai", Config{APIKey: "k", APIBase: srv.URL + "/v1"})
	resp, err := p.Generate(context.Background(), Request{Prompt: "a cat", Model: "openai/dall-e-3", AspectRatio: "16:9", ImageSize: "1K"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 1 || resp.Images[0] != "data:image/png;base64,"+b64 {
		t.Errorf("images = %v", resp.Images)
	}
	if got["model"] != "dall-e-3" || got["response_format"] != "b64_json" || got["n"] != float64(1) || got["size"] != "1792x1024" {
		t.Errorf("body = %v", got)
	}

	// GPT Image models get no response_format; extraBody null drops a key.
	p, _ = New("openai", Config{APIKey: "k", APIBase: srv.URL + "/v1", ExtraBody: map[string]any{"size": nil, "quality": "low"}})
	if _, err := p.Generate(context.Background(), Request{Prompt: "a cat", Model: "gpt-image-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["response_format"]; ok || got["quality"] != "low" {
		t.Errorf("gpt-image body = %v", got)
	}
	if _, ok := got["size"]; ok {
		t.Errorf("null extraBody key kept: %v", got)
	}
}

func TestOpenAIEditsWithReferences(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(tinyPNG(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		fields, files := map[string]string{}, 0
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "image[]" {
				files++
				if part.Header.Get("Content-Type") != "image/png" {
					t.Errorf("file part type = %s", part.Header.Get("Content-Type"))
				}
			} else {
				fields[part.FormName()] = string(data)
			}
		}
		if files != 1 || fields["model"] != "gpt-image-1" || fields["prompt"] != "edit" || fields["size"] != "1536x1024" {
			t.Errorf("fields = %v files = %d", fields, files)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": b64}}})
	}))
	defer srv.Close()
	ref := filepath.Join(t.TempDir(), "ref.png")
	os.WriteFile(ref, tinyPNG(t), 0o600)
	p, _ := New("openai", Config{APIKey: "k", APIBase: srv.URL})
	if _, err := p.Generate(context.Background(), Request{Prompt: "edit", Model: "gpt-image-1", ReferenceImages: []string{ref}, AspectRatio: "16:9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Generate(context.Background(), Request{Prompt: "edit", Model: "dall-e-3", ReferenceImages: []string{ref}}); err == nil {
		t.Error("dall-e-3 accepted reference images")
	}
}

func TestOpenAIURLResponseUsesSSRFPolicy(t *testing.T) {
	// The default policy refuses loopback, so a URL pointing back at the
	// test server is not fetched.
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"url": srvURL + "/img.png"}}})
	}))
	defer srv.Close()
	srvURL = srv.URL
	p, _ := New("openai", Config{APIKey: "k", APIBase: srv.URL})
	if _, err := p.Generate(context.Background(), Request{Prompt: "x", Model: "dall-e-2"}); err == nil || !strings.Contains(err.Error(), "download generated image") {
		t.Errorf("err = %v", err)
	}
}

func TestOpenAISize(t *testing.T) {
	for _, c := range []struct{ model, ratio, size, want string }{
		{"dall-e-2", "16:9", "", "1024x1024"},
		{"dall-e-2", "", "512x512", "512x512"},
		{"dall-e-3", "9:16", "1K", "1024x1792"},
		{"gpt-image-1", "4:3", "", "1536x1024"},
		{"gpt-image-1", "", "2048x2048", "1024x1024"},
		{"gpt-image-2", "", "2048x1152", "2048x1152"},
		{"gpt-image-1", "", "auto", "auto"},
	} {
		if got := openAISize(c.model, c.ratio, c.size); got != c.want {
			t.Errorf("openAISize(%q, %q, %q) = %q, want %q", c.model, c.ratio, c.size, got, c.want)
		}
	}
}

func TestNewUnknownProvider(t *testing.T) {
	if _, err := New("gemini", Config{}); err == nil {
		t.Error("gemini accepted")
	}
}
