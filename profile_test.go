package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func profilesForTest() []*Profile {
	fc := &Profile{Name: "firecrawl", RoutePrefix: "", pool: NewKeyPool([]string{"fc-a"})}
	tv := &Profile{Name: "tavily", RoutePrefix: "/tavily", pool: NewKeyPool([]string{"tvly-a"})}
	ap := &Profile{Name: "apify", RoutePrefix: "/v2/acts", KeepPrefix: true, AuthQueryParam: "token", pool: NewKeyPool([]string{"apify-a"})}
	return []*Profile{fc, tv, ap}
}

func TestMatchProfile(t *testing.T) {
	profiles := profilesForTest()
	cases := []struct {
		path         string
		wantName     string
		wantStripped string
		wantOK       bool
	}{
		{"/v2/scrape", "firecrawl", "/v2/scrape", true},
		{"/tavily/search", "tavily", "/search", true},
		{"/tavily/extract", "tavily", "/extract", true},
		{"/tavily", "tavily", "/", true},
		{"/tavilyfoo/search", "firecrawl", "/tavilyfoo/search", true}, // no segment boundary -> default
		{"/v2/acts/u~a/run-sync-get-dataset-items", "apify", "/v2/acts/u~a/run-sync-get-dataset-items", true}, // kept whole
		{"/v2/acts", "apify", "/v2/acts", true}, // exact match also kept whole
		{"/v2/actsfoo/x", "firecrawl", "/v2/actsfoo/x", true}, // no segment boundary -> default
		{"/", "firecrawl", "/", true},
	}
	for _, c := range cases {
		p, stripped, ok := matchProfile(profiles, c.path)
		if !ok || p.Name != c.wantName || stripped != c.wantStripped {
			t.Errorf("matchProfile(%q) = (%v, %q, %v), want (%s, %q, true)",
				c.path, p, stripped, ok, c.wantName, c.wantStripped)
		}
	}
}

func TestMatchProfile_unconfiguredPrefixFallsToDefault(t *testing.T) {
	// Only the firecrawl profile configured: /tavily/... falls through to the
	// default profile (byte-compat: single-profile deployments change nothing).
	profiles := []*Profile{{Name: "firecrawl", RoutePrefix: "", pool: NewKeyPool([]string{"fc-a"})}}
	p, stripped, ok := matchProfile(profiles, "/tavily/search")
	if !ok || p.Name != "firecrawl" || stripped != "/tavily/search" {
		t.Fatalf("matchProfile = (%v, %q, %v), want firecrawl default fallthrough", p, stripped, ok)
	}
}

func TestProfile_shouldRotate_firecrawl(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	for _, st := range []int{401, 402, 429} {
		if rotate, _ := p.shouldRotate(st, nil); !rotate {
			t.Errorf("firecrawl shouldRotate(%d) = false, want true", st)
		}
	}
	if rotate, _ := p.shouldRotate(403, nil); rotate {
		t.Error("firecrawl shouldRotate(403) = true, want false (transient)")
	}
	// Failure envelope with denylist phrase still rotates on 200.
	body := []byte(`{"success":false,"error":"rate limit exceeded"}`)
	if rotate, _ := p.shouldRotate(200, body); !rotate {
		t.Error("firecrawl shouldRotate(200, failure envelope) = false, want true")
	}
	// success:true NEVER rotates, even with denylist words in content.
	ok := []byte(`{"success":true,"data":[{"markdown":"payment required here"}]}`)
	if rotate, _ := p.shouldRotate(200, ok); rotate {
		t.Error("firecrawl shouldRotate(200, success:true) = true, want false")
	}
}

func TestProfile_shouldRotate_tavily(t *testing.T) {
	p := &Profile{Name: "tavily"}
	for _, st := range []int{401, 429, 432, 433} {
		if rotate, _ := p.shouldRotate(st, nil); !rotate {
			t.Errorf("tavily shouldRotate(%d) = false, want true", st)
		}
	}
	for _, st := range []int{200, 400, 403, 402} {
		if rotate, _ := p.shouldRotate(st, nil); rotate {
			t.Errorf("tavily shouldRotate(%d) = true, want false", st)
		}
	}
	// Body text never triggers rotation for tavily (status codes suffice).
	body := []byte(`{"detail":{"error":"payment required"}}`)
	if rotate, _ := p.shouldRotate(200, body); rotate {
		t.Error("tavily shouldRotate(200, denylist-ish body) = true, want false")
	}
}

func TestProfile_shouldRotate_apify(t *testing.T) {
	p := &Profile{Name: "apify"}
	for _, st := range []int{401, 402, 429} {
		if rotate, _ := p.shouldRotate(st, nil); !rotate {
			t.Errorf("apify shouldRotate(%d) = false, want true", st)
		}
	}
	// 403 is NOT here: it stays transient (retried on the same key) like the
	// other profiles - only backoff-exhausted 403s rotate via the netErr path.
	for _, st := range []int{200, 400, 403, 404} {
		if rotate, _ := p.shouldRotate(st, nil); rotate {
			t.Errorf("apify shouldRotate(%d) = true, want false", st)
		}
	}
	// Body text never triggers rotation: a 200 dataset-items array containing
	// denylist words is content, not a rejection.
	body := []byte(`[{"text":"rate limit exceeded"}]`)
	if rotate, _ := p.shouldRotate(200, body); rotate {
		t.Error("apify shouldRotate(200, denylist-ish array body) = true, want false")
	}
}

func TestProfile_isCreditExhausted(t *testing.T) {
	fc := &Profile{Name: "firecrawl"}
	if !fc.isCreditExhausted(402, nil) {
		t.Error("firecrawl isCreditExhausted(402) = false, want true")
	}
	if !fc.isCreditExhausted(200, []byte(`{"success":false,"error":"insufficient credits"}`)) {
		t.Error("firecrawl isCreditExhausted(credits envelope) = false, want true")
	}
	if fc.isCreditExhausted(429, nil) {
		t.Error("firecrawl isCreditExhausted(429) = true, want false")
	}
	// 429 with "exceeded" in body must NOT be treated as credit exhaustion.
	// (The word "exceeded" in a rate-limit message like "rate limit exceeded"
	// should not match the creditExhaustedPattern — it's a rate-limit, not
	// a credit balance of zero.)
	if fc.isCreditExhausted(429, []byte(`{"success":false,"error":"rate limit exceeded"}`)) {
		t.Error("firecrawl isCreditExhausted(429 with 'exceeded' body) = true, want false")
	}

	tv := &Profile{Name: "tavily"}
	if !tv.isCreditExhausted(432, nil) || !tv.isCreditExhausted(433, nil) {
		t.Error("tavily isCreditExhausted(432/433) = false, want true")
	}
	for _, st := range []int{401, 402, 429} {
		if tv.isCreditExhausted(st, nil) {
			t.Errorf("tavily isCreditExhausted(%d) = true, want false", st)
		}
	}
	// Tavily never disables on body text.
	if tv.isCreditExhausted(200, []byte(`{"detail":{"error":"insufficient credits"}}`)) {
		t.Error("tavily isCreditExhausted(200 body) = true, want false")
	}

	ap := &Profile{Name: "apify"}
	if !ap.isCreditExhausted(402, nil) {
		t.Error("apify isCreditExhausted(402) = false, want true")
	}
	for _, st := range []int{401, 429} {
		if ap.isCreditExhausted(st, nil) {
			t.Errorf("apify isCreditExhausted(%d) = true, want false", st)
		}
	}
	// Apify never disables on body text, whatever it says.
	if ap.isCreditExhausted(200, []byte(`{"error":{"type":"insufficient-credits"}}`)) {
		t.Error("apify isCreditExhausted(200 body) = true, want false")
	}
}

func TestTavilyRemaining(t *testing.T) {
	cases := []struct {
		name                                    string
		keyU, keyL, planU, planL, payU, payL *int64
		want                                    int64
		wantOK                                  bool
	}{
		{"all layers", ptr(150), ptr(1000), ptr(500), ptr(15000), ptr(25), ptr(100), 75, true},      // min(850, 14500, 75)
		{"key layer smallest", ptr(990), ptr(1000), ptr(0), ptr(15000), ptr(0), ptr(100), 10, true}, // min(10, 15000, 100)
		{"plan layer smallest", ptr(0), ptr(1000), ptr(14990), ptr(15000), ptr(0), ptr(100), 10, true},
		{"no key limit (null)", ptr(100), nil, ptr(500), ptr(15000), ptr(25), ptr(100), 75, true},
		{"no paygo limit (null)", ptr(100), ptr(1000), ptr(500), ptr(15000), ptr(0), nil, 900, true},
		{"key null + paygo null (researcher plan shape)", ptr(2), nil, ptr(2), ptr(1000), ptr(0), nil, 998, true},
		{"all unlimited (null)", ptr(100), nil, ptr(500), nil, ptr(25), nil, 0, false},
		{"exhausted key", ptr(1000), ptr(1000), ptr(500), ptr(15000), ptr(25), ptr(100), 0, true},
		{"explicit zero limit = no credits", ptr(0), ptr(0), ptr(500), ptr(15000), ptr(0), ptr(100), 0, true},
	}
	for _, c := range cases {
		got, ok := tavilyRemaining(c.keyU, c.keyL, c.planU, c.planL, c.payU, c.payL)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: tavilyRemaining = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func ptr(v int64) *int64 { return &v }

func TestFetchTavilyUsage(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tvly-a" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"key": {"usage": 150, "limit": 1000},
			"account": {"plan_usage": 500, "plan_limit": 15000, "paygo_usage": 25, "paygo_limit": 100}
		}`))
	}))
	defer fake.Close()

	u := fetchTavilyUsage(fake.Client(), fake.URL, "tvly-a", nil)
	if !u.ok {
		t.Fatal("fetchTavilyUsage failed")
	}
	if u.remaining != 75 {
		t.Fatalf("remaining = %d, want 75 (min of 850/14500/75)", u.remaining)
	}
	if !u.periodEnd.IsZero() {
		t.Fatalf("periodEnd = %v, want zero (tavily returns no period end)", u.periodEnd)
	}
}

func TestBuildProfiles_firecrawlOnly(t *testing.T) {
	cfg := Config{
		APIKeys:            []string{"fc-a", "fc-b"},
		Upstream:           "https://api.firecrawl.dev",
		UpstreamHost:       "api.firecrawl.dev",
		CreditResetDay:     3,
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
	}
	profiles := buildProfiles(cfg)
	if len(profiles) != 1 {
		t.Fatalf("len = %d, want 1 (tavily disabled)", len(profiles))
	}
	p := profiles[0]
	if p.Name != "firecrawl" || p.RoutePrefix != "" || !p.RewriteNext {
		t.Fatalf("firecrawl profile = %+v", p)
	}
	if p.CreditResetDay != 3 || p.UpstreamHost != "api.firecrawl.dev" {
		t.Fatalf("firecrawl profile fields wrong: %+v", p)
	}
	snap := p.pool.Snapshot()
	if snap.PoolSize != 2 {
		t.Fatalf("pool size = %d, want 2", snap.PoolSize)
	}
}

func TestBuildProfiles_withTavily(t *testing.T) {
	cfg := Config{
		APIKeys:            []string{"fc-a"},
		Upstream:           "https://api.firecrawl.dev",
		UpstreamHost:       "api.firecrawl.dev",
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
		Tavily: TavilyConfig{
			APIKeys:     []string{"tvly-a", "tvly-b"},
			Upstream:    "https://api.tavily.com",
			RoutePrefix: "/tavily",
			LowCredit:   5,
			StopCredit:  1,
		},
	}
	profiles := buildProfiles(cfg)
	if len(profiles) != 2 {
		t.Fatalf("len = %d, want 2", len(profiles))
	}
	tv := profiles[1]
	if tv.Name != "tavily" || tv.RoutePrefix != "/tavily" || tv.RewriteNext {
		t.Fatalf("tavily profile = %+v", tv)
	}
	if tv.UpstreamHost != "api.tavily.com" {
		t.Fatalf("tavily UpstreamHost = %q", tv.UpstreamHost)
	}
	// Thresholds are per-profile: tavily pool uses its own 5/1.
	tv.pool.mu.Lock()
	low, stop := tv.pool.lowThreshold, tv.pool.stopThreshold
	tv.pool.mu.Unlock()
	if low != 5 || stop != 1 {
		t.Fatalf("tavily pool thresholds = %d/%d, want 5/1", low, stop)
	}
	if tv.pool.Snapshot().PoolSize != 2 {
		t.Fatalf("tavily pool size = %d, want 2", tv.pool.Snapshot().PoolSize)
	}
}

func TestBuildProfiles_withApify(t *testing.T) {
	cfg := Config{
		APIKeys:            []string{"fc-a"},
		Upstream:           "https://api.firecrawl.dev",
		UpstreamHost:       "api.firecrawl.dev",
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
		Apify: ApifyConfig{
			APIKeys:     []string{"apify-a", "apify-b"},
			Upstream:    "https://api.apify.com",
			RoutePrefix: "/v2/acts",
			TimeoutSec:  180,
		},
	}
	profiles := buildProfiles(cfg)
	if len(profiles) != 2 {
		t.Fatalf("len = %d, want 2", len(profiles))
	}
	ap := profiles[1]
	if ap.Name != "apify" || ap.RoutePrefix != "/v2/acts" || ap.RewriteNext {
		t.Fatalf("apify profile = %+v", ap)
	}
	if ap.UpstreamHost != "api.apify.com" {
		t.Fatalf("apify UpstreamHost = %q", ap.UpstreamHost)
	}
	if ap.AuthQueryParam != "token" {
		t.Fatalf("apify AuthQueryParam = %q, want token", ap.AuthQueryParam)
	}
	if ap.UpstreamTimeout != 180*time.Second {
		t.Fatalf("apify UpstreamTimeout = %v, want 180s", ap.UpstreamTimeout)
	}
	if ap.pool.Snapshot().PoolSize != 2 {
		t.Fatalf("apify pool size = %d, want 2", ap.pool.Snapshot().PoolSize)
	}
}

func TestFetchTavilyUsage_unauthorized(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer fake.Close()
	if u := fetchTavilyUsage(fake.Client(), fake.URL, "bad", nil); u.ok {
		t.Fatal("expected failure for 401")
	}
}
