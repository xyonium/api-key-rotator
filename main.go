package main

import (
	"flag"
	"net/http"
	"os"
	"time"
)

func buildServer() (*http.Server, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}

	profiles := buildProfiles(cfg)
	tr, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
	}
	log := newLogger(cfg.LogLevel)

	for _, p := range profiles {
		p.refresh = NewRefresher(p, client, cfg, log)
		if p.UpstreamTimeout > 0 {
			// Long-running upstreams (apify sync actor runs) get their own
			// client; the shared 30s one would kill them mid-run.
			p.client = &http.Client{Transport: tr, Timeout: p.UpstreamTimeout}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(profiles))
	mux.HandleFunc("/status", statusHandler(profiles))
	// Everything else goes to the rotator.
	mux.Handle("/", newRotator(cfg, profiles, client, log))

	// Per-profile background loops: warm-up (self-healing), reset re-enable,
	// daily catch-all refresh.
	for _, p := range profiles {
		go warmupLoop(p.refresh, log)
		go resetLoop(p.pool, log)
		go dailyRefreshLoop(p.refresh, log)
	}

	log.info("api-key-rotator starting",
		"profiles", len(profiles),
		"keys", len(cfg.APIKeys), "upstream", cfg.Upstream, "maxPasses", cfg.MaxPasses,
		"tavilyKeys", len(cfg.Tavily.APIKeys), "tavilyUpstream", cfg.Tavily.Upstream,
		"tavilyPrefix", cfg.Tavily.RoutePrefix,
		"apifyKeys", len(cfg.Apify.APIKeys), "apifyUpstream", cfg.Apify.Upstream,
		"apifyPrefix", cfg.Apify.RoutePrefix, "apifyTimeoutSec", cfg.Apify.TimeoutSec,
		"creditResetDay", cfg.CreditResetDay,
		"lowCreditThreshold", cfg.LowCreditThreshold,
		"stopCreditThreshold", cfg.StopCreditThreshold,
		"creditRefreshSec", cfg.CreditRefreshSec)

	return &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: mux,
	}, nil
}

// resetLoop wakes every minute and re-enables any key whose disabledUntil has
// passed. Cheap and bounded; the heavy work (the /v2/team/credit-usage query)
// happens once, at disable time.
func resetLoop(pool *KeyPool, log *logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if n := pool.ReenableDue(time.Now().UTC()); n > 0 {
			log.info("re-enabled credit-disabled keys", "count", n)
		}
	}
}

// dailyRefreshLoop wakes hourly and asks the refresher to refresh any key whose
// last daily refresh is older than 24h. (The refresher itself enforces the
// 24h-per-key cadence; this ticker just pokes it.)
func dailyRefreshLoop(refresh *Refresher, log *logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		refresh.DailyRefresh()
	}
}

// warmupLoop runs the initial warm-up. If any keys remain unmeasured after a
// pass (their credit-usage fetch failed), it retries every minute until all are
// measured - so a transient startup network failure self-heals quickly instead
// of leaving keys at "unmeasured" (-1) until the next daily refresh.
func warmupLoop(refresh *Refresher, log *logger) {
	if unmeasured := refresh.RefreshAll(); unmeasured > 0 {
		log.warn("warm-up left keys unmeasured, will retry", "unmeasured", unmeasured)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if unmeasured := refresh.RefreshAll(); unmeasured == 0 {
				log.info("warm-up complete, all keys measured")
				return
			}
		}
	}
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "GET /healthz and exit 0/1")
	flag.Parse()

	if *healthcheck {
		cfg, err := LoadConfig()
		if err != nil {
			os.Exit(1)
		}
		resp, err := http.Get("http://127.0.0.1:" + cfg.Port + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}

	srv, err := buildServer()
	if err != nil {
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := srv.ListenAndServe(); err != nil {
		os.Stderr.WriteString("server error: " + err.Error() + "\n")
		os.Exit(1)
	}
}
