package llm

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// classifyStatus maps an HTTP status code to a sentinel error Kind. body is the
// (optional) error message, used to disambiguate 400s into ErrContextTooLong.
func classifyStatus(status int, body string) error {
	switch {
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusRequestEntityTooLarge:
		return ErrContextTooLong
	case status == http.StatusBadRequest:
		if looksLikeContextLength(body) {
			return ErrContextTooLong
		}
		return ErrInvalidRequest
	case status == 529:
		return ErrOverloaded
	case status >= 500:
		return ErrServer
	default:
		return ErrInvalidRequest
	}
}

// looksLikeContextLength heuristically detects a context-window-exceeded error
// from a provider's 400 message. Providers phrase this differently, so we match
// a few well-known fragments.
func looksLikeContextLength(msg string) bool {
	m := strings.ToLower(msg)
	for _, frag := range []string{
		"context length",
		"context window",
		"maximum context",
		"too many tokens",
		"prompt is too long",
		"input is too long",
		"maximum number of tokens",
		"exceeds the maximum",
		"reduce the length",
	} {
		if strings.Contains(m, frag) {
			return true
		}
	}
	return false
}

// parseRetryAfter reads a Retry-After header (seconds or HTTP-date) from resp
// and returns it as a duration. Returns 0 if absent or unparseable.
func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// newAPIError builds a normalized *APIError from a classified failure.
func newAPIError(provider string, status int, retryAfter time.Duration, kind error, message string, underlying error) *APIError {
	return &APIError{
		Kind:       kind,
		StatusCode: status,
		RetryAfter: retryAfter,
		Provider:   provider,
		Message:    message,
		err:        underlying,
	}
}
