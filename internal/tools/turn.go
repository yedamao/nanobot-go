package tools

import (
	"context"
	"maps"
	"sync"
)

// Turn describes the chat turn a tool call belongs to, like nanobot's
// request context: where the message came from and which session it is
// recorded in. The agent loop attaches it to the context of every turn;
// tools that act on "this chat" (such as cron) read it with TurnFrom.
// Ephemeral runs (Dream) have none.
type Turn struct {
	Channel    string
	ChatID     string
	SessionKey string
	// Metadata is the inbound message's routing metadata (threads,
	// message ids, the cron trigger marker). Treat it as read-only.
	Metadata map[string]any
}

type turnKey struct{}

// WithTurn returns a context carrying t. t.Metadata is copied.
func WithTurn(ctx context.Context, t Turn) context.Context {
	t.Metadata = maps.Clone(t.Metadata)
	return context.WithValue(ctx, turnKey{}, t)
}

// TurnFrom returns the turn attached by WithTurn.
func TurnFrom(ctx context.Context) (Turn, bool) {
	t, ok := ctx.Value(turnKey{}).(Turn)
	return t, ok
}

// Deliveries records the chats a tool sent a message to during one turn,
// like nanobot's capture_message_deliveries: the loop skips its own reply
// to a chat the message tool already delivered to.
type Deliveries struct {
	mu      sync.Mutex
	targets map[[2]string]bool
}

type deliveriesKey struct{}

// WithDeliveries returns a context in which RecordDelivery records into
// the returned Deliveries.
func WithDeliveries(ctx context.Context) (context.Context, *Deliveries) {
	d := &Deliveries{targets: map[[2]string]bool{}}
	return context.WithValue(ctx, deliveriesKey{}, d), d
}

// RecordDelivery notes a message delivered to channel:chatID, if ctx
// came from WithDeliveries.
func RecordDelivery(ctx context.Context, channel, chatID string) {
	d, ok := ctx.Value(deliveriesKey{}).(*Deliveries)
	if !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets[[2]string{channel, chatID}] = true
}

// Delivered reports whether a message was delivered to channel:chatID.
func (d *Deliveries) Delivered(channel, chatID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.targets[[2]string{channel, chatID}]
}
