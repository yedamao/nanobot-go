package telegram

import (
	"strings"
	"testing"
)

// TestReplyContextFallbacks covers the sender fallbacks and the length cap
// of nanobot's _extract_reply_context.
func TestReplyContextFallbacks(t *testing.T) {
	c := &Channel{botID: botID}
	long := strings.Repeat("é", replyContextMax+5)
	tests := []struct {
		name string
		r    *message
		want string
	}{
		{"first name", &message{From: &user{ID: 7, FirstName: "Dave"}, Text: "hi"}, "[Reply to Dave: hi]"},
		{"anonymous", &message{Text: "hi"}, "[Reply to: hi]"},
		{"caption", &message{From: &user{ID: 7}, Caption: "pic"}, "[Reply to: pic]"},
		{"empty", &message{From: alice}, ""},
		{"truncated", &message{Text: long}, "[Reply to: " + strings.Repeat("é", replyContextMax) + "...]"},
	}
	for _, tt := range tests {
		if got := c.replyContext(tt.r); got != tt.want {
			t.Errorf("%s: replyContext = %.40q, want %.40q", tt.name, got, tt.want)
		}
	}
}

func TestReplyMetadata(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	u := dm(1, alice, "and?")
	u.Message.ReplyToMessage = &message{MessageID: 5, From: &user{ID: botID, IsBot: true}, Text: "earlier"}
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to bot: earlier]\nand?" {
		t.Errorf("content = %q", in.Content)
	}
	if in.Metadata["reply_to_message_id"] != int64(5) || in.Metadata["message_id"] != int64(10) {
		t.Errorf("metadata = %v", in.Metadata)
	}
}
