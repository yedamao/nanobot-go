package bus

import "context"

// DefaultBufferSize is the queue capacity used when NewMessageBus gets a
// non-positive size.
const DefaultBufferSize = 256

// MessageBus carries messages between channels and the agent loop through
// two buffered queues. It is safe for concurrent use.
type MessageBus struct {
	inbound  chan InboundMessage
	outbound chan OutboundMessage
}

// NewMessageBus returns a bus whose queues each hold up to size messages.
func NewMessageBus(size int) *MessageBus {
	if size <= 0 {
		size = DefaultBufferSize
	}
	return &MessageBus{
		inbound:  make(chan InboundMessage, size),
		outbound: make(chan OutboundMessage, size),
	}
}

// PublishInbound queues a message for the agent. It blocks while the queue
// is full and returns ctx.Err() if ctx is done first.
func (b *MessageBus) PublishInbound(ctx context.Context, msg InboundMessage) error {
	return send(ctx, b.inbound, msg)
}

// ConsumeInbound waits for the next message for the agent.
func (b *MessageBus) ConsumeInbound(ctx context.Context) (InboundMessage, error) {
	return recv(ctx, b.inbound)
}

// PublishOutbound queues a message for delivery to a channel.
func (b *MessageBus) PublishOutbound(ctx context.Context, msg OutboundMessage) error {
	return send(ctx, b.outbound, msg)
}

// ConsumeOutbound waits for the next message to deliver.
func (b *MessageBus) ConsumeOutbound(ctx context.Context) (OutboundMessage, error) {
	return recv(ctx, b.outbound)
}

// InboundLen reports how many inbound messages are queued.
func (b *MessageBus) InboundLen() int { return len(b.inbound) }

// OutboundLen reports how many outbound messages are queued.
func (b *MessageBus) OutboundLen() int { return len(b.outbound) }

func send[T any](ctx context.Context, ch chan<- T, v T) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case ch <- v:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func recv[T any](ctx context.Context, ch <-chan T) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	select {
	case v := <-ch:
		return v, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}
