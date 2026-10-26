// Package bus decouples chat channels from the agent loop.
//
// Channels publish InboundMessage values and consume OutboundMessage values;
// the agent loop does the reverse. It mirrors nanobot/bus.
package bus

import (
	"strings"
	"time"
)

// InboundMessage is a message received from a chat channel.
type InboundMessage struct {
	Channel   string    // telegram, discord, slack, cli, ...
	SenderID  string    // user identifier
	ChatID    string    // chat/channel identifier
	Content   string    // message text
	Timestamp time.Time // when the message was received
	Media     []string  // media paths or URLs
	Metadata  map[string]any

	// SessionKeyOverride, when set, replaces the default channel:chat_id
	// session key (for example for thread-scoped sessions).
	SessionKeyOverride string
}

// SessionKey identifies the conversation this message belongs to.
func (m InboundMessage) SessionKey() string {
	if m.SessionKeyOverride != "" {
		return m.SessionKeyOverride
	}
	return m.Channel + ":" + m.ChatID
}

// OutboundMessage is a message to send to a chat channel.
type OutboundMessage struct {
	Channel string
	ChatID  string
	Content string
	ReplyTo string
	Media   []string
	// Buttons are rows of choice labels (the message tool's buttons).
	// Telegram can show them as an inline keyboard; other channels show
	// ButtonsText under the content.
	Buttons  [][]string
	Metadata map[string]any // channel routing context (message ids, threads, ...)
}

// ButtonsText renders button rows as text, one row per line ("[Yes]
// [No]"), skipping empty rows, like nanobot's _buttons_as_text: the
// choices stay visible where no keyboard can be shown.
func ButtonsText(rows [][]string) string {
	var lines []string
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		labels := make([]string, len(row))
		for i, l := range row {
			labels[i] = "[" + l + "]"
		}
		lines = append(lines, strings.Join(labels, " "))
	}
	return strings.Join(lines, "\n")
}

// WithButtonsText returns content with ButtonsText(rows) appended after
// a blank line, or content unchanged without buttons.
func WithButtonsText(content string, rows [][]string) string {
	t := ButtonsText(rows)
	switch {
	case t == "":
		return content
	case content == "":
		return t
	}
	return content + "\n\n" + t
}

// MetaProgress is the Metadata key that marks an OutboundMessage as a
// progress update (a short status such as the tool being run) rather than
// a chat message. Channels that cannot show a transient status must
// ignore such messages; see IsProgress.
const MetaProgress = "_progress"

// IsProgress reports whether msg is a progress update rather than a
// reply. Channels without a status display drop these.
func IsProgress(msg OutboundMessage) bool {
	v, _ := msg.Metadata[MetaProgress].(bool)
	return v
}

// MetaDelta is the Metadata key that marks an OutboundMessage as a piece
// of a reply being streamed: Content is the new text only, to append to
// what came before. The turn's final OutboundMessage still carries the
// whole reply and supersedes the deltas. Channels that cannot show a
// growing reply must ignore such messages; see IsDelta.
const MetaDelta = "_delta"

// IsDelta reports whether msg is a streamed piece of a reply. Channels
// that only send whole messages drop these.
func IsDelta(msg OutboundMessage) bool {
	v, _ := msg.Metadata[MetaDelta].(bool)
	return v
}

// MetaThinking is the Metadata key that marks an OutboundMessage as a
// piece of the model's thinking being streamed: Content is the new text
// only, to append to what came before. Thinking is never part of the
// reply and is not stored in the session. Chat apps must not show it;
// see IsThinking.
const MetaThinking = "_thinking"

// IsThinking reports whether msg is a streamed piece of thinking.
// Channels without a place to show it apart from the reply drop these.
func IsThinking(msg OutboundMessage) bool {
	v, _ := msg.Metadata[MetaThinking].(bool)
	return v
}

// MetaThinkingEnd is the Metadata key that marks an OutboundMessage as the
// end of a run of thinking pieces (see MetaThinking), sent when the answer
// starts or the response ends. Content is empty. See IsThinkingEnd.
const MetaThinkingEnd = "_thinking_end"

// IsThinkingEnd reports whether msg ends a run of thinking pieces.
func IsThinkingEnd(msg OutboundMessage) bool {
	v, _ := msg.Metadata[MetaThinkingEnd].(bool)
	return v
}

// MetaSuperseded is the Metadata key that marks a turn's final reply to
// a chat the message tool already sent to during the turn, like
// nanobot's suppressed response. Chat apps drop it, so the chat does not
// get a second message; a channel that showed the reply's deltas (the
// WebUI) delivers it as usual to settle the streamed text. See
// IsSuperseded.
const MetaSuperseded = "_superseded"

// IsSuperseded reports whether msg is a final reply superseded by the
// message tool's own send.
func IsSuperseded(msg OutboundMessage) bool {
	v, _ := msg.Metadata[MetaSuperseded].(bool)
	return v
}

// IsStreamed reports whether msg is a transient streamed update rather
// than a reply: a progress update, a delta, thinking or its end, or a
// superseded reply that only closes the stream. Channels that only send
// whole messages drop these.
func IsStreamed(msg OutboundMessage) bool {
	return IsProgress(msg) || IsDelta(msg) || IsThinking(msg) || IsThinkingEnd(msg) || IsSuperseded(msg)
}
