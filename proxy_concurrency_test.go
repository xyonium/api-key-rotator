package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concTestRotator builds a single firecrawl profile with a concurrency cap and
// saturation policy for testing the rotator's concurrency paths.
func concTestRotator(cfg Config, pool *KeyPool, client *http.Client, maxConc int, saturation string, queue time.Duration, max403 int) *rotator {
	pool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	pool.SetMaxConcurrent(maxConc)
	p := &Profile{
		Name:                  "firecrawl",
		Upstream:              cfg.Upstream,
		UpstreamHost:          cfg.UpstreamHost,
		CreditResetDay:        cfg.CreditResetDay,
		RewriteNext:           true,
		ConcurrencySaturation: saturation,
		ConcurrencyQueue:      queue,
		Max403Retries:         max403,
		pool:                  pool,
	}
	return newRotator(cfg, []*Profile{p}, client, newLogger("info"))
}

// TestRotator_ConcurrencySerializesSameKey: one key, cap 1 -> concurrent
// requests never exceed 1 in-flight upstream; queue lets them through serially.
func TestRotator_ConcurrencySerializesSameKey(t *testing.T) {
	var inflight, maxInflight atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if cur <= m || maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond) // hold the slot
		inflight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "queue", 2*time.Second, 0)

	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = post(t, r, "/v2/scrape", `{"url":"https://x"}`).Code
		}(i)
	}
	wg.Wait()
	if got := maxInflight.Load(); got > 1 {
		t.Fatalf("max concurrent upstream in-flight = %d, want <= 1", got)
	}
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d status = %d, want 200 (queued then served)", i, c)
		}
	}
}

// TestRotator_SaturationRejectReturns429: all slots held, saturation=reject ->
// 429, and the upstream is never hit for the rejected request.
func TestRotator_SaturationRejectReturns429(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // hold the slot until we let it go
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "reject", 0, 0)

	// First request occupies the only slot in the background.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); post(t, r, "/v2/scrape", `{}`) }()
	time.Sleep(50 * time.Millisecond) // let it acquire the slot and reach the fake

	// Second request: saturated, reject -> 429, no new upstream hit.
	before := hits.Load()
	rec := post(t, r, "/v2/scrape", `{}`)
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429 (reject when saturated)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "busy") {
		t.Fatalf("body = %q, want a busy message", rec.Body.String())
	}
	close(release)
	wg.Wait()
	if got := hits.Load(); got != before+0 && got != 1 {
		// the rejected request must not have reached upstream
		t.Fatalf("upstream hits = %d, want the rejected request to add 0 hits", got)
	}
}

// TestRotator_SaturationQueueWaitsThenServes: all slots held, saturation=queue
// with a generous timeout -> the queued request waits and is eventually served.
func TestRotator_SaturationQueueWaitsThenServes(t *testing.T) {
	release := make(chan struct{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "queue", 2*time.Second, 0)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); post(t, r, "/v2/scrape", `{}`) }()
	time.Sleep(50 * time.Millisecond)

	done := make(chan int, 1)
	go func() { done <- post(t, r, "/v2/scrape", `{}`).Code }()
	// Free the slot shortly after; the queued request should be served.
	go func() { time.Sleep(60 * time.Millisecond); close(release) }()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("queued request status = %d, want 200 (served after wait)", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request did not complete in time")
	}
	wg.Wait()
}

// TestRotator_SaturationQueueTimeoutReturns503: all slots held past the queue
// timeout -> 503.
func TestRotator_SaturationQueueTimeoutReturns503(t *testing.T) {
	release := make(chan struct{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "queue", 30*time.Millisecond, 0)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); post(t, r, "/v2/scrape", `{}`) }()
	time.Sleep(50 * time.Millisecond)

	rec := post(t, r, "/v2/scrape", `{}`) // queued, times out at 30ms
	close(release)
	wg.Wait()
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (queue timeout)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "busy") {
		t.Fatalf("body = %q, want a busy/queue-timeout message", rec.Body.String())
	}
}

// TestRotator_429ConcurrencyRotatesToFreeSlot: key0 429s "concurrency limit",
// key1 succeeds -> key0 hit once (no same-key retry), 200 via key1, key0 not disabled.
func TestRotator_429ConcurrencyRotatesToFreeSlot(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer fc-a":
			hitsA.Add(1)
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"success":false,"error":"Concurrent browser limit reached"}`))
		default:
			hitsB.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
		}
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a", "fc-b"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	pool.SetCredits(1, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "queue", time.Second, 0)

	rec := post(t, r, "/v2/scrape", `{"url":"https://x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after rotation (body %q)", rec.Code, rec.Body.String())
	}
	if hitsA.Load() != 1 {
		t.Fatalf("key0 hits = %d, want 1 (429 rotates immediately, no same-key retry)", hitsA.Load())
	}
	if pool.Snapshot().Keys[0].Disabled {
		t.Fatal("429 must NOT disable the key")
	}
}

// TestRotator_403CappedRetries: with Max403Retries=1, key0 gets at most 2 hits
// (1 + 1 retry) before rotating to key1 which succeeds.
func TestRotator_403CappedRetries(t *testing.T) {
	orig := backoffSchedule
	backoffSchedule = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { backoffSchedule = orig }()

	var hitsA, hitsB atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer fc-a" {
			hitsA.Add(1)
			w.WriteHeader(403)
			return
		}
		hitsB.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a", "fc-b"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	pool.SetCredits(1, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "queue", time.Second, 1)

	rec := post(t, r, "/v2/scrape", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 via key1 (body %q)", rec.Code, rec.Body.String())
	}
	if got := hitsA.Load(); got != 2 {
		t.Fatalf("key0 hits = %d, want 2 (1 + 1 capped retry), not the legacy 6", got)
	}
	if pool.Snapshot().Keys[0].Disabled {
		t.Fatal("403 must NOT disable the key")
	}
}

// TestRotator_403SingleRetryRecoversOnSameKey: one key, 403 then 200, with
// Max403Retries=1 -> recovers on the same key (preserves transient-recovery).
func TestRotator_403SingleRetryRecoversOnSameKey(t *testing.T) {
	orig := backoffSchedule
	backoffSchedule = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { backoffSchedule = orig }()

	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "queue", time.Second, 1)

	rec := post(t, r, "/v2/scrape", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (single retry recovers on same key)", rec.Code)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (403 then success)", calls.Load())
	}
}

// TestRotator_ConcurrencySlotReleasedOnSuccess: after a successful request the
// slot is released, so a subsequent request on the same key is served.
func TestRotator_ConcurrencySlotReleasedOnSuccess(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetCredits(0, 100)
	r := concTestRotator(cfg, pool, http.DefaultClient, 1, "reject", 0, 0)

	for i := 0; i < 3; i++ {
		rec := post(t, r, "/v2/scrape", `{}`)
		if rec.Code != 200 {
			t.Fatalf("request %d status = %d, want 200 (slot released each time)", i, rec.Code)
		}
	}
	if got := pool.inFlight[0]; got != 0 {
		t.Fatalf("inFlight[0] = %d after requests, want 0 (no slot leak)", got)
	}
}
