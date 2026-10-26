package generateimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/imagegen"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
	"github.com/yedamao/nanobot-go/internal/tools/message"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeProvider returns perCall images per call.
type fakeProvider struct {
	t       *testing.T
	perCall int
	err     error
	reqs    []imagegen.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Generate(_ context.Context, req imagegen.Request) (imagegen.Response, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return imagegen.Response{}, f.err
	}
	var r imagegen.Response
	for range f.perCall {
		r.Images = append(r.Images, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(tinyPNG(f.t)))
	}
	return r, nil
}

func harness(t *testing.T, p imagegen.Provider, restrict bool) (*tools.Registry, *security.Workspace) {
	t.Helper()
	ws, err := security.NewWorkspace(t.TempDir(), restrict)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Tools.ImageGeneration
	cfg.Enabled = true
	reg := tools.NewRegistry()
	if err := reg.Register(New(p, ws, cfg)); err != nil {
		t.Fatal(err)
	}
	return reg, ws
}

type result struct {
	Artifacts []imagegen.Artifact `json:"artifacts"`
	NextStep  string              `json:"next_step"`
}

func run(t *testing.T, reg *tools.Registry, args string) (tools.Result, result) {
	t.Helper()
	res := reg.Execute(context.Background(), "generate_image", json.RawMessage(args))
	var r result
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Content), &r); err != nil {
			t.Fatalf("result %q: %v", res.Content, err)
		}
	}
	return res, r
}

func TestGenerateStoresArtifactsDeliverableByMessageTool(t *testing.T) {
	fp := &fakeProvider{t: t, perCall: 1}
	reg, ws := harness(t, fp, true)
	res, r := run(t, reg, `{"prompt":"a cat","count":2}`)
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if len(r.Artifacts) != 2 || len(fp.reqs) != 2 || !strings.Contains(r.NextStep, "message tool") {
		t.Fatalf("artifacts = %+v, calls = %d", r.Artifacts, len(fp.reqs))
	}
	if fp.reqs[0].AspectRatio != "1:1" || fp.reqs[0].ImageSize != "1K" || fp.reqs[0].Model != config.DefaultImageModel {
		t.Errorf("request defaults = %+v", fp.reqs[0])
	}
	day := time.Now().Format("2006-01-02")
	for _, a := range r.Artifacts {
		if filepath.Dir(a.Path) != filepath.Join(ws.Root, "media", "generated", day) {
			t.Errorf("path = %s", a.Path)
		}
		if _, err := os.Stat(a.Path); err != nil {
			t.Error(err)
		}
	}

	// The paths go straight into the message tool's media.
	b := bus.NewMessageBus(1)
	mreg := tools.NewRegistry()
	mreg.Register(message.NewTool(b, ws))
	ctx := tools.WithTurn(context.Background(), tools.Turn{Channel: "telegram", ChatID: "1"})
	media, _ := json.Marshal([]string{r.Artifacts[0].Path, r.Artifacts[1].Path})
	if res := mreg.Execute(ctx, "message", json.RawMessage(`{"content":"here","media":`+string(media)+`}`)); res.IsError {
		t.Fatalf("message = %+v", res)
	}
	out, err := b.ConsumeOutbound(ctx)
	if err != nil || len(out.Media) != 2 || out.Media[0] != r.Artifacts[0].Path {
		t.Errorf("outbound = %+v, %v", out, err)
	}
}

func TestGenerateStopsAtCountWhenProviderReturnsMore(t *testing.T) {
	fp := &fakeProvider{t: t, perCall: 3}
	reg, _ := harness(t, fp, true)
	_, r := run(t, reg, `{"prompt":"x","count":2,"aspect_ratio":"16:9"}`)
	if len(r.Artifacts) != 2 || len(fp.reqs) != 1 || fp.reqs[0].AspectRatio != "16:9" {
		t.Errorf("artifacts = %d, calls = %d", len(r.Artifacts), len(fp.reqs))
	}
}

func TestGenerateCountCapAndErrors(t *testing.T) {
	fp := &fakeProvider{t: t, perCall: 1}
	reg, _ := harness(t, fp, true)
	if res, _ := run(t, reg, `{"prompt":"x","count":5}`); !res.IsError || !strings.Contains(res.Content, "maxImagesPerTurn (4)") {
		t.Errorf("count 5 = %+v", res)
	}
	if res, _ := run(t, reg, `{"prompt":"x","count":0}`); !res.IsError {
		t.Errorf("count 0 = %+v", res)
	}
	if len(fp.reqs) != 0 {
		t.Errorf("provider called %d times", len(fp.reqs))
	}
	fp.err = errors.New("provider down")
	if res, _ := run(t, reg, `{"prompt":"x"}`); !res.IsError || !strings.Contains(res.Content, "provider down") {
		t.Errorf("provider error = %+v", res)
	}
}

func TestGenerateMissingAPIKey(t *testing.T) {
	p, err := imagegen.New("openrouter", imagegen.Config{})
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := harness(t, p, true)
	if res, _ := run(t, reg, `{"prompt":"x"}`); !res.IsError || !strings.Contains(res.Content, "providers.openrouter.apiKey") {
		t.Errorf("result = %+v", res)
	}
}

func TestReferenceImages(t *testing.T) {
	fp := &fakeProvider{t: t, perCall: 1}
	// Even an unrestricted workspace keeps references inside it.
	reg, ws := harness(t, fp, false)
	os.WriteFile(filepath.Join(ws.Root, "ref.png"), tinyPNG(t), 0o600)
	os.WriteFile(filepath.Join(ws.Root, "notes.txt"), []byte("hello"), 0o600)
	outside := filepath.Join(t.TempDir(), "out.png")
	os.WriteFile(outside, tinyPNG(t), 0o600)

	res, r := run(t, reg, `{"prompt":"edit","reference_images":["ref.png"]}`)
	if res.IsError || len(fp.reqs) != 1 || fp.reqs[0].ReferenceImages[0] != filepath.Join(ws.Root, "ref.png") ||
		r.Artifacts[0].SourceImages[0] != filepath.Join(ws.Root, "ref.png") {
		t.Fatalf("result = %+v, reqs = %+v", res, fp.reqs)
	}
	for _, ref := range []string{outside, "../x.png", "notes.txt", "missing.png"} {
		args, _ := json.Marshal(map[string]any{"prompt": "x", "reference_images": []string{ref}})
		if res, _ := run(t, reg, string(args)); !res.IsError {
			t.Errorf("reference %q accepted", ref)
		}
	}
	if len(fp.reqs) != 1 {
		t.Errorf("provider called for refused references")
	}
}

func TestEndToEndOpenAI(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(tinyPNG(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": b64}}})
	}))
	defer srv.Close()
	p, _ := imagegen.New("openai", imagegen.Config{APIKey: "k", APIBase: srv.URL})
	reg, ws := harness(t, p, true)
	res, r := run(t, reg, `{"prompt":"x"}`)
	if res.IsError || len(r.Artifacts) != 1 || !ws.Contains(r.Artifacts[0].Path) {
		t.Fatalf("result = %+v", res)
	}
}
