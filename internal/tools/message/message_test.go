package message

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/heartbeat"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

var chatTurn = tools.Turn{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42",
	Metadata: map[string]any{"message_id": 7, "message_thread_id": 3}}

func newHarness(t *testing.T, restrict bool) (*tools.Registry, *bus.MessageBus, *security.Workspace) {
	t.Helper()
	ws, err := security.NewWorkspace(t.TempDir(), restrict)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws.Root, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, "out", "cat.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := bus.NewMessageBus(4)
	reg := tools.NewRegistry()
	if err := reg.Register(NewTool(b, ws)); err != nil {
		t.Fatal(err)
	}
	return reg, b, ws
}

func run(reg *tools.Registry, ctx context.Context, args string) tools.Result {
	return reg.Execute(ctx, "message", json.RawMessage(args))
}

func consume(t *testing.T, b *bus.MessageBus) bus.OutboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	msg, err := b.ConsumeOutbound(ctx)
	if err != nil {
		t.Fatalf("no outbound message: %v", err)
	}
	return msg
}

func TestSendToTheTurnsChatWithMedia(t *testing.T) {
	reg, b, ws := newHarness(t, false)
	ctx, sent := tools.WithDeliveries(tools.WithTurn(context.Background(), chatTurn))

	res := run(reg, ctx, `{"content":"here it is","media":["out/cat.png"]}`)
	if res.IsError || res.Content != "Message sent to telegram:42 with 1 attachments" {
		t.Fatalf("result = %+v", res)
	}
	msg := consume(t, b)
	want := filepath.Join(ws.Root, "out", "cat.png")
	if msg.Channel != "telegram" || msg.ChatID != "42" || msg.Content != "here it is" ||
		len(msg.Media) != 1 || msg.Media[0] != want {
		t.Errorf("outbound = %+v", msg)
	}
	// The same chat keeps the turn's routing (thread, replied message).
	if msg.Metadata["message_id"] != 7 || msg.Metadata["message_thread_id"] != 3 || len(msg.Metadata) != 2 {
		t.Errorf("metadata = %v", msg.Metadata)
	}
	msg.Metadata["message_id"] = 8
	if turn, _ := tools.TurnFrom(ctx); turn.Metadata["message_id"] != 7 {
		t.Error("outbound metadata aliases the turn's")
	}
	if !sent.Delivered("telegram", "42") || sent.Delivered("telegram", "43") {
		t.Error("delivery not recorded for the turn's chat only")
	}
}

func TestSendWithoutMediaAndAbsolutePath(t *testing.T) {
	reg, b, ws := newHarness(t, true)
	ctx := tools.WithTurn(context.Background(), chatTurn)

	if res := run(reg, ctx, `{"content":"hi"}`); res.IsError || res.Content != "Message sent to telegram:42" {
		t.Fatalf("result = %+v", res)
	}
	if msg := consume(t, b); msg.Content != "hi" || msg.Media != nil {
		t.Errorf("outbound = %+v", msg)
	}
	abs := filepath.Join(ws.Root, "out", "cat.png")
	if res := run(reg, ctx, `{"content":"x","media":["`+abs+`"]}`); res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if msg := consume(t, b); len(msg.Media) != 1 || msg.Media[0] != abs {
		t.Errorf("outbound = %+v", msg)
	}
}

func TestSendToAnotherChatDropsRouting(t *testing.T) {
	reg, b, _ := newHarness(t, false)
	ctx, sent := tools.WithDeliveries(tools.WithTurn(context.Background(), chatTurn))

	res := run(reg, ctx, `{"content":"fyi","chat_id":"99"}`)
	if res.IsError || res.Content != "Message sent to telegram:99" {
		t.Fatalf("result = %+v", res)
	}
	if msg := consume(t, b); msg.Channel != "telegram" || msg.ChatID != "99" || len(msg.Metadata) != 0 {
		t.Errorf("outbound = %+v", msg)
	}
	if res := run(reg, ctx, `{"content":"fyi","channel":"websocket"}`); res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if msg := consume(t, b); msg.Channel != "websocket" || msg.ChatID != "42" || len(msg.Metadata) != 0 {
		t.Errorf("outbound = %+v", msg)
	}
	if sent.Delivered("telegram", "42") || !sent.Delivered("telegram", "99") {
		t.Error("deliveries recorded wrongly")
	}
}

func TestMediaErrors(t *testing.T) {
	reg, b, _ := newHarness(t, false)
	ctx := tools.WithTurn(context.Background(), chatTurn)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	many := strings.Repeat(`"out/cat.png",`, MaxMedia) + `"out/cat.png"`
	tests := []struct{ media, want string }{
		{`["out/missing.png"]`, "media file not found: out/missing.png"},
		{`["` + outside + `"]`, "media path is not allowed"},
		{`["../escape.png"]`, "media path is not allowed"},
		{`["out"]`, "media path is not a file"},
		{`["out/cat.png", " "]`, "media[1] is empty"},
		{`["https://example.com/a.png"]`, "remote URLs are not supported"},
		{`[` + many + `]`, "too many attachments"},
	}
	for _, tt := range tests {
		res := run(reg, ctx, `{"content":"x","media":`+tt.media+`}`)
		if !res.IsError || !strings.Contains(res.Content, tt.want) {
			t.Errorf("media %s: result = %+v, want %q", tt.media, res, tt.want)
		}
	}
	if b.OutboundLen() != 0 {
		t.Errorf("%d messages published", b.OutboundLen())
	}
}

func TestTargetAndBusErrors(t *testing.T) {
	reg, b, ws := newHarness(t, false)
	// No turn (Dream): the target must be explicit.
	res := run(reg, context.Background(), `{"content":"x"}`)
	if !res.IsError || !strings.Contains(res.Content, "No target channel/chat") {
		t.Errorf("no turn: result = %+v", res)
	}
	if res := run(reg, context.Background(), `{"content":"x","channel":"telegram","chat_id":"5"}`); res.IsError {
		t.Errorf("explicit target: result = %+v", res)
	} else if msg := consume(t, b); msg.ChatID != "5" || msg.Metadata != nil {
		t.Errorf("outbound = %+v", msg)
	}

	noBus := tools.NewRegistry()
	if err := noBus.Register(NewTool(nil, ws)); err != nil {
		t.Fatal(err)
	}
	res = run(noBus, tools.WithTurn(context.Background(), chatTurn), `{"content":"x"}`)
	if !res.IsError || !strings.Contains(res.Content, "Message sending not configured") {
		t.Errorf("nil bus: result = %+v", res)
	}
}

func TestHeartbeatSendsAreNotDelivered(t *testing.T) {
	reg, b, _ := newHarness(t, false)
	turn := tools.Turn{Channel: "telegram", ChatID: "42", SessionKey: heartbeat.SessionKey}
	ctx, sent := tools.WithDeliveries(tools.WithTurn(context.Background(), turn))

	res := run(reg, ctx, `{"content":"all good","media":["out/cat.png"]}`)
	if res.IsError || res.Content != "Message acknowledged for telegram:42 (not delivered)" {
		t.Fatalf("result = %+v", res)
	}
	if b.OutboundLen() != 0 || sent.Delivered("telegram", "42") {
		t.Error("heartbeat send was delivered")
	}
}
