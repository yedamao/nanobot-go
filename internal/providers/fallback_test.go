package providers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeProvider returns scripted errors (nil means success) and records the
// models it was asked for.
type fakeProvider struct {
	name   string
	errs   []error
	models []string
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	n := len(f.models)
	f.models = append(f.models, req.Model)
	var err error
	if len(f.errs) > 0 {
		err = f.errs[min(n, len(f.errs)-1)]
	}
	if err != nil {
		return nil, err
	}
	return &ChatResponse{Content: f.name, FinishReason: FinishStop}, nil
}

func apiErr(kind Kind) error { return &APIError{Provider: "fake", Kind: kind, Message: string(kind)} }

func TestFallbackFailoverKinds(t *testing.T) {
	tests := []struct {
		kind     Kind
		failover bool
	}{
		{KindTimeout, true},
		{KindConnection, true},
		{KindServerError, true},
		{KindRateLimit, true},
		{KindAuth, true},
		{KindBadRequest, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			primary := &fakeProvider{name: "primary", errs: []error{apiErr(tt.kind)}}
			fb := &fakeProvider{name: "fb"}
			p := NewFallback(primary, []Fallback{{Provider: fb, Model: "fb-model"}})
			resp, err := p.Chat(context.Background(), ChatRequest{Model: "main"})
			if tt.failover {
				if err != nil || resp.Content != "fb" {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
				if len(fb.models) != 1 || fb.models[0] != "fb-model" {
					t.Errorf("fallback models = %v", fb.models)
				}
			} else {
				var e *APIError
				if !errors.As(err, &e) || e.Kind != tt.kind {
					t.Fatalf("err = %v", err)
				}
				if len(fb.models) != 0 {
					t.Error("fallback called for non-failover error")
				}
			}
		})
	}
}

func TestFallbackOrderAndExhaustion(t *testing.T) {
	primary := &fakeProvider{name: "primary", errs: []error{apiErr(KindServerError)}}
	fb1 := &fakeProvider{name: "fb1", errs: []error{apiErr(KindRateLimit)}}
	fb2 := &fakeProvider{name: "fb2"}
	fb3 := &fakeProvider{name: "fb3"}
	p := NewFallback(primary, []Fallback{{Provider: fb1}, {Provider: fb2}, {Provider: fb3}})
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "main"})
	if err != nil || resp.Content != "fb2" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	if fb1.models[0] != "main" {
		t.Errorf("fallback without model override got %q", fb1.models[0])
	}
	if len(fb3.models) != 0 {
		t.Error("fb3 called after fb2 succeeded")
	}

	// A non-failover error from a fallback stops the chain.
	fb2.errs = []error{apiErr(KindBadRequest)}
	_, err = p.Chat(context.Background(), ChatRequest{})
	var e *APIError
	if !errors.As(err, &e) || e.Kind != KindBadRequest || len(fb3.models) != 0 {
		t.Fatalf("err = %v, fb3 calls = %d", err, len(fb3.models))
	}

	// All fail: the last error is returned, wrapped.
	fb2.errs = []error{apiErr(KindTimeout)}
	fb3.errs = []error{apiErr(KindConnection)}
	_, err = p.Chat(context.Background(), ChatRequest{})
	if !errors.As(err, &e) || e.Kind != KindConnection {
		t.Fatalf("err = %v", err)
	}
}

func TestFallbackCancelDoesNotFailOver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	primary := &fakeProvider{name: "primary", errs: []error{context.Canceled}}
	fb := &fakeProvider{name: "fb"}
	p := NewFallback(primary, []Fallback{{Provider: fb}})
	if _, err := p.Chat(ctx, ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(fb.models) != 0 {
		t.Error("fallback called after cancellation")
	}
}

func TestFallbackCircuitBreaker(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	primary := &fakeProvider{name: "primary", errs: []error{apiErr(KindServerError)}}
	fb := &fakeProvider{name: "fb"}
	p := NewFallback(primary, []Fallback{{Provider: fb}}, WithClock(clock))

	call := func() string {
		t.Helper()
		resp, err := p.Chat(context.Background(), ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Content
	}

	for range 3 {
		call()
	}
	if len(primary.models) != 3 {
		t.Fatalf("primary calls = %d, want 3", len(primary.models))
	}
	// Circuit open: primary skipped.
	call()
	now = now.Add(59 * time.Second)
	call()
	if len(primary.models) != 3 {
		t.Fatalf("primary called while circuit open (%d calls)", len(primary.models))
	}
	// Half-open probe fails: re-trips immediately.
	now = now.Add(time.Second)
	call()
	if len(primary.models) != 4 {
		t.Fatalf("primary calls = %d, want probe", len(primary.models))
	}
	call()
	if len(primary.models) != 4 {
		t.Fatal("primary called right after failed probe")
	}
	// Probe succeeds after cooldown: circuit closes.
	now = now.Add(60 * time.Second)
	primary.errs = []error{nil}
	if got := call(); got != "primary" {
		t.Fatalf("got %q, want primary", got)
	}
	if got := call(); got != "primary" {
		t.Fatalf("got %q, want primary after close", got)
	}
}

// blockingProvider signals each call, then blocks until release is closed.
type blockingProvider struct {
	calls   chan struct{}
	release chan struct{}
}

func (b *blockingProvider) Name() string { return "primary" }
func (b *blockingProvider) Chat(ctx context.Context, _ ChatRequest) (*ChatResponse, error) {
	b.calls <- struct{}{}
	select {
	case <-b.release:
		return &ChatResponse{Content: "primary", FinishReason: FinishStop}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestFallbackHalfOpenAllowsOneProbe(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	primary := &blockingProvider{calls: make(chan struct{}, 10), release: make(chan struct{})}
	fb := &fakeProvider{name: "fb"}
	p := NewFallback(primary, []Fallback{{Provider: fb}}, WithCircuitBreaker(1, time.Minute), WithClock(clock))

	// Trip the circuit with a failure-worthy error.
	p.recordFailure()
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()

	probe := make(chan string, 1)
	go func() {
		resp, err := p.Chat(context.Background(), ChatRequest{})
		if err != nil {
			probe <- err.Error()
			return
		}
		probe <- resp.Content
	}()
	<-primary.calls // the probe is in flight

	// While it runs, other callers go straight to the fallback.
	for range 3 {
		resp, err := p.Chat(context.Background(), ChatRequest{})
		if err != nil || resp.Content != "fb" {
			t.Fatalf("resp = %+v, err = %v; want fallback during probe", resp, err)
		}
	}
	close(primary.release)
	if got := <-probe; got != "primary" {
		t.Fatalf("probe = %q", got)
	}
	if n := len(primary.calls); n != 0 {
		t.Errorf("primary called %d extra times during the probe", n)
	}
	// The successful probe closed the circuit.
	if resp, err := p.Chat(context.Background(), ChatRequest{}); err != nil || resp.Content != "primary" {
		t.Fatalf("after probe: resp = %+v, err = %v", resp, err)
	}
}

func TestFallbackCircuitOpenNoFallbacks(t *testing.T) {
	primary := &fakeProvider{name: "primary", errs: []error{apiErr(KindServerError)}}
	p := NewFallback(primary, nil, WithCircuitBreaker(1, time.Minute))
	if _, err := p.Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("want error")
	}
	_, err := p.Chat(context.Background(), ChatRequest{})
	var e *APIError
	if !errors.As(err, &e) || e.Kind != KindServerError || len(primary.models) != 1 {
		t.Fatalf("err = %v, primary calls = %d", err, len(primary.models))
	}
}
