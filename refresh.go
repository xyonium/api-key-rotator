package main

import (
	"net/http"
	"sync"
	"time"
)

// lowRefreshThreshold is the predicted-credit level below which a key is
// refreshed more often (every r.lowInterval) instead of only on switch or
// daily. Matches the user's "< 100 -> every few minutes" rule. For apify the
// pool tracks CENTS, so 100 credits would mean $1.00 - too coarse; apify uses
// its own cent-denominated threshold derived from its low-water mark.
const (
	lowRefreshThreshold      = 100
	apifyLowRefreshCents     = 100 // $1.00: re-check a low apify balance often
	dailyRefresh             = 24 * time.Hour
)

// Refresher applies the on-demand credit-refresh strategy to a KeyPool:
//   - refresh the key we just switched AWAY from and the one we switched TO
//     (on rotation),
//   - refresh any key whose PREDICTED remaining has dropped below
//     lowRefreshThreshold, throttled to once per lowInterval (CREDIT_REFRESH_INTERVAL),
//   - refresh every key once per day as a catch-all.
// It never blocks the request path: refreshes run in their own goroutine.
type Refresher struct {
	profile     *Profile
	client      *http.Client
	cfg         Config
	keys        []string
	log         *logger
	lowInterval time.Duration // CREDIT_REFRESH_INTERVAL as a Duration

	mu        sync.Mutex
	lastLow   []time.Time // last refresh-at when predicted was < lowRefreshThreshold
	lastDaily []time.Time // last daily refresh-at per key
}

func NewRefresher(p *Profile, client *http.Client, cfg Config, log *logger) *Refresher {
	n := len(p.pool.Snapshot().Keys)
	return &Refresher{
		profile:     p,
		client:      client,
		cfg:         cfg,
		keys:        p.pool.keys,
		log:         log,
		lowInterval: time.Duration(cfg.CreditRefreshSec) * time.Second,
		lastLow:     make([]time.Time, n),
		lastDaily:   make([]time.Time, n),
	}
}

// OnSwitch refreshes the key we rotated off (fromIdx) and onto (toIdx) in the
// background. Called by the rotator whenever it advances to a different key.
func (r *Refresher) OnSwitch(fromIdx, toIdx int) {
	if r == nil {
		return
	}
	go func() {
		if fromIdx >= 0 && fromIdx < len(r.keys) {
			r.refreshOne(fromIdx)
		}
		if toIdx >= 0 && toIdx < len(r.keys) && toIdx != fromIdx {
			r.refreshOne(toIdx)
		}
	}()
}

// lowMark is the predicted-balance level below which MaybeRefreshLow kicks in.
// Apify tracks cents, so it needs a cent-denominated mark; other profiles use
// the shared credit mark.
func (r *Refresher) lowMark() int64 {
	if r.profile != nil && r.profile.Name == "apify" {
		return apifyLowRefreshCents
	}
	return lowRefreshThreshold
}

// MaybeRefreshLow refreshes a key in the background if its PREDICTED remaining
// is below the low-water mark and it hasn't been refreshed in lowInterval.
// Called by the rotator after each successful response.
func (r *Refresher) MaybeRefreshLow(idx int) {
	if r == nil || idx < 0 || idx >= len(r.keys) {
		return
	}
	// Check predicted balance without holding the refresher lock.
	predicted := r.profile.pool.Snapshot().Keys[idx].RemainingCredits // -1 = unmeasured
	if predicted < 0 || predicted >= r.lowMark() {
		return
	}
	r.mu.Lock()
	last := r.lastLow[idx]
	if !last.IsZero() && time.Since(last) < r.lowInterval {
		r.mu.Unlock()
		return
	}
	r.lastLow[idx] = time.Now()
	r.mu.Unlock()

	go r.refreshOne(idx)
}

// DailyRefresh refreshes every key whose last daily refresh is older than
// dailyRefresh (or never). Intended to be called by a periodic ticker.
func (r *Refresher) DailyRefresh() {
	if r == nil {
		return
	}
	now := time.Now()
	for i := range r.keys {
		r.mu.Lock()
		last := r.lastDaily[i]
		if !last.IsZero() && now.Sub(last) < dailyRefresh {
			r.mu.Unlock()
			continue
		}
		r.lastDaily[i] = now
		r.mu.Unlock()
		go r.refreshOne(i)
	}
}

// RefreshAll force-refreshes every key now (used at startup warm-up). Returns
// the count of keys that are STILL unmeasured after the attempt (i.e. their
// fetch failed). The caller retries when this is > 0 so a transient startup
// network blip self-heals instead of stranding keys at "unmeasured" for a day.
func (r *Refresher) RefreshAll() int {
	if r == nil {
		return 0
	}
	var wg sync.WaitGroup
	for i := range r.keys {
		wg.Add(1)
		r.mu.Lock()
		r.lastDaily[i] = time.Now()
		r.mu.Unlock()
		go func(i int) {
			defer wg.Done()
			r.refreshOne(i)
		}(i)
	}
	wg.Wait()
	unmeasured := 0
	for _, k := range r.profile.pool.Snapshot().Keys {
		if k.RemainingCredits < 0 {
			unmeasured++
		}
	}
	return unmeasured
}

// EnsureMeasured synchronously refreshes key idx only if it is currently
// unmeasured (no balance ever fetched). Called on the request path so a key's
// real balance gates selection from the first request, instead of serving
// blind at "unmeasured = plenty" until the background warm-up lands. A
// successful fetch marks the key fresh for the daily loop; a failed fetch
// leaves it unmeasured (fail-open) without tripping the low-refresh throttle.
func (r *Refresher) EnsureMeasured(idx int) {
	if r == nil || idx < 0 || idx >= len(r.keys) {
		return
	}
	if r.profile.pool.Snapshot().Keys[idx].RemainingCredits >= 0 {
		return // already measured
	}
	r.refreshOne(idx)
}

// refreshOne fetches one key's live usage and applies it. Updates lastDaily so
// the daily loop treats it as freshly refreshed.
func (r *Refresher) refreshOne(idx int) {
	if idx < 0 || idx >= len(r.keys) {
		return
	}
	got := refreshKey(r.profile, r.client, idx, r.keys[idx], r.log)
	if got >= 0 {
		r.log.debug("refreshed credits", "key", idx, "remaining", got)
		r.mu.Lock()
		r.lastDaily[idx] = time.Now()
		r.mu.Unlock()
	}
}

// OnSwitchReason is OnSwitch plus the status that caused the rotation.
//
// The key we rotated OFF (fromIdx) is only refreshed when the rotation reason
// could actually have changed its balance: a credit/limit rejection (402, 432,
// 433). A 429, 401, 403-that-exhausted-backoff or a network error proves
// nothing about the balance -- the request was refused before it was billed --
// so refreshing it just spends a /usage call.
//
// The key we rotated ONTO (toIdx) is NOT refreshed here either, for the same
// reason: every reason that reaches this call other than a credit/limit
// rejection left its balance untouched. Its first-ever measurement is already
// handled synchronously on the request path by EnsureMeasured (proxy.go), so
// skipping it here cannot leave a key "unmeasured = plenty".
//
// This matters because Tavily rate-limits /usage to 10 requests per 10 MINUTES,
// far tighter than /search: a 403/429 storm that rotates 8 times per client
// request blew the usage budget on its own (32 rotations -> 64 fetches in the
// 2026-08-28 incident), which then showed up as the spurious
// "usage fetch failed reason=status:429" warnings at the end of each burst.
func (r *Refresher) OnSwitchReason(fromIdx, toIdx, status int) {
	if r == nil {
		return
	}
	if !balanceMayHaveChanged(status) {
		return
	}
	go func() {
		if fromIdx >= 0 && fromIdx < len(r.keys) {
			r.refreshOne(fromIdx)
		}
		if toIdx >= 0 && toIdx < len(r.keys) && toIdx != fromIdx {
			r.refreshOne(toIdx)
		}
	}()
}

// balanceMayHaveChanged reports whether a rotation caused by this status could
// have moved the key's balance, making a /usage re-fetch worth its cost.
// Credit/limit rejections mean the account is at or past a cap, so the real
// balance is unknown and worth reading. Everything else (rate limits, auth
// failures, edge blocks like a 403, network errors, 5xx) means the request was
// refused before being billed, so the last known balance is still accurate.
func balanceMayHaveChanged(status int) bool {
	switch status {
	case 402, 432, 433:
		return true
	}
	return false
}
