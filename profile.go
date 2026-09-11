package main

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Profile is one upstream provider's runtime bundle: key pool, refresher, and
// the per-provider rotation policy. The default (firecrawl) profile has an
// empty RoutePrefix and catches every path no other profile claims.
type Profile struct {
	Name           string
	RoutePrefix    string
	Upstream       string
	UpstreamHost   string
	CreditResetDay int
	RewriteNext    bool // rewrite "next" pagination URLs (firecrawl only)
	// KeepPrefix forwards the matched RoutePrefix as part of the upstream path
	// instead of stripping it (apify: the real API lives under /v2/acts).
	KeepPrefix bool
	// AuthQueryParam authenticates by setting this query parameter to the key
	// instead of an Authorization header (apify: "token").
	AuthQueryParam string
	// AuthHeader names the header that carries the key. When set, the key is
	// sent in this header verbatim (exa: "x-api-key"). Empty = legacy
	// "Authorization: Bearer <key>", which every other profile uses, so adding
	// this field cannot regress them. Precedence in tryKey:
	// AuthHeader > AuthQueryParam > Bearer.
	AuthHeader string
	// UpstreamTimeout, when > 0, gives this profile its own HTTP client with
	// this timeout (apify sync actor runs take 30-120s, beyond the shared 30s).
	UpstreamTimeout time.Duration
	// ApifyFreeCreditUsd overrides the included monthly credit (apify only).
	// When 0, fetchApifyUsage uses the account's own limits.maxMonthlyUsageUsd.
	// remaining = includedCredit - current.monthlyUsageUsd, tracked in cents.
	ApifyFreeCreditUsd float64
	// Concurrency control (firecrawl only; the other profiles leave these zero
	// and behave as before). ConcurrencySaturation is "queue" or "reject";
	// ConcurrencyQueue is how long a queued request waits for a free slot;
	// Max403Retries caps same-key 403 retries before rotating (0 = legacy full
	// backoff, used by tests).
	ConcurrencySaturation string
	ConcurrencyQueue      time.Duration
	Max403Retries         int

	pool    *KeyPool
	refresh *Refresher
	client  *http.Client // per-profile override; nil = use the shared client
}

// buildProfiles constructs the runtime profiles from config. The firecrawl
// (default, unprefixed) profile always comes first. Tavily is appended only
// when TAVILY_API_KEYS is set. Each profile gets its own KeyPool with its own
// thresholds.
func buildProfiles(cfg Config) []*Profile {
	fcPool := NewKeyPool(cfg.APIKeys)
	fcPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	fcPool.SetMaxConcurrent(cfg.FirecrawlMaxConcurrentPerKey)
	profiles := []*Profile{{
		Name:           "firecrawl",
		Upstream:       cfg.Upstream,
		UpstreamHost:   cfg.UpstreamHost,
		CreditResetDay: cfg.CreditResetDay,
		RewriteNext:    true,
		ConcurrencySaturation: cfg.FirecrawlConcurrencySaturation,
		ConcurrencyQueue:      time.Duration(cfg.FirecrawlConcurrencyQueueMs) * time.Millisecond,
		Max403Retries:         cfg.Firecrawl403Retries,
		pool:           fcPool,
	}}

	if len(cfg.Tavily.APIKeys) > 0 {
		tvPool := NewKeyPool(cfg.Tavily.APIKeys)
		tvPool.SetThresholds(cfg.Tavily.LowCredit, cfg.Tavily.StopCredit)
		host := cfg.Tavily.Upstream
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		profiles = append(profiles, &Profile{
			Name:           "tavily",
			RoutePrefix:    cfg.Tavily.RoutePrefix,
			Upstream:       cfg.Tavily.Upstream,
			UpstreamHost:   host,
			CreditResetDay: cfg.CreditResetDay,
			RewriteNext:    false,
			// Tavily's 403 is an AWS-edge block of the egress IP (bare HTML from
			// awselb/2.0), not a key rejection. Without this budget the full
			// 6-request legacy backoff fires per key, so one client request
			// becomes ~48 upstream hits and deepens the block.
			Max403Retries: cfg.Tavily.Retries403,
			pool:          tvPool,
		})
	}

	if len(cfg.Exa.APIKeys) > 0 {
		exPool := NewKeyPool(cfg.Exa.APIKeys)
		// Exa balances are ACCOUNTED in microUSD (Exa exposes no balance
		// endpoint), so thresholds are microUSD too.
		exPool.SetThresholds(cfg.Exa.LowMicro, cfg.Exa.StopMicro)
		host := cfg.Exa.Upstream
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		// Seed every key at the full budget. This is what makes
		// EnsureMeasured a no-op from the first request, so the refresher
		// never issues a balance fetch (which would 404 - Exa has no such
		// endpoint - and would leave keys "unmeasured" forever, retried every
		// minute by warmupLoop).
		for i := range cfg.Exa.APIKeys {
			exPool.SetCredits(i, cfg.Exa.BudgetMicro)
		}
		profiles = append(profiles, &Profile{
			Name:           "exa",
			RoutePrefix:    cfg.Exa.RoutePrefix,
			Upstream:       cfg.Exa.Upstream,
			UpstreamHost:   host,
			CreditResetDay: cfg.CreditResetDay,
			RewriteNext:    false, // Exa search returns no "next" and embeds no upstream URLs
			AuthHeader:     "x-api-key",
			pool:           exPool,
		})
	}

	if len(cfg.Apify.APIKeys) > 0 {
		apPool := NewKeyPool(cfg.Apify.APIKeys)
		// Apify credits are tracked in CENTS so sub-dollar stop thresholds are exact.
		apPool.SetThresholds(cfg.Apify.LowCreditCents, cfg.Apify.StopCreditCents)
		host := cfg.Apify.Upstream
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		profiles = append(profiles, &Profile{
			Name:               "apify",
			RoutePrefix:        cfg.Apify.RoutePrefix,
			Upstream:           cfg.Apify.Upstream,
			UpstreamHost:       host,
			CreditResetDay:     cfg.CreditResetDay,
			RewriteNext:        false,
			KeepPrefix:         true,
			AuthQueryParam:     "token",
			UpstreamTimeout:    time.Duration(cfg.Apify.TimeoutSec) * time.Second,
			ApifyFreeCreditUsd: cfg.Apify.FreeCreditUsd,
			pool:               apPool,
		})
	}

	if len(cfg.FMP.APIKeys) > 0 {
		fPool := NewKeyPool(cfg.FMP.APIKeys)
		fPool.SetThresholds(cfg.FMP.LowCredit, cfg.FMP.StopCredit)
		host := cfg.FMP.Upstream
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		// Seed each key at the daily budget so selection is gated from the
		// first request (no balance endpoint). Decrement by 1 per success.
		for i := range cfg.FMP.APIKeys {
			fPool.SetCredits(i, cfg.FMP.DailyBudget)
		}
		profiles = append(profiles, &Profile{
			Name:           "fmp",
			RoutePrefix:    cfg.FMP.RoutePrefix,
			Upstream:       cfg.FMP.Upstream,
			UpstreamHost:   host,
			CreditResetDay: cfg.CreditResetDay,
			AuthQueryParam: "apikey",
			pool:           fPool,
		})
	}

	if len(cfg.AlphaVantage.APIKeys) > 0 {
		aPool := NewKeyPool(cfg.AlphaVantage.APIKeys)
		aPool.SetThresholds(cfg.AlphaVantage.LowCredit, cfg.AlphaVantage.StopCredit)
		host := cfg.AlphaVantage.Upstream
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		for i := range cfg.AlphaVantage.APIKeys {
			aPool.SetCredits(i, cfg.AlphaVantage.DailyBudget)
		}
		profiles = append(profiles, &Profile{
			Name:           "alphavantage",
			RoutePrefix:    cfg.AlphaVantage.RoutePrefix,
			Upstream:       cfg.AlphaVantage.Upstream,
			UpstreamHost:   host,
			CreditResetDay: cfg.CreditResetDay,
			AuthQueryParam: "apikey",
			pool:           aPool,
		})
	}

	if len(cfg.Marketaux.APIKeys) > 0 {
		mPool := NewKeyPool(cfg.Marketaux.APIKeys)
		mPool.SetThresholds(cfg.Marketaux.LowCredit, cfg.Marketaux.StopCredit)
		host := cfg.Marketaux.Upstream
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		for i := range cfg.Marketaux.APIKeys {
			mPool.SetCredits(i, cfg.Marketaux.DailyBudget)
		}
		profiles = append(profiles, &Profile{
			Name:           "marketaux",
			RoutePrefix:    cfg.Marketaux.RoutePrefix,
			Upstream:       cfg.Marketaux.Upstream,
			UpstreamHost:   host,
			CreditResetDay: cfg.CreditResetDay,
			AuthQueryParam: "api_token",
			pool:           mPool,
		})
	}
	return profiles
}
//
// firecrawl: 402/429/401 always rotate; otherwise a failure envelope whose
// error text matches the denylist rotates. A success:true response NEVER
// rotates (scraped content legitimately contains denylist words).
//
// tavily: 401/429/432/433 rotate purely on status; the body is never
// consulted (Tavily error codes are unambiguous).
//
// apify: 401/402/429 rotate purely on status; the body is never consulted
// (success is a bare dataset-items ARRAY, which has no envelope to scan).
//
// 403 is never here for any profile - it is transient (edge/WAF) and retried
// with backoff on the SAME key via shouldRetry.
func (p *Profile) shouldRotate(status int, body []byte) (bool, string) {
	if p.Name == "tavily" {
		switch status {
		case 401, 429, 432, 433:
			return true, "status " + strconv.Itoa(status)
		}
		return false, ""
	}
	if p.Name == "apify" {
		switch status {
		case 401, 402, 429:
			return true, "status " + strconv.Itoa(status)
		}
		return false, ""
	}
	if p.Name == "exa" {
		// Status-only, like tavily/apify. Exa codes are unambiguous: 401 bad
		// key, 402 credits exhausted, 429 rate limit. The body is never
		// scanned - search results contain arbitrary scraped page text.
		switch status {
		case 401, 402, 429:
			return true, "status " + strconv.Itoa(status)
		}
		return false, ""
	}
	if p.Name == "fmp" {
		// FMP free tier: 429 "Limit Reach" is a daily-cap exhaust, not a
		// transient rate-limit. 401 rotates (bad/expired key). Other 4xx/5xx
		// fall through to transient handling.
		switch status {
		case 401, 429:
			return true, "status " + strconv.Itoa(status)
		}
		return false, ""
	}
	if p.Name == "alphavantage" {
		// Alpha Vantage reports daily-cap exhaustion as HTTP 200 + JSON body
		// with a "Note" field (not a 4xx). This is the ONE profile allowed to
		// scan a 200 body: a genuine data response never contains the key.
		// We rotate on any 200 body that contains the rate-limit phrase; the
		// downstream caller (isCreditExhausted) decides whether to disable.
		if status == 200 && alphaVantageIsExhausted(body) {
			return true, "body:rate-limit Note"
		}
		switch status {
		case 401:
			return true, "status " + strconv.Itoa(status)
		}
		return false, ""
	}
	if p.Name == "marketaux" {
		switch status {
		case 401, 402, 429:
			return true, "status " + strconv.Itoa(status)
		}
		return false, ""
	}
	// firecrawl (default)
	switch status {
	case 402, 429, 401:
		return true, "status " + strconv.Itoa(status)
	}
	if !firecrawlFailure(status, body) {
		return false, ""
	}
	if m := rejectDenylist.Find(body); m != nil {
		return true, "body:" + string(m)
	}
	return false, ""
}

// isDailyCappedFinance reports whether this profile's free tier resets daily
// (so a low predicted balance should block the LAST request instead of letting
// it fire a real upstream 429/402 to learn the truth). Firecrawl/tavily/exa
// don't need this guard - they recover quickly and threshold logic already
// handles them.
func (p *Profile) isDailyCappedFinance() bool {
	return p.Name == "fmp" || p.Name == "alphavantage" || p.Name == "marketaux"
}

// alphaVantageIsExhausted reports whether a 200 response from Alpha Vantage is
// the daily-cap "Thank you for using Alpha Vantage! ..." note (or its sibling
// "Information" message). Genuine data payloads never contain these keys, so
// scanning a success body is safe for THIS profile ONLY.
func alphaVantageIsExhausted(body []byte) bool {
	// Cheap substring match on the two documented rate-limit envelopes.
	// Examples: {"Note":"Thank you for using Alpha Vantage! Our standard API rate limit is ..."}
	//           {"Information":"Thank you for using Alpha Vantage! ..."}
	return bytes.Contains(body, []byte(`"Note"`)) ||
		bytes.Contains(body, []byte(`"Information"`))
}

// isCreditExhausted reports whether a rejection means the key's credits are
// genuinely gone until reset (disables the key). Rate-limit/auth never
// disable.
//
// firecrawl: 402, or a failure envelope mentioning credits/payment.
// tavily: 432 (plan limit) / 433 (pay-as-you-go limit), status only.
// apify: 402 only, status only (its 402 means the account is out of credits).
func (p *Profile) isCreditExhausted(status int, body []byte) bool {
	if p.Name == "tavily" {
		return status == 432 || status == 433
	}
	if p.Name == "apify" {
		return status == 402
	}
	// Exa: 402 means the account's credits are gone (NO_MORE_CREDITS /
	// API_KEY_BUDGET_EXCEEDED / TEAM_BUDGET_EXCEEDED). Exa uses the x402
	// payment protocol, so this is a real 402, not a 200-with-error-body.
	if p.Name == "exa" {
		return status == 402
	}
	// FMP: the free-tier 429 "Limit Reach" is a daily cap, not a transient
	// rate-limit (a rate-limit would come back in seconds; this only resets
	// at midnight UTC). Disabling until reset is exactly what the key needs.
	if p.Name == "fmp" {
		return status == 429
	}
	// Alpha Vantage: 200 + Note/Information body. This is the ONLY profile
	// allowed to credit-disable on a 200 - and only when shouldRotate has
	// already confirmed the exact rate-limit phrase.
	if p.Name == "alphavantage" {
		return status == 200 && alphaVantageIsExhausted(body)
	}
	// Marketaux: 402 is its daily-cap "usage_limit_reached" (not a payment
	// failure like firecrawl). 429 is a true rate-limit.
	if p.Name == "marketaux" {
		return status == 402
	}
	if status == 402 {
		return true
	}
	if status == 429 {
		return false // rate-limit never disables a key, regardless of body text
	}
	if firecrawlFailure(status, body) {
		return creditExhaustedPattern.Find(body) != nil
	}
	return false
}

// matchProfile resolves a request path to a profile. A prefixed profile
// matches when path == prefix or path starts with prefix+"/" (segment
// boundary, so "/tavilyfoo" does not match "/tavily"). The matched prefix is
// stripped unless the profile sets KeepPrefix. The no-prefix profile is the
// fallback for everything else, including prefixes that were never configured.
func matchProfile(profiles []*Profile, path string) (*Profile, string, bool) {
	var def *Profile
	for _, p := range profiles {
		if p.RoutePrefix == "" {
			def = p
			continue
		}
		if path == p.RoutePrefix {
			if p.KeepPrefix {
				return p, path, true
			}
			return p, "/", true
		}
		if strings.HasPrefix(path, p.RoutePrefix+"/") {
			if p.KeepPrefix {
				return p, path, true
			}
			return p, path[len(p.RoutePrefix):], true
		}
	}
	if def != nil {
		return def, path, true
	}
	return nil, path, false
}
