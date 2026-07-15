// Package registry implements a client for the ARD registry POST /search
// API: querying a registry, paginating through results, and harvesting the
// full contents of a registry (results + referrals) for a bounded number of
// pages.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/helgesverre/ardvark/internal/ard"
)

// DefaultSearchPath is appended to a registry's base URL to form the
// /search endpoint, unless the base URL's path already ends with
// DefaultSearchPath (a trailing slash on the base URL is ignored either
// way), in which case the base URL is used as-is.
const DefaultSearchPath = "/search"

// federationNone is the SearchRequest.Federation value meaning "do not
// federate this query to other registries".
const federationNone = "none"

// maxResponseBytes bounds how much of a /search response body is read,
// guarding against a misbehaving or malicious registry streaming an
// unbounded response.
const maxResponseBytes = 10 << 20

// Client queries an ARD registry's POST /search endpoint.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New returns a Client for the registry at baseURL (e.g.
// "https://registry.example.com"). If httpClient is nil, http.DefaultClient
// is used.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, httpClient: httpClient}
}

// QueryFilter narrows a registry search. All fields are optional.
type QueryFilter struct {
	Type string   `json:"type,omitempty"`
	Tags []string `json:"tags,omitempty"`
}

// Query is the "query" object of a /search request body.
type Query struct {
	Text   string       `json:"text"`
	Filter *QueryFilter `json:"filter,omitempty"`
}

// SearchRequest is the body of a POST /search request.
type SearchRequest struct {
	Query      Query  `json:"query"`
	Federation string `json:"federation"`
	PageSize   int    `json:"pageSize,omitempty"`
	PageToken  string `json:"pageToken,omitempty"`
}

// Result is a single catalog-entry-shaped search hit, with the registry's
// relevance score and source registry attribution.
type Result struct {
	ard.Entry
	Score  float64 `json:"score,omitempty"`
	Source string  `json:"source,omitempty"`
}

// SearchResponse is the body of a POST /search response.
type SearchResponse struct {
	Results   []Result    `json:"results"`
	Referrals []ard.Entry `json:"referrals"`
	PageToken string      `json:"pageToken"`
}

// StatusError is returned when the registry responds with a non-200 status.
// The status code is preserved so callers can distinguish, e.g., a 501 Not
// Implemented federation request from a transient 5xx.
type StatusError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("registry: search: unexpected status %s", e.Status)
}

// IsNotImplemented reports whether err is a StatusError for HTTP 501 (Not
// Implemented) — the status a registry returns when it does not support a
// requested feature, e.g. federation.
func IsNotImplemented(err error) bool {
	var serr *StatusError
	if !errors.As(err, &serr) {
		return false
	}
	return serr.StatusCode == http.StatusNotImplemented
}

// Search issues a single POST /search request and decodes the response.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	if req.Federation == "" {
		req.Federation = federationNone
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("registry: search: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.searchURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("registry: search: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("registry: search: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("registry: search: reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(respBody),
		}
	}

	var out SearchResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("registry: search: decoding response: %w", err)
	}
	return &out, nil
}

// searchURL returns the resolved /search endpoint URL for the registry's
// base URL: a trailing slash is trimmed, and DefaultSearchPath is appended
// unless the base URL's path already ends with it.
func (c *Client) searchURL() string {
	base := strings.TrimSuffix(c.baseURL, "/")
	if strings.HasSuffix(base, DefaultSearchPath) {
		return base
	}
	return base + DefaultSearchPath
}

// HarvestOptions configures a HarvestAll run.
type HarvestOptions struct {
	// PageLimit bounds the number of /search pages fetched. Zero or
	// negative means unlimited (paginate until the registry stops
	// returning a pageToken).
	PageLimit int
	// PageSize is the requested page size per /search call. Zero leaves
	// it unset, letting the registry choose its default.
	PageSize int
	// QueryText is the query text used for harvesting. Empty/broad text
	// is expected to surface the registry's full catalog.
	QueryText string
}

// HarvestResult accumulates everything collected across a HarvestAll run.
type HarvestResult struct {
	Results   []Result
	Referrals []ard.Entry
	Pages     int
}

// HarvestAll paginates a registry's /search endpoint with a broad query,
// accumulating results and referrals until the registry stops returning a
// pageToken or opts.PageLimit pages have been fetched.
func (c *Client) HarvestAll(ctx context.Context, opts HarvestOptions) (*HarvestResult, error) {
	out := &HarvestResult{}

	pageToken := ""
	for {
		if opts.PageLimit > 0 && out.Pages >= opts.PageLimit {
			break
		}

		req := SearchRequest{
			Query:     Query{Text: opts.QueryText},
			PageSize:  opts.PageSize,
			PageToken: pageToken,
		}

		resp, err := c.Search(ctx, req)
		if err != nil {
			return out, fmt.Errorf("registry: harvest: page %d: %w", out.Pages+1, err)
		}

		out.Results = append(out.Results, resp.Results...)
		out.Referrals = append(out.Referrals, resp.Referrals...)
		out.Pages++

		if resp.PageToken == "" {
			break
		}
		if resp.PageToken == pageToken {
			// Guard against a misbehaving registry that echoes the same
			// token forever; treat it as exhausted rather than looping.
			break
		}
		pageToken = resp.PageToken
	}

	return out, nil
}
