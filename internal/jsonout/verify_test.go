package jsonout

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helgesverre/ardvark/internal/ard"
)

// VerifyTarget must use ardvark's lenient default verification, accepting a
// legacy "urn:ai:" identifier, while VerifyTargetStrict must validate
// against the exact published spec schema and reject it.
func TestVerifyTarget_LenientVsStrict(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	catalog := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:ai:example.com:tool:x",
			"displayName": "X",
			"type": "application/mcp-server-card+json",
			"url": "https://example.com/x.json",
			"representativeQueries": ["one", "two"]
		}]
	}`
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o644); err != nil {
		t.Fatalf("writing catalog: %v", err)
	}

	lenient, err := VerifyTarget(context.Background(), catalogPath)
	if err != nil {
		t.Fatalf("VerifyTarget: %v", err)
	}
	if lenient.Verdict != ard.VerdictValid {
		t.Fatalf("VerifyTarget Verdict = %q, want %q; checks: %+v", lenient.Verdict, ard.VerdictValid, lenient.Checks)
	}

	strict, err := VerifyTargetStrict(context.Background(), catalogPath)
	if err != nil {
		t.Fatalf("VerifyTargetStrict: %v", err)
	}
	if strict.Verdict != ard.VerdictInvalid {
		t.Fatalf("VerifyTargetStrict Verdict = %q, want %q; checks: %+v", strict.Verdict, ard.VerdictInvalid, strict.Checks)
	}
}

// VerifyTarget and VerifyTargetStrict must both return an error, rather than
// panic, for a target that doesn't exist.
func TestVerifyTarget_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")

	if _, err := VerifyTarget(context.Background(), missing); err == nil {
		t.Fatal("VerifyTarget: want error for missing file, got nil")
	}
	if _, err := VerifyTargetStrict(context.Background(), missing); err == nil {
		t.Fatal("VerifyTargetStrict: want error for missing file, got nil")
	}
}
