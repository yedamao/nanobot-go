package bus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestIsProgress(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		want bool
	}{
		{"nil metadata", nil, false},
		{"unmarked", map[string]any{"message_id": 7}, false},
		{"marked", map[string]any{MetaProgress: true}, true},
		{"false", map[string]any{MetaProgress: false}, false},
		{"not a bool", map[string]any{MetaProgress: "yes"}, false},
	}
	for _, tt := range tests {
		if got := IsProgress(OutboundMessage{Metadata: tt.meta}); got != tt.want {
			t.Errorf("%s: IsProgress = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsDelta(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		want bool
	}{
		{"nil metadata", nil, false},
		{"progress", map[string]any{MetaProgress: true}, false},
		{"marked", map[string]any{MetaDelta: true}, true},
		{"false", map[string]any{MetaDelta: false}, false},
		{"not a bool", map[string]any{MetaDelta: 1}, false},
	}
	for _, tt := range tests {
		if got := IsDelta(OutboundMessage{Metadata: tt.meta}); got != tt.want {
			t.Errorf("%s: IsDelta = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsThinkingAndIsStreamed(t *testing.T) {
	tests := []struct {
		name                    string
		meta                    map[string]any
		thinking, end, streamed bool
	}{
		{"nil metadata", nil, false, false, false},
		{"progress", map[string]any{MetaProgress: true}, false, false, true},
		{"delta", map[string]any{MetaDelta: true}, false, false, true},
		{"thinking", map[string]any{MetaThinking: true}, true, false, true},
		{"thinking end", map[string]any{MetaThinkingEnd: true}, false, true, true},
		{"not a bool", map[string]any{MetaThinking: "yes"}, false, false, false},
		{"superseded", map[string]any{MetaSuperseded: true}, false, false, true},
		{"superseded false", map[string]any{MetaSuperseded: false}, false, false, false},
	}
	for _, tt := range tests {
		msg := OutboundMessage{Metadata: tt.meta}
		if got := IsThinking(msg); got != tt.thinking {
			t.Errorf("%s: IsThinking = %v", tt.name, got)
		}
		if got := IsThinkingEnd(msg); got != tt.end {
			t.Errorf("%s: IsThinkingEnd = %v", tt.name, got)
		}
		if got := IsStreamed(msg); got != tt.streamed {
			t.Errorf("%s: IsStreamed = %v", tt.name, got)
		}
	}
}

func TestSessionKey(t *testing.T) {
	tests := []struct {
		name string
		msg  InboundMessage
		want string
	}{
		{"default", InboundMessage{Channel: "telegram", ChatID: "42"}, "telegram:42"},
		{"override", InboundMessage{Channel: "slack", ChatID: "C1", SessionKeyOverride: "slack:C1:thread9"}, "slack:C1:thread9"},
		{"empty chat", InboundMessage{Channel: "cli"}, "cli:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.msg.SessionKey(); got != tt.want {
				t.Errorf("SessionKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInboundRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := NewMessageBus(4)
	in := InboundMessage{Channel: "cli", ChatID: "direct", Content: "hi", Media: []string{"a.png"}}
	if err := b.PublishInbound(ctx, in); err != nil {
		t.Fatal(err)
	}
	if b.InboundLen() != 1 {
		t.Fatalf("InboundLen = %d, want 1", b.InboundLen())
	}
	got, err := b.ConsumeInbound(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "hi" || got.SessionKey() != "cli:direct" || len(got.Media) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestOutboundOrder(t *testing.T) {
	ctx := context.Background()
	b := NewMessageBus(0)
	for _, c := range []string{"a", "b", "c"} {
		if err := b.PublishOutbound(ctx, OutboundMessage{Channel: "cli", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"a", "b", "c"} {
		got, err := b.ConsumeOutbound(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != want {
			t.Errorf("got %q, want %q", got.Content, want)
		}
	}
	if b.OutboundLen() != 0 {
		t.Errorf("OutboundLen = %d, want 0", b.OutboundLen())
	}
}

func TestConsumeHonoursContext(t *testing.T) {
	b := NewMessageBus(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.ConsumeInbound(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ConsumeInbound err = %v, want DeadlineExceeded", err)
	}
	if _, err := b.ConsumeOutbound(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ConsumeOutbound err = %v, want DeadlineExceeded", err)
	}
}

func TestPublishBlocksWhenFull(t *testing.T) {
	b := NewMessageBus(1)
	if err := b.PublishInbound(context.Background(), InboundMessage{Content: "1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.PublishInbound(ctx, InboundMessage{Content: "2"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("PublishInbound err = %v, want DeadlineExceeded", err)
	}
}

func TestCancelledContextDoesNotPublish(t *testing.T) {
	b := NewMessageBus(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.PublishOutbound(ctx, OutboundMessage{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want Canceled", err)
	}
	if b.OutboundLen() != 0 {
		t.Errorf("message was queued despite cancelled context")
	}
}

func TestConcurrentProducers(t *testing.T) {
	ctx := context.Background()
	b := NewMessageBus(2)
	const producers, each = 8, 50
	var wg sync.WaitGroup
	for range producers {
		wg.Go(func() {
			for range each {
				if err := b.PublishInbound(ctx, InboundMessage{Content: "x"}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range producers * each {
		if _, err := b.ConsumeInbound(ctx); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
