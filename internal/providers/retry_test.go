package providers

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// step is one scripted server reply; status 0 drops the connection.
type step struct {
	status     int
	retryAfter string
}

func scriptedServer(t *testing.T, steps []step, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		s := steps[min(n, len(steps)-1)]
		if s.status == 0 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if s.retryAfter != "" {
			w.Header().Set("Retry-After", s.retryAfter)
		}
		w.WriteHeader(s.status)
		if s.status == 200 {
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		} else {
			io.WriteString(w, `{"error":{"message":"failure"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type sleepRecorder struct{ waits []time.Duration }

func (s *sleepRecorder) sleep(_ context.Context, d time.Duration) error {
	s.waits = append(s.waits, d)
	return nil
}

func TestRetry(t *testing.T) {
	sec := time.Second
	tests := []struct {
		name      string
		steps     []step
		wantCalls int
		wantWaits []time.Duration
		wantKind  Kind // empty means success
	}{
		{"success", []step{{status: 200}}, 1, nil, ""},
		{"5xx then success", []step{{status: 503}, {status: 500}, {status: 200}}, 3, []time.Duration{sec, 2 * sec}, ""},
		{"408 409 then success", []step{{status: 408}, {status: 409}, {status: 200}}, 3, []time.Duration{sec, 2 * sec}, ""},
		{"exhausted", []step{{status: 502}}, 4, []time.Duration{sec, 2 * sec, 4 * sec}, KindServerError},
		{"retry-after seconds", []step{{status: 429, retryAfter: "7"}, {status: 200}}, 2, []time.Duration{7 * sec}, ""},
		{"retry-after capped", []step{{status: 429, retryAfter: "3600"}, {status: 200}}, 2, []time.Duration{DefaultMaxRetryAfter}, ""},
		{"bad request not retried", []step{{status: 400}}, 1, nil, KindBadRequest},
		{"auth not retried", []step{{status: 401}}, 1, nil, KindAuth},
		{"connection dropped", []step{{status: 0}, {status: 200}}, 2, []time.Duration{sec}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := scriptedServer(t, tt.steps, &calls)
			rec := &sleepRecorder{}
			p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: RetryPolicy{Sleep: rec.sleep}})
			resp, err := p.Chat(context.Background(), ChatRequest{})
			if tt.wantKind == "" {
				if err != nil || resp.Content != "ok" {
					t.Fatalf("resp = %+v, err = %v", resp, err)
				}
			} else {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Kind != tt.wantKind {
					t.Fatalf("err = %v, want kind %s", err, tt.wantKind)
				}
			}
			if got := int(calls.Load()); got != tt.wantCalls {
				t.Errorf("calls = %d, want %d", got, tt.wantCalls)
			}
			if !slices.Equal(rec.waits, tt.wantWaits) {
				t.Errorf("waits = %v, want %v", rec.waits, tt.wantWaits)
			}
		})
	}
}

func TestRetryConnectionErrorKind(t *testing.T) {
	var calls atomic.Int32
	srv := scriptedServer(t, []step{{status: 0}}, &calls)
	p := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, Retry: noRetry})
	_, err := p.Chat(context.Background(), ChatRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindConnection || apiErr.StatusCode != 0 {
		t.Fatalf("err = %v, want connection error", err)
	}
}

func TestRetryStopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	srv := scriptedServer(t, []step{{status: 503}}, &calls)
	ctx, cancel := context.WithCancel(context.Background())
	sleep := func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Retry: RetryPolicy{Sleep: sleep}})
	_, err := p.Chat(ctx, ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"-1", 0},
		{"garbage", 0},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{now.Add(-30 * time.Second).Format(http.TimeFormat), 0},
		// Non-finite or huge values must not overflow into a negative wait
		// (which would skip the backoff); the caller caps the result.
		{"NaN", 0},
		{"Inf", math.MaxInt64},
		{"1e300", math.MaxInt64},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// Without a client timeout a server that accepts the request but never
// answers hangs the turn forever and retries/failover never run.
func TestDefaultHTTPClientHasTimeout(t *testing.T) {
	for name, ep := range map[string]httpEndpoint{
		"anthropic": NewAnthropic(AnthropicConfig{}).ep,
		"openai":    NewOpenAI(OpenAIConfig{}).ep,
	} {
		if ep.client == nil || ep.client.Timeout != DefaultRequestTimeout {
			t.Errorf("%s: default client timeout = %v, want %v", name, ep.client.Timeout, DefaultRequestTimeout)
		}
	}
	custom := &http.Client{}
	if NewAnthropic(AnthropicConfig{HTTPClient: custom}).ep.client != custom {
		t.Error("explicit HTTPClient not used")
	}
}

func TestHangingServerTimesOutAsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 100 * time.Millisecond},
		Retry: RetryPolicy{Delays: []time.Duration{}}})
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindTimeout || !ShouldFailover(err) {
		t.Errorf("err = %v, want a failover-worthy timeout", err)
	}
}
