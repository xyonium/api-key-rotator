package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefresher_LowIntervalFromConfig verifies CREDIT_REFRESH_INTERVAL (seconds)
// actually controls the low-balance refresh throttle. Regression for a bug where
// the interval was a hardcoded constant and the env var was ignored.
func TestRefresher_LowIntervalFromConfig(t *testing.T) {
	pool := NewKeyPool([]string{"fc-a"})
	pool.SetThresholds(10, 2)
	pool.SetCredits(0, 50) // below lowRefreshThreshold(100) -> eligible for low refresh

	cfg := Config{
		APIKeys:          []string{"fc-a"},
		Upstream:         "http://unused",
		CreditRefreshSec: 300, // 5 minutes
	}
	p := &Profile{
		Name:         "firecrawl",
		Upstream:     cfg.Upstream,
		UpstreamHost: cfg.UpstreamHost,
		pool:         pool,
	}
	r := NewRefresher(p, &http.Client{}, cfg, newLogger("info"))
	if r.lowInterval != 300*time.Second {
		t.Fatalf("lowInterval = %v, want 300s (CREDIT_REFRESH_INTERVAL must be honored)", r.lowInterval)
	}

	// After one refresh, a second MaybeRefreshLow within the interval is throttled.
	r.lastLow[0] = time.Now()
	// Swap the fetcher out for a counting stub via a fake upstream would be heavy;
	// instead assert the throttle guard returns early (no goroutine spawned) by
	// checking that lastLow is unchanged after the call.
	before := r.lastLow[0]
	r.MaybeRefreshLow(0)
	// give any spawned goroutine a moment, then confirm lastLow wasn't advanced
	time.Sleep(20 * time.Millisecond)
	if !r.lastLow[0].Equal(before) {
		t.Fatalf("lastLow advanced despite being within throttle window: was %v, now %v", before, r.lastLow[0])
	}
}

// TestRefresher_RefreshAllFetchesUsage confirms RefreshAll hits credit-usage and
// sets remainingCredits on the pool.
func TestRefresher_RefreshAllFetchesUsage(t *testing.T) {
	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"remainingCredits":4242,"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`))
	}))
	defer fake.Close()

	pool := NewKeyPool([]string{"fc-a", "fc-b"})
	pool.SetThresholds(10, 2)
	cfg := Config{APIKeys: []string{"fc-a", "fc-b"}, Upstream: fake.URL, UpstreamHost: httptestURLHost(fake.URL), CreditRefreshSec: 60}
	p := &Profile{
		Name:         "firecrawl",
		Upstream:     cfg.Upstream,
		UpstreamHost: cfg.UpstreamHost,
		pool:         pool,
	}
	r := NewRefresher(p, &http.Client{}, cfg, newLogger("info"))
	r.RefreshAll()

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("credit-usage hits = %d, want 2 (one per key)", got)
	}
	snap := pool.Snapshot()
	if snap.Keys[0].RemainingCredits != 4242 || snap.Keys[1].RemainingCredits != 4242 {
		t.Fatalf("remainingCredits = %d/%d, want 4242/4242", snap.Keys[0].RemainingCredits, snap.Keys[1].RemainingCredits)
	}
}

// TestOnSwitch_ReasonGatedFetches guards Tavily's /usage rate limit (only 10
// requests per 10 MINUTES -- far tighter than /search). A rotation whose reason
// proves the balance did NOT change must not spend a /usage call.
//
// Regression for the 2026-08-28 incident: a burst of AWS-edge 403s rotated
// 32 times, and every rotation refreshed TWO keys, so 64 /usage calls blew a
// 10-per-10-minutes budget. Those failures then surfaced as the misleading
// "usage fetch failed reason=status:429" warnings at the end of each burst,
// which looked like a second bug but were collateral.
func TestOnSwitch_ReasonGatedFetches(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantHit bool
	}{
		{"netErr_no_response: balance unchanged", 0, false},
		{"403_backoff_exhausted: balance unchanged", 403, false},
		{"429_rate_limit: balance unchanged", 429, false},
		{"401_auth: balance unchanged", 401, false},
		{"500_upstream: balance unchanged", 500, false},
		{"432_plan_limit: balance moved", 432, true},
		{"433_paygo_limit: balance moved", 433, true},
		{"402_credit_exhausted: balance moved", 402, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var usageHits int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/usage" {
					atomic.AddInt32(&usageHits, 1)
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"key":{"usage":10,"limit":null},"account":{"plan_usage":10,"plan_limit":1000,"paygo_usage":0,"paygo_limit":null}}`))
					return
				}
				w.WriteHeader(599)
			}))
			defer fake.Close()

			pool := NewKeyPool([]string{"tv-a", "tv-b"})
			pool.SetThresholds(10, 2)
			p := &Profile{
				Name:           "tavily",
				Upstream:       fake.URL,
				UpstreamHost:   httptestURLHost(fake.URL),
				CreditResetDay: 1,
				pool:           pool,
			}
			r := NewRefresher(p, fake.Client(), Config{}, newLogger("error"))

			r.OnSwitchReason(0, 1, tc.status)

			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				if atomic.LoadInt32(&usageHits) > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond) // let a second goroutine land if any

			got := atomic.LoadInt32(&usageHits) > 0
			if got != tc.wantHit {
				t.Fatalf("status %d: /usage fetched = %v, want %v (hits=%d)", tc.status, got, tc.wantHit, atomic.LoadInt32(&usageHits))
			}
		})
	}
}
