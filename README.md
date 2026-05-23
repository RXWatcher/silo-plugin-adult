# Adult for Silo

`silo.adult` is a metadata provider plugin that aggregates adult content metadata from multiple upstream sources (ThePornDB and a self-hosted Stash instance ship today) and exposes them to the Silo host as a single `metadata_provider.v1` capability. Studios and sites are mapped to TV series, scenes resolved under a parent studio/site become episodes, and standalone full-length releases are exposed as movies.

## Category

Lives under **Video / Metadata** (`category: "Video/Metadata"` in `manifest.json`).

## Capabilities

| Type | ID | Purpose |
| --- | --- | --- |
| `metadata_provider.v1` | `adult` | Metadata aggregator for adult content. Implements Search, GetMetadata, GetSeasons, GetEpisodes, GetImages, GetPersonDetail, and ResolveImageURL(s). Default priority `5` for movie / series / season / episode. |

## Dependencies

Standalone. The plugin is consumed directly by the Silo host's metadata pipeline alongside other metadata providers (e.g. `silo-plugin-tmdb`, `silo-plugin-tvdb`, `silo-plugin-sports-fitness`). It has no SPA, no library catalog of its own, and no playback wiring.

## External services

- **ThePornDB** (`api.theporndb.net`) — REST API, bearer-token auth.
- **Stash** — operator-supplied self-hosted Stash GraphQL endpoint, `ApiKey` header auth.

Both are optional. Sources with missing or empty credentials are silently disabled at configure time — the operator may enable only the subset they have accounts for.

## Source providers

- **TPDB** (`provider/sources/tpdb/`) — REST client against `api.theporndb.net`. Handles scenes, sites, performers, and image URLs. Authenticated with a bearer token from the operator's TPDB account (`account > API`).
- **Stash** (`provider/sources/stash/`) — GraphQL client. POSTs queries to `<your-stash>/graphql`. The configured URL accepts either the bare host (the plugin appends `/graphql`) or the full endpoint. Handles scenes, studios, and performers from the operator's local Stash database.

Each source implements the `provider.Source` interface in `provider/source.go` (`Slug`, `Name`, `Enabled`, `Priority`, `Search`, `GetMetadata`, `GetPersonDetail`, `GetSeasons`, `GetEpisodes`, `GetImages`, `ResolveImage`). Adding a new source is a matter of dropping a package under `provider/sources/<name>/`, adding a config card to `manifest.json`, and wiring the constructor into `Configure` in `main.go`.

## Mapping

| Upstream entity | Host content type |
| --- | --- |
| TPDB movie | Movie |
| TPDB / Stash scene (standalone) | Movie |
| TPDB / Stash scene (under a site/studio) | Episode |
| TPDB site / Stash studio | Series (single synthetic season) |
| TPDB / Stash performer | Person |

Sites and studios become TV series with a single synthetic season; scenes underneath are ordered by release date and exposed as episodes. Scenes resolved standalone (no parent studio context) are exposed as movies. Provider IDs use a canonical `ProviderIDs["adult"] = "<slug>:<id>"` form (e.g. `"tpdb:scene:abc"`); per-source keys like `ProviderIDs["tpdb"]` are also accepted by the aggregator for routing.

The `provider.Aggregator` (`provider/aggregator.go`) owns fan-out: `Search` runs all enabled sources concurrently and merges results in priority order; all other RPCs route to the single source identified by the request's provider IDs.

## Configuration

Configured globally via the host admin UI from `global_config_schema` in `manifest.json`. Two cards are exposed today:

| Card | Fields | Notes |
| --- | --- | --- |
| `tpdb` — ThePornDB | `enabled` (switch), `api_key` (password / secret), `priority` (number) | API key from your TPDB account → API. |
| `stash` — Stash | `enabled` (switch), `url` (text), `api_key` (password / secret), `priority` (number) | GraphQL endpoint, e.g. `http://stash.local:9999/graphql`. API key from Stash `Settings > Security`. |

Lower `priority` values are preferred when multiple sources return candidates for the same query. Sources whose `enabled` is off or whose `api_key` (or `url`, for Stash) is empty are dropped at configure time without erroring.

There are no per-region or per-language settings; the host's request language is passed through to whichever source handles the call. Caching and rate limiting are handled inside each source client (TPDB is rate-limited client-side to roughly 120 req/min); there is no plugin-level TTL knob exposed in the manifest.

Images are returned to the host as opaque `adult://<slug>/<role>/<path>` URIs so `ResolveImageURL` / `ResolveImageURLs` can dispatch back to the source that produced them. Resolved URLs are advertised with a 24-hour TTL.

## Detailed docs

The existing top-of-tree notes (architecture diagram, "Adding a new source" recipe, dependency model for the SDK module) live in this README's git history and in source comments at:

- `provider/aggregator.go` — fan-out and routing rules
- `provider/source.go` — the `Source` interface contract (including the image URL scheme rules)
- `provider/sources/tpdb/source.go`, `provider/sources/stash/source.go` — reference source implementations
- `main.go` — proto ↔ internal type translation and config wiring

There is no separate `docs/` directory; design notes live alongside the code.

## Build and release

CI builds linux-amd64 binaries on push to main via the reusable workflow in [RXWatcher/silo-plugin-repository](https://github.com/RXWatcher/silo-plugin-repository) and publishes them to the catalog at [`./binaries/`](https://github.com/RXWatcher/silo-plugin-repository/tree/main/binaries).

Local build targets (see `Makefile`):

```sh
make build       # single binary for the host arch
make build-all   # linux/amd64, linux/arm64, darwin/arm64 into dist/
make test
make lint
```
