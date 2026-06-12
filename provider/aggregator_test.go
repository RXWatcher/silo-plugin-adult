package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/RXWatcher/silo-plugin-adult/metadata"
)

// fakeSource is a minimal Source implementation for testing aggregator
// fanout, routing, and image dispatch.
type fakeSource struct {
	slug       string
	enabled    bool
	priority   int
	searchHits []metadata.SearchResult
	searchErr  error
	resolved   map[string]string
}

func (f *fakeSource) Slug() string  { return f.slug }
func (f *fakeSource) Name() string  { return f.slug }
func (f *fakeSource) Enabled() bool { return f.enabled }
func (f *fakeSource) Priority() int { return f.priority }

func (f *fakeSource) Search(_ context.Context, _ metadata.SearchQuery) ([]metadata.SearchResult, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	out := make([]metadata.SearchResult, len(f.searchHits))
	copy(out, f.searchHits)
	return out, nil
}

func (f *fakeSource) GetMetadata(_ context.Context, _ metadata.MetadataRequest) (*metadata.MetadataResult, error) {
	return &metadata.MetadataResult{
		HasMetadata: true,
		Title:       f.slug + "-title",
		ProviderIDs: map[string]string{f.slug: "abc"},
	}, nil
}

func (f *fakeSource) GetPersonDetail(_ context.Context, _ metadata.PersonDetailRequest) (*metadata.PersonDetailResult, error) {
	return &metadata.PersonDetailResult{Name: f.slug + "-person", ProviderIDs: map[string]string{f.slug: "p1"}}, nil
}

func (f *fakeSource) GetSeasons(_ context.Context, _ metadata.SeasonsRequest) ([]metadata.SeasonResult, error) {
	return nil, nil
}

func (f *fakeSource) GetEpisodes(_ context.Context, _ metadata.EpisodesRequest) ([]metadata.EpisodeResult, error) {
	return nil, nil
}

func (f *fakeSource) GetImages(_ context.Context, _ metadata.ImageRequest) ([]metadata.RemoteImage, error) {
	return nil, nil
}

func (f *fakeSource) ResolveImage(role, rawPath, _ string) string {
	if v, ok := f.resolved[rawPath]; ok {
		return v
	}
	return f.slug + ":" + role + ":" + rawPath
}

func TestAggregatorSearchOrdersByPriority(t *testing.T) {
	low := &fakeSource{slug: "low", enabled: true, priority: 1, searchHits: []metadata.SearchResult{
		{Name: "from-low", ProviderIDs: map[string]string{"low": "1"}},
	}}
	high := &fakeSource{slug: "high", enabled: true, priority: 10, searchHits: []metadata.SearchResult{
		{Name: "from-high", ProviderIDs: map[string]string{"high": "2"}},
	}}

	agg := NewAggregator()
	agg.SetSources([]Source{high, low}) // input order is intentionally reversed

	got, err := agg.Search(context.Background(), metadata.SearchQuery{Title: "x"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].Name != "from-low" {
		t.Errorf("expected highest-priority (lowest number) source first; got %q", got[0].Name)
	}
	if got[0].ProviderIDs[CapabilityID] != "low:1" {
		t.Errorf("expected canonical id %q, got %q", "low:1", got[0].ProviderIDs[CapabilityID])
	}
}

func TestAggregatorSearchSkipsDisabledSources(t *testing.T) {
	on := &fakeSource{slug: "on", enabled: true, priority: 1, searchHits: []metadata.SearchResult{
		{Name: "from-on", ProviderIDs: map[string]string{"on": "1"}},
	}}
	off := &fakeSource{slug: "off", enabled: false, priority: 1, searchHits: []metadata.SearchResult{
		{Name: "from-off", ProviderIDs: map[string]string{"off": "1"}},
	}}
	agg := NewAggregator()
	agg.SetSources([]Source{on, off})
	got, _ := agg.Search(context.Background(), metadata.SearchQuery{Title: "x"})
	if len(got) != 1 || got[0].Name != "from-on" {
		t.Errorf("expected only enabled source's results, got %#v", got)
	}
}

func TestAggregatorRoutesByCanonicalProviderID(t *testing.T) {
	low := &fakeSource{slug: "low", enabled: true, priority: 1}
	high := &fakeSource{slug: "high", enabled: true, priority: 10}
	agg := NewAggregator()
	agg.SetSources([]Source{low, high})

	got, err := agg.GetMetadata(context.Background(), metadata.MetadataRequest{
		ProviderIDs: map[string]string{CapabilityID: "high:xyz"},
	})
	if err != nil {
		t.Fatalf("GetMetadata returned error: %v", err)
	}
	if got == nil || got.Title != "high-title" {
		t.Errorf("expected routing to high source; got %#v", got)
	}
	if got.ProviderIDs[CapabilityID] != "high:abc" {
		t.Errorf("expected canonical id stamped on result; got %q", got.ProviderIDs[CapabilityID])
	}
}

func TestAggregatorResolveImageDispatchesToSource(t *testing.T) {
	src := &fakeSource{slug: "src", enabled: true, priority: 1, resolved: map[string]string{"raw": "https://example.test/img.jpg"}}
	agg := NewAggregator()
	agg.SetSources([]Source{src})

	encoded := EncodeImagePath("src", "poster", "raw")
	got := agg.ResolveImage(encoded, "card")
	if got != "https://example.test/img.jpg" {
		t.Errorf("ResolveImage = %q, want example URL", got)
	}
}

func TestAggregatorReturnsErrorWhenAllSourcesFail(t *testing.T) {
	a := &fakeSource{slug: "a", enabled: true, priority: 1, searchErr: errors.New("a-boom")}
	b := &fakeSource{slug: "b", enabled: true, priority: 2, searchErr: errors.New("b-boom")}
	agg := NewAggregator()
	agg.SetSources([]Source{a, b})

	_, err := agg.Search(context.Background(), metadata.SearchQuery{Title: "x"})
	if err == nil {
		t.Fatal("expected error when all sources fail")
	}
}

func TestAggregatorSearchErrorIsHighestPriority(t *testing.T) {
	// high priority (lower number) sorts first; its error must be the one
	// surfaced regardless of goroutine completion order.
	high := &fakeSource{slug: "high", enabled: true, priority: 1, searchErr: errors.New("high-boom")}
	low := &fakeSource{slug: "low", enabled: true, priority: 9, searchErr: errors.New("low-boom")}
	agg := NewAggregator()
	agg.SetSources([]Source{low, high}) // input order reversed on purpose

	for i := 0; i < 50; i++ { // repeat to shake out nondeterminism
		_, err := agg.Search(context.Background(), metadata.SearchQuery{Title: "x"})
		if err == nil {
			t.Fatal("expected error when all sources fail")
		}
		if got := err.Error(); got[:4] != "high" {
			t.Fatalf("expected highest-priority source error first, got %q", got)
		}
	}
}

func TestAggregatorSearchRespectsContextCancellation(t *testing.T) {
	// blockingSource never returns until its context is cancelled, so the
	// collector's only ready select case is ctx.Done().
	src := &blockingSource{slug: "src"}
	agg := NewAggregator()
	agg.SetSources([]Source{src})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	if _, err := agg.Search(ctx, metadata.SearchQuery{Title: "x"}); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// blockingSource blocks Search until the context is done, modelling a slow
// upstream so we can exercise the collector's ctx.Done() path.
type blockingSource struct{ slug string }

func (b *blockingSource) Slug() string  { return b.slug }
func (b *blockingSource) Name() string  { return b.slug }
func (b *blockingSource) Enabled() bool { return true }
func (b *blockingSource) Priority() int { return 1 }

func (b *blockingSource) Search(ctx context.Context, _ metadata.SearchQuery) ([]metadata.SearchResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingSource) GetMetadata(context.Context, metadata.MetadataRequest) (*metadata.MetadataResult, error) {
	return nil, nil
}

func (b *blockingSource) GetPersonDetail(context.Context, metadata.PersonDetailRequest) (*metadata.PersonDetailResult, error) {
	return nil, nil
}

func (b *blockingSource) GetSeasons(context.Context, metadata.SeasonsRequest) ([]metadata.SeasonResult, error) {
	return nil, nil
}

func (b *blockingSource) GetEpisodes(context.Context, metadata.EpisodesRequest) ([]metadata.EpisodeResult, error) {
	return nil, nil
}

func (b *blockingSource) GetImages(context.Context, metadata.ImageRequest) ([]metadata.RemoteImage, error) {
	return nil, nil
}

func (b *blockingSource) ResolveImage(_, _, _ string) string { return "" }

func TestEncodeDecodeProviderID(t *testing.T) {
	encoded := EncodeProviderID("tpdb", "scene:abc")
	if encoded != "tpdb:scene:abc" {
		t.Errorf("EncodeProviderID = %q, want tpdb:scene:abc", encoded)
	}
	slug, id, ok := DecodeProviderID(encoded)
	if !ok || slug != "tpdb" || id != "scene:abc" {
		t.Errorf("DecodeProviderID = (%q, %q, %v), want (tpdb, scene:abc, true)", slug, id, ok)
	}
}

func TestEncodeDecodeImagePath(t *testing.T) {
	encoded := EncodeImagePath("tpdb", "poster", "https%3A%2F%2Fcdn.example%2Fa.jpg")
	if encoded != "adult://tpdb/poster/https%3A%2F%2Fcdn.example%2Fa.jpg" {
		t.Errorf("EncodeImagePath = %q", encoded)
	}
	slug, role, raw, ok := DecodeImagePath(encoded)
	if !ok || slug != "tpdb" || role != "poster" || raw != "https%3A%2F%2Fcdn.example%2Fa.jpg" {
		t.Errorf("DecodeImagePath = (%q, %q, %q, %v)", slug, role, raw, ok)
	}
}
