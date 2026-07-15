// Package fetch implements a polite HTTP client for ardvark: per-host
// token-bucket rate limiting, request timeouts, a maximum body size cap, a
// redirect cap, http/https-only scheme enforcement, a custom User-Agent, and
// a robots.txt gate (including exposure of the raw robots.txt body for
// Agentmap directive scanning elsewhere in the codebase).
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/temoto/robotstxt"
	"golang.org/x/time/rate"
)

// maxRedirects is the maximum number of redirects the client will follow
// before giving up, per the design spec's politeness rules.
const maxRedirects = 5

// ErrBodyTooLarge is returned when a response body exceeds the configured
// MaxBodyBytes cap.
var ErrBodyTooLarge = errors.New("fetch: response body exceeds max size")

// ErrDisallowedScheme is returned when a URL uses a scheme other than http
// or https.
var ErrDisallowedScheme = errors.New("fetch: only http and https schemes are supported")

// ErrRobotsDisallowed is returned by Get when robots.txt disallows fetching
// the requested URL.
var ErrRobotsDisallowed = errors.New("fetch: disallowed by robots.txt")

// Fetched is the result of a successful fetch.
type Fetched struct {
	// URL is the final URL after following redirects.
	URL string
	// Status is the final HTTP status code.
	Status int
	// ContentType is the value of the Content-Type response header.
	ContentType string
	// Body is the (size-capped) response body.
	Body []byte
	// SHA256 is the lowercase hex-encoded SHA-256 digest of Body.
	SHA256 string
}

// Error is a typed fetch error that distinguishes transient failures
// (timeouts, 5xx, 429 — worth retrying) from permanent ones (other 4xx, DNS
// errors, disallowed schemes — not worth retrying).
type Error struct {
	Op        string
	URL       string
	Status    int // 0 if no HTTP response was received
	Err       error
	transient bool
}

func (e *Error) Error() string {
	if e.URL != "" {
		return fmt.Sprintf("fetch: %s %s: %v", e.Op, e.URL, e.Err)
	}
	return fmt.Sprintf("fetch: %s: %v", e.Op, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Transient reports whether err represents a transient failure (safe to
// retry with backoff) as opposed to a permanent one.
func Transient(err error) bool {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.transient
	}
	// Fall back to inspecting well-known transient network conditions for
	// errors not wrapped by this package.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// Client is a polite, rate-limited, robots.txt-respecting HTTP client.
type Client struct {
	cfg        config.CrawlerConfig
	httpClient *http.Client

	limitersMu sync.Mutex
	limiters   map[string]*rate.Limiter

	robotsMu    sync.Mutex
	robotsCache map[string]*robotstxt.RobotsData
	rawRobots   map[string]string

	// baseTransport is the http.RoundTripper the rate-limited transport
	// wraps; overridable via WithTransport for tests.
	baseTransport http.RoundTripper
}

// Option configures optional Client behavior. The zero value of Client (via
// New with no options) is the production-ready default; options exist
// primarily so tests can reach fixtures New's defaults cannot, such as an
// httptest.NewTLSServer standing in for a well-known probe's hardcoded
// https:// scheme.
type Option func(*Client)

// WithTransport overrides the base http.RoundTripper that the client's
// rate-limiting/politeness transport wraps (http.DefaultTransport otherwise).
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) {
		c.baseTransport = rt
	}
}

// New builds a Client configured from cfg.
func New(cfg config.CrawlerConfig, opts ...Option) *Client {
	c := &Client{
		cfg:           cfg,
		limiters:      make(map[string]*rate.Limiter),
		robotsCache:   make(map[string]*robotstxt.RobotsData),
		rawRobots:     make(map[string]string),
		baseTransport: http.DefaultTransport,
	}

	for _, opt := range opts {
		opt(c)
	}

	transport := &rateLimitedTransport{
		base:   c.baseTransport,
		client: c,
	}

	c.httpClient = &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.RequestTimeoutSeconds) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("fetch: stopped after %d redirects", maxRedirects)
			}
			if !isSupportedScheme(req.URL) {
				return ErrDisallowedScheme
			}
			return nil
		},
	}

	return c
}

func isSupportedScheme(u *url.URL) bool {
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}

// limiterForHost returns (creating if necessary) the token-bucket rate
// limiter for host.
func (c *Client) limiterForHost(host string) *rate.Limiter {
	c.limitersMu.Lock()
	defer c.limitersMu.Unlock()

	if l, ok := c.limiters[host]; ok {
		return l
	}

	rps := c.cfg.PerHostRequestsPerSecond
	if rps <= 0 {
		rps = 1
	}
	// Burst of 1: strictly one request per interval, the politest reading
	// of "N requests per second" for a crawler.
	l := rate.NewLimiter(rate.Limit(rps), 1)
	c.limiters[host] = l
	return l
}

// rateLimitedTransport wraps an http.RoundTripper, waiting on the
// per-host rate limiter before every request (including redirects and
// robots.txt fetches, so all outbound traffic is governed).
type rateLimitedTransport struct {
	base   http.RoundTripper
	client *Client
}

func (t *rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isSupportedScheme(req.URL) {
		return nil, ErrDisallowedScheme
	}

	limiter := t.client.limiterForHost(req.URL.Hostname())
	if err := limiter.Wait(req.Context()); err != nil {
		return nil, err
	}

	if req.Header.Get("User-Agent") == "" && t.client.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", t.client.cfg.UserAgent)
	}

	return t.base.RoundTrip(req)
}

// GetWellKnown performs a GET request against rawURL, bypassing the
// robots.txt gate: per the design spec, well-known discovery paths (e.g.
// /.well-known/ai-catalog.json) are always probed regardless of robots.txt,
// since that is the purpose of the well-known mechanism. Per-host rate
// limiting, the redirect cap, scheme enforcement, and the body size cap
// still apply — only the robots.txt check is skipped.
func (c *Client) GetWellKnown(ctx context.Context, rawURL string) (*Fetched, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &Error{Op: "get_well_known", URL: rawURL, Err: err, transient: false}
	}
	if !isSupportedScheme(u) {
		return nil, &Error{Op: "get_well_known", URL: rawURL, Err: ErrDisallowedScheme, transient: false}
	}
	return c.doGet(ctx, rawURL)
}

// Get performs a polite GET request against rawURL: it enforces the
// http/https scheme, checks robots.txt (if enabled and the URL is not the
// robots.txt fetch itself), applies per-host rate limiting, follows
// redirects up to the cap, and caps the response body size.
func (c *Client) Get(ctx context.Context, rawURL string) (*Fetched, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &Error{Op: "get", URL: rawURL, Err: err, transient: false}
	}
	if !isSupportedScheme(u) {
		return nil, &Error{Op: "get", URL: rawURL, Err: ErrDisallowedScheme, transient: false}
	}

	if c.cfg.RespectRobotsTxt {
		allowed, err := c.Allowed(ctx, rawURL)
		if err != nil {
			// A robots.txt fetch problem should not block well-known probes
			// or page fetches outright; treat as allowed but surface via
			// caller logs is out of scope here — the design says well-known
			// probes are always attempted regardless of robots outcome, and
			// for page fetches we fail open on robots-fetch errors so a
			// broken robots.txt doesn't wedge the crawl.
			allowed = true
		}
		if !allowed {
			return nil, &Error{Op: "get", URL: rawURL, Err: ErrRobotsDisallowed, transient: false}
		}
	}

	return c.doGet(ctx, rawURL)
}

// doGet performs the raw GET without the robots gate; used both for the
// public Get and for internal robots.txt fetches.
func (c *Client) doGet(ctx context.Context, rawURL string) (*Fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &Error{Op: "get", URL: rawURL, Err: err, transient: false}
	}
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, classifyNetError("get", rawURL, err)
	}
	defer resp.Body.Close()

	maxBody := c.cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 5 * 1024 * 1024
	}

	limited := io.LimitReader(resp.Body, maxBody+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, &Error{Op: "get", URL: rawURL, Status: resp.StatusCode, Err: err, transient: true}
	}
	if int64(len(body)) > maxBody {
		return nil, &Error{Op: "get", URL: rawURL, Status: resp.StatusCode, Err: ErrBodyTooLarge, transient: false}
	}

	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode >= 400 {
		return nil, &Error{
			Op:        "get",
			URL:       finalURL,
			Status:    resp.StatusCode,
			Err:       fmt.Errorf("unexpected status %d", resp.StatusCode),
			transient: isTransientStatus(resp.StatusCode),
		}
	}

	sum := sha256.Sum256(body)

	return &Fetched{
		URL:         finalURL,
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
		SHA256:      hex.EncodeToString(sum[:]),
	}, nil
}

// isTransientStatus reports whether an HTTP status code should be treated
// as a transient (retryable) failure: 5xx and 429. All other 4xx codes are
// permanent.
func isTransientStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

// classifyNetError wraps a network-level error (from http.Client.Do) into
// a typed Error, distinguishing timeouts (transient) from DNS/other
// permanent failures.
func classifyNetError(op, rawURL string, err error) *Error {
	transient := false

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		transient = true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		transient = false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		transient = true
	}

	return &Error{Op: op, URL: rawURL, Err: err, transient: transient}
}

// Allowed reports whether rawURL may be fetched per the target host's
// robots.txt. It fetches and caches robots.txt per host.
func (c *Client) Allowed(ctx context.Context, rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false, &Error{Op: "robots", URL: rawURL, Err: err, transient: false}
	}

	data, err := c.robotsFor(ctx, u.Scheme, u.Host)
	if err != nil {
		return false, err
	}
	if data == nil {
		// No robots.txt (404/empty): everything is allowed.
		return true, nil
	}

	ua := c.cfg.UserAgent
	if ua == "" {
		ua = "ardvark"
	}
	group := data.FindGroup(ua)
	return group.Test(u.Path), nil
}

// RawRobots returns the raw robots.txt body for host, fetching and caching
// it if necessary. It tries https first, falling back to http on
// connection-level failure (as opposed to an HTTP error status). It returns
// an empty string if the host has no robots.txt.
func (c *Client) RawRobots(ctx context.Context, host string) (string, error) {
	c.robotsMu.Lock()
	if raw, ok := c.rawRobots[host]; ok {
		c.robotsMu.Unlock()
		return raw, nil
	}
	c.robotsMu.Unlock()

	if _, err := c.robotsFor(ctx, "", host); err != nil {
		return "", err
	}

	c.robotsMu.Lock()
	defer c.robotsMu.Unlock()
	return c.rawRobots[host], nil
}

// robotsFor fetches and caches robots.txt for the given host, returning the
// parsed robots data (nil if the host has no robots.txt). If scheme is
// empty, https is tried first with a fallback to http on connection-level
// failure (not on HTTP error statuses, which are meaningful responses).
func (c *Client) robotsFor(ctx context.Context, scheme, host string) (*robotstxt.RobotsData, error) {
	c.robotsMu.Lock()
	if data, ok := c.robotsCache[host]; ok {
		c.robotsMu.Unlock()
		return data, nil
	}
	c.robotsMu.Unlock()

	schemes := []string{scheme}
	if scheme == "" {
		schemes = []string{"https", "http"}
	}

	var lastErr error
	for _, s := range schemes {
		robotsURL := fmt.Sprintf("%s://%s/robots.txt", s, host)

		fetched, err := c.doGet(ctx, robotsURL)
		if err != nil {
			var fe *Error
			if errors.As(err, &fe) && fe.Status >= 400 && fe.Status < 500 {
				// No robots.txt: everything allowed, empty raw body.
				c.robotsMu.Lock()
				c.robotsCache[host] = nil
				c.rawRobots[host] = ""
				c.robotsMu.Unlock()
				return nil, nil
			}
			lastErr = err
			continue
		}

		data, err := robotstxt.FromBytes(fetched.Body)
		if err != nil {
			return nil, &Error{Op: "robots", URL: robotsURL, Err: err, transient: false}
		}

		c.robotsMu.Lock()
		c.robotsCache[host] = data
		c.rawRobots[host] = string(fetched.Body)
		c.robotsMu.Unlock()

		return data, nil
	}

	return nil, lastErr
}
