package stash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/RXWatcher/silo-plugin-adult/provider/httpx"
	"github.com/RXWatcher/silo-plugin-adult/provider/logging"
)

// Client is a minimal GraphQL client for a self-hosted Stash instance.
//
// Stash exposes a /graphql endpoint that accepts POST {query, variables}. The
// optional ApiKey header is sent on every request when configured.
// defaultRPS bounds requests to a self-hosted Stash instance. Studio episode
// walks can fire up to ~10 paged queries back-to-back; the limiter keeps that
// from hammering a small home server. The burst lets a single page-1 lookup go
// out immediately while still smoothing the multi-page case.
const defaultRPS = 5

type Client struct {
	url     string // full GraphQL endpoint, e.g. http://stash.local:9999/graphql
	apiKey  string
	http    *http.Client
	limiter *rate.Limiter
}

// NewClient constructs a client. base is expected to be the GraphQL endpoint;
// a path of /graphql is appended if missing so users can paste the bare host.
//
// The base URL is admin-configured and used as a POST target, so it is
// validated here: it must parse, use an http(s) scheme, and carry a host. An
// invalid base yields an empty client URL, which makes every request fail fast
// in do() rather than being sent to an unexpected target.
func NewClient(base, apiKey string) *Client {
	return &Client{
		url:     normalizeBaseURL(base),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(defaultRPS), 2),
	}
}

// normalizeBaseURL validates and canonicalizes the admin-supplied Stash base
// URL. It parses the URL (rather than substring-matching) so the /graphql
// suffix is only appended when the *path* is missing it, and rejects any URL
// that is not an absolute http(s) URL with a host. Returns "" on rejection.
func normalizeBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return ""
	}
	if u.Host == "" {
		return ""
	}
	// Append /graphql only when the path doesn't already end in it.
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/graphql") {
		u.Path = path + "/graphql"
	} else {
		u.Path = path
	}
	return u.String()
}

// SetHTTPClient overrides the underlying HTTP client. Used by tests.
func (c *Client) SetHTTPClient(h *http.Client) { c.http = h }

// FindScenes runs a free-text search against findScenes.
func (c *Client) FindScenes(ctx context.Context, query string) ([]sceneDTO, error) {
	const q = `query FindScenes($q: String) {
		findScenes(filter: {q: $q, per_page: 50}) {
			scenes { id title details date paths { screenshot } studio { id name image_path parent_studio { name image_path } } tags { name } performers { id name disambiguation image_path } }
		}
	}`
	var resp findScenesResp
	if err := c.do(ctx, q, map[string]any{"q": query}, &resp); err != nil {
		return nil, err
	}
	return resp.FindScenes.Scenes, nil
}

// FindScene fetches a single scene by ID.
func (c *Client) FindScene(ctx context.Context, id string) (*sceneDTO, error) {
	const q = `query FindScene($id: ID!) {
		findScene(id: $id) { id title details date paths { screenshot } studio { id name image_path details parent_studio { id name image_path } } tags { name } performers { id name disambiguation image_path } }
	}`
	var resp findSceneResp
	if err := c.do(ctx, q, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	if resp.FindScene == nil {
		return nil, ErrNotFound
	}
	return resp.FindScene, nil
}

// FindStudios runs a free-text search against findStudios.
func (c *Client) FindStudios(ctx context.Context, query string) ([]studioDTO, error) {
	const q = `query FindStudios($q: String) {
		findStudios(filter: {q: $q, per_page: 50}) {
			studios { id name details image_path parent_studio { id name image_path } }
		}
	}`
	var resp findStudiosResp
	if err := c.do(ctx, q, map[string]any{"q": query}, &resp); err != nil {
		return nil, err
	}
	return resp.FindStudios.Studios, nil
}

// FindStudio fetches a single studio by ID.
func (c *Client) FindStudio(ctx context.Context, id string) (*studioDTO, error) {
	const q = `query FindStudio($id: ID!) {
		findStudio(id: $id) { id name details image_path parent_studio { id name image_path } }
	}`
	var resp findStudioResp
	if err := c.do(ctx, q, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	if resp.FindStudio == nil {
		return nil, ErrNotFound
	}
	return resp.FindStudio, nil
}

// ListScenesForStudio returns scenes filtered to a studio, ordered by date.
// page is 1-indexed; perPage maxes out at 100.
func (c *Client) ListScenesForStudio(ctx context.Context, studioID string, page, perPage int) ([]sceneDTO, error) {
	if page < 1 {
		page = 1
	}
	if perPage <= 0 || perPage > 100 {
		perPage = 100
	}
	const q = `query StudioScenes($studio: ID!, $page: Int!, $perPage: Int!) {
		findScenes(
			filter: {page: $page, per_page: $perPage, sort: "date", direction: ASC},
			scene_filter: {studios: {value: [$studio], modifier: INCLUDES}}
		) { scenes { id title details date paths { screenshot } performers { id name image_path } tags { name } } }
	}`
	var resp findScenesResp
	if err := c.do(ctx, q, map[string]any{
		"studio":  studioID,
		"page":    page,
		"perPage": perPage,
	}, &resp); err != nil {
		return nil, err
	}
	return resp.FindScenes.Scenes, nil
}

// FindPerformer fetches a single performer by ID.
func (c *Client) FindPerformer(ctx context.Context, id string) (*performerDTO, error) {
	const q = `query FindPerformer($id: ID!) {
		findPerformer(id: $id) { id name details disambiguation image_path alias_list birthdate death_date country tags { name } }
	}`
	var resp findPerformerResp
	if err := c.do(ctx, q, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	if resp.FindPerformer == nil {
		return nil, ErrNotFound
	}
	return resp.FindPerformer, nil
}

// do POSTs a GraphQL query and decodes the data envelope into out.
//
// Although the transport is POST, these are read-only GraphQL queries against
// a self-hosted instance and are safe to retry, so transient 5xx/network blips
// are retried with bounded backoff. A rate limiter smooths multi-page studio
// walks against small home servers.
func (c *Client) do(ctx context.Context, query string, variables map[string]any, out any) error {
	if c.url == "" {
		return errors.New("stash: GraphQL endpoint not configured")
	}
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
	}
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return err
	}
	resp, err := httpx.DoWithRetry(ctx, c.http, httpx.RetryConfig{Source: "stash"}, func() (*http.Request, error) {
		// Fresh reader per attempt: a retried request must re-read the body
		// from the start rather than from the previous attempt's EOF.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if c.apiKey != "" {
			req.Header.Set("ApiKey", c.apiKey)
		}
		return req, nil
	})
	if err != nil {
		logging.L().Error("stash: request failed", "error", err.Error())
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// The upstream body may contain attacker-influenced or sensitive
		// content, so it is logged for operators but never folded into the
		// returned error (which can surface to clients / other plugins).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		logging.L().Error("stash: upstream error",
			"status", resp.StatusCode,
			"body", strings.TrimSpace(string(body)),
		)
		return fmt.Errorf("stash: request failed with status %d", resp.StatusCode)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("stash: %s", envelope.Errors[0].Message)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return ErrNotFound
	}
	return json.Unmarshal(envelope.Data, out)
}

// ErrNotFound is returned when a query resolves to null or a 404 response.
var ErrNotFound = errors.New("stash: not found")
