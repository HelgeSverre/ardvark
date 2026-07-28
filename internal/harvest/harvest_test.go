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

// -- link relation aliases -------------------------------------------------

// rel="ard" is the preferred link relation; rel="ai-catalog" remains a
// supported alias. Both must be recognized inside a space-separated,
// arbitrarily-cased token list.
func TestExtract_RecognizesArdAndLegacyRelTokens(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{"preferred token", "ard", true},
		{"legacy token", "ai-catalog", true},
		{"preferred uppercase", "ARD", true},
		{"preferred in a token list", "alternate ard noopener", true},
		{"legacy in a token list", "alternate ai-catalog", true},
		{"unrelated token", "stylesheet", false},
		{"substring must not match", "guard", false},
		{"substring prefix must not match", "ard-extra", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := `<html><head><link rel="` + tt.rel + `" href="/catalog.json"></head><body></body></html>`
			res, err := ExtractLinks("https://example.com/", strings.NewReader(page))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			got := len(res.AICatalogHints) > 0
			if got != tt.want {
				t.Errorf("rel=%q produced hints %v, want hint=%v", tt.rel, res.AICatalogHints, tt.want)
			}
		})
	}
}

// -- inline JSON-LD --------------------------------------------------------

func TestExtractLinks_CollectsJSONLDBlocks(t *testing.T) {
	const page = `<html><head>
	<script type="application/ld+json">{"@type":"Organization","name":"Example"}</script>
	<script type="APPLICATION/LD+JSON">{"specVersion":"1.0","entries":[]}</script>
	<script type="text/javascript">var notJSONLD = 1;</script>
	</head><body><a href="/next">n</a></body></html>`

	res, err := ExtractLinks("https://example.com/", strings.NewReader(page))
	if err != nil {
		t.Fatalf("ExtractLinks: %v", err)
	}
	if len(res.JSONLD) != 2 {
		t.Fatalf("collected %d ld+json blocks, want 2: %q", len(res.JSONLD), res.JSONLD)
	}
	if !strings.Contains(string(res.JSONLD[1]), "specVersion") {
		t.Errorf("second block = %q, want the specVersion document", res.JSONLD[1])
	}
	if len(res.Links) != 1 {
		t.Errorf("Links = %v, want the ordinary anchor still harvested", res.Links)
	}
}

// Malformed or oversized JSON-LD must never cost us the page's ordinary link
// harvesting — that is the crawler's primary job on an HTML page.
func TestExtractLinks_MalformedJSONLDDoesNotAbortHarvesting(t *testing.T) {
	const page = `<html><head>
	<script type="application/ld+json">{ this is not json </script>
	</head><body><a href="/next">n</a><a href="https://other.example/x">o</a></body></html>`

	res, err := ExtractLinks("https://example.com/", strings.NewReader(page))
	if err != nil {
		t.Fatalf("ExtractLinks: %v", err)
	}
	if len(res.Links) != 2 {
		t.Errorf("Links = %v, want both anchors", res.Links)
	}
	if len(res.Hosts) != 2 {
		t.Errorf("Hosts = %v, want both hosts", res.Hosts)
	}
}

// A single page must not be able to bypass the crawler's body limits by
// embedding unbounded or unlimited JSON-LD.
func TestExtractLinks_BoundsJSONLDSizeAndCount(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<html><head>")
	for i := 0; i < maxJSONLDBlocks+10; i++ {
		sb.WriteString(`<script type="application/ld+json">{"n":1}</script>`)
	}
	sb.WriteString(`<script type="application/ld+json">{"big":"`)
	sb.WriteString(strings.Repeat("x", maxJSONLDBlockBytes+1))
	sb.WriteString(`"}</script>`)
	sb.WriteString("</head><body></body></html>")

	res, err := ExtractLinks("https://example.com/", strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ExtractLinks: %v", err)
	}
	if len(res.JSONLD) > maxJSONLDBlocks {
		t.Errorf("collected %d blocks, want at most %d", len(res.JSONLD), maxJSONLDBlocks)
	}
	for i, b := range res.JSONLD {
		if len(b) > maxJSONLDBlockBytes {
			t.Errorf("block %d is %d bytes, want at most %d", i, len(b), maxJSONLDBlockBytes)
		}
	}
}
