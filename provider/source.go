package provider

import (
	"context"

	"github.com/RXWatcher/continuum-plugin-adult/metadata"
)

// Source is the contract every upstream (TPDB, Stash, etc.) implements.
//
// Each source is responsible for its own network I/O, auth, rate limiting,
// and translation of upstream payloads into the shared metadata.* types.
//
// Sources do NOT prefix their image paths with a URL scheme — the aggregator
// wraps source-relative paths in adult://<slug>/<role>/<path> before handing
// them to the host. On ResolveImage, the aggregator strips the scheme and
// the source slug and passes the raw remainder back here.
type Source interface {
	// Slug is the stable short identifier used in provider IDs and image
	// URL prefixes (e.g. "tpdb", "stash").
	Slug() string

	// Name is the human-readable name shown in logs and config UI.
	Name() string

	// Enabled reports whether the source is configured well enough to be
	// queried. Sources with missing API keys / URLs should return false.
	Enabled() bool

	// Priority controls ordering when multiple sources are eligible.
	// Lower = preferred. Sources without an explicit priority should
	// return a high default (e.g. 100).
	Priority() int

	Search(ctx context.Context, query metadata.SearchQuery) ([]metadata.SearchResult, error)
	GetMetadata(ctx context.Context, req metadata.MetadataRequest) (*metadata.MetadataResult, error)
	GetPersonDetail(ctx context.Context, req metadata.PersonDetailRequest) (*metadata.PersonDetailResult, error)
	GetSeasons(ctx context.Context, req metadata.SeasonsRequest) ([]metadata.SeasonResult, error)
	GetEpisodes(ctx context.Context, req metadata.EpisodesRequest) ([]metadata.EpisodeResult, error)
	GetImages(ctx context.Context, req metadata.ImageRequest) ([]metadata.RemoteImage, error)

	// ResolveImage converts a source-relative path (the part after
	// adult://<slug>/<role>/) into an absolute HTTPS URL suitable for the
	// host to fetch or proxy. variant is the host-requested size hint
	// ("card", "featured", "full", or "original"); sources may ignore it
	// when the upstream returns a single fixed URL per asset.
	ResolveImage(role, rawPath, variant string) string
}
