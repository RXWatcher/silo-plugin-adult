package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/RXWatcher/continuum-plugin-adult/metadata"
)

// Aggregator fans out requests across one or more enabled Sources and merges
// the results. Sources can be swapped in via SetSources; this is called when
// the host delivers configuration updates.
type Aggregator struct {
	mu      sync.RWMutex
	sources []Source
}

// NewAggregator returns an empty Aggregator. Use SetSources to populate it.
func NewAggregator() *Aggregator {
	return &Aggregator{}
}

// SetSources replaces the current source list with the supplied slice.
// Sources reporting Enabled() == false are filtered out; the rest are sorted
// by Priority() ascending.
func (a *Aggregator) SetSources(sources []Source) {
	enabled := make([]Source, 0, len(sources))
	for _, s := range sources {
		if s != nil && s.Enabled() {
			enabled = append(enabled, s)
		}
	}
	sort.SliceStable(enabled, func(i, j int) bool {
		return enabled[i].Priority() < enabled[j].Priority()
	})

	a.mu.Lock()
	a.sources = enabled
	a.mu.Unlock()
}

// Sources returns a snapshot of the active source list in priority order.
func (a *Aggregator) Sources() []Source {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Source, len(a.sources))
	copy(out, a.sources)
	return out
}

// sourceBySlug returns the configured source with the given slug, or nil.
func (a *Aggregator) sourceBySlug(slug string) Source {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, s := range a.sources {
		if s.Slug() == slug {
			return s
		}
	}
	return nil
}

// targetSource picks the source a routed request should hit. It checks
// ProviderIDs["adult"] (canonical "<slug>:<id>") first, then any per-source
// keys (e.g. ProviderIDs["tpdb"]), returning the matching source if found.
//
// Returns (source, sourceID, ok). When ok is false the caller should fan out
// instead of routing.
func (a *Aggregator) targetSource(providerIDs map[string]string) (Source, string, bool) {
	if id, ok := providerIDs[CapabilityID]; ok && id != "" {
		if slug, sourceID, ok := DecodeProviderID(id); ok {
			if s := a.sourceBySlug(slug); s != nil {
				return s, sourceID, true
			}
		}
	}
	// Fall back to per-source keys (e.g. ProviderIDs["tpdb"]).
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, s := range a.sources {
		if id, ok := providerIDs[s.Slug()]; ok && id != "" {
			return s, id, true
		}
	}
	return nil, "", false
}

// Search fans out across all enabled sources (or routes to a single source if
// the query carries a provider ID) and returns the merged result list.
//
// Results are returned in priority order: the highest-priority source's
// results come first, then the next, etc. Within a source, the source's
// own ordering is preserved.
func (a *Aggregator) Search(ctx context.Context, query metadata.SearchQuery) ([]metadata.SearchResult, error) {
	if s, sourceID, ok := a.targetSource(query.ProviderIDs); ok {
		routed := query
		routed.ProviderIDs = map[string]string{s.Slug(): sourceID}
		results, err := s.Search(ctx, routed)
		if err != nil {
			return nil, err
		}
		return decorateResults(s, results), nil
	}

	sources := a.Sources()
	if len(sources) == 0 {
		return nil, nil
	}

	type sourceResult struct {
		idx     int
		results []metadata.SearchResult
		err     error
	}
	ch := make(chan sourceResult, len(sources))
	for i, s := range sources {
		go func(idx int, src Source) {
			r, err := src.Search(ctx, query)
			if err == nil {
				r = decorateResults(src, r)
			}
			ch <- sourceResult{idx: idx, results: r, err: err}
		}(i, s)
	}

	collected := make([][]metadata.SearchResult, len(sources))
	var firstErr error
	for i := 0; i < len(sources); i++ {
		r := <-ch
		if r.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", sources[r.idx].Slug(), r.err)
			continue
		}
		collected[r.idx] = r.results
	}

	merged := make([]metadata.SearchResult, 0)
	for _, batch := range collected {
		merged = append(merged, batch...)
	}
	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return merged, nil
}

// GetMetadata routes to the source identified by req.ProviderIDs.
func (a *Aggregator) GetMetadata(ctx context.Context, req metadata.MetadataRequest) (*metadata.MetadataResult, error) {
	s, sourceID, ok := a.targetSource(req.ProviderIDs)
	if !ok {
		return nil, errors.New("adult: no source matches request (missing or unknown provider id)")
	}
	routed := req
	routed.ProviderIDs = mergeIDs(req.ProviderIDs, s.Slug(), sourceID)
	result, err := s.GetMetadata(ctx, routed)
	if err != nil || result == nil {
		return result, err
	}
	stampResult(s, result)
	return result, nil
}

// GetPersonDetail routes to the source identified by req.ProviderIDs.
func (a *Aggregator) GetPersonDetail(ctx context.Context, req metadata.PersonDetailRequest) (*metadata.PersonDetailResult, error) {
	s, sourceID, ok := a.targetSource(req.ProviderIDs)
	if !ok {
		return nil, errors.New("adult: no source matches person request")
	}
	routed := req
	routed.ProviderIDs = mergeIDs(req.ProviderIDs, s.Slug(), sourceID)
	result, err := s.GetPersonDetail(ctx, routed)
	if err != nil || result == nil {
		return result, err
	}
	stampPerson(s, result)
	return result, nil
}

// GetSeasons routes to the source identified by req.ProviderIDs.
func (a *Aggregator) GetSeasons(ctx context.Context, req metadata.SeasonsRequest) ([]metadata.SeasonResult, error) {
	s, sourceID, ok := a.targetSource(req.ProviderIDs)
	if !ok {
		return nil, errors.New("adult: no source matches seasons request")
	}
	routed := req
	routed.ProviderIDs = mergeIDs(req.ProviderIDs, s.Slug(), sourceID)
	return s.GetSeasons(ctx, routed)
}

// GetEpisodes routes to the source identified by req.ProviderIDs.
func (a *Aggregator) GetEpisodes(ctx context.Context, req metadata.EpisodesRequest) ([]metadata.EpisodeResult, error) {
	s, sourceID, ok := a.targetSource(req.ProviderIDs)
	if !ok {
		return nil, errors.New("adult: no source matches episodes request")
	}
	routed := req
	routed.ProviderIDs = mergeIDs(req.ProviderIDs, s.Slug(), sourceID)
	return s.GetEpisodes(ctx, routed)
}

// GetImages routes to the source identified by req.ProviderIDs.
func (a *Aggregator) GetImages(ctx context.Context, req metadata.ImageRequest) ([]metadata.RemoteImage, error) {
	s, sourceID, ok := a.targetSource(req.ProviderIDs)
	if !ok {
		return nil, errors.New("adult: no source matches images request")
	}
	routed := req
	routed.ProviderIDs = mergeIDs(req.ProviderIDs, s.Slug(), sourceID)
	return s.GetImages(ctx, routed)
}

// ResolveImage parses an adult:// image path and dispatches to the source's
// own resolver. Returns "" if the path is malformed or the source is gone.
func (a *Aggregator) ResolveImage(encodedPath, variant string) string {
	slug, role, rawPath, ok := DecodeImagePath(encodedPath)
	if !ok {
		return ""
	}
	s := a.sourceBySlug(slug)
	if s == nil {
		return ""
	}
	return s.ResolveImage(role, rawPath, variant)
}

// decorateResults stamps the source slug into each result so callers can tell
// which source matched without looking at the ProviderIDs map.
func decorateResults(s Source, results []metadata.SearchResult) []metadata.SearchResult {
	for i := range results {
		results[i].Provider = s.Slug()
		if results[i].ProviderIDs == nil {
			results[i].ProviderIDs = map[string]string{}
		}
		if id := results[i].ProviderIDs[s.Slug()]; id != "" {
			results[i].ProviderIDs[CapabilityID] = EncodeProviderID(s.Slug(), id)
		}
	}
	return results
}

func stampResult(s Source, result *metadata.MetadataResult) {
	if result.ProviderIDs == nil {
		result.ProviderIDs = map[string]string{}
	}
	if id := result.ProviderIDs[s.Slug()]; id != "" {
		result.ProviderIDs[CapabilityID] = EncodeProviderID(s.Slug(), id)
	}
}

func stampPerson(s Source, result *metadata.PersonDetailResult) {
	if result.ProviderIDs == nil {
		result.ProviderIDs = map[string]string{}
	}
	if id := result.ProviderIDs[s.Slug()]; id != "" {
		result.ProviderIDs[CapabilityID] = EncodeProviderID(s.Slug(), id)
	}
}

func mergeIDs(in map[string]string, slug, id string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	if id != "" {
		out[slug] = id
	}
	return out
}
