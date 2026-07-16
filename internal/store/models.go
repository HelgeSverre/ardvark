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
	// DedupKey is the frontier's uniqueness key: at most one row per distinct
	// (kind, natural key) may exist. It is a hex-encoded SHA-256 of
	// "kind:natural" (see internal/crawler.dedupKey), so it is always exactly
	// 64 lowercase hex chars — fixed width, driver-independent, and safely
	// under MySQL's utf8mb4 768-char unique-index key limit — regardless of how
	// long the source URL is. The column must stay at size:64: it must not be
	// widened toward the URL column's size:2048, which would exceed the index
	// key limit and fail migration. Upgrading from <=0.4.0 (which stored raw
	// "kind:natural" strings here) re-keys existing rows because new enqueues
	// hash the key; AutoMigrate narrows the column and old rows simply stop
	// dedup-matching new lookups, which is benign for a crawler (see dedupKey).
	DedupKey string `gorm:"uniqueIndex;size:64"`

	// HostShard is fnv32a(fetchHost) % HostShardCount, computed once at
	// enqueue time (see internal/frontier.Enqueue), where fetchHost is the
	// hostname of URL when URL is non-empty and parses, or Host otherwise.
	// It must be derived from the host that will actually be dialed, not
	// from Host: entry follow-ups (catalog_fetch/artifact_fetch/
	// registry_harvest built from an entry or ref URL) set Host to the
	// *parent* catalog's host for attribution purposes even when URL points
	// at a completely different host (e.g. an artifact hosted on a CDN
	// domain), so using Host for sharding would route the HTTP request to a
	// worker that does not own that foreign host. HostShard partitions the
	// frontier by fetch-target host for distributed crawling: N worker
	// processes each configured with a distinct crawler.worker.index
	// (0..count-1) can restrict Dequeue to "host_shard % count = index", so
	// every host is owned by exactly one worker for the crawl's lifetime.
	// This is what makes per-process politeness (internal/fetch's in-memory
	// rate limiter) correct without any cross-process coordination — see
	// internal/fetch's package doc.
	HostShard int `gorm:"index"`

	// -- Provenance columns -------------------------------------------------
	//
	// These carry the context a handler needs to attribute its result (which
	// catalog a nested catalog_fetch belongs to, which entry an
	// artifact_fetch/registry_harvest was declared by, ...). They are set by
	// the enqueuing side immediately before the item is written to the
	// frontier and read by the dequeuing side's handler, so this data
	// survives both process restarts and, critically, a different worker
	// process dequeuing the item than the one that enqueued it (see
	// internal/crawler's package doc).

	// ParentCatalogID is the catalogs.id of the catalog that referenced this
	// catalog_fetch item's URL as a nested catalog entry. Nil for
	// top-level catalogs (discovered via host_probe or a link_tag hint).
	ParentCatalogID *uint `gorm:"index"`
	// ArtifactEntryID is the catalog_entries.id that declared this
	// artifact_fetch item's URL.
	ArtifactEntryID *uint `gorm:"index"`
	// RegistryEntryID is the catalog_entries.id that declared this
	// registry_harvest item's registry (the entry whose media type is
	// application/ai-registry+json).
	RegistryEntryID *uint `gorm:"index"`
	// RegistryCatalogID is the catalogs.id that harvested registry entries
	// should be attributed to.
	RegistryCatalogID *uint `gorm:"index"`
	// RegistryRowID is the registries.id row for this registry_harvest
	// item's registry (or referral), whose harvest_status/last_harvested_at
	// the handler updates.
	RegistryRowID *uint `gorm:"index"`
	// ProbeMethod is which probe method (well_known, robots_agentmap,
	// link_tag) discovered this catalog_fetch item's URL, reported on the
	// verified-catalog ProbeEvent.
	ProbeMethod string `gorm:"size:32"`

	// LeasedUntil is set when a worker dequeues an item (status moves to
	// in_flight): it is now plus the frontier's configured lease duration.
	// A distributed reclaimer (frontier.ReclaimExpired) resets any in_flight
	// row whose lease has passed back to pending, so a worker process that
	// dies mid-item does not strand that item forever — this is what makes
	// multiple worker processes sharing one mysql/postgres database safe.
	// Expiry is judged by comparing this timestamp (written by the leasing
	// worker's clock) against the reclaimer's own clock, which may be a
	// different worker process entirely — so distributed deployments must
	// keep worker clocks reasonably synchronized (e.g. NTP). Clock skew
	// approaching the configured lease duration can cause premature
	// reclaim: a worker still legitimately processing an item may have its
	// lease stolen out from under it by a reclaimer whose clock runs ahead.
	// Cleared (nil) whenever an item leaves in_flight (Complete/Fail/Requeue).
	LeasedUntil *time.Time `gorm:"index"`
	// WorkerID identifies which worker process currently holds the lease
	// (informational — reclaiming is decided purely by LeasedUntil, not by
	// which worker is recorded here). Cleared alongside LeasedUntil.
	WorkerID string `gorm:"size:64"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// HostShardCount is the modulus FrontierItem.HostShard is computed against.
// 8192 is large enough that even a huge worker count (far beyond any
// realistic deployment) still gets a reasonably even distribution, while
// staying a cheap, fixed, portable modulus across sqlite/mysql/postgres (the
// '%' operator works identically on all three; MOD() does not exist on
// sqlite).
const HostShardCount = 8192

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
