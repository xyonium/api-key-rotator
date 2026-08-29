# Exa (exa.ai) key rotation — Design

**Date:** 2026-08-29
**Status:** Draft
**Repo:** `api-key-rotator`
**Prior spec:** `docs/superpowers/specs/2026-07-21-tavily-key-rotation-design.md`

## Problem

The user uses Exa for search (`reach-mcp`, key in that repo's gitignored
`.env` as `exa_api`). Exa is metered and the free tier is small, so a single
key hitting its quota breaks search. They want Exa added to the rotator as a
fourth profile under route prefix `/exa`.

## Established facts

Verified by live probing `https://api.exa.ai` with a real key on 2026-08-29,
cross-checked against `docs.exa.ai`. Where docs and probes disagreed, the
probes are authoritative.

### Auth

- Canonical header: `x-api-key: <key>`.
- `Authorization: Bearer <key>` **also** returns 200, but this is undocumented.
  Do not rely on it; send the documented header.
- Invalid key → **HTTP 401** `{"requestId":...,"error":"Invalid API key","tag":"INVALID_API_KEY"}`.
  (Not 403 — matters for the rotation policy.)

### Status-code semantics (rotation policy)

| Code | Meaning | Policy |
|---|---|---|
| 401 | invalid/revoked key | rotate, keep key |
| 402 | credits exhausted (`NO_MORE_CREDITS`, `API_KEY_BUDGET_EXCEEDED`, `TEAM_BUDGET_EXCEEDED`) | rotate + **disable** |
| 403 | ACCESS_DENIED / FEATURE_DISABLED / ROBOTS_FILTER_FAILED / PROHIBITED_CONTENT | transient, retry same key |
| 429 | rate limit (`RATE_LIMIT_EXCEEDED`) | rotate, keep key |
| 5xx | server error | transient, retry same key |

Exa uses the x402 payment protocol (the API exposes `PAYMENT-REQUIRED` /
`Payment-Receipt` headers), and quota exhaustion is a real **402**, not a
200-with-error-body. Error envelope is `{"requestId","error","tag"}`.

The body is **never** scanned: status codes are unambiguous, and Exa search
results contain arbitrary scraped page text (which can contain "rate limit",
"insufficient credits", etc.).

### Cost reporting

Every response carries `costDollars`, an **object**:

```json
"costDollars": {"total": 0.007, "search": {"neural": 0.007}}
```

`costDollars.total` is the billed USD amount. Docs: "estimated ... Billing is
computed from usage counters rather than this response object" — it is an
**estimate**, so 402 remains the authoritative exhaustion signal.

### There is no balance endpoint

- Probed `GET /billing /v1/billing /usage /v1/usage /account /v1/account
  /credits /organization /me /key` on `api.exa.ai` → **all HTTP 404**.
- The only usage API is `admin-api.exa.ai/team-management/api-keys/{id}/usage`.
  It requires a **service key** (docs: "Service API key for team
  authentication"; exa-cli integration tests confirm a plain search key cannot
  reach team management), requires knowing each key's UUID `{id}`, and returns
  **spend** (`total_cost_usd`) rather than remaining credits.

→ **Balance cannot be read.** It can only be *accounted*: start from a
configured budget and subtract `costDollars.total` per response.

### Other

- Free tier: **$20 on sign-up + $10 every month**.
- Rate limits: `/search` 10 QPS, `/contents` 100 QPS, `/answer` 10 QPS.
- No pagination: `/search` returns no `next`/cursor (`numResults` 1–100 only).
  A live response contained **zero** occurrences of `api.exa.ai`, so **no
  `next`-URL rewriting is needed**.
- Endpoints that exist: `/search`, `/answer`, `/contents`, `/findSimilar`
  (case-insensitive). `/research` → **410 Gone**. `/websets` → 404 (separate API).

**UNVERIFIED:** 429 `Retry-After` presence/format; whether rate limiting is
per-key or per-network (one error string says "your network"); credit rollover.
Design defensively — the existing `Retry-After` cooldown applies automatically
if the header appears.

## Two traps

1. **Cents truncate to zero.** `int(0.007 * 100) == 0`. A $0.007 search would
   decrement 0, so a cents-denominated balance would never move and the profile
   would never auto-stop. Firecrawl/Tavily bill whole credits (1 search = 1) so
   cents were fine there. **Use microUSD** (millionths of a USD): $0.007 → 7000,
   exact; $10 = 10_000_000, far inside int64.
2. **`fetchUsageFor` falls through to Firecrawl.** Unrecognized profile names get
   `GET {upstream}/v2/team/credit-usage`; against `api.exa.ai` that 404s forever
   while `warmupLoop` retries every minute waiting for "unmeasured == 0". Guard
   with an explicit `exa` branch **and** seed the pool so the warm-up no-ops.

## Approach

Reuse the existing profile machinery unchanged (rotation loop, backoff,
cooldown, pool, refresher). Exa differs from the other profiles in exactly four
ways:

| Concern | Firecrawl | Tavily | Apify | **Exa** |
|---|---|---|---|---|
| Key transport | `Authorization: Bearer` | `Authorization: Bearer` | `?token=` query | **`x-api-key` header** |
| Rotate (keep) | 401/402/429 + denylist | 401/429 | 401/429 | **401/429** |
| Disable | 402 | 432/433 | 402 | **402** |
| Balance source | `/v2/team/credit-usage` | `/usage` | `/v2/users/me/limits` | **none — accounted** |
| Unit | credits | credits | cents | **microUSD** |
| Rewrite `next` | yes | no | no | **no** |

Concretely:

- New `Profile.AuthHeader` field. Empty → legacy `Authorization: Bearer`, so all
  three existing profiles are byte-identical (zero regression risk). Reuses the
  pattern `AuthQueryParam` established for apify.
- Seed every exa key's balance to the configured budget at startup. This makes
  `EnsureMeasured` a no-op, so no balance request is ever issued.
- Account spend: on each 200, `Decrement(costDollars.total × 1e6)`. Fallback
  when `costDollars` is absent is **0** (not 1 — 1 microUSD is meaningless).
- On 402, disable until the `CREDIT_RESET_DAY` fallback (same as Tavily), with
  **no** network request at disable time.

## Consequences (accepted)

- **Mid-month restart over-estimates** the balance (starts at full budget). It
  cannot cause a false 503 — only extra spend until a real 402 arrives, which is
  authoritative. Persisting spend to disk was rejected: the project is
  deliberately stateless.
- Balance is **estimated** (from `costDollars`), so drift is expected. The
  daily/low refresh mechanisms are no-ops for this profile by construction.

## Non-goals

- Service-key / team-management balance integration (no service key; returns
  spend, not remaining).
- Exa MCP server (`mcp.exa.ai/mcp`): adds a JSON-RPC layer with only 4 tools, no
  balance tool, and **per-connection** auth (`?exaApiKey=`) that fights key
  rotation. Raw REST is strictly better here.
- Per-key concurrency for Exa (limits are QPS-level, not concurrency-level).
- Touching apify's `Max403Retries` (previously deferred by the user).
