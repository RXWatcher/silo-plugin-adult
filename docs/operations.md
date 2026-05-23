# Operations & Debugging

Operator-facing notes for `silo.adult`. Pairs with the top-level `README.md`
(which covers what the plugin is, capabilities, and configuration shape). This
document focuses on day-2 concerns: how to tell whether a source is healthy, how
the aggregator decides who answers a request, and what specific failure
signatures from each upstream actually mean.

## Source lifecycle

Each call to the host's `Configure` RPC rebuilds the source list from scratch
(see `main.go` → `Configure` → `provider.Aggregator.SetSources`). For a given
source to make it into the live list:

- `enabled` must be `true` in the card.
- The source's credential / URL must be non-empty:
  - **TPDB**: `api_key` non-empty (`provider/sources/tpdb/source.go` → `New`).
  - **Stash**: `url` non-empty (`provider/sources/stash/source.go` → `New`).
    `api_key` is optional — some Stash deployments accept anonymous queries.
- The source's `Enabled()` must return true. `SetSources` silently drops the
  rest.

This means **flipping the `enabled` switch alone is not enough**: clearing the
API key (or URL for Stash) also disables the source on the next configure pass,
and a card whose switch is on but whose credential is blank stays disabled
without an error surfaced in the UI. If a source seems "stuck off" after toggle,
re-save the card to force a fresh `Configure` round-trip.

Default priorities when the operator leaves the field at 0:

| Source | Default priority |
| --- | --- |
| TPDB | 10 |
| Stash | 20 |

Lower wins. Within a single source, results retain the upstream's own ordering.

## Aggregator routing model

The aggregator (`provider/aggregator.go`) has two distinct modes:

1. **Fan-out (Search only)**: every enabled source runs concurrently in its
   own goroutine. Results are stitched together in priority order; per-source
   ordering is preserved within its slice. If *every* source errors, the first
   error wins (wrapped with the slug). If at least one source returns
   successfully (even with zero hits), the merged list is returned and other
   sources' errors are swallowed — they will not appear in the host log path
   for this call. Check the per-source client log lines if a result you expect
   from one source never shows up.
2. **Routed (everything else)**: `GetMetadata`, `GetPersonDetail`,
   `GetSeasons`, `GetEpisodes`, `GetImages` all require a provider ID and
   route to exactly one source. The lookup checks:
   - `ProviderIDs["adult"]` first, parsed as `<slug>:<id>` (canonical form).
   - Then any per-source key like `ProviderIDs["tpdb"]` or
     `ProviderIDs["stash"]`.

   If neither resolves to an enabled source, the call fails with
   `adult: no source matches request (missing or unknown provider id)` (or the
   equivalent for the other RPCs). This is the most common "everything went
   silent" symptom after disabling a source whose IDs still live in the host's
   item DB.

`Search` *can* also be routed: if the search query carries a provider ID, the
fan-out is skipped and only that source is queried. This is what powers the
"re-identify with this specific provider" flow.

## Provider ID shapes (cheat sheet)

When debugging a routing miss, dump the `ProviderIDs` map on the affected item
and check against:

| Source | Local ID format | Canonical (`adult` key) |
| --- | --- | --- |
| TPDB movie | `movie:<uuid>` | `tpdb:movie:<uuid>` |
| TPDB scene | `scene:<uuid>` | `tpdb:scene:<uuid>` |
| TPDB site | `site:<int>` | `tpdb:site:<int>` |
| TPDB performer | `performer:<uuid>` | `tpdb:performer:<uuid>` |
| Stash scene | `scene:<id>` | `stash:scene:<id>` |
| Stash studio | `studio:<id>` | `stash:studio:<id>` |
| Stash performer | `performer:<id>` | `stash:performer:<id>` |

The slug prefix (`movie:`, `scene:`, etc.) is mandatory for routing —
`GetMetadata` dispatches off it. Strip it and the source returns
`tpdb: unrecognised id "..."` / `stash: unrecognised id "..."`.

## TPDB upstream

- Base URL: `https://api.theporndb.net` (constant, not configurable).
- Auth: `Authorization: Bearer <api_key>` from the operator's account page →
  API.
- Client-side rate limiter: `golang.org/x/time/rate` at **1.8 rps with burst
  4**, intentionally below TPDB's documented 120 req/min so retries have
  headroom (see `provider/sources/tpdb/client.go` → `defaultRPS`). The limiter
  is per-client / per-source instance, not global; re-saving config creates a
  fresh limiter so its token bucket resets.
- HTTP timeout: 30 s per request.
- Endpoints used: `/scenes`, `/movies`, `/sites`, `/performers`,
  `/scenes?site_id=...` (paginated, 100 per page, ordered `date_asc`).
- Episode walk cap: 1000 scenes per site (`maxEpisodesPerSite`). Long-running
  series get truncated; if an operator complains about missing episodes for a
  high-volume site, this is the knob.

Failure signatures:

| Symptom | Likely cause |
| --- | --- |
| `tpdb: /scenes?... returned 401: ...` | API key revoked or wrong. Regenerate at theporndb.net → account → API. |
| `tpdb: /scenes?... returned 429: ...` | The 1.8 rps limit was bypassed (parallel hosts sharing one key, or a burst > 4 between calls). Lower TPDB priority so it's hit less, or wait. |
| `tpdb: not found` (`ErrNotFound`) | Bare 404 from upstream. Returned by GET-by-id for an item that does not exist. Aggregator surfaces this as a `GetMetadata` error to the host. |
| Context deadline exceeded | TPDB latency spike. The 30 s timeout is set on the client; if the host's own RPC deadline is shorter the host cancels first. |

## Stash upstream

- URL: operator-supplied. The client appends `/graphql` if the path doesn't
  already contain it, so `http://stash.local:9999` and
  `http://stash.local:9999/graphql` both work.
- Auth: `ApiKey: <key>` header. Note the capitalisation — `Api-Key`,
  `API-Key`, and `X-API-Key` are *not* accepted by Stash. The key comes from
  Stash → Settings → Security → "API Key".
- HTTP timeout: 30 s. No client-side rate limit (Stash is a private,
  self-hosted GraphQL endpoint).
- GraphQL queries used:
  `findScenes`, `findScene`, `findStudios`, `findStudio`,
  `findScenes(scene_filter.studios)`, `findPerformer`.
- Episode walk cap: 1000 scenes per studio (`maxEpisodesPerStudio`).

Failure signatures:

| Symptom | Likely cause |
| --- | --- |
| `stash: GraphQL endpoint not configured` | `url` was empty when `Configure` ran. Source should already be disabled — if you see this, check that the configure round-trip happened. |
| `stash: <url> returned 401: ...` | Wrong / missing `ApiKey` header. Older Stash builds also return 401 if the upstream was started without an API key but the plugin sent one. |
| `stash: <url> returned 404: ...` | URL points at the wrong host, or `/graphql` path missing on a Stash build that doesn't auto-redirect. |
| `stash: <some message>` (no HTTP status) | GraphQL `errors[0].message` from upstream — e.g. an invalid ID. The plugin returns the first error verbatim. |
| `stash: not found` (`ErrNotFound`) | Either upstream returned HTTP 4xx where the GraphQL payload was `null`, or the query resolved to a null entity. Treated like a hard miss by the aggregator. |
| TLS / connection refused | Local DNS, container networking, or HTTP-vs-HTTPS mismatch. Stash defaults to plain HTTP; double-check the scheme in the URL field. |

## Image URL plumbing

All sources hand the aggregator pre-escaped, source-relative paths. The
aggregator wraps them as `adult://<slug>/<role>/<path>` before returning to
the host. When the host later asks for the real URL, `ResolveImage` /
`ResolveImageURLs`:

1. Decodes the scheme.
2. Looks up the source by slug.
3. Calls the source's `ResolveImage(role, rawPath, variant)`.

For both TPDB and Stash today, `ResolveImage` simply URL-decodes `rawPath` —
the stored value is an already-absolute upstream URL that the original
`encodeAbsolute` helper percent-escaped so the `://` and `/` characters
survive being wrapped in the `adult://` scheme. **The `variant` argument is
ignored by both sources** because neither upstream exposes size variants on a
single asset. If `ResolveImage` returns an empty string, the most likely cause
is a corrupted (non-URL-escaped) path stored from an older plugin version —
re-run the host's image refresh to repopulate.

Resolved URLs are advertised to the host with a **24-hour TTL** (see
`main.go`). The host caches the resolution; if upstream rotates an image (TPDB
in particular re-uploads with new CDN paths from time to time), the cached
host-side URL may 404 until the TTL lapses or the operator triggers a refresh.

## Common operator workflows

### "I added an API key but nothing changed"
1. Confirm the `enabled` switch is on for that card.
2. Confirm the field is non-empty (`api_key` for TPDB, `url` for Stash).
3. Re-save the card to force a `Configure` round-trip — the source list is
   rebuilt on every save, not on every request.
4. Run a host-side metadata refresh on a known item. The plugin does not have
   its own "test connection" surface; the canary is an actual lookup.

### "TPDB hits but no Stash results in Search"
- Stash returns its own ordering; if TPDB's slice is non-empty and Stash's is
  empty, the merged result is `[tpdb..., (empty)]` and that's expected.
- If Stash errored, the aggregator swallows it as long as TPDB succeeded. The
  source's own log line is the only evidence — search host logs for `stash:`.

### "Episode list stops short on a big site/studio"
- TPDB / Stash sources cap each walk at 1000 entries
  (`maxEpisodesPerSite` / `maxEpisodesPerStudio`). Raise the constant and
  rebuild if a specific operator needs more. There is no manifest knob for
  this today.

### "Re-identify against a specific source"
- The host passes `ProviderIDs` on the search request. Set
  `ProviderIDs["tpdb"]` (or `"stash"`) to the local ID and Search skips
  fan-out, hitting only that source. Useful when one source has a wrong match
  cached.

### "Source is disabled but its IDs are still on items"
- `GetMetadata` / `GetEpisodes` etc. will fail with `adult: no source matches
  request` for those items. Either re-enable the source, or have the host
  re-identify the items so a different source's IDs land on them.

## Where to look in the code

| Concern | File |
| --- | --- |
| Fan-out, routing, ID encoding | `provider/aggregator.go` |
| Source contract | `provider/source.go` |
| TPDB HTTP client, rate limiter, ErrNotFound | `provider/sources/tpdb/client.go` |
| TPDB mapping, episode walk cap | `provider/sources/tpdb/source.go` |
| Stash GraphQL client, ApiKey header, URL normalisation | `provider/sources/stash/client.go` |
| Stash mapping, episode walk cap | `provider/sources/stash/source.go` |
| Config wiring (`Configure` → `SetSources`) | `main.go` |
| Manifest cards (admin form shape) | `manifest.json` |
