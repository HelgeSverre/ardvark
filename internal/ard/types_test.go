package ard

import (
	"reflect"
	"testing"
)

func TestURN_String(t *testing.T) {
	tests := []struct {
		name string
		urn  URN
		want string
	}{
		{
			name: "no namespace",
			urn:  URN{NID: "air", Publisher: "example.com", Namespace: []string{}, Name: "a"},
			want: "urn:air:example.com:a",
		},
		{
			name: "with namespace",
			urn:  URN{NID: "ai", Publisher: "example.com", Namespace: []string{"tool"}, Name: "a"},
			want: "urn:ai:example.com:tool:a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.urn.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Two case-insensitive-equivalent URNs (differing only in NID/publisher
// case) must normalize to the same canonical String(), which is what
// identifier.unique relies on to catch case-insensitive duplicates.
func TestURN_String_CaseInsensitiveEquivalence(t *testing.T) {
	a, err := ParseURN("urn:air:Example.com:agents:a")
	if err != nil {
		t.Fatalf("ParseURN: %v", err)
	}
	b, err := ParseURN("URN:AIR:example.COM:agents:a")
	if err != nil {
		t.Fatalf("ParseURN: %v", err)
	}
	if a.String() != b.String() {
		t.Fatalf("String() mismatch: %q vs %q, want equal", a.String(), b.String())
	}
}

func TestNormalizeIdentifierPublisherASCII(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "unicode publisher normalized to punycode, rest untouched",
			input: "urn:air:café.example:agents:a",
			want:  "urn:air:xn--caf-dma.example:agents:a",
		},
		{
			name:  "already-ASCII publisher left unchanged",
			input: "urn:air:example.com:agents:a",
			want:  "urn:air:example.com:agents:a",
		},
		{
			name:  "ai NID prefix is also handled",
			input: "urn:ai:café.example:agents:a",
			want:  "urn:ai:xn--caf-dma.example:agents:a",
		},
		{
			name:  "unrecognized prefix returned unchanged",
			input: "not-a-urn",
			want:  "not-a-urn",
		},
		{
			name:  "original prefix casing is preserved",
			input: "URN:AIR:café.example:agents:a",
			want:  "URN:AIR:xn--caf-dma.example:agents:a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeIdentifierPublisherASCII(tt.input); got != tt.want {
				t.Fatalf("normalizeIdentifierPublisherASCII(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseURN(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    URN
		wantErr bool
	}{
		{
			name:  "no namespace",
			input: "urn:air:example.com:my-agent",
			want: URN{
				NID:       "air",
				Publisher: "example.com",
				Namespace: []string{},
				Name:      "my-agent",
			},
		},
		{
			name:  "ai NID (seen in the wild)",
			input: "urn:ai:unlimit.website:tool:pagenode-cms",
			want: URN{
				NID:       "ai",
				Publisher: "unlimit.website",
				Namespace: []string{"tool"},
				Name:      "pagenode-cms",
			},
		},
		{
			name:  "uppercase ai NID",
			input: "URN:AI:example.com:my-agent",
			want: URN{
				NID:       "ai",
				Publisher: "example.com",
				Namespace: []string{},
				Name:      "my-agent",
			},
		},
		{
			name:  "multi-segment namespace",
			input: "urn:air:example.com:tools:search:web-search",
			want: URN{
				NID:       "air",
				Publisher: "example.com",
				Namespace: []string{"tools", "search"},
				Name:      "web-search",
			},
		},
		{
			name:  "single-segment namespace",
			input: "urn:air:acme.io:agents:helper",
			want: URN{
				NID:       "air",
				Publisher: "acme.io",
				Namespace: []string{"agents"},
				Name:      "helper",
			},
		},
		{
			name:  "uppercase urn scheme and NID (RFC 8141 case-insensitive)",
			input: "URN:AIR:example.com:my-agent",
			want: URN{
				NID:       "air",
				Publisher: "example.com",
				Namespace: []string{},
				Name:      "my-agent",
			},
		},
		{
			name:  "mixed-case urn scheme and NID",
			input: "Urn:Air:example.com:tools:my-agent",
			want: URN{
				NID:       "air",
				Publisher: "example.com",
				Namespace: []string{"tools"},
				Name:      "my-agent",
			},
		},
		{
			name:    "bad prefix",
			input:   "urn:foo:example.com:name",
			wantErr: true,
		},
		{
			name:    "missing urn scheme entirely",
			input:   "example.com:name",
			wantErr: true,
		},
		{
			name:    "empty segment in namespace",
			input:   "urn:air:example.com::name",
			wantErr: true,
		},
		{
			name:    "trailing colon (empty name)",
			input:   "urn:air:example.com:name:",
			wantErr: true,
		},
		{
			name:  "uppercase publisher is normalized, not rejected",
			input: "urn:air:Example.com:agents:name",
			want: URN{
				NID:       "air",
				Publisher: "example.com",
				Namespace: []string{"agents"},
				Name:      "name",
			},
		},
		{
			name:    "publisher without dot",
			input:   "urn:air:localhost:name",
			wantErr: true,
		},
		{
			name:    "missing name segment (publisher only)",
			input:   "urn:air:example.com",
			wantErr: true,
		},
		{
			name:  "IDN publisher in Unicode (U-label) form is punycode-normalized",
			input: "urn:air:café.example:agents:a",
			want: URN{
				NID:       "air",
				Publisher: "xn--caf-dma.example",
				Namespace: []string{"agents"},
				Name:      "a",
			},
		},
		{
			name:  "IDN publisher already in punycode (A-label) form is left as-is",
			input: "urn:air:xn--caf-dma.example:agents:a",
			want: URN{
				NID:       "air",
				Publisher: "xn--caf-dma.example",
				Namespace: []string{"agents"},
				Name:      "a",
			},
		},
		{
			name:    "empty string after prefix",
			input:   "urn:air:",
			wantErr: true,
		},
		{
			name:    "completely empty",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseURN(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseURN(%q) expected error, got nil (result: %+v)", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURN(%q) unexpected error: %v", tt.input, err)
			}
			if got.NID != tt.want.NID || got.Publisher != tt.want.Publisher || got.Name != tt.want.Name {
				t.Fatalf("ParseURN(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
			if !reflect.DeepEqual(got.Namespace, tt.want.Namespace) {
				t.Fatalf("ParseURN(%q) namespace = %+v, want %+v", tt.input, got.Namespace, tt.want.Namespace)
			}
		})
	}
}
