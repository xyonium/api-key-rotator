package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type rotator struct {
	cfg      Config
	profiles []*Profile
	client   *http.Client
	log      *logger
}

func newRotator(cfg Config, profiles []*Profile, client *http.Client, log *logger) *rotator {
	return &rotator{cfg: cfg, profiles: profiles, client: client, log: log}
}

// backoff schedule for transient retries (network/403/5xx) on the SAME key:
// 500ms, 1s, 2s, 4s, 8s. Total worst case ~15.5s before giving up on a key.
var backoffSchedule = []time.Duration{
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
}

// rotResultKind classifies the outcome of one rotation attempt (serveOnce).
type rotResultKind int

const (
	resOK        rotResultKind = iota // success; stop the loop and serve
	resCapped                         // response body over MAX_BODY_BYTES; forward untouched
	resRotate                         // shouldRotate; RecordRejection, maybe disable, Advance, continue
	resNetErr                         // transient retries exhausted on the key; Advance, continue
	resExhausted                      // Current() < 0 (all below stop / disabled) -> 503
	resSaturated                      // usable keys exist but every in-flight slot is full -> queue/reject
)

type rotResult struct {
	kind   rotResultKind
	idx    int
	key    string
	status int
	header http.Header
	body   []byte
}

// serveOnce performs one rotation attempt: pick a key, acquire its in-flight
// slot (released on EVERY path via defer), and classify the outcome. The slot
// is held across the key's same-key backoff retries inside tryKey, then
// released before any response writing or disable network calls.
func (r *rotator) serveOnce(p *Profile, req *http.Request, inBody []byte, strippedPath string) rotResult {
	idx, key := p.pool.Current()
	if idx < 0 {
		return rotResult{kind: resExhausted}
	}
	if !p.pool.TryAcquire(idx) {
		return rotResult{kind: resSaturated, idx: idx, key: key}
	}
	defer p.pool.Release(idx)

	// A key whose balance was never fetched is "unmeasured = plenty". Fetch
	// it now (synchronously, once) so the real balance gates selection from
	// the first request - this is what lets a near-empty token stop serving
	// instead of burning through the month. No-op once measured.
	if p.refresh != nil {
		p.refresh.EnsureMeasured(idx)
	}

	status, header, body, capped, netErr := r.tryKey(req, p, idx, key, inBody, strippedPath)
	if capped {
		return rotResult{kind: resCapped, idx: idx, key: key, status: status, header: header, body: body}
	}
	if netErr {
		return rotResult{kind: resNetErr, idx: idx, key: key, status: status, header: header, body: body}
	}
	if rotate, reason := p.shouldRotate(status, body); rotate {
		if status == 429 && isConcurrency429(body) {
			r.log.info("429 concurrency limit, rotating to a free-slot key", "profile", p.Name, "key", idx, "reason", reason)
		}
		return rotResult{kind: resRotate, idx: idx, key: key, status: status, header: header, body: body}
	}
	return rotResult{kind: resOK, idx: idx, key: key, status: status, header: header, body: body}
}

func (r *rotator) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p, strippedPath, ok := matchProfile(r.profiles, req.URL.Path)
	if !ok {
		http.Error(w, `{"success":false,"error":"no profile configured for this path"}`, http.StatusNotFound)
		return
	}

	// Buffer the incoming body once so retries can replay it.
	inBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), 400)
		return
	}
	_ = req.Body.Close()

	maxRotations := r.cfg.MaxPasses * len(p.pool.keys)
	var lastStatus int
	var lastHeader http.Header
	var lastBody []byte
	var overCap bool
	var cleanBreak bool

	// Saturation policy (firecrawl): "queue" waits up to this long for a free
	// slot across the whole request; "reject" fails fast.
	saturation := p.ConcurrencySaturation
	if saturation == "" {
		saturation = "queue"
	}
	var queueDeadline time.Time
	if saturation == "queue" && p.ConcurrencyQueue > 0 {
		queueDeadline = time.Now().Add(p.ConcurrencyQueue)
	}

	for tries := 0; tries < maxRotations; {
		res := r.serveOnce(p, req, inBody, strippedPath)

		if res.kind == resSaturated {
			if saturation == "reject" {
				r.log.warn("all keys busy, rejecting", "profile", p.Name)
				http.Error(w, `{"success":false,"error":"all keys busy","profile":"`+p.Name+`"}`, http.StatusTooManyRequests)
				return
			}
			remaining := time.Until(queueDeadline)
			if p.ConcurrencyQueue <= 0 || remaining <= 0 || !p.pool.WaitForSlot(req.Context(), remaining) {
				if req.Context().Err() != nil {
					return // client went away; don't write
				}
				r.log.warn("all keys busy, queue timeout", "profile", p.Name)
				http.Error(w, `{"success":false,"error":"all keys busy, queue timeout","profile":"`+p.Name+`"}`, http.StatusServiceUnavailable)
				return
			}
			continue // queued: a slot freed, retry WITHOUT consuming a rotation pass
		}

		tries++
		lastStatus, lastHeader, lastBody = res.status, res.header, res.body

		switch res.kind {
		case resOK:
			p.pool.RecordSuccess(res.idx)
			p.pool.Decrement(res.idx, extractCreditsUsed(res.body))
			if p.refresh != nil {
				p.refresh.MaybeRefreshLow(res.idx)
			}
			cleanBreak = true
		case resCapped:
			overCap = true
			cleanBreak = true
			r.log.warn("response body over MAX_BODY_BYTES, forwarding untouched", "profile", p.Name, "key", res.idx)
		case resNetErr:
			r.log.warn("transient errors exhausted on key, rotating", "profile", p.Name, "key", res.idx)
			prev := res.idx
			p.pool.Advance()
			if next, _ := p.pool.Current(); next >= 0 && p.refresh != nil {
				p.refresh.OnSwitch(prev, next)
			}
			continue
		case resRotate:
			kind := rejectKind(res.status)
			p.pool.RecordRejection(res.idx, kind)
			r.log.info("rotating key", "profile", p.Name, "from", res.idx, "reason", "status "+strconv.Itoa(res.status), "masked", maskKey(res.key))
			// Genuine credit exhaustion disables the key until its billing reset.
			if p.isCreditExhausted(res.status, res.body) {
				disableUntilReset(p, r.client, res.idx, res.key, time.Now().UTC(), r.log)
				r.log.warn("key credit-disabled until reset", "profile", p.Name, "key", res.idx, "masked", maskKey(res.key))
			}
			prev := res.idx
			p.pool.Advance()
			if next, _ := p.pool.Current(); next >= 0 && next != prev && p.refresh != nil {
				p.refresh.OnSwitch(prev, next)
			}
			continue
		case resExhausted:
			r.log.warn("no usable keys (all below stop credit threshold)", "profile", p.Name)
		}

		if cleanBreak || res.kind == resExhausted {
			break
		}
	}

	if !cleanBreak {
		r.log.warn("all keys exhausted", "profile", p.Name, "lastStatus", lastStatus, "keys", len(p.pool.keys), "maxPasses", r.cfg.MaxPasses)
	}

	if overCap {
		writeRawResponse(w, lastStatus, lastHeader, lastBody)
		return
	}

	// If every key is credit-exhausted, return 503 with a clear message.
	if idx, _ := p.pool.Current(); idx < 0 {
		http.Error(w, `{"success":false,"error":"all keys credit-exhausted until billing reset","profile":"`+p.Name+`"}`, http.StatusServiceUnavailable)
		return
	}

	// On exhaustion with a non-503 last status, surface the last error verbatim
	// (e.g. a real 401/402). lastStatus 0 means we never got a response.
	if !cleanBreak && lastStatus == 0 {
		http.Error(w, `{"success":false,"error":"upstream unavailable"}`, http.StatusBadGateway)
		return
	}

	// Rewrite next URLs + pagination guard on the final body (firecrawl only).
	finalBody := lastBody
	if p.RewriteNext && isJSON(lastHeader) {
		proxyBase := r.cfg.ProxyBaseURL
		if proxyBase == "" {
			proxyBase = "http://" + req.Host
		}
		proxyBase = formatProxyBase(proxyBase)
		if rb, changed := rewriteNext(lastBody, proxyBase, p.UpstreamHost); changed {
			finalBody = rb
		}
		if paginationGuard(lastBody) {
			r.log.warn("pagination response with more data but no 'next' key - rewrite skipped, pagination may bypass proxy")
		}
	}

	writeRawResponse(w, lastStatus, lastHeader, finalBody)
}

// tryKey sends one request with the given key, retrying transient errors
// (network/403/5xx) with exponential backoff on the SAME key. Returns the final
// status/header/body, whether the body was over the cap, and netErr=true if the
// key gave up after exhausting backoff retries (caller rotates).
func (r *rotator) tryKey(req *http.Request, p *Profile, idx int, key string, inBody []byte, strippedPath string) (status int, header http.Header, body []byte, capped bool, netErr bool) {
	client := r.client
	if p.client != nil {
		client = p.client
	}
	for attempt := 0; attempt < len(backoffSchedule)+1; attempt++ {
		upstreamURL := p.Upstream + strippedPath
		if req.URL.RawQuery != "" {
			upstreamURL += "?" + req.URL.RawQuery
		}
		upReq, err := http.NewRequestWithContext(req.Context(), req.Method, upstreamURL, bytes.NewReader(inBody))
		if err != nil {
			return 0, nil, nil, false, true
		}
		copyHeaders(upReq.Header, req.Header)
		upReq.Header.Del("Authorization")
		upReq.Header.Del("Host")
		if p.AuthQueryParam != "" {
			// Query-param auth (apify): the key rides in the URL, not a header.
			// Set replaces any client-supplied value, so rotation just works.
			q := upReq.URL.Query()
			q.Set(p.AuthQueryParam, key)
			upReq.URL.RawQuery = q.Encode()
		} else {
			upReq.Header.Set("Authorization", "Bearer "+key)
		}
		upReq.Host = p.UpstreamHost
		for k := range upReq.Header {
			if isHopByHop(k) {
				upReq.Header.Del(k)
			}
		}

		resp, err := client.Do(upReq)
		if err != nil {
			// network error: retry same key with backoff if attempts remain
			if attempt < len(backoffSchedule) && r.sleepOrCancel(req, backoffSchedule[attempt]) {
				return 0, nil, nil, false, true // client canceled
			}
			if attempt >= len(backoffSchedule) {
				r.log.warn("upstream request error, giving up on key", "profile", p.Name, "key", idx, "err", err)
				return 0, nil, nil, false, true
			}
			continue
		}

		b, overCap := readCapped(resp.Body, r.cfg.MaxBodyBytes)
		_ = resp.Body.Close()

		if overCap {
			return resp.StatusCode, resp.Header, b, true, false
		}

		if shouldRetry(resp.StatusCode, false) {
			// transient status (403/5xx): backoff and retry SAME key. A 403 gets
			// a smaller budget (Firecrawl documents it as non-retryable - it's a
			// permission/edge block, so hammering it 6x per key just churns);
			// p.Max403Retries == 0 keeps the legacy full backoff.
			budget := len(backoffSchedule)
			if resp.StatusCode == 403 && p.Max403Retries > 0 && p.Max403Retries < budget {
				budget = p.Max403Retries
			}
			if attempt < budget {
				r.log.warn("transient upstream status, backing off", "profile", p.Name, "key", idx, "status", resp.StatusCode, "attempt", attempt+1)
				if r.sleepOrCancel(req, backoffSchedule[attempt]) {
					return resp.StatusCode, resp.Header, b, false, true
				}
				continue
			}
			// backoff exhausted on this key: signal rotate
			r.log.warn("transient status persisted, rotating", "profile", p.Name, "key", idx, "status", resp.StatusCode)
			return resp.StatusCode, resp.Header, b, false, true
		}

		return resp.StatusCode, resp.Header, b, false, false
	}
	return 0, nil, nil, false, true
}

// sleepOrCancel sleeps for d, returning true if the request was canceled during
// the sleep (caller should abort). Cancellation propagates the client context.
func (r *rotator) sleepOrCancel(req *http.Request, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-req.Context().Done():
		return true
	}
}

// extractCreditsUsed reads a Firecrawl response's top-level creditsUsed field
// (present on scrape/search/crawl responses). Returns -1 when absent so the
// caller can fall back to a 1-credit estimate.
func extractCreditsUsed(body []byte) int64 {
	var env struct {
		CreditsUsed int64 `json:"creditsUsed"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.CreditsUsed == 0 {
		return -1
	}
	return env.CreditsUsed
}

func rejectKind(status int) string {
	switch status {
	case 402, 432, 433:
		return "exhausted"
	case 429:
		return "rate"
	case 401:
		return "auth"
	default:
		return "retry"
	}
}

func isJSON(h http.Header) bool {
	ct := strings.ToLower(h.Get("Content-Type"))
	return strings.Contains(ct, "json")
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func readCapped(r io.Reader, cap int64) ([]byte, bool) {
	if cap == 0 {
		b, _ := io.ReadAll(r)
		return b, false
	}
	lr := io.LimitReader(r, cap+1)
	b, _ := io.ReadAll(lr)
	if int64(len(b)) > cap {
		return b, true
	}
	return b, false
}

func writeRawResponse(w http.ResponseWriter, status int, h http.Header, body []byte) {
	for k, vs := range h {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func isHopByHop(k string) bool {
	switch k {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailers", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}
