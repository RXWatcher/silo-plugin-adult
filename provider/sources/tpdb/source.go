// Package tpdb implements the provider.Source interface backed by
// ThePornDB (api.theporndb.net).
//
// TPDB exposes three top-level item kinds — scenes, movies, and sites — plus
// performers. This source maps:
//
//	TPDB movie    → host movie
//	TPDB scene    → host movie (when standalone) / host episode (when fetched
//	                under a site)
//	TPDB site     → host series (with a synthetic single season)
//	TPDB performer → host person
//
// Source-local IDs are namespaced: "movie:<uuid>", "scene:<uuid>",
// "site:<id>", "performer:<uuid>". The namespace lets GetMetadata route to
// the right TPDB endpoint without re-querying.
package tpdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/RXWatcher/silo-plugin-adult/metadata"
	"github.com/RXWatcher/silo-plugin-adult/models"
	"github.com/RXWatcher/silo-plugin-adult/provider"
	"github.com/RXWatcher/silo-plugin-adult/provider/logging"
)

// Slug is the source identifier used in ProviderIDs maps and image URLs.
const Slug = "tpdb"

// Source implements provider.Source for ThePornDB.
type Source struct {
	client   *Client
	enabled  bool
	priority int
}

// Config holds the user-provided configuration for the TPDB source.
type Config struct {
	APIKey   string
	Enabled  bool
	Priority int
}

// New returns a configured TPDB source. If cfg.APIKey is empty the source
// reports Enabled() == false regardless of cfg.Enabled.
func New(cfg Config) *Source {
	enabled := cfg.Enabled && cfg.APIKey != ""
	priority := cfg.Priority
	if priority <= 0 {
		priority = 10
	}
	return &Source{
		client:   NewClient(cfg.APIKey),
		enabled:  enabled,
		priority: priority,
	}
}

// Slug implements provider.Source.
func (s *Source) Slug() string { return Slug }

// Name implements provider.Source.
func (s *Source) Name() string { return "ThePornDB" }

// Enabled implements provider.Source.
func (s *Source) Enabled() bool { return s.enabled }

// Priority implements provider.Source.
func (s *Source) Priority() int { return s.priority }

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// Search implements provider.Source. The search strategy depends on the
// requested content type:
//
//	"movie": queries both /movies and /scenes; many on-disk files are
//	         scene-format treated as movies by the host.
//	"series": queries /sites.
func (s *Source) Search(ctx context.Context, query metadata.SearchQuery) ([]metadata.SearchResult, error) {
	if id, ok := query.ProviderIDs[Slug]; ok && id != "" {
		return s.searchByLocalID(ctx, id)
	}
	if query.Title == "" {
		return nil, nil
	}

	switch query.ContentType {
	case "series":
		return s.searchSites(ctx, query)
	default:
		return s.searchMoviesAndScenes(ctx, query)
	}
}

func (s *Source) searchByLocalID(ctx context.Context, id string) ([]metadata.SearchResult, error) {
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok {
		return nil, fmt.Errorf("tpdb: unrecognised id %q", id)
	}
	switch kind {
	case "movie":
		m, err := s.client.GetMovie(ctx, raw)
		if err != nil {
			return nil, err
		}
		return []metadata.SearchResult{movieToSearchResult(*m)}, nil
	case "scene":
		sc, err := s.client.GetScene(ctx, raw)
		if err != nil {
			return nil, err
		}
		return []metadata.SearchResult{sceneToSearchResult(*sc)}, nil
	case "site":
		st, err := s.client.GetSite(ctx, raw)
		if err != nil {
			return nil, err
		}
		return []metadata.SearchResult{siteToSearchResult(*st)}, nil
	}
	return nil, fmt.Errorf("tpdb: unsupported id kind %q", kind)
}

func (s *Source) searchMoviesAndScenes(ctx context.Context, query metadata.SearchQuery) ([]metadata.SearchResult, error) {
	movies, mErr := s.client.SearchMovies(ctx, query.Title, query.Year)
	scenes, sErr := s.client.SearchScenes(ctx, query.Title, query.Year)
	if mErr != nil && sErr != nil {
		return nil, errors.Join(mErr, sErr)
	}

	results := make([]metadata.SearchResult, 0, len(movies)+len(scenes))
	for _, m := range movies {
		results = append(results, movieToSearchResult(m))
	}
	for _, sc := range scenes {
		results = append(results, sceneToSearchResult(sc))
	}
	return results, nil
}

func (s *Source) searchSites(ctx context.Context, query metadata.SearchQuery) ([]metadata.SearchResult, error) {
	sites, err := s.client.SearchSites(ctx, query.Title)
	if err != nil {
		return nil, err
	}
	out := make([]metadata.SearchResult, 0, len(sites))
	for _, st := range sites {
		out = append(out, siteToSearchResult(st))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// GetMetadata
// ---------------------------------------------------------------------------

// GetMetadata implements provider.Source. Dispatches by the prefix on the
// source-local ID (movie:/scene:/site:).
func (s *Source) GetMetadata(ctx context.Context, req metadata.MetadataRequest) (*metadata.MetadataResult, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("tpdb: missing source id")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok {
		return nil, fmt.Errorf("tpdb: unrecognised id %q", id)
	}
	switch kind {
	case "movie":
		m, err := s.client.GetMovie(ctx, raw)
		if err != nil {
			return nil, err
		}
		return movieToMetadata(*m), nil
	case "scene":
		sc, err := s.client.GetScene(ctx, raw)
		if err != nil {
			return nil, err
		}
		return sceneToMetadata(*sc), nil
	case "site":
		st, err := s.client.GetSite(ctx, raw)
		if err != nil {
			return nil, err
		}
		return siteToMetadata(*st), nil
	}
	return nil, fmt.Errorf("tpdb: unsupported id kind %q", kind)
}

// ---------------------------------------------------------------------------
// GetSeasons / GetEpisodes
// ---------------------------------------------------------------------------

// GetSeasons implements provider.Source. TPDB sites don't have seasons, so
// we return a single synthetic Season 1 covering all the site's scenes.
func (s *Source) GetSeasons(ctx context.Context, req metadata.SeasonsRequest) ([]metadata.SeasonResult, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("tpdb: missing source id for seasons")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok || kind != "site" {
		return nil, fmt.Errorf("tpdb: GetSeasons requires a site id, got %q", id)
	}
	st, err := s.client.GetSite(ctx, raw)
	if err != nil {
		return nil, err
	}
	return []metadata.SeasonResult{{
		ContentID:    "site:" + strconv.Itoa(st.ID) + ":season:1",
		SeasonNumber: 1,
		Title:        st.Name,
		Overview:     st.Description,
		AirDate:      st.Date,
		PosterPath:   provider.EncodeAbsolute(st.Poster),
	}}, nil
}

// maxEpisodesPerSite caps how many scenes we'll walk for a single site. The
// pagination loop stops earlier when TPDB returns a short page.
const maxEpisodesPerSite = 1000

// GetEpisodes implements provider.Source. Walks /scenes?site_id=... across
// pages and builds the full episode list ordered by date.
func (s *Source) GetEpisodes(ctx context.Context, req metadata.EpisodesRequest) ([]metadata.EpisodeResult, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("tpdb: missing source id for episodes")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok || kind != "site" {
		return nil, fmt.Errorf("tpdb: GetEpisodes requires a site id, got %q", id)
	}

	const perPage = 100
	out := make([]metadata.EpisodeResult, 0, perPage)
	for page := 1; len(out) < maxEpisodesPerSite; page++ {
		scenes, err := s.client.ListScenesForSite(ctx, raw, page, perPage)
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
	if len(out) >= maxEpisodesPerSite {
		// The loop stopped on the cap, not a short page, so the site may have
		// more scenes that were silently dropped. Surface that for operators.
		logging.L().Warn("tpdb: episode cap reached, results truncated",
			"site_id", raw,
			"cap", maxEpisodesPerSite,
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
		return nil, errors.New("tpdb: missing performer id")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok || kind != "performer" {
		raw = id
	}
	p, err := s.client.GetPerformer(ctx, raw)
	if err != nil {
		return nil, err
	}
	return &metadata.PersonDetailResult{
		Name:       p.Name,
		Bio:        p.Bio,
		BirthDate:  p.BirthDate,
		DeathDate:  p.DeathDate,
		Birthplace: p.Birthplace,
		Homepage:   p.Homepage,
		PhotoPath:  provider.EncodeAbsolute(p.Image),
		ProviderIDs: map[string]string{
			Slug: "performer:" + p.ID,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Images / ResolveImage
// ---------------------------------------------------------------------------

// GetImages implements provider.Source. TPDB attaches at most a poster and
// background per scene/movie/site, so this returns those.
func (s *Source) GetImages(ctx context.Context, req metadata.ImageRequest) ([]metadata.RemoteImage, error) {
	id := req.ProviderIDs[Slug]
	if id == "" {
		return nil, errors.New("tpdb: missing source id for images")
	}
	kind, raw, ok := provider.ParseLocalID(id)
	if !ok {
		return nil, fmt.Errorf("tpdb: unrecognised id %q", id)
	}
	var (
		poster, background string
	)
	switch kind {
	case "movie":
		m, err := s.client.GetMovie(ctx, raw)
		if err != nil {
			return nil, err
		}
		poster, background = m.Poster, firstNonEmpty(m.Background.URL, m.Background.Large, m.Background.Medium)
	case "scene":
		sc, err := s.client.GetScene(ctx, raw)
		if err != nil {
			return nil, err
		}
		poster, background = sc.Poster, firstNonEmpty(sc.Background.URL, sc.Background.Large, sc.Background.Medium)
	case "site":
		st, err := s.client.GetSite(ctx, raw)
		if err != nil {
			return nil, err
		}
		poster = firstNonEmpty(st.Poster, st.Logo)
	}

	out := make([]metadata.RemoteImage, 0, 2)
	if poster != "" {
		out = append(out, metadata.RemoteImage{URL: poster, Type: metadata.ImagePoster})
	}
	if background != "" {
		out = append(out, metadata.RemoteImage{URL: background, Type: metadata.ImageBackdrop})
	}
	return out, nil
}

// ResolveImage implements provider.Source. TPDB image URLs are already
// absolute, so the "path" we store is just the URL-encoded original URL.
// variant is ignored — TPDB does not expose size variants on a single asset.
//
// The decoded value originates from an upstream API response, so we constrain
// it to a valid absolute https URL before handing it to the host; anything
// else is dropped rather than letting the host fetch an arbitrary target.
func (s *Source) ResolveImage(role, rawPath, variant string) string {
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return ""
	}
	return provider.SanitizeImageURL(decoded, true)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// DTO → metadata mappers
// ---------------------------------------------------------------------------

func movieToSearchResult(m movieDTO) metadata.SearchResult {
	return metadata.SearchResult{
		Name:     m.Title,
		Year:     provider.YearFromDate(m.Date),
		ImageURL: provider.EncodeAbsolute(m.Poster),
		Overview: m.Description,
		ProviderIDs: map[string]string{
			Slug: "movie:" + m.ID,
		},
	}
}

func sceneToSearchResult(sc sceneDTO) metadata.SearchResult {
	return metadata.SearchResult{
		Name:     sc.Title,
		Year:     provider.YearFromDate(sc.Date),
		ImageURL: provider.EncodeAbsolute(sc.Poster),
		Overview: sc.Description,
		ProviderIDs: map[string]string{
			Slug: "scene:" + sc.ID,
		},
	}
}

func siteToSearchResult(st siteDTO) metadata.SearchResult {
	return metadata.SearchResult{
		Name:     st.Name,
		Year:     provider.YearFromDate(st.Date),
		ImageURL: provider.EncodeAbsolute(firstNonEmpty(st.Poster, st.Logo)),
		Overview: st.Description,
		ProviderIDs: map[string]string{
			Slug: "site:" + strconv.Itoa(st.ID),
		},
	}
}

func movieToMetadata(m movieDTO) *metadata.MetadataResult {
	return &metadata.MetadataResult{
		HasMetadata: true,
		Title:       m.Title,
		Overview:    m.Description,
		Year:        provider.YearFromDate(m.Date),
		Runtime:     m.Duration / 60,
		ReleaseDate: m.Date,
		Studios:     studiosFromSite(m.Site),
		Genres:      provider.NamesToGenres(m.Tags, func(t tagDTO) string { return t.Name }),
		People:      peopleFromScene(m.Performers, m.Directors),
		PosterPath:  provider.EncodeAbsolute(m.Poster),
		BackdropPath: provider.EncodeAbsolute(firstNonEmpty(
			m.Background.URL, m.Background.Large, m.Background.Medium,
		)),
		ProviderIDs: map[string]string{
			Slug: "movie:" + m.ID,
		},
	}
}

func sceneToMetadata(sc sceneDTO) *metadata.MetadataResult {
	return &metadata.MetadataResult{
		HasMetadata: true,
		Title:       sc.Title,
		Overview:    sc.Description,
		Year:        provider.YearFromDate(sc.Date),
		Runtime:     sc.Duration / 60,
		ReleaseDate: sc.Date,
		Studios:     studiosFromSite(sc.Site),
		Genres:      provider.NamesToGenres(sc.Tags, func(t tagDTO) string { return t.Name }),
		People:      peopleFromScene(sc.Performers, sc.Directors),
		PosterPath:  provider.EncodeAbsolute(sc.Poster),
		BackdropPath: provider.EncodeAbsolute(firstNonEmpty(
			sc.Background.URL, sc.Background.Large, sc.Background.Medium,
		)),
		ProviderIDs: map[string]string{
			Slug: "scene:" + sc.ID,
		},
	}
}

func siteToMetadata(st siteDTO) *metadata.MetadataResult {
	return &metadata.MetadataResult{
		HasMetadata:  true,
		Title:        st.Name,
		Overview:     st.Description,
		Year:         provider.YearFromDate(st.Date),
		FirstAirDate: st.Date,
		LastAirDate:  st.LastScene,
		SeasonCount:  1,
		Studios:      []string{st.Network},
		PosterPath:   provider.EncodeAbsolute(firstNonEmpty(st.Poster, st.Logo)),
		LogoPath:     provider.EncodeAbsolute(st.Logo),
		ProviderIDs: map[string]string{
			Slug: "site:" + strconv.Itoa(st.ID),
		},
	}
}

func sceneToEpisode(sc sceneDTO, seasonNumber, episodeNumber int) metadata.EpisodeResult {
	return metadata.EpisodeResult{
		ContentID:     "scene:" + sc.ID,
		SeasonNumber:  seasonNumber,
		EpisodeNumber: episodeNumber,
		Title:         sc.Title,
		Overview:      sc.Description,
		AirDate:       sc.Date,
		Runtime:       sc.Duration / 60,
		StillPath:     provider.EncodeAbsolute(sc.Poster),
		ProviderIDs: map[string]string{
			Slug: "scene:" + sc.ID,
		},
	}
}

func studiosFromSite(ref siteRefDTO) []string {
	out := make([]string, 0, 2)
	if ref.Name != "" {
		out = append(out, ref.Name)
	}
	if ref.Network != "" && ref.Network != ref.Name {
		out = append(out, ref.Network)
	}
	return out
}

func peopleFromScene(performers []performerDTO, directors []directorDTO) []models.ItemPerson {
	people := make([]models.ItemPerson, 0, len(performers)+len(directors))
	for i, p := range performers {
		people = append(people, models.ItemPerson{
			Person: models.Person{
				Name:      p.Name,
				TpdbID:    p.ID,
				PhotoPath: provider.EncodeAbsolute(p.Image),
			},
			Kind:      models.PersonKindActor,
			SortOrder: i,
		})
	}
	for i, d := range directors {
		people = append(people, models.ItemPerson{
			Person: models.Person{
				Name:   d.Name,
				TpdbID: d.ID,
			},
			Kind:      models.PersonKindDirector,
			SortOrder: i,
		})
	}
	return people
}
