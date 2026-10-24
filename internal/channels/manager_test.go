package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
)

// recorder collects events from several fakes in order.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// fakeChannel records sends. send, when set, decides each attempt's
// result.
type fakeChannel struct {
	name     string
	startErr error
	send     func(ctx context.Context, attempt int, msg bus.OutboundMessage) error
	rec      *recorder

	mu       sync.Mutex
	attempts int
	sent     []bus.OutboundMessage
	gotSent  chan bus.OutboundMessage
}

func newFake(name string, rec *recorder) *fakeChannel {
	return &fakeChannel{name: name, rec: rec, gotSent: make(chan bus.OutboundMessage, 64)}
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Start(context.Context) error {
	f.rec.add("start " + f.name)
	return f.startErr
}
func (f *fakeChannel) Stop(context.Context) error {
	f.rec.add("stop " + f.name)
	return nil
}
func (f *fakeChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	f.mu.Lock()
	f.attempts++
	attempt := f.attempts
	f.mu.Unlock()
	if f.send != nil {
		if err := f.send(ctx, attempt, msg); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.mu.Unlock()
	f.rec.add("send " + f.name + " " + msg.Content)
	f.gotSent <- msg
	return nil
}

func (f *fakeChannel) contents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		out = append(out, m.Content)
	}
	return out
}

func (f *fakeChannel) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func waitSent(t *testing.T, f *fakeChannel) bus.OutboundMessage {
	t.Helper()
	select {
	case m := <-f.gotSent:
		return m
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no message delivered", f.name)
		return bus.OutboundMessage{}
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use, for log output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// sleeps records the requested delays without waiting.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) list() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d)
}

func startManager(t *testing.T, b *bus.MessageBus, chans []Channel, opts ...Option) *Manager {
	t.Helper()
	m, err := NewManager(b, chans, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m
}

func publish(t *testing.T, b *bus.MessageBus, channel, content string) {
	t.Helper()
	if err := b.PublishOutbound(context.Background(), bus.OutboundMessage{Channel: channel, ChatID: "c1", Content: content}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRoutesByName(t *testing.T) {
	b := bus.NewMessageBus(16)
	a, c := newFake("alpha", nil), newFake("beta", nil)
	startManager(t, b, []Channel{a, c})
	for _, m := range []struct{ ch, content string }{{"alpha", "a1"}, {"beta", "b1"}, {"alpha", "a2"}, {"alpha", "a3"}} {
		publish(t, b, m.ch, m.content)
	}
	for range 3 {
		waitSent(t, a)
	}
	waitSent(t, c)
	if got := a.contents(); !slices.Equal(got, []string{"a1", "a2", "a3"}) {
		t.Errorf("alpha got %v", got)
	}
	if got := c.contents(); !slices.Equal(got, []string{"b1"}) {
		t.Errorf("beta got %v", got)
	}
}

func TestManagerDropsUnknownChannel(t *testing.T) {
	b := bus.NewMessageBus(16)
	a := newFake("alpha", nil)
	log, buf := testLogger()
	startManager(t, b, []Channel{a}, WithLogger(log))
	publish(t, b, "nowhere", "lost")
	publish(t, b, "alpha", "kept")
	waitSent(t, a)
	if got := a.contents(); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("alpha got %v", got)
	}
	if out := buf.String(); !strings.Contains(out, "unknown channel") || !strings.Contains(out, "channel=nowhere") {
		t.Errorf("log = %q, want a drop warning for nowhere", out)
	}
}

func TestManagerRetries(t *testing.T) {
	errFlaky := errors.New("flaky")
	tests := []struct {
		name         string
		failures     int // attempts that fail before success; -1 fails forever
		err          error
		wantAttempts int
		wantSleeps   []time.Duration
		wantSent     bool
		wantLog      string
	}{
		{"first try", 0, errFlaky, 1, nil, true, ""},
		{"two failures", 2, errFlaky, 3, []time.Duration{time.Second, 2 * time.Second}, true, "retrying"},
		{"always fails", -1, errFlaky, 4, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, false, "giving up"},
		{"permanent", -1, Permanent(errFlaky), 1, nil, false, "permanent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := bus.NewMessageBus(16)
			f := newFake("alpha", nil)
			f.send = func(_ context.Context, attempt int, _ bus.OutboundMessage) error {
				if tt.failures < 0 || attempt <= tt.failures {
					return tt.err
				}
				return nil
			}
			s := &sleeps{}
			log, buf := testLogger()
			m := startManager(t, b, []Channel{f}, WithSleep(s.sleep), WithLogger(log))
			publish(t, b, "alpha", "hi")
			if tt.wantSent {
				waitSent(t, f)
			}
			// Stop drains: once it returns, the delivery has finished.
			if err := m.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := f.attemptCount(); got != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", got, tt.wantAttempts)
			}
			if got := s.list(); !slices.Equal(got, tt.wantSleeps) {
				t.Errorf("sleeps = %v, want %v", got, tt.wantSleeps)
			}
			if got := len(f.contents()) == 1; got != tt.wantSent {
				t.Errorf("sent = %v, want %v", got, tt.wantSent)
			}
			if tt.wantLog != "" && !strings.Contains(buf.String(), tt.wantLog) {
				t.Errorf("log = %q, want %q", buf.String(), tt.wantLog)
			}
		})
	}
}

func TestManagerProgressIsBestEffort(t *testing.T) {
	b := bus.NewMessageBus(16)
	f := newFake("alpha", nil)
	f.send = func(_ context.Context, _ int, msg bus.OutboundMessage) error {
		if bus.IsProgress(msg) {
			return errors.New("flaky")
		}
		return nil
	}
	s := &sleeps{}
	log, buf := testLogger()
	startManager(t, b, []Channel{f}, WithSleep(s.sleep), WithLogger(log))
	progress := map[string]any{bus.MetaProgress: true}
	for _, msg := range []bus.OutboundMessage{
		{Channel: "nowhere", ChatID: "c1", Content: "lost", Metadata: progress},
		{Channel: "alpha", ChatID: "c1", Content: "status", Metadata: progress},
		{Channel: "alpha", ChatID: "c1", Content: "reply"},
	} {
		if err := b.PublishOutbound(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	waitSent(t, f)
	// The failed progress update was tried once, not retried.
	if got := f.attemptCount(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if got := s.list(); len(got) != 0 {
		t.Errorf("sleeps = %v, want none", got)
	}
	if got := f.contents(); !slices.Equal(got, []string{"reply"}) {
		t.Errorf("alpha got %v", got)
	}
	if out := buf.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("log = %q, want no warnings for progress", out)
	}
}

func TestManagerSlowChannelDoesNotBlockOthers(t *testing.T) {
	b := bus.NewMessageBus(16)
	release := make(chan struct{})
	slow, fast := newFake("slow", nil), newFake("fast", nil)
	slow.send = func(ctx context.Context, _ int, _ bus.OutboundMessage) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	startManager(t, b, []Channel{slow, fast})
	publish(t, b, "slow", "s1")
	publish(t, b, "fast", "f1")
	waitSent(t, fast)
	close(release)
	waitSent(t, slow)
}

func TestManagerSkipsChannelThatFailsToStart(t *testing.T) {
	rec := &recorder{}
	b := bus.NewMessageBus(16)
	bad, good := newFake("bad", rec), newFake("good", rec)
	bad.startErr = errors.New("no token")
	log, buf := testLogger()
	m := startManager(t, b, []Channel{bad, good}, WithLogger(log))
	if got := m.Running(); !slices.Equal(got, []string{"good"}) {
		t.Errorf("Running = %v", got)
	}
	if got := m.Names(); !slices.Equal(got, []string{"bad", "good"}) {
		t.Errorf("Names = %v", got)
	}
	publish(t, b, "bad", "x")
	publish(t, b, "good", "y")
	waitSent(t, good)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bad.attemptCount() != 0 {
		t.Error("sent to a channel that failed to start")
	}
	want := []string{"start bad", "start good", "send good y", "stop good"}
	if got := rec.list(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if !strings.Contains(buf.String(), "no token") {
		t.Errorf("log = %q, want the start error", buf.String())
	}
}

func TestManagerStopDrainsThenStopsInReverse(t *testing.T) {
	rec := &recorder{}
	b := bus.NewMessageBus(16)
	a, c := newFake("alpha", rec), newFake("beta", rec)
	m, err := NewManager(b, []Channel{a, c})
	if err != nil {
		t.Fatal(err)
	}
	// Queued before Start and still on the bus at Stop: delivered anyway.
	publish(t, b, "alpha", "1")
	publish(t, b, "beta", "2")
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := rec.list()
	if len(got) != 6 {
		t.Fatalf("events = %v", got)
	}
	if !slices.Equal(got[:2], []string{"start alpha", "start beta"}) ||
		!slices.Equal(got[4:], []string{"stop beta", "stop alpha"}) {
		t.Errorf("events = %v, want starts, sends, then stops in reverse", got)
	}
	sends := slices.Sorted(slices.Values(got[2:4]))
	if !slices.Equal(sends, []string{"send alpha 1", "send beta 2"}) {
		t.Errorf("sends = %v", sends)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Errorf("second Stop = %v", err)
	}
}

func TestManagerStopDeadlineAbandonsRetries(t *testing.T) {
	b := bus.NewMessageBus(16)
	f := newFake("alpha", nil)
	f.send = func(context.Context, int, bus.OutboundMessage) error { return errors.New("down") }
	m := startManager(t, b, []Channel{f}) // the real sleep: 1s, 2s, 4s
	publish(t, b, "alpha", "hi")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Errorf("Stop took %v; the retry wait was not abandoned", d)
	}
}

func TestManagerStartTwice(t *testing.T) {
	m := startManager(t, bus.NewMessageBus(1), nil)
	if err := m.Start(context.Background()); err == nil {
		t.Error("second Start succeeded")
	}
}

func TestNewManagerValidates(t *testing.T) {
	b := bus.NewMessageBus(1)
	tests := []struct {
		name  string
		bus   *bus.MessageBus
		chans []Channel
	}{
		{"nil bus", nil, nil},
		{"empty name", b, []Channel{newFake("", nil)}},
		{"duplicate", b, []Channel{newFake("a", nil), newFake("a", nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewManager(tt.bus, tt.chans); err == nil {
				t.Error("NewManager succeeded")
			}
		})
	}
}

func TestRegistryBuild(t *testing.T) {
	type fakeCfg struct {
		Enabled bool   `json:"enabled"`
		Token   string `json:"token"`
	}
	r := NewRegistry()
	var gotToken string
	if err := r.Register("fake", func(raw json.RawMessage, deps Deps) (Channel, error) {
		var c fakeCfg
		if err := DecodeConfig(raw, &c); err != nil {
			return nil, err
		}
		if deps.Bus == nil || deps.Logger == nil {
			return nil, errors.New("missing deps")
		}
		gotToken = c.Token
		return newFake("fake", nil), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("misnamed", func(json.RawMessage, Deps) (Channel, error) { return newFake("other", nil), nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("fake", func(json.RawMessage, Deps) (Channel, error) { return nil, nil }); err == nil {
		t.Error("duplicate Register succeeded")
	}
	if err := r.Register("", func(json.RawMessage, Deps) (Channel, error) { return nil, nil }); err == nil {
		t.Error("empty-name Register succeeded")
	}
	if err := r.Register("nilf", nil); err == nil {
		t.Error("nil-factory Register succeeded")
	}
	deps := Deps{Bus: bus.NewMessageBus(1)}

	tests := []struct {
		name      string
		sections  map[string]string
		wantNames []string
		wantErr   string
	}{
		{"none", nil, nil, ""},
		{"enabled", map[string]string{"fake": `{"enabled": true, "token": "t"}`}, []string{"fake"}, ""},
		{"disabled", map[string]string{"fake": `{"enabled": false}`}, nil, ""},
		{"unknown but disabled", map[string]string{"nope": `{"token": "x"}`}, nil, ""},
		{"unknown enabled", map[string]string{"nope": `{"enabled": true}`}, nil, "unknown channel"},
		{"strict decode", map[string]string{"fake": `{"enabled": true, "tokn": "t"}`}, nil, "tokn"},
		{"name mismatch", map[string]string{"misnamed": `{"enabled": true}`}, nil, `named "other"`},
		{"bad json", map[string]string{"fake": `[1]`}, nil, "channels.fake"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sections := map[string]json.RawMessage{}
			for k, v := range tt.sections {
				sections[k] = json.RawMessage(v)
			}
			chans, err := r.Build(sections, deps)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Build err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, ch := range chans {
				names = append(names, ch.Name())
			}
			if !slices.Equal(names, tt.wantNames) {
				t.Errorf("names = %v, want %v", names, tt.wantNames)
			}
		})
	}
	if gotToken != "t" {
		t.Errorf("factory saw token %q", gotToken)
	}
	if got := r.Names(); !slices.Equal(got, []string{"fake", "misnamed"}) {
		t.Errorf("Names = %v", got)
	}
}

func TestPermanent(t *testing.T) {
	base := errors.New("blocked")
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) != nil")
	}
	err := Permanent(base)
	if !IsPermanent(err) || !errors.Is(err, base) || err.Error() != "blocked" {
		t.Errorf("Permanent(base) = %v", err)
	}
	if IsPermanent(base) {
		t.Error("plain error reported permanent")
	}
	if !IsPermanent(errors.Join(errors.New("x"), err)) {
		t.Error("wrapped permanent error not detected")
	}
}
