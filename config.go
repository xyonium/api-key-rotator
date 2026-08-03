package main

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIKeys       []string
	Upstream      string
	UpstreamHost  string
	Port          string
	Host          string
	MaxPasses     int
	MaxBodyBytes  int64
	ProxyBaseURL  string
	UpstreamProxy string
	LogLevel      string
	CreditResetDay      int   // fallback reset day-of-month when /v2/team/credit-usage is unreachable; 1-31
	LowCreditThreshold  int64 // switch off a key when its remainingCredits drops to/below this (default 10)
	StopCreditThreshold int64 // stop accepting requests when every key is below this (default 2)
	CreditRefreshSec    int   // seconds between background remainingCredits refreshes (default 300)
	Tavily              TavilyConfig
	Apify               ApifyConfig
}

// TavilyConfig holds the Tavily profile's settings. The profile is disabled
// when APIKeys is empty.
type TavilyConfig struct {
	APIKeys     []string
	Upstream    string
	RoutePrefix string
	LowCredit   int64
	StopCredit  int64
}

// ApifyConfig holds the Apify profile's settings. The profile is disabled
// when APIKeys is empty. Credit tracking uses the /v2/users/me/limits endpoint:
// remaining = (FreeCreditUsd - current.monthlyUsageUsd), tracked in CENTS so
// sub-dollar balances (the auto-stop thresholds) are exact integers.
type ApifyConfig struct {
	APIKeys          []string
	Upstream         string
	RoutePrefix      string
	TimeoutSec       int     // upstream client timeout; sync actor runs take 30-120s
	FreeCreditUsd    float64 // plan's included monthly credit (free plan: 5)
	LowCreditCents   int64   // switch off a token below this remaining (default 10 = $0.10)
	StopCreditCents  int64   // stop serving when every token is below this (default 5 = $0.05)
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %w", key, err)
	}
	return n, nil
}

func parseKeys(raw string) []string {
	var out []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func LoadConfig() (Config, error) {
	keys := parseKeys(os.Getenv("FIRECRAWL_API_KEYS"))
	if len(keys) == 0 {
		return Config{}, fmt.Errorf("FIRECRAWL_API_KEYS is required and must contain at least one non-empty key")
	}

	upstream := envStr("UPSTREAM", "https://api.firecrawl.dev")
	u, err := url.Parse(upstream)
	if err != nil {
		return Config{}, fmt.Errorf("UPSTREAM %q is not a valid http(s) URL: %w", upstream, err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Config{}, fmt.Errorf("UPSTREAM %q is not a valid http(s) URL (scheme must be http/https, host required)", upstream)
	}

	maxPasses, err := envInt("MAX_PASSES", 2)
	if err != nil {
		return Config{}, err
	}
	if maxPasses < 1 {
		return Config{}, fmt.Errorf("MAX_PASSES must be >= 1, got %d", maxPasses)
	}

	maxBody, err := envInt64("MAX_BODY_BYTES", 16*1024*1024)
	if err != nil {
		return Config{}, err
	}
	if maxBody < 0 {
		return Config{}, fmt.Errorf("MAX_BODY_BYTES must be >= 0, got %d", maxBody)
	}

	resetDay, err := envInt("CREDIT_RESET_DAY", 1)
	if err != nil {
		return Config{}, err
	}
	if resetDay < 1 || resetDay > 31 {
		return Config{}, fmt.Errorf("CREDIT_RESET_DAY must be 1-31, got %d", resetDay)
	}

	lowCredit, err := envInt64("LOW_CREDIT_THRESHOLD", 10)
	if err != nil {
		return Config{}, err
	}
	if lowCredit < 0 {
		return Config{}, fmt.Errorf("LOW_CREDIT_THRESHOLD must be >= 0, got %d", lowCredit)
	}
	stopCredit, err := envInt64("STOP_CREDIT_THRESHOLD", 2)
	if err != nil {
		return Config{}, err
	}
	if stopCredit < 0 {
		return Config{}, fmt.Errorf("STOP_CREDIT_THRESHOLD must be >= 0, got %d", stopCredit)
	}
	if stopCredit > lowCredit {
		return Config{}, fmt.Errorf("STOP_CREDIT_THRESHOLD (%d) must be <= LOW_CREDIT_THRESHOLD (%d)", stopCredit, lowCredit)
	}
	refreshSec, err := envInt("CREDIT_REFRESH_INTERVAL", 300)
	if err != nil {
		return Config{}, err
	}
	if refreshSec < 10 {
		return Config{}, fmt.Errorf("CREDIT_REFRESH_INTERVAL must be >= 10s, got %d", refreshSec)
	}

	proxyStr := strings.TrimSpace(os.Getenv("UPSTREAM_PROXY"))
	if proxyStr != "" {
		pu, err := url.Parse(proxyStr)
		if err != nil {
			return Config{}, fmt.Errorf("UPSTREAM_PROXY %q is not a valid proxy URL: %w", proxyStr, err)
		}
		if pu.Host == "" {
			return Config{}, fmt.Errorf("UPSTREAM_PROXY %q is not a valid proxy URL (host required)", proxyStr)
		}
		switch pu.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("UPSTREAM_PROXY scheme %q not supported (use http/https/socks5/socks5h)", pu.Scheme)
		}
	}

	tavily, err := loadTavilyConfig(lowCredit, stopCredit)
	if err != nil {
		return Config{}, err
	}

	apify, err := loadApifyConfig()
	if err != nil {
		return Config{}, err
	}

	return Config{
		APIKeys:       keys,
		Tavily:        tavily,
		Apify:         apify,
		Upstream:      strings.TrimRight(upstream, "/"),
		UpstreamHost:  u.Host,
		Port:          envStr("PORT", "8788"),
		Host:          envStr("HOST", "0.0.0.0"),
		MaxPasses:     maxPasses,
		MaxBodyBytes:  maxBody,
		ProxyBaseURL:  strings.TrimSpace(os.Getenv("PROXY_BASE_URL")),
		UpstreamProxy: proxyStr,
		LogLevel:           envStr("LOG_LEVEL", "info"),
		CreditResetDay:     resetDay,
		LowCreditThreshold: lowCredit,
		StopCreditThreshold: stopCredit,
		CreditRefreshSec:   refreshSec,
	}, nil
}

// loadTavilyConfig parses the TAVILY_* env vars. Tavily thresholds default to
// the shared LOW/STOP values. The route prefix must start with '/', must not
// end with '/', and must not shadow the reserved /healthz or /status paths.
func loadTavilyConfig(sharedLow, sharedStop int64) (TavilyConfig, error) {
	t := TavilyConfig{
		APIKeys:     parseKeys(os.Getenv("TAVILY_API_KEYS")),
		RoutePrefix: envStr("TAVILY_ROUTE_PREFIX", "/tavily"),
		LowCredit:   sharedLow,
		StopCredit:  sharedStop,
	}

	upstream := envStr("TAVILY_UPSTREAM", "https://api.tavily.com")
	tu, err := url.Parse(upstream)
	if err != nil || tu.Host == "" || (tu.Scheme != "http" && tu.Scheme != "https") {
		return TavilyConfig{}, fmt.Errorf("TAVILY_UPSTREAM %q is not a valid http(s) URL", upstream)
	}
	t.Upstream = strings.TrimRight(upstream, "/")

	if !strings.HasPrefix(t.RoutePrefix, "/") || len(t.RoutePrefix) < 2 || strings.HasSuffix(t.RoutePrefix, "/") {
		return TavilyConfig{}, fmt.Errorf("TAVILY_ROUTE_PREFIX %q must start with '/' and be a non-root path without trailing slash", t.RoutePrefix)
	}
	if t.RoutePrefix == "/healthz" || t.RoutePrefix == "/status" {
		return TavilyConfig{}, fmt.Errorf("TAVILY_ROUTE_PREFIX %q shadows a reserved path", t.RoutePrefix)
	}

	if v := strings.TrimSpace(os.Getenv("TAVILY_LOW_CREDIT_THRESHOLD")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return TavilyConfig{}, fmt.Errorf("TAVILY_LOW_CREDIT_THRESHOLD must be a non-negative integer, got %q", v)
		}
		t.LowCredit = n
	}
	if v := strings.TrimSpace(os.Getenv("TAVILY_STOP_CREDIT_THRESHOLD")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return TavilyConfig{}, fmt.Errorf("TAVILY_STOP_CREDIT_THRESHOLD must be a non-negative integer, got %q", v)
		}
		t.StopCredit = n
	}
	if t.StopCredit > t.LowCredit {
		return TavilyConfig{}, fmt.Errorf("TAVILY_STOP_CREDIT_THRESHOLD (%d) must be <= TAVILY_LOW_CREDIT_THRESHOLD (%d)", t.StopCredit, t.LowCredit)
	}
	return t, nil
}

// loadApifyConfig parses the APIFY_* env vars. The route prefix must start
// with '/', must not end with '/', and must not shadow the reserved /healthz
// or /status paths. The default prefix /v2/acts shadows only Apify actor
// endpoints on the firecrawl default profile; other /v2/* paths (firecrawl's
// own API) are unaffected. APIFY_TIMEOUT_SEC must cover synchronous actor
// runs (30-120s), so it defaults to 180 and must be positive.
func loadApifyConfig() (ApifyConfig, error) {
	a := ApifyConfig{
		APIKeys:          parseKeys(os.Getenv("APIFY_API_KEYS")),
		RoutePrefix:      envStr("APIFY_ROUTE_PREFIX", "/v2/acts"),
		TimeoutSec:       180,
		FreeCreditUsd:    5,
		LowCreditCents:   10, // $0.10
		StopCreditCents:  5,  // $0.05
	}

	upstream := envStr("APIFY_UPSTREAM", "https://api.apify.com")
	au, err := url.Parse(upstream)
	if err != nil || au.Host == "" || (au.Scheme != "http" && au.Scheme != "https") {
		return ApifyConfig{}, fmt.Errorf("APIFY_UPSTREAM %q is not a valid http(s) URL", upstream)
	}
	a.Upstream = strings.TrimRight(upstream, "/")

	if !strings.HasPrefix(a.RoutePrefix, "/") || len(a.RoutePrefix) < 2 || strings.HasSuffix(a.RoutePrefix, "/") {
		return ApifyConfig{}, fmt.Errorf("APIFY_ROUTE_PREFIX %q must start with '/' and be a non-root path without trailing slash", a.RoutePrefix)
	}
	if a.RoutePrefix == "/healthz" || a.RoutePrefix == "/status" {
		return ApifyConfig{}, fmt.Errorf("APIFY_ROUTE_PREFIX %q shadows a reserved path", a.RoutePrefix)
	}

	if v := strings.TrimSpace(os.Getenv("APIFY_TIMEOUT_SEC")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return ApifyConfig{}, fmt.Errorf("APIFY_TIMEOUT_SEC must be a positive integer (seconds), got %q", v)
		}
		a.TimeoutSec = n
	}

	freeUsd, err := envUsd("APIFY_FREE_CREDIT_USD", a.FreeCreditUsd)
	if err != nil {
		return ApifyConfig{}, err
	}
	a.FreeCreditUsd = freeUsd

	low, err := envUsdCents("APIFY_LOW_CREDIT_USD", a.LowCreditCents)
	if err != nil {
		return ApifyConfig{}, err
	}
	stop, err := envUsdCents("APIFY_STOP_CREDIT_USD", a.StopCreditCents)
	if err != nil {
		return ApifyConfig{}, err
	}
	if stop > low {
		return ApifyConfig{}, fmt.Errorf("APIFY_STOP_CREDIT_USD (%d cents) must be <= APIFY_LOW_CREDIT_USD (%d cents)", stop, low)
	}
	a.LowCreditCents, a.StopCreditCents = low, stop
	return a, nil
}

// envUsd parses a non-negative USD amount from an env var, returning def when unset.
func envUsd(key string, def float64) (float64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number (USD), got %q", key, v)
	}
	return f, nil
}

// envUsdCents parses a USD amount from an env var into integer cents (rounded
// to the nearest cent), returning def (already in cents) when unset.
func envUsdCents(key string, defCents int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return defCents, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number (USD), got %q", key, v)
	}
	return int64(math.Round(f * 100)), nil
}

// fallbackReset computes the per-key fallback reset instant: the next occurrence
// of day-of-month resetDay, at 00:00 UTC. Used only when the live
// /v2/team/credit-usage endpoint cannot be reached for a key.
func fallbackReset(now time.Time, resetDay int) time.Time {
	y, m, _ := now.UTC().Date()
	t := time.Date(y, m, resetDay, 0, 0, 0, 0, time.UTC)
	if !t.After(now.UTC()) {
		// this month's reset day has passed -> next month
		t = t.AddDate(0, 1, 0)
	}
	return t
}
