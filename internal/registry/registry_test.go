package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helgesverre/ardvark/internal/ard"
)

func entry(id string) ard.Entry {
	return ard.Entry{
		Identifier:  id,
		DisplayName: "Test " + id,
		Type:        "application/agent-card+json",
		URL:         "https://example.com/" + id,
		Description: "a test entry",
	}
}

func TestSearch_Basic(t *testing.T) {
	var gotReq SearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/search" {
			t.Errorf("path = %s, want /search", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decoding request: %v", err)
		}

		resp := SearchResponse{
			Results: []Result{
				{Entry: entry("a"), Score: 0.9, Source: "test-registry"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	resp, err := c.Search(context.Background(), SearchRequest{
		Query: Query{Text: "search text"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotReq.Federation != "none" {
		t.Errorf("federation defaulted to %q, want none", gotReq.Federation)
	}
	if gotReq.Query.Text != "search text" {
		t.Errorf("query text = %q", gotReq.Query.Text)
	}
	if len(resp.Results) != 1 || resp.Results[0].Identifier != "a" {
		t.Fatalf("unexpected results: %+v", resp.Results)
	}
	if resp.Results[0].Score != 0.9 || resp.Results[0].Source != "test-registry" {
		t.Errorf("unexpected score/source: %+v", resp.Results[0])
	}
}

func TestSearch_NonOKStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"not found", http.StatusNotFound},
		{"server error", http.StatusInternalServerError},
		{"not implemented", http.StatusNotImplemented},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte("boom"))
			}))
			defer srv.Close()

			c := New(srv.URL, nil)
			_, err := c.Search(context.Background(), SearchRequest{Query: Query{Text: "x"}})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			serr, ok := err.(*StatusError)
			if !ok {
				t.Fatalf("error type = %T, want *StatusError", err)
			}
			if serr.StatusCode != tt.statusCode {
				t.Errorf("StatusCode = %d, want %d", serr.StatusCode, tt.statusCode)
			}
			if tt.statusCode == http.StatusNotImplemented && !IsNotImplemented(err) {
				t.Error("IsNotImplemented(err) = false, want true")
			}
			if tt.statusCode != http.StatusNotImplemented && IsNotImplemented(err) {
				t.Error("IsNotImplemented(err) = true, want false")
			}
		})
	}
}

// TestHarvestAll_Pagination drives a fake registry across 3 pages,
// surfacing results and referrals from each, and verifies pagination
// terminates when pageToken becomes empty.
func TestHarvestAll_Pagination(t *testing.T) {
	pages := map[string]SearchResponse{
		"": {
			Results:   []Result{{Entry: entry("p1-a")}, {Entry: entry("p1-b")}},
			Referrals: []ard.Entry{entry("ref-1")},
			PageToken: "page2",
		},
		"page2": {
			Results:   []Result{{Entry: entry("p2-a")}},
			Referrals: nil,
			PageToken: "page3",
		},
		"page3": {
			Results:   []Result{{Entry: entry("p3-a")}},
			Referrals: []ard.Entry{entry("ref-2")},
			PageToken: "",
		},
	}

	var requests []SearchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req SearchRequest
		json.NewDecoder(r.Body).Decode(&req)
		requests = append(requests, req)

		resp, ok := pages[req.PageToken]
		if !ok {
			t.Fatalf("unexpected page token %q", req.PageToken)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	result, err := c.HarvestAll(context.Background(), HarvestOptions{})
	if err != nil {
		t.Fatalf("HarvestAll: %v", err)
	}

	if result.Pages != 3 {
		t.Errorf("Pages = %d, want 3", result.Pages)
	}
	if len(result.Results) != 4 {
		t.Errorf("len(Results) = %d, want 4", len(result.Results))
	}
	if len(result.Referrals) != 2 {
		t.Errorf("len(Referrals) = %d, want 2", len(result.Referrals))
	}
	if len(requests) != 3 {
		t.Fatalf("made %d requests, want 3", len(requests))
	}
	if requests[0].PageToken != "" || requests[1].PageToken != "page2" || requests[2].PageToken != "page3" {
		t.Errorf("unexpected page token sequence: %+v", requests)
	}
}

// TestHarvestAll_PageLimit verifies a configured PageLimit halts pagination
// even though the registry would keep returning more pages.
func TestHarvestAll_PageLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req SearchRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := SearchResponse{
			Results:   []Result{{Entry: entry("x")}},
			PageToken: "next-" + req.PageToken, // always has a next page
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	result, err := c.HarvestAll(context.Background(), HarvestOptions{PageLimit: 2})
	if err != nil {
		t.Fatalf("HarvestAll: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if result.Pages != 2 {
		t.Errorf("Pages = %d, want 2", result.Pages)
	}
	if len(result.Results) != 2 {
		t.Errorf("len(Results) = %d, want 2", len(result.Results))
	}
}

// TestHarvestAll_SameTokenGuard verifies a registry that keeps echoing the
// same pageToken does not cause an infinite loop.
func TestHarvestAll_SameTokenGuard(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		resp := SearchResponse{
			Results:   []Result{{Entry: entry("x")}},
			PageToken: "stuck",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	result, err := c.HarvestAll(context.Background(), HarvestOptions{})
	if err != nil {
		t.Fatalf("HarvestAll: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (first with empty token, second with 'stuck', then guard trips)", calls)
	}
	if result.Pages != 2 {
		t.Errorf("Pages = %d, want 2", result.Pages)
	}
}

// TestSearchURL verifies /search is appended to the base URL, except when
// the base URL's path already ends with /search.
func TestSearchURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"bare host", "https://example.com", "https://example.com/search"},
		{"trailing slash", "https://example.com/", "https://example.com/search"},
		{"path suffix", "https://example.com/registry", "https://example.com/registry/search"},
		{"already /search", "https://example.com/search", "https://example.com/search"},
		{"already /search with trailing slash", "https://example.com/search/", "https://example.com/search"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(tt.baseURL, nil)
			if got := c.searchURL(); got != tt.want {
				t.Errorf("searchURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIsNotImplemented_ThroughHarvestAll verifies IsNotImplemented sees
// through the fmt.Errorf %w wrapping HarvestAll applies to page errors.
func TestIsNotImplemented_ThroughHarvestAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte("federation not supported"))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	_, err := c.HarvestAll(context.Background(), HarvestOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsNotImplemented(err) {
		t.Error("IsNotImplemented(err) = false, want true")
	}
}

// TestHarvestAll_ErrorStopsHarvest verifies a mid-pagination error surfaces
// with partial results still returned.
func TestHarvestAll_ErrorStopsHarvest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			resp := SearchResponse{
				Results:   []Result{{Entry: entry("p1")}},
				PageToken: "page2",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	result, err := c.HarvestAll(context.Background(), HarvestOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(result.Results) != 1 {
		t.Errorf("len(Results) = %d, want 1 (partial results preserved)", len(result.Results))
	}
}
