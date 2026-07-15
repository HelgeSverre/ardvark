// Package seed bootstraps the crawl frontier from external domain sources
// when there is no seed list to start from. Every source implements the
// Seeder interface and shares the same tail: sanitize domains (strip a
// leading "*.", lowercase, drop IPs and invalid hostnames, dedupe), leaving
// the caller (cmd/ardvark's seed subcommands) to upsert domains rows with
// the appropriate discovery_source and enqueue host_probe frontier items.
// See docs/superpowers/specs/2026-07-15-ardvark-crawler-design.md's
// "Seeding" section for the design this package implements.
package seed

import (
	"context"
	"net"
	"strings"
)

// Seeder is implemented by every pluggable seed source (CT logs, crt.sh,
// Tranco, …).
type Seeder interface {
	// Domains streams sanitized hostnames until n collected or the source
	// is exhausted; ctx cancellation stops it.
	Domains(ctx context.Context, n int) ([]string, error)
	// Source is the discovery_source tag recorded on the domains row for
	// every hostname this seeder yields, e.g. "ct_log", "crtsh", "tranco".
	Source() string
}

// Sanitize normalizes a list of raw hostname-like values (SAN/CN entries,
// list-file rows, …) into a deduped list of plausible domain names:
//   - strips a leading "*." wildcard label (the apex is probed instead)
//   - lowercases
//   - drops IP addresses
//   - drops values with no dot or containing characters invalid in a
//     hostname
//   - dedupes while preserving first-seen order
func Sanitize(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))

	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		name = strings.TrimPrefix(name, "*.")
		name = strings.ToLower(name)
		name = strings.TrimSuffix(name, ".")

		if !strings.Contains(name, ".") {
			continue
		}
		if net.ParseIP(name) != nil {
			continue
		}
		if !isValidHostname(name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	return out
}

// isValidHostname reports whether s consists only of characters legal in a
// DNS hostname: letters, digits, hyphens and dots, with labels that don't
// start or end with a hyphen.
func isValidHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r == '-':
			default:
				return false
			}
		}
	}
	return true
}
