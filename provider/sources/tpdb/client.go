package tpdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/RXWatcher/silo-plugin-adult/provider/httpx"
	"github.com/RXWatcher/silo-plugin-adult/provider/logging"
)

const (
	defaultBaseURL = "https://api.theporndb.net"
	// TPDB documents 120 req/min for authenticated users. We bias slightly
	// under that to leave headroom for retries.
	defaultRPS = 1.8
)

// Client is a thin HTTP client for the TPDB v1 API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	limiter *rate.Limiter
}

// NewClient constructs a client with the supplied API key.
func NewClient(apiKey string) *Client {
	return &Client{
		baseURL: defaultBaseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(defaultRPS), 4),
	}
}

// SetBaseURL overrides the upstream base. Used by tests.
func (c *Client) SetBaseURL(base string) { c.baseURL = base }

// SetHTTPClient overrides the underlying HTTP client. Used by tests.
func (c *Client) SetHTTPClient(h *http.Client) { c.http = h }

// SearchScenes searches by parsed title with an optional year.
func (c *Client) SearchScenes(ctx context.Context, query string, year int) ([]sceneDTO, error) {
	params := url.Values{"parse": []string{query}}
	if year > 0 {
		params.Set("year", strconv.Itoa(year))
	}
	var resp listResponse[sceneDTO]
	if err := c.get(ctx, "/scenes?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// SearchMovies searches the /movies endpoint (full-length releases / DVDs).
func (c *Client) SearchMovies(ctx context.Context, query string, year int) ([]movieDTO, error) {
	params := url.Values{"q": []string{query}}
	if year > 0 {
		params.Set("year", strconv.Itoa(year))
	}
	var resp listResponse[movieDTO]
	if err := c.get(ctx, "/movies?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// SearchSites searches the /sites endpoint by name (studios / series).
func (c *Client) SearchSites(ctx context.Context, query string) ([]siteDTO, error) {
	params := url.Values{"q": []string{query}}
	var resp listResponse[siteDTO]
	if err := c.get(ctx, "/sites?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// GetScene fetches a single scene by ID or slug.
func (c *Client) GetScene(ctx context.Context, id string) (*sceneDTO, error) {
	var resp itemResponse[sceneDTO]
	if err := c.get(ctx, "/scenes/"+url.PathEscape(id), &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// GetMovie fetches a single movie by ID or slug.
func (c *Client) GetMovie(ctx context.Context, id string) (*movieDTO, error) {
	var resp itemResponse[movieDTO]
	if err := c.get(ctx, "/movies/"+url.PathEscape(id), &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// GetSite fetches a single site by ID or UUID.
func (c *Client) GetSite(ctx context.Context, id string) (*siteDTO, error) {
	var resp itemResponse[siteDTO]
	if err := c.get(ctx, "/sites/"+url.PathEscape(id), &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// ListSceneForSite returns scenes belonging to a site, ordered by release date.
// page is 1-indexed; perPage is capped at 100 by TPDB.
func (c *Client) ListScenesForSite(ctx context.Context, siteID string, page, perPage int) ([]sceneDTO, error) {
	if page < 1 {
		page = 1
	}
	if perPage <= 0 || perPage > 100 {
		perPage = 100
	}
	params := url.Values{
		"site_id":  []string{siteID},
		"page":     []string{strconv.Itoa(page)},
		"per_page": []string{strconv.Itoa(perPage)},
		"order":    []string{"date_asc"},
	}
	var resp listResponse[sceneDTO]
	if err := c.get(ctx, "/scenes?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// GetPerformer fetches a single performer by ID or slug.
func (c *Client) GetPerformer(ctx context.Context, id string) (*performerDTO, error) {
	var resp itemResponse[performerDTO]
	if err := c.get(ctx, "/performers/"+url.PathEscape(id), &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	// GETs are idempotent, so transient 5xx/network blips are retried with
	// bounded backoff rather than failing the whole metadata fetch.
	resp, err := httpx.DoWithRetry(ctx, c.http, httpx.RetryConfig{Source: "tpdb"}, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		return req, nil
	})
	if err != nil {
		logging.L().Error("tpdb: request failed", "path", path, "error", err.Error())
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		// The upstream body may carry sensitive/attacker-influenced content,
		// so it is logged for operators but kept out of the returned error.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		logging.L().Error("tpdb: upstream error",
			"path", path,
			"status", resp.StatusCode,
			"body", string(body),
		)
		return fmt.Errorf("tpdb: %s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ErrNotFound is returned for HTTP 404 responses so callers can distinguish
// "no such id" from a transport / server error.
var ErrNotFound = errors.New("tpdb: not found")
