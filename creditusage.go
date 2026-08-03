package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// usage holds a key's live credit balance and billing reset instant. remaining
// is in the profile's credit unit (credits for firecrawl/tavily, CENTS for
// apify). periodEnd is meaningful only when hasEnd is true. ok is false when
// the call failed or the response was unparseable.
type usage struct {
	remaining int64
	periodEnd time.Time
	hasEnd    bool
	ok        bool
}

// usageBackoff is the retry schedule for TRANSIENT credit-usage failures only
// (network errors, 408, 5xx). Permanent failures (404/401/403/400) are not
// retried - they usually mean the key's account can't access this endpoint.
// Stored in an atomic.Value because usage-refresh goroutines read it off the
// request path while tests override it; a plain package var races under -race.
var usageBackoff atomic.Value // []time.Duration

func init() {
	usageBackoff.Store([]time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
	})
}

// usageBackoffSchedule returns the current usage retry schedule.
func usageBackoffSchedule() []time.Duration {
	return usageBackoff.Load().([]time.Duration)
}

// usageRetryable reports whether a non-200 status is worth retrying.
func usageRetryable(status int) bool {
	switch status {
	case 408, 500, 502, 503, 504:
		return true
	}
	return false
}

// fetchUsage queries a key's own billing period: remaining credits and reset
// instant. The /v2/team/credit-usage endpoint is read-only and consumes no
// credits, so it works even after the key is exhausted. Retries transient
// failures (network, 408, 5xx) with backoff; permanent failures (404/401/403)
// return immediately. log may be nil.
func fetchUsage(client *http.Client, upstream, key string, log *logger) usage {
	const timeout = 5 * time.Second
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	} else {
		c = &http.Client{Transport: c.Transport, Timeout: timeout}
	}

	var lastReason string
	backoff := usageBackoffSchedule()
	for attempt := 0; attempt <= len(backoff); attempt++ {
		u, reason := fetchUsageOnce(c, upstream, key)
		if u.ok {
			return u
		}
		lastReason = reason
		// Retry only transient reasons. fetchUsageOnce returns reason prefixed
		// "status:" for non-200 and "net:" for network errors; we retry those
		// (status that is retryable, or any net error) but not permanent status.
		if !shouldRetryUsage(reason) || attempt >= len(backoff) {
			break
		}
		time.Sleep(backoff[attempt])
	}
	if log != nil {
		log.warn("credit-usage fetch failed", "reason", lastReason, "masked", maskKey(key))
	}
	return usage{}
}

// shouldRetryUsage decides whether to retry based on the reason string emitted
// by fetchUsageOnce. "net:" always retries; "status:N" retries only for
// transient N (408/5xx); parse errors and permanent statuses don't.
func shouldRetryUsage(reason string) bool {
	if strings.HasPrefix(reason, "net:") {
		return true
	}
	if strings.HasPrefix(reason, "status:") {
		code, err := strconv.Atoi(strings.TrimPrefix(reason, "status:"))
		if err != nil {
			return false
		}
		return usageRetryable(code)
	}
	return false
}

// fetchUsageOnce performs a single credit-usage request. Returns the usage and
// a short reason string on failure ("net:...", "status:N", "parse:...",
// "nobody").
func fetchUsageOnce(c *http.Client, upstream, key string) (usage, string) {
	req, err := http.NewRequest(http.MethodGet, upstream+"/v2/team/credit-usage", nil)
	if err != nil {
		return usage{}, "build:" + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return usage{}, "net:" + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return usage{}, "status:" + strconv.Itoa(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return usage{}, "read:" + err.Error()
	}
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			RemainingCredits int64  `json:"remainingCredits"`
			BillingPeriodEnd string `json:"billingPeriodEnd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return usage{}, "parse:" + err.Error()
	}
	u := usage{remaining: env.Data.RemainingCredits, ok: true}
	if env.Data.BillingPeriodEnd != "" {
		if t, err := time.Parse(time.RFC3339, env.Data.BillingPeriodEnd); err == nil {
			u.periodEnd = t
			u.hasEnd = true
		}
	}
	return u, ""
}

// fetchUsageFor dispatches to the profile's provider usage endpoint.
func fetchUsageFor(p *Profile, client *http.Client, key string, log *logger) usage {
	if p.Name == "tavily" {
		return fetchTavilyUsage(client, p.Upstream, key, log)
	}
	if p.Name == "apify" {
		return fetchApifyUsage(client, p, key, log)
	}
	return fetchUsage(client, p.Upstream, key, log)
}

// fetchApifyUsage reads a token's balance from GET {upstream}/v2/users/me/limits
// (read-only). Apify bills in USD against an included monthly credit, so
// remaining is computed as (includedCredit - current.monthlyUsageUsd) in CENTS.
// The included credit is the account's own limits.maxMonthlyUsageUsd (read
// fresh each call, so a plan change is picked up automatically), unless
// ApifyFreeCreditUsd > 0 overrides it. periodEnd is the real
// monthlyUsageCycle.endAt (the billing anniversary, NOT the 1st of the month).
// Retries transient failures like the other providers.
func fetchApifyUsage(client *http.Client, p *Profile, key string, log *logger) usage {
	const timeout = 5 * time.Second
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	} else {
		c = &http.Client{Transport: c.Transport, Timeout: timeout}
	}

	var lastReason string
	backoff := usageBackoffSchedule()
	for attempt := 0; attempt <= len(backoff); attempt++ {
		u, reason := fetchApifyUsageOnce(c, p, key)
		if u.ok {
			return u
		}
		lastReason = reason
		if !shouldRetryUsage(reason) || attempt >= len(backoff) {
			break
		}
		time.Sleep(backoff[attempt])
	}
	if log != nil {
		log.warn("apify limits fetch failed", "reason", lastReason, "masked", maskKey(key))
	}
	return usage{}
}

func fetchApifyUsageOnce(c *http.Client, p *Profile, key string) (usage, string) {
	req, err := http.NewRequest(http.MethodGet, p.Upstream+"/v2/users/me/limits?token="+url.QueryEscape(key), nil)
	if err != nil {
		return usage{}, "build:" + err.Error()
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return usage{}, "net:" + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return usage{}, "status:" + strconv.Itoa(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return usage{}, "read:" + err.Error()
	}
	var env struct {
		Data struct {
			MonthlyUsageCycle struct {
				StartAt string `json:"startAt"`
				EndAt   string `json:"endAt"`
			} `json:"monthlyUsageCycle"`
			Limits struct {
				MaxMonthlyUsageUsd float64 `json:"maxMonthlyUsageUsd"`
			} `json:"limits"`
			Current struct {
				MonthlyUsageUsd float64 `json:"monthlyUsageUsd"`
			} `json:"current"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return usage{}, "parse:" + err.Error()
	}
	// Included monthly credit: explicit override wins; otherwise use the
	// account's reported value so a plan change is picked up automatically.
	credit := p.ApifyFreeCreditUsd
	if credit <= 0 {
		credit = env.Data.Limits.MaxMonthlyUsageUsd
	}
	// remaining in cents; clamp at 0 when over the included credit.
	remUsd := credit - env.Data.Current.MonthlyUsageUsd
	remCents := int64(math.Round(remUsd * 100))
	if remCents < 0 {
		remCents = 0
	}
	u := usage{remaining: remCents, ok: true}
	if env.Data.MonthlyUsageCycle.EndAt != "" {
		if t, err := time.Parse(time.RFC3339, env.Data.MonthlyUsageCycle.EndAt); err == nil {
			u.periodEnd = t
			u.hasEnd = true
		}
	}
	return u, ""
}

// fetchTavilyUsage reads a key's usage from GET {upstream}/usage (read-only,
// no credit cost). remaining is the min across the key/plan/paygo limit
// layers; periodEnd is always zero (Tavily exposes no billing period end).
func fetchTavilyUsage(client *http.Client, upstream, key string, log *logger) usage {
	const timeout = 5 * time.Second
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	} else {
		c = &http.Client{Transport: c.Transport, Timeout: timeout}
	}

	var lastReason string
	backoff := usageBackoffSchedule()
	for attempt := 0; attempt <= len(backoff); attempt++ {
		u, reason := fetchTavilyUsageOnce(c, upstream, key)
		if u.ok {
			return u
		}
		lastReason = reason
		if !shouldRetryUsage(reason) || attempt >= len(backoff) {
			break
		}
		time.Sleep(backoff[attempt])
	}
	if log != nil {
		log.warn("tavily usage fetch failed", "reason", lastReason, "masked", maskKey(key))
	}
	return usage{}
}

func fetchTavilyUsageOnce(c *http.Client, upstream, key string) (usage, string) {
	req, err := http.NewRequest(http.MethodGet, upstream+"/usage", nil)
	if err != nil {
		return usage{}, "build:" + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return usage{}, "net:" + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return usage{}, "status:" + strconv.Itoa(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return usage{}, "read:" + err.Error()
	}
	var env struct {
		Key struct {
			Usage *int64 `json:"usage"`
			Limit *int64 `json:"limit"`
		} `json:"key"`
		Account struct {
			PlanUsage  *int64 `json:"plan_usage"`
			PlanLimit  *int64 `json:"plan_limit"`
			PaygoUsage *int64 `json:"paygo_usage"`
			PaygoLimit *int64 `json:"paygo_limit"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return usage{}, "parse:" + err.Error()
	}
	rem, ok := tavilyRemaining(env.Key.Usage, env.Key.Limit,
		env.Account.PlanUsage, env.Account.PlanLimit,
		env.Account.PaygoUsage, env.Account.PaygoLimit)
	if !ok {
		return usage{}, "parse:no limit layers"
	}
	return usage{remaining: rem, ok: true}, ""
}

// tavilyRemaining computes the effective remaining credits as the minimum over
// the limit layers present. A nil usage or limit (JSON null/absent) means the
// layer is unmeasured and is skipped. An explicit limit of 0 means the layer
// genuinely has no credits (remaining 0), NOT unlimited. ok is false when
// every layer is unmeasured (caller treats the key as unmeasured).
func tavilyRemaining(keyUsage, keyLimit, planUsage, planLimit, paygoUsage, paygoLimit *int64) (int64, bool) {
	layers := [][2]*int64{
		{keyUsage, keyLimit},
		{planUsage, planLimit},
		{paygoUsage, paygoLimit},
	}
	best := int64(-1)
	for _, l := range layers {
		used, limit := l[0], l[1]
		if used == nil || limit == nil {
			continue // unmeasured layer
		}
		rem := *limit - *used
		if rem < 0 {
			rem = 0
		}
		if best < 0 || rem < best {
			best = rem
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

// refreshKey fetches a key's live usage (via the profile's provider) and
// applies it to the profile's pool. Returns the fetched remaining credits
// (-1 if the call failed).
func refreshKey(p *Profile, client *http.Client, index int, key string, log *logger) int64 {
	u := fetchUsageFor(p, client, key, log)
	if !u.ok {
		return -1
	}
	p.pool.SetCredits(index, u.remaining)
	return u.remaining
}

// disableUntilReset disables key index in the profile's pool. Firecrawl keys
// reset at their real billing-period end when available, and Apify keys at
// their real monthlyUsageCycle.endAt; Tavily exposes no period end, so it
// always uses the CREDIT_RESET_DAY fallback (as does any profile whose usage
// fetch fails to return a period end).
func disableUntilReset(p *Profile, client *http.Client, index int, key string, now time.Time, log *logger) {
	fallback := fallbackReset(now, p.CreditResetDay)
	reset := fallback
	if p.Name != "tavily" {
		u := fetchUsageFor(p, client, key, log)
		if u.ok && u.hasEnd && !u.periodEnd.Before(now) && !u.periodEnd.After(now.AddDate(1, 0, 0)) {
			reset = u.periodEnd
		}
	}
	p.pool.Disable(index, reset)
}
