package mediatype

import (
	"reflect"
	"testing"
)

func TestParse_Fields(t *testing.T) {
	tests := []struct {
		in                        string
		wantType, wantBase, wantSuffix string
		wantParams                map[string]string
		wantFullType              string
	}{
		{"application/ai-catalog+json", "application", "ai-catalog", "json", map[string]string{}, "application/ai-catalog"},
		{"application/ai-registry", "application", "ai-registry", "", map[string]string{}, "application/ai-registry"},
		{"application/ai-skill-archive+gzip", "application", "ai-skill-archive", "gzip", map[string]string{}, "application/ai-skill-archive"},
		{"APPLICATION/AI-Catalog+JSON", "application", "ai-catalog", "json", map[string]string{}, "application/ai-catalog"},
		{"  application/ai-skill  ", "application", "ai-skill", "", map[string]string{}, "application/ai-skill"},
		{`text/markdown; profile="urn:air:agent-skills"`, "text", "markdown", "", map[string]string{"profile": "urn:air:agent-skills"}, "text/markdown"},
		{"application/ai-catalog+json; charset=utf-8", "application", "ai-catalog", "json", map[string]string{"charset": "utf-8"}, "application/ai-catalog"},
		{"application/vnd.oai.openapi", "application", "vnd.oai.openapi", "", map[string]string{}, "application/vnd.oai.openapi"},
		{"garbage", "", "garbage", "", map[string]string{}, "/garbage"},
		{"application/", "application", "", "", map[string]string{}, "application/"},
		{"", "", "", "", map[string]string{}, "/"},
	}
	for _, tt := range tests {
		m := Parse(tt.in)
		if m.Raw != tt.in {
			t.Errorf("Parse(%q).Raw = %q, want %q", tt.in, m.Raw, tt.in)
		}
		if m.Type != tt.wantType || m.Base != tt.wantBase || m.Suffix != tt.wantSuffix {
			t.Errorf("Parse(%q) = {Type:%q Base:%q Suffix:%q}, want {%q %q %q}", tt.in, m.Type, m.Base, m.Suffix, tt.wantType, tt.wantBase, tt.wantSuffix)
		}
		if !reflect.DeepEqual(m.Params, tt.wantParams) {
			t.Errorf("Parse(%q).Params = %v, want %v", tt.in, m.Params, tt.wantParams)
		}
		if m.FullType() != tt.wantFullType {
			t.Errorf("Parse(%q).FullType() = %q, want %q", tt.in, m.FullType(), tt.wantFullType)
		}
	}
}
