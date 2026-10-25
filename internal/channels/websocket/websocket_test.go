package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
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
	return newHarnessWith(t, start, Options{Host: "127.0.0.1"})
}

func newHarnessWith(t *testing.T, start bool, opts Options) *harness {
	t.Helper()
	b := bus.NewMessageBus(8)
	ch, err := New(Config{Enabled: true}, channels.Deps{Bus: b}, opts)
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
// hello frame, consuming the history frame that follows it.
func (h *harness) dial(t *testing.T, query string) (*websocket.Conn, string) {
	t.Helper()
	conn, chatID, _ := h.dialHistory(t, query)
	return conn, chatID
}

// dialHistory connects and returns the connection, the chat id and the
// raw history frame.
func (h *harness) dialHistory(t *testing.T, query string) (*websocket.Conn, string, []byte) {
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
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil || f.Type != TypeHistory {
		t.Fatalf("second frame = %s (%v), want history", data, err)
	}
	return conn, hello.ChatID, data
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
	if f := readFrame(t, conn); f.Type != TypeMessage || f.Role != "assistant" || f.Content != "hi there" || f.Messages != nil {
		t.Errorf("reply frame = %+v", f)
	}
}

func TestProgressFrames(t *testing.T) {
	h := newHarness(t, true)
	conn, chatID := h.dial(t, "")
	progress := map[string]any{bus.MetaProgress: true}
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: chatID, Content: "read notes.md", Metadata: progress}); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, conn); f.Type != TypeProgress || f.Content != "read notes.md" || f.Role != "" {
		t.Errorf("progress frame = %+v", f)
	}
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: chatID, Content: "done"}); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, conn); f.Type != TypeMessage || f.Content != "done" {
		t.Errorf("reply frame = %+v", f)
	}
	// Progress for a closed tab is not worth an error.
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: "nobody", Content: "x", Metadata: progress}); err != nil {
		t.Errorf("Send(progress) to unknown chat = %v", err)
	}
}

func TestDeltaFrames(t *testing.T) {
	h := newHarness(t, true)
	conn, chatID := h.dial(t, "")
	delta := map[string]any{bus.MetaDelta: true}
	for _, d := range []string{"Hel", "lo"} {
		if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: chatID, Content: d, Metadata: delta}); err != nil {
			t.Fatal(err)
		}
		if f := readFrame(t, conn); f.Type != TypeDelta || f.Content != d || f.Role != "" {
			t.Errorf("delta frame = %+v", f)
		}
	}
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: chatID, Content: "Hello"}); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, conn); f.Type != TypeMessage || f.Role != "assistant" || f.Content != "Hello" {
		t.Errorf("reply frame = %+v", f)
	}
	// A delta for a closed tab is not worth an error.
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: "nobody", Content: "x", Metadata: delta}); err != nil {
		t.Errorf("Send(delta) to unknown chat = %v", err)
	}
}

func TestThinkingFrames(t *testing.T) {
	h := newHarness(t, true)
	conn, chatID := h.dial(t, "")
	thinking := map[string]any{bus.MetaThinking: true}
	for _, d := range []string{"Hm", "m."} {
		if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: chatID, Content: d, Metadata: thinking}); err != nil {
			t.Fatal(err)
		}
		if f := readFrame(t, conn); f.Type != TypeThinking || f.Content != d || f.Role != "" {
			t.Errorf("thinking frame = %+v", f)
		}
	}
	end := map[string]any{bus.MetaThinkingEnd: true}
	if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: chatID, Metadata: end}); err != nil {
		t.Fatal(err)
	}
	if f := readFrame(t, conn); f.Type != TypeThinkingEnd || f.Content != "" || f.Role != "" {
		t.Errorf("thinking_end frame = %+v", f)
	}
	// Thinking for a closed tab is not worth an error.
	for _, meta := range []map[string]any{thinking, end} {
		if err := h.ch.Send(testCtx(t), bus.OutboundMessage{Channel: Name, ChatID: "nobody", Content: "x", Metadata: meta}); err != nil {
			t.Errorf("Send(%v) to unknown chat = %v", meta, err)
		}
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
		{"stop", `{"type":"stop"}`, "/stop", ""},
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
		{"rebound host on chats", ChatsPath, "evil.example", "", http.StatusForbidden},
		{"chats on loopback", ChatsPath, "", "", http.StatusOK},
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `src="/ui/assets/`) {
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

// failingLoader fails every load with an error other than not-found.
type failingLoader struct{}

func (failingLoader) Load(string) (*session.Session, error) {
	return nil, errors.New("disk on fire")
}

func (failingLoader) List() ([]session.Info, error) {
	return nil, errors.New("disk on fire")
}

func (failingLoader) Save(*session.Session) error { return errors.New("disk on fire") }

func (failingLoader) Delete(string) error { return errors.New("disk on fire") }

func TestHistoryOnConnect(t *testing.T) {
	store := session.NewJSONLStore(t.TempDir())
	noArgs := json.RawMessage(`{}`)
	sess := session.New(Name + ":old-chat")
	sess.Append(
		providers.Message{Role: providers.RoleUser, Content: "what time is it?"},
		providers.Message{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{ID: "c1", Name: "clock", Arguments: noArgs}}},
		providers.Message{Role: providers.RoleTool, ToolCallID: "c1", Name: "clock", Content: "12:00"},
		providers.Message{Role: providers.RoleAssistant, Content: "Checking again.", ToolCalls: []providers.ToolCall{{ID: "c2", Name: "clock", Arguments: noArgs}}},
		providers.Message{Role: providers.RoleTool, ToolCallID: "c2", Name: "clock", Content: "12:00"},
		providers.Message{Role: providers.RoleAssistant, Content: "It is noon."},
		providers.Message{Role: providers.RoleSystem, Content: "system note"},
		providers.Message{Role: providers.RoleUser, Content: "thanks"},
		providers.Message{Role: providers.RoleAssistant, Content: "  "},
		providers.Message{Role: providers.RoleAssistant, Content: "You're welcome."},
	)
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		loader SessionStore
		query  string
		want   []HistoryMessage
	}{
		{"stored chat", store, "?chat_id=old-chat", []HistoryMessage{
			{"user", "what time is it?"},
			{"assistant", "Checking again."},
			{"assistant", "It is noon."},
			{"user", "thanks"},
			{"assistant", "You're welcome."},
		}},
		{"new chat id", store, "", []HistoryMessage{}},
		{"unknown chat id", store, "?chat_id=never-seen", []HistoryMessage{}},
		{"no loader", nil, "?chat_id=old-chat", []HistoryMessage{}},
		{"load error", failingLoader{}, "?chat_id=old-chat", []HistoryMessage{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Sessions: tt.loader})
			conn, chatID, raw := h.dialHistory(t, tt.query)
			// An empty history is still an explicit list.
			if !strings.Contains(string(raw), `"messages":[`) {
				t.Errorf("history frame %s has no messages list", raw)
			}
			var f Frame
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			if f.Messages == nil {
				f.Messages = []HistoryMessage{}
			}
			if !slices.Equal(f.Messages, tt.want) {
				t.Errorf("history = %+v, want %+v", f.Messages, tt.want)
			}
			// The connection works as usual afterwards.
			write(t, conn, "next")
			if in := h.inbound(t); in.ChatID != chatID || in.Content != "next" {
				t.Errorf("inbound = %+v", in)
			}
		})
	}
}

func TestTranscriptCaps(t *testing.T) {
	msgs := []providers.Message{
		{Role: providers.RoleUser, Content: "aaaa"},
		{Role: providers.RoleAssistant, Content: "bbbb"},
		{Role: providers.RoleTool, Content: "ignored"},
		{Role: providers.RoleUser, Content: "cc"},
		{Role: providers.RoleAssistant, Content: "dddddddd"},
	}
	tests := []struct {
		name                  string
		maxMessages, maxBytes int
		want                  []string
	}{
		{"under the caps", 10, 100, []string{"aaaa", "bbbb", "cc", "dddddddd"}},
		{"message cap", 2, 100, []string{"cc", "dddddddd"}},
		{"byte cap", 10, 14, []string{"bbbb", "cc", "dddddddd"}},
		{"byte cap exact", 10, 18, []string{"aaaa", "bbbb", "cc", "dddddddd"}},
		{"newest alone too big", 10, 3, []string{"dddddddd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := []string{}
			for _, m := range transcript(msgs, tt.maxMessages, tt.maxBytes) {
				got = append(got, m.Content)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("transcript = %q, want %q", got, tt.want)
			}
		})
	}
	if got := transcript(nil, 10, 10); got == nil || len(got) != 0 {
		t.Errorf("transcript(nil) = %#v, want an empty list", got)
	}
}

func TestChatList(t *testing.T) {
	store := session.NewJSONLStore(t.TempDir())
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	save := func(key string, age time.Duration, msgs ...providers.Message) {
		t.Helper()
		sess := session.New(key)
		sess.Append(msgs...)
		sess.UpdatedAt = base.Add(-age)
		if err := store.Save(sess); err != nil {
			t.Fatal(err)
		}
	}
	user := func(s string) providers.Message { return providers.Message{Role: providers.RoleUser, Content: s} }
	bot := func(s string) providers.Message { return providers.Message{Role: providers.RoleAssistant, Content: s} }
	save(Name+":older", 2*time.Hour, user("first question"), bot("an answer"))
	save(Name+":newer", time.Hour, user("  plan\n\nthe   trip  "), bot(strings.Repeat("é", 100)))
	save(Name+":no-text", 0, providers.Message{Role: providers.RoleSystem, Content: "note"})
	save(Name+":bot-only", 3*time.Hour, bot("hello from cron"))
	save("telegram:42", 0, user("not a web chat"))

	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Sessions: store})
	resp, err := http.Get(h.srv.URL + ChatsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET %s = %d %q", ChatsPath, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var got ChatList
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := []ChatInfo{
		{ChatID: "newer", UpdatedAt: base.Add(-time.Hour), Title: "plan the trip", Preview: strings.Repeat("é", 79) + "…"},
		{ChatID: "older", UpdatedAt: base.Add(-2 * time.Hour), Title: "first question", Preview: "an answer"},
		{ChatID: "bot-only", UpdatedAt: base.Add(-3 * time.Hour), Preview: "hello from cron"},
	}
	if !slices.EqualFunc(got.Chats, want, func(a, b ChatInfo) bool {
		return a.ChatID == b.ChatID && a.UpdatedAt.Equal(b.UpdatedAt) && a.Title == b.Title && a.Preview == b.Preview
	}) {
		t.Errorf("chats = %+v, want %+v", got.Chats, want)
	}
}

func TestChatListCapAndFailures(t *testing.T) {
	store := session.NewJSONLStore(t.TempDir())
	for i := range MaxChats + 5 {
		sess := session.New(fmt.Sprintf("%s:chat-%02d", Name, i))
		sess.Append(providers.Message{Role: providers.RoleUser, Content: "hi"})
		if err := store.Save(sess); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		reader SessionStore
		code   int
		chats  int
	}{
		{"capped", store, http.StatusOK, MaxChats},
		{"no store", nil, http.StatusOK, 0},
		{"list error", failingLoader{}, http.StatusInternalServerError, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Sessions: tt.reader})
			resp, err := http.Get(h.srv.URL + ChatsPath)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tt.code {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tt.code, body)
			}
			if tt.code != http.StatusOK {
				return
			}
			var got ChatList
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Chats == nil || len(got.Chats) != tt.chats {
				t.Errorf("chats = %s, want %d entries", body, tt.chats)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	started := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	calls := 0
	opts := Options{Host: "127.0.0.1", Status: func() Status {
		calls++
		return Status{
			Version: "1.2.3", Go: "go1.99", StartedAt: started, UptimeSeconds: 42,
			Model: "m", Provider: "anthropic", Workspace: "workspace",
			Channels:   []ChannelStatus{{Name: Name, Enabled: true, Running: true}, {Name: "telegram", Enabled: true}},
			APIEnabled: true, Tools: ToolStatus{Exec: true},
		}
	}}
	h := newHarnessWith(t, false, opts)
	get := func(host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+StatusPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	if resp := get("evil.example"); resp.StatusCode != http.StatusForbidden || calls != 0 {
		t.Errorf("rebound host: status = %d, calls = %d", resp.StatusCode, calls)
	}
	resp := get("")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" ||
		resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET %s = %d %q %q", StatusPath, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"1.2.3","go":"go1.99","started_at":"2026-10-25T09:00:00Z","uptime_seconds":42,` +
		`"model":"m","provider":"anthropic","workspace":"workspace",` +
		`"channels":[{"name":"websocket","enabled":true,"running":true},{"name":"telegram","enabled":true,"running":false}],` +
		`"api_enabled":true,"tools":{"exec":true,"web_fetch":false}}` + "\n"
	if string(body) != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	if calls != 1 {
		t.Errorf("Status called %d times, want 1", calls)
	}
}

func TestStatusEmptyAndUnset(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Status: func() Status { return Status{} }})
	resp, err := http.Get(h.srv.URL + StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if ch, ok := got["channels"].([]any); !ok || len(ch) != 0 {
		t.Errorf("channels = %#v, want []", got["channels"])
	}
	if _, ok := got["started_at"]; ok {
		t.Errorf("zero started_at is present: %v", got)
	}

	// Without Options.Status the path is not mounted.
	h = newHarness(t, false)
	resp, err = http.Get(h.srv.URL + StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unset Status: status = %d, want 404", resp.StatusCode)
	}
}

// request sends method path with body (when not empty) to h and returns
// the status code and body. host, when set, overrides the Host header and
// origin, when set, is sent as Origin.
func (h *harness) request(t *testing.T, method, path, body, host, origin string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// managedChats returns a session manager holding the websocket chat
// "trip" and a Telegram chat, with "trip" cached as the agent loop's
// worker would hold it.
func managedChats(t *testing.T) (*session.Manager, *session.Session) {
	t.Helper()
	m := session.NewManager(session.NewJSONLStore(t.TempDir()))
	tg := session.New("telegram:42")
	tg.Append(providers.Message{Role: providers.RoleUser, Content: "not a web chat"})
	if err := m.Save(tg); err != nil {
		t.Fatal(err)
	}
	cached, err := m.GetOrCreate(Name + ":trip")
	if err != nil {
		t.Fatal(err)
	}
	cached.Append(
		providers.Message{Role: providers.RoleUser, Content: "plan the trip"},
		providers.Message{Role: providers.RoleAssistant, Content: "Where to?"},
	)
	if err := m.Save(cached); err != nil {
		t.Fatal(err)
	}
	return m, cached
}

func (h *harness) chatList(t *testing.T) []ChatInfo {
	t.Helper()
	code, body := h.request(t, http.MethodGet, ChatsPath, "", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", ChatsPath, code, body)
	}
	var list ChatList
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	return list.Chats
}

func TestRenameChat(t *testing.T) {
	m, cached := managedChats(t)
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Sessions: m})
	path := ChatsPath + "/trip"

	code, body := h.request(t, http.MethodPatch, path, `{"title":"  Summer \n  holiday "}`, "", "")
	if code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", code, body)
	}
	var info ChatInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatal(err)
	}
	if info.ChatID != "trip" || info.Title != "Summer holiday" || info.Preview != "Where to?" || !info.UpdatedAt.Equal(cached.UpdatedAt) {
		t.Errorf("PATCH body = %+v", info)
	}
	if got := h.chatList(t); len(got) != 1 || got[0].Title != "Summer holiday" || got[0].Preview != "Where to?" {
		t.Errorf("list after rename = %+v", got)
	}
	stored, err := m.Load(Name + ":trip")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata[TitleKey] != "Summer holiday" || stored.Metadata[TitleEditedKey] != true {
		t.Errorf("stored metadata = %v", stored.Metadata)
	}
	// The agent loop's next turn sees the title instead of saving over it
	// from its stale cached session.
	next, err := m.GetOrCreate(Name + ":trip")
	if err != nil {
		t.Fatal(err)
	}
	if next == cached || next.Metadata[TitleKey] != "Summer holiday" || len(next.Messages) != 2 {
		t.Errorf("cached session after rename = %+v", next)
	}

	// The longest title is accepted; a longer one is refused.
	long := strings.Repeat("é", maxTitleRunes)
	if code, body := h.request(t, http.MethodPatch, path, `{"title":"`+long+`"}`, "", ""); code != http.StatusOK || !strings.Contains(body, long) {
		t.Errorf("PATCH %d-rune title = %d %s", maxTitleRunes, code, body)
	}
	if code, _ := h.request(t, http.MethodPatch, path, `{"title":"`+long+`x"}`, "", ""); code != http.StatusBadRequest {
		t.Errorf("PATCH too long a title = %d, want 400", code)
	}

	// An empty title falls back to the first user message.
	if code, body := h.request(t, http.MethodPatch, path, `{"title":"  "}`, "", ""); code != http.StatusOK || !strings.Contains(body, `"title":"plan the trip"`) {
		t.Errorf("PATCH empty title = %d %s", code, body)
	}
	if stored, _ := m.Load(Name + ":trip"); len(stored.Metadata) != 0 {
		t.Errorf("metadata after clearing = %v, want none", stored.Metadata)
	}
	if got := h.chatList(t); len(got) != 1 || got[0].Title != "plan the trip" {
		t.Errorf("list after clearing = %+v", got)
	}
}

func TestRenameChatRefusals(t *testing.T) {
	m, _ := managedChats(t)
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Sessions: m})
	tests := []struct {
		name, path, body, host, origin string
		code                           int
	}{
		{"missing chat", "/never-seen", `{"title":"x"}`, "", "", http.StatusNotFound},
		{"invalid chat id", "/bad.id", `{"title":"x"}`, "", "", http.StatusBadRequest},
		{"too long chat id", "/" + strings.Repeat("a", 65), `{"title":"x"}`, "", "", http.StatusBadRequest},
		{"other channel's session", "/telegram:42", `{"title":"x"}`, "", "", http.StatusBadRequest},
		{"escaped other channel's session", "/telegram%3A42", `{"title":"x"}`, "", "", http.StatusBadRequest},
		{"no title", "/trip", `{}`, "", "", http.StatusBadRequest},
		{"title not a string", "/trip", `{"title":1}`, "", "", http.StatusBadRequest},
		{"unknown field", "/trip", `{"title":"x","pinned":true}`, "", "", http.StatusBadRequest},
		{"not JSON", "/trip", `title=x`, "", "", http.StatusBadRequest},
		{"body too large", "/trip", `{"title":"` + strings.Repeat(" ", maxRenameBody) + `x"}`, "", "", http.StatusBadRequest},
		{"foreign host", "/trip", `{"title":"x"}`, "evil.example", "", http.StatusForbidden},
		{"cross-origin", "/trip", `{"title":"x"}`, "", "http://evil.example", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, body := h.request(t, http.MethodPatch, ChatsPath+tt.path, tt.body, tt.host, tt.origin); code != tt.code {
				t.Errorf("PATCH = %d %s, want %d", code, body, tt.code)
			}
		})
	}
	// Nothing was renamed.
	for _, key := range []string{Name + ":trip", "telegram:42"} {
		if sess, err := m.Load(key); err != nil || len(sess.Metadata) != 0 {
			t.Errorf("%s = %+v, %v; want it unchanged", key, sess, err)
		}
	}
}

func TestDeleteChat(t *testing.T) {
	m, cached := managedChats(t)
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Sessions: m})
	path := ChatsPath + "/trip"

	for _, tt := range []struct {
		name, path, host, origin string
		code                     int
	}{
		{"invalid chat id", "/bad.id", "", "", http.StatusBadRequest},
		{"other channel's session", "/telegram:42", "", "", http.StatusBadRequest},
		{"escaped other channel's session", "/telegram%3A42", "", "", http.StatusBadRequest},
		{"foreign host", "/trip", "evil.example", "", http.StatusForbidden},
		{"cross-origin", "/trip", "", "http://evil.example", http.StatusForbidden},
	} {
		if code, body := h.request(t, http.MethodDelete, ChatsPath+tt.path, "", tt.host, tt.origin); code != tt.code {
			t.Errorf("%s: DELETE = %d %s, want %d", tt.name, code, body, tt.code)
		}
	}
	if len(h.chatList(t)) != 1 {
		t.Fatal("a refused DELETE removed the chat")
	}

	if code, body := h.request(t, http.MethodDelete, path, "", "", ""); code != http.StatusNoContent || body != "" {
		t.Fatalf("DELETE = %d %q, want 204", code, body)
	}
	if got := h.chatList(t); len(got) != 0 {
		t.Errorf("list after delete = %+v", got)
	}
	if _, err := m.Load(Name + ":trip"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Load after delete = %v, want ErrNotFound", err)
	}
	// The agent loop's next turn starts afresh rather than bringing the
	// chat back from its cache.
	next, err := m.GetOrCreate(Name + ":trip")
	if err != nil {
		t.Fatal(err)
	}
	if next == cached || len(next.Messages) != 0 {
		t.Errorf("cached session after delete = %+v, want a new one", next)
	}
	// Deleting again (or a chat never stored) is fine.
	if code, _ := h.request(t, http.MethodDelete, path, "", "", ""); code != http.StatusNoContent {
		t.Errorf("second DELETE = %d, want 204", code)
	}
	// Other channels' sessions are untouched.
	if _, err := m.Load("telegram:42"); err != nil {
		t.Errorf("telegram session after delete: %v", err)
	}
}

func TestRenameAndDeleteFailures(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Sessions: failingLoader{}})
	if code, _ := h.request(t, http.MethodPatch, ChatsPath+"/trip", `{"title":"x"}`, "", ""); code != http.StatusInternalServerError {
		t.Errorf("PATCH with a failing store = %d, want 500", code)
	}
	if code, _ := h.request(t, http.MethodDelete, ChatsPath+"/trip", "", "", ""); code != http.StatusInternalServerError {
		t.Errorf("DELETE with a failing store = %d, want 500", code)
	}

	// Without sessions there is nothing to rename or delete.
	h = newHarness(t, false)
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		if code, _ := h.request(t, method, ChatsPath+"/trip", `{"title":"x"}`, "", ""); code < 400 || code >= 500 {
			t.Errorf("%s without sessions = %d, want a 4xx", method, code)
		}
	}
}

func TestShorten(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{" a \n b\t", 10, "a b"},
		{"abcdefghij", 10, "abcdefghij"},
		{"abcdefghijk", 10, "abcdefghi…"},
		{"abcd efghijk", 6, "abcd…"},
		{"日本語のテキスト", 4, "日本語…"},
	}
	for _, tt := range tests {
		if got := shorten(tt.in, tt.n); got != tt.want {
			t.Errorf("shorten(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}
