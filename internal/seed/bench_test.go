package seed

import (
	"fmt"
	"strings"
	"testing"
)

// benchAwesomeList builds a synthetic awesome-list at the real
// punkpeye/awesome-mcp-servers scale: ~3.3k glama.ai links, ~3k github.com
// links, and a few hundred product domains — the workload the blocklist and
// dedupe have to chew through per seed run.
func benchAwesomeList() string {
	var b strings.Builder
	b.WriteString("# Awesome MCP Servers\n\n")
	for i := 0; i < 3300; i++ {
		fmt.Fprintf(&b, "- [srv%d](https://github.com/acme/server-%d) ([glama](https://glama.ai/mcp/servers/%d))\n", i, i, i)
	}
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "- [prod%d](https://product-%d.example.com/mcp) ![badge](https://img.shields.io/badge/x-%d)\n", i, i, i)
	}
	return b.String()
}

// BenchmarkHostsFromText measures URL extraction + infra filtering over a
// realistic awesome-list document (the curated seeder's per-list hot path).
func BenchmarkHostsFromText(b *testing.B) {
	text := benchAwesomeList()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hosts := hostsFromText(text)
		if len(hosts) != 300 {
			b.Fatalf("got %d hosts, want 300", len(hosts))
		}
	}
}

// BenchmarkCuratedCollect measures the full parse-and-dedupe tail of a
// curated seed run: extraction, blocklist, sanitize, dedupe, count cap.
func BenchmarkCuratedCollect(b *testing.B) {
	text := benchAwesomeList()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		collector := newDomainCollector(500)
		collector.add(hostsFromText(text))
		if got := len(collector.domains()); got != 300 {
			b.Fatalf("got %d domains, want 300", got)
		}
	}
}

// benchRanksLines builds n synthetic domain-ranks rows (header included).
func benchRanksLines(n int) []string {
	lines := make([]string, 0, n+1)
	lines = append(lines, "#harmonicc_pos\tharmonicc_val\tpr_pos\tpr_val\thost_rev\tn_hosts")
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("%d\t0.5\t%d\t0.5\tcom.domain-%d\t1", i+1, i+1, i))
	}
	return lines
}

// BenchmarkCommonCrawlReverseAndCollect measures the per-row cost of the
// Common Crawl stream loop body (field split, host reversal, sanitize,
// dedupe) over 100k rows — the dominant CPU cost of a large --top run once
// bytes are on the wire.
func BenchmarkCommonCrawlReverseAndCollect(b *testing.B) {
	lines := benchRanksLines(100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		collector := newDomainCollector(100_000)
		for _, line := range lines {
			if strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}
			collector.add([]string{reverseHost(fields[4])})
		}
		if got := len(collector.domains()); got != 100_000 {
			b.Fatalf("got %d domains, want 100000", got)
		}
	}
}
