package telegram

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yedamao/nanobot-go/internal/bus"
)

var choice = [][]string{{"Yes", "No"}, {}, {"Cancel"}}

// keyboardOf decodes the reply_markup of a recorded call: an object in a
// JSON body, a JSON string in a multipart one.
func keyboardOf(t *testing.T, c apiCall) [][]inlineKeyboardButton {
	t.Helper()
	raw, ok := c.Body["reply_markup"]
	if !ok {
		return nil
	}
	var data []byte
	if s, isString := raw.(string); isString {
		data = []byte(s)
	} else {
		data, _ = json.Marshal(raw)
	}
	var m inlineKeyboardMarkup
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("reply_markup %v: %v", raw, err)
	}
	return m.InlineKeyboard
}

func TestButtonsAsTextWithoutInlineKeyboards(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: "42", Content: "Proceed?", Buttons: choice})
	if err != nil {
		t.Fatal(err)
	}
	sent := h.f.callsOf("sendMessage")
	if len(sent) != 1 || sent[0].Body["text"] != "Proceed?\n\n[Yes] [No]\n[Cancel]" {
		t.Fatalf("sent %v", sent)
	}
	if _, ok := sent[0].Body["reply_markup"]; ok {
		t.Errorf("reply_markup with inlineKeyboards off: %v", sent[0].Body)
	}
	if polls := h.f.waitFor("getUpdates", 1); !reflect.DeepEqual(polls[0].Body["allowed_updates"], []any{"message"}) {
		t.Errorf("allowed_updates = %v", polls[0].Body["allowed_updates"])
	}
}

func TestInlineKeyboardOnLastChunk(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.InlineKeyboards = true })
	h.start(t)
	long := strings.Repeat("word ", 1000) + "Proceed?"
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: "42", Content: long, Buttons: choice}); err != nil {
		t.Fatal(err)
	}
	sent := h.f.callsOf("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	if kb := keyboardOf(t, sent[0]); kb != nil {
		t.Errorf("keyboard on the first chunk: %v", kb)
	}
	want := [][]inlineKeyboardButton{
		{{Text: "Yes", CallbackData: "Yes"}, {Text: "No", CallbackData: "No"}},
		{{Text: "Cancel", CallbackData: "Cancel"}},
	}
	if kb := keyboardOf(t, sent[1]); !reflect.DeepEqual(kb, want) {
		t.Errorf("keyboard = %v, want %v", kb, want)
	}
	for _, s := range sent {
		if strings.Contains(s.Body["text"].(string), "[Yes]") {
			t.Errorf("text fallback with a keyboard: %q", s.Body["text"])
		}
	}
	polls := h.f.waitFor("getUpdates", 1)
	if got := polls[0].Body["allowed_updates"]; !reflect.DeepEqual(got, []any{"message", "callback_query"}) {
		t.Errorf("allowed_updates = %v", got)
	}
}

func TestInlineKeyboardOnMedia(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.InlineKeyboards = true })
	writeWS(t, h, "out/a.png", pngFixture)
	writeWS(t, h, "out/b.txt", []byte("b"))
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: "42", Content: "Pick one",
		Media: []string{"out/a.png", "out/b.txt"}, Buttons: [][]string{{"A", "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	photo, doc := h.f.callsOf("sendPhoto"), h.f.callsOf("sendDocument")
	if len(photo) != 1 || len(doc) != 1 || len(h.f.callsOf("sendMessage")) != 0 {
		t.Fatalf("calls: %d photos, %d documents, %d messages", len(photo), len(doc), len(h.f.callsOf("sendMessage")))
	}
	if photo[0].Body["caption"] != "Pick one" || keyboardOf(t, photo[0]) != nil {
		t.Errorf("photo fields = %v", photo[0].Body)
	}
	if kb := keyboardOf(t, doc[0]); len(kb) != 1 || len(kb[0]) != 2 || kb[0][1].CallbackData != "B" {
		t.Errorf("document keyboard = %v", kb)
	}

	// Without text, the buttons are the text the keyboard sits on.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: "42", Buttons: [][]string{{"OK"}}}); err != nil {
		t.Fatal(err)
	}
	sent := h.f.callsOf("sendMessage")
	if len(sent) != 1 || sent[0].Body["text"] != "[OK]" || len(keyboardOf(t, sent[0])) != 1 {
		t.Errorf("sent %v", sent)
	}
}

func TestSafeCallbackData(t *testing.T) {
	if got := safeCallbackData("Yes"); got != "Yes" {
		t.Errorf("short label = %q", got)
	}
	ascii := strings.Repeat("a", 70)
	if got := safeCallbackData(ascii); got != ascii[:64] {
		t.Errorf("ascii = %q", got)
	}
	// 21 three-byte runes are 63 bytes; the 22nd would cross 64.
	han := strings.Repeat("好", 30)
	got := safeCallbackData(han)
	if len(got) != 63 || !utf8.ValidString(got) || got != strings.Repeat("好", 21) {
		t.Errorf("multibyte = %q (%d bytes)", got, len(got))
	}
	h := newHarness(t, func(c *Config) { c.InlineKeyboards = true })
	kb := h.ch.buildInlineKeyboard([][]string{{han}})
	if b := kb.InlineKeyboard[0][0]; b.Text != han || b.CallbackData != got {
		t.Errorf("button = %+v", b)
	}
	if h.ch.buildInlineKeyboard([][]string{{}}) != nil {
		t.Error("keyboard without labels")
	}
}

func tap(id int64, from *user, data string, msg *message) update {
	return update{UpdateID: id, CallbackQuery: &callbackQuery{ID: "cb" + string(rune('0'+id)), From: from, Message: msg, Data: data}}
}

func botMessage(chatID int64) *message {
	return &message{MessageID: 77, Date: 1700000000, From: &user{ID: botID, IsBot: true}, Chat: chat{ID: chatID, Type: "private"}}
}

func TestCallbackQueryPublishesLabel(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.InlineKeyboards = true })
	h.start(t)
	h.push(tap(1, alice, "Yes", botMessage(42)))
	in := h.inbound(t)
	if in.Channel != Name || in.ChatID != "42" || in.SenderID != "42|alice" || in.Content != "Yes" {
		t.Errorf("inbound = %+v", in)
	}
	want := map[string]any{"callback_query_id": "cb1", "button_label": "Yes", "user_id": int64(42),
		"username": "alice", "first_name": "Alice", "is_callback": true}
	if !reflect.DeepEqual(in.Metadata, want) {
		t.Errorf("metadata = %v", in.Metadata)
	}
	if in.SessionKey() != "telegram:42" {
		t.Errorf("session = %s", in.SessionKey())
	}
	answers := h.f.waitFor("answerCallbackQuery", 1)
	if answers[0].Body["callback_query_id"] != "cb1" {
		t.Errorf("answer = %v", answers[0].Body)
	}
	edits := h.f.waitFor("editMessageReplyMarkup", 1)
	if edits[0].Body["chat_id"] != float64(42) || edits[0].Body["message_id"] != float64(77) {
		t.Errorf("edit = %v", edits[0].Body)
	}
	if _, ok := edits[0].Body["reply_markup"]; ok {
		t.Errorf("edit keeps a keyboard: %v", edits[0].Body)
	}
	h.f.waitFor("sendChatAction", 1)

	// An inaccessible message (date 0) is answered but not edited; a
	// tap in a topic keeps the topic's session.
	topic := botMessage(-100500)
	topic.Date, topic.Chat.Type, topic.IsTopicMessage, topic.MessageThreadID = 0, "supergroup", true, 9
	h.push(tap(2, alice, "No", topic))
	in = h.inbound(t)
	if in.Content != "No" || in.ChatID != "-100500" || in.SessionKey() != "telegram:-100500:topic:9" ||
		in.Metadata["message_thread_id"] != int64(9) {
		t.Errorf("inbound = %+v", in)
	}
	h.f.waitFor("answerCallbackQuery", 2)
	h.noInbound(t)
	if n := len(h.f.callsOf("editMessageReplyMarkup")); n != 1 {
		t.Errorf("edited an inaccessible message (%d edits)", n)
	}
}

func TestCallbackQueryIgnored(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.InlineKeyboards = true })
	h.start(t)
	mallory := &user{ID: 666, FirstName: "Mallory", Username: "mallory"}
	h.f.waitFor("getUpdates", 1)
	h.push(tap(1, mallory, "Yes", botMessage(666)), tap(2, alice, "Yes", nil))
	h.noInbound(t)
	if n := len(h.f.callsOf("answerCallbackQuery")) + len(h.f.callsOf("editMessageReplyMarkup")); n != 0 {
		t.Errorf("%d answers/edits for ignored taps", n)
	}
	if !strings.Contains(h.logs.String(), "access denied for a button tap") {
		t.Errorf("denied tap not logged:\n%s", h.logs)
	}

	// With inlineKeyboards off, a stray callback query is dropped.
	h2 := newHarness(t, nil)
	h2.start(t)
	h2.f.waitFor("getUpdates", 1)
	h2.push(tap(1, alice, "Yes", botMessage(42)))
	h2.noInbound(t)
	if n := len(h2.f.callsOf("answerCallbackQuery")); n != 0 {
		t.Errorf("answered %d taps with inlineKeyboards off", n)
	}
}
