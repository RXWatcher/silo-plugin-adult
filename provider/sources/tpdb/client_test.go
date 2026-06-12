package tpdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGetRetriesTransient500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"abc","title":"Scene"}}`))
	}))
	defer srv.Close()

	c := NewClient("key")
	c.SetBaseURL(srv.URL)
	sc, err := c.GetScene(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetScene: %v", err)
	}
	if sc.ID != "abc" {
		t.Fatalf("scene id = %q, want abc", sc.ID)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2 (one 500 retry)", got)
	}
}

func TestGetMaps404ToNotFound(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient("key")
	c.SetBaseURL(srv.URL)
	if _, err := c.GetScene(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server calls = %d, want 1 (404 must not retry)", got)
	}
}
