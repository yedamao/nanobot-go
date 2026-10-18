package providers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// Kind classifies a provider failure.
type Kind string

// Error kinds.
const (
	KindTimeout     Kind = "timeout"
	KindConnection  Kind = "connection"
	KindServerError Kind = "server_error"
	KindRateLimit   Kind = "rate_limit"
	KindAuth        Kind = "auth"
	KindBadRequest  Kind = "bad_request"
)

// APIError is a failed provider call.
type APIError struct {
	Provider string
	// StatusCode is the HTTP status, or 0 for transport failures.
	StatusCode int
	Kind       Kind
	Message    string
	// Err is the underlying transport error, if any.
	Err error
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: %s (HTTP %d): %s", e.Provider, e.Kind, e.StatusCode, msg)
	}
	return fmt.Sprintf("%s: %s: %s", e.Provider, e.Kind, msg)
}

func (e *APIError) Unwrap() error { return e.Err }

// Retryable reports whether the same request may succeed if sent again:
// 408/409/429/5xx responses and network failures.
func (e *APIError) Retryable() bool {
	switch e.Kind {
	case KindTimeout, KindConnection, KindServerError, KindRateLimit:
		return true
	}
	return false
}

// kindForStatus classifies an HTTP error status.
func kindForStatus(status int) Kind {
	switch {
	case status == http.StatusRequestTimeout:
		return KindTimeout
	case status == http.StatusTooManyRequests:
		return KindRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusConflict || status >= 500:
		// 409 is transient on these APIs (concurrent request conflicts);
		// 5xx includes Anthropic's 529 "overloaded".
		return KindServerError
	default:
		return KindBadRequest
	}
}

// transportError classifies an error returned by http.Client.Do.
func transportError(provider string, err error) *APIError {
	kind := KindConnection
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		kind = KindTimeout
	}
	return &APIError{Provider: provider, Kind: kind, Err: err}
}
