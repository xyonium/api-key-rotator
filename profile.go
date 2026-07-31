package main

import (
	"strconv"
	"strings"
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

	pool    *KeyPool
	refresh *Refresher
}

// buildProfiles constructs the runtime profiles from config. The firecrawl
// (default, unprefixed) profile always comes first. Tavily is appended only
// when TAVILY_API_KEYS is set. Each profile gets its own KeyPool with its own
// thresholds.
func buildProfiles(cfg Config) []*Profile {
	fcPool := NewKeyPool(cfg.APIKeys)
	fcPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	profiles := []*Profile{{
		Name:           "firecrawl",
		Upstream:       cfg.Upstream,
		UpstreamHost:   cfg.UpstreamHost,
		CreditResetDay: cfg.CreditResetDay,
		RewriteNext:    true,
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
			pool:           tvPool,
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

// isCreditExhausted reports whether a rejection means the key's credits are
// genuinely gone until reset (disables the key). Rate-limit/auth never
// disable.
//
// firecrawl: 402, or a failure envelope mentioning credits/payment.
// tavily: 432 (plan limit) / 433 (pay-as-you-go limit), status only.
func (p *Profile) isCreditExhausted(status int, body []byte) bool {
	if p.Name == "tavily" {
		return status == 432 || status == 433
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
// stripped. The no-prefix profile is the fallback for everything else,
// including prefixes that were never configured.
func matchProfile(profiles []*Profile, path string) (*Profile, string, bool) {
	var def *Profile
	for _, p := range profiles {
		if p.RoutePrefix == "" {
			def = p
			continue
		}
		if path == p.RoutePrefix {
			return p, "/", true
		}
		if strings.HasPrefix(path, p.RoutePrefix+"/") {
			return p, path[len(p.RoutePrefix):], true
		}
	}
	if def != nil {
		return def, path, true
	}
	return nil, path, false
}
