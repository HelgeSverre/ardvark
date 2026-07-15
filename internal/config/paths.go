package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// SearchPaths returns the locations tried in order when no explicit config
// path is given: ardvark.json in the working directory, then the per-user
// config directory — $XDG_CONFIG_HOME/ardvark/ardvark.json (defaulting to
// ~/.config) on Unix and macOS, %AppData%\ardvark\ardvark.json on Windows.
func SearchPaths() []string {
	paths := []string{"ardvark.json"}
	if dir := userConfigDir(); dir != "" {
		paths = append(paths, filepath.Join(dir, "ardvark", "ardvark.json"))
	}
	return paths
}

// ResolvePath picks the config file to load. A non-empty explicit path (the
// --config flag) always wins. Otherwise the first existing SearchPaths()
// entry is used; if none exists, the working-directory default is returned
// (Load treats a missing file as pure defaults).
func ResolvePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	for _, p := range SearchPaths() {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "ardvark.json"
}

// userConfigDir returns the platform's per-user config root: os.UserConfigDir
// on Windows (%AppData%), XDG_CONFIG_HOME or ~/.config elsewhere (including
// macOS, where CLI tools conventionally use ~/.config rather than
// ~/Library/Application Support). Empty when the home directory is unknown.
func userConfigDir() string {
	if runtime.GOOS == "windows" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		return dir
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}
