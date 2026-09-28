package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_defaults(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a, fc-b ,, fc-c")
	for _, k := range []string{"UPSTREAM", "PORT", "HOST", "MAX_PASSES", "MAX_BODY_BYTES", "PROXY_BASE_URL", "UPSTREAM_PROXY", "LOG_LEVEL", "CREDIT_RESET_DAY", "LOW_CREDIT_THRESHOLD", "STOP_CREDIT_THRESHOLD", "CREDIT_REFRESH_INTERVAL", "TAVILY_API_KEYS", "TAVILY_UPSTREAM", "TAVILY_ROUTE_PREFIX", "TAVILY_LOW_CREDIT_THRESHOLD", "TAVILY_STOP_CREDIT_THRESHOLD", "APIFY_API_KEYS", "APIFY_UPSTREAM", "APIFY_ROUTE_PREFIX", "APIFY_TIMEOUT_SEC", "APIFY_FREE_CREDIT_USD", "APIFY_LOW_CREDIT_USD", "APIFY_STOP_CREDIT_USD", "EXA_API_KEYS", "EXA_UPSTREAM", "EXA_ROUTE_PREFIX", "EXA_BUDGET_USD", "EXA_LOW_CREDIT_USD", "EXA_STOP_CREDIT_USD", "FIRECRAWL_MAX_CONCURRENT_PER_KEY", "FIRECRAWL_CONCURRENCY_SATURATION", "FIRECRAWL_CONCURRENCY_QUEUE_MS", "FIRECRAWL_403_RETRIES",
		"FMP_API_KEYS", "FMP_UPSTREAM", "FMP_ROUTE_PREFIX", "FMP_DAILY_BUDGET", "FMP_LOW_CREDIT", "FMP_STOP_CREDIT",
		"ALPHAVANTAGE_API_KEYS", "ALPHAVANTAGE_UPSTREAM", "ALPHAVANTAGE_ROUTE_PREFIX", "ALPHAVANTAGE_DAILY_BUDGET", "ALPHAVANTAGE_LOW_CREDIT", "ALPHAVANTAGE_STOP_CREDIT",
		"MARKETAUX_API_KEYS", "MARKETAUX_UPSTREAM", "MARKETAUX_ROUTE_PREFIX", "MARKETAUX_DAILY_BUDGET", "MARKETAUX_LOW_CREDIT", "MARKETAUX_STOP_CREDIT"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		APIKeys:                        []string{"fc-a", "fc-b", "fc-c"}, // trimmed, empties dropped
		Upstream:                       "https://api.firecrawl.dev",
		UpstreamHost:                   "api.firecrawl.dev",
		Port:                           "8788",
		Host:                           "0.0.0.0",
		MaxPasses:                      2,
		MaxBodyBytes:                   16 * 1024 * 1024,
		LogLevel:                       "info",
		CreditResetDay:                 1,
		LowCreditThreshold:             10,
		StopCreditThreshold:            2,
		CreditRefreshSec:               300,
		FirecrawlMaxConcurrentPerKey:   1,
		FirecrawlConcurrencySaturation: "queue",
		FirecrawlConcurrencyQueueMs:    15000,
		Firecrawl403Retries:            1,
		FirecrawlRoutePrefix:           "/firecrawl",
		FirecrawlLegacyFallback:        true,
		Tavily: TavilyConfig{
			APIKeys:     nil,
			Upstream:    "https://api.tavily.com",
			RoutePrefix: "/tavily",
			LowCredit:   10,
			StopCredit:  2,
			// Shares FIRECRAWL_403_RETRIES' default of 1: tavily's 403 is an
			// AWS-edge block, and the uncapped 6-request legacy backoff per key
			// is what turned one client request into ~48 upstream hits.
			Retries403: 1,
		},
		Exa: ExaConfig{
			APIKeys:     nil,
			Upstream:    "https://api.exa.ai",
			RoutePrefix: "/exa",
			// $10 free-tier monthly credits, tracked in MICRO-USD (a search
			// costs $0.007 = 0 cents but 7000 microUSD, so cents would never
			// decrement and the profile would never auto-stop).
			BudgetMicro: 10_000_000,
			LowMicro:    500_000,
			StopMicro:   100_000,
		},
		Apify: ApifyConfig{
			APIKeys:         nil,
			Upstream:        "https://api.apify.com",
			RoutePrefix:     "/apify",
			TimeoutSec:      180,
			FreeCreditUsd:   0, // 0 = no override: use the account's own included monthly credit
			LowCreditCents:  10,
			StopCreditCents: 5,
		},
		FMP: FMPConfig{
			APIKeys:     nil,
			Upstream:    "https://financialmodelingprep.com",
			RoutePrefix: "/fmp",
			DailyBudget: 250,
			LowCredit:   10,
			StopCredit:  1,
		},
		AlphaVantage: AlphaVantageConfig{
			APIKeys:     nil,
			Upstream:    "https://www.alphavantage.co",
			RoutePrefix: "/alphavantage",
			DailyBudget: 25,
			LowCredit:   5,
			StopCredit:  1,
		},
		Marketaux: MarketauxConfig{
			APIKeys:     nil,
			Upstream:    "https://api.marketaux.com",
			RoutePrefix: "/marketaux",
			DailyBudget: 100,
			LowCredit:   10,
			StopCredit:  1,
		},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadConfig_emptyKeysErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", " , , ")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for empty key pool, got nil")
	}
}

func TestLoadConfig_badUpstreamErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM", "://no-scheme")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for unparseable upstream, got nil")
	}
}

func TestLoadConfig_badUpstreamProxyErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM_PROXY", "://bad")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for unparseable UPSTREAM_PROXY, got nil")
	}
}

func TestLoadConfig_maxBodyBytesZero(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("MAX_BODY_BYTES", "0")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxBodyBytes != 0 {
		t.Fatalf("MaxBodyBytes = %d, want 0", cfg.MaxBodyBytes)
	}
}

func TestLoadConfig_negativeMaxPassesErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("MAX_PASSES", "0")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for MAX_PASSES=0, got nil")
	}
}

func TestLoadConfig_nonIntegerMaxPassesErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("MAX_PASSES", "abc")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for MAX_PASSES=abc, got nil")
	}
}

func TestLoadConfig_upstreamEmptyHostErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM", "http://")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for UPSTREAM=http://, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid http(s) URL") {
		t.Fatalf("error %q does not contain %q", err.Error(), "not a valid http(s) URL")
	}
}

func TestLoadConfig_upstreamProxyBadSchemeErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM_PROXY", "ftp://proxy:21")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for UPSTREAM_PROXY=ftp://proxy:21, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error %q does not contain %q", err.Error(), "not supported")
	}
}

func TestLoadConfig_creditResetDayValidation(t *testing.T) {
	for _, bad := range []string{"0", "32", "-1"} {
		t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
		t.Setenv("CREDIT_RESET_DAY", bad)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("CREDIT_RESET_DAY=%s: expected error, got nil", bad)
		}
	}
}

func TestFallbackReset(t *testing.T) {
	// resetDay 15, on July 13 2026 -> next July 15 2026 00:00 UTC
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	got := fallbackReset(now, 15)
	want := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("fallbackReset = %v, want %v", got, want)
	}
	// on July 15 after midnight -> rolls to next month (Aug 15)
	now = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	got = fallbackReset(now, 15)
	want = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("fallbackReset roll = %v, want %v", got, want)
	}
}

func TestLoadConfig_tavilyDefaultsWhenUnset(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tavily.APIKeys) != 0 {
		t.Fatalf("Tavily.APIKeys = %v, want empty (profile disabled)", cfg.Tavily.APIKeys)
	}
	// Defaults are still populated so the profile is ready if keys appear.
	if cfg.Tavily.Upstream != "https://api.tavily.com" {
		t.Fatalf("Tavily.Upstream = %q", cfg.Tavily.Upstream)
	}
	if cfg.Tavily.RoutePrefix != "/tavily" {
		t.Fatalf("Tavily.RoutePrefix = %q", cfg.Tavily.RoutePrefix)
	}
	if cfg.Tavily.LowCredit != cfg.LowCreditThreshold || cfg.Tavily.StopCredit != cfg.StopCreditThreshold {
		t.Fatalf("tavily thresholds = %d/%d, want shared %d/%d",
			cfg.Tavily.LowCredit, cfg.Tavily.StopCredit, cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	}
}

func TestLoadConfig_tavilyKeysParsed(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a, tvly-b")
	t.Setenv("TAVILY_ROUTE_PREFIX", "/tv")
	t.Setenv("TAVILY_LOW_CREDIT_THRESHOLD", "5")
	t.Setenv("TAVILY_STOP_CREDIT_THRESHOLD", "1")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tavily.APIKeys) != 2 || cfg.Tavily.APIKeys[0] != "tvly-a" || cfg.Tavily.APIKeys[1] != "tvly-b" {
		t.Fatalf("Tavily.APIKeys = %v", cfg.Tavily.APIKeys)
	}
	if cfg.Tavily.RoutePrefix != "/tv" {
		t.Fatalf("Tavily.RoutePrefix = %q", cfg.Tavily.RoutePrefix)
	}
	if cfg.Tavily.LowCredit != 5 || cfg.Tavily.StopCredit != 1 {
		t.Fatalf("tavily thresholds = %d/%d", cfg.Tavily.LowCredit, cfg.Tavily.StopCredit)
	}
}

func TestLoadConfig_tavilyBadPrefixErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a")
	for _, bad := range []string{"tavily", "/", "/tavily/", "/healthz", "/status"} {
		t.Setenv("TAVILY_ROUTE_PREFIX", bad)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("expected error for TAVILY_ROUTE_PREFIX=%q, got nil", bad)
		}
	}
}

func TestLoadConfig_tavilyBadUpstreamErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a")
	t.Setenv("TAVILY_UPSTREAM", "ftp://example.com")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error for TAVILY_UPSTREAM=ftp://example.com, got nil")
	}
}

func TestLoadConfig_tavilyStopAboveLowErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a")
	t.Setenv("TAVILY_LOW_CREDIT_THRESHOLD", "2")
	t.Setenv("TAVILY_STOP_CREDIT_THRESHOLD", "5")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when TAVILY_STOP > TAVILY_LOW, got nil")
	}
}

func TestLoadConfig_apifyDefaultsWhenUnset(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Apify.APIKeys) != 0 {
		t.Fatalf("Apify.APIKeys = %v, want empty (profile disabled)", cfg.Apify.APIKeys)
	}
	// Defaults are still populated so the profile is ready if keys appear.
	if cfg.Apify.Upstream != "https://api.apify.com" {
		t.Fatalf("Apify.Upstream = %q", cfg.Apify.Upstream)
	}
	if cfg.Apify.RoutePrefix != "/apify" {
		t.Fatalf("Apify.RoutePrefix = %q", cfg.Apify.RoutePrefix)
	}
	if cfg.Apify.TimeoutSec != 180 {
		t.Fatalf("Apify.TimeoutSec = %d, want 180 (sync actor runs take 30-120s)", cfg.Apify.TimeoutSec)
	}
	if cfg.Apify.FreeCreditUsd != 0 {
		t.Fatalf("Apify.FreeCreditUsd = %v, want 0 (no override: use account's included credit)", cfg.Apify.FreeCreditUsd)
	}
	if cfg.Apify.LowCreditCents != 10 || cfg.Apify.StopCreditCents != 5 {
		t.Fatalf("Apify thresholds = %d/%d cents, want 10/5 ($0.10/$0.05)", cfg.Apify.LowCreditCents, cfg.Apify.StopCreditCents)
	}
}

func TestLoadConfig_apifyKeysParsed(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("APIFY_API_KEYS", "apify-a, apify-b")
	t.Setenv("APIFY_TIMEOUT_SEC", "300")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Apify.APIKeys) != 2 || cfg.Apify.APIKeys[0] != "apify-a" || cfg.Apify.APIKeys[1] != "apify-b" {
		t.Fatalf("Apify.APIKeys = %v", cfg.Apify.APIKeys)
	}
	if cfg.Apify.TimeoutSec != 300 {
		t.Fatalf("Apify.TimeoutSec = %d, want 300", cfg.Apify.TimeoutSec)
	}
}

func TestLoadConfig_apifyBadTimeoutErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("APIFY_API_KEYS", "apify-a")
	for _, bad := range []string{"0", "-5", "abc"} {
		t.Setenv("APIFY_TIMEOUT_SEC", bad)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("expected error for APIFY_TIMEOUT_SEC=%q, got nil", bad)
		}
	}
}

func TestLoadConfig_apifyBadPrefixErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("APIFY_API_KEYS", "apify-a")
	for _, bad := range []string{"v2/acts", "/", "/apify/", "/healthz", "/status"} {
		t.Setenv("APIFY_ROUTE_PREFIX", bad)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("expected error for APIFY_ROUTE_PREFIX=%q, got nil", bad)
		}
	}
}

func TestLoadConfig_apifyBadUpstreamErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("APIFY_API_KEYS", "apify-a")
	t.Setenv("APIFY_UPSTREAM", "ftp://example.com")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error for APIFY_UPSTREAM=ftp://example.com, got nil")
	}
}

func TestLoadConfig_concurrencyDefaults(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FirecrawlMaxConcurrentPerKey != 1 {
		t.Fatalf("FirecrawlMaxConcurrentPerKey = %d, want 1", cfg.FirecrawlMaxConcurrentPerKey)
	}
	if cfg.FirecrawlConcurrencySaturation != "queue" {
		t.Fatalf("FirecrawlConcurrencySaturation = %q, want queue", cfg.FirecrawlConcurrencySaturation)
	}
	if cfg.FirecrawlConcurrencyQueueMs != 15000 {
		t.Fatalf("FirecrawlConcurrencyQueueMs = %d, want 15000", cfg.FirecrawlConcurrencyQueueMs)
	}
	if cfg.Firecrawl403Retries != 1 {
		t.Fatalf("Firecrawl403Retries = %d, want 1", cfg.Firecrawl403Retries)
	}
}

func TestLoadConfig_concurrencyParsed(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("FIRECRAWL_MAX_CONCURRENT_PER_KEY", "2")
	t.Setenv("FIRECRAWL_CONCURRENCY_SATURATION", "reject")
	t.Setenv("FIRECRAWL_CONCURRENCY_QUEUE_MS", "5000")
	t.Setenv("FIRECRAWL_403_RETRIES", "0")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FirecrawlMaxConcurrentPerKey != 2 {
		t.Fatalf("FirecrawlMaxConcurrentPerKey = %d, want 2", cfg.FirecrawlMaxConcurrentPerKey)
	}
	if cfg.FirecrawlConcurrencySaturation != "reject" {
		t.Fatalf("FirecrawlConcurrencySaturation = %q, want reject", cfg.FirecrawlConcurrencySaturation)
	}
	if cfg.FirecrawlConcurrencyQueueMs != 5000 {
		t.Fatalf("FirecrawlConcurrencyQueueMs = %d, want 5000", cfg.FirecrawlConcurrencyQueueMs)
	}
	if cfg.Firecrawl403Retries != 0 {
		t.Fatalf("Firecrawl403Retries = %d, want 0", cfg.Firecrawl403Retries)
	}
}

func TestLoadConfig_concurrencyValidation(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	for _, kv := range [][2]string{
		{"FIRECRAWL_MAX_CONCURRENT_PER_KEY", "-1"},
		{"FIRECRAWL_CONCURRENCY_SATURATION", "bogus"},
		{"FIRECRAWL_CONCURRENCY_QUEUE_MS", "-5"},
		{"FIRECRAWL_403_RETRIES", "-2"},
	} {
		t.Setenv(kv[0], kv[1])
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("expected error for %s=%q, got nil", kv[0], kv[1])
		}
		t.Setenv(kv[0], "")
	}
}

func TestLoadConfig_apifyCreditThresholdsParsed(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("APIFY_API_KEYS", "apify-a")
	t.Setenv("APIFY_FREE_CREDIT_USD", "10")
	t.Setenv("APIFY_LOW_CREDIT_USD", "0.25")
	t.Setenv("APIFY_STOP_CREDIT_USD", "0.10")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Apify.FreeCreditUsd != 10 {
		t.Fatalf("Apify.FreeCreditUsd = %v, want 10", cfg.Apify.FreeCreditUsd)
	}
	// USD floats are converted to integer cents: 0.25 -> 25, 0.10 -> 10.
	if cfg.Apify.LowCreditCents != 25 || cfg.Apify.StopCreditCents != 10 {
		t.Fatalf("Apify thresholds = %d/%d cents, want 25/10", cfg.Apify.LowCreditCents, cfg.Apify.StopCreditCents)
	}
}

func TestLoadConfig_apifyBadCreditThresholdsError(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("APIFY_API_KEYS", "apify-a")
	for _, kv := range [][2]string{
		{"APIFY_FREE_CREDIT_USD", "abc"},
		{"APIFY_FREE_CREDIT_USD", "-1"},
		{"APIFY_LOW_CREDIT_USD", "-0.5"},
		{"APIFY_STOP_CREDIT_USD", "xyz"},
	} {
		t.Setenv(kv[0], kv[1])
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("expected error for %s=%q, got nil", kv[0], kv[1])
		}
		// reset to a valid value for the next iteration
		t.Setenv(kv[0], "")
	}
	// stop above low must error
	t.Setenv("APIFY_LOW_CREDIT_USD", "0.05")
	t.Setenv("APIFY_STOP_CREDIT_USD", "0.20")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when APIFY_STOP > APIFY_LOW, got nil")
	}
}

func TestLoadConfig_exaKeysParsed(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("EXA_API_KEYS", "exa-1, exa-2 ,, exa-3")
	t.Setenv("EXA_UPSTREAM", "https://exa.internal:8443")
	t.Setenv("EXA_ROUTE_PREFIX", "/exasearch")
	t.Setenv("EXA_BUDGET_USD", "25.50")
	t.Setenv("EXA_LOW_CREDIT_USD", "1.25")
	t.Setenv("EXA_STOP_CREDIT_USD", "0.25")
	t.Setenv("TAVILY_API_KEYS", "")
	t.Setenv("APIFY_API_KEYS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Exa.APIKeys) != 3 {
		t.Fatalf("exa keys = %v, want 3", cfg.Exa.APIKeys)
	}
	if cfg.Exa.Upstream != "https://exa.internal:8443" {
		t.Fatalf("exa upstream = %q", cfg.Exa.Upstream)
	}
	if cfg.Exa.RoutePrefix != "/exasearch" {
		t.Fatalf("exa prefix = %q", cfg.Exa.RoutePrefix)
	}
	// USD -> microUSD: 25.50 -> 25_500_000, 1.25 -> 1_250_000, 0.25 -> 250_000
	if cfg.Exa.BudgetMicro != 25_500_000 {
		t.Fatalf("BudgetMicro = %d, want 25_500_000", cfg.Exa.BudgetMicro)
	}
	if cfg.Exa.LowMicro != 1_250_000 {
		t.Fatalf("LowMicro = %d, want 1_250_000", cfg.Exa.LowMicro)
	}
	if cfg.Exa.StopMicro != 250_000 {
		t.Fatalf("StopMicro = %d, want 250_000", cfg.Exa.StopMicro)
	}
}

func TestLoadConfig_exaStopAboveLowErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("EXA_API_KEYS", "exa-1")
	t.Setenv("EXA_LOW_CREDIT_USD", "0.10")
	t.Setenv("EXA_STOP_CREDIT_USD", "0.50")
	t.Setenv("TAVILY_API_KEYS", "")
	t.Setenv("APIFY_API_KEYS", "")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when EXA_STOP_CREDIT_USD > EXA_LOW_CREDIT_USD")
	}
}

func TestLoadConfig_exaBadUpstreamErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("EXA_API_KEYS", "exa-1")
	t.Setenv("EXA_UPSTREAM", "not-a-url")
	t.Setenv("TAVILY_API_KEYS", "")
	t.Setenv("APIFY_API_KEYS", "")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error for an invalid EXA_UPSTREAM")
	}
}

func TestLoadConfig_exaBadPrefixErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("EXA_API_KEYS", "exa-1")
	t.Setenv("TAVILY_API_KEYS", "")
	t.Setenv("APIFY_API_KEYS", "")
	for _, bad := range []string{"exa", "/exa/", "/", "/healthz", "/status"} {
		t.Setenv("EXA_ROUTE_PREFIX", bad)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("EXA_ROUTE_PREFIX %q should be rejected", bad)
		}
	}
}

func TestLoadConfig_exaZeroBudgetErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("EXA_API_KEYS", "exa-1")
	t.Setenv("EXA_BUDGET_USD", "0")
	t.Setenv("TAVILY_API_KEYS", "")
	t.Setenv("APIFY_API_KEYS", "")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error for EXA_BUDGET_USD = 0 (a zero budget would 503 every request immediately)")
	}
}

// TestEnvUsdMicro guards the precision the whole Exa profile depends on: a
// search costs $0.007, which is 0 cents.
func TestEnvUsdMicro(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"10", 10_000_000},
		{"0.007", 7000},
		{"0.0025", 2500},
		{"1.25", 1_250_000},
		{"0", 0},
	}
	for _, tc := range cases {
		t.Setenv("EXA_TEST_USD", tc.in)
		got, err := envUsdMicro("EXA_TEST_USD", -1)
		if err != nil {
			t.Fatalf("envUsdMicro(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("envUsdMicro(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// Unset -> default
	t.Setenv("EXA_TEST_USD", "")
	if got, _ := envUsdMicro("EXA_TEST_USD", 777); got != 777 {
		t.Errorf("unset envUsdMicro = %d, want default 777", got)
	}
	// Negative / non-numeric are rejected
	for _, bad := range []string{"-1", "abc"} {
		t.Setenv("EXA_TEST_USD", bad)
		if _, err := envUsdMicro("EXA_TEST_USD", 0); err == nil {
			t.Errorf("envUsdMicro(%q) should error", bad)
		}
	}
}
