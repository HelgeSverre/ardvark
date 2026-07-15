package ard

import (
	"reflect"
	"testing"
)

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
				Publisher: "example.com",
				Namespace: []string{},
				Name:      "my-agent",
			},
		},
		{
			name:  "multi-segment namespace",
			input: "urn:air:example.com:tools:search:web-search",
			want: URN{
				Publisher: "example.com",
				Namespace: []string{"tools", "search"},
				Name:      "web-search",
			},
		},
		{
			name:  "single-segment namespace",
			input: "urn:air:acme.io:agents:helper",
			want: URN{
				Publisher: "acme.io",
				Namespace: []string{"agents"},
				Name:      "helper",
			},
		},
		{
			name:  "uppercase urn scheme and NID (RFC 8141 case-insensitive)",
			input: "URN:AIR:example.com:my-agent",
			want: URN{
				Publisher: "example.com",
				Namespace: []string{},
				Name:      "my-agent",
			},
		},
		{
			name:  "mixed-case urn scheme and NID",
			input: "Urn:Air:example.com:tools:my-agent",
			want: URN{
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
			name:    "uppercase publisher",
			input:   "urn:air:Example.com:name",
			wantErr: true,
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
			if got.Publisher != tt.want.Publisher || got.Name != tt.want.Name {
				t.Fatalf("ParseURN(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
			if !reflect.DeepEqual(got.Namespace, tt.want.Namespace) {
				t.Fatalf("ParseURN(%q) namespace = %+v, want %+v", tt.input, got.Namespace, tt.want.Namespace)
			}
		})
	}
}
