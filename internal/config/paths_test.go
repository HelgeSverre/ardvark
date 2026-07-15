package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// An explicit path (the --config flag) must always win, even when standard
// locations exist.
func TestResolvePathExplicitWins(t *testing.T) {
	if got := ResolvePath("/some/explicit.json"); got != "/some/explicit.json" {
		t.Errorf("ResolvePath(explicit) = %q, want /some/explicit.json", got)
	}
}

// Without an explicit path, a working-directory ardvark.json must win over
// the user config dir, which in turn must win over the bare default.
func TestResolvePathSearchOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("search-order test drives XDG_CONFIG_HOME, which Windows does not use")
	}
	cwd := t.TempDir()
	xdg := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	// Nothing exists anywhere: fall back to the working-directory default.
	if got := ResolvePath(""); got != "ardvark.json" {
		t.Errorf("ResolvePath with no files = %q, want ardvark.json", got)
	}

	// Only the user config dir has a file: it must be picked.
	userCfg := filepath.Join(xdg, "ardvark", "ardvark.json")
	if err := os.MkdirAll(filepath.Dir(userCfg), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(userCfg, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("writing user config: %v", err)
	}
	if got := ResolvePath(""); got != userCfg {
		t.Errorf("ResolvePath with user config = %q, want %q", got, userCfg)
	}

	// A working-directory ardvark.json must shadow the user config dir.
	if err := os.WriteFile(filepath.Join(cwd, "ardvark.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("writing cwd config: %v", err)
	}
	if got := ResolvePath(""); got != "ardvark.json" {
		t.Errorf("ResolvePath with cwd config = %q, want ardvark.json", got)
	}
}

// SearchPaths must list the working directory first, then the per-user
// config directory.
func TestSearchPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path expectations below are for Unix XDG layout")
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")

	got := SearchPaths()
	want := []string{"ardvark.json", "/xdg/ardvark/ardvark.json"}
	if len(got) != len(want) {
		t.Fatalf("SearchPaths() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SearchPaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
