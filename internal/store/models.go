// Package store implements the GORM-backed persistence layer for ardvark:
// the nine tables described in the design doc's data-model section
// (crawl_runs, frontier_items, domains, probes, catalogs, catalog_entries,
// artifacts, registries, verification_checks), plus a Store type exposing
// the focused methods the crawler needs.
package store

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// LongText is a string column that maps to an effectively unbounded text
// type on every supported dialect. A bare `type:text` caps at 64 KB on
// MySQL/MariaDB, which real-world catalog documents and artifact bodies
// exceed; Postgres and SQLite TEXT are unbounded.
type LongText string

// GormDBDataType implements schema.GormDBDataTypeInterface.
func (LongText) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "mysql" {
		return "LONGTEXT"
	}
	return "TEXT"
}

// Frontier item kinds.
const (
	KindPageFetch       = "page_fetch"
	KindHostProbe       = "host_probe"
	KindCatalogFetch    = "catalog_fetch"
	KindArtifactFetch   = "artifact_fetch"
	KindRegistryHarvest = "registry_harvest"
)

// Frontier item statuses.
const (
	FrontierStatusPending  = "pending"
	FrontierStatusInFlight = "in_flight"
	FrontierStatusDone     = "done"
	FrontierStatusFailed   = "failed"
)

// Domain discovery sources.
const (
	DiscoverySourceSeed             = "seed"
	DiscoverySourceAnchor           = "anchor"
	DiscoverySourceURLList          = "url_list"
	DiscoverySourceCatalogRef       = "catalog_ref"
	DiscoverySourceRegistryReferral = "registry_referral"
	DiscoverySourceCTLog            = "ct_log"
	DiscoverySourceCrtsh            = "crtsh"
	DiscoverySourceTranco           = "tranco"
	DiscoverySourceGitHub           = "github"
	DiscoverySourceMCPRegistry      = "mcp_registry"
	DiscoverySourceCurated          = "curated_list"
	DiscoverySourceCommonCrawl      = "commoncrawl"
)

// Domain ARD status.
const (
	ARDStatusUnprobed     = "unprobed"
	ARDStatusNotFound     = "not_found"
	ARDStatusFoundInvalid = "found_invalid"
	ARDStatusFoundValid   = "found_valid"
)

// Probe methods.
const (
	ProbeMethodWellKnown      = "well_known"
	ProbeMethodRobotsAgentmap = "robots_agentmap"
	ProbeMethodLinkTag        = "link_tag"
)

// Probe outcomes.
const (
	ProbeOutcomeHit   = "hit"
	ProbeOutcomeMiss  = "miss"
	ProbeOutcomeError = "error"
)

// Catalog verification statuses.
const (
	VerificationStatusValid             = "valid"
	VerificationStatusValidWithWarnings = "valid_with_warnings"
	VerificationStatusInvalid           = "invalid"
)

// Catalog entry source.
const (
	EntrySourceCatalog  = "catalog"
	EntrySourceRegistry = "registry"
)

// Verification check subject types.
const (
	SubjectTypeCatalog = "catalog"
	SubjectTypeEntry   = "entry"
)

// Verification check severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Registry harvest statuses.
const (
	HarvestStatusPending = "pending"
	HarvestStatusOK      = "ok"
	HarvestStatusError   = "error"
)

// Artifact fetch statuses.
const (
	FetchStatusOK    = "ok"
	FetchStatusError = "error"
)

// CrawlRun records one invocation of the crawler.
type CrawlRun struct {
	ID             uint `gorm:"primarykey"`
	StartedAt      time.Time
	FinishedAt     *time.Time
	ConfigSnapshot string `gorm:"type:text"` // JSON

	PagesFetched  int
	HostsProbed   int
	CatalogsFound int
	CatalogsValid int
	Errors        int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// FrontierItem is one unit of work in the persistent crawl frontier.
type FrontierItem struct {
	ID        uint   `gorm:"primarykey"`
	RunID     uint   `gorm:"index"`
	Kind      string `gorm:"index;size:32"`
	URL       string `gorm:"size:2048"`
	Host      string `gorm:"index;size:255"`
	Depth     int
	Priority  int    `gorm:"index"`
	Status    string `gorm:"index;size:16"`
	Attempts  int
	LastError string `gorm:"type:text"`
	DedupKey  string `gorm:"uniqueIndex;size:512"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Domain is a discovered host and its ARD probing state.
type Domain struct {
	ID              uint   `gorm:"primarykey"`
	Host            string `gorm:"uniqueIndex;size:255"`
	FirstSeenAt     time.Time
	LastProbedAt    *time.Time
	DiscoverySource string `gorm:"size:32"`
	ARDStatus       string `gorm:"size:32"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Probe is one attempt to discover an ARD document on a domain.
type Probe struct {
	ID          uint   `gorm:"primarykey"`
	DomainID    uint   `gorm:"index"`
	Method      string `gorm:"size:32"`
	URL         string `gorm:"size:2048"`
	HTTPStatus  int
	ContentType string `gorm:"size:255"`
	Outcome     string `gorm:"size:16"`
	ErrorDetail string `gorm:"type:text"`
	ProbedAt    time.Time

	CreatedAt time.Time
}

// Catalog is a fetched and verified ai-catalog.json document.
type Catalog struct {
	ID                 uint   `gorm:"primarykey"`
	DomainID           uint   `gorm:"index"`
	SourceURL          string `gorm:"size:2048"`
	ParentCatalogID    *uint  `gorm:"index"`
	SpecVersion        string `gorm:"size:32"`
	HostDisplayName    string `gorm:"size:255"`
	HostIdentifier     string `gorm:"size:255"`
	RawJSON            LongText
	ContentHash        string `gorm:"index;size:64"`
	FetchedAt          time.Time
	VerificationStatus string `gorm:"size:32"`

	Entries []CatalogEntry `gorm:"foreignKey:CatalogID"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CatalogEntry is a single agentic resource entry within a catalog.
type CatalogEntry struct {
	ID                    uint   `gorm:"primarykey"`
	CatalogID             uint   `gorm:"index"`
	URN                   string `gorm:"index;size:512"`
	URNPublisher          string `gorm:"index;size:255"`
	URNNamespace          string `gorm:"size:512"`
	URNName               string `gorm:"size:255"`
	DisplayName           string `gorm:"size:255"`
	MediaType             string `gorm:"size:128"`
	RefURL                string `gorm:"size:2048"`
	HasEmbeddedData       bool
	Description           string   `gorm:"type:text"`
	Version               string   `gorm:"size:64"`
	EntryUpdatedAt        string   `gorm:"size:64"`
	Tags                  string   `gorm:"type:text"` // JSON
	Capabilities          string   `gorm:"type:text"` // JSON
	RepresentativeQueries string   `gorm:"type:text"` // JSON
	TrustManifest         LongText // JSON, verbatim
	RawJSON               LongText
	Source                string `gorm:"size:16"`
	SourceRegistryID      *uint  `gorm:"index"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Artifact is the fetched referenced document for a catalog entry (agent
// card, MCP server card, ...).
type Artifact struct {
	ID          uint   `gorm:"primarykey"`
	EntryID     uint   `gorm:"index"`
	URL         string `gorm:"size:2048"`
	HTTPStatus  int
	ContentType string `gorm:"size:255"`
	// RawBody holds the artifact verbatim. Artifacts can be binary (skill
	// tarballs, gzip archives), so this must be a byte column: text columns
	// reject non-UTF-8 on Postgres and MySQL (and cap at 64 KB on MySQL).
	RawBody     []byte
	ContentHash string `gorm:"index;size:64"`
	FetchedAt   time.Time
	FetchStatus string `gorm:"size:16"`

	CreatedAt time.Time
}

// Registry is an ARD registry discovered via a catalog entry.
type Registry struct {
	ID               uint   `gorm:"primarykey"`
	EntryID          uint   `gorm:"index"`
	BaseURL          string `gorm:"size:2048"`
	LastHarvestedAt  *time.Time
	HarvestStatus    string `gorm:"size:16"`
	ReferralSourceID *uint  `gorm:"index"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// VerificationCheck is one check result recorded for a catalog or entry.
type VerificationCheck struct {
	ID          uint   `gorm:"primarykey"`
	SubjectType string `gorm:"index;size:16"`
	SubjectID   uint   `gorm:"index"`
	CheckID     string `gorm:"size:64"`
	Severity    string `gorm:"size:16"`
	Passed      bool
	Message     string `gorm:"type:text"`
	SpecRef     string `gorm:"size:255"`
	CheckedAt   time.Time

	CreatedAt time.Time
}
