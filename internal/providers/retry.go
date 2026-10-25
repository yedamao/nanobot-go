package providers

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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

// DefaultRequestTimeout bounds one provider HTTP request when no client is
// configured, like the official SDKs' 10-minute default. It also bounds
// reading the body, streamed or not, so it must cover a long generation.
const DefaultRequestTimeout = 10 * time.Minute

// clientOrDefault returns c, or a client with DefaultRequestTimeout. Never
// http.DefaultClient: it has no timeout, so a stalled server would hang
// the turn forever and retries and failover would never run.
func clientOrDefault(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: DefaultRequestTimeout}
}

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
	var out []byte
	err := e.send(ctx, url, header, body, true, func(r io.Reader) error {
		var err error
		if out, err = io.ReadAll(io.LimitReader(r, maxResponseBytes)); err != nil {
			return transportError(e.provider, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// stream sends body to url and hands the body of a 2xx reply to read as it
// arrives. Failures before the reply are retried per the policy; a failure
// while reading is not, since read may already have passed part of the
// stream on.
func (e *httpEndpoint) stream(ctx context.Context, url string, header http.Header, body []byte, read func(io.Reader) error) error {
	return e.send(ctx, url, header, body, false, read)
}

// send posts body until read has consumed a 2xx reply or the retry policy
// gives up. Errors from read are retried only when retryRead is set.
func (e *httpEndpoint) send(ctx context.Context, url string, header http.Header, body []byte, retryRead bool, read func(io.Reader) error) error {
	delays := e.retry.delays()
	for attempt := 0; ; attempt++ {
		retryAfter, reading, err := e.once(ctx, url, header, body, read)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", e.provider, ctx.Err())
		}
		var apiErr *APIError
		if (reading && !retryRead) || !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt >= len(delays) {
			return err
		}
		wait := delays[attempt]
		if retryAfter > 0 {
			wait = min(retryAfter, cmp.Or(e.retry.MaxRetryAfter, DefaultMaxRetryAfter))
		}
		if err := e.retry.sleep(ctx, wait); err != nil {
			return fmt.Errorf("%s: %w", e.provider, err)
		}
	}
}

// once makes one request and passes the body of a 2xx reply to read.
// reading reports whether it did, so that err came from read.
func (e *httpEndpoint) once(ctx context.Context, url string, header http.Header, body []byte, read func(io.Reader) error) (retryAfter time.Duration, reading bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, false, fmt.Errorf("%s: build request: %w", e.provider, err)
	}
	req.Header = header.Clone()
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return 0, false, transportError(e.provider, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0, true, read(resp.Body)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, false, transportError(e.provider, err)
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
	return parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), false, apiErr
}

// parseRetryAfter parses a Retry-After header in seconds or HTTP-date form.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		switch {
		case math.IsNaN(secs) || secs <= 0:
			return 0
		case secs >= float64(math.MaxInt64/int64(time.Second)):
			return math.MaxInt64 // converting would overflow; the caller caps it
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
