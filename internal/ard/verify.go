package ard

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Severity levels for verification checks.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

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

// knownEntryMediaTypes are the ARD media types recognized by entry.type as
// of specVersion 1.0. The spec explicitly says unrecognized types should
// not be strictly enforced, hence this check is a warning.
var knownEntryMediaTypes = map[string]bool{
	"application/a2a-agent-card+json":  true,
	"application/mcp-server-card+json": true,
	"application/ai-catalog+json":      true,
	"application/ai-registry+json":     true,
	"application/ai-skill+json":        true,
	// Forms seen on catalogs published in the wild (e.g. unlimit.website),
	// predating the "-card" suffix in the spec draft.
	"application/mcp-server+json": true,
	"application/a2a-agent+json":  true,
	"application/ai-skill":        true,
}

// TransportChecks runs the verification pipeline's step-1 transport checks
// (all warning-severity): the response's Content-Type indicates JSON, the
// body is within maxBodyBytes (a non-positive maxBodyBytes disables this
// check), and the body is valid UTF-8. These are the caller's
// responsibility rather than Verify's, per Verify's doc comment, since they
// depend on HTTP transport metadata Verify does not have.
func TransportChecks(contentType string, body []byte, maxBodyBytes int64) []Check {
	contentTypeOK := strings.Contains(strings.ToLower(contentType), "json")
	contentTypeMsg := fmt.Sprintf("Content-Type %q indicates JSON", contentType)
	if !contentTypeOK {
		contentTypeMsg = fmt.Sprintf("Content-Type %q does not indicate JSON", contentType)
	}

	sizeOK := maxBodyBytes <= 0 || int64(len(body)) <= maxBodyBytes
	sizeMsg := fmt.Sprintf("body size %d bytes is within the %d byte cap", len(body), maxBodyBytes)
	if !sizeOK {
		sizeMsg = fmt.Sprintf("body size %d bytes exceeds the %d byte cap", len(body), maxBodyBytes)
	}

	utf8OK := utf8.Valid(body)
	utf8Msg := "body is valid UTF-8"
	if !utf8OK {
		utf8Msg = "body is not valid UTF-8"
	}

	return []Check{
		{CheckID: "transport.content_type", Severity: SeverityWarning, Passed: contentTypeOK, Message: contentTypeMsg, SpecRef: specRef, Subject: "catalog"},
		{CheckID: "transport.size", Severity: SeverityWarning, Passed: sizeOK, Message: sizeMsg, SpecRef: specRef, Subject: "catalog"},
		{CheckID: "transport.utf8", Severity: SeverityWarning, Passed: utf8OK, Message: utf8Msg, SpecRef: specRef, Subject: "catalog"},
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
// catalog was fetched from, used by the urn.publisher_matches check.
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
			Subject:  "catalog",
		}}
		report.Verdict = VerdictInvalid
		return report
	}

	report.Checks = append(report.Checks, validateSchema(raw)...)

	var catalog Catalog
	if err := json.Unmarshal(raw, &catalog); err == nil {
		report.Checks = append(report.Checks, semanticChecks(catalog, servingDomain)...)
	}

	report.Verdict = rollUp(report.Checks)
	return report
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
			Subject:  "catalog",
		}}
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return []Check{{
			CheckID:  "schema.parse",
			Severity: SeverityError,
			Passed:   false,
			Message:  fmt.Sprintf("catalog is not valid JSON: %v", err),
			SpecRef:  specRef,
			Subject:  "catalog",
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
			Subject:  "catalog",
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
		return "catalog"
	}
	rest := strings.TrimPrefix(instanceLocation, prefix)
	idxStr := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		idxStr = rest[:i]
	}
	var idx int
	if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
		return "catalog"
	}

	var doc struct {
		Entries []struct {
			Identifier string `json:"identifier"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "catalog"
	}
	if idx < 0 || idx >= len(doc.Entries) {
		return "catalog"
	}
	if id := doc.Entries[idx].Identifier; id != "" {
		return id
	}
	return "catalog"
}

// semanticChecks runs the seven spec-defined semantic checks that JSON
// Schema cannot express.
func semanticChecks(c Catalog, servingDomain string) []Check {
	checks := []Check{
		checkSpecVersion(c),
		checkIdentifierUnique(c),
	}

	for _, e := range c.Entries {
		subject := entrySubject(e)

		checks = append(checks, checkValueOrReference(e, subject))

		urn, urnErr := ParseURN(e.Identifier)
		checks = append(checks, checkURNFormat(urnErr, subject))
		if urnErr == nil {
			checks = append(checks, checkPublisherMatches(urn, servingDomain, subject))
		}

		// representativeQueries describe a callable capability; they are not
		// meaningful for container/pointer entries (a nested catalog or a
		// registry endpoint), so don't warn on their absence there.
		if !isPointerMediaType(e.Type) {
			checks = append(checks, checkQueriesCount(e, subject))
		}
		checks = append(checks, checkMediaType(e, subject))
	}

	return checks
}

func entrySubject(e Entry) string {
	if e.Identifier != "" {
		return e.Identifier
	}
	return "catalog"
}

// checkSpecVersion: catalog.spec_version (error) — specVersion must be "1.0".
func checkSpecVersion(c Catalog) Check {
	passed := c.SpecVersion == "1.0"
	msg := fmt.Sprintf("specVersion is %q", c.SpecVersion)
	if !passed {
		msg = fmt.Sprintf("specVersion must be \"1.0\", got %q", c.SpecVersion)
	}
	return Check{
		CheckID:  "catalog.spec_version",
		Severity: SeverityError,
		Passed:   passed,
		Message:  msg,
		SpecRef:  specRef,
		Subject:  "catalog",
	}
}

// checkIdentifierUnique: identifier.unique (error) — no duplicate URNs
// within a catalog.
func checkIdentifierUnique(c Catalog) Check {
	seen := make(map[string]int, len(c.Entries))
	var dups []string
	for _, e := range c.Entries {
		seen[e.Identifier]++
		if seen[e.Identifier] == 2 {
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
		Subject:  "catalog",
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
// domain vs the serving domain.
func checkPublisherMatches(u URN, servingDomain, subject string) Check {
	passed := strings.EqualFold(u.Publisher, servingDomain)
	msg := fmt.Sprintf("URN publisher %q matches serving domain %q", u.Publisher, servingDomain)
	if !passed {
		msg = fmt.Sprintf("URN publisher %q does not match serving domain %q (may be legitimate for aggregators)", u.Publisher, servingDomain)
	}
	return Check{
		CheckID:  "urn.publisher_matches",
		Severity: SeverityWarning,
		Passed:   passed,
		Message:  msg,
		SpecRef:  specRef,
		Subject:  subject,
	}
}

// checkQueriesCount: queries.count (warning) — 2-5 representativeQueries
// recommended.
// isPointerMediaType reports whether an entry type is a container or endpoint
// pointer (a nested catalog or a registry) rather than a callable capability.
func isPointerMediaType(mediaType string) bool {
	return mediaType == "application/ai-catalog+json" || mediaType == "application/ai-registry+json"
}

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

// checkMediaType: entry.media_type (warning) — unrecognized ARD media type.
func checkMediaType(e Entry, subject string) Check {
	passed := knownEntryMediaTypes[e.Type]
	msg := fmt.Sprintf("type %q is a recognized ARD media type", e.Type)
	if !passed {
		msg = fmt.Sprintf("type %q is not a recognized ARD media type (spec does not enforce this strictly)", e.Type)
	}
	return Check{
		CheckID:  "entry.media_type",
		Severity: SeverityWarning,
		Passed:   passed,
		Message:  msg,
		SpecRef:  specRef,
		Subject:  subject,
	}
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
