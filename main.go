// Package main is the entry point for the Silo Adult metadata plugin.
//
// The plugin is a multi-source metadata aggregator for adult content. It
// declares a single metadata_provider.v1 capability and routes Search /
// GetMetadata / etc. across the sources the user has enabled in config
// (ThePornDB ships day-one; Stash is a placeholder slot).
package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/RXWatcher/silo-plugin-adult/metadata"
	"github.com/RXWatcher/silo-plugin-adult/models"
	"github.com/RXWatcher/silo-plugin-adult/provider"
	"github.com/RXWatcher/silo-plugin-adult/provider/sources/stash"
	"github.com/RXWatcher/silo-plugin-adult/provider/sources/tpdb"
)

// version is set at build time via -ldflags "-X main.version=...".
var version string

const resolvedImageURLTTL = 24 * time.Hour

//go:embed manifest.json
var manifestJSON []byte

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer

	manifest   *pluginv1.PluginManifest
	aggregator *provider.Aggregator
}

type metadataServer struct {
	pluginv1.UnimplementedMetadataProviderServer
	runtime *runtimeServer
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

// Configure rebuilds the aggregator's source list from the host-supplied
// config map. Sources whose config is missing or whose api_key is empty are
// disabled rather than erroring — the user may have only configured a subset.
func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	cfgByKey := map[string]map[string]any{}
	for _, entry := range req.GetConfig() {
		if entry == nil || entry.GetValue() == nil {
			continue
		}
		cfgByKey[entry.GetKey()] = entry.GetValue().AsMap()
	}

	sources := []provider.Source{
		tpdb.New(tpdbConfigFromMap(cfgByKey["tpdb"])),
		stash.New(stashConfigFromMap(cfgByKey["stash"])),
	}
	s.aggregator.SetSources(sources)
	return &pluginv1.ConfigureResponse{}, nil
}

func tpdbConfigFromMap(m map[string]any) tpdb.Config {
	if m == nil {
		return tpdb.Config{}
	}
	return tpdb.Config{
		Enabled:  asBool(m["enabled"]),
		APIKey:   asString(m["api_key"]),
		Priority: asInt(m["priority"]),
	}
}

func stashConfigFromMap(m map[string]any) stash.Config {
	if m == nil {
		return stash.Config{}
	}
	return stash.Config{
		Enabled:  asBool(m["enabled"]),
		URL:      asString(m["url"]),
		APIKey:   asString(m["api_key"]),
		Priority: asInt(m["priority"]),
	}
}

// ---------------------------------------------------------------------------
// MetadataProvider implementations — straight pass-throughs to the aggregator.
// ---------------------------------------------------------------------------

func (s *metadataServer) Search(ctx context.Context, req *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	results, err := s.runtime.aggregator.Search(ctx, metadata.SearchQuery{
		Title:       req.GetQuery(),
		Year:        int(req.GetYear()),
		ContentType: req.GetItemType(),
		ProviderIDs: stringMapFromStruct(req.GetProviderIds()),
		Language:    req.GetLanguage(),
	})
	if err != nil {
		return nil, err
	}

	resp := &pluginv1.SearchMetadataResponse{
		Results: make([]*pluginv1.ProviderSearchResult, 0, len(results)),
	}
	for _, r := range results {
		providerIDs, err := stringStruct(r.ProviderIDs)
		if err != nil {
			return nil, err
		}
		resp.Results = append(resp.Results, &pluginv1.ProviderSearchResult{
			ProviderId:  r.ProviderIDs[provider.CapabilityID],
			ItemType:    req.GetItemType(),
			Title:       r.Name,
			Year:        int32(r.Year),
			Overview:    r.Overview,
			ProviderIds: providerIDs,
			ImageUrl:    canonicalImagePath(r.Provider, "poster", r.ImageURL),
		})
	}
	return resp, nil
}

func (s *metadataServer) GetMetadata(ctx context.Context, req *pluginv1.GetMetadataRequest) (*pluginv1.GetMetadataResponse, error) {
	result, err := s.runtime.aggregator.GetMetadata(ctx, metadata.MetadataRequest{
		ProviderIDs: providerIDsFromProto(req.GetProviderIds(), req.GetProviderId()),
		ContentType: req.GetItemType(),
		Language:    req.GetLanguage(),
		FilePath:    req.GetFilePath(),
	})
	if err != nil || result == nil {
		return nil, err
	}

	item, err := metadataItemFromResult(result, req.GetItemType())
	if err != nil {
		return nil, err
	}
	return &pluginv1.GetMetadataResponse{Item: item}, nil
}

func (s *metadataServer) GetPersonDetail(ctx context.Context, req *pluginv1.GetPersonDetailRequest) (*pluginv1.GetPersonDetailResponse, error) {
	result, err := s.runtime.aggregator.GetPersonDetail(ctx, metadata.PersonDetailRequest{
		ProviderIDs: stringMapFromStruct(req.GetProviderIds()),
		Language:    req.GetLanguage(),
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &pluginv1.GetPersonDetailResponse{}, nil
	}

	providerIDs, err := stringStruct(result.ProviderIDs)
	if err != nil {
		return nil, err
	}
	source := sourceSlugFromProviderIDs(result.ProviderIDs)
	return &pluginv1.GetPersonDetailResponse{Person: &pluginv1.PersonDetailRecord{
		Name:        result.Name,
		Bio:         result.Bio,
		BirthDate:   result.BirthDate,
		DeathDate:   result.DeathDate,
		Birthplace:  result.Birthplace,
		Homepage:    result.Homepage,
		PhotoPath:   canonicalImagePath(source, "profile", result.PhotoPath),
		ProviderIds: providerIDs,
	}}, nil
}

func (s *metadataServer) GetSeasons(ctx context.Context, req *pluginv1.GetSeasonsRequest) (*pluginv1.GetSeasonsResponse, error) {
	results, err := s.runtime.aggregator.GetSeasons(ctx, metadata.SeasonsRequest{
		ProviderIDs: providerIDsFromProto(req.GetProviderIds(), req.GetSeriesProviderId()),
		ContentType: "series",
		Language:    req.GetLanguage(),
	})
	if err != nil {
		return nil, err
	}

	source := sourceSlugFromProviderIDs(stringMapFromStruct(req.GetProviderIds()))
	resp := &pluginv1.GetSeasonsResponse{
		Seasons: make([]*pluginv1.SeasonRecord, 0, len(results)),
	}
	for _, r := range results {
		providerIDs, err := stringStruct(map[string]string{source: r.ContentID})
		if err != nil {
			return nil, err
		}
		resp.Seasons = append(resp.Seasons, &pluginv1.SeasonRecord{
			ProviderId:   r.ContentID,
			ProviderIds:  providerIDs,
			SeasonNumber: int32(r.SeasonNumber),
			Title:        r.Title,
			Overview:     r.Overview,
			AirDate:      r.AirDate,
			PosterPath:   canonicalImagePath(source, "poster", r.PosterPath),
		})
	}
	return resp, nil
}

func (s *metadataServer) GetEpisodes(ctx context.Context, req *pluginv1.GetEpisodesRequest) (*pluginv1.GetEpisodesResponse, error) {
	results, err := s.runtime.aggregator.GetEpisodes(ctx, metadata.EpisodesRequest{
		ProviderIDs:  providerIDsFromProto(req.GetProviderIds(), req.GetSeriesProviderId()),
		SeasonNumber: int(req.GetSeasonNumber()),
		Language:     req.GetLanguage(),
	})
	if err != nil {
		return nil, err
	}

	source := sourceSlugFromProviderIDs(stringMapFromStruct(req.GetProviderIds()))
	resp := &pluginv1.GetEpisodesResponse{
		Episodes: make([]*pluginv1.EpisodeRecord, 0, len(results)),
	}
	for _, r := range results {
		providerIDs, err := stringStruct(r.ProviderIDs)
		if err != nil {
			return nil, err
		}
		resp.Episodes = append(resp.Episodes, &pluginv1.EpisodeRecord{
			ProviderId:    r.ContentID,
			SeasonNumber:  int32(r.SeasonNumber),
			EpisodeNumber: int32(r.EpisodeNumber),
			Title:         r.Title,
			Overview:      r.Overview,
			AirDate:       r.AirDate,
			Runtime:       int32(r.Runtime),
			StillPath:     canonicalImagePath(source, "still", r.StillPath),
			ProviderIds:   providerIDs,
		})
	}
	return resp, nil
}

func (s *metadataServer) GetImages(ctx context.Context, req *pluginv1.GetImagesRequest) (*pluginv1.GetImagesResponse, error) {
	images, err := s.runtime.aggregator.GetImages(ctx, metadata.ImageRequest{
		ProviderIDs: providerIDsFromProto(req.GetProviderIds(), req.GetProviderId()),
		ContentType: req.GetItemType(),
		Language:    req.GetLanguage(),
	})
	if err != nil {
		return nil, err
	}

	source := sourceSlugFromProviderIDs(stringMapFromStruct(req.GetProviderIds()))
	resp := &pluginv1.GetImagesResponse{}
	for _, img := range images {
		role := imageRole(img.Type)
		resp.Images = append(resp.Images, &pluginv1.ImageRecord{
			Kind:     role,
			Url:      canonicalImagePath(source, role, img.URL),
			Language: img.Language,
			Width:    int32(img.Width),
			Height:   int32(img.Height),
		})
	}
	return resp, nil
}

func (s *metadataServer) ResolveImageURL(_ context.Context, req *pluginv1.ResolveImageURLRequest) (*pluginv1.ResolveImageURLResponse, error) {
	url := s.runtime.aggregator.ResolveImage(req.GetPath(), req.GetVariant())
	return &pluginv1.ResolveImageURLResponse{
		Url:       url,
		ExpiresAt: timestamppb.New(time.Now().Add(resolvedImageURLTTL)),
	}, nil
}

func (s *metadataServer) ResolveImageURLs(_ context.Context, req *pluginv1.ResolveImageURLsRequest) (*pluginv1.ResolveImageURLsResponse, error) {
	urls := make(map[string]string, len(req.GetPaths()))
	resolved := make(map[string]*pluginv1.ResolvedImageURL, len(req.GetPaths()))
	expiresAt := timestamppb.New(time.Now().Add(resolvedImageURLTTL))
	for _, path := range req.GetPaths() {
		u := s.runtime.aggregator.ResolveImage(path, req.GetVariant())
		urls[path] = u
		resolved[path] = &pluginv1.ResolvedImageURL{
			Url:       u,
			ExpiresAt: expiresAt,
		}
	}
	return &pluginv1.ResolveImageURLsResponse{Urls: urls, ResolvedUrls: resolved}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func canonicalImagePath(source, role, path string) string {
	if source == "" || path == "" {
		return ""
	}
	return provider.EncodeImagePath(source, role, path)
}

func imageRole(t metadata.ImageType) string {
	switch t {
	case metadata.ImagePoster:
		return "poster"
	case metadata.ImageBackdrop:
		return "backdrop"
	case metadata.ImageLogo:
		return "logo"
	case metadata.ImageStill:
		return "still"
	default:
		return ""
	}
}

// sourceSlugFromProviderIDs picks the source slug out of a ProviderIDs map by
// checking the canonical "adult" key first, then known per-source keys.
// Returns empty string if nothing matches.
func sourceSlugFromProviderIDs(ids map[string]string) string {
	if encoded, ok := ids[provider.CapabilityID]; ok && encoded != "" {
		if slug, _, ok := provider.DecodeProviderID(encoded); ok {
			return slug
		}
	}
	for _, slug := range []string{tpdb.Slug, stash.Slug} {
		if _, ok := ids[slug]; ok {
			return slug
		}
	}
	return ""
}

func metadataItemFromResult(result *metadata.MetadataResult, itemType string) (*pluginv1.MetadataItem, error) {
	providerIDs, err := stringStruct(result.ProviderIDs)
	if err != nil {
		return nil, err
	}
	source := sourceSlugFromProviderIDs(result.ProviderIDs)
	return &pluginv1.MetadataItem{
		ProviderId:        result.ProviderIDs[provider.CapabilityID],
		ItemType:          itemType,
		Title:             result.Title,
		OriginalTitle:     result.OriginalTitle,
		SortTitle:         result.SortTitle,
		Year:              int32(result.Year),
		Overview:          result.Overview,
		Tagline:           result.Tagline,
		Runtime:           int32(result.Runtime),
		Genres:            append([]string(nil), result.Genres...),
		Studios:           append([]string(nil), result.Studios...),
		Networks:          append([]string(nil), result.Networks...),
		Countries:         append([]string(nil), result.Countries...),
		OriginalLanguage:  result.OriginalLanguage,
		ContentRating:     result.ContentRating,
		ProviderIds:       providerIDs,
		PosterPath:        canonicalImagePath(source, "poster", result.PosterPath),
		PosterThumbhash:   result.PosterThumbhash,
		BackdropPath:      canonicalImagePath(source, "backdrop", result.BackdropPath),
		BackdropThumbhash: result.BackdropThumbhash,
		LogoPath:          canonicalImagePath(source, "logo", result.LogoPath),
		SeasonCount:       int32(result.SeasonCount),
		FirstAirDate:      result.FirstAirDate,
		LastAirDate:       result.LastAirDate,
		ReleaseDate:       result.ReleaseDate,
		People:            peopleToRecords(result.People, source),
	}, nil
}

func peopleToRecords(people []models.ItemPerson, source string) []*pluginv1.PersonRecord {
	if len(people) == 0 {
		return nil
	}
	records := make([]*pluginv1.PersonRecord, 0, len(people))
	for _, p := range people {
		records = append(records, &pluginv1.PersonRecord{
			Name:           p.Name,
			Kind:           p.Kind.String(),
			Character:      p.Character,
			SortOrder:      int32(p.SortOrder),
			TmdbId:         p.TmdbID,
			TvdbId:         p.TvdbID,
			ImdbId:         p.ImdbID,
			PlexGuid:       p.PlexGUID,
			PhotoPath:      canonicalImagePath(source, "profile", p.PhotoPath),
			PhotoThumbhash: p.PhotoThumbhash,
		})
	}
	return records
}

func providerIDsFromProto(value *structpb.Struct, fallbackID string) map[string]string {
	result := stringMapFromStruct(value)
	if fallbackID != "" && result[provider.CapabilityID] == "" {
		result[provider.CapabilityID] = fallbackID
	}
	return result
}

func stringMapFromStruct(value *structpb.Struct) map[string]string {
	result := make(map[string]string)
	if value == nil {
		return result
	}
	for key, raw := range value.AsMap() {
		if text, ok := raw.(string); ok && text != "" {
			result[key] = text
		}
	}
	return result
}

func stringStruct(value map[string]string) (*structpb.Struct, error) {
	if len(value) == 0 {
		return nil, nil
	}
	converted := make(map[string]any, len(value))
	for k, v := range value {
		if v == "" {
			continue
		}
		converted[k] = v
	}
	if len(converted) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(converted)
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	if v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// ---------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}

	rs := &runtimeServer{
		manifest:   manifest,
		aggregator: provider.NewAggregator(),
	}

	runtime.Serve(runtime.ServeConfig{
		Servers: runtime.CapabilityServers{
			Runtime:          rs,
			MetadataProvider: &metadataServer{runtime: rs},
		},
	})
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if version != "" {
		manifest.Version = version
	}
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])
	return manifest, nil
}
