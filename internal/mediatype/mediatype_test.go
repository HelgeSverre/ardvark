package mediatype

import (
	"reflect"
	"testing"
)

func TestParse_Fields(t *testing.T) {
	tests := []struct {
		in                             string
		wantType, wantBase, wantSuffix string
		wantParams                     map[string]string
		wantFullType                   string
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

func TestKindClassification(t *testing.T) {
	tests := []struct {
		in       string
		wantKind Kind
		pointer  bool
		known    bool
	}{
		{"application/ai-catalog+json", KindCatalog, true, true},
		{"application/ai-catalog", KindCatalog, true, true},
		{"application/ai-registry+json", KindRegistry, true, true},
		{"application/ai-registry", KindRegistry, true, true},
		{"application/ai-skill", KindSkill, false, true},
		{"application/ai-skill+md", KindSkill, false, true},
		{"application/agent-skills+md", KindSkill, false, true},
		{"application/agent-skills+gzip", KindSkill, false, true},
		{"application/ai-skill-archive+gzip", KindSkill, false, true},
		{`text/markdown; profile="urn:air:agent-skills"`, KindSkill, false, true},
		{"application/mcp-server-card+json", KindMCPServer, false, true},
		{"application/mcp-server+json", KindMCPServer, false, true},
		{"application/a2a-agent-card+json", KindA2AAgent, false, true},
		{"application/a2a-agent+json", KindA2AAgent, false, true},
		{"application/json", KindGeneric, false, true},
		{"application/vnd.oai.openapi", KindGeneric, false, true},
		{"application/linkset+json", KindGeneric, false, true},
		{"text/markdown", KindGeneric, false, true},
		{"text/plain", KindGeneric, false, true},
		{"text/html", KindGeneric, false, true},
		{"application/octet-stream", KindUnknown, false, false},
		{"garbage", KindUnknown, false, false},
		{"", KindUnknown, false, false},
		{`application/ai-catalog+json; profile="urn:air:agent-skills"`, KindCatalog, true, true},
	}
	for _, tt := range tests {
		m := Parse(tt.in)
		if got := m.Kind(); got != tt.wantKind {
			t.Errorf("Parse(%q).Kind() = %v, want %v", tt.in, got, tt.wantKind)
		}
		if got := m.IsPointer(); got != tt.pointer {
			t.Errorf("Parse(%q).IsPointer() = %v, want %v", tt.in, got, tt.pointer)
		}
		if got := m.IsKnown(); got != tt.known {
			t.Errorf("Parse(%q).IsKnown() = %v, want %v", tt.in, got, tt.known)
		}
	}
}

func TestProfile(t *testing.T) {
	if got := Parse(`text/markdown; profile="urn:air:agent-skills"`).Profile(); got != "urn:air:agent-skills" {
		t.Errorf("Profile() = %q, want urn:air:agent-skills", got)
	}
	if got := Parse("application/ai-skill").Profile(); got != "" {
		t.Errorf("Profile() = %q, want empty", got)
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		in   string
		want Format
	}{
		{"application/ai-catalog+json", FormatJSON},
		{"application/ai-skill+md", FormatMarkdown},
		{"application/agent-skills+md", FormatMarkdown},
		{"application/agent-skills+zip", FormatZip},
		{"application/ai-skill-archive+gzip", FormatGzip},
		{"text/markdown", FormatMarkdown},
		{"text/html", FormatHTML},
		{"text/plain", FormatText},
		{"application/vnd.oai.openapi", FormatUnknown},
		{"application/ai-skill", FormatUnknown},
		{"garbage", FormatUnknown},
	}
	for _, tt := range tests {
		if got := Parse(tt.in).Format(); got != tt.want {
			t.Errorf("Parse(%q).Format() = %v, want %v", tt.in, got, tt.want)
		}
	}
}
