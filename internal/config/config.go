// Package config loads and validates the ardvark.json configuration file:
// storage, logging, crawler politeness, ARD verification depth, registry
// harvesting, and pluggable seed-source settings (CT logs, crt.sh, Tranco,
// GitHub code search, the MCP registry, curated lists, Common Crawl). All
// keys are optional; missing
// values fall back to documented defaults.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var defaultPrinter = message.NewPrinter(language.English)

// DefaultMaxBodyBytes is the default crawler.maxBodyBytes cap (5 MiB). It is
// exported so the fetch layer's zero-value fallback can't drift from the
// documented config default.
const DefaultMaxBodyBytes int64 = 5 << 20

// Config is the root ardvark.json shape.
type Config struct {
	Storage  StorageConfig  `json:"storage"`
	Log      LogConfig      `json:"log"`
	Crawler  CrawlerConfig  `json:"crawler"`
	ARD      ARDConfig      `json:"ard"`
	Registry RegistryConfig `json:"registry"`
	Seed     SeedConfig     `json:"seed"`
}

// StorageConfig selects the database backend.
type StorageConfig struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
}

// LogConfig configures the JSONL event log.
type LogConfig struct {
	File  string `json:"file"`
	Level string `json:"level"`
}

// CrawlerConfig controls HTML harvesting and fetch politeness.
type CrawlerConfig struct {
	Concurrency              int     `json:"concurrency"`
	MaxDepth                 int     `json:"maxDepth"`
	MaxPagesPerDomain        int     `json:"maxPagesPerDomain"`
	PerHostRequestsPerSecond float64 `json:"perHostRequestsPerSecond"`
	RequestTimeoutSeconds    int     `json:"requestTimeoutSeconds"`
	MaxBodyBytes             int64   `json:"maxBodyBytes"`
	UserAgent                string  `json:"userAgent"`
	RespectRobotsTxt         bool    `json:"respectRobotsTxt"`
	RefreshAfterHours        int     `json:"refreshAfterHours"`
	// LeaseSeconds is how long a dequeued frontier item's in_flight lease
	// lasts before internal/frontier's ReclaimExpired considers it
	// abandoned and returns it to pending. This only matters for
	// distributed crawling (mysql/postgres, multiple worker processes): a
	// worker that dies mid-item can no longer requeue it itself, so the
	// lease is what lets another worker eventually pick it back up. It must
	// outlast the slowest legitimate handler (registry_harvest pagination
	// plus retry backoff — see frontier.defaultLeaseSeconds for the
	// derivation of the 600s default) so a live worker's item is not
	// reclaimed out from under it. 0 (or unset) means "use the frontier
	// package's own default" rather than "no lease" — see
	// frontier.defaultLeaseSeconds.
	LeaseSeconds int `json:"leaseSeconds"`
	// Worker configures this process's slice of a distributed crawl. See
	// WorkerConfig.
	Worker WorkerConfig `json:"worker"`
}

// WorkerConfig identifies this process among N cooperating worker
// processes sharing one mysql/postgres frontier (see
// internal/frontier.WithWorkerShard and store.FrontierItem.HostShard).
// Meaningless (and unvalidated beyond its own bounds) for a single-process
// sqlite crawl, where Count defaults to 1 and sharding is a no-op.
//
// Shard assignment is static: hosts hash to shards, shards belong to a
// fixed (Index, Count) partition. A worker that dies permanently strands
// its partition's pending items (never leased, so lease-expiry reclaim
// never touches them), and its surviving peers keep polling forever
// because the global pending count stays non-zero. Recovery is
// operational, not automatic: restart the missing worker (same Index and
// Count), or restart the fleet with a new Count.
type WorkerConfig struct {
	// Index is this process's position among Count cooperating workers
	// (0-based). Must be less than Count — validated at config load (see
	// Validate) rather than left for internal/frontier to discover, since
	// an out-of-range index would silently dequeue nothing forever instead
	// of failing fast at startup.
	Index int `json:"index"`
	// Count is the total number of cooperating worker processes sharing
	// the frontier. 1 (the default) means "no distributed sharding" — every
	// host is dequeued by this one process, matching today's behavior.
	Count int `json:"count"`
}

// ARDConfig controls catalog resolution depth.
type ARDConfig struct {
	MaxCatalogDepth int  `json:"maxCatalogDepth"`
	FetchArtifacts  bool `json:"fetchArtifacts"`
}

// RegistryConfig controls ARD registry harvesting.
type RegistryConfig struct {
	Harvest          bool `json:"harvest"`
	MaxReferralDepth int  `json:"maxReferralDepth"`
	PageLimit        int  `json:"pageLimit"`
}

// SeedConfig groups settings for every pluggable seed source under
// internal/seed (see the design doc's "Seeding" section).
type SeedConfig struct {
	CT          CTSeedConfig          `json:"ct"`
	Crtsh       CrtshSeedConfig       `json:"crtsh"`
	Tranco      TrancoSeedConfig      `json:"tranco"`
	GitHub      GitHubSeedConfig      `json:"github"`
	MCPRegistry MCPRegistrySeedConfig `json:"mcp"`
	Curated     CuratedSeedConfig     `json:"curated"`
	CommonCrawl CommonCrawlSeedConfig `json:"commoncrawl"`
}

// CTSeedConfig controls Certificate Transparency log seeding
// (`ardvark seed ct`). Logs are resolved dynamically from LogListURL rather
// than hardcoded, since CT shards rotate every few months.
type CTSeedConfig struct {
	LogListURL string   `json:"logListUrl"`
	Logs       []string `json:"logs"`
	EntryCount int      `json:"entryCount"`
}

// CrtshSeedConfig controls crt.sh seeding (`ardvark seed crtsh`).
type CrtshSeedConfig struct {
	Endpoint string `json:"endpoint"`
	// Count is the default number of domains to enqueue, overridden by
	// --count. Kept separate from seed.ct.entryCount so tuning one source's
	// default doesn't silently change another's.
	Count int `json:"count"`
}

// TrancoSeedConfig controls Tranco top-domains list seeding
// (`ardvark seed tranco`).
type TrancoSeedConfig struct {
	ListURL string `json:"listUrl"`
	// Top is the default number of top-ranked domains to enqueue,
	// overridden by --top.
	Top int `json:"top"`
}

// GitHubSeedConfig controls GitHub code-search seeding
// (`ardvark seed github`).
type GitHubSeedConfig struct {
	// Query is the GitHub code-search query used to find catalog files.
	// Defaults to filename:ai-catalog.json path:.well-known.
	Query string `json:"query"`
	// Count is the default number of domains to enqueue, overridden by
	// --count.
	Count int `json:"count"`
}

// MCPRegistrySeedConfig controls MCP registry seeding (`ardvark seed mcp`).
type MCPRegistrySeedConfig struct {
	// RegistryURL is the base URL of the MCP registry's server-listing API.
	RegistryURL string `json:"registryUrl"`
	// Count is the default number of domains to enqueue, overridden by
	// --count.
	Count int `json:"count"`
}

// CuratedSeedConfig controls curated awesome-list seeding
// (`ardvark seed curated`).
type CuratedSeedConfig struct {
	// URLs are the list documents scanned for candidate domains. Overridden
	// (replaced, not appended to) by repeated --url flags.
	URLs []string `json:"urls"`
	// Count is the default number of domains to enqueue, overridden by
	// --count.
	Count int `json:"count"`
}

// CommonCrawlSeedConfig controls Common Crawl web-graph domain-ranks seeding
// (`ardvark seed commoncrawl`).
type CommonCrawlSeedConfig struct {
	// GraphInfoURL lists available web-graph releases (newest first).
	GraphInfoURL string `json:"graphInfoUrl"`
	// Graph pins a release id (e.g. "cc-main-2026-apr-may-jun"); empty uses
	// the newest release. Overridden by --graph.
	Graph string `json:"graph"`
	// Top is the default number of top-ranked domains to enqueue,
	// overridden by --top.
	Top int `json:"top"`
	// Offset skips the first Offset ranked domains, overridden by --offset.
	Offset int `json:"offset"`
}

// Defaults returns a Config populated with the documented defaults from the
// ardvark design spec.
func Defaults() Config {
	return Config{
		Storage: StorageConfig{
			Driver: "sqlite",
			DSN:    "ardvark.db",
		},
		Log: LogConfig{
			File:  "ardvark.jsonl",
			Level: "info",
		},
		Crawler: CrawlerConfig{
			Concurrency:              8,
			MaxDepth:                 2,
			MaxPagesPerDomain:        50,
			PerHostRequestsPerSecond: 1,
			RequestTimeoutSeconds:    15,
			MaxBodyBytes:             DefaultMaxBodyBytes,
			UserAgent:                "ardvark/0.1 (+https://github.com/helgesverre/ardvark)",
			RespectRobotsTxt:         true,
			RefreshAfterHours:        168,
			LeaseSeconds:             600,
			Worker:                   WorkerConfig{Index: 0, Count: 1},
		},
		ARD: ARDConfig{
			MaxCatalogDepth: 3,
			FetchArtifacts:  true,
		},
		Registry: RegistryConfig{
			Harvest:          true,
			MaxReferralDepth: 2,
			PageLimit:        20,
		},
		Seed: SeedConfig{
			CT: CTSeedConfig{
				LogListURL: "https://www.gstatic.com/ct/log_list/v3/log_list.json",
				// Several high-volume DV operators; whichever shards are
				// usable and current get used, so seeding keeps working as
				// individual logs are retired and rotated.
				Logs:       []string{"oak", "argon", "nimbus"},
				EntryCount: 1000,
			},
			Crtsh: CrtshSeedConfig{
				Endpoint: "https://crt.sh",
				Count:    1000,
			},
			Tranco: TrancoSeedConfig{
				ListURL: "https://tranco-list.eu/top-1m.csv.zip",
				Top:     1000,
			},
			GitHub: GitHubSeedConfig{
				Query: "filename:ai-catalog.json path:.well-known",
				Count: 100,
			},
			MCPRegistry: MCPRegistrySeedConfig{
				RegistryURL: "https://registry.modelcontextprotocol.io",
				Count:       1000,
			},
			Curated: CuratedSeedConfig{
				URLs: []string{
					"https://raw.githubusercontent.com/punkpeye/awesome-mcp-servers/main/README.md",
					"https://raw.githubusercontent.com/wong2/awesome-mcp-servers/main/README.md",
					"https://raw.githubusercontent.com/appcypher/awesome-mcp-servers/main/README.md",
				},
				Count: 500,
			},
			CommonCrawl: CommonCrawlSeedConfig{
				GraphInfoURL: "https://index.commoncrawl.org/graphinfo.json",
				Graph:        "",
				Top:          1000,
				Offset:       0,
			},
		},
	}
}

// Load reads and validates the config file at path. If path does not exist,
// Load returns pure defaults (this is not an error). If the file exists but
// is invalid JSON or fails schema validation, Load returns a descriptive
// error.
//
// Values present in the file override defaults; missing keys keep their
// default values (the raw file is decoded over a Defaults() value, so
// json.Unmarshal fills in only the fields the file specifies).
func Load(path string) (Config, error) {
	defaults := Defaults()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaults, nil
		}
		return Config{}, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg, err := LoadBytes(raw)
	if err != nil {
		return cfg, err
	}
	return anchorPaths(cfg, filepath.Dir(path)), nil
}

// anchorPaths resolves the config's relative file paths against base, the
// directory containing the loaded config file. Relative paths in a config
// file are relative to the file, not to whatever directory the process
// happens to run from — so a config in ~/.config/ardvark/ keeps its database
// and event log there unless it says otherwise. For a config in the working
// directory base is ".", making this a no-op. Only plain file paths are
// touched: sqlite DSNs that aren't file: URIs or :memory:, and the event log
// path. Server DSNs (mysql, postgres) pass through untouched.
func anchorPaths(cfg Config, base string) Config {
	if cfg.Storage.Driver == "sqlite" && isPlainRelPath(cfg.Storage.DSN) {
		cfg.Storage.DSN = filepath.Join(base, cfg.Storage.DSN)
	}
	if isPlainRelPath(cfg.Log.File) {
		cfg.Log.File = filepath.Join(base, cfg.Log.File)
	}
	return cfg
}

// isPlainRelPath reports whether p is a relative filesystem path that is
// safe to re-anchor: not empty, not absolute, and not a sqlite special form
// (file: URI or :memory:).
func isPlainRelPath(p string) bool {
	return p != "" && p != ":memory:" &&
		!strings.HasPrefix(p, "file:") && !filepath.IsAbs(p)
}

// LoadBytes parses and validates raw JSON config bytes, decoding over
// defaults. Exposed separately from Load for testing without touching disk.
// The schema has no required fields, so validating the raw file directly is
// equivalent to validating it merged over defaults; json.Unmarshal into a
// Defaults() value then overwrites only the fields the file specifies
// (nested objects merge field-by-field, arrays and scalars replace).
func LoadBytes(raw []byte) (Config, error) {
	if err := Validate(raw); err != nil {
		return Config{}, err
	}

	// The schema's "integer" accepts zero-fraction floats (2.0, 1e3), which
	// json.Unmarshal rejects for int fields. Round-tripping through any
	// (float64) re-renders them in integer form, so files that pass
	// validation also decode.
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Config{}, fmt.Errorf("config: decoding config: %w", err)
	}
	normalized, err := json.Marshal(doc)
	if err != nil {
		return Config{}, fmt.Errorf("config: decoding config: %w", err)
	}

	cfg := Defaults()
	if err := json.Unmarshal(normalized, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: decoding config: %w", err)
	}

	if err := validateSemantics(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// validateSemantics checks cross-field constraints the JSON schema cannot
// express on its own (the schema only bounds each field individually).
func validateSemantics(cfg Config) error {
	if cfg.Crawler.Worker.Index >= cfg.Crawler.Worker.Count {
		return fmt.Errorf(
			"config: crawler.worker.index (%d) must be less than crawler.worker.count (%d)",
			cfg.Crawler.Worker.Index, cfg.Crawler.Worker.Count,
		)
	}
	return nil
}

// Validate checks raw JSON config bytes against the embedded config schema,
// returning friendly, field-scoped error messages on failure.
func Validate(raw []byte) error {
	compiler := jsonschema.NewCompiler()

	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(configSchemaJSON))
	if err != nil {
		return fmt.Errorf("config: parsing embedded schema: %w", err)
	}
	if err := compiler.AddResource("config.schema.json", schemaDoc); err != nil {
		return fmt.Errorf("config: loading embedded schema: %w", err)
	}

	schema, err := compiler.Compile("config.schema.json")
	if err != nil {
		return fmt.Errorf("config: compiling embedded schema: %w", err)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("config: invalid JSON: %w", err)
	}

	if err := schema.Validate(instance); err != nil {
		if verr, ok := err.(*jsonschema.ValidationError); ok {
			return friendlyValidationError(verr)
		}
		return fmt.Errorf("config: validation failed: %w", err)
	}

	return nil
}

// friendlyValidationError converts a jsonschema.ValidationError tree into a
// single human-friendly error message, e.g.:
//
//	config.storage.driver: must be one of "sqlite", "mysql", "postgres"
func friendlyValidationError(verr *jsonschema.ValidationError) error {
	leaves := collectLeaves(verr)
	if len(leaves) == 0 {
		return fmt.Errorf("config: %s", verr.Error())
	}

	msgs := make([]string, 0, len(leaves))
	for _, leaf := range leaves {
		msgs = append(msgs, formatLeaf(leaf))
	}
	return fmt.Errorf("config: %s", strings.Join(msgs, "; "))
}

// collectLeaves walks a ValidationError tree and returns the deepest,
// most specific causes (the leaves), which carry the most useful messages.
func collectLeaves(verr *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(verr.Causes) == 0 {
		return []*jsonschema.ValidationError{verr}
	}
	var leaves []*jsonschema.ValidationError
	for _, c := range verr.Causes {
		leaves = append(leaves, collectLeaves(c)...)
	}
	return leaves
}

func formatLeaf(verr *jsonschema.ValidationError) string {
	loc := instanceLocationPath(verr.InstanceLocation)
	if loc == "" {
		loc = "config"
	} else {
		loc = "config." + loc
	}
	return fmt.Sprintf("%s: %s", loc, verr.ErrorKind.LocalizedString(defaultPrinter))
}

// instanceLocationPath converts a JSON pointer-style instance location
// (e.g. []string{"storage", "driver"}) into a dotted path.
func instanceLocationPath(loc []string) string {
	return strings.Join(loc, ".")
}
