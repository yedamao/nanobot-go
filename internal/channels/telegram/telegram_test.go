package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
)

func TestParseConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	tests := []struct {
		name    string
		raw     string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "defaults",
			raw:  `{"enabled": true, "token": "` + testToken + `"}`,
			want: Config{Enabled: true, Token: testToken, APIBase: DefaultAPIBase, PollTimeout: 30, GroupPolicy: "mention"},
		},
		{
			name: "env token wins; numbers in allowFrom",
			raw:  `{"enabled": true, "token": "1:old", "allowFrom": [42, "alice", "*"], "apiBase": "http://127.0.0.1:9/", "pollTimeout": 5, "groupPolicy": "open", "replyToMessage": true, "proxy": "127.0.0.1:8080"}`,
			env:  map[string]string{EnvToken: testToken},
			want: Config{Enabled: true, Token: testToken, AllowFrom: allowList{"42", "alice", "*"}, APIBase: "http://127.0.0.1:9",
				PollTimeout: 5, GroupPolicy: "open", ReplyToMessage: true, Proxy: "127.0.0.1:8080"},
		},
		{name: "no token", raw: `{"enabled": true}`, wantErr: "token is required"},
		{name: "malformed token", raw: `{"token": "123456:abc/../../x"}`, wantErr: "token is malformed"},
		{name: "unknown key", raw: `{"token": "` + testToken + `", "tokn": "x"}`, wantErr: "unknown field"},
		{name: "bad group policy", raw: `{"token": "` + testToken + `", "groupPolicy": "allowlist"}`, wantErr: "groupPolicy"},
		{name: "bad api base", raw: `{"token": "` + testToken + `", "apiBase": "ftp://x"}`, wantErr: "apiBase"},
		{name: "negative poll timeout", raw: `{"token": "` + testToken + `", "pollTimeout": -1}`, wantErr: "pollTimeout"},
		{name: "bad proxy scheme", raw: `{"token": "` + testToken + `", "proxy": "ftp://u:secretpw@h:1"}`, wantErr: "proxy scheme"},
		{name: "bad allowFrom entry", raw: `{"token": "` + testToken + `", "allowFrom": [true]}`, wantErr: "allowFrom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfig(json.RawMessage(tt.raw), env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				for _, secret := range []string{"abc/../../x", "secretpw", testToken} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error leaks %q: %v", secret, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestStartGetMeFailures(t *testing.T) {
	t.Run("rejected token", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.reply = func(method string, _ int) (int, string, bool) {
			return http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, method == "getMe"
		}
		err := h.ch.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("Start = %v, want a rejected-token error", err)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("error leaks the token: %v", err)
		}
		if n := len(h.f.callsOf("getUpdates")); n != 0 {
			t.Errorf("polled %d times after a failed start", n)
		}
		// Stop after a failed Start is a no-op.
		if err := h.ch.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	t.Run("unreachable server", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.srv.Close()
		err := h.ch.Start(context.Background())
		if err == nil {
			t.Fatal("Start succeeded against a closed server")
		}
		if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "TEST-secret") {
			t.Errorf("error leaks the token: %v", err)
		}
		if !strings.Contains(err.Error(), "/bot<redacted>/getMe") {
			t.Errorf("error = %v, want the redacted URL", err)
		}
	})
}

func TestTokenRedaction(t *testing.T) {
	c := &client{base: "http://x", token: testToken}
	err := c.redact(fmt.Errorf("wrap: %w", &json.SyntaxError{}))
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("leak")
	}
	err = c.redact(fmt.Errorf("Post \"http://x/bot%s/getMe\": %w", testToken, context.Canceled))
	if strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "bot<redacted>/getMe") {
		t.Errorf("redact = %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Error("redact dropped context.Canceled from the chain")
	}
	if errors.Unwrap(c.redact(errors.New("bot"+testToken))) != nil {
		t.Error("redact kept a non-context error in the chain")
	}
	// Descriptions from the server are scrubbed too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":false,"error_code":400,"description":"bad token %s"}`, testToken)
	}))
	defer srv.Close()
	c = &client{base: srv.URL, token: testToken, http: srv.Client()}
	err = c.call(context.Background(), "getMe", struct{}{}, nil)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("call = %v", err)
	}
}

func TestPollingPublishesAndAdvancesOffset(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	if n := len(h.f.callsOf("deleteWebhook")); n != 1 {
		t.Errorf("deleteWebhook calls = %d, want 1", n)
	}
	h.push(dm(10, alice, "hello"), dm(11, alice, "again"))

	first := h.inbound(t)
	want := bus.InboundMessage{
		Channel:  "telegram",
		SenderID: "42|alice",
		ChatID:   "42",
		Content:  "hello",
		Metadata: map[string]any{"message_id": int64(100), "user_id": int64(42), "username": "alice",
			"first_name": "Alice", "is_group": false, "is_forum": false},
	}
	first.Timestamp = time.Time{}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("inbound = %+v\nwant %+v", first, want)
	}
	if first.SessionKey() != "telegram:42" {
		t.Errorf("session key = %s", first.SessionKey())
	}
	if second := h.inbound(t); second.Content != "again" {
		t.Errorf("second inbound = %q", second.Content)
	}

	polls := h.f.waitFor("getUpdates", 2)
	if _, ok := polls[0].Body["offset"]; ok {
		t.Errorf("first getUpdates sent an offset: %v", polls[0].Body)
	}
	if got := polls[1].Body["offset"]; got != float64(12) {
		t.Errorf("second offset = %v, want 12", got)
	}
	if got := polls[0].Body["timeout"]; got != float64(1) {
		t.Errorf("timeout = %v, want the configured 1", got)
	}
	if got := polls[0].Body["allowed_updates"]; !reflect.DeepEqual(got, []any{"message"}) {
		t.Errorf("allowed_updates = %v", got)
	}
}

func TestIgnoredUpdates(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	bot := &user{ID: 7, IsBot: true, Username: "other_bot"}
	h.push(
		update{UpdateID: 1}, // an edit or another kind: no message
		dm(2, bot, "from a bot"),
		update{UpdateID: 3, Message: &message{MessageID: 1, Chat: chat{ID: -1, Type: "channel"}, Text: "channel post"}},
		update{UpdateID: 4, Message: &message{MessageID: 2, From: alice, Chat: chat{ID: 42, Type: "private"}}}, // a sticker
	)
	h.noInbound(t)
}

func TestIsAllowed(t *testing.T) {
	bob := &user{ID: 7, Username: "Bob"}
	anon := &user{ID: 8}
	tests := []struct {
		allow []string
		u     *user
		want  bool
	}{
		{nil, bob, false},
		{[]string{"*"}, anon, true},
		{[]string{"7"}, bob, true},
		{[]string{"Bob"}, bob, true},
		{[]string{"@bob"}, bob, true},
		{[]string{"7|Bob"}, bob, true},
		{[]string{"8"}, bob, false},
		{[]string{"Bob"}, anon, false},
		{[]string{"8"}, anon, true},
	}
	for _, tt := range tests {
		h := newHarness(t, func(c *Config) { c.AllowFrom = tt.allow })
		if got := h.ch.isAllowed(tt.u); got != tt.want {
			t.Errorf("allowFrom %q, user %+v: isAllowed = %v, want %v", tt.allow, tt.u, got, tt.want)
		}
	}
}

func TestPairingFlow(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowFrom = nil })
	h.start(t)
	h.push(dm(1, alice, "hi"))
	sent := h.f.waitFor("sendMessage", 1)
	text, _ := sent[0].Body["text"].(string)
	pending, err := h.store.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	code := pending[0].Code
	if pending[0].SenderID != "42" || !strings.Contains(text, code) || sent[0].Body["chat_id"] != float64(42) {
		t.Errorf("pairing reply %q to %v for request %+v", text, sent[0].Body["chat_id"], pending[0])
	}
	if h.bus.InboundLen() != 0 {
		t.Error("a denied message reached the bus")
	}
	// Writing again re-sends the same code.
	h.push(dm(2, alice, "hello?"))
	sent = h.f.waitFor("sendMessage", 2)
	if text, _ := sent[1].Body["text"].(string); !strings.Contains(text, code) {
		t.Errorf("second reply %q lacks code %s", text, code)
	}

	if _, ok, err := h.store.Approve(code); !ok || err != nil {
		t.Fatalf("Approve = %v, %v", ok, err)
	}
	h.push(dm(3, alice, "now?"))
	if msg := h.inbound(t); msg.Content != "now?" {
		t.Errorf("inbound after approval = %q", msg.Content)
	}
}

func TestDeniedGroupSenderGetsNoCode(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowFrom = nil })
	h.start(t)
	h.push(groupMsg(1, alice, "@"+botName+" hi"))
	h.noInbound(t)
	if n := len(h.f.callsOf("sendMessage")); n != 0 {
		t.Errorf("sent %d messages to a denied group sender", n)
	}
	if p, _ := h.store.Pending(); len(p) != 0 {
		t.Errorf("pairing requests for a group sender: %v", p)
	}
}

func TestPairingDisabledWithoutStore(t *testing.T) {
	f := newFakeAPI(t)
	b := bus.NewMessageBus(4)
	cfg := Config{Token: testToken, APIBase: f.srv.URL, PollTimeout: 1, GroupPolicy: GroupPolicyMention}
	ch, err := New(cfg, channels.Deps{Bus: b}, Options{HTTPClient: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if ch.isAllowed(alice) {
		t.Error("empty allowFrom without pairing allowed a sender")
	}
}

func TestGroupAddressing(t *testing.T) {
	carol := &user{ID: 43, Username: "carol"}
	mention := groupMsg(1, alice, "hey @Nano_Bot what's up")
	withEntity := groupMsg(2, alice, "😀 @nano_bot ping")
	withEntity.Message.Entities = []entity{{Type: "mention", Offset: 3, Length: 9}} // 😀 is two UTF-16 units
	textMention := groupMsg(3, alice, "Nano please")
	textMention.Message.Entities = []entity{{Type: "text_mention", Offset: 0, Length: 4, User: &user{ID: botID}}}
	replyToBot := groupMsg(4, alice, "and then?")
	replyToBot.Message.ReplyToMessage = &message{MessageID: 5, From: &user{ID: botID, IsBot: true}, Text: "earlier answer"}
	replyToCarol := groupMsg(5, alice, "agreed")
	replyToCarol.Message.ReplyToMessage = &message{MessageID: 6, From: carol, Text: "x"}
	chatter := groupMsg(6, alice, "just chatting")
	cmdOther := groupMsg(7, alice, "/new@other_bot")
	cmd := groupMsg(8, alice, "/new@nano_bot")
	cmdArgs := groupMsg(9, alice, "/help@Nano_Bot me")

	tests := []struct {
		policy string
		u      update
		want   string // inbound content, "" for none
	}{
		{"mention", mention, "hey @Nano_Bot what's up"},
		{"mention", withEntity, "😀 @nano_bot ping"},
		{"mention", textMention, "Nano please"},
		{"mention", replyToBot, "[Reply to bot: earlier answer]\nand then?"},
		{"mention", replyToCarol, ""},
		{"mention", chatter, ""},
		{"mention", cmdOther, ""},
		{"mention", cmd, "/new"},
		{"mention", cmdArgs, "/help me"},
		{"open", chatter, "just chatting"},
		{"open", replyToCarol, "[Reply to @carol: x]\nagreed"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%d", tt.policy, tt.u.UpdateID), func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.GroupPolicy = tt.policy })
			h.start(t)
			h.push(tt.u)
			if tt.want == "" {
				h.noInbound(t)
				return
			}
			msg := h.inbound(t)
			if msg.Content != tt.want || msg.ChatID != "-100500" || msg.Metadata["is_group"] != true {
				t.Errorf("inbound = %+v, want content %q", msg, tt.want)
			}
		})
	}
}

func TestTopicSessionKey(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.GroupPolicy = GroupPolicyOpen })
	h.start(t)
	u := groupMsg(1, alice, "in a topic")
	u.Message.MessageThreadID = 77
	u.Message.IsTopicMessage = true
	u.Message.Chat.IsForum = true
	// A reply thread in a regular supergroup is not a topic.
	plain := groupMsg(2, alice, "a reply thread")
	plain.Message.MessageThreadID = 5
	h.push(u, plain)

	msg := h.inbound(t)
	if msg.SessionKey() != "telegram:-100500:topic:77" || msg.Metadata["message_thread_id"] != int64(77) {
		t.Errorf("topic message: key %s, metadata %v", msg.SessionKey(), msg.Metadata)
	}
	msg = h.inbound(t)
	if _, ok := msg.Metadata["message_thread_id"]; ok || msg.SessionKey() != "telegram:-100500" {
		t.Errorf("non-topic message: key %s, metadata %v", msg.SessionKey(), msg.Metadata)
	}

	// The reply goes back to the topic.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: "-100500", Content: "ok",
		Metadata: map[string]any{"message_thread_id": float64(77)}}); err != nil {
		t.Fatal(err)
	}
	if got := h.f.callsOf("sendMessage")[0].Body["message_thread_id"]; got != float64(77) {
		t.Errorf("message_thread_id = %v", got)
	}
}

func TestStartCommandIsAnsweredLocally(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.push(dm(1, alice, "/start"))
	sent := h.f.waitFor("sendMessage", 1)
	if text, _ := sent[0].Body["text"].(string); !strings.Contains(text, "Hi Alice") {
		t.Errorf("greeting = %q", text)
	}
	h.noInbound(t)
}

func TestSendChunksPlainText(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ReplyToMessage = true })
	h.start(t)
	long := strings.Repeat("a <b>&amp; **bold** ", 300) // 6000 bytes
	err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: "42", Content: long,
		Metadata: map[string]any{"message_id": int64(100)}})
	if err != nil {
		t.Fatal(err)
	}
	sent := h.f.callsOf("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	var joined []string
	for _, s := range sent {
		text := s.Body["text"].(string)
		if utf16Len(text) > maxMessageLen {
			t.Errorf("chunk of %d units", utf16Len(text))
		}
		if _, ok := s.Body["parse_mode"]; ok {
			t.Errorf("parse_mode set: %v", s.Body["parse_mode"])
		}
		if s.Body["chat_id"] != float64(42) {
			t.Errorf("chat_id = %v", s.Body["chat_id"])
		}
		rp, _ := s.Body["reply_parameters"].(map[string]any)
		if rp["message_id"] != float64(100) || rp["allow_sending_without_reply"] != true {
			t.Errorf("reply_parameters = %v", s.Body["reply_parameters"])
		}
		joined = append(joined, text)
	}
	// Plain text goes out verbatim: no escaping, no markup.
	if got := strings.Join(joined, " "); got != strings.TrimSpace(long) {
		t.Errorf("text changed in transit")
	}

	// Without replyToMessage there is no reply_parameters, unless the
	// message names one.
	h2 := newHarness(t, nil)
	h2.start(t)
	h2.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "x", Metadata: map[string]any{"message_id": int64(5)}})
	h2.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "y", ReplyTo: "9"})
	sent = h2.f.callsOf("sendMessage")
	if _, ok := sent[0].Body["reply_parameters"]; ok {
		t.Errorf("reply_parameters without replyToMessage: %v", sent[0].Body)
	}
	if rp, _ := sent[1].Body["reply_parameters"].(map[string]any); rp["message_id"] != float64(9) {
		t.Errorf("ReplyTo ignored: %v", sent[1].Body)
	}
}

func TestSendEdgeCases(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "not-a-number", Content: "x"})
	if !channels.IsPermanent(err) {
		t.Errorf("invalid chat id: %v, want a permanent error", err)
	}
	for _, content := range []string{"", "  \n", emptyMessage} {
		if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: content}); err != nil {
			t.Errorf("Send(%q) = %v", content, err)
		}
	}
	if n := len(h.f.callsOf("sendMessage")); n != 0 {
		t.Errorf("sent %d empty messages", n)
	}
}

func TestSendFloodControlAndRejections(t *testing.T) {
	tooMany := `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`
	tests := []struct {
		name      string
		reply     func(n int) (int, string, bool)
		wantErr   bool
		permanent bool
		sleeps    []time.Duration
		attempts  int
	}{
		{
			name:     "429 then success",
			reply:    func(n int) (int, string, bool) { return 429, tooMany, n == 1 },
			sleeps:   []time.Duration{3 * time.Second},
			attempts: 2,
		},
		{
			name:     "429 every time",
			reply:    func(int) (int, string, bool) { return 429, tooMany, true },
			wantErr:  true,
			sleeps:   []time.Duration{3 * time.Second, 3 * time.Second},
			attempts: sendAttempts,
		},
		{
			name: "403 blocked",
			reply: func(int) (int, string, bool) {
				return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, true
			},
			wantErr: true, permanent: true, attempts: 1,
		},
		{
			name: "500",
			reply: func(int) (int, string, bool) {
				return 500, `{"ok":false,"error_code":500,"description":"Internal"}`, true
			},
			wantErr: true, attempts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.start(t)
			h.f.mu.Lock()
			h.f.reply = func(method string, n int) (int, string, bool) {
				if method != "sendMessage" {
					return 0, "", false
				}
				return tt.reply(n)
			}
			h.f.mu.Unlock()
			err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "hi"})
			if (err != nil) != tt.wantErr || channels.IsPermanent(err) != tt.permanent {
				t.Fatalf("Send = %v (permanent %v)", err, channels.IsPermanent(err))
			}
			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks the token: %v", err)
			}
			if n := len(h.f.callsOf("sendMessage")); n != tt.attempts {
				t.Errorf("attempts = %d, want %d", n, tt.attempts)
			}
			var got []time.Duration
			for len(h.sleeps) > 0 {
				got = append(got, <-h.sleeps)
			}
			if !reflect.DeepEqual(got, tt.sleeps) {
				t.Errorf("sleeps = %v, want %v", got, tt.sleeps)
			}
		})
	}
}

func TestPollingBackoffAndRetryAfter(t *testing.T) {
	h := newHarness(t, nil)
	h.ch.minBackoff, h.ch.maxBackoff = time.Second, 4*time.Second
	// Fail getUpdates five times (500 four times, then a 429), then serve.
	h.f.reply = func(method string, n int) (int, string, bool) {
		if method != "getUpdates" {
			return 0, "", false
		}
		switch {
		case n <= 4:
			return 500, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`, true
		case n == 5:
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`, true
		}
		return 0, "", false
	}
	// The stub sleep returns at once, so let each one through explicitly.
	h.ch.sleep = func(ctx context.Context, d time.Duration) error {
		h.sleeps <- d
		return ctx.Err()
	}
	h.start(t)
	var got []time.Duration
	for range 5 {
		select {
		case d := <-h.sleeps:
			got = append(got, d)
		case <-time.After(5 * time.Second):
			t.Fatalf("waits so far: %v", got)
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 7 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
	// Polling recovers.
	h.push(dm(1, alice, "back"))
	if msg := h.inbound(t); msg.Content != "back" {
		t.Errorf("inbound = %q", msg.Content)
	}
}

func TestStopEndsLongPollPromptly(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PollTimeout = 50 })
	if err := h.ch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.f.waitFor("getUpdates", 1) // the long poll is in flight
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := h.ch.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop took %s", d)
	}
	if err := h.ch.Stop(ctx); err != nil {
		t.Errorf("second Stop = %v", err)
	}
	n := len(h.f.callsOf("getUpdates"))
	time.Sleep(50 * time.Millisecond)
	if m := len(h.f.callsOf("getUpdates")); m != n {
		t.Errorf("polled after Stop: %d -> %d", n, m)
	}
}

func TestTypingWhileTurnRuns(t *testing.T) {
	h := newHarness(t, nil)
	h.ch.typingInterval = 20 * time.Millisecond
	h.start(t)
	h.push(dm(1, alice, "think hard"))
	msg := h.inbound(t)
	calls := h.f.waitFor("sendChatAction", 2) // refreshed while the turn runs
	if calls[0].Body["action"] != "typing" || calls[0].Body["chat_id"] != float64(42) {
		t.Errorf("chat action = %v", calls[0].Body)
	}
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: msg.ChatID, Content: "done"}); err != nil {
		t.Fatal(err)
	}
	h.ch.mu.Lock()
	n := len(h.ch.typing)
	h.ch.mu.Unlock()
	if n != 0 {
		t.Errorf("%d typing indicators still running after the reply", n)
	}
	time.Sleep(30 * time.Millisecond) // let a call already in flight land
	before := len(h.f.callsOf("sendChatAction"))
	time.Sleep(100 * time.Millisecond)
	if after := len(h.f.callsOf("sendChatAction")); after != before {
		t.Errorf("typing continued after the reply: %d -> %d", before, after)
	}
}

func TestFactoryRegistersAndReadsEnv(t *testing.T) {
	f := newFakeAPI(t)
	r := channels.NewRegistry()
	getenv := func(k string) string {
		if k == EnvToken {
			return testToken
		}
		return ""
	}
	if err := r.Register(Name, Factory(Options{Getenv: getenv, HTTPClient: f.srv.Client()})); err != nil {
		t.Fatal(err)
	}
	sections := map[string]json.RawMessage{Name: json.RawMessage(`{"enabled": true, "apiBase": "` + f.srv.URL + `"}`)}
	chans, err := r.Build(sections, channels.Deps{Bus: bus.NewMessageBus(1)})
	if err != nil || len(chans) != 1 || chans[0].Name() != Name {
		t.Fatalf("Build = %v, %v", chans, err)
	}
	if err := chans[0].Start(context.Background()); err != nil {
		t.Fatalf("Start with the env token: %v", err)
	}
	chans[0].Stop(context.Background())

	// Without the env token the section must carry one.
	r2 := channels.NewRegistry()
	r2.Register(Name, Factory(Options{}))
	if _, err := r2.Build(sections, channels.Deps{Bus: bus.NewMessageBus(1)}); err == nil || !strings.Contains(err.Error(), EnvToken) {
		t.Errorf("Build without a token = %v", err)
	}
}

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  []string
	}{
		{"short", "hello", 10, []string{"hello"}},
		{"empty", "  \n ", 10, nil},
		{"newline preferred", "aaaa bbb\ncc dd", 10, []string{"aaaa bbb", "cc dd"}},
		{"space fallback", "aaaa bbbb cccc", 10, []string{"aaaa bbbb", "cccc"}},
		{"hard cut", "abcdefghijkl", 5, []string{"abcde", "fghij", "kl"}},
		{"emoji count twice", "😀😀😀", 4, []string{"😀😀", "😀"}},
		{"never splits a surrogate pair", "a😀b", 2, []string{"a", "😀", "b"}},
		{"multibyte BMP", "ééééé", 2, []string{"éé", "éé", "é"}},
		{"tiny limit", "😀", 1, []string{"😀"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitMessage(tt.text, tt.limit)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitMessage(%q, %d) = %q, want %q", tt.text, tt.limit, got, tt.want)
			}
		})
	}
	// A long reply fits Telegram's limit in every chunk.
	text := strings.Repeat("word 😀 ", 2000)
	for _, c := range splitMessage(text, maxMessageLen) {
		if utf16Len(c) > maxMessageLen {
			t.Fatalf("chunk of %d units", utf16Len(c))
		}
	}
}

func TestMetaInt(t *testing.T) {
	md := map[string]any{"a": int64(1), "b": 2, "c": float64(3), "d": json.Number("4"), "e": "5", "f": 1.5, "g": "x"}
	for k, want := range map[string]int64{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5} {
		if got, ok := metaInt(md, k); !ok || got != want {
			t.Errorf("metaInt(%s) = %d, %v", k, got, ok)
		}
	}
	for _, k := range []string{"f", "g", "missing"} {
		if _, ok := metaInt(md, k); ok {
			t.Errorf("metaInt(%s) succeeded", k)
		}
	}
}
