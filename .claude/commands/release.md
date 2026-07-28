---
description: Cut and verify an ardvark release end to end (gate, tag, GitHub release, Homebrew tap, website deploy)
argument-hint: "[version] e.g. 0.6.0"
---

Cut release **$ARGUMENTS** of ardvark.

Releasing here is four separate systems that can each fail independently: the
GitHub release (goreleaser), the Homebrew tap, the website (Vercel, *not* wired
into CI), and the published catalog on that website. A green Actions run does
not mean all four succeeded — see the traps below. Verify each one against its
live artifact, never against the job status alone.

Work through the phases in order. Stop and report if a gate fails; do not
proceed past a red gate to "fix it after".

## Traps this procedure exists to catch

1. **The Homebrew job reports `success` when it silently skips.** It gates on
   `HOMEBREW_TAP_TOKEN` and emits a `::notice::` then exits 0 if the secret is
   absent. It is also skipped for any tag containing `-` (prereleases). Always
   verify the formula's actual version in the tap repo.
2. **The website is deployed by hand.** No workflow deploys it. Tagging a
   release does nothing to ardvark.no. It is the step most likely to be
   forgotten.
3. **Only committed files may be published.** The CLI defaults to
   `./ardvark.db`, so running it from `website/` drops a database next to the
   static files. `just deploy-website` deploys a clean export of `HEAD` rather
   than the working directory, so untracked files cannot reach the CDN at all;
   `website/.vercelignore` is a second layer. Deploying by hand from `website/`
   throws that guarantee away — don't.
4. **The smoke test is not in CI.** It needs Docker. CI never runs it, so
   changes to `internal/store`, `internal/crawler`, or `internal/frontier` are
   unverified against MySQL/Postgres until you run it locally.

## Phase 1 — Preflight

```sh
git branch --show-current          # expect: main
git status --porcelain             # expect: empty
git fetch origin && git status -sb # expect: no divergence from origin/main
git tag --sort=-v:refname | head -3
```

Pick the version from the change set, pre-1.0 semantics: new features or any
behaviour change → minor bump; fixes only → patch. State the choice and why.

## Phase 2 — Gate

```sh
just check                 # gofmt + go vet + go test ./...
golangci-lint run ./...    # expect: 0 issues
goreleaser check           # validate .goreleaser.yaml
```

If the diff touches `internal/store`, `internal/crawler`, or
`internal/frontier`, also run the containerized smoke test on **both**
backends — CI cannot:

```sh
cd tools/smoketest && ./run.sh && DB=postgres ./run.sh
```

Expect 4/4 scenarios PASS on each. It tears down its own containers; confirm
with `docker ps -a --filter name=^sm-` (expect empty).

## Phase 3 — Docs and CHANGELOG

Audit for claims the release makes stale — discovery mechanisms, status enums,
config defaults, command help:

- `README.md`
- `SKILLS.md` and `website/SKILLS.md`
- `website/docs.html`
- `internal/ard/schema/PROVENANCE.md` (spec/vendoring status)
- `docs/superpowers/specs/` (design docs describing changed behaviour)

Then promote the CHANGELOG, matching the existing style:

- Move `## [Unreleased]` content under `## [X.Y.Z] - YYYY-MM-DD`, leaving an
  empty `[Unreleased]` above it.
- Keep the `### Added` / `### Fixed` / `### Upgrading` headings.
- Add a `### Known limitations` section for anything deliberately not fixed —
  say so plainly rather than letting it look complete.
- Update the link refs at the bottom: point `[Unreleased]` at
  `compare/vX.Y.Z...HEAD` and add `[X.Y.Z]: compare/vPREV...vX.Y.Z`.

Anything in `### Upgrading` that requires operator action (destructive
migrations, changed request volume, new statuses) must be stated explicitly.

## Phase 4 — Commit and push

Split into coherent commits rather than one blob — the repo's history is
readable and should stay that way. Group by theme, ordering so each commit
builds. Follow the existing style: imperative subject, sentence case, no
`feat:`/`fix:` prefixes, body explaining *why*, wrapped at ~72 chars.

The CHANGELOG promotion is conventionally its own commit
("Promote CHANGELOG to X.Y.Z" or "Document the X.Y.Z ...").

```sh
go build ./...   # after each commit
git push origin main
```

Wait for CI on the push to go green before tagging:

```sh
gh run list --limit 3
```

## Phase 5 — Tag

`just release vX.Y.Z` does `git tag -a` + push, but prefer an annotated tag
with real release notes:

```sh
git tag -a vX.Y.Z -m "vX.Y.Z

Added
- ...

Fixed
- ...
"
git push origin vX.Y.Z
```

The tag push is what triggers `.github/workflows/release.yml`.

## Phase 6 — Verify the GitHub release

```sh
gh run list --limit 3
gh run view <run-id> --json jobs --jq '.jobs[] | "\(.name): \(.conclusion)"'
gh release view vX.Y.Z --json name,tagName,isDraft,assets \
  --jq '{name,tagName,isDraft,assets:[.assets[].name]}'
```

Expect both jobs `success`, `isDraft: false`, and six assets:
`ardvark-{darwin,linux}-{amd64,arm64}`, `ardvark-windows-amd64.exe`,
`checksums.txt`.

Sanity-check the injected version (ldflags `-X main.version`):

```sh
go build -o /tmp/ardvark-rel ./cmd/ardvark && /tmp/ardvark-rel --version
```

## Phase 7 — Verify the Homebrew tap (do not trust the job status)

```sh
curl -sS https://raw.githubusercontent.com/HelgeSverre/homebrew-tap/main/Formula/ardvark.rb | grep -E 'version|url.*vX\.Y\.Z' | head -5
curl -sS "https://api.github.com/repos/HelgeSverre/homebrew-tap/commits?per_page=1" \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print(d[0]['commit']['message'].splitlines()[0], d[0]['commit']['committer']['date'])"
```

Expect `version "X.Y.Z"`, asset URLs pointing at `vX.Y.Z`, and a tap commit
`ardvark vX.Y.Z` dated just after the release run. If the formula still shows
the previous version while the job was green, the token gate skipped it.

## Phase 8 — Deploy the website

Static site on Vercel, linked via `website/.vercel/project.json`. Not in CI.

```sh
cd website && npx vercel whoami   # expect: helgesverre
cd .. && just deploy-website
```

The recipe exports `HEAD` to a staging directory and deploys that, so only
committed files can be published and the deploy always matches a reviewable
commit. It refuses to run while `website/` has uncommitted changes — commit
them rather than working around it, or the live site silently diverges from
the tag you just cut. It prints the exact file list it is publishing; skim it.

## Phase 9 — Verify the live site

```sh
curl -sS -o /dev/null -w 'ard.json        %{http_code} %{content_type}\n' https://ardvark.no/.well-known/ard.json
curl -sS -o /dev/null -w 'ai-catalog.json %{http_code} %{content_type}\n' https://ardvark.no/.well-known/ai-catalog.json
curl -sS -o /dev/null -w 'ardvark.db      %{http_code} (regression guard, expect 404)\n' https://ardvark.no/ardvark.db
curl -sS https://ardvark.no/robots.txt
curl -sS https://ardvark.no/ | grep -o '<link rel="ar[^>]*>'
```

Expect: both well-known paths `200 application/json`, both `Agentmap:` lines
present, `rel="ard"` in the page head. The `ardvark.db` 404 is a regression
guard on the clean-export deploy, not the thing keeping it private — if it
ever returns 200, someone deployed the working directory by hand.

## Phase 10 — Dogfood

Prove the shipped binary discovers and verifies ardvark's own publication:

```sh
./ardvark probe ardvark.no --json     # expect well_known hit on ard.json, first try
./ardvark verify https://ardvark.no/.well-known/ard.json   # expect: verdict: valid
```

## Report

Summarize: version and why, commits, all four systems' verified state, and
anything deliberately left undone (from Known limitations). Call out explicitly
if any verification was skipped rather than passing.

## If something goes wrong

- **Release run failed:** delete the tag locally and remotely
  (`git tag -d vX.Y.Z && git push --delete origin vX.Y.Z`), delete the draft
  release if one exists, fix, re-tag. Do not reuse a tag that already published
  assets — cut the next patch instead.
- **Bad website deploy:** `npx vercel rollback` from `website/`, or redeploy the
  previous commit. The site is static, so rollback is safe and immediate.
- **Tap wrong or stale:** it is a separate repo; fix the formula there directly
  rather than re-cutting an ardvark release.
