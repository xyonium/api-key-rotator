package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tavilyTestRotator builds a rotator with BOTH profiles: firecrawl (no
// prefix, upstream fcFake) and tavily (/tavily, upstream tvFake).
func tavilyTestRotator(cfg Config, fcPool, tvPool *KeyPool, client *http.Client, fcUpstream, tvUpstream string) *rotator {
	fcPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	tvPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	host := func(u string) string {
		if i := strings.Index(u, "://"); i >= 0 {
			return u[i+3:]
		}
		return u
	}
	fc := &Profile{Name: "firecrawl", Upstream: fcUpstream, UpstreamHost: host(fcUpstream), CreditResetDay: cfg.CreditResetDay, RewriteNext: true, pool: fcPool}
	tv := &Profile{Name: "tavily", RoutePrefix: "/tavily", Upstream: tvUpstream, UpstreamHost: host(tvUpstream), CreditResetDay: cfg.CreditResetDay, pool: tvPool}
	return newRotator(cfg, []*Profile{fc, tv}, client, newLogger("info"))
}

func post(t *testing.T, r *rotator, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "rotator.test"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRotator_TavilyPrefixStripped(t *testing.T) {
	var gotPath, gotAuth string
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if gotPath != "/search" {
		t.Fatalf("upstream path = %q, want /search (prefix stripped)", gotPath)
	}
	if gotAuth != "Bearer tvly-a" {
		t.Fatalf("upstream auth = %q, want pooled key", gotAuth)
	}
}

func TestRotator_TavilyRotatesOn432AndDisables(t *testing.T) {
	var callsA, callsB atomic.Int32
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer tvly-a":
			callsA.Add(1)
			w.WriteHeader(432)
			_, _ = w.Write([]byte(`{"detail":{"error":"plan usage limit exceeded"}}`))
		default:
			callsB.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[{"title":"t"}]}`))
		}
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after rotation (body %q)", rec.Code, rec.Body.String())
	}
	if callsA.Load() != 1 || callsB.Load() != 1 {
		t.Fatalf("calls a/b = %d/%d, want 1/1", callsA.Load(), callsB.Load())
	}
	snap := tvPool.Snapshot()
	if !snap.Keys[0].Disabled {
		t.Fatal("key 0 (432) should be disabled")
	}
	if snap.Keys[1].Disabled {
		t.Fatal("key 1 (success) should not be disabled")
	}
}

func TestRotator_TavilyRotatesOn433(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tvly-a" {
			w.WriteHeader(433)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/extract", `{"urls":["https://x"]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !tvPool.Snapshot().Keys[0].Disabled {
		t.Fatal("key 0 (433) should be disabled")
	}
}

func TestRotator_Tavily429RotatesButKeepsKey(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tvly-a" {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if tvPool.Snapshot().Keys[0].Disabled {
		t.Fatal("429 must NOT disable the key")
	}
}

func TestRotator_TavilySuccessDoesNotRotateAndDecrementsOne(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Even denylist-flavored content in a 200 must not rotate.
		_, _ = w.Write([]byte(`{"results":[{"title":"rate limit exceeded news"}]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	tvPool.SetCredits(0, 50)
	tvPool.SetCredits(1, 50)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if got := tvPool.Snapshot().Keys[0].RemainingCredits; got != 49 {
		t.Fatalf("remaining = %d, want 49 (decrement by 1)", got)
	}
}

func TestRotator_TavilyNoDenylistRotationOn200FailureShapedBody(t *testing.T) {
	// Tavily errors are always non-2xx; a 200 with an error-looking body is
	// content, not a rejection. Must NOT rotate.
	var calls atomic.Int32
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"detail":{"error":"odd but 200"}}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no rotation on 200)", calls.Load())
	}
}

func TestRotator_UnprefixedStillFirecrawl(t *testing.T) {
	fcFake := newFakeBackend(t, "fc-a", "fc-b")
	defer fcFake.Close()
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer tvFake.Close()

	cfg := cfgFor(fcFake)
	fcPool := NewKeyPool(cfg.APIKeys)
	withCredits(fcPool)
	r := tavilyTestRotator(cfg, fcPool, NewKeyPool([]string{"tvly-a"}), http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/v2/scrape", `{"url":"https://x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 from firecrawl profile", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("body = %q, want firecrawl success envelope", rec.Body.String())
	}
}

func TestRotator_TavilyAllExhausted503(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(432)
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a"})
	tvPool.SetCredits(0, 5)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "credit-exhausted") {
		t.Fatalf("body = %q, want credit-exhausted message", rec.Body.String())
	}
}

func TestRotator_TavilyDisableUsesFallbackReset(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(432)
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	cfg.CreditResetDay = 15
	tvPool := NewKeyPool([]string{"tvly-a"})
	tvPool.SetCredits(0, 5)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	post(t, r, "/tavily/search", `{"query":"x"}`)
	k := tvPool.Snapshot().Keys[0]
	if !k.Disabled {
		t.Fatal("key should be disabled after 432")
	}
	// Tavily has no billingPeriodEnd: reset is the CREDIT_RESET_DAY fallback
	// (day 15 of this or next month, 00:00 UTC), so DisabledUntil's day is 15.
	if k.DisabledUntil.Day() != 15 {
		t.Fatalf("DisabledUntil = %v, want day 15 (CREDIT_RESET_DAY fallback)", k.DisabledUntil)
	}
	if k.DisabledUntil.Before(time.Now()) {
		t.Fatalf("DisabledUntil = %v is in the past", k.DisabledUntil)
	}
}

// TestRotator_Tavily403RetryBudgetIsCapped is an app-level regression test for
// the 2026-08-28 403 storm. It deliberately goes through LoadConfig +
// buildProfiles (unlike the other tests here, which hand-build a Profile) so it
// catches the actual defect: the tavily profile never set Max403Retries, so
// tryKey's 403 budget guard -- added for firecrawl in 726e487 -- silently did
// not apply and tavily fell back to the full 6-request legacy backoff per key.
//
// Effect: 4 keys x 6 hits x MaxPasses 2 = ~48 upstream requests per client
// request, from one egress IP. That is what got the IP blocked by Tavily's AWS
// edge (403s with a bare awselb/2.0 HTML body), and it made a failing request
// take ~106s to return 403, by which time the caller had usually timed out.
func TestRotator_Tavily403RetryBudgetIsCapped(t *testing.T) {
	orig := backoffSchedule
	backoffSchedule = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { backoffSchedule = orig }()

	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tv-a,tv-b")
	t.Setenv("UPSTREAM", "http://firecrawl.invalid")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	profiles := buildProfiles(cfg)
	var tv *Profile
	for _, p := range profiles {
		if p.Name == "tavily" {
			tv = p
		}
	}
	if tv == nil {
		t.Fatal("tavily profile not built")
	}
	if tv.Max403Retries <= 0 {
		t.Fatalf("tavily Max403Retries = %d, want > 0 (uncapped 403 retries caused the storm)", tv.Max403Retries)
	}

	// Verify the budget actually caps upstream hits end-to-end.
	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(403)
	}))
	defer fake.Close()

	tv.Upstream = fake.URL
	tv.UpstreamHost = strings.TrimPrefix(fake.URL, "http://")
	tv.pool.SetCredits(0, 1000)
	tv.pool.SetCredits(1, 1000)
	r := newRotator(cfg, []*Profile{tv}, fake.Client(), newLogger("error"))

	req := httptest.NewRequest(http.MethodPost, "/tavily/search", strings.NewReader(`{"query":"x"}`))
	req.Host = "rotator.test"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := atomic.LoadInt32(&hits)
	want := int32(2 * (tv.Max403Retries + 1) * cfg.MaxPasses) // 2 keys x hits/key x passes
	if got != want {
		t.Fatalf("upstream hits = %d, want %d (Max403Retries=%d, keys=2, maxPasses=%d)", got, want, tv.Max403Retries, cfg.MaxPasses)
	}
	if got > 8 {
		t.Fatalf("upstream hits = %d, want <= 8: the 403 budget is not capping the storm", got)
	}
}
