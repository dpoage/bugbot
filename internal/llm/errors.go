package llm

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors callers can match with errors.Is. Adapters normalize
// provider-specific failures into these where the cause is recognizable.
var (
	// ErrRateLimited indicates a 429 / rate-limit response. The retry wrapper
	// treats it as retryable and honors any Retry-After it carries.
	ErrRateLimited = errors.New("llm: rate limited")
	// ErrAuth indicates an authentication/authorization failure (401/403). Not
	// retryable.
	ErrAuth = errors.New("llm: authentication failed")
	// ErrContextTooLong indicates the request exceeded the model's context
	// window (often surfaced as a 400 with a recognizable message, or a 413).
	// Not retryable without shrinking the request.
	ErrContextTooLong = errors.New("llm: context too long")
	// ErrInvalidRequest indicates a malformed request the provider rejected
	// (400, other than context-too-long). Not retryable.
	ErrInvalidRequest = errors.New("llm: invalid request")
	// ErrServer indicates a transient server-side failure (5xx). Retryable.
	ErrServer = errors.New("llm: server error")
	// ErrOverloaded indicates the provider is temporarily overloaded (e.g. 529).
	// Retryable.
	ErrOverloaded = errors.New("llm: provider overloaded")
)

// APIError is the normalized error type adapters return. It wraps one of the
// sentinel errors (accessible via errors.Is / the Kind field), preserves the
// HTTP status code and any Retry-After hint, and chains the underlying SDK
// error for debugging. It deliberately carries no API key or request body.
type APIError struct {
	// Kind is the sentinel error this maps to (ErrRateLimited, ErrAuth, ...).
	Kind error
	// StatusCode is the HTTP status, or 0 if not an HTTP error (e.g. a timeout
	// or transport error).
	StatusCode int
	// RetryAfter is the server-suggested wait before retrying, or 0 if none was
	// provided. Only populated for ErrRateLimited / ErrOverloaded responses that
	// carried a Retry-After header.
	RetryAfter time.Duration
	// Provider names the backend that produced the error (e.g. "anthropic").
	Provider string
	// Message is a short, key-free description.
	Message string
	// err is the underlying SDK error, for Unwrap chaining.
	err error
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("llm: %s error (status %d): %s", e.Provider, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("llm: %s error: %s", e.Provider, e.Message)
}

// Unwrap returns both the Kind sentinel (so errors.Is(err, ErrRateLimited) etc.
// work) and the underlying SDK error (so callers can still reach provider
// detail). Uses the multi-error Unwrap form (Go 1.20+).
func (e *APIError) Unwrap() []error {
	if e.err != nil {
		return []error{e.Kind, e.err}
	}
	return []error{e.Kind}
}

// retryable reports whether this error class is worth retrying.
func (e *APIError) retryable() bool {
	switch e.Kind {
	case ErrRateLimited, ErrServer, ErrOverloaded:
		return true
	default:
		return false
	}
}

// isRetryable reports whether err is a retryable APIError or a bare transport
// error (timeout / connection reset) that the adapters could not classify.
func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.retryable()
	}
	// Unclassified non-API errors (transport timeouts, connection resets) are
	// transient by nature; retrying is the safer default.
	return err != nil
}

// retryAfter extracts a server-suggested delay from err, if any.
func retryAfter(err error) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter, true
	}
	return 0, false
}
