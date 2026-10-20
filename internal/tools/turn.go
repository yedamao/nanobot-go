package tools

import (
	"context"
	"maps"
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
