// Package config loads and validates the ardvark.json configuration file:
// storage, logging, crawler politeness, ARD verification depth, registry
// harvesting, and pluggable seed-source settings (CT logs, crt.sh, Tranco).
// All keys are optional; missing values fall back to documented defaults.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var defaultPrinter = message.NewPrinter(language.English)

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
	CT     CTSeedConfig     `json:"ct"`
	Crtsh  CrtshSeedConfig  `json:"crtsh"`
	Tranco TrancoSeedConfig `json:"tranco"`
}

// CTSeedConfig controls Certificate Transparency log seeding
// (`ardvark seed ct`).
type CTSeedConfig struct {
	LogURL     string `json:"logUrl"`
	EntryCount int    `json:"entryCount"`
}

// CrtshSeedConfig controls crt.sh seeding (`ardvark seed crtsh`).
type CrtshSeedConfig struct {
	Endpoint string `json:"endpoint"`
}

// TrancoSeedConfig controls Tranco top-domains list seeding
// (`ardvark seed tranco`).
type TrancoSeedConfig struct {
	ListURL string `json:"listUrl"`
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
			MaxBodyBytes:             5242880,
			UserAgent:                "ardvark/0.1 (+https://github.com/helgesverre/ardvark)",
			RespectRobotsTxt:         true,
			RefreshAfterHours:        168,
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
				LogURL:     "https://oak.ct.letsencrypt.org/2026h2/",
				EntryCount: 1000,
			},
			Crtsh: CrtshSeedConfig{
				Endpoint: "https://crt.sh",
			},
			Tranco: TrancoSeedConfig{
				ListURL: "https://tranco-list.eu/top-1m.csv.zip",
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
// default values (defaults are applied by merging the raw file over a
// defaults document before validation and decoding).
func Load(path string) (Config, error) {
	defaults := Defaults()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaults, nil
		}
		return Config{}, fmt.Errorf("config: reading %s: %w", path, err)
	}

	return LoadBytes(raw)
}

// LoadBytes parses and validates raw JSON config bytes, merging over
// defaults. Exposed separately from Load for testing without touching disk.
func LoadBytes(raw []byte) (Config, error) {
	defaults := Defaults()

	// Merge raw file content over a JSON representation of the defaults so
	// any keys omitted in the file keep their default value.
	defaultsJSON, err := json.Marshal(defaults)
	if err != nil {
		return Config{}, fmt.Errorf("config: marshalling defaults: %w", err)
	}

	var defaultsMap map[string]any
	if err := json.Unmarshal(defaultsJSON, &defaultsMap); err != nil {
		return Config{}, fmt.Errorf("config: unmarshalling defaults: %w", err)
	}

	var fileMap map[string]any
	if err := json.Unmarshal(raw, &fileMap); err != nil {
		return Config{}, fmt.Errorf("config: invalid JSON: %w", err)
	}

	merged := mergeMaps(defaultsMap, fileMap)

	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return Config{}, fmt.Errorf("config: marshalling merged config: %w", err)
	}

	if err := Validate(mergedJSON); err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := json.Unmarshal(mergedJSON, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: decoding merged config: %w", err)
	}

	return cfg, nil
}

// mergeMaps recursively overlays override onto base, returning a new map.
// Only JSON object values are merged recursively; arrays and scalars in
// override fully replace the base value.
func mergeMaps(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		if baseVal, ok := result[k]; ok {
			baseMap, baseIsMap := baseVal.(map[string]any)
			overrideMap, overrideIsMap := v.(map[string]any)
			if baseIsMap && overrideIsMap {
				result[k] = mergeMaps(baseMap, overrideMap)
				continue
			}
		}
		result[k] = v
	}
	return result
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
