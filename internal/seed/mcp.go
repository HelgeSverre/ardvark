package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// mcpRegistryDefaultURL is the official MCP registry's public API base URL
// (https://github.com/modelcontextprotocol/registry). Configurable via
// seed.mcp.registryUrl since the registry's deployment location and API
// shape were still evolving at the time of writing.
const mcpRegistryDefaultURL = "https://registry.modelcontextprotocol.io"

// mcpRegistryPageSize is the page size requested per listing call.
const mcpRegistryPageSize = 100

// mcpRegistryMaxPages bounds how many pages are walked per Domains call, so
// a pathologically large n (or a registry that never stops paginating)
// can't spin forever.
const mcpRegistryMaxPages = 100

// MCPRegistrySeeder fetches server listings from the official MCP registry
// and extracts candidate domains from each entry: every remote endpoint's
// host, plus a domain decoded from the server's reverse-DNS-style name (the
// registry requires publishers to namespace names under a domain they
// control, e.g. "io.github.acme/foo-server" or "com.acme.tools/foo-server",
// which reverse to "acme.github.io" / "tools.acme.com"). It implements
// Seeder with Source() "mcp_registry".
type MCPRegistrySeeder struct {
	// RegistryURL is the registry's API base URL. Defaults to
	// mcpRegistryDefaultURL if empty.
	RegistryURL string

	// HTTPClient is used for all requests. Defaults to a client with a 30s
	// timeout if nil.
	HTTPClient *http.Client
}

// NewMCPRegistrySeeder returns an MCPRegistrySeeder reading the given
// registry base URL (empty uses the default official registry).
func NewMCPRegistrySeeder(registryURL string) *MCPRegistrySeeder {
	return &MCPRegistrySeeder{RegistryURL: registryURL}
}

// Source implements Seeder.
func (m *MCPRegistrySeeder) Source() string { return "mcp_registry" }

func (m *MCPRegistrySeeder) registryURL() string {
	if m.RegistryURL != "" {
		return m.RegistryURL
	}
	return mcpRegistryDefaultURL
}

func (m *MCPRegistrySeeder) httpClient() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

type mcpRegistryListResponse struct {
	Servers  []mcpRegistryServerEntry `json:"servers"`
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"metadata"`
}

type mcpRegistryServerEntry struct {
	Server mcpRegistryServer `json:"server"`
}

type mcpRegistryServer struct {
	Name    string `json:"name"`
	Remotes []struct {
		URL string `json:"url"`
	} `json:"remotes"`
}

// Domains implements Seeder: it pages through the MCP registry's server
// listing, extracts candidate domains per entry, sanitizes and dedupes
// them, and returns up to n.
func (m *MCPRegistrySeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: mcp: n must be positive, got %d", n)
	}

	var names []string
	cursor := ""
	for page := 0; page < mcpRegistryMaxPages; page++ {
		resp, err := m.listPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		for _, entry := range resp.Servers {
			names = append(names, domainsFromMCPServer(entry.Server)...)
		}
		if len(Sanitize(names)) >= n || resp.Metadata.NextCursor == "" || len(resp.Servers) == 0 {
			break
		}
		cursor = resp.Metadata.NextCursor
	}

	sanitized := Sanitize(names)
	if len(sanitized) > n {
		sanitized = sanitized[:n]
	}
	return sanitized, nil
}

// listPage fetches one page of the registry's server listing.
func (m *MCPRegistrySeeder) listPage(ctx context.Context, cursor string) (*mcpRegistryListResponse, error) {
	endpoint, err := url.Parse(strings.TrimSuffix(m.registryURL(), "/") + "/v0/servers")
	if err != nil {
		return nil, fmt.Errorf("seed: mcp: invalid registry URL: %w", err)
	}
	q := endpoint.Query()
	q.Set("limit", fmt.Sprintf("%d", mcpRegistryPageSize))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("seed: mcp: request to %s: %w", endpoint.String(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("seed: mcp: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seed: mcp: %s returned status %d: %s", endpoint.String(), resp.StatusCode, string(body))
	}

	var parsed mcpRegistryListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("seed: mcp: decoding response: %w", err)
	}
	return &parsed, nil
}

// domainsFromMCPServer extracts candidate domains from one registry server
// entry: each remote endpoint's host, plus a domain decoded from the
// server's reverse-DNS-style name where derivable.
func domainsFromMCPServer(s mcpRegistryServer) []string {
	var names []string
	for _, r := range s.Remotes {
		if u, err := url.Parse(r.URL); err == nil && u.Host != "" {
			names = append(names, u.Host)
		}
	}
	if d := domainFromReverseDNSName(s.Name); d != "" {
		names = append(names, d)
	}
	return names
}

// domainFromReverseDNSName decodes a domain from the MCP registry's
// required naming convention: server names are namespaced under a
// reverse-DNS-style prefix the publisher controls (Java-package style),
// e.g. "io.github.acme/foo-server" or "com.acme.tools/foo-server". Reversing
// the full label order recovers the domain: "acme.github.io" /
// "tools.acme.com". Sanitize's hostname validation drops anything malformed
// downstream regardless, so this stays a best-effort heuristic.
func domainFromReverseDNSName(name string) string {
	namespace, _, ok := strings.Cut(name, "/")
	if !ok {
		return ""
	}
	labels := strings.Split(namespace, ".")
	if len(labels) < 2 {
		return ""
	}
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	return strings.ToLower(strings.Join(labels, "."))
}
