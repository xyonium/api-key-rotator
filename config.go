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
	LowCreditThreshold  int64 // a measured key at/below this gets refreshed more often; selection is credit-maximizing so a below-low key only serves when it is the richest usable one (default 10)
	StopCreditThreshold int64 // hard floor: a key below this never serves; 503 when every key is below it (default 2)
	CreditRefreshSec    int   // seconds between background remainingCredits refreshes (default 300)
	// Firecrawl per-key concurrency control (firecrawl profile only). See
	// "Concurrent browser limits" in the Firecrawl docs: each free key is ~2
	// concurrent browsers, so a sub-agent burst on one key triggers 429s.
	FirecrawlMaxConcurrentPerKey  int    // max in-flight requests per firecrawl key (default 1, 0 = unlimited)
	FirecrawlConcurrencySaturation string // "queue" (wait for a slot) or "reject" (429) when all keys busy
	FirecrawlConcurrencyQueueMs   int    // how long a queued request waits for a free slot (default 15000)
	Firecrawl403Retries           int    // same-key retries on a 403 before rotating (default 1; 0 = legacy full backoff)
	// FirecrawlRoutePrefix is the explicit route for the firecrawl profile
	// (default "/firecrawl"). Callers pick firecrawl by hitting this prefix;
	// when empty, firecrawl becomes the catch-all (legacy behaviour).
	FirecrawlRoutePrefix string
	// FirecrawlLegacyFallback keeps the historic "every unrecognized path goes
	// to firecrawl" behaviour alive while FIRECRAWL_ROUTE_PREFIX is set. True
	// by default so existing callers that still point at the bare host:port are
	// not broken on upgrade. Set to false (FIRECRAWL_LEGACY_FALLBACK=false)
	// once every caller has moved to the /firecrawl prefix.
	FirecrawlLegacyFallback bool
	Tavily              TavilyConfig
	Apify               ApifyConfig
	Exa                            ExaConfig
	FMP                   FMPConfig
	AlphaVantage          AlphaVantageConfig
	Marketaux             MarketauxConfig
}

// TavilyConfig holds the Tavily profile's settings. The profile is disabled
// when APIKeys is empty.
type TavilyConfig struct {
	APIKeys     []string
	Upstream    string
	RoutePrefix string
	LowCredit   int64
	StopCredit  int64
	// Retries403 caps same-key retries on a 403 before rotating. Tavily's 403
	// comes from its AWS edge (server: awselb/2.0, bare HTML body) - it is an
	// IP-level block, not a key problem, so hammering it 6x per key across all
	// keys turns one client request into ~48 upstream hits and deepens the
	// block. Defaults to the firecrawl 403 budget.
	Retries403 int
}

// ExaConfig holds the Exa profile's settings. The profile is disabled when
// APIKeys is empty.
//
// Exa exposes NO balance endpoint (probed GET /billing /usage /account
// /credits /me /key on api.exa.ai -> all 404; the only usage API is
// admin-api.exa.ai, which needs a separate service key and returns *spend*, not
// remaining). So the balance is ACCOUNTED, not read: each key starts at
// BudgetMicro and is decremented by costDollars.total on every response.
//
// All money fields are in MICRO-USD (millionths of a dollar). Exa prices a
// search at $0.007, and int(0.007*100) == 0, so a cents-denominated balance
// would never decrement at all - the key would sit at a full budget forever and
// never auto-stop. Micro-USD keeps $0.007 exactly (7000) and still fits int64
// ($10 = 10_000_000).
type ExaConfig struct {
	APIKeys     []string
	Upstream    string
	RoutePrefix string
	BudgetMicro int64 // starting (and reset) balance in microUSD; default 10_000_000 ($10 free tier)
	LowMicro    int64 // rotate off a key below this; default 500_000 ($0.50)
	StopMicro   int64 // 503 when every key is below this; default 100_000 ($0.10)
}

// ApifyConfig holds the Apify profile's settings. The profile is disabled
// when APIKeys is empty. Credit tracking uses the /v2/users/me/limits endpoint:
// remaining = (includedMonthlyCredit - current.monthlyUsageUsd), tracked in
// CENTS so sub-dollar balances (the auto-stop thresholds) are exact integers.
// The included monthly credit is read from the account's own
// limits.maxMonthlyUsageUsd on every fetch; FreeCreditUsd, when > 0, overrides
// it (use when the account's reported cap isn't the real included credit).
type ApifyConfig struct {
	APIKeys          []string
	Upstream         string
	RoutePrefix      string
	TimeoutSec       int     // upstream client timeout; sync actor runs take 30-120s
	FreeCreditUsd    float64 // override for the included monthly credit; 0 = use the account's reported value
	LowCreditCents   int64   // a measured token at/below this gets refreshed more often (default 10 = $0.10)
	StopCreditCents  int64   // hard floor: a token below this never serves; 503 when all are below it (default 5 = $0.05)
}

// FMPConfig holds the FMP profile's settings. The profile is disabled when
// APIKeys is empty. FMP authenticates with the ?apikey= query param. The free
// tier is daily-capped (typ. 250 req/day) and signals exhaustion with a real
// 429 "Limit Reach" status - so a 429 is treated as credit-exhausted (disabled
// until the daily reset), not a transient rate-limit. No balance endpoint, so
// the balance is accounted: each key starts at DailyBudget requests and is
// decremented by 1 on success.
type FMPConfig struct {
	APIKeys     []string
	Upstream    string
	RoutePrefix string
	DailyBudget int64 // starting allowance per day; default 250 (free tier)
	LowCredit   int64 // rotate off a key below this many predicted requests left
	StopCredit  int64 // 503 when every key is below this
}

// AlphaVantageConfig holds the Alpha Vantage profile's settings. The profile
// is disabled when APIKeys is empty. Alpha Vantage authenticates with the
// ?apikey= query param. Its peculiar trait: rate-limit/daily-cap exhaustion is
// reported as HTTP 200 with a JSON body containing a "Note" (or "Information")
// message, NOT a 4xx - so it is the ONE profile allowed to scan a 200 body for
// an exhaustion signal. A genuine data response never contains those keys.
// Free tier is 25 requests/day, so DailyBudget defaults to 25.
type AlphaVantageConfig struct {
	APIKeys     []string
	Upstream    string
	RoutePrefix string
	DailyBudget int64 // default 25 (free tier daily request cap)
	LowCredit   int64
	StopCredit  int64
}

// MarketauxConfig holds the Marketaux profile's settings. The profile is
// disabled when APIKeys is empty. Marketaux authenticates with the ?api_token=
// query param. Errors are an error-code + HTTP status envelope: 401
// invalid_api_token rotates, 402 usage_limit_reached is credit-exhausted
// (disabled until the daily reset), 429 rate_limit rotates. Free tier is daily
// (typ. 100 req/day), so DailyBudget defaults to 100.
type MarketauxConfig struct {
	APIKeys     []string
	Upstream    string
	RoutePrefix string
	DailyBudget int64 // default 100 (free tier daily request cap)
	LowCredit   int64
	StopCredit  int64
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

	// Parsed before the profiles are built: firecrawl and tavily share this
	// budget (both hit edge-level 403s that are not key problems).
	retries403, err := envInt("FIRECRAWL_403_RETRIES", 1)
	if err != nil {
		return Config{}, err
	}
	if retries403 < 0 {
		return Config{}, fmt.Errorf("FIRECRAWL_403_RETRIES must be >= 0, got %d", retries403)
	}

	tavily, err := loadTavilyConfig(lowCredit, stopCredit, retries403)
	if err != nil {
		return Config{}, err
	}

	apify, err := loadApifyConfig()
	if err != nil {
		return Config{}, err
	}

	exa, err := loadExaConfig()
	if err != nil {
		return Config{}, err
	}

	fmp, err := loadFMPConfig()
	if err != nil {
		return Config{}, err
	}
	alpha, err := loadAlphaVantageConfig()
	if err != nil {
		return Config{}, err
	}
	marketaux, err := loadMarketauxConfig()
	if err != nil {
		return Config{}, err
	}

	maxConc, err := envInt("FIRECRAWL_MAX_CONCURRENT_PER_KEY", 1)
	if err != nil {
		return Config{}, err
	}
	if maxConc < 0 {
		return Config{}, fmt.Errorf("FIRECRAWL_MAX_CONCURRENT_PER_KEY must be >= 0, got %d", maxConc)
	}
	saturation := envStr("FIRECRAWL_CONCURRENCY_SATURATION", "queue")
	if saturation != "queue" && saturation != "reject" {
		return Config{}, fmt.Errorf("FIRECRAWL_CONCURRENCY_SATURATION %q must be \"queue\" or \"reject\"", saturation)
	}
	queueMs, err := envInt("FIRECRAWL_CONCURRENCY_QUEUE_MS", 15000)
	if err != nil {
		return Config{}, err
	}
	if queueMs < 0 {
		return Config{}, fmt.Errorf("FIRECRAWL_CONCURRENCY_QUEUE_MS must be >= 0, got %d", queueMs)
	}

	// Firecrawl routing. Default: explicit /firecrawl prefix + legacy fallback
	// so every historical caller keeps working while they migrate.
	fcPrefix := envStr("FIRECRAWL_ROUTE_PREFIX", "/firecrawl")
	if fcPrefix != "" {
		if err := checkRoutePrefix("FIRECRAWL", fcPrefix); err != nil {
			return Config{}, err
		}
	}
	fcLegacyFallback := os.Getenv("FIRECRAWL_LEGACY_FALLBACK") != "false"

	return Config{
		APIKeys:       keys,
		Tavily:        tavily,
		Apify:         apify,
		Exa:                            exa,
		FMP:                            fmp,
		AlphaVantage:                   alpha,
		Marketaux:                      marketaux,
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
		FirecrawlMaxConcurrentPerKey:   maxConc,
		FirecrawlConcurrencySaturation: saturation,
		FirecrawlConcurrencyQueueMs:    queueMs,
		Firecrawl403Retries:            retries403,
		FirecrawlRoutePrefix:           fcPrefix,
		FirecrawlLegacyFallback:        fcLegacyFallback,
	}, nil
}

// loadTavilyConfig parses the TAVILY_* env vars. Tavily thresholds default to
// the shared LOW/STOP values. The route prefix must start with '/', must not
// end with '/', and must not shadow the reserved /healthz or /status paths.
func loadTavilyConfig(sharedLow, sharedStop int64, retries403 int) (TavilyConfig, error) {
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
	t.Retries403 = retries403
	return t, nil
}

// loadApifyConfig parses the APIFY_* env vars. The route prefix must start
// with '/', must not end with '/', and must not shadow the reserved /healthz
// or /status paths. The default prefix /apify makes Apify one of the explicit
// prefixed profiles (like /firecrawl, /tavily, /exa); it is stripped before
// forwarding to api.apify.com so callers hit `/apify/v2/acts/...` and the
// upstream receives `/v2/acts/...`. APIFY_TIMEOUT_SEC must cover synchronous
// actor runs (30-120s), so it defaults to 180 and must be positive.
func loadApifyConfig() (ApifyConfig, error) {
	a := ApifyConfig{
		APIKeys:         parseKeys(os.Getenv("APIFY_API_KEYS")),
		RoutePrefix:     envStr("APIFY_ROUTE_PREFIX", "/apify"),
		TimeoutSec:      180,
		FreeCreditUsd:   0, // 0 = no override: read the included credit from the account each fetch
		LowCreditCents:  10, // $0.10
		StopCreditCents: 5,  // $0.05
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

// checkRoutePrefix validates a finance profile's strip-prefix. Same rules as
// tavily's: leading slash, non-root, no trailing slash, no /healthz|/status.
func checkRoutePrefix(name, prefix string) error {
	if !strings.HasPrefix(prefix, "/") || len(prefix) < 2 || strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("%s_ROUTE_PREFIX %q must start with '/' and be a non-root path without trailing slash", name, prefix)
	}
	if prefix == "/healthz" || prefix == "/status" {
		return fmt.Errorf("%s_ROUTE_PREFIX %q shadows a reserved path", name, prefix)
	}
	return nil
}

// parseOptInt64 parses a non-negative int64 env override. The returned bool is
// false when the env var is absent (caller keeps its default) or unparsable.
func parseOptInt64(name string) (int64, bool, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("%s must be a non-negative integer, got %q", name, v)
	}
	return n, true, nil
}

// parseDailyBudget parses a positive daily request allowance (e.g. FMP free
// tier 250/day, Alpha Vantage 25/day). Must be >= 1 so "budget 0" can't mean
// "disabled" - it would silently stack every key at zero remaining.
func parseDailyBudget(name string) (int64, bool, error) {
	n, set, err := parseOptInt64(name)
	if err != nil {
		return 0, false, err
	}
	if set && n < 1 {
		return 0, false, fmt.Errorf("%s must be a positive integer, got %d", name, n)
	}
	return n, set, nil
}

// financeDailyThresholds resolves the LOW/STOP pair for a daily-capped finance
// profile, applying the three budget env vars when set. Stop defaults to 1 so
// a key never serves its very last predicted request (protects the upstream
// from a real 429 that would deepen a ban).
func financeDailyThresholds(lowName, stopName, budgetName string, defLow, defStop int64) (budget, low, stop int64, err error) {
	budget, budgetSet, err := parseDailyBudget(budgetName)
	if err != nil {
		return 0, 0, 0, err
	}
	low, lowSet, err := parseOptInt64(lowName)
	if err != nil {
		return 0, 0, 0, err
	}
	stop, stopSet, err := parseOptInt64(stopName)
	if err != nil {
		return 0, 0, 0, err
	}
	if !budgetSet {
		budget = -1
	}
	if !lowSet {
		low = defLow
	}
	if !stopSet {
		stop = defStop
	}
	if budgetSet && stop > budget {
		return 0, 0, 0, fmt.Errorf("%s (%d) must be <= %s (%d)", stopName, stop, budgetName, budget)
	}
	if stop > low {
		return 0, 0, 0, fmt.Errorf("%s (%d) must be <= %s (%d)", stopName, stop, lowName, low)
	}
	return budget, low, stop, nil
}

// loadFMPConfig parses FMP_* env vars. The free tier is daily-capped (typ.
// 250 req/day) and reports exhaustion with a real 429 "Limit Reach"; see
// FMPConfig for why 429 is treated as credit-exhausted instead of a
// transient rate-limit.
func loadFMPConfig() (FMPConfig, error) {
	f := FMPConfig{
		APIKeys:     parseKeys(os.Getenv("FMP_API_KEYS")),
		RoutePrefix: envStr("FMP_ROUTE_PREFIX", "/fmp"),
		DailyBudget: 250,
		LowCredit:   10,
		StopCredit:  1,
	}
	upstream := envStr("FMP_UPSTREAM", "https://financialmodelingprep.com")
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return FMPConfig{}, fmt.Errorf("FMP_UPSTREAM %q is not a valid http(s) URL", upstream)
	}
	f.Upstream = strings.TrimRight(upstream, "/")
	if err := checkRoutePrefix("FMP", f.RoutePrefix); err != nil {
		return FMPConfig{}, err
	}
	budget, low, stop, err := financeDailyThresholds("FMP_LOW_CREDIT", "FMP_STOP_CREDIT", "FMP_DAILY_BUDGET", f.LowCredit, f.StopCredit)
	if err != nil {
		return FMPConfig{}, err
	}
	if budget > 0 {
		f.DailyBudget = budget
	}
	f.LowCredit, f.StopCredit = low, stop
	return f, nil
}

// loadAlphaVantageConfig parses ALPHAVANTAGE_* env vars. Alpha Vantage free
// tier is 25 requests/day and reports exhaustion in an HTTP 200 body
// containing "Note" - this profile is the one allowed to scan a 200 body.
func loadAlphaVantageConfig() (AlphaVantageConfig, error) {
	a := AlphaVantageConfig{
		APIKeys:     parseKeys(os.Getenv("ALPHAVANTAGE_API_KEYS")),
		RoutePrefix: envStr("ALPHAVANTAGE_ROUTE_PREFIX", "/alphavantage"),
		DailyBudget: 25,
		LowCredit:   5,
		StopCredit:  1,
	}
	upstream := envStr("ALPHAVANTAGE_UPSTREAM", "https://www.alphavantage.co")
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return AlphaVantageConfig{}, fmt.Errorf("ALPHAVANTAGE_UPSTREAM %q is not a valid http(s) URL", upstream)
	}
	a.Upstream = strings.TrimRight(upstream, "/")
	if err := checkRoutePrefix("ALPHAVANTAGE", a.RoutePrefix); err != nil {
		return AlphaVantageConfig{}, err
	}
	budget, low, stop, err := financeDailyThresholds("ALPHAVANTAGE_LOW_CREDIT", "ALPHAVANTAGE_STOP_CREDIT", "ALPHAVANTAGE_DAILY_BUDGET", a.LowCredit, a.StopCredit)
	if err != nil {
		return AlphaVantageConfig{}, err
	}
	if budget > 0 {
		a.DailyBudget = budget
	}
	a.LowCredit, a.StopCredit = low, stop
	return a, nil
}

// loadMarketauxConfig parses MARKETAUX_* env vars. Marketaux errors are an
// error-code + status envelope: 401 invalid_api_token, 402
// usage_limit_reached (credit-exhausted), 429 rate_limit.
func loadMarketauxConfig() (MarketauxConfig, error) {
	m := MarketauxConfig{
		APIKeys:     parseKeys(os.Getenv("MARKETAUX_API_KEYS")),
		RoutePrefix: envStr("MARKETAUX_ROUTE_PREFIX", "/marketaux"),
		DailyBudget: 100,
		LowCredit:   10,
		StopCredit:  1,
	}
	upstream := envStr("MARKETAUX_UPSTREAM", "https://api.marketaux.com")
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return MarketauxConfig{}, fmt.Errorf("MARKETAUX_UPSTREAM %q is not a valid http(s) URL", upstream)
	}
	m.Upstream = strings.TrimRight(upstream, "/")
	if err := checkRoutePrefix("MARKETAUX", m.RoutePrefix); err != nil {
		return MarketauxConfig{}, err
	}
	budget, low, stop, err := financeDailyThresholds("MARKETAUX_LOW_CREDIT", "MARKETAUX_STOP_CREDIT", "MARKETAUX_DAILY_BUDGET", m.LowCredit, m.StopCredit)
	if err != nil {
		return MarketauxConfig{}, err
	}
	if budget > 0 {
		m.DailyBudget = budget
	}
	m.LowCredit, m.StopCredit = low, stop
	return m, nil
}

// loadExaConfig parses the EXA_* env vars. The route prefix must start with
// '/', must not end with '/', and must not shadow the reserved /healthz or
// /status paths.
//
// Budget and thresholds are configured in USD (EXA_BUDGET_USD etc.) and stored
// in microUSD. The default budget is the free tier's $10/month: Exa exposes no
// endpoint reporting remaining credits, so this is the starting balance the
// rotator accounts spend against.
func loadExaConfig() (ExaConfig, error) {
	e := ExaConfig{
		APIKeys:     parseKeys(os.Getenv("EXA_API_KEYS")),
		RoutePrefix: envStr("EXA_ROUTE_PREFIX", "/exa"),
		BudgetMicro: 10_000_000, // $10 (free tier monthly credits)
		LowMicro:    500_000,    // $0.50
		StopMicro:   100_000,    // $0.10
	}

	upstream := envStr("EXA_UPSTREAM", "https://api.exa.ai")
	eu, err := url.Parse(upstream)
	if err != nil || eu.Host == "" || (eu.Scheme != "http" && eu.Scheme != "https") {
		return ExaConfig{}, fmt.Errorf("EXA_UPSTREAM %q is not a valid http(s) URL", upstream)
	}
	e.Upstream = strings.TrimRight(upstream, "/")

	if !strings.HasPrefix(e.RoutePrefix, "/") || len(e.RoutePrefix) < 2 || strings.HasSuffix(e.RoutePrefix, "/") {
		return ExaConfig{}, fmt.Errorf("EXA_ROUTE_PREFIX %q must start with '/' and be a non-root path without trailing slash", e.RoutePrefix)
	}
	if e.RoutePrefix == "/healthz" || e.RoutePrefix == "/status" {
		return ExaConfig{}, fmt.Errorf("EXA_ROUTE_PREFIX %q shadows a reserved path", e.RoutePrefix)
	}

	budget, err := envUsdMicro("EXA_BUDGET_USD", e.BudgetMicro)
	if err != nil {
		return ExaConfig{}, err
	}
	low, err := envUsdMicro("EXA_LOW_CREDIT_USD", e.LowMicro)
	if err != nil {
		return ExaConfig{}, err
	}
	stop, err := envUsdMicro("EXA_STOP_CREDIT_USD", e.StopMicro)
	if err != nil {
		return ExaConfig{}, err
	}
	if stop > low {
		return ExaConfig{}, fmt.Errorf("EXA_STOP_CREDIT_USD (%d microUSD) must be <= EXA_LOW_CREDIT_USD (%d microUSD)", stop, low)
	}
	if budget <= 0 {
		return ExaConfig{}, fmt.Errorf("EXA_BUDGET_USD must be greater than 0, got %d microUSD", budget)
	}
	e.BudgetMicro, e.LowMicro, e.StopMicro = budget, low, stop
	return e, nil
}

// envUsdMicro parses a USD amount from an env var into integer MICRO-USD
// (millionths of a dollar), returning def (already microUSD) when unset.
// Exa prices a search at $0.007, which is 0 cents but 7000 microUSD - the
// precision is the whole point (see ExaConfig).
func envUsdMicro(key string, defMicro int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return defMicro, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number (USD), got %q", key, v)
	}
	return int64(math.Round(f * 1_000_000)), nil
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
