package ard

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/helgesverre/ardvark/internal/httpx"
	"github.com/helgesverre/ardvark/internal/mediatype"
)

// Severity levels for verification checks.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// SubjectCatalog is the Check.Subject value for catalog-level checks (as
// opposed to checks scoped to a single entry's URN).
const SubjectCatalog = "catalog"

// Verdicts produced by the roll-up of a Report's checks.
const (
	VerdictValid             = "valid"
	VerdictValidWithWarnings = "valid_with_warnings"
	VerdictInvalid           = "invalid"
)

// specRef points at the section of the design doc that defines the
// verification pipeline and its checks.
const specRef = "docs/superpowers/specs/2026-07-15-ardvark-crawler-design.md#verification-pipeline"

// Check is a single verification result, one row of a catalog's "report
// card".
type Check struct {
	CheckID  string
	Severity string
	Passed   bool
	Message  string
	SpecRef  string
	// Subject is the entry URN the check applies to, or "catalog" for
	// catalog-wide checks.
	Subject string
}

// Report is the full set of verification results for one catalog document,
// plus the rolled-up verdict.
type Report struct {
	Checks  []Check
	Verdict string
}

//go:embed schema/ai-catalog.schema.json
var catalogSchemaJSON []byte

// catalogSchemaURL is the $id declared by the vendored schema; it doubles as
// the resource URL registered with the compiler.
const catalogSchemaURL = "https://raw.githubusercontent.com/ards-project/ard-spec/main/spec/schemas/ai-catalog.schema.json"

var (
	catalogSchemaOnce sync.Once
	catalogSchema     *jsonschema.Schema
	catalogSchemaErr  error
)

// compiledCatalogSchema lazily compiles the vendored ai-catalog.schema.json,
// caching the result for the process lifetime.
func compiledCatalogSchema() (*jsonschema.Schema, error) {
	catalogSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(catalogSchemaJSON))
		if err != nil {
			catalogSchemaErr = fmt.Errorf("ard: parse vendored schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(catalogSchemaURL, doc); err != nil {
			catalogSchemaErr = fmt.Errorf("ard: register vendored schema: %w", err)
			return
		}
		sch, err := c.Compile(catalogSchemaURL)
		if err != nil {
			catalogSchemaErr = fmt.Errorf("ard: compile vendored schema: %w", err)
			return
		}
		catalogSchema = sch
	})
	return catalogSchema, catalogSchemaErr
}

// TransportChecks runs the verification pipeline's step-1 transport checks
// (all warning-severity): the response's Content-Type indicates JSON, the
// body is within maxBodyBytes (a non-positive maxBodyBytes disables this
// check), and the body is valid UTF-8. These are the caller's
// responsibility rather than Verify's, per Verify's doc comment, since they
// depend on HTTP transport metadata Verify does not have.
func TransportChecks(contentType string, body []byte, maxBodyBytes int64) []Check {
	contentTypeOK := httpx.IsJSONContentType(contentType)
	sizeOK := maxBodyBytes <= 0 || int64(len(body)) <= maxBodyBytes
	utf8OK := utf8.Valid(body)

	return []Check{
		newCheck("transport.content_type", SeverityWarning, SubjectCatalog, contentTypeOK,
			fmt.Sprintf("Content-Type %q indicates JSON", contentType),
			fmt.Sprintf("Content-Type %q does not indicate JSON", contentType)),
		newCheck("transport.size", SeverityWarning, SubjectCatalog, sizeOK,
			fmt.Sprintf("body size %d bytes is within the %d byte cap", len(body), maxBodyBytes),
			fmt.Sprintf("body size %d bytes exceeds the %d byte cap", len(body), maxBodyBytes)),
		newCheck("transport.utf8", SeverityWarning, SubjectCatalog, utf8OK,
			"body is valid UTF-8", "body is not valid UTF-8"),
	}
}

// newCheck builds a Check from its pass/fail outcome, picking passMsg or
// failMsg accordingly. Checks whose Message needs more than a binary
// pass/fail choice (e.g. schema.validation's per-leaf detail) construct
// Check directly instead.
func newCheck(id, severity, subject string, passed bool, passMsg, failMsg string) Check {
	msg := passMsg
	if !passed {
		msg = failMsg
	}
	return Check{
		CheckID:  id,
		Severity: severity,
		Passed:   passed,
		Message:  msg,
		SpecRef:  specRef,
		Subject:  subject,
	}
}

// MergeChecks appends extra checks (e.g. from TransportChecks) to report
// and recomputes the rolled-up verdict.
func (r Report) MergeChecks(extra []Check) Report {
	r.Checks = append(r.Checks, extra...)
	r.Verdict = rollUp(r.Checks)
	return r
}

// Verify runs the ARD verification pipeline over a raw ai-catalog.json
// document: JSON Schema validation against the vendored spec schema,
// followed by the seven semantic checks. servingDomain is the host the
// catalog was fetched from, used by the urn.publisher_matches check; when
// empty (e.g. a local file), that check is skipped.
//
// Transport-level checks (content type, size, UTF-8 validity) are the
// caller's responsibility and are not repeated here.
func Verify(raw []byte, servingDomain string) Report {
	var report Report

	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		report.Checks = []Check{{
			CheckID:  "schema.parse",
			Severity: SeverityError,
			Passed:   false,
			Message:  fmt.Sprintf("catalog is not valid JSON: %v", err),
			SpecRef:  specRef,
			Subject:  SubjectCatalog,
		}}
		report.Verdict = VerdictInvalid
		return report
	}

	schemaChecks := validateSchema(raw)
	report.Checks = append(report.Checks, schemaChecks...)

	// Semantic checks re-derive several of the same failures JSON Schema
	// already caught (missing/malformed identifier, bad specVersion, ...),
	// which would double-report the same underlying defect as two rows. Once
	// schema validation has failed, skip semantic checks whose entire
	// purpose is expressible by the schema (spec_version, value_or_reference,
	// urn.format via a missing/malformed identifier) and only keep the
	// checks that find genuinely additional problems the schema cannot
	// express (duplicate identifiers, publisher/domain mismatch, query
	// count, media type). See checkSchemaFailed docs for why we don't just
	// skip semantic checks outright: they still catch real, distinct issues
	// even on an otherwise-invalid catalog.
	var catalog Catalog
	if err := json.Unmarshal(raw, &catalog); err == nil {
		report.Checks = append(report.Checks, semanticChecks(catalog, servingDomain, schemaFailed(schemaChecks))...)
	}

	report.Verdict = rollUp(report.Checks)
	return report
}

// schemaFailed reports whether any JSON Schema validation check failed.
func schemaFailed(checks []Check) bool {
	for _, c := range checks {
		if !c.Passed {
			return true
		}
	}
	return false
}

// normalizeIdentifiersForSchema returns raw with each entries[i].identifier
// publisher segment normalized to ASCII/punycode, for JSON Schema validation
// purposes only. The vendored schema's identifier pattern is ASCII-only
// ("[a-zA-Z0-9.-]+" for the publisher segment), so a catalog with a
// Unicode-form (U-label) IDN publisher — which ParseURN accepts fine — would
// otherwise fail schema.validation purely on encoding, not on any real
// defect. If raw doesn't parse as a document with an "entries" array, or
// nothing needs normalizing, raw is returned unchanged.
func normalizeIdentifiersForSchema(raw []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	entries, ok := doc["entries"].([]any)
	if !ok {
		return raw
	}

	changed := false
	for _, entryAny := range entries {
		entry, ok := entryAny.(map[string]any)
		if !ok {
			continue
		}
		id, ok := entry["identifier"].(string)
		if !ok {
			continue
		}
		if normalized := normalizeIdentifierPublisherASCII(id); normalized != id {
			entry["identifier"] = normalized
			changed = true
		}
	}
	if !changed {
		return raw
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// validateSchema runs JSON Schema validation and turns each leaf failure
// into a Check row carrying the instance location in its message.
func validateSchema(raw []byte) []Check {
	sch, err := compiledCatalogSchema()
	if err != nil {
		return []Check{{
			CheckID:  "schema.parse",
			Severity: SeverityError,
			Passed:   false,
			Message:  fmt.Sprintf("internal error compiling vendored schema: %v", err),
			SpecRef:  specRef,
			Subject:  SubjectCatalog,
		}}
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(normalizeIdentifiersForSchema(raw)))
	if err != nil {
		return []Check{{
			CheckID:  "schema.parse",
			Severity: SeverityError,
			Passed:   false,
			Message:  fmt.Sprintf("catalog is not valid JSON: %v", err),
			SpecRef:  specRef,
			Subject:  SubjectCatalog,
		}}
	}

	verr := sch.Validate(instance)
	if verr == nil {
		return nil
	}

	ve, ok := verr.(*jsonschema.ValidationError)
	if !ok {
		return []Check{{
			CheckID:  "schema.validation",
			Severity: SeverityError,
			Passed:   false,
			Message:  verr.Error(),
			SpecRef:  specRef,
			Subject:  SubjectCatalog,
		}}
	}

	basic := ve.BasicOutput()
	leaves := basic.Errors
	if len(leaves) == 0 {
		leaves = []jsonschema.OutputUnit{*basic}
	}

	checks := make([]Check, 0, len(leaves))
	for _, leaf := range leaves {
		loc := leaf.InstanceLocation
		if loc == "" {
			loc = "(root)"
		}
		detail := "schema validation failed"
		if leaf.Error != nil {
			detail = leaf.Error.String()
		}
		checks = append(checks, Check{
			CheckID:  "schema.validation",
			Severity: SeverityError,
			Passed:   false,
			Message:  fmt.Sprintf("at %s: %s", loc, detail),
			SpecRef:  specRef,
			Subject:  schemaSubject(leaf.InstanceLocation, raw),
		})
	}
	return checks
}

// schemaSubject best-efforts a Subject (entry URN or "catalog") for a JSON
// Schema failure by resolving its RFC 6901 instance location against the
// raw document.
func schemaSubject(instanceLocation string, raw []byte) string {
	const prefix = "/entries/"
	if !strings.HasPrefix(instanceLocation, prefix) {
		return SubjectCatalog
	}
	rest := strings.TrimPrefix(instanceLocation, prefix)
	idxStr := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		idxStr = rest[:i]
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		return SubjectCatalog
	}

	var doc struct {
		Entries []struct {
			Identifier string `json:"identifier"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return SubjectCatalog
	}
	if idx < 0 || idx >= len(doc.Entries) {
		return SubjectCatalog
	}
	if id := doc.Entries[idx].Identifier; id != "" {
		return id
	}
	return SubjectCatalog
}

// semanticChecks runs the seven spec-defined semantic checks that JSON
// Schema cannot express. schemaFailed indicates JSON Schema validation
// already failed for this document: in that case, checks whose entire
// purpose duplicates a constraint the schema itself already enforces
// (catalog.spec_version, entry.value_or_reference, urn.format) are skipped
// so the same defect isn't reported twice. Checks that find genuinely
// additional problems the schema cannot express (duplicate identifiers,
// publisher/domain mismatch, representativeQueries count, unrecognized
// media type) still run regardless.
func semanticChecks(c Catalog, servingDomain string, schemaFailed bool) []Check {
	checks := []Check{
		checkIdentifierUnique(c),
	}
	if !schemaFailed {
		checks = append(checks, checkSpecVersion(c))
	}

	for _, e := range c.Entries {
		subject := entrySubject(e)

		if !schemaFailed {
			checks = append(checks, checkValueOrReference(e, subject))
		}

		urn, urnErr := ParseURN(e.Identifier)
		if !schemaFailed {
			checks = append(checks, checkURNFormat(urnErr, subject))
		}
		// An empty servingDomain means the catalog wasn't fetched from a
		// host (e.g. a local file), so there is nothing to compare the URN
		// publisher against and the check is skipped rather than warned.
		if urnErr == nil && servingDomain != "" {
			checks = append(checks, checkPublisherMatches(urn, servingDomain, subject))
		}

		mt := mediatype.Parse(e.Type)

		// representativeQueries describe a callable capability; they are not
		// meaningful for container/pointer entries (a nested catalog or a
		// registry endpoint), so don't warn on their absence there.
		if !mt.IsPointer() {
			checks = append(checks, checkQueriesCount(e, subject))
		}
		checks = append(checks, checkMediaType(e, mt, subject))
	}

	return checks
}

func entrySubject(e Entry) string {
	if e.Identifier != "" {
		return e.Identifier
	}
	return SubjectCatalog
}

// checkSpecVersion: catalog.spec_version (error) — specVersion must be "1.0".
func checkSpecVersion(c Catalog) Check {
	passed := c.SpecVersion == "1.0"
	return newCheck("catalog.spec_version", SeverityError, SubjectCatalog, passed,
		fmt.Sprintf("specVersion is %q", c.SpecVersion),
		fmt.Sprintf("specVersion must be \"1.0\", got %q", c.SpecVersion))
}

// checkIdentifierUnique: identifier.unique (error) — no duplicate URNs
// within a catalog. Comparison is on the normalized parsed URN (lowercased
// NID/publisher, ASCII-normalized publisher) rather than the raw string, so
// e.g. "urn:air:Example.com:a" and "urn:AIR:example.com:a" are caught as the
// same identifier. Entries whose identifier fails to parse fall back to raw
// string comparison so malformed URNs are still deduped against each other.
func checkIdentifierUnique(c Catalog) Check {
	seen := make(map[string]int, len(c.Entries))
	var dups []string
	for _, e := range c.Entries {
		key := e.Identifier
		if u, err := ParseURN(e.Identifier); err == nil {
			key = u.String()
		}
		seen[key]++
		if seen[key] == 2 {
			dups = append(dups, e.Identifier)
		}
	}
	passed := len(dups) == 0
	msg := "all entry identifiers are unique"
	if !passed {
		msg = fmt.Sprintf("duplicate identifiers found: %s", strings.Join(dups, ", "))
	}
	return Check{
		CheckID:  "identifier.unique",
		Severity: SeverityError,
		Passed:   passed,
		Message:  msg,
		SpecRef:  specRef,
		Subject:  SubjectCatalog,
	}
}

// checkValueOrReference: entry.value_or_reference (error) — exactly one of
// url / data.
func checkValueOrReference(e Entry, subject string) Check {
	hasURL := e.URL != ""
	hasData := len(e.Data) > 0 && string(e.Data) != "null"
	passed := hasURL != hasData
	msg := "exactly one of url or data is present"
	switch {
	case hasURL && hasData:
		msg = "both url and data are present; exactly one is required"
	case !hasURL && !hasData:
		msg = "neither url nor data is present; exactly one is required"
	}
	return Check{
		CheckID:  "entry.value_or_reference",
		Severity: SeverityError,
		Passed:   passed,
		Message:  msg,
		SpecRef:  specRef,
		Subject:  subject,
	}
}

// checkURNFormat: urn.format (error) — urn:air:<publisher>:<namespace…>:<name>
// grammar, RFC 8141.
func checkURNFormat(urnErr error, subject string) Check {
	passed := urnErr == nil
	msg := "identifier is a well-formed ARD URN"
	if !passed {
		msg = urnErr.Error()
	}
	return Check{
		CheckID:  "urn.format",
		Severity: SeverityError,
		Passed:   passed,
		Message:  msg,
		SpecRef:  specRef,
		Subject:  subject,
	}
}

// checkPublisherMatches: urn.publisher_matches (warning) — URN publisher
// domain vs the serving domain, compared on registrable domain (eTLD+1)
// rather than exact host equality, so a catalog served from "www.example.com"
// with publisher "example.com" (or vice versa), or from a different
// subdomain of the same registrable domain, does not warn.
func checkPublisherMatches(u URN, servingDomain, subject string) Check {
	passed := registrableDomainEqual(u.Publisher, servingDomain)
	return newCheck("urn.publisher_matches", SeverityWarning, subject, passed,
		fmt.Sprintf("URN publisher %q matches serving domain %q", u.Publisher, servingDomain),
		fmt.Sprintf("URN publisher %q does not match serving domain %q (may be legitimate for aggregators)", u.Publisher, servingDomain))
}

// registrableDomainEqual reports whether a and b share the same registrable
// domain (eTLD+1, e.g. "example.com" for both "www.example.com" and
// "api.example.com"). Both inputs are ASCII/punycode-normalized first (via
// idna, falling back to the original string on conversion failure) so a
// Unicode-form IDN host compares equal to its punycode form. It falls back
// to exact case-insensitive host comparison when either input isn't a
// domain publicsuffix recognizes (e.g. a bare single-label host, an IP
// address, or an unlisted TLD), so behavior degrades to the old exact-match
// semantics rather than silently passing.
func registrableDomainEqual(a, b string) bool {
	a, b = asciiDomain(a), asciiDomain(b)
	if strings.EqualFold(a, b) {
		return true
	}
	aReg, aErr := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(a))
	bReg, bErr := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(b))
	if aErr != nil || bErr != nil {
		return false
	}
	return strings.EqualFold(aReg, bReg)
}

// asciiDomain best-effort converts a possibly-Unicode (U-label) domain to
// its ASCII/punycode (A-label) form; on failure it returns s unchanged.
func asciiDomain(s string) string {
	if ascii, err := idna.ToASCII(strings.ToLower(s)); err == nil {
		return ascii
	}
	return s
}

// checkQueriesCount: queries.count (warning) — 2-5 representativeQueries
// recommended.
func checkQueriesCount(e Entry, subject string) Check {
	n := len(e.RepresentativeQueries)
	passed := n >= 2 && n <= 5
	return Check{
		CheckID:  "queries.count",
		Severity: SeverityWarning,
		Passed:   passed,
		Message:  fmt.Sprintf("%d representativeQueries present (recommended: 2-5)", n),
		SpecRef:  specRef,
		Subject:  subject,
	}
}

// checkMediaType: entry.media_type (warning) — unrecognized media type. A type
// classifies as known when it maps to any Kind (including a recognized non-ARD
// artifact type); the message reports the classified kind and encoding hint.
func checkMediaType(e Entry, mt mediatype.MediaType, subject string) Check {
	passed := mt.IsKnown()
	return newCheck("entry.media_type", SeverityWarning, subject, passed,
		fmt.Sprintf("type %q recognized as %s (format: %s)", e.Type, mt.Kind(), mt.Format()),
		fmt.Sprintf("type %q is not a recognized ARD media type (spec does not enforce this strictly)", e.Type))
}

// rollUp derives the overall verdict: any failed error-severity check wins
// as invalid; failed warnings alone downgrade to valid_with_warnings;
// otherwise valid.
func rollUp(checks []Check) string {
	warnFailed := false
	for _, c := range checks {
		if c.Passed {
			continue
		}
		if c.Severity == SeverityError {
			return VerdictInvalid
		}
		warnFailed = true
	}
	if warnFailed {
		return VerdictValidWithWarnings
	}
	return VerdictValid
}
