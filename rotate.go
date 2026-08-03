package main

import (
	"encoding/json"
	"regexp"
)

// rejectDenylist matches error phrases Firecrawl puts in a FAILED response's
// error/message fields (the envelope), never in scraped content. It is only
// consulted after we have confirmed the response is a Firecrawl failure
// envelope (success:false, or a 4xx status) - see shouldRotate.
var rejectDenylist = regexp.MustCompile(`(?i)(insufficient credits?|rate limit|exceeded|payment required|unauthorized|forbidden)`)

// creditExhaustedPattern matches failure-envelope text that means the key's
// credits are genuinely gone (firecrawl profile only).
var creditExhaustedPattern = regexp.MustCompile(`(?i)(insufficient credits?|payment required|exceeded)`)

// concurrency429Pattern matches a Firecrawl 429 "concurrency limit reached"
// message (as opposed to a per-minute "rate limit exceeded"). Used only for
// observational logging - both kinds rotate to a different key regardless.
var concurrency429Pattern = regexp.MustCompile(`(?i)(concurr\w*|browser limit)`)

// isConcurrency429 reports whether a 429 body is the concurrency-limit variant.
func isConcurrency429(body []byte) bool { return concurrency429Pattern.Match(body) }

// firecrawlFailure reports whether body is a Firecrawl error envelope:
// {"success":false, ...} OR a non-success status. A {"success":true,...}
// response is NEVER a failure, even if its scraped data mentions "rate limit"
// or "payment required" - those are real scraped words, not key rejections.
func firecrawlFailure(status int, body []byte) bool {
	if status >= 400 && status < 500 {
		return true // 4xx: auth/credits/rate-limit envelope
	}
	var env struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Success != nil {
		return !*env.Success
	}
	return false
}

// shouldRetry reports whether an error is transient and worth retrying on the
// SAME key with backoff before giving up or rotating: network errors, 403
// (edge/WAF), 408 (timeout), and 5xx (upstream). These are NOT key problems.
func shouldRetry(status int, networkErr bool) bool {
	if networkErr {
		return true
	}
	switch status {
	case 403, 408, 500, 502, 503, 504:
		return true
	}
	return false
}
