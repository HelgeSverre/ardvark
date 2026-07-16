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
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
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
// caching the result for the process lifetime. Format assertions
// (AssertFormat) are always enabled: strict mode wants format failures
// reported as errors, and lenient mode wants them reported too, just
// downgraded to warnings (see validateSchema) — either way the compiler must
// actually assert formats for a leaf failure to exist to classify.
func compiledCatalogSchema() (*jsonschema.Schema, error) {
	catalogSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(catalogSchemaJSON))
		if err != nil {
			catalogSchemaErr = fmt.Errorf("ard: parse vendored schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
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
// document in lenient mode (the ardvark default): JSON Schema validation
// against the vendored spec schema — with several deliberate leniencies
// relative to the raw schema, documented at schema/PROVENANCE.md and at each
// leniency's implementation site below — followed by the seven semantic
// checks. servingDomain is the host the catalog was fetched from, used by
// the urn.publisher_matches check; when empty (e.g. a local file), that
// check is skipped.
//
// Transport-level checks (content type, size, UTF-8 validity) are the
// caller's responsibility and are not repeated here.
func Verify(raw []byte, servingDomain string) Report {
	return verify(raw, servingDomain, false)
}

// VerifyStrict runs the same verification pipeline as Verify, but validates
// the raw document against the vendored spec schema exactly as published:
// no identifier/data normalization, and format assertion failures (e.g. a
// malformed uri or date-time) are reported as errors rather than downgraded
// to warnings. A legacy "urn:ai:" identifier, which Verify accepts, fails
// schema validation here since the raw schema's pattern only permits the
// "air" NID.
func VerifyStrict(raw []byte, servingDomain string) Report {
	return verify(raw, servingDomain, true)
}

// verify is the shared core of Verify and VerifyStrict.
func verify(raw []byte, servingDomain string, strict bool) Report {
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

	schemaChecks := validateSchema(raw, strict)
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

// schemaFailed reports whether any error-severity JSON Schema validation
// check failed. Lenient mode downgrades some schema failures (format
// assertions) to warning severity rather than dropping them, and those
// downgraded rows must not trip the semantic-check skip logic below — a
// catalog failing only on a downgraded format warning should still get the
// full set of semantic checks.
func schemaFailed(checks []Check) bool {
	for _, c := range checks {
		if !c.Passed && c.Severity == SeverityError {
			return true
		}
	}
	return false
}

// normalizeIdentifierForSchema rewrites the "urn:<nid>:" prefix of a raw
// identifier string to the lowercase, canonical "urn:air:" form, then
// ASCII/punycode-normalizes its publisher segment, for JSON Schema
// validation purposes only. This folds two deliberate lenient-mode
// deviations from the raw vendored schema into one step:
//
//   - Legacy "urn:ai:" identifiers (seen on catalogs published in the wild
//     before the spec settled on "air") are rewritten to "urn:air:" so they
//     pass the schema's ASCII pattern, which only accepts "air". ParseURN
//     and everything downstream already accept both NIDs directly; this
//     rewrite exists purely so schema validation doesn't reject a document
//     ardvark otherwise treats as valid.
//   - An uppercase/mixed-case "URN:AIR:" (or "urn:Ai:") prefix is lowercased,
//     since the schema's pattern is a case-sensitive literal match on
//     "urn:air:" while URN schemes and NIDs are case-insensitive per RFC
//     8141.
//
// If s doesn't look like a recognized ARD URN, s is returned unchanged and
// schema validation reports whatever it would have reported anyway.
func normalizeIdentifierForSchema(s string) string {
	lower := strings.ToLower(s)
	for _, candidate := range urnNIDs {
		prefix := "urn:" + candidate + ":"
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		rewritten := "urn:air:" + s[len(prefix):]
		return normalizeIdentifierPublisherASCII(rewritten)
	}
	return s
}

// normalizeForLenientSchema returns raw with each entries[i] normalized for
// JSON Schema validation purposes only, applying two deliberate lenient-mode
// deviations from the raw vendored schema (see normalizeIdentifierForSchema
// and the "data": null case below, and schema/PROVENANCE.md for the full
// list). If raw doesn't parse as a document with an "entries" array, or
// nothing needs normalizing, raw is returned unchanged.
func normalizeForLenientSchema(raw []byte) []byte {
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
		if id, ok := entry["identifier"].(string); ok {
			if normalized := normalizeIdentifierForSchema(id); normalized != id {
				entry["identifier"] = normalized
				changed = true
			}
		}
		// An explicit "data": null is, semantically, the same as data being
		// absent (checkValueOrReference already treats it that way); strip
		// the key so the schema's value-or-reference oneOf/not constraint
		// doesn't see both "url" and "data" present and reject the entry.
		if v, exists := entry["data"]; exists && v == nil {
			delete(entry, "data")
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
// into a Check row carrying the instance location in its message. In strict
// mode, the raw document is validated as-is and every leaf failure is an
// error. In lenient mode, the instance is first normalized (see
// normalizeForLenientSchema), and two further deviations are applied per
// leaf: a representativeQueries minItems/maxItems failure is dropped
// entirely (the semantic queries.count check already reports it, as a
// warning, so keeping both would double-report the same defect), and a
// format-assertion failure is downgraded to a warning rather than dropped
// (there's no semantic check duplicating it, so it still needs to surface).
func validateSchema(raw []byte, strict bool) []Check {
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

	instanceBytes := raw
	if !strict {
		instanceBytes = normalizeForLenientSchema(raw)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(instanceBytes))
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

	// DetailedOutput, not BasicOutput, is used deliberately: BasicOutput's
	// flattening collapses a $ref indirection (every catalogEntry is reached
	// through "entries.items" -> "$ref") into its parent node and, in doing
	// so, overwrites that leaf's ErrorKind with the wrapping *kind.Reference
	// — which would break the Kind-based classification below (isFormatFailure,
	// isRepresentativeQueriesCountFailure) for every per-entry failure.
	// DetailedOutput preserves the real per-leaf ErrorKind and message;
	// collectLeaves walks its (possibly multi-level, for combinators like
	// oneOf) tree down to the true leaves itself.
	leaves := collectLeaves(*ve.DetailedOutput())

	checks := make([]Check, 0, len(leaves))
	for _, leaf := range leaves {
		if !strict && isRepresentativeQueriesCountFailure(leaf) {
			continue
		}

		severity := SeverityError
		if !strict && isFormatFailure(leaf) {
			severity = SeverityWarning
		}

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
			Severity: severity,
			Passed:   false,
			Message:  fmt.Sprintf("at %s: %s", loc, detail),
			SpecRef:  specRef,
			Subject:  schemaSubject(leaf.InstanceLocation, raw),
		})
	}
	return checks
}

// collectLeaves recursively walks a jsonschema.OutputUnit tree (as produced
// by ValidationError.DetailedOutput) and returns its true leaves: units with
// no nested Errors. Combinator keywords (oneOf, anyOf, allOf, not, and $ref
// indirection) show up as intermediate units carrying their own (generally
// unhelpful, e.g. "'not' failed") Error alongside nested Errors for the
// actual cause; only the deepest units — which carry the real per-property
// ErrorKind and message — are returned.
func collectLeaves(unit jsonschema.OutputUnit) []jsonschema.OutputUnit {
	if len(unit.Errors) == 0 {
		if unit.Error == nil {
			return nil
		}
		return []jsonschema.OutputUnit{unit}
	}
	var leaves []jsonschema.OutputUnit
	for _, child := range unit.Errors {
		leaves = append(leaves, collectLeaves(child)...)
	}
	return leaves
}

// isFormatFailure reports whether leaf is a "format" keyword assertion
// failure (e.g. a malformed "uri" or "date-time"), as opposed to a
// structural schema failure.
func isFormatFailure(leaf jsonschema.OutputUnit) bool {
	if leaf.Error == nil {
		return false
	}
	_, ok := leaf.Error.Kind.(*kind.Format)
	return ok
}

// isRepresentativeQueriesCountFailure reports whether leaf is a minItems or
// maxItems failure on an entry's representativeQueries array — the schema's
// 2-5 item constraint, which the design doc treats as a recommendation
// rather than a hard rule.
func isRepresentativeQueriesCountFailure(leaf jsonschema.OutputUnit) bool {
	if leaf.Error == nil {
		return false
	}
	if !strings.HasSuffix(leaf.InstanceLocation, "/representativeQueries") {
		return false
	}
	switch leaf.Error.Kind.(type) {
	case *kind.MinItems, *kind.MaxItems:
		return true
	default:
		return false
	}
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
