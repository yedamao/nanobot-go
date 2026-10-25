package channels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
)

// senderQueueSize is how many outbound messages may wait for one channel
// before the dispatcher blocks (nanobot: _OUTBOUND_PENDING_LIMIT).
const senderQueueSize = 256

// retryDelays are the waits between delivery attempts: 1s, 2s and 4s, so
// a message is tried four times (nanobot: _SEND_RETRY_DELAYS).
func retryDelays() []time.Duration {
	return []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
}

// Option configures a Manager.
type Option func(*Manager)

// WithLogger sets the logger; the default discards output.
func WithLogger(l *slog.Logger) Option { return func(m *Manager) { m.log = l } }

// WithSleep replaces the wait between delivery attempts (for tests). It
// returns ctx.Err() if ctx ends first.
func WithSleep(f func(ctx context.Context, d time.Duration) error) Option {
	return func(m *Manager) { m.sleep = f }
}

// Manager runs a set of channels and the outbound dispatcher, which takes
// messages off the bus and hands each to the channel named by its Channel
// field. Every channel has its own sender goroutine and queue, so a
// channel that is slow or retrying does not hold up the others; messages
// to one channel are delivered in order.
type Manager struct {
	bus      *bus.MessageBus
	channels []Channel
	log      *slog.Logger
	sleep    func(context.Context, time.Duration) error
	delays   []time.Duration

	mu      sync.Mutex
	started bool
	stopped bool
	// running holds the channels that started, by name. It is written by
	// Start before the dispatcher starts and only read afterwards.
	running map[string]*sender
	order   []*sender // start order

	stopDispatch context.CancelFunc
	dispatchDone chan struct{}
	// sendCtx bounds deliveries; Stop cancels it when its deadline passes.
	sendCtx     context.Context
	cancelSends context.CancelFunc
	senders     sync.WaitGroup
}

type sender struct {
	ch    Channel
	queue chan bus.OutboundMessage
}

// NewManager returns a Manager for chans, which must have distinct,
// non-empty names.
func NewManager(b *bus.MessageBus, chans []Channel, opts ...Option) (*Manager, error) {
	if b == nil {
		return nil, errors.New("channels: manager: bus is required")
	}
	seen := map[string]bool{}
	for _, ch := range chans {
		name := ch.Name()
		switch {
		case name == "":
			return nil, errors.New("channels: manager: channel with an empty name")
		case seen[name]:
			return nil, fmt.Errorf("channels: manager: duplicate channel %q", name)
		}
		seen[name] = true
	}
	m := &Manager{
		bus:      b,
		channels: chans,
		log:      slog.New(slog.DiscardHandler),
		sleep:    sleepCtx,
		delays:   retryDelays(),
		running:  map[string]*sender{},
	}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Names returns the managed channel names in start order.
func (m *Manager) Names() []string {
	names := make([]string, len(m.channels))
	for i, ch := range m.channels {
		names[i] = ch.Name()
	}
	return names
}

// Running returns the names of the channels that started, in start order.
func (m *Manager) Running() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, len(m.order))
	for i, s := range m.order {
		names[i] = s.ch.Name()
	}
	return names
}

// Start starts the channels one by one, then the dispatcher. A channel
// that fails to start is logged and left out, like nanobot: the others
// keep running, and messages for it are dropped. Call Stop afterwards in
// every case.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return errors.New("channels: manager already started")
	}
	m.started = true
	if len(m.channels) == 0 {
		m.log.Warn("no channels enabled")
	}
	m.sendCtx, m.cancelSends = context.WithCancel(context.Background())
	for _, ch := range m.channels {
		name := ch.Name()
		if err := ch.Start(ctx); err != nil {
			m.log.Error("channel failed to start", "channel", name, "error", err)
			continue
		}
		m.log.Info("channel started", "channel", name)
		s := &sender{ch: ch, queue: make(chan bus.OutboundMessage, senderQueueSize)}
		m.running[name] = s
		m.order = append(m.order, s)
		m.senders.Go(func() { m.sendLoop(s) })
	}
	dctx, cancel := context.WithCancel(context.Background())
	m.stopDispatch = cancel
	m.dispatchDone = make(chan struct{})
	go m.dispatch(dctx)
	return nil
}

// Stop stops the dispatcher, delivers the outbound messages already on
// the bus or queued (retrying as usual), then stops the started channels
// in reverse start order. When ctx ends first, pending deliveries and
// retries are abandoned. Stop is safe to call more than once.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.started || m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	m.mu.Unlock()

	abandon := context.AfterFunc(ctx, m.cancelSends)
	defer abandon()
	m.stopDispatch()
	<-m.dispatchDone
	for _, s := range m.order {
		close(s.queue)
	}
	m.senders.Wait()
	m.cancelSends()

	var errs []error
	for i := len(m.order) - 1; i >= 0; i-- {
		ch := m.order[i].ch
		if err := ch.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop channel %s: %w", ch.Name(), err))
			continue
		}
		m.log.Info("channel stopped", "channel", ch.Name())
	}
	return errors.Join(errs...)
}

// dispatch routes outbound messages until ctx ends, then routes whatever
// is still on the bus. It is the bus's only outbound consumer, so the
// length check in the drain cannot race with another reader.
func (m *Manager) dispatch(ctx context.Context) {
	defer close(m.dispatchDone)
	for {
		msg, err := m.bus.ConsumeOutbound(ctx)
		if err != nil {
			break
		}
		m.route(msg)
	}
	for m.bus.OutboundLen() > 0 {
		msg, err := m.bus.ConsumeOutbound(context.Background())
		if err != nil {
			return
		}
		m.route(msg)
	}
}

// route queues msg for its channel, or drops it if no such channel is
// running. Progress updates and deltas for such a channel are dropped
// quietly.
func (m *Manager) route(msg bus.OutboundMessage) {
	s, ok := m.running[msg.Channel]
	if !ok {
		if transient(msg) {
			m.log.Debug("drop progress update for unknown channel", "channel", msg.Channel, "chat_id", msg.ChatID, "delta", bus.IsDelta(msg))
			return
		}
		m.log.Warn("drop outbound message for unknown channel", "channel", msg.Channel, "chat_id", msg.ChatID)
		return
	}
	select {
	case s.queue <- msg:
	case <-m.sendCtx.Done():
		m.log.Warn("drop outbound message: shutting down", "channel", msg.Channel, "chat_id", msg.ChatID)
	}
}

func (m *Manager) sendLoop(s *sender) {
	for msg := range s.queue {
		m.deliver(s.ch, msg)
	}
}

// deliver sends msg, retrying after each delay unless the error is
// Permanent or the Manager is giving up on pending deliveries. A progress
// update or delta is tried once: a retried status would be stale and would
// hold up the reply behind it, which carries a delta's text anyway.
func (m *Manager) deliver(ch Channel, msg bus.OutboundMessage) {
	ctx := m.sendCtx
	for attempt := 1; ; attempt++ {
		err := ch.Send(ctx, msg)
		if err == nil {
			return
		}
		log := m.log.With("channel", msg.Channel, "chat_id", msg.ChatID, "attempt", attempt, "error", err)
		switch {
		case transient(msg):
			log.Debug("drop progress update", "delta", bus.IsDelta(msg))
			return
		case ctx.Err() != nil:
			log.Warn("drop outbound message: shutting down")
			return
		case IsPermanent(err):
			log.Error("send failed with a permanent error")
			return
		case attempt > len(m.delays):
			log.Error("send failed; giving up")
			return
		}
		delay := m.delays[attempt-1]
		log.Warn("send failed; retrying", "delay", delay)
		if err := m.sleep(ctx, delay); err != nil {
			log.Warn("drop outbound message: shutting down")
			return
		}
	}
}

// transient reports whether msg is a progress update, a streamed delta or
// thinking (bus.IsStreamed), which are delivered best-effort.
func transient(msg bus.OutboundMessage) bool { return bus.IsStreamed(msg) }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
