// Package httpx provides shared HTTP helpers for the plugin's upstream clients:
// a bounded retry/backoff loop for idempotent GET-style requests against
// transient upstream failures.
package httpx

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/RXWatcher/silo-plugin-adult/provider/logging"
)

// DefaultMaxAttempts is the total number of attempts (initial try + retries)
// made for a transient failure before giving up.
const DefaultMaxAttempts = 3

// baseBackoff is the delay before the first retry; subsequent retries grow
// exponentially (base, 2*base, 4*base, ...).
const baseBackoff = 250 * time.Millisecond

// Doer is the subset of *http.Client used here, so tests can stub it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// RetryConfig tunes DoWithRetry. The zero value is usable: MaxAttempts falls
// back to DefaultMaxAttempts and Source defaults to "upstream".
type RetryConfig struct {
	MaxAttempts int
	Source      string // short label used in log records (e.g. "tpdb", "stash")
}

// DoWithRetry executes build()/Do with bounded retry on transient failures.
//
// It is intended only for idempotent requests (GETs and read-only GraphQL
// queries). A request is retried when the transport returns a network error or
// when the response status is a retryable 5xx (500, 502, 503, 504). Non-5xx
// responses (including 4xx) and context cancellation are returned immediately.
//
// build is called once per attempt and must return a fresh *http.Request bound
// to ctx (request bodies are consumed by Do, so they cannot be reused). The
// returned response is the caller's to close.
func DoWithRetry(ctx context.Context, doer Doer, cfg RetryConfig, build func() (*http.Request, error)) (*http.Response, error) {
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}
	source := cfg.Source
	if source == "" {
		source = "upstream"
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := doer.Do(req)
		if err != nil {
			// Context cancellation/deadline is not transient — fail fast.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, err
			}
			lastErr = err
			if attempt == attempts {
				break
			}
			logging.L().Warn("upstream request failed, retrying",
				"source", source,
				"attempt", attempt,
				"max_attempts", attempts,
				"error", err.Error(),
			)
			if waitErr := sleep(ctx, backoff(attempt)); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if !retryableStatus(resp.StatusCode) || attempt == attempts {
			return resp, nil
		}

		// Transient server error: drain+close this response and retry.
		status := resp.StatusCode
		resp.Body.Close()
		lastErr = &statusError{source: source, status: status}
		logging.L().Warn("upstream returned retryable status, retrying",
			"source", source,
			"status", status,
			"attempt", attempt,
			"max_attempts", attempts,
		)
		if waitErr := sleep(ctx, backoff(attempt)); waitErr != nil {
			return nil, waitErr
		}
	}

	return nil, lastErr
}

// retryableStatus reports whether an HTTP status warrants a retry. Only the
// transient 5xx family is retried; 4xx and other codes are surfaced as-is.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, // 500
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	default:
		return false
	}
}

// backoff returns the delay before the retry following the given (1-indexed)
// attempt: base, 2*base, 4*base, ...
func backoff(attempt int) time.Duration {
	return time.Duration(float64(baseBackoff) * math.Pow(2, float64(attempt-1)))
}

// sleep waits for d or until ctx is done, returning ctx.Err() if cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type statusError struct {
	source string
	status int
}

func (e *statusError) Error() string {
	return e.source + ": upstream returned retryable status " + http.StatusText(e.status)
}
