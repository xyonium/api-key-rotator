package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_defaults(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a, fc-b ,, fc-c")
	for _, k := range []string{"UPSTREAM", "PORT", "HOST", "MAX_PASSES", "MAX_BODY_BYTES", "PROXY_BASE_URL", "UPSTREAM_PROXY", "LOG_LEVEL", "CREDIT_RESET_DAY", "LOW_CREDIT_THRESHOLD", "STOP_CREDIT_THRESHOLD", "CREDIT_REFRESH_INTERVAL", "TAVILY_API_KEYS", "TAVILY_UPSTREAM", "TAVILY_ROUTE_PREFIX", "TAVILY_LOW_CREDIT_THRESHOLD", "TAVILY_STOP_CREDIT_THRESHOLD", "APIFY_API_KEYS", "APIFY_UPSTREAM", "APIFY_ROUTE_PREFIX", "APIFY_TIMEOUT_SEC", "APIFY_FREE_CREDIT_USD", "APIFY_LOW_CREDIT_USD", "APIFY_STOP_CREDIT_USD"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		APIKeys:       []string{"fc-a", "fc-b", "fc-c"}, // trimmed, empties dropped
		Upstream:      "https://api.firecrawl.dev",
		UpstreamHost:  "api.firecrawl.dev",
		Port:          "8788",
		Host:          "0.0.0.0",
		MaxPasses:     2,
		MaxBodyBytes:  16 * 1024 * 1024,
		LogLevel:      "info",
		CreditResetDay: 1,
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
		CreditRefreshSec: 300,
		Tavily: TavilyConfig{
			APIKeys:     nil,
			Upstream:    "https://api.tavily.com",
			RoutePrefix: "/tavily",
			LowCredit:   10,
			StopCredit:  2,
		},
		Apify: ApifyConfig{
			APIKeys:        nil,
			Upstream:       "https://api.apify.com",
			RoutePrefix:    "/v2/acts",
			TimeoutSec:     180,
			FreeCreditUsd:  5,
			LowCreditCents:  10,
			StopCreditCents: 5,
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
	if cfg.Apify.RoutePrefix != "/v2/acts" {
		t.Fatalf("Apify.RoutePrefix = %q", cfg.Apify.RoutePrefix)
	}
	if cfg.Apify.TimeoutSec != 180 {
		t.Fatalf("Apify.TimeoutSec = %d, want 180 (sync actor runs take 30-120s)", cfg.Apify.TimeoutSec)
	}
	if cfg.Apify.FreeCreditUsd != 5 {
		t.Fatalf("Apify.FreeCreditUsd = %v, want 5 (default free monthly credit)", cfg.Apify.FreeCreditUsd)
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
	for _, bad := range []string{"v2/acts", "/", "/v2/acts/", "/healthz", "/status"} {
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
