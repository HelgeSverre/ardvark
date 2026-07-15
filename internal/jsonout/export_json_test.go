package jsonout

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// nastyStrings covers every branch of appendJSONString: quotes, backslashes,
// control characters, HTML-unsafe runes, U+2028/U+2029, invalid UTF-8,
// multi-byte runes, and empties.
var nastyStrings = []string{
	"",
	"plain ascii",
	`quote " backslash \ done`,
	"newline\n tab\t carriage\r",
	"\x00\x01\x1f control chars",
	"html <script>&amp;</script> unsafe",
	"line sep ators",
	"invalid utf8: \xff\xfe raw bytes",
	"truncated rune: \xe2\x82",
	"emoji 🎉 and åccénts and 日本語",
	strings.Repeat("long ", 1000) + "<end>",
	"� already a replacement char",
}

func TestAppendJSONString_MatchesEncodingJSON(t *testing.T) {
	check := func(t *testing.T, s string) {
		t.Helper()
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("json.Marshal(%q): %v", s, err)
		}
		got := appendJSONString(nil, s)
		if string(got) != string(want) {
			t.Errorf("appendJSONString(%q)\n got %s\nwant %s", s, got, want)
		}
	}

	for _, s := range nastyStrings {
		check(t, s)
	}

	// Deterministic random byte strings sweep the branch combinations the
	// fixed cases might miss (including random invalid UTF-8).
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 500; i++ {
		n := rng.Intn(64)
		b := make([]byte, n)
		rng.Read(b)
		check(t, string(b))
	}
}

func TestAppendRowJSON_MatchesEncodingJSON(t *testing.T) {
	rows := []ExportRow{
		{},
		{
			Host:                  "höst-<1>.example",
			CatalogSourceURL:      "https://example.com/?a=1&b=2",
			VerificationStatus:    "valid",
			URN:                   `urn:air:example.com:tool:with"quote`,
			URNPublisher:          "example.com",
			DisplayName:           "Line Separated",
			MediaType:             "application/json",
			RefURL:                "https://example.com/x.json",
			Description:           "desc with \n newline and invalid \xff utf8",
			Version:               "1.0.0",
			Source:                "catalog",
			Tags:                  `["a","b"]`,
			RepresentativeQueries: `["what is <this>?"]`,
		},
	}
	for i, r := range rows {
		want, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		got := appendRowJSON(nil, &r)
		if string(got) != string(want)+"\n" {
			t.Errorf("row %d:\n got %s\nwant %s\\n", i, got, want)
		}
	}
}
