# Schema provenance

The following files are vendored **verbatim**, byte-for-byte, from the
official ARD specification repository, pinned to a single commit so the
whole spec surface (schema, CDDL, OpenAPI) stays in sync:

- `ai-catalog.schema.json` — the JSON Schema for the `ai-catalog.json`
  capability manifest.
  Source: https://raw.githubusercontent.com/ards-project/ard-spec/4a8a6b8fdd3ac4a50dcb63213573159c1eed7856/spec/schemas/ai-catalog.schema.json
- `ard.cddl` — the CDDL definition of the ARD wire formats.
  Source: https://raw.githubusercontent.com/ards-project/ard-spec/4a8a6b8fdd3ac4a50dcb63213573159c1eed7856/spec/schemas/ard.cddl
- `ard.openapi.yaml` — the OpenAPI description of the ARD discovery
  endpoints.
  Source: https://raw.githubusercontent.com/ards-project/ard-spec/4a8a6b8fdd3ac4a50dcb63213573159c1eed7856/spec/schemas/ard.openapi.yaml

Repository: https://github.com/ards-project/ard-spec
Commit at time of vendoring: `4a8a6b8fdd3ac4a50dcb63213573159c1eed7856` (2026-06-20)
Vendoring date (this vendoring pass): 2026-07-16
Spec version declared by the schema: `specVersion` const `"1.0"`

`ard.cddl` and `ard.openapi.yaml` are reference material only — ardvark does
not currently generate code or perform additional validation from them. They
are vendored alongside the JSON Schema so the three documents that together
define the spec surface are versioned as a unit and can't silently drift
apart.

No modifications were made to any of the three files' contents.

## Deliberate leniency lives in code, not in the schema

`ai-catalog.schema.json` is intentionally left untouched, including its
strict `^urn:air:...` identifier pattern and its `format` keywords. Ardvark's
**default** verification (`ard.Verify`) is deliberately *more lenient* than
this raw schema — that leniency is implemented entirely in
`internal/ard/verify.go`, never by editing the vendored file. A separate
**strict** mode (`ard.VerifyStrict`, or `--strict` on `ardvark verify`)
validates the raw document against the schema exactly as published, with
format assertions enabled as hard errors.

The lenient default's deviations from the raw schema, each called out at its
implementation site in `verify.go`:

1. **Legacy `urn:ai:` identifiers.** The schema's pattern only accepts the
   `air` NID. Catalogs already published in the wild use the older `ai` NID
   (e.g. `urn:ai:unlimit.website:tool:x`). Before schema validation, the
   instance's identifiers are normalized: the `urn:<nid>:` prefix is
   lowercased and a legacy `ai` NID is rewritten to `air`, purely for schema
   validation purposes. `ParseURN` and everything downstream (identifier
   uniqueness, stored identifiers) already accept both NIDs directly and are
   untouched by this normalization.
2. **IDN publisher normalization.** A Unicode-form (U-label) IDN publisher
   (e.g. `café.example`) is converted to its ASCII/punycode form before
   schema validation, since the schema's identifier pattern is ASCII-only.
3. **`"data": null` treated as absent.** An entry with an explicit
   `"data": null` is, for schema-validation purposes, treated the same as an
   entry with no `data` key at all (matching the semantic
   `entry.value_or_reference` check's existing treatment), so a `url` +
   `"data": null` entry validates instead of tripping the schema's
   `oneOf`/`not` value-or-reference constraint.
4. **Format assertions downgraded to warnings.** Strict mode enables
   `format` assertions (via the compiler's `AssertFormat()`) and reports any
   `format`-only failure (e.g. a malformed `uri` or `date-time`) as a
   `schema.validation` error. The lenient default still runs the same format
   checks, but a failure attributable *only* to a `format` assertion is
   downgraded to a warning, so an otherwise-clean catalog with e.g. a
   slightly malformed `updatedAt` timestamp rolls up to
   `valid_with_warnings` rather than `invalid`.
5. **`representativeQueries` count downgraded to a warning.** The schema
   enforces `minItems: 2` / `maxItems: 5`. The design doc treats 2–5 as a
   recommendation, and the semantic `queries.count` check already reports
   violations as a warning; the schema-level `minItems`/`maxItems` failure is
   downgraded to the same warning severity (and de-duplicated against the
   semantic check) rather than also failing the whole document.

## Re-vendoring

Run `just sync-schema` to re-fetch all three files from the upstream `main`
branch and print a diff against what's currently vendored. It does not
auto-commit — after reviewing the diff, update the pinned commit hash above
by hand (and re-check whether any of the leniencies above are still needed).
