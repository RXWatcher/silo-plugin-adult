package stash

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RXWatcher/silo-plugin-adult/metadata"
	"github.com/RXWatcher/silo-plugin-adult/provider/logging"
)

// fullPage builds a findScenes envelope with n scenes, all distinct IDs.
func fullPage(n int) string {
	var b strings.Builder
	b.WriteString(`{"data":{"findScenes":{"scenes":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"s","title":"t","date":"2020-01-01"}`)
	}
	b.WriteString(`]}}}`)
	return b.String()
}

func TestGetEpisodesLogsWhenCapTruncates(t *testing.T) {
	orig := logging.L()
	t.Cleanup(func() { logging.SetLogger(orig) })
	var buf bytes.Buffer
	logging.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Always return a full page so the loop only stops on the cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullPage(100)))
	}))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Enabled: true})
	eps, err := s.GetEpisodes(context.Background(), metadata.EpisodesRequest{
		ProviderIDs:  map[string]string{Slug: "studio:7"},
		SeasonNumber: 1,
	})
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) < maxEpisodesPerStudio {
		t.Fatalf("episodes = %d, want >= cap %d", len(eps), maxEpisodesPerStudio)
	}
	if !strings.Contains(buf.String(), "episode cap reached") {
		t.Fatalf("expected cap-truncation warning, got logs: %q", buf.String())
	}
}

func TestGetEpisodesNoCapLogOnShortPage(t *testing.T) {
	orig := logging.L()
	t.Cleanup(func() { logging.SetLogger(orig) })
	var buf bytes.Buffer
	logging.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullPage(3))) // short page → loop ends naturally
	}))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Enabled: true})
	eps, err := s.GetEpisodes(context.Background(), metadata.EpisodesRequest{
		ProviderIDs:  map[string]string{Slug: "studio:7"},
		SeasonNumber: 1,
	})
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) != 3 {
		t.Fatalf("episodes = %d, want 3", len(eps))
	}
	if strings.Contains(buf.String(), "episode cap reached") {
		t.Fatalf("unexpected cap warning on short page: %q", buf.String())
	}
}
