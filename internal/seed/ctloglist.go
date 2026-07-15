package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		for _, log := range op.Logs {
			if !log.usable() || !log.coversNow(now) {
				continue
			}
			if all || operatorMatches(operators, op.Name, log.Description) {
				urls = append(urls, log.URL)
			}
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("seed: ct: no usable current logs matched %v in %s", operators, logListURL)
	}
	return urls, nil
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
