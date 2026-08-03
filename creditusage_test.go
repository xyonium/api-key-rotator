package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchUsage_RetriesOn5xx: a transient 503 is retried and succeeds when the
// server later returns 200. usageBackoff is an atomic.Value, so shrinking it
// here is race-free even though refresh goroutines read it off the request path.
func TestFetchUsage_RetriesOn5xx(t *testing.T) {
	orig := usageBackoffSchedule()
	usageBackoff.Store([]time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond})
	defer usageBackoff.Store(orig)

	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"remainingCredits":99,"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`))
	}))
	defer fake.Close()

	u := fetchUsage(&http.Client{}, fake.URL, "fc-x", newLogger("info"))
	if !u.ok {
		t.Fatal("expected ok after retry, got !ok")
	}
	if u.remaining != 99 {
		t.Fatalf("remaining = %d, want 99", u.remaining)
	}
	if hits < 3 {
		t.Fatalf("hits = %d, want >=3 (should have retried 503)", hits)
	}
}

// TestFetchUsage_NoRetryOn404: a 404 is permanent; must NOT be retried.
func TestFetchUsage_NoRetryOn404(t *testing.T) {
	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(404)
	}))
	defer fake.Close()

	u := fetchUsage(&http.Client{}, fake.URL, "fc-x", newLogger("info"))
	if u.ok {
		t.Fatal("expected !ok for 404")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (404 must not be retried)", hits)
	}
}

// TestFetchUsage_NoRetryOn401: a 401 is permanent; must NOT be retried.
func TestFetchUsage_NoRetryOn401(t *testing.T) {
	var hits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(401)
	}))
	defer fake.Close()

	u := fetchUsage(&http.Client{}, fake.URL, "fc-x", newLogger("info"))
	if u.ok {
		t.Fatal("expected !ok for 401")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (401 must not be retried)", hits)
	}
}

func TestShouldRetryUsage(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"net:connection refused", true},
		{"net:EOF", true},
		{"status:503", true},
		{"status:502", true},
		{"status:408", true},
		{"status:404", false},
		{"status:401", false},
		{"status:403", false},
		{"status:400", false},
		{"parse:unexpected end of JSON", false},
		{"read:short read", false},
	}
	for _, c := range cases {
		if got := shouldRetryUsage(c.reason); got != c.want {
			t.Errorf("shouldRetryUsage(%q) = %v, want %v", c.reason, got, c.want)
		}
	}
}
