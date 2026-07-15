package harvest

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractLinks(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		html      string
		wantLinks []string
		wantHosts []string
		wantHints []string
		wantErr   bool
	}{
		{
			name:    "absolute and relative links",
			baseURL: "https://example.com/dir/page.html",
			html: `<html><body>
				<a href="https://other.com/a">abs</a>
				<a href="/root-relative">root</a>
				<a href="relative.html">rel</a>
				<a href="../up.html">up</a>
			</body></html>`,
			wantLinks: []string{
				"https://example.com/dir/relative.html",
				"https://example.com/root-relative",
				"https://example.com/up.html",
				"https://other.com/a",
			},
			wantHosts: []string{"example.com", "other.com"},
			wantHints: nil,
		},
		{
			name:      "protocol relative url",
			baseURL:   "https://example.com/",
			html:      `<a href="//cdn.example.com/lib.js">lib</a>`,
			wantLinks: []string{"https://cdn.example.com/lib.js"},
			wantHosts: []string{"cdn.example.com"},
		},
		{
			name:    "fragment stripped and dedup",
			baseURL: "https://example.com/",
			html: `
				<a href="/page#section1">one</a>
				<a href="/page#section2">two</a>
				<a href="/page">three</a>
			`,
			wantLinks: []string{"https://example.com/page"},
			wantHosts: []string{"example.com"},
		},
		{
			name:    "non http scheme and malformed hrefs skipped",
			baseURL: "https://example.com/",
			html: `
				<a href="mailto:foo@example.com">mail</a>
				<a href="javascript:void(0)">js</a>
				<a href="ftp://example.com/file">ftp</a>
				<a href="">empty</a>
				<a href="   ">whitespace</a>
				<a href="%zz%zz">broken</a>
				<a>no href</a>
				<a href="https://good.com/x">good</a>
			`,
			wantLinks: []string{"https://good.com/x"},
			wantHosts: []string{"good.com"},
		},
		{
			name:    "base tag overrides relative resolution",
			baseURL: "https://example.com/some/deep/page.html",
			html: `
				<html><head><base href="https://cdn.example.com/assets/"></head>
				<body><a href="img.png">img</a></body></html>
			`,
			wantLinks: []string{"https://cdn.example.com/assets/img.png"},
			wantHosts: []string{"cdn.example.com"},
		},
		{
			name:    "ai-catalog link rel hint",
			baseURL: "https://example.com/",
			html: `
				<html><head>
					<link rel="ai-catalog" href="/.well-known/ai-catalog.json">
					<link rel="stylesheet" href="/style.css">
				</head><body></body></html>
			`,
			wantLinks: nil,
			wantHosts: nil,
			wantHints: []string{"https://example.com/.well-known/ai-catalog.json"},
		},
		{
			name:      "ai-catalog rel among multiple tokens",
			baseURL:   "https://example.com/",
			html:      `<link rel="alternate ai-catalog" href="/catalog.json">`,
			wantHints: []string{"https://example.com/catalog.json"},
		},
		{
			name:    "malformed markup unclosed tags",
			baseURL: "https://example.com/",
			html: `
				<html><body>
				<div><a href="/one">one<a href="/two">two
				<p>unclosed paragraph
				<a href="/three">three</a>
			`,
			wantLinks: []string{
				"https://example.com/one",
				"https://example.com/three",
				"https://example.com/two",
			},
			wantHosts: []string{"example.com"},
		},
		{
			name:      "area href included",
			baseURL:   "https://example.com/",
			html:      `<map><area href="/mapped" shape="rect"></map>`,
			wantLinks: []string{"https://example.com/mapped"},
			wantHosts: []string{"example.com"},
		},
		{
			name:      "invalid base url in doc ignored falls back",
			baseURL:   "https://example.com/dir/",
			html:      `<base href=":::not a url"><a href="page.html">p</a>`,
			wantLinks: []string{"https://example.com/dir/page.html"},
			wantHosts: []string{"example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractLinks(tt.baseURL, strings.NewReader(tt.html))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExtractLinks() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if !reflect.DeepEqual(got.Links, tt.wantLinks) && !(len(got.Links) == 0 && len(tt.wantLinks) == 0) {
				t.Errorf("Links = %v, want %v", got.Links, tt.wantLinks)
			}
			if !reflect.DeepEqual(got.Hosts, tt.wantHosts) && !(len(got.Hosts) == 0 && len(tt.wantHosts) == 0) {
				t.Errorf("Hosts = %v, want %v", got.Hosts, tt.wantHosts)
			}
			if !reflect.DeepEqual(got.AICatalogHints, tt.wantHints) && !(len(got.AICatalogHints) == 0 && len(tt.wantHints) == 0) {
				t.Errorf("AICatalogHints = %v, want %v", got.AICatalogHints, tt.wantHints)
			}
		})
	}
}

func TestExtractLinksInvalidBaseURL(t *testing.T) {
	_, err := ExtractLinks("://not-a-url", strings.NewReader(`<a href="/x">x</a>`))
	if err == nil {
		t.Fatal("expected error for invalid base URL, got nil")
	}
}
