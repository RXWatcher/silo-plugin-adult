package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// stubDoer drives DoWithRetry through a fixed sequence of outcomes.
type stubDoer struct {
	calls   int32
	results []func() (*http.Response, error)
}

func (s *stubDoer) Do(_ *http.Request) (*http.Response, error) {
	n := atomic.AddInt32(&s.calls, 1)
	idx := int(n) - 1
	if idx >= len(s.results) {
		idx = len(s.results) - 1
	}
	return s.results[idx]()
}

func okResp(code int) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Body:       io.NopCloser(strings.NewReader("body")),
		}, nil
	}
}

func netErr() func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
}

func build() (*http.Request, error) {
	return http.NewRequest(http.MethodGet, "http://example.test/x", nil)
}

func TestDoWithRetry_SucceedsFirstTry(t *testing.T) {
	d := &stubDoer{results: []func() (*http.Response, error){okResp(200)}}
	resp, err := DoWithRetry(context.Background(), d, RetryConfig{Source: "t"}, build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1", d.calls)
	}
}

func TestDoWithRetry_RetriesOn503ThenSucceeds(t *testing.T) {
	d := &stubDoer{results: []func() (*http.Response, error){
		okResp(503),
		okResp(503),
		okResp(200),
	}}
	resp, err := DoWithRetry(context.Background(), d, RetryConfig{Source: "t", MaxAttempts: 3}, build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if d.calls != 3 {
		t.Fatalf("calls = %d, want 3", d.calls)
	}
}

func TestDoWithRetry_RetriesOnNetworkErrorThenSucceeds(t *testing.T) {
	d := &stubDoer{results: []func() (*http.Response, error){
		netErr(),
		okResp(200),
	}}
	resp, err := DoWithRetry(context.Background(), d, RetryConfig{Source: "t", MaxAttempts: 3}, build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if d.calls != 2 {
		t.Fatalf("calls = %d, want 2", d.calls)
	}
}

func TestDoWithRetry_ExhaustsAndReturnsLastError(t *testing.T) {
	d := &stubDoer{results: []func() (*http.Response, error){netErr()}}
	_, err := DoWithRetry(context.Background(), d, RetryConfig{Source: "t", MaxAttempts: 3}, build)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if d.calls != 3 {
		t.Fatalf("calls = %d, want 3", d.calls)
	}
}

func TestDoWithRetry_Returns503WhenAttemptsExhausted(t *testing.T) {
	d := &stubDoer{results: []func() (*http.Response, error){okResp(503)}}
	resp, err := DoWithRetry(context.Background(), d, RetryConfig{Source: "t", MaxAttempts: 2}, build)
	// Final attempt's 503 is surfaced as a response (not retried again), so the
	// caller's normal status handling applies.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if d.calls != 2 {
		t.Fatalf("calls = %d, want 2", d.calls)
	}
}

func TestDoWithRetry_DoesNotRetry4xx(t *testing.T) {
	d := &stubDoer{results: []func() (*http.Response, error){okResp(404)}}
	resp, err := DoWithRetry(context.Background(), d, RetryConfig{Source: "t", MaxAttempts: 3}, build)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1 (4xx must not retry)", d.calls)
	}
}

func TestDoWithRetry_StopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &stubDoer{results: []func() (*http.Response, error){netErr()}}
	_, err := DoWithRetry(ctx, d, RetryConfig{Source: "t", MaxAttempts: 3}, build)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cancelled ctx must not retry)", d.calls)
	}
}
