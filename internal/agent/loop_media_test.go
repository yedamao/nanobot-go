package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools/message"
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

func TestMessageToolSupersedesTheReply(t *testing.T) {
	ws, err := security.NewWorkspace(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, "cat.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := bus.NewMessageBus(16)
	p := &scripted{steps: []step{
		callTools(call("c1", "message", `{"content":"here","media":["cat.png"]}`)), reply("Sent it."),
		callTools(call("c2", "message", `{"content":"fyi","chat_id":"other"}`)), reply("Told them."),
	}}
	l, _, _ := newTestLoop(t, p, registry(t, message.NewTool(b, ws)), b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	// next returns the next outbound message that is not a progress
	// update or delta.
	next := func() bus.OutboundMessage {
		t.Helper()
		for {
			out, err := consumeWithTimeout(b)
			if err != nil {
				t.Fatal(err)
			}
			if !bus.IsProgress(out) && !bus.IsDelta(out) && !bus.IsThinking(out) && !bus.IsThinkingEnd(out) {
				return out
			}
		}
	}
	in := bus.InboundMessage{Channel: "telegram", ChatID: "42", Content: "send the cat", Metadata: map[string]any{"message_id": 5}}
	if err := b.PublishInbound(ctx, in); err != nil {
		t.Fatal(err)
	}
	sent := next()
	if sent.Content != "here" || len(sent.Media) != 1 || sent.Media[0] != filepath.Join(ws.Root, "cat.png") ||
		sent.Metadata["message_id"] != 5 || bus.IsStreamed(sent) {
		t.Errorf("tool message = %+v", sent)
	}
	// The reply to the chat the tool answered is marked superseded, so
	// chat apps drop it (bus.IsStreamed) while the WebUI settles its
	// streamed text.
	final := next()
	if final.Content != "Sent it." || !bus.IsSuperseded(final) || !bus.IsStreamed(final) || final.Metadata["message_id"] != 5 {
		t.Errorf("final = %+v", final)
	}
	if in.Metadata[bus.MetaSuperseded] != nil {
		t.Error("inbound metadata modified")
	}

	// A send to another chat leaves this chat's reply alone.
	in.Content = "tell the others"
	if err := b.PublishInbound(ctx, in); err != nil {
		t.Fatal(err)
	}
	if sent := next(); sent.ChatID != "other" || sent.Content != "fyi" {
		t.Errorf("tool message = %+v", sent)
	}
	if final := next(); final.Content != "Told them." || bus.IsSuperseded(final) || bus.IsStreamed(final) {
		t.Errorf("final = %+v", final)
	}
}
