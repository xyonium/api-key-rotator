package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// apifyTestRotator builds a rotator with firecrawl (default, fcFake) and apify
// (/v2/acts, apifyFake) profiles, mirroring tavilyTestRotator. The apify
// profile gets a refresher so EnsureMeasured can fetch real balances; its
// limits responses come from the fake's /v2/users/me/limits branch.
func apifyTestRotator(cfg Config, fcPool, apPool *KeyPool, client *http.Client, fcUpstream, apifyUpstream string) *rotator {
	fcPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	// Apify tracks cents; pull its thresholds from cfg.Apify (default 10/5).
	apLow, apStop := cfg.Apify.LowCreditCents, cfg.Apify.StopCreditCents
	if apLow == 0 && apStop == 0 {
		apLow, apStop = 10, 5
	}
	apPool.SetThresholds(apLow, apStop)
	host := func(u string) string {
		if i := strings.Index(u, "://"); i >= 0 {
			return u[i+3:]
		}
		return u
	}
	fc := &Profile{Name: "firecrawl", Upstream: fcUpstream, UpstreamHost: host(fcUpstream), CreditResetDay: cfg.CreditResetDay, RewriteNext: true, pool: fcPool}
	freeUsd := cfg.Apify.FreeCreditUsd
	if freeUsd == 0 {
		freeUsd = 5
	}
	ap := &Profile{Name: "apify", RoutePrefix: "/v2/acts", Upstream: apifyUpstream, UpstreamHost: host(apifyUpstream), CreditResetDay: cfg.CreditResetDay, KeepPrefix: true, AuthQueryParam: "token", ApifyFreeCreditUsd: freeUsd, pool: apPool}
	r := newRotator(cfg, []*Profile{fc, ap}, client, newLogger("info"))
	ap.refresh = NewRefresher(ap, client, cfg, newLogger("info"))
	return r
}

// apifyLimitsHandler routes /v2/users/me/limits to a 200 with the given usage
// USD; everything else to the actor handler. Shared by tests that need a real
// balance refresh to succeed.
func apifyLimitsHandler(usageUsd, endAt string, actor http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/users/me/limits" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"monthlyUsageCycle":{"startAt":"2026-08-01T00:00:00.000Z","endAt":"` + endAt + `"},"limits":{"maxMonthlyUsageUsd":0},"current":{"monthlyUsageUsd":` + usageUsd + `}}}`))
			return
		}
		actor(w, r)
	}
}

func TestRotator_ApifyTokenQueryParamReplaced(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotBody string
	apifyFake := httptest.NewServer(apifyLimitsHandler("1.00", "2026-09-01T23:59:59.999Z", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"x":1}]`))
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	apPool := NewKeyPool([]string{"apify-good"})
	withCredits(apPool)
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), apPool, http.DefaultClient, fcFake.URL, apifyFake.URL)

	rec := post(t, r, "/v2/acts/apimaestro~scraper/run-sync-get-dataset-items?token=stale-client-token&timeout=120", `{"q":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if gotPath != "/v2/acts/apimaestro~scraper/run-sync-get-dataset-items" {
		t.Fatalf("upstream path = %q, want full /v2/acts/... path preserved", gotPath)
	}
	q := gotQuery
	if !strings.Contains(q, "token=apify-good") {
		t.Fatalf("upstream query = %q, want token replaced with pooled key", q)
	}
	if strings.Contains(q, "stale-client-token") {
		t.Fatalf("upstream query = %q, client token must be replaced, not kept", q)
	}
	if !strings.Contains(q, "timeout=120") {
		t.Fatalf("upstream query = %q, want timeout=120 preserved", q)
	}
	if gotAuth != "" {
		t.Fatalf("upstream Authorization = %q, want empty (apify authenticates via query param)", gotAuth)
	}
	if gotBody != `{"q":"x"}` {
		t.Fatalf("upstream body = %q, want actor input forwarded verbatim", gotBody)
	}
}

func TestRotator_ApifyTokenAddedWhenMissing(t *testing.T) {
	var gotQuery string
	apifyFake := httptest.NewServer(apifyLimitsHandler("1.00", "2026-09-01T23:59:59.999Z", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	apPool := NewKeyPool([]string{"apify-good"})
	withCredits(apPool)
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), apPool, http.DefaultClient, fcFake.URL, apifyFake.URL)

	rec := post(t, r, "/v2/acts/u~a/run-sync-get-dataset-items?timeout=60", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotQuery, "token=apify-good") || !strings.Contains(gotQuery, "timeout=60") {
		t.Fatalf("upstream query = %q, want token=apify-good added, timeout preserved", gotQuery)
	}
}

func TestRotator_ApifyRotatesOn402AndDisables(t *testing.T) {
	var callsA, callsB atomic.Int32
	apifyFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/users/me/limits" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"monthlyUsageCycle":{"startAt":"2026-08-01T00:00:00.000Z","endAt":"2026-09-01T23:59:59.999Z"},"limits":{"maxMonthlyUsageUsd":0},"current":{"monthlyUsageUsd":5.0}}}`))
			return
		}
		switch r.URL.Query().Get("token") {
		case "apify-a":
			callsA.Add(1)
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"error":{"type":"insufficient-credits"}}`))
		default:
			callsB.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"ok":true}]`))
		}
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	apPool := NewKeyPool([]string{"apify-a", "apify-b"})
	withCredits(apPool)
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), apPool, http.DefaultClient, fcFake.URL, apifyFake.URL)

	rec := post(t, r, "/v2/acts/u~a/run-sync-get-dataset-items?token=whatever", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after rotation (body %q)", rec.Code, rec.Body.String())
	}
	if callsA.Load() != 1 || callsB.Load() != 1 {
		t.Fatalf("calls a/b = %d/%d, want 1/1", callsA.Load(), callsB.Load())
	}
	snap := apPool.Snapshot()
	if !snap.Keys[0].Disabled {
		t.Fatal("key 0 (402) should be disabled")
	}
	if snap.Keys[1].Disabled {
		t.Fatal("key 1 (success) should not be disabled")
	}
}

func TestRotator_Apify429RotatesButKeepsKey(t *testing.T) {
	apifyFake := httptest.NewServer(apifyLimitsHandler("1.00", "2026-09-01T23:59:59.999Z", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "apify-a" {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	apPool := NewKeyPool([]string{"apify-a", "apify-b"})
	withCredits(apPool)
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), apPool, http.DefaultClient, fcFake.URL, apifyFake.URL)

	rec := post(t, r, "/v2/acts/u~a/run-sync-get-dataset-items", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if apPool.Snapshot().Keys[0].Disabled {
		t.Fatal("429 must NOT disable the key")
	}
}

func TestRotator_ApifySuccessArrayDoesNotRotate(t *testing.T) {
	// Apify success is a bare JSON ARRAY of dataset items, often containing
	// scraped text with denylist words. Must not rotate, and the body must be
	// forwarded untouched. The pool starts UNMEASURED and the balance refresh
	// reports 400 cents; success then decrements by exactly 1 credit.
	var calls atomic.Int32
	apifyFake := httptest.NewServer(apifyLimitsHandler("1.00", "2026-09-01T23:59:59.999Z", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"text":"rate limit exceeded in the news"}]`))
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	apPool := NewKeyPool([]string{"apify-a", "apify-b"}) // unmeasured
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), apPool, http.DefaultClient, fcFake.URL, apifyFake.URL)

	rec := post(t, r, "/v2/acts/u~a/run-sync-get-dataset-items", `{}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no rotation on 200 array body)", calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded") {
		t.Fatalf("body = %q, want array forwarded verbatim", rec.Body.String())
	}
	// 5.00 free - 1.00 used = 400 cents refreshed; minus 1 credit on success.
	if got := apPool.Snapshot().Keys[0].RemainingCredits; got != 399 {
		t.Fatalf("remaining = %d, want 399 (400 refreshed - 1 success)", got)
	}
}

func TestRotator_ApifyDisableUsesFallbackReset(t *testing.T) {
	// The limits endpoint 404s, so the disable must fall back to CREDIT_RESET_DAY.
	apifyFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/users/me/limits" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(402)
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	cfg.CreditResetDay = 15
	apPool := NewKeyPool([]string{"apify-a"})
	apPool.SetCredits(0, 5)
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), apPool, http.DefaultClient, fcFake.URL, apifyFake.URL)

	post(t, r, "/v2/acts/u~a/run-sync-get-dataset-items", `{}`)
	k := apPool.Snapshot().Keys[0]
	if !k.Disabled {
		t.Fatal("key should be disabled after 402")
	}
	// Limits endpoint unreachable: reset is the CREDIT_RESET_DAY fallback.
	if k.DisabledUntil.Day() != 15 {
		t.Fatalf("DisabledUntil = %v, want day 15 (CREDIT_RESET_DAY fallback)", k.DisabledUntil)
	}
	if k.DisabledUntil.Before(time.Now()) {
		t.Fatalf("DisabledUntil = %v is in the past", k.DisabledUntil)
	}
}

func TestRotator_ApifyDisableQueriesLimitsEndpoint(t *testing.T) {
	// Disabling a key must query the apify limits endpoint for the real reset
	// instant (unlike before, when apify had no usage call at all).
	var limitsCalls atomic.Int32
	apifyFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/users/me/limits" {
			limitsCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"monthlyUsageCycle":{"startAt":"2026-08-01T00:00:00.000Z","endAt":"2026-09-01T23:59:59.999Z"},"limits":{"maxMonthlyUsageUsd":0},"current":{"monthlyUsageUsd":5.0}}}`))
			return
		}
		w.WriteHeader(402)
	}))
	defer apifyFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	apPool := NewKeyPool([]string{"apify-a"})
	apPool.SetCredits(0, 5)
	r := apifyTestRotator(cfg, NewKeyPool(cfg.APIKeys), apPool, http.DefaultClient, fcFake.URL, apifyFake.URL)

	post(t, r, "/v2/acts/u~a/run-sync-get-dataset-items", `{}`)
	if limitsCalls.Load() != 1 {
		t.Fatalf("limits endpoint calls = %d, want 1 (disable should fetch real reset)", limitsCalls.Load())
	}
	if !apPool.Snapshot().Keys[0].Disabled {
		t.Fatal("key should be disabled after 402")
	}
}
