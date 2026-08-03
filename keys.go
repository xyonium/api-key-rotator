package main

import (
	"context"
	"math"
	"sync"
	"time"
)

type keyStat struct {
	Success     int `json:"success"`
	Exhausted   int `json:"exhausted"`
	RateLimited int `json:"rateLimited"`
	Auth        int `json:"auth"`
	Retries     int `json:"retries"`
}

type keySnapshot struct {
	Index           int       `json:"index"`
	Last4           string    `json:"last4"`
	Stats           keyStat   `json:"stats"`
	Disabled        bool      `json:"disabled"`
	DisabledUntil   time.Time `json:"disabledUntil,omitempty"`
	RemainingCredits int64    `json:"remainingCredits"`
}

type PoolSnapshot struct {
	PoolSize     int           `json:"poolSize"`
	CurrentIndex int           `json:"currentIndex"`
	Keys         []keySnapshot `json:"keys"`
}

type KeyPool struct {
	mu     sync.Mutex
	keys   []string
	cursor int
	stats  []keyStat
	// disabled[i] is true when key i is credit-exhausted until resetDay.
	// Rate-limit (429) and auth (401/403) do NOT disable - they are transient
	// or global and disabling would take a good key offline.
	disabled      []bool
	disabledUntil []time.Time
	// remainingCredits[i] is the last known credit balance for key i. Unknown
	// until the first refresh; math.MaxInt64 means "unmeasured, assume plenty".
	remainingCredits []int64
	lowThreshold     int64 // switch off a key at/below this
	stopThreshold    int64 // stop the pool when every key is below this
	// cooldownUntil[i] is set when key i is rotated off, so the next selection
	// prefers a different key even when credits are equal. A key past its
	// cooldown is eligible again.
	cooldownUntil []time.Time
	// maxConcurrent caps how many requests may be in-flight on a single key at
	// once (0 = unlimited). inFlight[i] is the current count for key i. slotFree
	// is a buffered size-1 wakeup token for WaitForSlot; it is only a hint -
	// correctness comes from re-checking state under mu, never from the token.
	maxConcurrent int
	inFlight      []int
	slotFree      chan struct{}
}

func NewKeyPool(keys []string) *KeyPool {
	p := &KeyPool{
		keys:             keys,
		stats:            make([]keyStat, len(keys)),
		disabled:         make([]bool, len(keys)),
		disabledUntil:    make([]time.Time, len(keys)),
		remainingCredits: make([]int64, len(keys)),
		cooldownUntil:    make([]time.Time, len(keys)),
		inFlight:         make([]int, len(keys)),
		slotFree:         make(chan struct{}, 1),
	}
	for i := range keys {
		p.remainingCredits[i] = math.MaxInt64 // unmeasured
	}
	return p
}

// SetThresholds configures the low (switch-off) and stop (refuse-requests)
// credit thresholds. Must be called once at startup.
func (p *KeyPool) SetThresholds(low, stop int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lowThreshold = low
	p.stopThreshold = stop
}

// SetMaxConcurrent caps in-flight requests per key. n <= 0 means unlimited
// (the default for pools that never call this, e.g. tavily/apify).
func (p *KeyPool) SetMaxConcurrent(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 0 {
		n = 0
	}
	p.maxConcurrent = n
}

// hasSlotLocked reports whether key i has a free in-flight slot. Unlimited
// pools always have a slot. Caller must hold mu.
func (p *KeyPool) hasSlotLocked(i int) bool {
	return p.maxConcurrent <= 0 || p.inFlight[i] < p.maxConcurrent
}

// anySlotFreeLocked reports whether any key has a free slot. Caller must hold mu.
func (p *KeyPool) anySlotFreeLocked() bool {
	for i := range p.keys {
		if p.hasSlotLocked(i) {
			return true
		}
	}
	return false
}

// TryAcquire takes an in-flight slot on key index without blocking. Returns
// false if the key is at its concurrency cap (nothing acquired). Unlimited
// pools always succeed and do not count.
func (p *KeyPool) TryAcquire(index int) bool {
	if p.maxConcurrent <= 0 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.keys) || p.inFlight[index] >= p.maxConcurrent {
		return false
	}
	p.inFlight[index]++
	return true
}

// Release returns an in-flight slot on key index and wakes any WaitForSlot
// waiters. Safe to call on invalid indexes and on over-release (guarded).
func (p *KeyPool) Release(index int) {
	if p.maxConcurrent <= 0 {
		return
	}
	p.mu.Lock()
	if index >= 0 && index < len(p.keys) && p.inFlight[index] > 0 {
		p.inFlight[index]--
	}
	p.mu.Unlock()
	// Wake one waiter (non-blocking; stale tokens are harmless).
	select {
	case p.slotFree <- struct{}{}:
	default:
	}
}

// WaitForSlot blocks until any key has a free in-flight slot, the timeout
// elapses, or ctx is canceled. It never holds the pool mutex while waiting;
// each wake re-checks state under mu so no wakeup is lost. Unlimited pools
// return true immediately.
func (p *KeyPool) WaitForSlot(ctx context.Context, timeout time.Duration) bool {
	if p.maxConcurrent <= 0 {
		return true
	}
	if timeout <= 0 {
		p.mu.Lock()
		ok := p.anySlotFreeLocked()
		p.mu.Unlock()
		return ok
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		p.mu.Lock()
		if p.anySlotFreeLocked() {
			p.mu.Unlock()
			return true
		}
		ch := p.slotFree
		p.mu.Unlock()
		select {
		case <-ch:
			continue // re-check; another goroutine may have taken the slot
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// rotateCooldown is how long a just-rotated-off key is deprioritized, so equal-
// credit keys actually take turns instead of the same one being re-picked.
const rotateCooldown = 30 * time.Second

// currentLocked picks the best usable key in three tiers:
//  1. has a free in-flight slot AND not in cooldown -> highest credits
//  2. has a free slot, ignoring cooldown -> highest credits (existing fallback)
//  3. any usable key, ignoring slot AND cooldown -> so that when EVERY usable
//     key is saturated Current still returns a key (>= 0), letting the caller
//     distinguish SATURATED (busy -> queue/reject) from EXHAUSTED (below stop
//     or disabled -> -1 -> 503). With maxConcurrent == 0 (unlimited), tiers
//     1-2 cover every usable key and the behavior is byte-for-byte the legacy
//     two-tier selection.
func (p *KeyPool) currentLocked() (int, string) {
	now := time.Now()
	best := -1
	var bestCredits int64 = -1
	fallbackSlot := -1
	var fbSlotCredits int64 = -1
	fallbackAny := -1
	var fbAnyCredits int64 = -1
	for i := range p.keys {
		if p.disabled[i] {
			continue
		}
		rc := p.remainingCredits[i]
		if rc < p.stopThreshold {
			continue
		}
		// Tier 3: any usable key, ignoring slot and cooldown.
		if rc > fbAnyCredits {
			fbAnyCredits = rc
			fallbackAny = i
		}
		if !p.hasSlotLocked(i) {
			continue
		}
		// Tier 2: free slot, ignoring cooldown.
		if rc > fbSlotCredits {
			fbSlotCredits = rc
			fallbackSlot = i
		}
		if !p.cooldownUntil[i].IsZero() && now.Before(p.cooldownUntil[i]) {
			continue
		}
		// Tier 1: free slot AND not in cooldown.
		if rc > bestCredits {
			bestCredits = rc
			best = i
		}
	}
	if best < 0 {
		best = fallbackSlot
	}
	if best < 0 {
		best = fallbackAny
	}
	if best >= 0 {
		p.cursor = best
		return best, p.keys[best]
	}
	return -1, ""
}

// Current returns the best usable key (highest remainingCredits >= stop
// threshold, preferring keys not in cooldown). Returns (-1,"") if none usable.
func (p *KeyPool) Current() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentLocked()
}

// Advance marks the current key as in cooldown (so it is deprioritized) and
// returns the next best key. Returns (-1,"") if none usable.
func (p *KeyPool) Advance() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cursor >= 0 && p.cursor < len(p.cooldownUntil) {
		p.cooldownUntil[p.cursor] = time.Now().Add(rotateCooldown)
	}
	return p.currentLocked()
}

func (p *KeyPool) RecordSuccess(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index >= 0 && index < len(p.stats) {
		p.stats[index].Success++
		// A successful key is proven good: clear its cooldown so it is
		// preferred again next time (it likely still has the most credits).
		if index < len(p.cooldownUntil) {
			p.cooldownUntil[index] = time.Time{}
		}
	}
}

func (p *KeyPool) RecordRejection(index int, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch kind {
	case "exhausted":
		p.stats[index].Exhausted++
	case "rate":
		p.stats[index].RateLimited++
	case "auth":
		p.stats[index].Auth++
	case "retry":
		p.stats[index].Retries++
	default:
		panic("unknown rejection kind: " + kind)
	}
}

// Decrement subtracts cost from a key's remainingCredits (local estimate
// between refreshes). cost <= 0 is treated as 1 (a best-effort estimate when
// the response does not report creditsUsed). Never drops below 0.
func (p *KeyPool) Decrement(index int, cost int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.keys) {
		return
	}
	if cost <= 0 {
		cost = 1
	}
	rc := p.remainingCredits[index]
	if rc == math.MaxInt64 {
		return // unmeasured: don't estimate down from "infinite"
	}
	rc -= cost
	if rc < 0 {
		rc = 0
	}
	p.remainingCredits[index] = rc
}

// SetCredits records a real remainingCredits value from /v2/team/credit-usage.
func (p *KeyPool) SetCredits(index int, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.keys) {
		return
	}
	if credits < 0 {
		credits = 0
	}
	p.remainingCredits[index] = credits
}

// Disable marks a key as credit-exhausted until t. A disabled key is skipped by
// Current/Advance. Use only for genuine credit exhaustion; never for 429/401/403.
func (p *KeyPool) Disable(index int, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.keys) {
		return
	}
	p.disabled[index] = true
	p.disabledUntil[index] = until
	if p.remainingCredits[index] != math.MaxInt64 {
		p.remainingCredits[index] = 0
	}
}

// AnyUsable reports whether at least one key can serve a request: non-disabled
// AND remainingCredits >= stopThreshold. /healthz uses this to return 503.
func (p *KeyPool) AnyUsable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.keys {
		if !p.disabled[i] && p.remainingCredits[i] >= p.stopThreshold {
			return true
		}
	}
	return false
}

// ReenableDue re-enables every disabled key whose disabledUntil is at or before
// now. Returns the count re-enabled. Because each key stores its own reset
// instant (fetched from that account's billing period), keys on different
// billing anniversaries come back online independently.
func (p *KeyPool) ReenableDue(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.disabled {
		if p.disabled[i] && !p.disabledUntil[i].IsZero() && !p.disabledUntil[i].After(now) {
			p.disabled[i] = false
			p.disabledUntil[i] = time.Time{}
			// credits will be corrected by the next refresh; mark unmeasured so
			// the key is immediately considered usable again.
			p.remainingCredits[i] = math.MaxInt64
			n++
		}
	}
	return n
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return k
	}
	return k[len(k)-4:]
}

func (p *KeyPool) Snapshot() PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]keySnapshot, len(p.keys))
	for i, k := range p.keys {
		ks := keySnapshot{
			Index:    i,
			Last4:    maskKey(k),
			Stats:    p.stats[i],
			Disabled: p.disabled[i],
		}
		if p.disabled[i] {
			ks.DisabledUntil = p.disabledUntil[i]
		}
		rc := p.remainingCredits[i]
		if rc == math.MaxInt64 {
			ks.RemainingCredits = -1 // signal "unmeasured" to clients
		} else {
			ks.RemainingCredits = rc
		}
		keys[i] = ks
	}
	return PoolSnapshot{PoolSize: len(p.keys), CurrentIndex: p.cursor, Keys: keys}
}
