package weixin

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
)

func TestInboundReachesBusWithSessionKey(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.f.push([]map[string]any{text("m1", alice, "ctx-1", "hello")}, "cursor-1")

	in := h.inbound(t)
	if in.Channel != Name || in.SenderID != alice || in.ChatID != alice || in.Content != "hello" {
		t.Errorf("inbound = %+v", in)
	}
	if got := in.SessionKey(); got != "weixin:"+alice {
		t.Errorf("session key = %q", got)
	}
	if in.Metadata["message_id"] != "m1" {
		t.Errorf("metadata = %v", in.Metadata)
	}
	h.f.waitFor("notifystart", 1)

	// Typing starts with a ticket from getconfig, for the message's context.
	cfgCall := h.f.waitFor("getconfig", 1)[0]
	if cfgCall.Body["ilink_user_id"] != alice || cfgCall.Body["context_token"] != "ctx-1" {
		t.Errorf("getconfig body = %v", cfgCall.Body)
	}
	typing := h.f.waitFor("sendtyping", 1)[0]
	if typing.Body["typing_ticket"] != "ticket-1" || typing.Body["status"] != float64(typingStatusTyping) {
		t.Errorf("sendtyping body = %v", typing.Body)
	}
}

func TestPollingAdvancesAndSavesTheCursor(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.f.push([]map[string]any{text("m1", alice, "ctx-1", "one")}, "cursor-1")
	h.inbound(t)
	h.f.push(nil, "") // an empty cursor keeps the old one
	h.f.push(nil, "cursor-2")
	calls := h.f.waitFor("getupdates", 4)
	var bufs []string
	for _, c := range calls {
		bufs = append(bufs, c.Body["get_updates_buf"].(string))
	}
	if want := []string{"", "cursor-1", "cursor-1", "cursor-2"}; strings.Join(bufs, ",") != strings.Join(want, ",") {
		t.Errorf("cursors sent = %q, want %q", bufs, want)
	}

	// Every request carries nanobot's headers and base_info.
	c := calls[0]
	for k, want := range map[string]string{
		"Authorization":           "Bearer " + testToken,
		"AuthorizationType":       "ilink_bot_token",
		"iLink-App-Id":            "bot",
		"iLink-App-ClientVersion": strconv.Itoa(2<<16 | 4<<8 | 6),
		"Content-Type":            "application/json",
	} {
		if got := c.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	uin, err := base64.StdEncoding.DecodeString(c.Header.Get("X-WECHAT-UIN"))
	if _, perr := strconv.ParseUint(string(uin), 10, 32); err != nil || perr != nil {
		t.Errorf("X-WECHAT-UIN = %q", c.Header.Get("X-WECHAT-UIN"))
	}
	bi, _ := c.Body["base_info"].(map[string]any)
	if bi["channel_version"] != "2.4.6" || !strings.HasPrefix(bi["bot_agent"].(string), "nanobot-go/test") {
		t.Errorf("base_info = %v", bi)
	}

	// The cursor and the context token are saved, mode 0600.
	st, err := h.state.load()
	if err != nil || st.GetUpdatesBuf != "cursor-2" || st.ContextTokens[alice] != "ctx-1" || st.Token != testToken {
		t.Errorf("state = %+v, %v", st, err)
	}
	if fi, err := os.Stat(h.state.path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("account.json mode = %v, %v", fi.Mode(), err)
	}
}

func TestRestartResumesFromSavedState(t *testing.T) {
	h := newHarness(t, nil)
	h.state.update(func(st *accountState) bool {
		st.GetUpdatesBuf = "saved-cursor"
		st.ContextTokens = map[string]string{alice: "saved-ctx"}
		return true
	})
	h.start(t)
	if got := h.f.waitFor("getupdates", 1)[0].Body["get_updates_buf"]; got != "saved-cursor" {
		t.Errorf("first cursor = %v", got)
	}
	// The restored context token has no age, so it is refreshed first.
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 200, `{"ret":0,"context_token":"fresh-ctx"}`, c.Endpoint == "getconfig"
	})
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: alice, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got := h.f.callsOf("getconfig")[0].Body["context_token"]; got != "saved-ctx" {
		t.Errorf("refresh sent %v", got)
	}
	if _, toks, _ := sentTexts(h.f.callsOf("sendmessage")); len(toks) != 1 || toks[0] != "fresh-ctx" {
		t.Errorf("sent with %q", toks)
	}
}

func TestServerSuggestedPollTimeout(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	for _, tc := range []struct {
		ms   int
		want time.Duration
	}{{7000, 7 * time.Second}, {1000, minPollTimeout}} {
		h.f.pushRaw(`{"ret":0,"msgs":[],"longpolling_timeout_ms":` + strconv.Itoa(tc.ms) + `}`)
		h.sync(t)
		h.ch.mu.Lock()
		got := h.ch.pollTimeout
		h.ch.mu.Unlock()
		if got != tc.want {
			t.Errorf("after %dms: poll timeout = %v, want %v", tc.ms, got, tc.want)
		}
	}
}

func TestDuplicatesAndBotMessagesAreSkipped(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	own := text("m0", alice, "ctx", "echo")
	own["message_type"] = messageTypeBot
	h.f.push([]map[string]any{own, text("m1", alice, "ctx", "first"), text("m1", alice, "ctx", "first again")}, "c1")
	if in := h.inbound(t); in.Content != "first" {
		t.Errorf("inbound = %q", in.Content)
	}
	h.noInbound(t)
}

func TestMessageText(t *testing.T) {
	item := func(typ int, extra map[string]any) wireItem {
		it := wireItem{Type: typ}
		switch typ {
		case itemText:
			it.TextItem = &textItem{Text: extra["text"].(string)}
			if r, ok := extra["ref"].(*refMsg); ok {
				it.RefMsg = r
			}
		case itemVoice:
			it.VoiceItem = &voiceItem{Text: extra["text"].(string)}
		case itemFile:
			it.FileItem = &fileItem{FileName: extra["name"].(string)}
		}
		return it
	}
	for _, tc := range []struct {
		name  string
		items []wireItem
		want  string
	}{
		{"text", []wireItem{item(itemText, map[string]any{"text": "hi"})}, "hi"},
		{"quote", []wireItem{item(itemText, map[string]any{"text": "yes", "ref": &refMsg{Title: "Bob",
			MessageItem: &wireItem{Type: itemText, TextItem: &textItem{Text: "ok?"}}}})}, "[引用: Bob | ok?]\nyes"},
		{"quoted media", []wireItem{item(itemText, map[string]any{"text": "this", "ref": &refMsg{Title: "pic",
			MessageItem: &wireItem{Type: itemImage}}})}, "this"},
		{"voice with transcript", []wireItem{item(itemVoice, map[string]any{"text": "call me"})}, "[voice] call me"},
		{"voice", []wireItem{item(itemVoice, map[string]any{"text": ""})}, "[voice]"},
		{"file", []wireItem{item(itemFile, map[string]any{"name": "a.pdf"})}, "[file: a.pdf]"},
		{"unnamed file", []wireItem{item(itemFile, map[string]any{"name": ""})}, "[file: unknown]"},
		{"image and text", []wireItem{{Type: itemImage}, item(itemText, map[string]any{"text": "look"})}, "[image]\nlook"},
		{"video", []wireItem{{Type: itemVideo}}, "[video]"},
		{"empty", []wireItem{item(itemText, map[string]any{"text": ""}), {Type: 99}}, ""},
	} {
		if got := messageText(tc.items); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

var pairingCodeRE = regexp.MustCompile(`[A-Z0-9]{4}-[A-Z0-9]{4}`)

func TestUnknownSenderGetsPairingCode(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowFrom = nil })
	h.start(t)
	h.f.push([]map[string]any{text("m1", bob, "bob-ctx", "let me in")}, "c1")
	send := h.f.waitFor("sendmessage", 1)[0]
	msg := send.Body["msg"].(map[string]any)
	texts, toks, _ := sentTexts([]apiCall{send})
	code := pairingCodeRE.FindString(texts[0])
	if msg["to_user_id"] != bob || toks[0] != "bob-ctx" || code == "" {
		t.Fatalf("pairing reply = %v", msg)
	}
	if !strings.Contains(texts[0], "nanobot pairing approve "+code) {
		t.Errorf("reply = %q", texts[0])
	}
	h.noInbound(t)
	// A denied sender's context token is not kept.
	if st, _ := h.state.load(); st.ContextTokens[bob] != "" {
		t.Errorf("state keeps the denied sender's context: %v", st.ContextTokens)
	}

	// The same sender gets the same code; once approved, they get through.
	h.f.push([]map[string]any{text("m2", bob, "bob-ctx2", "again")}, "c2")
	texts, _, _ = sentTexts(h.f.waitFor("sendmessage", 2)[1:])
	if pairingCodeRE.FindString(texts[0]) != code {
		t.Errorf("second code = %q, want %s", texts[0], code)
	}
	if _, ok, err := h.store.Approve(code); !ok || err != nil {
		t.Fatalf("approve: %v %v", ok, err)
	}
	h.f.push([]map[string]any{text("m3", bob, "bob-ctx3", "thanks")}, "c3")
	if in := h.inbound(t); in.SenderID != bob || in.Content != "thanks" {
		t.Errorf("inbound after approval = %+v", in)
	}
}

func TestDeniedWithoutPairingCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		from   string
		ctx    string
		noPair bool
	}{
		{"group", "room@chatroom", "ctx", false},
		{"no context token", bob, "", false},
		{"pairing disabled", bob, "ctx", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.AllowFrom = nil })
			if tc.noPair {
				h.ch.pairing = nil
			}
			h.start(t)
			h.f.push([]map[string]any{text("m1", tc.from, tc.ctx, "hi")}, "c1")
			h.noInbound(t)
			if n := len(h.f.callsOf("sendmessage")); n != 0 {
				t.Errorf("%d messages sent", n)
			}
			if !strings.Contains(h.logs.String(), "access denied") {
				t.Errorf("no denial logged:\n%s", h.logs)
			}
		})
	}
}

func TestAllowAll(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowFrom = []string{"*"} })
	h.start(t)
	h.f.push([]map[string]any{text("m1", bob, "ctx", "hi")}, "c1")
	if in := h.inbound(t); in.SenderID != bob {
		t.Errorf("inbound = %+v", in)
	}
}

// greet makes alice write, so her context token is cached.
func (h *harness) greet(t *testing.T, id, ctxToken string) {
	t.Helper()
	h.f.push([]map[string]any{text(id, alice, ctxToken, "hi")}, "c-"+id)
	h.inbound(t)
}

func send(t *testing.T, h *harness, content string) error {
	t.Helper()
	return h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: alice, Content: content})
}

func TestSendChunksLongReplies(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.waitFor("sendtyping", 1)
	long := strings.Repeat("ab ", 700) + "\n\n" + strings.Repeat("cd ", 700) // 4202 characters
	if err := send(t, h, long); err != nil {
		t.Fatal(err)
	}
	calls := h.f.callsOf("sendmessage")
	texts, toks, ids := sentTexts(calls)
	n := len(texts)
	if n < 3 || n != len(splitMessage(long, maxMessageLen)) {
		t.Fatalf("%d messages sent", n)
	}
	seen := map[string]bool{}
	for i, s := range texts {
		if utf8.RuneCountInString(s) > maxMessageLen || toks[i] != "ctx-1" || seen[ids[i]] || !strings.HasPrefix(ids[i], "nanobot-") {
			t.Errorf("chunk %d: %d chars, context %q, client_id %q", i, utf8.RuneCountInString(s), toks[i], ids[i])
		}
		seen[ids[i]] = true
	}
	msg := calls[0].Body["msg"].(map[string]any)
	if msg["to_user_id"] != alice || msg["message_type"] != float64(messageTypeBot) || msg["message_state"] != float64(messageStateFinish) || msg["from_user_id"] != "" {
		t.Errorf("msg = %v", msg)
	}
	if strings.ReplaceAll(strings.Join(texts, " "), " ", "") != strings.ReplaceAll(strings.ReplaceAll(long, " ", ""), "\n", "") {
		t.Error("the chunks do not add up to the reply")
	}
	// Sending clears the typing indicator.
	if typing := h.f.waitFor("sendtyping", 2); typing[len(typing)-1].Body["status"] != float64(typingStatusCancel) {
		t.Errorf("last sendtyping = %v", typing[len(typing)-1].Body)
	}
	// Markdown WeChat cannot show is cleaned up.
	if err := send(t, h, "a <b> ~~c~~ ![img](x.png)"); err != nil {
		t.Fatal(err)
	}
	if texts, _, _ := sentTexts(h.f.callsOf("sendmessage")[n:]); texts[0] != "a ＜b＞ c" {
		t.Errorf("sanitized = %q", texts[0])
	}
}

func TestSendSkipsProgress(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.waitFor("sendtyping", 1)
	for _, key := range []string{bus.MetaProgress, bus.MetaDelta} {
		msg := bus.OutboundMessage{Channel: Name, ChatID: alice, Content: "read notes.md", Metadata: map[string]any{key: true}}
		if err := h.ch.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send(%s) = %v", key, err)
		}
	}
	if n := len(h.f.callsOf("sendmessage")); n != 0 {
		t.Errorf("progress and deltas sent as %d WeChat messages", n)
	}
	// The typing indicator is left running for the reply.
	if n := len(h.f.callsOf("sendtyping")); n != 1 {
		t.Errorf("%d sendtyping calls, want only the greeting's", n)
	}
}

func TestSendRefreshesOldContextToken(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.waitFor("sendtyping", 1) // the typing ticket's getconfig is done
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 200, `{"ret":0,"context_token":"ctx-2"}`, c.Endpoint == "getconfig" && c.Body["context_token"] == "ctx-1"
	})
	before := len(h.f.callsOf("getconfig"))
	// Young enough: no refresh.
	h.clock.advance(30 * time.Second)
	send(t, h, "one")
	if n := len(h.f.callsOf("getconfig")); n != before {
		t.Errorf("refreshed a %v old token", 30*time.Second)
	}
	h.clock.advance(31 * time.Second)
	send(t, h, "two")
	_, toks, _ := sentTexts(h.f.callsOf("sendmessage"))
	if strings.Join(toks, ",") != "ctx-1,ctx-2" {
		t.Errorf("context tokens used = %q", toks)
	}
	if st, _ := h.state.load(); st.ContextTokens[alice] != "ctx-2" {
		t.Errorf("saved context = %v", st.ContextTokens)
	}
}

func TestReplyWithoutContextIsHeldUntilUserWrites(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	if err := send(t, h, "reminder"); err != nil {
		t.Fatalf("Send = %v, want nil (held)", err)
	}
	if n := len(h.f.callsOf("sendmessage")); n != 0 {
		t.Fatalf("%d messages sent without a context token", n)
	}
	h.greet(t, "m1", "ctx-1")
	texts, toks, _ := sentTexts(h.f.waitFor("sendmessage", 1))
	if texts[0] != "reminder" || toks[0] != "ctx-1" {
		t.Errorf("held reply sent as %q with %q", texts, toks)
	}
}

func TestQuotaRejectionHoldsReply(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 200, `{"ret":-2,"errmsg":"quota"}`, c.Endpoint == "sendmessage" && n == 1
	})
	if err := send(t, h, "answer"); err != nil {
		t.Fatalf("Send = %v, want nil (held)", err)
	}
	h.greet(t, "m2", "ctx-2")
	texts, toks, _ := sentTexts(h.f.waitFor("sendmessage", 2))
	if texts[1] != "answer" || toks[1] != "ctx-2" {
		t.Errorf("sent %q with %q", texts, toks)
	}
}

func TestContextBudgetAndHeldReplyLimit(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	for i := range contextMessageBudget + 4 {
		if err := send(t, h, "msg "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.f.callsOf("sendmessage")); n != contextMessageBudget {
		t.Fatalf("%d sent with one context token, want %d", n, contextMessageBudget)
	}
	// Four were held; the oldest beyond three is dropped.
	h.greet(t, "m2", "ctx-2")
	texts, _, _ := sentTexts(h.f.waitFor("sendmessage", contextMessageBudget+3)[contextMessageBudget:])
	if want := []string{"msg 9", "msg 10", "msg 11"}; strings.Join(texts, ",") != strings.Join(want, ",") {
		t.Errorf("held replies sent = %q, want %q", texts, want)
	}
}

func TestRetryResumesAfterPartialFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 502, `bad gateway`, c.Endpoint == "sendmessage" && n == 2
	})
	long := strings.Repeat("x", 1500) + "\n" + strings.Repeat("y", 1500) + "\n" + strings.Repeat("z", 1500)
	err := send(t, h, long)
	if err == nil || channels.IsPermanent(err) {
		t.Fatalf("first attempt = %v, want a retryable error", err)
	}
	// The manager's retry sends only what is missing, with the same
	// client_id for the chunk that failed.
	if err := send(t, h, long); err != nil {
		t.Fatal(err)
	}
	texts, _, ids := sentTexts(h.f.callsOf("sendmessage"))
	if len(texts) != 4 || texts[1][0] != 'y' || texts[2][0] != 'y' || texts[3][0] != 'z' || ids[1] != ids[2] {
		t.Errorf("sent %d messages, first letters %q, ids %q", len(texts), firstLetters(texts), ids)
	}
	// A later identical reply is a new delivery.
	send(t, h, "ok")
	send(t, h, "ok")
	if _, _, ids := sentTexts(h.f.callsOf("sendmessage")); len(ids) != 6 || ids[4] == ids[5] {
		t.Errorf("client ids = %q", ids)
	}
}

func firstLetters(ss []string) string {
	var b strings.Builder
	for _, s := range ss {
		b.WriteByte(s[0])
	}
	return b.String()
}

func TestSendErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"business error", 200, `{"ret":-1,"errmsg":"nope"}`, true},
		{"errcode only", 200, `{"ret":0,"errcode":-100}`, true},
		{"bad request", 400, `{}`, true},
		{"rate limited", 429, `{}`, false},
		{"server error", 500, `{}`, false},
		{"invalid JSON", 200, `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.start(t)
			h.greet(t, "m1", "ctx-1")
			h.f.setReply(func(c apiCall, n int) (int, string, bool) {
				return tc.status, tc.body, c.Endpoint == "sendmessage"
			})
			err := send(t, h, "hi")
			if err == nil || channels.IsPermanent(err) != tc.permanent {
				t.Errorf("Send = %v, permanent %t, want permanent %t", err, channels.IsPermanent(err), tc.permanent)
			}
		})
	}
}

func TestStaleTokenWaitsForNewLogin(t *testing.T) {
	h := newHarness(t, nil)
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		if c.Header.Get("Authorization") == "Bearer "+testToken && c.Endpoint == "getupdates" {
			return 200, `{"ret":-14,"errmsg":"session timeout"}`, true
		}
		return 0, "", false
	})
	h.start(t)
	h.f.waitFor("getupdates", 1)
	waitUntil(t, func() bool { _, _, auth := h.ch.session(); return auth })
	err := send(t, h, "hi")
	if !errors.Is(err, errLoginExpired) || !channels.IsPermanent(err) {
		t.Errorf("Send while logged out = %v", err)
	}
	if !strings.Contains(h.logs.String(), "nanobot channels login weixin --force") {
		t.Errorf("no hint to log in again:\n%s", h.logs)
	}

	// A new login is picked up without a restart.
	h.saveLogin(t, "new-token-SECRET")
	waitUntil(t, func() bool {
		for _, c := range h.f.callsOf("getupdates") {
			if c.Header.Get("Authorization") == "Bearer new-token-SECRET" {
				return true
			}
		}
		return false
	})
	h.f.push([]map[string]any{text("m1", alice, "ctx", "back")}, "c1")
	if in := h.inbound(t); in.Content != "back" {
		t.Errorf("inbound = %+v", in)
	}
	// The running channel never overwrote the new login with the stale one.
	if st, _ := h.state.load(); st.Token != "new-token-SECRET" {
		t.Error("the saved token was overwritten")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPollBackoff(t *testing.T) {
	h := newHarness(t, nil)
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 500, `oops`, c.Endpoint == "getupdates" && n <= 4
	})
	h.start(t)
	var got []time.Duration
	for range 4 {
		select {
		case d := <-h.sleeps:
			got = append(got, d)
		case <-time.After(5 * time.Second):
			t.Fatalf("waits so far: %v", got)
		}
	}
	want := []time.Duration{retryDelay, retryDelay, backoffDelay, retryDelay}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("waits = %v, want %v", got, want)
			break
		}
	}
	// Polling recovers afterwards.
	h.f.push([]map[string]any{text("m1", alice, "ctx", "up")}, "c1")
	if in := h.inbound(t); in.Content != "up" {
		t.Errorf("inbound = %+v", in)
	}
}

func TestPollTimeoutRetriesAtOnce(t *testing.T) {
	h := newHarness(t, nil)
	// The client gives up on getupdates after the poll timeout plus
	// pollSlack; pretend it already did.
	h.ch.pollTimeout = -pollSlack + 20*time.Millisecond
	h.start(t)
	h.f.waitFor("getupdates", 3)
	select {
	case d := <-h.sleeps:
		t.Errorf("waited %v after a long-poll timeout", d)
	default:
	}
}

func TestStopIsPrompt(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.ch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.f.waitFor("getupdates", 1) // a long poll is in flight
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := h.ch.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop took %v", d)
	}
	h.f.waitFor("notifystop", 1)
	if err := h.ch.Stop(ctx); err != nil {
		t.Errorf("second Stop = %v", err)
	}
}

func TestStartWithoutLogin(t *testing.T) {
	h := newHarness(t, nil)
	os.Remove(h.state.path)
	err := h.ch.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nanobot channels login weixin") {
		t.Errorf("Start = %v", err)
	}
}

func TestConfiguredTokenAndEnv(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Token = "config-token-SECRET" })
	h.start(t)
	if got := h.f.waitFor("getupdates", 1)[0].Header.Get("Authorization"); got != "Bearer config-token-SECRET" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestTokenIsRedactedFromErrors(t *testing.T) {
	f := newFakeILink(t)
	f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 200, `{"ret":1,"errmsg":"bad token ` + testToken + `"}`, true
	})
	cl := newClient(f.srv.Client(), "", "test")
	err := cl.do(context.Background(), request{method: "POST", base: f.srv.URL, endpoint: "ilink/bot/sendmessage",
		body: struct{}{}, token: testToken}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), redacted) {
		t.Errorf("error = %v", err)
	}
	// A transport error, with the token's request canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = cl.do(ctx, request{method: "POST", base: f.srv.URL, endpoint: "x", body: struct{}{}, token: testToken}, nil)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("canceled error = %v", err)
	}
}

func TestRouteTagHeader(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RouteTag = "1234" })
	h.start(t)
	if got := h.f.waitFor("getupdates", 1)[0].Header.Get("SKRouteTag"); got != "1234" {
		t.Errorf("SKRouteTag = %q", got)
	}
}
