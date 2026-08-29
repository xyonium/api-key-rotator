package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// exaTestRotator builds a rotator with firecrawl (default, fcFake) and exa
// (/exa, exaFake) profiles, mirroring tavilyTestRotator/apifyTestRotator. The
// exa profile gets a refresher so the (no-op) balance path is exercised too.
func exaTestRotator(cfg Config, fcPool, exPool *KeyPool, client *http.Client, fcUpstream, exaUpstream string) *rotator {
	fcPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	// Exa tracks microUSD; pull its thresholds from cfg.Exa.
	exLow, exStop := cfg.Exa.LowMicro, cfg.Exa.StopMicro
	if exLow == 0 && exStop == 0 {
		exLow, exStop = 500_000, 100_000
	}
	exPool.SetThresholds(exLow, exStop)
	host := func(u string) string {
		if i := strings.Index(u, "://"); i >= 0 {
			return u[i+3:]
		}
		return u
	}
	fc := &Profile{Name: "firecrawl", Upstream: fcUpstream, UpstreamHost: host(fcUpstream), CreditResetDay: cfg.CreditResetDay, RewriteNext: true, pool: fcPool}
	ex := &Profile{
		Name:           "exa",
		RoutePrefix:    "/exa",
		Upstream:       exaUpstream,
		UpstreamHost:   host(exaUpstream),
		CreditResetDay: cfg.CreditResetDay,
		RewriteNext:    false,
		AuthHeader:     "x-api-key",
		pool:           exPool,
	}
	r := newRotator(cfg, []*Profile{fc, ex}, client, newLogger("info"))
	ex.refresh = NewRefresher(ex, client, cfg, newLogger("info"))
	return r
}

// exaFakeBackend serves POST /search with a real-shaped Exa response. Any other
// path is counted as a "usage/balance probe" and 404s, so tests can assert the
// rotator never asks Exa for a balance it cannot provide.
func exaFakeBackend(t *testing.T, handler func(w http.ResponseWriter, r *http.Request), probe *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			if probe != nil {
				atomic.AddInt32(probe, 1)
			}
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Not found","tag":"NOT_FOUND"}`))
			return
		}
		handler(w, r)
	}))
}

// exaOK writes a 200 search response. costDollars is an OBJECT in the real API;
// the naive scalar form is accepted defensively too.
func exaOK(costTotal string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cost := `"costDollars":{"total":` + costTotal + `,"search":{"neural":` + costTotal + `}}`
		_, _ = w.Write([]byte(`{"requestId":"abc","results":[{"id":"https://x","title":"t","url":"https://x"}],` + cost + `}`))
	}
}

// TestRotator_ExaPrefixStrippedAndAuthHeader proves both halves of the Exa
// integration: the /exa prefix is stripped before forwarding, and the key
// travels in the x-api-key header (NOT Authorization: Bearer).
func TestRotator_ExaPrefixStrippedAndAuthHeader(t *testing.T) {
	var gotPath, gotKey, gotBearer string
	fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotBearer = r.Header.Get("Authorization")
		exaOK("0.007")(w, r)
	}, nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1"})
	exPool.SetCredits(0, 10_000_000)
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x","numResults":1}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if gotPath != "/search" {
		t.Fatalf("upstream path = %q, want /search (prefix stripped)", gotPath)
	}
	if gotKey != "exa-aaa1" {
		t.Fatalf("x-api-key = %q, want pooled key", gotKey)
	}
	if gotBearer != "" {
		t.Fatalf("Authorization = %q, want empty (exa uses x-api-key)", gotBearer)
	}
}

// TestRotator_ExaSuccessDecrementsCostMicroUSD is the regression test for the
// unit trap. Exa bills $0.007 per search; int(0.007*100) == 0, so a
// CENTS-denominated balance would never decrement and the profile would never
// auto-stop. Tracking in microUSD makes it exact: 10_000_000 -> 9_993_000.
func TestRotator_ExaSuccessDecrementsCostMicroUSD(t *testing.T) {
	fake := exaFakeBackend(t, exaOK("0.007"), nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1"})
	exPool.SetCredits(0, 10_000_000) // $10.00
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := exPool.Snapshot().Keys[0].RemainingCredits
	if got != 9_993_000 {
		t.Fatalf("remaining = %d microUSD, want 9_993_000 (10_000_000 - 7000). "+
			"Got 10_000_000? That is the cents-truncation bug: int(0.007*100)==0.", got)
	}
}

// TestRotator_ExaNoBalanceProbesEver guards the second trap: fetchUsageFor
// falls through to Firecrawl's /v2/team/credit-usage for any unrecognized
// profile name. Against api.exa.ai that 404s forever while warmupLoop retries
// every minute waiting for "unmeasured == 0", which never arrives.
func TestRotator_ExaNoBalanceProbesEver(t *testing.T) {
	var probes int32
	fake := exaFakeBackend(t, exaOK("0.007"), &probes)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1", "exa-bbb2"})
	exPool.SetCredits(0, 10_000_000)
	exPool.SetCredits(1, 10_000_000)
	// Serve several requests and force a rotation (402 on key 0), each of
	// which historically triggered a balance refresh.
	var hits atomic.Int32
	hot := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 && r.Header.Get("x-api-key") == "exa-aaa1" {
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"error":"No credits","tag":"NO_MORE_CREDITS"}`))
			return
		}
		exaOK("0.007")(w, r)
	}, &probes)
	defer hot.Close()

	r2 := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", hot.URL)
	exPool.SetCredits(0, 10_000_000)
	exPool.SetCredits(1, 10_000_000)
	post(t, r2, "/exa/search", `{"query":"x"}`) // rotates off key 0 (402)

	// The refresher runs in a goroutine; give it a moment to (not) fire.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&probes); got != 0 {
		t.Fatalf("balance probes = %d, want 0 (exa has no balance endpoint; probed paths 404)",
			got)
	}
}

// TestRotator_Exa402RotatesAndDisables: 402 (NO_MORE_CREDITS) rotates to the
// next key AND disables the exhausted one until the CREDIT_RESET_DAY fallback.
func TestRotator_Exa402RotatesAndDisables(t *testing.T) {
	var callsA, callsB atomic.Int32
	fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "exa-aaa1" {
			callsA.Add(1)
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"error":"No more credits","tag":"NO_MORE_CREDITS"}`))
			return
		}
		callsB.Add(1)
		exaOK("0.007")(w, r)
	}, nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.CreditResetDay = 15 // distinguish "fallback" from "today"
	exPool := NewKeyPool([]string{"exa-aaa1", "exa-bbb2"})
	exPool.SetCredits(0, 10_000_000)
	exPool.SetCredits(1, 10_000_000)
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after rotation (body %q)", rec.Code, rec.Body.String())
	}
	if callsA.Load() != 1 || callsB.Load() != 1 {
		t.Fatalf("calls a/b = %d/%d, want 1/1", callsA.Load(), callsB.Load())
	}
	k0 := exPool.Snapshot().Keys[0]
	if !k0.Disabled {
		t.Fatal("key 0 (402) should be disabled")
	}
	if k0.DisabledUntil.Day() != 15 {
		t.Fatalf("DisabledUntil = %v, want day 15 (CREDIT_RESET_DAY fallback)", k0.DisabledUntil)
	}
	if exPool.Snapshot().Keys[1].Disabled {
		t.Fatal("key 1 (success) should not be disabled")
	}
}

// TestRotator_Exa429RotatesButKeepsKey: a rate limit rotates but must NOT
// disable - the key is fine, Exa just throttled it.
func TestRotator_Exa429RotatesButKeepsKey(t *testing.T) {
	fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "exa-aaa1" {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"You've exceeded your Exa rate limit"}`))
			return
		}
		exaOK("0.007")(w, r)
	}, nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1", "exa-bbb2"})
	exPool.SetCredits(0, 10_000_000)
	exPool.SetCredits(1, 10_000_000)
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after rotation (body %q)", rec.Code, rec.Body.String())
	}
	if exPool.Snapshot().Keys[0].Disabled {
		t.Fatal("429 must NOT disable the key")
	}
}

// TestRotator_Exa401RotatesButKeepsKey: an invalid key rotates but must not be
// disabled forever - it may be re-enabled or replaced by a working key.
func TestRotator_Exa401RotatesButKeepsKey(t *testing.T) {
	fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "exa-aaa1" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"Invalid API key","tag":"INVALID_API_KEY"}`))
			return
		}
		exaOK("0.007")(w, r)
	}, nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1", "exa-bbb2"})
	exPool.SetCredits(0, 10_000_000)
	exPool.SetCredits(1, 10_000_000)
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after rotation (body %q)", rec.Code, rec.Body.String())
	}
	if exPool.Snapshot().Keys[0].Disabled {
		t.Fatal("401 must NOT disable the key")
	}
}

// TestRotator_ExaAllExhausted503: when every key is exhausted the profile
// returns the standard credit-exhausted 503 rather than an opaque error.
func TestRotator_ExaAllExhausted503(t *testing.T) {
	fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":"No more credits","tag":"NO_MORE_CREDITS"}`))
	}, nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1"})
	exPool.SetCredits(0, 10_000_000)
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x"}`)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "credit-exhausted") {
		t.Fatalf("body = %q, want credit-exhausted message", rec.Body.String())
	}
}

// TestRotator_ExaBodyNeverScanned: Exa rotation is status-code only. Search
// results contain arbitrary scraped page text, which can easily include
// "insufficient credits" or "rate limit" - that must never trigger a rotation.
func TestRotator_ExaBodyNeverScanned(t *testing.T) {
	var calls atomic.Int32
	fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"requestId":"a","results":[{"title":"insufficient credits","text":"rate limit exceeded and payment required"}],"costDollars":{"total":0.007}}`))
	}, nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1", "exa-bbb2"})
	exPool.SetCredits(0, 10_000_000)
	exPool.SetCredits(1, 10_000_000)
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (denylist words in content must not rotate)", got)
	}
}

// TestRotator_ExaCostDollarsScalarDoesNotCrash: the real API returns
// costDollars as an object. A scalar or missing field must degrade gracefully
// (decrement 0) rather than panic or silently zero the balance.
func TestRotator_ExaCostDollarsScalarDoesNotCrash(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"object (real shape)", `{"costDollars":{"total":0.007}}`, 9_993_000},
		{"scalar (defensive)", `{"costDollars":0.007}`, 10_000_000},
		{"missing", `{"results":[]}`, 10_000_000},
		{"null total", `{"costDollars":{"total":null}}`, 10_000_000},
		{"not JSON", `not json at all`, 10_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}, nil)
			defer fake.Close()

			cfg := cfgFor(fake)
			exPool := NewKeyPool([]string{"exa-aaa1"})
			exPool.SetCredits(0, 10_000_000)
			r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

			rec := post(t, r, "/exa/search", `{"query":"x"}`)
			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := exPool.Snapshot().Keys[0].RemainingCredits; got != tc.want {
				t.Fatalf("remaining = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBuildProfiles_ExaUsesApiKeyHeader is an app-level test: it goes through
// LoadConfig + buildProfiles instead of hand-building the Profile.
//
// This matters because the hand-built helpers elsewhere in this file bypass
// buildProfiles entirely, so a missing AuthHeader: "x-api-key" there would go
// completely unnoticed - the exact shape of the Tavily 403 defect (2026-08-28),
// where buildProfiles never set Max403Retries and every unit test still passed.
func TestBuildProfiles_ExaUsesApiKeyHeader(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("EXA_API_KEYS", "exa-aaa1,exa-bbb2")
	t.Setenv("UPSTREAM", "http://firecrawl.invalid")
	t.Setenv("TAVILY_API_KEYS", "")
	t.Setenv("APIFY_API_KEYS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	profiles := buildProfiles(cfg)
	var ex *Profile
	for _, p := range profiles {
		if p.Name == "exa" {
			ex = p
		}
	}
	if ex == nil {
		t.Fatal("exa profile not built")
	}
	if ex.AuthHeader != "x-api-key" {
		t.Fatalf("exa AuthHeader = %q, want %q (empty means it silently falls back to Bearer)", ex.AuthHeader, "x-api-key")
	}
	if ex.RoutePrefix != "/exa" || ex.RewriteNext {
		t.Fatalf("exa profile = %+v, want prefix /exa and RewriteNext false", ex)
	}
	if ex.UpstreamHost != "api.exa.ai" {
		t.Fatalf("exa UpstreamHost = %q, want api.exa.ai", ex.UpstreamHost)
	}
	// Balances are seeded at budget, so EnsureMeasured never fetches.
	for i, k := range ex.pool.Snapshot().Keys {
		if k.RemainingCredits != cfg.Exa.BudgetMicro {
			t.Fatalf("key %d remaining = %d, want %d (seeded full budget)", i, k.RemainingCredits, cfg.Exa.BudgetMicro)
		}
	}
	// Thresholds are microUSD.
	ex.pool.mu.Lock()
	low, stop := ex.pool.lowThreshold, ex.pool.stopThreshold
	ex.pool.mu.Unlock()
	if low != cfg.Exa.LowMicro || stop != cfg.Exa.StopMicro {
		t.Fatalf("exa thresholds = %d/%d, want %d/%d microUSD", low, stop, cfg.Exa.LowMicro, cfg.Exa.StopMicro)
	}
}

// TestRotator_UnprefixedStillFirecrawlWithExaConfigured proves adding the exa
// profile does not steal traffic from the default (firecrawl) profile.
func TestRotator_UnprefixedStillFirecrawlWithExaConfigured(t *testing.T) {
	fcFake := newFakeBackend(t, "fc-a", "fc-b")
	defer fcFake.Close()
	exaFake := exaFakeBackend(t, exaOK("0.007"), nil)
	defer exaFake.Close()

	cfg := cfgFor(fcFake)
	fcPool := NewKeyPool(cfg.APIKeys)
	withCredits(fcPool)
	r := exaTestRotator(cfg, fcPool, NewKeyPool([]string{"exa-aaa1"}), http.DefaultClient, fcFake.URL, exaFake.URL)

	rec := post(t, r, "/v2/scrape", `{"url":"https://x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 from firecrawl profile", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("body = %q, want firecrawl success envelope", rec.Body.String())
	}
}

// TestRotator_ExaBudgetStopsServing: once accounted spend pushes every key
// below the stop threshold, the profile 503s instead of burning real credits.
func TestRotator_ExaBudgetStopsServing(t *testing.T) {
	var calls atomic.Int32
	fake := exaFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		exaOK("0.007")(w, r)
	}, nil)
	defer fake.Close()

	cfg := cfgFor(fake)
	exPool := NewKeyPool([]string{"exa-aaa1"})
	exPool.SetThresholds(500_000, 100_000)
	// 50_000 microUSD = $0.05, below the $0.10 stop threshold.
	exPool.SetCredits(0, 50_000)
	r := exaTestRotator(cfg, NewKeyPool(cfg.APIKeys), exPool, http.DefaultClient, "http://fc.invalid", fake.URL)

	rec := post(t, r, "/exa/search", `{"query":"x"}`)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (budget below stop threshold)", rec.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 (must not spend real credits)", got)
	}
}
