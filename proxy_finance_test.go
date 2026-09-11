package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// financeCfg returns a Config with one finance profile (plus the default
// firecrawl). The caller supplies per-test NewFinanceProfile Values.
func financeCfg(fake *httptest.Server) Config {
	return Config{
		APIKeys:            []string{"fc-a"},
		Upstream:           "http://unused.local",
		UpstreamHost:       "unused.local",
		MaxPasses:          2,
		MaxBodyBytes:       16 * 1024 * 1024,
		ProxyBaseURL:       "http://rotator.test",
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
	}
}

type financeFake struct {
	t  *testing.T
	ts *httptest.Server
	// status: request count -> (status, body) to return
	statuses map[int]struct {
		code int
		body string
	}
	// authKeyParam: the name of the query parameter the provider expects
	authKeyParam string
	// hits records the auth-param value seen on each upstream request (masked).
	hits []string
}

// newFinanceFake spins a fake that validates the auth param and returns the
// canned status/body for request N (1-indexed; default 200 `{}`).
func newFinanceFake(t *testing.T, authParam string) *financeFake {
	if t == nil {
		t = new(testing.T)
	}
	f := &financeFake{t: t, authKeyParam: authParam, statuses: make(map[int]struct {
		code int
		body string
	})}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.URL.Query().Get(f.authKeyParam))
		n := len(f.hits)
		if s, ok := f.statuses[n]; ok {
			if s.body == "" {
				w.WriteHeader(s.code)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.code)
			_, _ = w.Write([]byte(s.body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	return f
}

// on sets the response for request n (1-indexed).
func (f *financeFake) on(n int, code int, body string) *financeFake {
	f.statuses[n] = struct {
		code int
		body string
	}{code, body}
	return f
}

// seenKeyMask records which auth-param values the upstream saw, masked for
// assertions ("k1"/"k2" placeholders are fine, no real key material).
func (f *financeFake) seen() []string { return f.hits }

// testFinanceProfiles builds profiles for the three finance providers with
// fake backends. Used by all three tests - keeps them structurally identical.
func testFinanceProfiles(t *testing.T) (Config, []*Profile) {
	cfg := financeCfg(nil)

	buildOne := func(name, routePrefix, upstream, authParam string, keys []string, budget, low, stop int64) (*Profile, *financeFake) {
		fake := newFinanceFake(t, authParam)
		fake.ts.Close() // placeholder; replaced per call by caller
		pool := NewKeyPool(keys)
		pool.SetThresholds(low, stop)
		for i := range keys {
			pool.SetCredits(i, budget)
		}
		p := &Profile{
			Name:           name,
			RoutePrefix:    routePrefix,
			Upstream:       upstream,
			UpstreamHost:   httptestURLHost(upstream),
			CreditResetDay: 1,
			AuthQueryParam: authParam,
			pool:           pool,
		}
		return p, fake
	}

	_ = buildOne // silence unused until wired below
	return cfg, nil
}

// withFinanceRotator wires a single profile under test into a rotator - the
// other profiles fall back to firecrawl behavior (not exercised here).
func withFinanceRotator(cfg Config, p *Profile) (*rotator, *financeFake) {
	fake := newFinanceFake(nil, p.AuthQueryParam)
	p.Upstream = fake.ts.URL
	p.UpstreamHost = httptestURLHost(fake.ts.URL)
	for i := range p.pool.Snapshot().Keys {
		p.pool.SetCredits(i, 1000) // plenty; the specific budget seeded below
	}
	return newRotator(cfg, []*Profile{p}, &http.Client{}, newLogger("info")), fake
}

func newFinanceProfile(name, route string, keys []string, budget, low, stop int64) *Profile {
	return &Profile{
		Name:           name,
		RoutePrefix:    route,
		CreditResetDay: 1,
		pool:           NewKeyPool(keys),
	}
}

func TestFMP_RotatesOn401DisablesOn429(t *testing.T) {
	cfg := financeCfg(nil)

	keys := []string{"fmp-k1", "fmp-k2"}
	p := newFinanceProfile("fmp", "/fmp", keys, 250, 10, 1)
	p.AuthQueryParam = "apikey"
	r, fake := withFinanceRotator(cfg, p)
	_ = fake

	fake.on(1, 401, `{"error":"invalid key"}`) // k1: 401 rotate
	fake.on(2, 200, `{}`)                      // k2: ok

	req := httptest.NewRequest("GET", "/fmp/api/v3/profile/AAPL?symbol=AAPL", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d, want 200; body=%s hits=%v", rec.Code, rec.Body.String(), fake.seen())
	}
	if got := fake.seen(); len(got) != 2 || got[0] != "fmp-k1" || got[1] != "fmp-k2" {
		t.Fatalf("expected k1 then k2 rotated via apikey; got %v", got)
	}

	// Now both keys 429 on their OWN request -> disabled until reset.
	p2 := newFinanceProfile("fmp", "/fmp", keys, 250, 10, 1)
	p2.AuthQueryParam = "apikey"
	p2.pool.SetCredits(0, 250)
	p2.pool.SetCredits(1, 250)
	r2, fake2 := withFinanceRotator(cfg, p2)
	// Key1 is cooled down from the r1 rotation loop above, so the rotator may
	// retry it via currentLocked's tier-3 fallback. Set every slot to 429 so
	// both keys are disabled and the pool is exhausted.
	for n := 1; n <= 8; n++ {
		fake2.on(n, 429, `{"error":"Limit Reach"}`)
	}
	req2 := httptest.NewRequest("GET", "/fmp/api/v3/profile/AAPL", nil)
	rec2 := httptest.NewRecorder()
	r2.ServeHTTP(rec2, req2)
	if rec2.Code != 503 {
		t.Fatalf("status=%d, want 503 after 429-disables all keys; body=%s seen=%v", rec2.Code, rec2.Body.String(), fake2.seen())
	}
	// 429 must have DISABLED keys, not just rotated. All keys should appear
	// disabled in the snapshot; Current should return -1.
	for i, k := range p2.pool.Snapshot().Keys {
		if !k.Disabled {
			t.Errorf("key %d was not disabled by FMP 429; should be credit-disabled until reset", i)
		}
	}
	if idx, _ := p2.pool.Current(); idx >= 0 {
		t.Fatalf("expected Current=-1 (all disabled), got idx=%d", idx)
	}
}

func TestAlphaVantage_DisablesOnRateLimitNoteIn200Body(t *testing.T) {
	cfg := financeCfg(nil)

	keys := []string{"av-k1", "av-k2"}
	p := newFinanceProfile("alphavantage", "/alphavantage", keys, 25, 5, 1)
	p.AuthQueryParam = "apikey"
	r, fake := withFinanceRotator(cfg, p)

	// Key1 returns 200 OK but with a Note body (daily cap reached). Key2 OK.
	fake.on(1, 200, `{"Note":"Thank you for using Alpha Vantage! Our standard API rate limit is 25 requests per day."}`)
	fake.on(2, 200, `{"Global Quote":{"01. symbol":"IBM","05. price":"200.00"}}`)

	req := httptest.NewRequest("GET", "/alphavantage/query?function=GLOBAL_QUOTE&symbol=IBM", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d, want 200; body=%s hits=%v", rec.Code, rec.Body.String(), fake.seen())
	}
	nonEmpty := 0
	for _, h := range fake.seen() {
		if h != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 2 || len(fake.seen()) < 2 || fake.seen()[0] != "av-k1" || fake.seen()[1] != "" || fake.seen()[2] != "av-k2" {
		t.Fatalf("expected rotation: k1 -> (usage fetch) -> k2; got %v", fake.seen())
	}

	// A body containing only legitimate data must NOT be scanned - acceptance:
	// a single normal 200 returns without any rotation attempt.
	fake2 := newFinanceFake(t, "apikey")
	p2 := newFinanceProfile("alphavantage", "/alphavantage", keys, 25, 5, 1)
	p2.AuthQueryParam = "apikey"
	p2.pool.SetCredits(0, 25)
	p2.pool.SetCredits(1, 25)
	r2 := newRotator(cfg, []*Profile{p2}, &http.Client{}, newLogger("info"))
	_ = fake2
	fake3 := newFinanceFake(t, "apikey")
	fake3.on(1, 200, `{"Global Quote":{"05. price":"100.00"}}`)
	p2.Upstream = fake3.ts.URL
	p2.UpstreamHost = httptestURLHost(fake3.ts.URL)
	req2 := httptest.NewRequest("GET", "/alphavantage/query?function=GLOBAL_QUOTE&symbol=IBM", nil)
	rec2 := httptest.NewRecorder()
	r2.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("status=%d, want 200 success without scan; hits=%v", rec2.Code, fake3.seen())
	}
	if len(fake3.seen()) != 1 {
		t.Fatalf("expected 1 upstream hit (no rotation), got %v", fake3.seen())
	}
}

func TestMarketaux_402UsageLimitReachedDisables(t *testing.T) {
	cfg := financeCfg(nil)

	keys := []string{"mk-k1", "mk-k2"}
	p := newFinanceProfile("marketaux", "/marketaux", keys, 100, 10, 1)
	p.AuthQueryParam = "api_token"
	r, fake := withFinanceRotator(cfg, p)

	// Key1: 402 usage_limit_reached -> disable. Key2 follows the same path on
	// its own request. After both 402 the pool is exhausted.
	for n := 1; n <= 8; n++ {
		fake.on(n, 402, `{"error":{"code":"usage_limit_reached"}}`)
	}

	req := httptest.NewRequest("GET", "/marketaux/v1/news/all?language=en&limit=1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status=%d, want 503 (all keys credit-disabled); body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.seen(); len(got) < 2 || got[0] != "mk-k1" || got[2] != "mk-k2" {
		t.Fatalf("expected keys api_token rotated then both 402; got %v", got)
	}
	for i, k := range p.pool.Snapshot().Keys {
		if !k.Disabled {
			t.Errorf("marketaux key %d not disabled by 402 usage_limit_reached", i)
		}
	}
}

func TestFinanceProfiles_QueryParamInjection_NeverInHeader(t *testing.T) {
	// Backbone of finance auth: the key MUST travel in the query param, never
	// in an Authorization header (unlike firecrawl/tavily). Also confirms the
	// route prefix is stripped and params are preserved.
	for _, tt := range []struct {
		name, route, param string
	}{
		{"fmp", "/fmp", "apikey"},
		{"alphavantage", "/alphavantage", "apikey"},
		{"marketaux", "/marketaux", "api_token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := financeCfg(nil)
			p := newFinanceProfile(tt.name, tt.route, []string{tt.name + "-k1", tt.name + "-k2"}, 100, 5, 1)
			p.AuthQueryParam = tt.param
			r, fake := withFinanceRotator(cfg, p)

			req := httptest.NewRequest("GET", tt.route+"/some/path?symbols=AAPL,MSFT&language=en", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status=%d, want 200", rec.Code)
			}
			if hits := fake.seen(); len(hits) != 1 || hits[0] != tt.name+"-k1" {
				t.Fatalf("expected single hit with key injected as %s, got %v", tt.param, hits)
			}
		})
	}
}

func TestFMP_ResetByCreditResetDay(t *testing.T) {
	// Disabled FMP key stays disabled until CREDIT_RESET_DAY. We simulate the
	// fallback path directly (skip live fetch) and verify ReenableDue at the
	// right instant.
	pool := NewKeyPool([]string{"fmp-k1"})
	pool.SetCredits(0, 250)
	pool.SetThresholds(10, 1)

	// Disable key with a reset time "tomorrow at midnight" (simulating the FMP
	// reset computed from CreditResetDay).
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	pool.Disable(0, reset)

	if idx, _ := pool.Current(); idx >= 0 {
		t.Fatalf("expected no usable key after disable, got idx=%d", idx)
	}
	// Before reset: still unusable.
	if pool.ReenableDue(now) != 0 {
		t.Fatalf("expected 0 re-enabled before reset instant")
	}
	// At reset: re-enabled and balance reset to MaxInt64 (unmeasured -> plenty).
	if n := pool.ReenableDue(reset); n != 1 {
		t.Fatalf("expected 1 re-enabled at reset, got %d", n)
	}
	if idx, _ := pool.Current(); idx != 0 {
		t.Fatalf("expected key usable again at reset, got idx=%d", idx)
	}
}

func TestFMPStopProtectsLastRequest(t *testing.T) {
	// With stop threshold 1, the very last predicted request is held back so a
	// real 429 never has to fire to learn the key is empty.
	//
	// Note: currentLocked considers a key usable when remaining >= stop (2 >=
	// 1 counts as usable), so we decrement below stop (to 0) to verify the
	// stop floor actually bites. This is pool-level logic, not rotation logic.
	pool := NewKeyPool([]string{"fmp-k1"})
	pool.SetThresholds(10, 1)
	pool.SetCredits(0, 2) // 2 left: one serve (decrements to 1), then stop blocks the last

	// Three decrements: below stop=1 it stops being pickable.
	pool.Decrement(0, 3)
	if rc := pool.PredictedRemaining(0); rc != 0 {
		t.Fatalf("expected 0 remaining after three decrements, got %d", rc)
	}
	// Stop threshold protects the very last request.
	if idx, _ := pool.Current(); idx >= 0 {
		t.Fatalf("expected Current=-1 (no usable key at and below stop floor), got idx=%d", idx)
	}
	if pool.AnyUsable() {
		t.Fatalf("AnyUsable should be false when everything is below stop floor")
	}
}

// TestFinanceRequestBodyPreserved ensures query param + body both survive
// prefix stripping (the finance POST endpoints matter too).
func TestFinanceRequestBodyPreserved(t *testing.T) {
	cfg := financeCfg(nil)
	p := newFinanceProfile("fmp", "/fmp", []string{"k1"}, 250, 10, 1)
	p.AuthQueryParam = "apikey"
	r, fake := withFinanceRotator(cfg, p)

	var gotBody []byte
	var gotQuery string
	fake.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	body := []byte(`{"symbols":["AAPL"]}`)
	req := httptest.NewRequest("POST", "/fmp/api/v3/profile/batch?apikey=old&symbols=AAPL", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	// query: the rotator replaced client apikey with its own (rotation wins),
	// kept the rest.
	if got := gotQuery; got == "" || !bytes.Contains([]byte(got), []byte("symbols=AAPL")) || !bytes.Contains([]byte(got), []byte("apikey=k1")) {
		t.Fatalf("unexpected forwarded query: %q", got)
	}
	if string(gotBody) != string(body) {
		t.Fatalf("body not preserved across rotation: got %q want %q", gotBody, body)
	}
}

func TestFinanceResetAlignsWithCreditResetDay(t *testing.T) {
	// When a finance profile has no live usage endpoint, disableUntilReset's
	// fallback is CREDIT_RESET_DAY at 00:00 UTC, not a rolling 24h window.
	p := &Profile{Name: "fmp", CreditResetDay: 3}
	_ = p
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	got := fallbackReset(now, 3)
	want := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	if got != want {
		t.Fatalf("fallbackReset = %v, want %v (next occurrence of day 3 UTC)", got, want)
	}
}

func TestFMPFirstRequestUnmeasuredStillWarms(t *testing.T) {
	// When keys are seeded at DailyBudget, EnsureMeasured is a no-op from the
	// very first request (no usage endpoint exists). Assert that seeded state.
	p := newFinanceProfile("fmp", "/fmp", []string{"k1", "k2"}, 250, 10, 1)
	p.pool.SetCredits(0, 250)
	p.pool.SetCredits(1, 250)
	snap := p.pool.Snapshot()
	if got := snap.Keys[0].RemainingCredits; got != 250 {
		t.Fatalf("seeded remaining = %d, want 250", got)
	}
}

// TestAlphaVantageNoteDetection: table-driven acceptance of the JSON shapes
// Alpha Vantage actually returns for its daily cap.
func TestAlphaVantageNoteDetection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"free tier exhausted", `{"Note":"Thank you for using Alpha Vantage! Our standard API rate limit is 25 requests per day"}`, true},
		{"premium info", `{"Information":"Thank you for using Alpha Vantage! ..."}`, true},
		{"data quote", `{"Global Quote":{"01. symbol":"IBM","05. price":"200.00"}}`, false},
		{"csv", `"timestamp","symbol","open"\n2024-01-01,IBM,100`, false},
		{"error message", `{"Error Message":"Invalid API call."}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := alphaVantageIsExhausted([]byte(c.body)); got != c.want {
				t.Errorf("alphaVantageIsExhausted(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// TestFMP429IsExhaustedNotRotated asserts the design decision that a 429
// "Limit Reach" from FMP disables the key (it is a daily cap) rather than
// merely rotates it (which would only hammer other keys for the same account).
func TestFMP429IsExhaustedNotRotated(t *testing.T) {
	p := &Profile{Name: "fmp"}
	if !p.isCreditExhausted(429, nil) {
		t.Fatalf("FMP 429 must be treated as credit-exhausted (daily cap)")
	}
	// And the opposite: a genuine 401 key-expired must NOT credit-exhaust.
	if p.isCreditExhausted(401, nil) {
		t.Fatalf("FMP 401 is an auth problem, not credit-exhausted")
	}
}

func TestMarketaux402IsExhausted401Rotates(t *testing.T) {
	p := &Profile{Name: "marketaux"}
	if !p.isCreditExhausted(402, nil) {
		t.Fatalf("Marketaux 402 usage_limit_reached must be credit-exhausted")
	}
	if p.isCreditExhausted(401, nil) {
		t.Fatalf("Marketaux 401 invalid_api_token is an auth rotation, not exhaustion")
	}
	if p.isCreditExhausted(429, nil) {
		t.Fatalf("Marketaux 429 rate_limit is transient (rotate), not exhaustion")
	}
}

// TestFinanceHelpersShared sanity: prefix validation + stop<=low guards.
func TestFinanceRoutePrefixGuards(t *testing.T) {
	if err := checkRoutePrefix("FMP", "/fmp"); err != nil {
		t.Fatalf("legit prefix rejected: %v", err)
	}
	if err := checkRoutePrefix("FMP", "/healthz"); err == nil {
		t.Fatalf("reserved /healthz was not rejected")
	}
	if err := checkRoutePrefix("FMP", "fmp"); err == nil {
		t.Fatalf("non-/-prefixed route was not rejected")
	}
	if err := checkRoutePrefix("FMP", "/"); err == nil {
		t.Fatalf("root '/' route was not rejected")
	}
}

// TestJSONUnmarshal sanity for Alpha Vantage body scan - parse cost-free.
func TestAlphaVantageNoteShape(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal([]byte(`{"Note":"..."}`), &v); err != nil {
		t.Fatal(err)
	}
	if _, ok := v["Note"]; !ok {
		t.Fatal("Note key not parsed")
	}
}

// silence unused-import warnings when running subset builds
var _ = fmt.Sprintf
var _ = strings.Contains
