# Add Exa (exa.ai) provider profile — Implementation Plan

**Spec:** `docs/superpowers/specs/2026-08-29-exa-key-rotation-design.md`
**Repo:** `api-key-rotator` (Go, stdlib-only, package `main`)

## Goal

Add a fourth provider profile, **Exa**, at route prefix `/exa`, so Exa requests
get the same key-rotation behavior as Firecrawl/Tavily/Apify.

Non-goal: reading a real balance. Exa exposes none (verified — see below).

## Established facts (verified 2026-08-29, live-probed with a real key)

Probed against `https://api.exa.ai` with a real key held outside this repo
(`exa_api`, gitignored). Where docs and probes disagreed, **probes win**.

| Fact | Evidence |
|---|---|
| Auth header is `x-api-key` | Canonical per docs. `Authorization: Bearer` also returns 200, but is undocumented — do not rely on it. |
| **Invalid key → 401**, not 403 | Live: `{"requestId":...,"error":"Invalid API key","tag":"INVALID_API_KEY"}` |
| Credit/quota exhaustion → **402** | Docs error-codes page: `NO_MORE_CREDITS`, `API_KEY_BUDGET_EXCEEDED`, `TEAM_BUDGET_EXCEEDED`. Exa uses x402. |
| 429 = rate limit; 403 = ACCESS_DENIED / ROBOTS_FILTER_FAILED etc. | Docs error-codes page. 403 stays transient. |
| Error envelope `{"requestId","error","tag"}` | Live: bad enum → 400 `tag:INVALID_REQUEST_BODY`; unknown path → 404 `tag:NOT_FOUND` |
| **`costDollars` is an OBJECT** | Live: `"costDollars":{"total":0.007,"search":{"neural":0.007}}`. Billed = `costDollars.total`, USD float. |
| **No balance endpoint on api.exa.ai** | Probed GET `/billing /v1/billing /usage /v1/usage /account /v1/account /credits /organization /me /key` → **all 404** |
| team-management usage needs a **service key** and returns *spend* | Docs say "Service API key"; exa-cli confirms a plain search key cannot reach it. Returns `total_cost_usd`, not remaining. |
| No pagination / no upstream URLs in bodies | Live response contained **0** occurrences of `api.exa.ai`; no `next` key. |
| Free tier = **$20 signup + $10/month** | exa.ai/pricing, docs/reference/billing |
| Rate limits: /search 10 QPS, /contents 100 QPS, /answer 10 QPS | docs/reference/rate-limits. 25 rapid searches all returned 200 (limit not observable at test rate) |

**UNVERIFIED (design defensively):** 429 `Retry-After` presence/format; whether
rate limiting is per-key or per-network (one error string says "your network");
unused-credit rollover.

### Two traps found by investigation (do not skip)

1. **Cents accounting silently breaks.** `int(0.007 * 100) == 0`. A $0.007
   search decrements **0**, so a cents-denominated balance would sit at full
   budget forever and never auto-stop. Firecrawl/Tavily bill whole credits so
   cents were fine there; Exa bills fractions of a cent.
   → **Track in microUSD** (millionths of a USD): $0.007 → 7000, exact.
   $10 = 10_000_000, comfortably inside int64 (max 9.22e18). Env stays in USD.
2. **`fetchUsageFor` falls through to the Firecrawl endpoint.** Any profile name
   it doesn't recognize gets `GET {upstream}/v2/team/credit-usage`. With
   `Upstream = api.exa.ai` that 404s forever, and `warmupLoop` retries every
   minute until "unmeasured == 0" — which never happens.
   → Give `fetchUsageFor` an explicit `exa` branch returning `usage{}`, AND seed
   the pool at build time so the warm-up never runs for this profile.

## Design decisions (confirmed with user)

| Decision | Choice |
|---|---|
| Balance tracking | Account spend from `costDollars.total` against a configured budget (no balance API exists) |
| Internal unit | **microUSD** (cents truncate to 0 — see trap 1) |
| Default budget | **$10.00** (free tier), `EXA_BUDGET_USD` overrides |
| Starting balance | **Seed full budget at startup** (mid-month restart over-estimates; accepted) |
| Auth | New `Profile.AuthHeader` field (empty = legacy Bearer → zero regression) |
| Exhaustion recovery | `CREDIT_RESET_DAY` fallback, like Tavily; no network call on disable |
| Concurrency | None (`maxConcurrent = 0`, unlimited) — limit is QPS-level, and existing 429-rotate + `Retry-After` cooldown already applies |
| Prefix | `/exa`, **stripped** (`/exa/search` → `api.exa.ai/search`) |
| Apify's `Max403Retries` | **Left at 0** (user's earlier explicit choice) |

## File changes

### 1. `config.go` — `ExaConfig` + parsing

```go
type ExaConfig struct {
    APIKeys     []string
    Upstream    string  // default https://api.exa.ai
    RoutePrefix string  // default /exa
    BudgetMicro int64   // budget in microUSD; default 10_000_000 ($10)
    LowMicro    int64   // rotate off below this; default 500_000 ($0.50)
    StopMicro   int64   // 503 when all below; default 100_000 ($0.10)
}
```

- `loadExaConfig()` mirrors `loadApifyConfig()`: validate `EXA_UPSTREAM` is
  http(s) with a host; validate prefix (leading `/`, non-root, no trailing `/`,
  not `/healthz` or `/status`); reject `stop > low`.
- Budget/thresholds parse from USD env vars via a new `envUsdMicro` helper
  (sibling of `envUsdCents`): `math.Round(f * 1_000_000)`.
- Env vars: `EXA_API_KEYS` (required to enable), `EXA_UPSTREAM`,
  `EXA_ROUTE_PREFIX`, `EXA_BUDGET_USD`, `EXA_LOW_CREDIT_USD`,
  `EXA_STOP_CREDIT_USD`.
- Add `Exa ExaConfig` to `Config`; call `loadExaConfig()` in `LoadConfig`.

### 2. `profile.go` — `AuthHeader` field + build the profile

- Add to `Profile`:
  ```go
  // AuthHeader names the header that carries the key. Empty = legacy
  // "Authorization: Bearer" (firecrawl/tavily/apify unchanged).
  AuthHeader string
  ```
- In `buildProfiles`, append the exa profile when `len(cfg.Exa.APIKeys) > 0`:
  `Name:"exa"`, `RoutePrefix`, `Upstream`, `UpstreamHost`, `CreditResetDay`,
  `RewriteNext:false`, `AuthHeader:"x-api-key"`, own pool with
  `SetThresholds(LowMicro, StopMicro)`.
- **Seed every key**: `exPool.SetCredits(i, cfg.Exa.BudgetMicro)`. This makes
  `EnsureMeasured` a no-op from the first request, so no balance fetch is ever
  attempted.
- **Policy (status-only, body never scanned):**
  - `shouldRotate`: 401, 402, 429 → rotate.
  - `isCreditExhausted`: 402 only.
  - 403 → transient, retried on the same key (never rotates/disables), matching
    every other profile.
  - `rejectKind(402)` already returns `"exhausted"` — no change needed there.

### 3. `proxy.go` — header selection + cost extraction

- In `tryKey`, extend the auth branch (order matters: header → query → bearer):
  ```go
  if p.AuthHeader != "" {
      upReq.Header.Set(p.AuthHeader, key)
  } else if p.AuthQueryParam != "" {
      // existing apify query-param path
  } else {
      upReq.Header.Set("Authorization", "Bearer "+key)
  }
  ```
  (`upReq.Header.Del("Authorization")` already runs above; keep it.)
- Add `extractExaCostMicro(body []byte) int64` in `creditusage.go`:
  parse `costDollars.total` (float64) → `int64(round(v * 1e6))`; return -1 when
  absent/unparseable so the caller falls back to 0-decrement (see below).
- At the `resOK` decrement site, dispatch by profile: exa uses
  `extractExaCostMicro`; others keep `extractCreditsUsed`.
  **Fallback must be 0, not 1**, for exa: guessing "1" would mean 1 microUSD
  ($0.000001) — harmless but meaningless.

### 4. `creditusage.go` — guard the fallthrough

In `fetchUsageFor`, add before the firecrawl default:
```go
if p.Name == "exa" {
    return usage{} // Exa exposes no balance endpoint; never query for one
}
```
This is belt-and-braces with the seeding (trap 2).

### 5. `refresh.go` — correct the low mark

`lowMark()` returns `lowRefreshThreshold` (100) for non-apify profiles, which is
meaningless as microUSD. Add an exa branch returning a microUSD mark
(e.g. $1.00 = 1_000_000) so `MaybeRefreshLow` behaves sanely even though exa has
no fetch to run.

### 6. `main.go` — startup log

Add `exaKeys`, `exaUpstream`, `exaPrefix`, `exaBudgetUsd` to the startup log
line, mirroring the apify fields.

### 7. Docs / config surface

- `README.md`: new "Exa profile" section + `EXA_*` rows in the env table.
  Document explicitly: no balance endpoint, spend accounted from
  `costDollars.total`, microUSD internal, restart = full budget, 402 disables
  until `CREDIT_RESET_DAY`.
- `CLAUDE.md`: add Exa to the provider list; document `AuthHeader`, the
  microUSD-vs-cents trap, and the no-balance-endpoint constraint.
- `docker-compose.yml`: commented `EXA_*` examples.

## Tests (TDD — write failing test first for each)

New file `proxy_exa_test.go` (mirrors `proxy_tavily_test.go` /
`proxy_apify_test.go`):

1. `TestRotator_ExaPrefixStrippedAndAuthHeader` — `/exa/search` → upstream
   `/search` with header `x-api-key: <pooled key>`, and **no** `Authorization`
   header. Asserts the strip AND the new auth path.
2. `TestRotator_Exa402RotatesAndDisables` — key A 402 → rotates to B, A disabled
   until `CREDIT_RESET_DAY` fallback (day-distinguishable), B serves 200.
3. `TestRotator_Exa429RotatesButKeepsKey` — 429 rotates, key stays enabled.
4. `TestRotator_Exa401RotatesButKeepsKey` — 401 rotates, key stays enabled.
5. `TestRotator_ExaSuccessNoRotateAndDecrementsCost` — 200 with
   `costDollars.total: 0.007`; assert remaining goes 10_000_000 → 9_993_000.
   **This is the regression test for the cents trap**: in cents it would stay at
   1000 and pass vacuously, so the test uses microUSD explicitly.
6. `TestRotator_ExaNoBalanceProbesEver` — with a fake upstream that 404s
   everything except the search path, assert **zero** requests to any
   `/usage`-like path across startup + several requests (guards trap 2).
7. `TestRotator_ExaCostDollarsScalarDoesNotCrash` — `costDollars` as an object is
   parsed; a scalar `costDollars: 0.007` (defensive) must not crash.
8. `TestRotator_ExaAllExhausted503` — every key 402 → 503
   `credit-exhausted`.
9. `TestRotator_ExaBodyNeverScanned` — a 200 whose body text contains
   "insufficient credits" must NOT rotate (status-only policy).

Update existing files:

- `config_test.go`: `TestLoadConfig_defaults` (add `Exa` zero-value block), plus
  `EXA_*` parsing/validation tests mirroring the apify ones.
- `profile_test.go`: `TestBuildProfiles_withExa` (prefix, host, AuthHeader,
  thresholds, seeded balance); extend `TestProfile_shouldRotate_*` and
  `TestProfile_isCreditExhausted` with exa rows; add exa to
  `profilesForTest()`/`TestMatchProfile` cases.
- `main_test.go`: no change expected; confirm smoke still passes.

## Verification

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine \
  sh -c "go vet ./... && go test ./... && go build -o /tmp/api-key-rotator ."
# then with -race (needs cgo):
docker run --rm -e CGO_ENABLED=1 -v "$PWD":/src -w /src golang:1.26-alpine \
  sh -c "apk add --no-cache gcc musl-dev && go test -race ./..."
```

Then a manual smoke against the real API (costs ~$0.007):
`POST http://127.0.0.1:8788/exa/search` and confirm 200 + balance decrement.
Check `gofmt -l` only flags files I touched (pre-existing drift exists at HEAD).

## Out of scope

- Apify's `Max403Retries` (user deferred).
- Per-key concurrency for exa (QPS-level limits; revisit if 429s appear).
- Team-management / service-key balance integration (needs a service key we
  don't have, and returns spend rather than remaining).
- Exa MCP server (raw REST is simpler for rotation; MCP auth is per-connection).
