# Continuum Adult Metadata Plugin

First-party Continuum metadata plugin for adult content. Registers a single
`metadata_provider.v1` capability and fans out lookups across whichever
upstream sources the operator has enabled, in priority order.

The plugin is intentionally headless — there is no SPA, no library catalog
of its own, and no playback wiring. The host treats it the same as
`continuum-plugin-tmdb` and `continuum-plugin-tvdb`: a metadata provider
the operator can attach to any movie or TV library.

## Content mapping

| Upstream entity            | Host content type |
|----------------------------|-------------------|
| TPDB movie                 | Movie             |
| TPDB / Stash scene         | Movie or Episode  |
| TPDB site / Stash studio   | Series            |
| TPDB / Stash performer     | Person            |

Sites and studios are exposed as TV series with a single synthetic season.
Scenes resolved under a parent site/studio become episodes ordered by
release date. Scenes resolved standalone are exposed as movies.

## Sources

Two sources ship today; both implement the small `provider.Source`
interface in `provider/source.go`.

**ThePornDB (TPDB)** — REST API at `api.theporndb.net`. Authenticated with a
bearer token, rate-limited client-side to ~120 req/min. Get an API key
from your TPDB account → API.

**Stash** — Self-hosted [Stash](https://stashapp.cc) GraphQL endpoint.
The plugin POSTs queries to `<your-stash>/graphql` and authenticates with
an `ApiKey` header. The URL field accepts the bare host (the plugin
appends `/graphql`) or the full endpoint.

Configure either or both via the plugin admin form in the Continuum host.
Each source card has Enabled / API key / Priority controls. Lower priority
values are preferred when multiple sources return candidates.

## Architecture

```
pluginv1.MetadataProvider gRPC
  └── main.go (proto ↔ internal types)
        └── provider.Aggregator
              ├── tpdb.Source  (REST)
              └── stash.Source (GraphQL)
```

`provider/aggregator.go` owns the fanout. `Search` runs all enabled sources
concurrently and merges results in priority order. The other RPCs route to
a specific source by inspecting the canonical `ProviderIDs["adult"]` key
(formatted `"<slug>:<id>"`, e.g. `"tpdb:scene:abc"`) or per-source keys
like `ProviderIDs["tpdb"]`.

Images are returned to the host as `adult://<slug>/<role>/<path>` URIs so
`ResolveImageURL` can dispatch back to the right source. Day-one sources
return absolute URLs and pass through on resolve; sources that need URL
construction can do so per-source without changing the host contract.

## Adding a new source

1. Create `provider/sources/<name>/` with `client.go`, `types.go`,
   `source.go`. Implement the `provider.Source` interface in `source.go`.
2. Add a config card to `manifest.json` under `global_config_schema`.
3. Add a constructor call in `main.go`'s `Configure` and add the new slug
   to the `sourceSlugFromProviderIDs` lookup list.

The existing TPDB and Stash sources are the reference shape.

## Dependency model

This repository consumes
`github.com/ContinuumApp/continuum-plugin-sdk` as a normal Go module
dependency. CI and release builds run with `GOWORK=off` and expect the SDK
version in `go.mod` to resolve from a published semver tag.

For local multi-repo development, use a temporary `replace` or a local
`go.work` pointing at `dev/github/continuum-plugin-sdk`. Do not commit
machine-local filesystem replaces as the supported release path.

## Development

```sh
go test ./...
go vet ./...
go build .
```

## License

`continuum-plugin-adult` is licensed under `AGPL-3.0-or-later`. See
[LICENSE](LICENSE).
