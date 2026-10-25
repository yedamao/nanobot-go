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

// failingLoader fails every load with an error other than not-found.
type failingLoader struct{}

func (failingLoader) Load(string) (*session.Session, error) {
	return nil, errors.New("disk on fire")
}

func (failingLoader) List() ([]session.Info, error) {
	return nil, errors.New("disk on fire")
}

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
		loader SessionReader
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
		reader SessionReader
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
