// Package stash implements the provider.Source interface backed by a
// self-hosted Stash instance (https://stashapp.cc).
//
// Stash exposes scenes, studios, and performers. This source maps:
//
//	Stash scene    → host movie (when standalone) / host episode (when fetched
//	                 under a studio)
//	Stash studio   → host series (with a synthetic single season)
//	Stash performer → host person
//
// Source-local IDs are namespaced: "scene:<id>", "studio:<id>",
// "performer:<id>".
package stash

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/RXWatcher/silo-plugin-adult/metadata"
	"github.com/RXWatcher/silo-plugin-adult/models"
	"github.com/RXWatcher/silo-plugin-adult/provider"
	"github.com/RXWatcher/silo-plugin-adult/provider/logging"
)

// Slug is the source identifier used in ProviderIDs maps and image URLs.
const Slug = "stash"

// maxEpisodesPerStudio caps how many scenes we'll walk for a single studio.
const maxEpisodesPerStudio = 1000

// Source implements provider.Source for Stash.
type Source struct {
	client   *Client
	enabled  bool
	priority int
}

// Config holds the user-provided configuration for the Stash source.
type Config struct {
	URL      string // GraphQL endpoint (e.g. http://stash.local:9999/graphql)
	APIKey   string
	Enabled  bool
	Priority int
}

// New returns a configured Stash source. If cfg.URL is empty the source
// reports Enabled() == false regardless of cfg.Enabled. APIKey is optional —
// some Stash deployments don't require auth.
func New(cfg Config) *Source {
	enabled := cfg.Enabled && cfg.URL != ""
	priority := cfg.Priority
	if priority <= 0 {
		priority = 20
	}
	return &Source{
		client:   NewClient(cfg.URL, cfg.APIKey),
		enabled:  enabled,
		priority: priority,
	}
}

// Slug implements provider.Source.
func (s *Source) Slug() string { return Slug }

// Name implements provider.Source.
func (s *Source) Name() string { return "Stash" }

// Enabled implements provider.Source.
func (s *Source) Enabled() bool { return s.enabled }

// Priority implements provider.Source.
func (s *Source) Priority() int { return s.priority }

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// Search implements provider.Source.
//
//	"movie": findScenes by title
//	"series": findStudios by name
func (s *Source) Search(ctx context.Context, query metadata.SearchQuery) ([]metadata.SearchResult, error) {
	if id, ok := query.ProviderIDs[Slug]; ok && id != "" {
		return s.searchByLocalID(ctx, id)
	}
	if query.Title == "" {
		return nil, nil
	}
	switch query.ContentType {
	case "series":
		studios, err := s.client.FindStudios(ctx, query.Title)
		if err != nil {
			return nil, err
		}
		out := make([]metadata.SearchResult, 0, len(studios))
		for _, st := range studios {
			out = append(out, studioToSearchResult(st))
		}
		return out, nil
	default:
		scenes, err := s.client.FindScenes(ctx, query.Title)
		if err != nil {
			return nil, err
		}
		out := make([]metadata.SearchResult, 0, len(scenes))
		for _, sc := range scenes {
			out = append(out, sceneToSearchResult(sc))
		}
		return out, nil
	}
}

func (s *Source) searchByLocalID(ctx context.Context, id string) ([]metadata.SearchResult, error) {
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok {
		return nil, fmt.Errorf("stash: unrecognised id %q", id)
	}
	switch kind {
	case "scene":
		sc, err := s.client.FindScene(ctx, raw)
		if err != nil {
			return nil, err
		}
		return []metadata.SearchResult{sceneToSearchResult(*sc)}, nil
	case "studio":
		st, err := s.client.FindStudio(ctx, raw)
		if err != nil {
			return nil, err
		}
		return []metadata.SearchResult{studioToSearchResult(*st)}, nil
	}
	return nil, fmt.Errorf("stash: unsupported id kind %q", kind)
}

// ---------------------------------------------------------------------------
// GetMetadata
// ---------------------------------------------------------------------------

// GetMetadata implements provider.Source.
func (s *Source) GetMetadata(ctx context.Context, req metadata.MetadataRequest) (*metadata.MetadataResult, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("stash: missing source id")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok {
		return nil, fmt.Errorf("stash: unrecognised id %q", id)
	}
	switch kind {
	case "scene":
		sc, err := s.client.FindScene(ctx, raw)
		if err != nil {
			return nil, err
		}
		return sceneToMetadata(*sc), nil
	case "studio":
		st, err := s.client.FindStudio(ctx, raw)
		if err != nil {
			return nil, err
		}
		return studioToMetadata(*st), nil
	}
	return nil, fmt.Errorf("stash: unsupported id kind %q", kind)
}

// ---------------------------------------------------------------------------
// GetSeasons / GetEpisodes
// ---------------------------------------------------------------------------

// GetSeasons implements provider.Source. Stash studios don't have seasons,
// so we return a single synthetic Season 1 covering all the studio's scenes.
func (s *Source) GetSeasons(ctx context.Context, req metadata.SeasonsRequest) ([]metadata.SeasonResult, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("stash: missing source id for seasons")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok || kind != "studio" {
		return nil, fmt.Errorf("stash: GetSeasons requires a studio id, got %q", id)
	}
	st, err := s.client.FindStudio(ctx, raw)
	if err != nil {
		return nil, err
	}
	return []metadata.SeasonResult{{
		ContentID:    "studio:" + st.ID + ":season:1",
		SeasonNumber: 1,
		Title:        st.Name,
		Overview:     st.Details,
		PosterPath:   provider.EncodeAbsolute(st.ImagePath),
	}}, nil
}

// GetEpisodes implements provider.Source. Walks findScenes filtered by
// studio across pages and builds the full episode list ordered by date.
func (s *Source) GetEpisodes(ctx context.Context, req metadata.EpisodesRequest) ([]metadata.EpisodeResult, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("stash: missing source id for episodes")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok || kind != "studio" {
		return nil, fmt.Errorf("stash: GetEpisodes requires a studio id, got %q", id)
	}

	const perPage = 100
	out := make([]metadata.EpisodeResult, 0, perPage)
	for page := 1; len(out) < maxEpisodesPerStudio; page++ {
		scenes, err := s.client.ListScenesForStudio(ctx, raw, page, perPage)
		if err != nil {
			return nil, err
		}
		if len(scenes) == 0 {
			break
		}
		for i, sc := range scenes {
			out = append(out, sceneToEpisode(sc, req.SeasonNumber, len(out)+i+1))
		}
		if len(scenes) < perPage {
			break
		}
	}
	if len(out) >= maxEpisodesPerStudio {
		// The loop stopped on the cap, not a short page, so the studio may
		// have more scenes that were silently dropped. Surface that.
		logging.L().Warn("stash: episode cap reached, results truncated",
			"studio_id", raw,
			"cap", maxEpisodesPerStudio,
			"returned", len(out),
		)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// GetPersonDetail
// ---------------------------------------------------------------------------

// GetPersonDetail implements provider.Source.
func (s *Source) GetPersonDetail(ctx context.Context, req metadata.PersonDetailRequest) (*metadata.PersonDetailResult, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("stash: missing performer id")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok || kind != "performer" {
		raw = id
	}
	p, err := s.client.FindPerformer(ctx, raw)
	if err != nil {
		return nil, err
	}
	return &metadata.PersonDetailResult{
		Name:       p.Name,
		Bio:        p.Details,
		BirthDate:  p.Birthdate,
		DeathDate:  p.DeathDate,
		Birthplace: p.Country,
		PhotoPath:  provider.EncodeAbsolute(p.ImagePath),
		ProviderIDs: map[string]string{
			Slug: "performer:" + p.ID,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Images / ResolveImage
// ---------------------------------------------------------------------------

// GetImages implements provider.Source.
func (s *Source) GetImages(ctx context.Context, req metadata.ImageRequest) ([]metadata.RemoteImage, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("stash: missing source id for images")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok {
		return nil, fmt.Errorf("stash: unrecognised id %q", id)
	}
	out := make([]metadata.RemoteImage, 0, 2)
	switch kind {
	case "scene":
		sc, err := s.client.FindScene(ctx, raw)
		if err != nil {
			return nil, err
		}
		if sc.Paths.Screenshot != "" {
			out = append(out,
				metadata.RemoteImage{URL: sc.Paths.Screenshot, Type: metadata.ImagePoster},
				metadata.RemoteImage{URL: sc.Paths.Screenshot, Type: metadata.ImageBackdrop},
			)
		}
		if sc.Studio != nil && sc.Studio.ImagePath != "" {
			out = append(out, metadata.RemoteImage{URL: sc.Studio.ImagePath, Type: metadata.ImageLogo})
		}
	case "studio":
		st, err := s.client.FindStudio(ctx, raw)
		if err != nil {
			return nil, err
		}
		if st.ImagePath != "" {
			out = append(out,
				metadata.RemoteImage{URL: st.ImagePath, Type: metadata.ImagePoster},
				metadata.RemoteImage{URL: st.ImagePath, Type: metadata.ImageLogo},
			)
		}
	}
	return out, nil
}

// ResolveImage implements provider.Source. Stash URLs are absolute (typically
// pointing at the Stash host); we URL-decode the stored path.
//
// The decoded value originates from an upstream API response, so we constrain
// it to a valid absolute http(s) URL before handing it to the host. Plain HTTP
// is permitted here because Stash is commonly self-hosted on a LAN without TLS.
func (s *Source) ResolveImage(role, rawPath, variant string) string {
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return ""
	}
	return provider.SanitizeImageURL(decoded, false)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// DTO → metadata mappers
// ---------------------------------------------------------------------------

func sceneToSearchResult(sc sceneDTO) metadata.SearchResult {
	return metadata.SearchResult{
		Name:     sc.Title,
		Year:     provider.YearFromDate(sc.Date),
		ImageURL: provider.EncodeAbsolute(sc.Paths.Screenshot),
		Overview: sc.Details,
		ProviderIDs: map[string]string{
			Slug: "scene:" + sc.ID,
		},
	}
}

func studioToSearchResult(st studioDTO) metadata.SearchResult {
	return metadata.SearchResult{
		Name:     st.Name,
		Overview: st.Details,
		ImageURL: provider.EncodeAbsolute(st.ImagePath),
		ProviderIDs: map[string]string{
			Slug: "studio:" + st.ID,
		},
	}
}

func sceneToMetadata(sc sceneDTO) *metadata.MetadataResult {
	return &metadata.MetadataResult{
		HasMetadata:  true,
		Title:        sc.Title,
		Overview:     sc.Details,
		Year:         provider.YearFromDate(sc.Date),
		ReleaseDate:  sc.Date,
		Studios:      studiosFromStudio(sc.Studio),
		Genres:       provider.NamesToGenres(sc.Tags, func(t tagDTO) string { return t.Name }),
		People:       peopleFromScene(sc.Performers),
		PosterPath:   provider.EncodeAbsolute(sc.Paths.Screenshot),
		BackdropPath: provider.EncodeAbsolute(sc.Paths.Screenshot),
		ProviderIDs: map[string]string{
			Slug: "scene:" + sc.ID,
		},
	}
}

func studioToMetadata(st studioDTO) *metadata.MetadataResult {
	studios := make([]string, 0, 1)
	if st.ParentStudio != nil && st.ParentStudio.Name != "" {
		studios = append(studios, st.ParentStudio.Name)
	}
	return &metadata.MetadataResult{
		HasMetadata: true,
		Title:       st.Name,
		Overview:    st.Details,
		SeasonCount: 1,
		Studios:     studios,
		PosterPath:  provider.EncodeAbsolute(st.ImagePath),
		LogoPath:    provider.EncodeAbsolute(st.ImagePath),
		ProviderIDs: map[string]string{
			Slug: "studio:" + st.ID,
		},
	}
}

func sceneToEpisode(sc sceneDTO, seasonNumber, episodeNumber int) metadata.EpisodeResult {
	return metadata.EpisodeResult{
		ContentID:     "scene:" + sc.ID,
		SeasonNumber:  seasonNumber,
		EpisodeNumber: episodeNumber,
		Title:         sc.Title,
		Overview:      sc.Details,
		AirDate:       sc.Date,
		StillPath:     provider.EncodeAbsolute(sc.Paths.Screenshot),
		ProviderIDs: map[string]string{
			Slug: "scene:" + sc.ID,
		},
	}
}

func studiosFromStudio(st *studioDTO) []string {
	if st == nil {
		return nil
	}
	out := make([]string, 0, 2)
	if st.Name != "" {
		out = append(out, st.Name)
	}
	if st.ParentStudio != nil && st.ParentStudio.Name != "" && st.ParentStudio.Name != st.Name {
		out = append(out, st.ParentStudio.Name)
	}
	return out
}

func peopleFromScene(performers []performerDTO) []models.ItemPerson {
	people := make([]models.ItemPerson, 0, len(performers))
	for i, p := range performers {
		name := p.Name
		if p.Disambiguation != "" {
			name = p.Name + " (" + p.Disambiguation + ")"
		}
		people = append(people, models.ItemPerson{
			Person: models.Person{
				Name:      name,
				StashID:   p.ID,
				PhotoPath: provider.EncodeAbsolute(p.ImagePath),
			},
			Kind:      models.PersonKindActor,
			SortOrder: i,
		})
	}
	return people
}
