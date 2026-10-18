package providers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Circuit breaker defaults, matching nanobot's FallbackProvider.
const (
	DefaultFailureThreshold = 3
	DefaultCooldown         = 60 * time.Second
)

// Fallback is a provider tried when the primary fails.
type Fallback struct {
	Provider Provider
	// Model, when set, replaces the request's model for this provider.
	Model string
}

// FallbackProvider tries a primary provider, then each fallback in order,
// but only for failover-worthy errors (see ShouldFailover). Each wrapped
// provider applies its own retry policy before failover happens.
//
// The primary sits behind a circuit breaker: after FailureThreshold
// consecutive failover-worthy failures it is skipped for Cooldown, then one
// probe request is allowed (half-open). A success closes the circuit.
type FallbackProvider struct {
	primary   Provider
	fallbacks []Fallback
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu        sync.Mutex
	failures  int
	trippedAt time.Time // zero when the circuit is closed
}

// FallbackOption configures a FallbackProvider.
type FallbackOption func(*FallbackProvider)

// WithCircuitBreaker sets the failure threshold and cooldown.
func WithCircuitBreaker(threshold int, cooldown time.Duration) FallbackOption {
	return func(f *FallbackProvider) { f.threshold, f.cooldown = threshold, cooldown }
}

// WithClock sets the time source used by the circuit breaker.
func WithClock(now func() time.Time) FallbackOption {
	return func(f *FallbackProvider) { f.now = now }
}

// NewFallback wraps primary with fallbacks.
func NewFallback(primary Provider, fallbacks []Fallback, opts ...FallbackOption) *FallbackProvider {
	f := &FallbackProvider{
		primary:   primary,
		fallbacks: fallbacks,
		threshold: DefaultFailureThreshold,
		cooldown:  DefaultCooldown,
		now:       time.Now,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Name implements Provider; it reports the primary's name.
func (f *FallbackProvider) Name() string { return f.primary.Name() }

// ShouldFailover reports whether err justifies trying another provider:
// timeouts, connection failures, server errors, rate limits, and auth
// failures (a fallback may use different credentials). Bad requests would
// fail the same way elsewhere, and cancellation must stop immediately.
func ShouldFailover(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Kind {
	case KindTimeout, KindConnection, KindServerError, KindRateLimit, KindAuth:
		return true
	}
	return false
}

// Chat implements Provider.
func (f *FallbackProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	var lastErr error
	if f.primaryAvailable() {
		resp, err := f.primary.Chat(ctx, req)
		if err == nil {
			f.recordSuccess()
			return resp, nil
		}
		if ctx.Err() != nil || !ShouldFailover(err) {
			return nil, err
		}
		f.recordFailure()
		lastErr = err
	}

	for _, fb := range f.fallbacks {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("fallback: %w", err)
		}
		r := req
		if fb.Model != "" {
			r.Model = fb.Model
		}
		resp, err := fb.Provider.Chat(ctx, r)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil || !ShouldFailover(err) {
			return nil, err
		}
	}

	if lastErr == nil {
		return nil, &APIError{Provider: f.primary.Name(), Kind: KindServerError, Message: "primary circuit open and no fallbacks available"}
	}
	if len(f.fallbacks) == 0 {
		return nil, lastErr
	}
	return nil, fmt.Errorf("all providers failed: %w", lastErr)
}

func (f *FallbackProvider) primaryAvailable() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trippedAt.IsZero() || f.now().Sub(f.trippedAt) >= f.cooldown
}

func (f *FallbackProvider) recordSuccess() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = 0
	f.trippedAt = time.Time{}
}

func (f *FallbackProvider) recordFailure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures++
	if f.failures >= f.threshold {
		// Also re-trips after a failed half-open probe.
		f.trippedAt = f.now()
	}
}
