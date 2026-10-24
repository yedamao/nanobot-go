package websocket

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
)

// harness is a started channel mounted on an httptest server (which
// listens on 127.0.0.1).
type harness struct {
	ch  *Channel
	bus *bus.MessageBus
	srv *httptest.Server
}

func newHarness(t *testing.T, start bool) *harness {
	t.Helper()
	b := bus.NewMessageBus(8)
	ch, err := New(Config{Enabled: true}, channels.Deps{Bus: b}, Options{Host: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ch.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if start {
		if err := ch.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ch.Stop(context.Background()) })
	}
	return &harness{ch: ch, bus: b, srv: srv}
}

func (h *harness) wsURL(query string) string {
	return "ws" + strings.TrimPrefix(h.srv.URL, "http") + Path + query
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dial connects and returns the connection and the chat id from the
// hello frame.
func (h *harness) dial(t *testing.T, query string) (*websocket.Conn, string) {
	t.Helper()
	ctx := testCtx(t)
	conn, _, err := websocket.Dial(ctx, h.wsURL(query), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	hello := readFrame(t, conn)
	if hello.Type != TypeHello || hello.ChatID == "" {
		t.Fatalf("first frame = %+v, want hello with a chat id", hello)
	}
	return conn, hello.ChatID
}

func readFrame(t *testing.T, conn *websocket.Conn) Frame {
	t.Helper()
	_, data, err := conn.Read(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("frame %q: %v", data, err)
	}
	return f
}

func write(t *testing.T, conn *websocket.Conn, s string) {
	t.Helper()
	if err := conn.Write(testCtx(t), websocket.MessageText, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) inbound(t *testing.T) bus.InboundMessage {
	t.Helper()
	msg, err := h.bus.ConsumeInbound(testCtx(t))
	if err != nil {
		t.Fatal("no inbound message:", err)
	}
	return msg
}

func TestRoundTrip(t *testing.T) {
	h := newHarness(t, true)
	conn, chatID := h.dial(t, "")

	write(t, conn, `{"type":"chat","content":"hello"}`)
	in := h.inbound(t)
	if in.Channel != Name || in.ChatID != chatID || in.Content != "hello" || in.SenderID != SenderID {
		t.Errorf("inbound = %+v", in)
	}
	if got, want := in.SessionKey(), "websocket:"+chatID; got != want {
		t.Errorf("session key = %q, want %q", got, want)
	}

	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: chatID, Content: "hi there"}); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, conn); f != (Frame{Type: TypeMessage, Role: "assistant", Content: "hi there"}) {
		t.Errorf("reply frame = %+v", f)
	}
}

func TestClientFrames(t *testing.T) {
	h := newHarness(t, true)
	conn, _ := h.dial(t, "")
	tests := []struct {
		name, frame string
		content     string // published content, or "" when rejected
		problem     string // part of the error frame
	}{
		{"json chat", `{"type":"chat","content":"a"}`, "a", ""},
		{"plain text", "  just text \n", "just text", ""},
		{"unknown type", `{"type":"ping"}`, "", "unsupported frame type"},
		{"malformed json", `{"type":`, "", "malformed JSON"},
		{"empty content", `{"type":"chat","content":"  "}`, "", "empty message"},
		{"blank text", "   ", "", "empty message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			write(t, conn, tt.frame)
			if tt.content != "" {
				if in := h.inbound(t); in.Content != tt.content {
					t.Errorf("content = %q, want %q", in.Content, tt.content)
				}
				return
			}
			f := readFrame(t, conn)
			if f.Type != TypeError || !strings.Contains(f.Content, tt.problem) {
				t.Errorf("frame = %+v, want an error about %q", f, tt.problem)
			}
			if n := h.bus.InboundLen(); n != 0 {
				t.Errorf("%d inbound messages published for a rejected frame", n)
			}
		})
	}
	if err := conn.Write(testCtx(t), websocket.MessageBinary, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, conn); f.Type != TypeError || !strings.Contains(f.Content, "binary") {
		t.Errorf("binary frame answer = %+v", f)
	}
}

func TestChatIDResumeAndFanOut(t *testing.T) {
	h := newHarness(t, true)
	a, id := h.dial(t, "?chat_id=my-chat_1")
	if id != "my-chat_1" {
		t.Fatalf("chat id = %q", id)
	}
	b, _ := h.dial(t, "?chat_id=my-chat_1")
	other, otherID := h.dial(t, "")
	if otherID == id {
		t.Fatal("a new connection reused the chat id")
	}

	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{ChatID: id, Content: "both"}); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []*websocket.Conn{a, b} {
		if f := readFrame(t, conn); f.Content != "both" {
			t.Errorf("frame = %+v", f)
		}
	}
	// The other chat got nothing: its next frame is the reply to it.
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{ChatID: otherID, Content: "mine"}); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, other); f.Content != "mine" {
		t.Errorf("other chat frame = %+v", f)
	}
}

func TestBadChatID(t *testing.T) {
	h := newHarness(t, true)
	for _, q := range []string{"?chat_id=a%20b", "?chat_id=" + strings.Repeat("x", 65), "?chat_id=..%2Fx"} {
		_, resp, err := websocket.Dial(testCtx(t), h.wsURL(q), nil)
		if err == nil {
			t.Errorf("%s: connected", q)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: response %v, want 400", q, resp)
		}
	}
}

func TestSendWithoutConnectionIsPermanent(t *testing.T) {
	h := newHarness(t, true)
	conn, id := h.dial(t, "")
	err := h.ch.Send(testCtx(t), bus.OutboundMessage{ChatID: "nobody", Content: "x"})
	if err == nil || !channels.IsPermanent(err) {
		t.Errorf("Send to unknown chat = %v, want a permanent error", err)
	}

	conn.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for len(h.ch.connsFor(id)) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{ChatID: id, Content: "x"}); !channels.IsPermanent(err) {
		t.Errorf("Send after the tab closed = %v, want a permanent error", err)
	}
}

func TestNotRunning(t *testing.T) {
	h := newHarness(t, false)
	_, resp, err := websocket.Dial(testCtx(t), h.wsURL(""), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("dial before Start: resp %v, err %v; want 503", resp, err)
	}
}

func TestStopClosesConnections(t *testing.T) {
	h := newHarness(t, true)
	conn, _ := h.dial(t, "")
	// The client reads, as a browser does, so it answers the close.
	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(context.Background())
		readErr <- err
	}()
	if err := h.ch.Stop(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	err := <-readErr
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Errorf("read after Stop: %v (status %d), want going away", err, got)
	}
	_, resp, err := websocket.Dial(testCtx(t), h.wsURL(""), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("dial after Stop: resp %v, err %v; want 503", resp, err)
	}
	if err := h.ch.Stop(testCtx(t)); err != nil {
		t.Errorf("second Stop = %v", err)
	}
	if err := h.ch.Start(context.Background()); err == nil {
		t.Error("Start after Stop succeeded")
	}
}

func TestRefusesForeignHostAndOrigin(t *testing.T) {
	h := newHarness(t, true)
	tests := []struct {
		name, path, host, origin string
		want                     int
	}{
		{"rebound host on ws", Path, "evil.example:80", "", http.StatusForbidden},
		{"rebound host on ui", "/ui/", "evil.example", "", http.StatusForbidden},
		{"cross-site origin", Path, "", "http://evil.example", http.StatusForbidden},
		{"ui on loopback", "/ui/", "", "", http.StatusOK},
		{"localhost name", "/ui/", "localhost:1234", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.host != "" {
				req.Host = tt.host
			}
			if tt.path == Path {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Sec-WebSocket-Version", "13")
				req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestServesUI(t *testing.T) {
	h := newHarness(t, false)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Errorf("GET / = %d to %q, want a redirect to /ui/", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = http.Get(h.srv.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `<script src="app.js">`) {
		t.Errorf("GET /ui/ = %d %q", resp.StatusCode, body)
	}
}

func TestNewRefusesNonLoopback(t *testing.T) {
	b := bus.NewMessageBus(1)
	for _, host := range []string{"0.0.0.0", "192.168.1.5", "::", "example.com"} {
		if _, err := New(Config{}, channels.Deps{Bus: b}, Options{Host: host}); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("New(host %q) = %v, want a loopback refusal", host, err)
		}
	}
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		if _, err := New(Config{}, channels.Deps{Bus: b}, Options{Host: host}); err != nil {
			t.Errorf("New(host %q) = %v", host, err)
		}
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{`{"enabled": true}`, false},
		{`{}`, false},
		{`{"enabled": true, "host": "0.0.0.0"}`, true},
		{`{"enabled": "yes"}`, true},
	}
	for _, tt := range tests {
		if _, err := ParseConfig(json.RawMessage(tt.raw)); (err != nil) != tt.wantErr {
			t.Errorf("ParseConfig(%s) = %v, wantErr %v", tt.raw, err, tt.wantErr)
		}
	}
}
