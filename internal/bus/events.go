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
