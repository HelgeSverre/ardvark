package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultCTLogListURL is Google's v3 CT log list — the same list Chrome and
// Firefox ship. Resolving logs from it means ardvark never hardcodes a shard
// URL, which rotate roughly every six months as logs are sharded by date and
// old shards go read-only.
const DefaultCTLogListURL = "https://www.gstatic.com/ct/log_list/v3/log_list.json"

// ctLogList mirrors the subset of Google's log_list.json (v3) that we need.
type ctLogList struct {
	Operators []struct {
		Name string     `json:"name"`
		Logs []ctLogRef `json:"logs"`
	} `json:"operators"`
}

type ctLogRef struct {
	Description string `json:"description"`
	URL         string `json:"url"`
	// State is a single-key object: "usable", "qualified", "readonly",
	// "pending", or "retired". We only seed from usable logs.
	State            map[string]json.RawMessage `json:"state"`
	TemporalInterval *struct {
		StartInclusive time.Time `json:"start_inclusive"`
		EndExclusive   time.Time `json:"end_exclusive"`
	} `json:"temporal_interval"`
}

func (l ctLogRef) usable() bool {
	_, ok := l.State["usable"]
	return ok
}

// coversNow reports whether the log's temporal shard is the one currently
// being written to, i.e. now falls in [start, end). A log without a temporal
// interval is treated as always current.
func (l ctLogRef) coversNow(now time.Time) bool {
	if l.TemporalInterval == nil {
		return true
	}
	return !now.Before(l.TemporalInterval.StartInclusive) && now.Before(l.TemporalInterval.EndExclusive)
}

// ctFreshCertWindow bounds how far into the future a shard's start may be
// for it to still be considered "about to receive freshly issued certs" —
// roughly the CA/Browser Forum's maximum TLS certificate validity (398
// days), rounded up. See selectOperatorShards for why this matters.
const ctFreshCertWindow = 400 * 24 * time.Hour

// ResolveCTLogs fetches the CT log list and returns the base URLs of usable
// logs whose temporal shard covers now, restricted to the given operator
// tokens. A token matches case-insensitively against the operator name or the
// log description (so "oak", "argon", "nimbus" all work); the token "all"
// (or an empty filter) selects every eligible log. Results preserve log-list
// order.
func ResolveCTLogs(ctx context.Context, httpClient *http.Client, logListURL string, operators []string, now time.Time) ([]string, error) {
	if logListURL == "" {
		logListURL = DefaultCTLogListURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, logListURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("seed: ct: fetching log list %s: %w", logListURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("seed: ct: reading log list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seed: ct: log list %s returned status %d", logListURL, resp.StatusCode)
	}

	var list ctLogList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("seed: ct: decoding log list: %w", err)
	}

	all := wantAllOperators(operators)
	var urls []string
	for _, op := range list.Operators {
		var matched []ctLogRef
		for _, log := range op.Logs {
			if !log.usable() {
				continue
			}
			if all || operatorMatches(operators, op.Name, log.Description) {
				matched = append(matched, log)
			}
		}
		urls = append(urls, selectOperatorShards(matched, now)...)
	}
	if len(urls) == 0 {
		return nil, noMatchError(operators, list, now, logListURL)
	}
	return urls, nil
}

// selectOperatorShards picks the base URLs to read from among one
// operator's usable, already-token-matched logs. CT log shards are temporal
// and partition by certificate NotAfter (expiration), not issuance time, so
// the shard whose interval contains "now" (coversNow) is filling with certs
// that expire soon — a cert issued today with a full-length (~13 month)
// validity period can already land in the *next* shard, whose window hasn't
// started yet from a wall-clock perspective. To still catch those
// freshly-issued certs, also include the usable shard whose interval starts
// soonest after now, as long as that start is within ctFreshCertWindow (a
// shard starting a year and a half from now isn't about certs issued
// today). This stays conservative: at most one "current" and one "next"
// shard per operator, never the whole future log list.
func selectOperatorShards(logs []ctLogRef, now time.Time) []string {
	var urls []string
	var nearestFuture *ctLogRef
	var nearestStart time.Time

	for i := range logs {
		log := logs[i]
		if log.coversNow(now) {
			urls = append(urls, log.URL)
			continue
		}
		if log.TemporalInterval == nil {
			continue
		}
		start := log.TemporalInterval.StartInclusive
		if !start.After(now) || start.After(now.Add(ctFreshCertWindow)) {
			continue
		}
		if nearestFuture == nil || start.Before(nearestStart) {
			l := log
			nearestFuture = &l
			nearestStart = start
		}
	}

	if nearestFuture != nil {
		urls = append(urls, nearestFuture.URL)
	}
	return urls
}

// noMatchError builds a friendly, actionable error when no usable current
// log matched the requested operator tokens (e.g. a retired operator like
// "oak" being requested after its logs go read-only): it lists which
// operators the caller can pick from right now, drawn from the same fetched
// log list, instead of just failing silently.
func noMatchError(operators []string, list ctLogList, now time.Time, logListURL string) error {
	usableOps := make(map[string]struct{})
	for _, op := range list.Operators {
		for _, log := range op.Logs {
			if log.usable() && (log.coversNow(now) || log.TemporalInterval == nil) {
				usableOps[op.Name] = struct{}{}
				break
			}
		}
	}
	names := make([]string, 0, len(usableOps))
	for name := range usableOps {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		return fmt.Errorf("seed: ct: no usable current logs matched %v in %s, and no operator currently has a usable current log either", operators, logListURL)
	}
	return fmt.Errorf("seed: ct: no usable current logs matched %v in %s (retired or unrecognized operator?); operators with a usable log right now: %s", operators, logListURL, strings.Join(names, ", "))
}

func wantAllOperators(operators []string) bool {
	if len(operators) == 0 {
		return true
	}
	for _, o := range operators {
		if strings.EqualFold(strings.TrimSpace(o), "all") {
			return true
		}
	}
	return false
}

func operatorMatches(tokens []string, operatorName, logDescription string) bool {
	name := strings.ToLower(operatorName)
	desc := strings.ToLower(logDescription)
	for _, t := range tokens {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if strings.Contains(name, t) || strings.Contains(desc, t) {
			return true
		}
	}
	return false
}
