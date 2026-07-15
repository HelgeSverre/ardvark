package seed

import "testing"

func TestSanitize(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "strips wildcard prefix",
			input: []string{"*.example.com"},
			want:  []string{"example.com"},
		},
		{
			name:  "lowercases",
			input: []string{"WWW.Example.COM"},
			want:  []string{"www.example.com"},
		},
		{
			name:  "drops IPv4 addresses",
			input: []string{"192.168.1.1"},
			want:  []string{},
		},
		{
			name:  "drops IPv6 addresses",
			input: []string{"2001:db8::1"},
			want:  []string{},
		},
		{
			name:  "drops names without a dot",
			input: []string{"localhost"},
			want:  []string{},
		},
		{
			name:  "drops names with invalid characters",
			input: []string{"exa mple.com", "foo_bar.com/path"},
			want:  []string{},
		},
		{
			name:  "dedupes preserving first-seen order",
			input: []string{"b.example.com", "a.example.com", "b.example.com"},
			want:  []string{"b.example.com", "a.example.com"},
		},
		{
			name:  "dedupes after normalization",
			input: []string{"*.Example.com", "example.com", "EXAMPLE.COM"},
			want:  []string{"example.com"},
		},
		{
			name:  "trims trailing dot",
			input: []string{"example.com."},
			want:  []string{"example.com"},
		},
		{
			name:  "allows hyphenated labels",
			input: []string{"my-app.example.com"},
			want:  []string{"my-app.example.com"},
		},
		{
			name:  "drops labels starting or ending with hyphen",
			input: []string{"-foo.example.com", "foo-.example.com"},
			want:  []string{},
		},
		{
			name:  "empty input",
			input: nil,
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Sanitize(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("Sanitize(%v) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Sanitize(%v) = %v, want %v", tt.input, got, tt.want)
				}
			}
		})
	}
}
