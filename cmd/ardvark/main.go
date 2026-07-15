// Command ardvark is the ardvark CLI: crawl the web for ARD ai-catalog.json
// documents, verify them against the spec, and index every discovered
// agentic resource. See the design doc at
// docs/superpowers/specs/2026-07-15-ardvark-crawler-design.md for the
// architecture this CLI drives.
package main

import "os"

func main() {
	if err := Execute(); err != nil {
		os.Exit(1)
	}
}
