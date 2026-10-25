package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/session"
)

func TestTurnSendsInboundImagesAndPersistsPaths(t *testing.T) {
	workspace := t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	if err := os.WriteFile(filepath.Join(workspace, "cat.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := &scripted{steps: []step{reply("a cat"), reply("still a cat")}}
	l, err := NewLoop(LoopConfig{
		Provider: p,
		Sessions: session.NewManager(session.NewJSONLStore(dir)),
		Context:  NewContextBuilder(workspace),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cat, notes := filepath.Join(workspace, "cat.png"), filepath.Join(workspace, "notes.pdf")
	in := bus.InboundMessage{Channel: "websocket", ChatID: "c1", Content: "what is this?", Media: []string{cat, notes}}
	if _, err := l.Process(ctx, in); err != nil {
		t.Fatal(err)
	}
	user := p.reqs[0].Messages[1]
	wantText := "what is this?\n\n[Attachment: " + notes + "]"
	if user.Content != wantText || len(user.Parts) != 3 || user.Parts[1].Image == nil || user.Parts[1].Image.MediaType != "image/png" {
		t.Fatalf("user message = %+v", user)
	}

	// The session stores the path, not the image bytes.
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("session files = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"media":["`+cat+`"]`) || strings.Contains(string(raw), "iVBOR") {
		t.Errorf("session file = %s", raw)
	}

	// A later turn replays the image from its path.
	if _, err := l.Process(ctx, bus.InboundMessage{Channel: "websocket", ChatID: "c1", Content: "sure?"}); err != nil {
		t.Fatal(err)
	}
	if hist := p.reqs[1].Messages[1]; len(hist.Parts) != 3 || hist.Parts[1].Image == nil {
		t.Errorf("replayed message = %+v", hist)
	}
}
