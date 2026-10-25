// Package bus decouples chat channels from the agent loop.
//
// Channels publish InboundMessage values and consume OutboundMessage values;
// the agent loop does the reverse. It mirrors nanobot/bus.
package bus

import "time"

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
	Channel  string
	ChatID   string
	Content  string
	ReplyTo  string
	Media    []string
	Metadata map[string]any // channel routing context (message ids, threads, ...)
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
