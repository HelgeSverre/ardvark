# ardvark — ARD catalog crawler & indexer

BINARY := "ardvark"
CMD := "./cmd/ardvark"

# Show available recipes
default:
    @just --list --unsorted

# === Build ===

# Build the binary into ./bin
[group('build')]
build:
    go build -o bin/{{BINARY}} {{CMD}}

# Run from source, passing args through
[group('build')]
run *ARGS:
    go run {{CMD}} {{ARGS}}

# Install into GOPATH/bin
[group('build')]
install:
    go install {{CMD}}

# Remove the installed binary
[group('build')]
uninstall:
    rm -f "$(go env GOPATH)/bin/{{BINARY}}"

# Remove build artifacts and local crawl output
[group('build')]
clean:
    rm -rf bin dist *.db *.jsonl

# === QA ===

# Run all tests
[group('qa')]
test:
    go test ./...

# Format all Go files
[group('qa')]
fmt:
    gofmt -w .

# Vet + verify formatting is clean
[group('qa')]
lint:
    go vet ./...
    @test -z "$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

# Full gate: lint + test
[group('qa')]
check: lint test

# === Spec ===

# Re-fetch the vendored ARD spec files from upstream main, diff against HEAD (no auto-commit)
[group('spec')]
sync-schema:
    #!/usr/bin/env bash
    set -euo pipefail
    base="https://raw.githubusercontent.com/ards-project/ard-spec/main/spec/schemas"
    dir="internal/ard/schema"
    for f in ai-catalog.schema.json ard.cddl ard.openapi.yaml; do
        curl -sf "$base/$f" -o "$dir/$f.new"
    done
    for f in ai-catalog.schema.json ard.cddl ard.openapi.yaml; do
        echo "--- $f ---"
        diff -u "$dir/$f" "$dir/$f.new" || true
        mv "$dir/$f.new" "$dir/$f"
    done
    echo
    echo "Re-vendored from upstream main. If anything changed above, update"
    echo "the pinned commit hash in $dir/PROVENANCE.md."

# === Release ===

# Validate the goreleaser config
[group('release')]
release-check:
    goreleaser check

# Build a local snapshot release (no publish)
[group('release')]
snapshot:
    goreleaser release --snapshot --clean

# Tag and push a release: just release v0.1.0
[group('release')]
release version:
    git tag -a {{version}} -m "Release {{version}}"
    git push origin {{version}}
