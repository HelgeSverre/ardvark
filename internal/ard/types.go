// Package ard defines Go types for the Agentic Resource Discovery (ARD)
// ai-catalog.json document format, plus URN parsing for the
// urn:air:<publisher>:<namespace...>:<name> grammar.
package ard

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Catalog is the top-level ai-catalog.json document.
type Catalog struct {
	SpecVersion string   `json:"specVersion"`
	Host        HostInfo `json:"host"`
	Entries     []Entry  `json:"entries"`
}

// HostInfo describes the publisher of the catalog.
type HostInfo struct {
	DisplayName      string         `json:"displayName"`
	Identifier       string         `json:"identifier,omitempty"`
	DocumentationURL string         `json:"documentationUrl,omitempty"`
	LogoURL          string         `json:"logoUrl,omitempty"`
	TrustManifest    *TrustManifest `json:"trustManifest,omitempty"`
}

// Entry is a single agentic resource entry in a catalog.
type Entry struct {
	Identifier            string          `json:"identifier"`
	DisplayName           string          `json:"displayName"`
	Type                  string          `json:"type"`
	URL                   string          `json:"url,omitempty"`
	Data                  json.RawMessage `json:"data,omitempty"`
	Description           string          `json:"description,omitempty"`
	Tags                  []string        `json:"tags,omitempty"`
	Capabilities          []string        `json:"capabilities,omitempty"`
	RepresentativeQueries []string        `json:"representativeQueries,omitempty"`
	Version               string          `json:"version,omitempty"`
	UpdatedAt             string          `json:"updatedAt,omitempty"`
	Metadata              map[string]any  `json:"metadata,omitempty"`
	TrustManifest         *TrustManifest  `json:"trustManifest,omitempty"`
}

// TrustManifest carries verbatim trust/attestation data for a host or entry.
// Cryptographic verification is out of scope for v1; data is stored as-is.
type TrustManifest struct {
	Identity     string           `json:"identity"`
	IdentityType string           `json:"identityType,omitempty"`
	TrustSchema  *TrustSchema     `json:"trustSchema,omitempty"`
	Attestations []Attestation    `json:"attestations,omitempty"`
	Provenance   []ProvenanceLink `json:"provenance,omitempty"`
	Signature    string           `json:"signature,omitempty"`
}

// TrustSchema describes the trust framework applied to the artifact.
type TrustSchema struct {
	Identifier          string   `json:"identifier"`
	Version             string   `json:"version"`
	GovernanceURI       string   `json:"governanceUri,omitempty"`
	VerificationMethods []string `json:"verificationMethods,omitempty"`
}

// Attestation is a single verifiable claim (SOC 2 audit, compliance
// statement, HIPAA check, etc.) referenced by a trust manifest.
type Attestation struct {
	Type      string `json:"type"`
	URI       string `json:"uri"`
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest,omitempty"`
}

// ProvenanceLink is a single lineage entry in a trust manifest's provenance
// trail.
type ProvenanceLink struct {
	Relation     string `json:"relation"`
	SourceID     string `json:"sourceId"`
	SourceDigest string `json:"sourceDigest,omitempty"`
}

// URN represents a parsed ARD URN:
// urn:<nid>:<publisher>:<namespace...>:<name>, where nid is "air" or "ai".
type URN struct {
	NID       string // "air" or "ai" (lowercased)
	Publisher string
	Namespace []string
	Name      string
}

// urnNIDs are the accepted namespace identifiers. "air" is the identifier in
// the ARD spec draft; "ai" appears on catalogs already published in the wild
// (e.g. unlimit.website), so we accept both.
var urnNIDs = []string{"air", "ai"}

// ParseURN parses an ARD identifier URN of the grammar
// urn:<nid>:<publisher>:<namespace...>:<name>, where nid is "air" or "ai",
// publisher is a FQDN, namespace is zero or more colon-separated segments,
// and name is the mandatory terminal segment.
func ParseURN(s string) (URN, error) {
	// RFC 8141: the "urn:" scheme and the NID are case-insensitive.
	lower := strings.ToLower(s)
	nid := ""
	rest := ""
	for _, candidate := range urnNIDs {
		prefix := "urn:" + candidate + ":"
		if strings.HasPrefix(lower, prefix) {
			nid = candidate
			rest = s[len(prefix):]
			break
		}
	}
	if nid == "" {
		return URN{}, fmt.Errorf("ard: invalid urn %q: must start with \"urn:air:\" or \"urn:ai:\"", s)
	}
	if rest == "" {
		return URN{}, fmt.Errorf("ard: invalid urn %q: missing publisher and name", s)
	}

	segments := strings.Split(rest, ":")
	for _, seg := range segments {
		if seg == "" {
			return URN{}, fmt.Errorf("ard: invalid urn %q: empty segment not allowed", s)
		}
	}

	if len(segments) < 2 {
		return URN{}, fmt.Errorf("ard: invalid urn %q: requires at least publisher and name", s)
	}

	publisher := segments[0]
	name := segments[len(segments)-1]
	namespace := segments[1 : len(segments)-1]

	if err := validatePublisher(publisher); err != nil {
		return URN{}, fmt.Errorf("ard: invalid urn %q: %w", s, err)
	}

	return URN{
		NID:       nid,
		Publisher: publisher,
		Namespace: namespace,
		Name:      name,
	}, nil
}

// validatePublisher does a light sanity check that publisher looks like an
// FQDN: lowercase, at least one dot, no whitespace.
func validatePublisher(publisher string) error {
	if publisher != strings.ToLower(publisher) {
		return fmt.Errorf("publisher %q must be lowercase", publisher)
	}
	if !strings.Contains(publisher, ".") {
		return fmt.Errorf("publisher %q must be a fully-qualified domain name", publisher)
	}
	if strings.ContainsAny(publisher, " \t\n") {
		return fmt.Errorf("publisher %q must not contain whitespace", publisher)
	}
	return nil
}
