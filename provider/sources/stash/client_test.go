package stash

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://stash.local:9999", "http://stash.local:9999/graphql"},
		{"http://stash.local:9999/", "http://stash.local:9999/graphql"},
		{"http://stash.local:9999/graphql", "http://stash.local:9999/graphql"},
		{"https://stash.example/graphql/", "https://stash.example/graphql"},
		{"", ""},
		{"ftp://stash.local", ""},
		{"not a url", ""},
		{"/relative/path", ""},
	}
	for _, c := range cases {
		if got := normalizeBaseURL(c.in); got != c.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClientHasRateLimiter(t *testing.T) {
	c := NewClient("http://stash.local:9999", "")
	if c.limiter == nil {
		t.Fatal("expected stash client to have a rate limiter")
	}
}

func TestDoRetriesTransient503(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"findScene":{"id":"42","title":"ok"}}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	sc, err := c.FindScene(context.Background(), "42")
	if err != nil {
		t.Fatalf("FindScene: %v", err)
	}
	if sc.ID != "42" || sc.Title != "ok" {
		t.Fatalf("unexpected scene: %+v", sc)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2 (one 503 retry)", got)
	}
}

func TestDoDoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	if _, err := c.FindScene(context.Background(), "1"); err == nil {
		t.Fatal("expected error on 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server calls = %d, want 1 (4xx must not retry)", got)
	}
}
