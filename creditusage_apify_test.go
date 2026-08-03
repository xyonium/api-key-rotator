package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// apifyLimitsBody builds a /v2/users/me/limits response body.
func apifyLimitsBody(usageUsd float64, endAt string) string {
	return `{"data":{"monthlyUsageCycle":{"startAt":"2026-08-01T00:00:00.000Z","endAt":"` + endAt + `"},` +
		`"limits":{"maxMonthlyUsageUsd":0},` +
		`"current":{"monthlyUsageUsd":` + jsonFloat(usageUsd) + `}}}`
}

func jsonFloat(f float64) string {
	// Render without trailing zeros loss for the values we use in tests.
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// TestFetchApifyUsage verifies remaining = (freeCredit - monthlyUsageUsd) in
// CENTS, and periodEnd = monthlyUsageCycle.endAt.
func TestFetchApifyUsage(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/users/me/limits" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("token") != "apify-a" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(apifyLimitsBody(4.93, "2026-09-01T23:59:59.999Z")))
	}))
	defer fake.Close()

	p := &Profile{Name: "apify", Upstream: fake.URL, ApifyFreeCreditUsd: 5}
	u := fetchApifyUsage(fake.Client(), p, "apify-a", nil)
	if !u.ok {
		t.Fatal("fetchApifyUsage failed")
	}
	// 5.00 - 4.93 = 0.07 USD = 7 cents
	if u.remaining != 7 {
		t.Fatalf("remaining = %d, want 7 cents (5.00 - 4.93)", u.remaining)
	}
	wantEnd, _ := time.Parse(time.RFC3339, "2026-09-01T23:59:59.999Z")
	if !u.periodEnd.Equal(wantEnd) {
		t.Fatalf("periodEnd = %v, want %v (monthlyUsageCycle.endAt)", u.periodEnd, wantEnd)
	}
}

// TestFetchApifyUsage_Exhausted: usage at/over the free credit -> 0 cents.
func TestFetchApifyUsage_Exhausted(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(apifyLimitsBody(5.02, "2026-09-01T23:59:59.999Z")))
	}))
	defer fake.Close()

	p := &Profile{Name: "apify", Upstream: fake.URL, ApifyFreeCreditUsd: 5}
	u := fetchApifyUsage(fake.Client(), p, "apify-a", nil)
	if !u.ok {
		t.Fatal("fetchApifyUsage failed")
	}
	if u.remaining != 0 {
		t.Fatalf("remaining = %d, want 0 (over free credit clamps to 0)", u.remaining)
	}
}

// TestFetchApifyUsage_Unauthorized: a 401 is permanent, not retried.
func TestFetchApifyUsage_Unauthorized(t *testing.T) {
	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(401)
	}))
	defer fake.Close()

	p := &Profile{Name: "apify", Upstream: fake.URL, ApifyFreeCreditUsd: 5}
	if u := fetchApifyUsage(fake.Client(), p, "bad", nil); u.ok {
		t.Fatal("expected !ok for 401")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (401 must not be retried)", hits)
	}
}

// TestFetchApifyUsage_RetriesOn5xx: transient 503 retried until 200.
func TestFetchApifyUsage_RetriesOn5xx(t *testing.T) {
	orig := usageBackoff
	usageBackoff = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { usageBackoff = orig }()

	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(apifyLimitsBody(0.10, "2026-09-01T23:59:59.999Z")))
	}))
	defer fake.Close()

	p := &Profile{Name: "apify", Upstream: fake.URL, ApifyFreeCreditUsd: 5}
	u := fetchApifyUsage(fake.Client(), p, "apify-a", nil)
	if !u.ok {
		t.Fatal("expected ok after retry")
	}
	// 5.00 - 0.10 = 4.90 USD = 490 cents
	if u.remaining != 490 {
		t.Fatalf("remaining = %d, want 490 cents", u.remaining)
	}
	if hits < 3 {
		t.Fatalf("hits = %d, want >=3 (should have retried 503)", hits)
	}
}

// TestFetchUsageFor_Apify: fetchUsageFor must dispatch apify to the limits
// endpoint (not return a zero usage).
func TestFetchUsageFor_Apify(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(apifyLimitsBody(1.00, "2026-09-01T23:59:59.999Z")))
	}))
	defer fake.Close()

	p := &Profile{Name: "apify", Upstream: fake.URL, ApifyFreeCreditUsd: 5}
	u := fetchUsageFor(p, fake.Client(), "apify-a", nil)
	if !u.ok {
		t.Fatal("fetchUsageFor(apify) failed")
	}
	if u.remaining != 400 {
		t.Fatalf("remaining = %d, want 400 cents (5.00 - 1.00)", u.remaining)
	}
}

// TestRotator_ApifyLowCreditStops: when the fetched balance drops below the
// stop threshold, the pool refuses to serve (Current < 0) so requests 503.
func TestRotator_ApifyLowCreditStops(t *testing.T) {
	// Limits endpoint reports only 3 cents left (below the 5-cent stop).
	apifyFake := httptest.NewServer(apifyLimitsHandler("4.97", "2026-09-01T23:59:59.999Z", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"ok":true}]`))
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), NewKeyPool([]string{"apify-a"}), http.DefaultClient, fcFake.URL, apifyFake.URL)

	// First request: EnsureMeasured pulls the 3-cent balance, then the pool
	// refuses (3 < 5 stop), so 503 and the actor endpoint is never hit.
	rec := post(t, r, "/v2/acts/u~a/run-sync-get-dataset-items", `{}`)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (balance below stop threshold)", rec.Code)
	}
}
