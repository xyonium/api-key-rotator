package main

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestMain_SmokeHealthz(t *testing.T) {
	// Start the full server on a random port and hit /healthz.
	t.Setenv("FIRECRAWL_API_KEYS", "fc-smoke")
	t.Setenv("APIFY_API_KEYS", "apify-smoke")
	// Clear potentially-set env vars that LoadConfig validates, so the test
	// is robust to the host environment.
	t.Setenv("UPSTREAM", "")
	t.Setenv("UPSTREAM_PROXY", "")
	t.Setenv("PORT", "")
	t.Setenv("HOST", "")
	t.Setenv("MAX_PASSES", "")
	t.Setenv("MAX_BODY_BYTES", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("CREDIT_RESET_DAY", "")
	t.Setenv("LOW_CREDIT_THRESHOLD", "")
	t.Setenv("STOP_CREDIT_THRESHOLD", "")
	t.Setenv("CREDIT_REFRESH_INTERVAL", "")
	srv, err := buildServer()
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	srv.Addr = "127.0.0.1:0"
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
