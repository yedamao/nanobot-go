package providers

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultRetryDelays are the waits between attempts, matching nanobot's
// (1, 2, 4) second schedule.
var defaultRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// DefaultMaxRetryAfter caps how long a Retry-After header can make us wait.
const DefaultMaxRetryAfter = 60 * time.Second

// maxResponseBytes caps response bodies read from providers.
const maxResponseBytes = 32 << 20

// RetryPolicy controls retries of transient failures.
type RetryPolicy struct {
	// Delays are the waits before each retry; its length is the number of
	// retries. Nil means 1s, 2s, 4s. A non-nil empty slice disables retries.
	Delays []time.Duration
	// MaxRetryAfter caps a server-provided Retry-After. Zero means
	// DefaultMaxRetryAfter.
	MaxRetryAfter time.Duration
	// Sleep waits for d or until ctx ends. Nil uses a real timer; tests
	// inject a fake.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (p RetryPolicy) delays() []time.Duration {
	if p.Delays == nil {
		return defaultRetryDelays
	}
	return p.Delays
}

func (p RetryPolicy) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// httpEndpoint posts JSON to a provider API with retries.
type httpEndpoint struct {
	provider string
	client   *http.Client
	retry    RetryPolicy
	// errorMessage extracts a human-readable message from an error body.
	errorMessage func(body []byte) string
}

// post sends body to url and returns the response body of a 2xx reply.
// Retryable failures are retried per the policy; the last error is returned.
func (e *httpEndpoint) post(ctx context.Context, url string, header http.Header, body []byte) ([]byte, error) {
	delays := e.retry.delays()
	for attempt := 0; ; attempt++ {
		out, retryAfter, err := e.once(ctx, url, header, body)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", e.provider, ctx.Err())
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt >= len(delays) {
			return nil, err
		}
		wait := delays[attempt]
		if retryAfter > 0 {
			wait = min(retryAfter, cmp.Or(e.retry.MaxRetryAfter, DefaultMaxRetryAfter))
		}
		if err := e.retry.sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("%s: %w", e.provider, err)
		}
	}
}

func (e *httpEndpoint) once(ctx context.Context, url string, header http.Header, body []byte) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%s: build request: %w", e.provider, err)
	}
	req.Header = header.Clone()
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, transportError(e.provider, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, 0, transportError(e.provider, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return out, 0, nil
	}
	msg := e.errorMessage(out)
	if msg == "" {
		msg = strings.TrimSpace(string(out))
		if len(msg) > 500 {
			msg = msg[:500]
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	apiErr := &APIError{Provider: e.provider, StatusCode: resp.StatusCode, Kind: kindForStatus(resp.StatusCode), Message: msg}
	return nil, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), apiErr
}

// parseRetryAfter parses a Retry-After header in seconds or HTTP-date form.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
