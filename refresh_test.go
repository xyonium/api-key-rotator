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
