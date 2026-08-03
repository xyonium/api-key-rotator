# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`api-key-rotator` is a Go reverse proxy that provides key rotation for
multiple API providers, currently **Firecrawl**, **Tavily**, and **Apify**.
Each provider is a "profile" with its own key pool, upstream base URL, route
prefix, and rotation policy.

- **Firecrawl**: requests without a Tavily/Apify prefix go to
  `api.firecrawl.dev`, with keys selected by remaining credits. Crawl `next`
  pagination URLs are rewritten so pagination stays under rotation.
- **Tavily**: requests with `/tavily` prefix are routed to `api.tavily.com`
  (prefix stripped). Rotation is purely status-code-driven (no body denylist).
- **Apify**: requests with `/v2/acts` prefix are routed to `api.apify.com`
  with the prefix **kept** (the real API lives under `/v2/acts`). Auth is the
  `?token=` **query param** (rotator replaces/adds it per attempt, drops any
  client `Authorization` header). Own HTTP client with `APIFY_TIMEOUT_SEC`
  (default 180s) because sync actor runs take 30-120s. Balance comes from
  `GET /v2/users/me/limits`: `remaining = APIFY_FREE_CREDIT_USD −
  current.monthlyUsageUsd` in **cents**, with the real reset at
  `monthlyUsageCycle.endAt` (billing anniversary, not the 1st). Auto-stops:
  rotate off below `APIFY_LOW_CREDIT_USD` ($0.10), 503 when all below
  `APIFY_STOP_CREDIT_USD` ($0.05). Success is a bare dataset-items array -
  body is never scanned.

The whole point: point firecrawl-mcp's `FIRECRAWL_API_URL` at this proxy and
get key rotation with **zero changes** to firecrawl-mcp. Tavily works by
sed-replacing the Tavily API URL inside OpenWebUI at container startup.

Stdlib-only (`go.mod` has no dependencies), single binary, no external state.

## Commands

```bash
go test ./...                      # run all tests (no Go on host: use docker)
go test -run TestRotator_RotatesOn402 ./...   # single test
go test -run TestRotator ./...      # all tests matching a pattern
go vet ./...
go build -o api-key-rotator .       # build binary

FIRECRAWL_API_KEYS=fc-x ./api-key-rotator   # run locally (firecrawl only)
./api-key-rotator -healthcheck     # GET /healthz on 127.0.0.1:PORT, exit 0/1 (Docker healthcheck)

# Docker-based test (no Go on host):
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test ./... && go vet ./... && go build -o /tmp/api-key-rotator ."
docker build -t api-key-rotator:test .

docker compose up -d --build        # run via compose (rotator + reference mcpo+firecrawl-mcp)
```

CI (`.github/workflows/build-docker.yml`) builds and pushes `ghcr.io/<repo>:latest` + `:sha` on push to main when `Dockerfile`, `*.go`, `go.mod`, or the workflow change.

## Architecture

Request flow lives in `proxy.go`'s `rotator.ServeHTTP`. Everything else exists to support it:

1. **Profile routing** (`profile.go`): `ServeHTTP` checks the request path
   against each profile's `RoutePrefix`. Non-matching requests are handled by
   the first (Firecrawl) profile. The matching profile's key pool, upstream,
   and rotation policy are used for the request.
2. **Buffer request body once** (`io.ReadAll`), then replay it across retry
   attempts - requests are not idempotent-safe to re-send, so the body must
   be re-readable.
3. **Rotation loop** (`maxRotations = MaxPasses * poolSize`): pick the best
   key via `pool.Current()` (highest `remainingCredits` above the stop
   threshold, skipping cooled-down keys). Call `tryKey`:
   - `tryKey` sends the request and, on a **transient** error
     (`shouldRetry`: network error, 403, 408, 5xx), retries the **same key**
     with exponential backoff `500ms/1s/2s/4s/8s` before returning. It only
     signals "give up on this key" (`netErr=true`) after backoff is exhausted.
   - **Over `MAX_BODY_BYTES`** -> forward untouched, break (no rotate, no rewrite).
   - **`shouldRotate`** (provider-dependent) -> record rejection, disable if
     `isCreditExhausted`, `Advance` (cools the key down ~30s), retry next key.
   - **Otherwise** -> record success, `Decrement` predicted credits by
     `creditsUsed` (or 1), trigger `MaybeRefreshLow`, break.
4. **After loop**: if no usable key, return `503`; if JSON and Firecrawl
   profile, rewrite `next` URLs (`rewrite.go`).

Key files and their roles:

| File | Responsibility |
|------|----------------|
| `main.go` | `buildServer` wires `Config` -> `[]Profile` -> transports -> clients (per-profile client when `UpstreamTimeout` > 0, e.g. apify's 180s) -> `Refresher` per profile -> `rotator`. Routes `/healthz`, `/status`, `/`. `--healthcheck` flag. Starts goroutines: `RefreshAll` warm-up, `resetLoop`, `dailyRefreshLoop`. |
| `config.go` | `LoadConfig` parses all env vars; `buildProfiles` constructs Firecrawl + (optional) Tavily/Apify profiles. Validates thresholds (stop <= low). |
| `keys.go` | `KeyPool` - per-key `stats`, `disabled`/`disabledUntil`, `remainingCredits` (MaxInt64 = unmeasured), `cooldownUntil`. `Current`/`currentLocked` pick highest-credit usable key, skipping cooled-down keys (fallback to them if all cooled). `Advance` cools the current key ~30s so equal-credit keys actually rotate. `Decrement`/`SetCredits` adjust predicted/real balances. `AnyUsable` checks >= stop threshold. `Snapshot` masks keys + reports `remainingCredits` (-1 = unmeasured). |
| `profile.go` | `Profile` struct (pool, upstream, route prefix, `KeepPrefix`, `AuthQueryParam`, `UpstreamTimeout`, rotation policy funcs). `matchProfile` routes requests (strips prefix unless `KeepPrefix`). |
| `proxy.go` | The rotator: profile routing, rotation loop + `tryKey` (backoff retries on transient), header copying, body cap, disable-on-credit-exhaustion, credit decrement on success, rewrite+guard, 503 when no usable key. `backoffSchedule`, `extractCreditsUsed`, `readCapped`, `writeRawResponse`, `isHopByHop`. |
| `rotate.go` | `shouldRotate` / `shouldRetry` / `isCreditExhausted` — now profile-aware wrappers that dispatch to Firecrawl-specific or Tavily-specific logic. |
| `refresh.go` | `Refresher` per profile: `OnSwitch`, `MaybeRefreshLow`, `DailyRefresh`, `RefreshAll`. All refreshes run in background goroutines. |
| `rewrite.go` | `rewriteNext` rewrites **only** `"next"` keys with absolute URLs on `upstreamHost`. `paginationGuard` warns on non-terminal crawl status with no `next`. Other host occurrences are never rewritten. |
| `transport.go` | `buildTransport` - `UPSTREAM_PROXY` wins; else `http.ProxyFromEnvironment` (curl-style). `ForceAttemptHTTP2: true`. |
| `creditusage.go` | Firecrawl: `fetchUsage` reads key's `remainingCredits` + `billingPeriodEnd` from `GET /v2/team/credit-usage` (read-only, no credit cost). Tavily: `fetchTavilyUsage` reads `key.usage`/`key.limit`, `account.plan_usage`/`plan_limit`, `account.paygo_usage`/`paygo_limit` from `GET /usage` (per-key); effective remaining = min over layers of (limit - usage), skipping unlimited layers. |
| `server.go` | `logger` (stderr, key=value; `LOG_LEVEL=debug`), `healthzHandler` (503 when no usable key), `statusHandler`. |

### Non-obvious design decisions (respect these when editing)

- **Selection is credit-based, not round-robin.** `Current()` picks the highest-`remainingCredits` usable key. To make rotation actually rotate when credits are equal (e.g. all unmeasured at startup, or all 1000), a rotated-off key gets a ~30s **cooldown** (`Advance` sets it; `RecordSuccess` clears it). Don't "simplify" this away or equal-credit keys will thrash on the same index.
- **Per-key concurrency is capped (firecrawl only).** `KeyPool.maxConcurrent` (default 1) limits in-flight requests per key; `currentLocked` is 3-tiered (free-slot+no-cooldown → free-slot → any-usable) so selection skips busy keys but still returns a key when ALL are saturated (tier 3 distinguishes "saturated/busy" from "exhausted/503"). Slots are acquired in `serveOnce` (`proxy.go`) and released via `defer` on every path. Saturation waits via `WaitForSlot` (a buffered-1 wakeup channel; never holds `mu` while waiting) or rejects per `FIRECRAWL_CONCURRENCY_SATURATION`. tavily/apify pools keep `maxConcurrent=0` (unlimited → legacy behavior).
- **403 is transient, not a key rejection.** A 403 is usually an edge/WAF/network-layer issue, so `tryKey` retries it with backoff on the **same key** (`shouldRetry`), and only rotates after backoff is exhausted. It is NOT in `shouldRotate` and never disables. Firecrawl documents 403 as non-retryable, so production caps these retries via `FIRECRAWL_403_RETRIES` (default 1, = 2 hits/key) to cut churn; `Max403Retries == 0` (the zero value used by test helpers) keeps the legacy full backoff. Never disable on 403 - that would reintroduce the production storm where every key 403s and all get disabled.
- **A `success:true` response NEVER rotates.** The denylist is checked only against failure envelopes (`success:false` or 4xx), never the whole body. Scraped content routinely contains "rate limit"/"payment required"/"credits"; scanning the whole body (the original bug) misclassified good responses and burned credits.
- **Credit-exhausted keys are disabled, not retried.** A genuine 402 / `success:false`+credits envelope disables the key until its reset instant (queried per-key via `/v2/team/credit-usage`); 429/401 rotate-but-keep. Disabling on rate-limit/auth would take a good key offline.
- **Tavily rotates purely on status codes, never body text.** The Firecrawl denylist (body scanning on failure envelopes) is firecrawl-only. Tavily rejects are detected by status code (401/429 rotate, 432/433 disable), matching Tavily's documented behavior. Apify likewise: 401/429 rotate, 402 disables, status only - a success is a bare dataset-items array with no envelope to scan.
- **Apify authenticates via `?token=` query param, not a header.** `tryKey` replaces/adds the param per attempt (`AuthQueryParam`) and drops any client `Authorization` header; the rest of the query string is preserved. Its route prefix is **kept** (`KeepPrefix`), and it gets its own `http.Client` with `APIFY_TIMEOUT_SEC` (default 180s) since sync actor runs take 30-120s.
- **Apify balances are tracked in CENTS.** `fetchApifyUsage` reads `/v2/users/me/limits`: `remaining = (includedCredit − current.monthlyUsageUsd) × 100`, and the reset instant is `monthlyUsageCycle.endAt` (the billing anniversary). The included credit is the account's own `limits.maxMonthlyUsageUsd` read fresh each fetch (plan changes picked up automatically); `APIFY_FREE_CREDIT_USD` only overrides it when > 0. The pool's low/stop thresholds for apify are cents (`APIFY_LOW/STOP_CREDIT_USD` → cents), so sub-dollar auto-stop is exact.
- **`EnsureMeasured` fetches a key's balance synchronously on the request path when it's still unmeasured** (`remainingCredits == MaxInt64`). This makes a real balance gate selection from the first request (so a near-empty apify token 503s immediately instead of serving blind at "unmeasured = plenty" until warm-up lands). No-op once measured; a failed fetch fails open (stays unmeasured).
- **Predicted credits decrement between refreshes.** `Decrement` subtracts `creditsUsed` (or 1) on success so selection stays roughly correct without a refresh per request. Unmeasured keys (MaxInt64) are never decremented. `Refresher` corrects drift: on switch, when predicted < 100 (throttled), and daily.
- **`Current()`/`Advance()` lock independently.** Concurrent requests can pick the same key; a per-request lock would serialize all upstream calls. This is deliberate. A good key is found within `MaxPasses` sweeps.
- **`next`-URL rewriting is intentionally narrow.** Only `"next"` keys with absolute URLs on the upstream host are rewritten. Never broaden this - scraped content can legitimately contain the upstream host.
- **`proxyBase` is derived from `req.Host`** when `PROXY_BASE_URL` is unset.
- **Body cap (`MAX_BODY_BYTES`) is a hard boundary.** Above it, forwarded untouched, no rotate/rewrite. `0` = no cap.
- **No external dependencies.** Stdlib only. Keep it that way.
- **Package is `main`**; tests use `httptest` fakes. When testing the rotator, override `backoffSchedule` to short durations (see `testRotator` + `cfgFor` helpers in `proxy_test.go`) so backoff tests don't sleep ~15s.
Design rationale: `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md`; implementation plan: `docs/superpowers/plans/2026-07-09-firecrawl-token-rotation-plan.md`.

## CodeGraph

This repo is indexed by CodeGraph (`.codegraph/` exists). Prefer `codegraph_explore` (MCP) or `codegraph explore "<symbol>"` (shell) over grep/Read when locating or understanding code.
