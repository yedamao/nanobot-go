package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	wschan "github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

// enableWebUI replaces the fake channel with the websocket channel.
func enableWebUI(c *config.Config) {
	delete(c.Channels, "fake")
	c.Channels[wschan.Name] = json.RawMessage(`{"enabled": true}`)
}

func readWSFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) wschan.Frame {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var f wschan.Frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("frame %q: %v", data, err)
	}
	return f
}

// TestGatewayWebUIRoundTrip runs one browser chat turn through the real
// agent loop (with the echoing fake provider) on the gateway's address.
func TestGatewayWebUIRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, enableWebUI)
	g := startGateway(t, e, nil)

	if !strings.Contains(e.stdout.String(), "WebUI: http://"+g.addr+"/") ||
		!strings.Contains(e.stdout.String(), "Channels: websocket") {
		t.Errorf("banner = %q", e.stdout)
	}
	// Health and the page share the address.
	if code, body := get(t, "http://"+g.addr+"/healthz"); code != http.StatusOK || body != healthBody {
		t.Errorf("GET /healthz = %d %q", code, body)
	}
	if code, body := get(t, "http://"+g.addr+"/"); code != http.StatusOK || !strings.Contains(body, "app.js") {
		t.Errorf("GET / = %d %q", code, body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+g.addr+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello := readWSFrame(t, ctx, conn)
	if hello.Type != wschan.TypeHello || hello.ChatID == "" {
		t.Fatalf("hello = %+v", hello)
	}
	if h := readWSFrame(t, ctx, conn); h.Type != wschan.TypeHistory || len(h.Messages) != 0 {
		t.Fatalf("history of a new chat = %+v", h)
	}
	// The socket outlives the health endpoint's 2-second timeouts.
	time.Sleep(healthReadTimeout + 500*time.Millisecond)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"chat","content":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	reply := readWSFrame(t, ctx, conn)
	if reply.Type != wschan.TypeMessage || reply.Role != "assistant" || reply.Content != "echo: hello" {
		t.Errorf("reply = %+v", reply)
	}

	// A reload resumes the chat and gets the turn back from the store.
	again, _, err := websocket.Dial(ctx, "ws://"+g.addr+"/ws?chat_id="+hello.ChatID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.CloseNow()
	if h := readWSFrame(t, ctx, again); h.Type != wschan.TypeHello || h.ChatID != hello.ChatID {
		t.Fatalf("resumed hello = %+v", h)
	}
	want := []wschan.HistoryMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "echo: hello"}}
	if h := readWSFrame(t, ctx, again); h.Type != wschan.TypeHistory || !slices.Equal(h.Messages, want) {
		t.Errorf("resumed history = %+v, want %+v", h, want)
	}
	again.Close(websocket.StatusNormalClosure, "")

	// Shutdown closes the socket with "going away".
	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(context.Background())
		readErr <- err
	}()
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	if err := <-readErr; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Errorf("socket after shutdown: %v", err)
	}
	if s, err := e.loadSession(t, "websocket:"+hello.ChatID); err != nil || len(s.Messages) != 2 {
		t.Errorf("session websocket:%s = %+v, %v", hello.ChatID, s, err)
	}
}

func TestGatewayWebUIDisabled(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		c.Channels[wschan.Name] = json.RawMessage(`{"enabled": false}`)
	})
	g := startGateway(t, e, newFakeChat(&eventLog{}))
	for _, path := range []string{"/", "/ui/", "/ws"} {
		if code, _ := get(t, "http://"+g.addr+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	if strings.Contains(e.stdout.String(), "WebUI:") {
		t.Errorf("banner mentions the WebUI: %q", e.stdout)
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
}
