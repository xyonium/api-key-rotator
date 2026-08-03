# api-key-rotator

A multi-provider API key rotation reverse proxy. Supports **Firecrawl**,
**Tavily**, and **Apify** profiles — each with its own key pool, upstream,
route prefix, and rotation policy.

## Why

`firecrawl-mcp` forwards all upstream calls to `FIRECRAWL_API_URL`. Pointing
that at this proxy adds key rotation with **zero changes** to firecrawl-mcp -
run the stock `npx -y firecrawl-mcp`.

Tavily integration works by sed-replacing the Tavily API URL inside
OpenWebUI's source so Tavily requests flow through the proxy, picking keys
from a separate pool (see "OpenWebUI + Tavily" below).

## Run

```bash
docker compose up -d
```

With `docker-compose.yml`:

```yaml
api-key-rotator:
  build: .
  environment:
    FIRECRAWL_API_KEYS: "fc-key1,fc-key2,fc-key3"
    UPSTREAM: "https://api.firecrawl.dev"
    PORT: "8788"
    MAX_PASSES: "2"

firecrawl:                     # your existing mcpo + firecrawl-mcp service
  environment:
    FIRECRAWL_API_URL: "http://api-key-rotator:8788"
    # FIRECRAWL_API_KEY removed - the rotator injects it
```

## Configuration (env vars)

| Var | Default | Purpose |
|-----|---------|---------|
| `FIRECRAWL_API_KEYS` | (required) | Comma-separated key pool. |
| `UPSTREAM` | `https://api.firecrawl.dev` | Upstream Firecrawl API base. |
| `UPSTREAM_PROXY` | (unset) | Explicit forward proxy for egress (`http`/`https`/`socks5`/`socks5h`). With `socks5h`, DNS is resolved through the SOCKS5 server (same as `socks5`; the `h` form is accepted for curl/SearXNG-style configs). Wins over system vars. |
| `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` | (unset) | System/curl-style proxy env, honored when `UPSTREAM_PROXY` is unset. |
| `PORT` | `8788` | Listen port. |
| `HOST` | `0.0.0.0` | Listen address. |
| `MAX_PASSES` | `2` | Full passes over the pool before giving up. |
| `MAX_BODY_BYTES` | `16777216` (16 MiB) | Cap on a buffered response body. Above it, forwarded untouched. `0` = no cap. |
| `PROXY_BASE_URL` | (from `Host` header) | Base used when rewriting `next` URLs. |
| `CREDIT_RESET_DAY` | `1` | Fallback day-of-month (1-31, UTC) when a key's billing reset can't be auto-detected. See "Credit disabling" below. |
| `LOW_CREDIT_THRESHOLD` | `10` | Switch off a key (rotate to the next) when its predicted `remainingCredits` drops to/below this. |
| `STOP_CREDIT_THRESHOLD` | `2` | Stop accepting requests when every key is below this. Must be <= `LOW_CREDIT_THRESHOLD`. |
| `CREDIT_REFRESH_INTERVAL` | `300` | Seconds; minimum interval between credit refreshes of a low-balance key. See "Credit-aware selection". |
| `TAVILY_API_KEYS` | (unset) | Comma-separated Tavily key pool. When set, enables the Tavily profile. |
| `TAVILY_UPSTREAM` | `https://api.tavily.com` | Upstream Tavily API base for the Tavily profile. |
| `TAVILY_ROUTE_PREFIX` | `/tavily` | Route prefix that selects the Tavily profile. Stripped before forwarding. |
| `TAVILY_LOW_CREDIT_THRESHOLD` | `10` | Same as `LOW_CREDIT_THRESHOLD` but for the Tavily key pool. |
| `TAVILY_STOP_CREDIT_THRESHOLD` | `2` | Same as `STOP_CREDIT_THRESHOLD` but for the Tavily key pool. |
| `APIFY_API_KEYS` | (unset) | Comma-separated Apify token pool. When set, enables the Apify profile. |
| `APIFY_UPSTREAM` | `https://api.apify.com` | Upstream Apify API base. |
| `APIFY_ROUTE_PREFIX` | `/v2/acts` | Route prefix that selects the Apify profile. **Kept** in the forwarded path (Apify's real API lives under `/v2/acts`). Note: this shadows those specific paths on the Firecrawl default profile; other `/v2/*` paths are unaffected. |
| `APIFY_TIMEOUT_SEC` | `180` | Upstream client timeout for the Apify profile. Synchronous actor runs take 30-120s, so this must comfortably exceed `timeout=` in the request URL. |
| `APIFY_FREE_CREDIT_USD` | (unset = auto) | Override for the plan's **included** monthly credit. When unset, the rotator reads it fresh from the account's `limits.maxMonthlyUsageUsd` on every fetch (so a plan change is picked up automatically). Set it only to force a different value. remaining = `credit − current.monthlyUsageUsd`. |
| `APIFY_LOW_CREDIT_USD` | `0.10` | Rotate off a token when its remaining balance drops below this. |
| `APIFY_STOP_CREDIT_USD` | `0.05` | Stop serving (503) when every Apify token is below this, until the usage cycle resets. |
| `LOG_LEVEL` | `info` | `debug` adds per-request lines. |

## Endpoints

- `GET /healthz` -> `200 {"ok":true}` if at least one key is usable, else `503`. Docker healthcheck target.
- `GET /status` -> pool size, current index, per-key stats, disabled state, and `remainingCredits` (keys masked to last 4 chars; `-1` = unmeasured).

## Credit-aware key selection

The rotator picks the key with the **highest remaining credits** that is above
the stop threshold, so traffic concentrates on the healthiest account. It tracks
each key's `remainingCredits` from `GET /v2/team/credit-usage` (read-only, costs
no credits):

- **Startup**: fetches every key's real balance in the background (the server
  starts immediately; until this completes, unmeasured keys are assumed plentiful).
- **On success**: decrements the used key's predicted balance by the response's
  `creditsUsed` (or by 1 if that field is absent).
- **On rotation**: refreshes the keys we switched off and onto.
- **Low balance**: when a key's *predicted* balance drops below 100, it is
  refreshed at most once per `CREDIT_REFRESH_INTERVAL` (default 5 min) to correct
  estimation drift.
- **Daily**: every key is refreshed once per 24h as a catch-all.

When the current key's predicted balance hits `LOW_CREDIT_THRESHOLD` (default
10), it is rotated off (cooled down ~30s) and the next-richest key takes over.
When **every** key is below `STOP_CREDIT_THRESHOLD` (default 2), `/healthz`
returns `503` and new requests return
`503 {"success":false,"error":"all keys credit-exhausted until billing reset"}`.

## Rotation & retry behavior

Two kinds of failure are handled differently:

**Key-level rejection -> rotate to another key:** HTTP **402** (credits),
**429** (rate limit), **401** (bad key), and failure envelopes
(`{"success":false,...}`) whose text matches `insufficient credits`, `rate
limit`, `exceeded`, `payment required`, `unauthorized`, `forbidden`. The key is
cooled down ~30s (or disabled if credit-exhausted - see below) and the next key
is tried, up to `MAX_PASSES` full sweeps.

**Transient error -> backoff on the SAME key:** HTTP **403** (edge/WAF), **408**,
**5xx**, and network errors are retried on the same key with exponential backoff
`500ms -> 1s -> 2s -> 4s -> 8s` (5 attempts, ~15s total) before rotating. A 403
is usually a network/edge-layer issue, not a per-key problem, so it does NOT
disable or rotate immediately.

A **successful** response (`status < 400` with `success:true`, or no `success`
field) **never** rotates - even if the scraped *content* mentions "rate limit" or
"payment required". The denylist is checked against the Firecrawl failure
envelope only, not the response body as a whole.

- The `next` field's absolute upstream URL is rewritten to the proxy so crawl
  pagination stays under rotation. Other occurrences of the host in response
  bodies are **never** rewritten (they may be real scraped content).

## Credit disabling

A key that returns a **genuine credit-exhaustion** signal (HTTP 402, or a
`success:false` envelope mentioning `insufficient credits` / `payment required`
/ `exceeded`) is **disabled** and skipped on all subsequent requests until its
credits reset - it is not retried every pass (which would waste upstream calls
and risk account flags).

- The reset instant is read **per key** from that key's own
  `GET /v2/team/credit-usage` -> `billingPeriodEnd` (a read-only endpoint that
  costs no credits). This matters because each key belongs to a separate
  account and resets on **that account's billing anniversary**, which is often
  a different day per key - not a universal date.
- If the credit-usage call fails, the key is disabled until the next
  occurrence of `CREDIT_RESET_DAY` (UTC) as a fallback.
- **429 (rate limit)** and **401 (auth)** rotate but do NOT disable -
  they are transient or account-global, and disabling on them would take a
  good key offline. **403** is retried with backoff, never disabled.
- A background loop re-enables each key at its own reset instant; restarting
  the container also clears all disables.

## Tavily profile

When `TAVILY_API_KEYS` is set, the proxy creates a separate key pool for
Tavily. Requests whose path starts with `TAVILY_ROUTE_PREFIX` (default
`/tavily`) are routed to the Tavily pool:

- **Prefix stripping**: the leading `/tavily` is removed before forwarding.
  A request to `/tavily/search` hits `{TAVILY_UPSTREAM}/search`.
- **Rotation policy**: HTTP **401** and **429** rotate the key (cool down
  ~30s) but do not disable. HTTP **432** and **433** disable the key until
  its credit reset (same reset mechanism as Firecrawl - per-key
  `/api/usage` or fallback `CREDIT_RESET_DAY`).
- **Body-based rejection detection**: Tavily rejects are detected purely by
  status code, never by scanning response body text. The Firecrawl denylist
  never applies to Tavily responses.
- **Usage tracking**: the proxy calls `GET /usage` on the Tavily upstream
  (per-key) to read `key.usage` / `key.limit`,
  `account.plan_usage` / `plan_limit`,
  and `account.paygo_usage` / `paygo_limit`. Effective remaining
  credits = min over layers of (limit - usage), skipping any layer whose
  limit <= 0 (unlimited/unmeasured). If all layers are unlimited the key
  stays unmeasured. Tavily returns no billing-period end, so a disabled key
  re-enables at the `CREDIT_RESET_DAY` fallback, same as the Firecrawl
  fallback path.
- **Credit thresholds**: `TAVILY_LOW_CREDIT_THRESHOLD` and
  `TAVILY_STOP_CREDIT_THRESHOLD` mirror the Firecrawl thresholds but
  apply to the Tavily pool independently.

## Apify profile

When `APIFY_API_KEYS` is set, the proxy creates a separate token pool for
Apify. Requests whose path starts with `APIFY_ROUTE_PREFIX` (default
`/v2/acts`) are routed to `https://api.apify.com`:

- **Auth is a query param, not a header.** Clients put `?token=...` in the
  URL (or omit it); the rotator replaces/adds the `token` query parameter
  with the pooled token on every attempt. Any client-sent `Authorization`
  header is dropped. The rest of the query string (`timeout=120`, ...) is
  preserved verbatim.
- **Prefix is kept, not stripped.** A request to
  `/v2/acts/{user}~{actor}/run-sync-get-dataset-items` is forwarded to
  `{APIFY_UPSTREAM}/v2/acts/{user}~{actor}/run-sync-get-dataset-items`
  unchanged.
- **Long timeout.** The Apify profile gets its own HTTP client with
  `APIFY_TIMEOUT_SEC` (default 180s) instead of the shared 30s, because
  synchronous actor runs take 30-120s.
- **Rotation policy** (status codes only, body never scanned - a success is
  a bare dataset-items *array* with no envelope): HTTP **401** and **429**
  rotate (cool down ~30s) but do not disable. HTTP **402** disables the
  token until its real usage-cycle end (see below). **403** stays transient:
  retried on the same token with backoff like every profile.
- **Credit / balance tracking (USD, in cents).** The proxy calls
  `GET /v2/users/me/limits?token=...` (read-only) and computes
  `remaining = includedCredit − current.monthlyUsageUsd`, tracked in cents
  so sub-dollar thresholds are exact. The included credit is the account's
  own `limits.maxMonthlyUsageUsd` read fresh each fetch (a plan upgrade is
  picked up automatically), unless `APIFY_FREE_CREDIT_USD` overrides it. The
  real reset instant is `monthlyUsageCycle.endAt` (the account's **billing
  anniversary**, not the 1st of the month), so a 402-disabled token
  re-enables exactly when the credit renews. If the limits call fails, it
  falls back to `CREDIT_RESET_DAY`.
- **Auto-stop near zero.** A token is rotated off when its remaining balance
  drops below `APIFY_LOW_CREDIT_USD` (default $0.10), and the whole profile
  returns `503` once every token is below `APIFY_STOP_CREDIT_USD` (default
  $0.05). The balance is measured on the first request and refreshed on
  rotation / when low / daily, so a token stops serving *before* it hits $0
  and comes back after the monthly reset - no manual intervention.
- **Note on the included credit:** it comes from the account's
  `limits.maxMonthlyUsageUsd` (the monthly usage cap reported by Apify), read
  fresh every fetch. `APIFY_FREE_CREDIT_USD` exists only as an escape hatch to
  force a different value (e.g. if the account's reported cap isn't the real
  included credit).

Client usage (no SDK, plain HTTP):

```bash
curl -X POST "http://localhost:8788/v2/acts/apimaestro~linkedin-posts-search-scraper-no-cookies/run-sync-get-dataset-items?timeout=120" \
  -H "Content-Type: application/json" \
  -d '{"searchUrl":"https://www.linkedin.com/search/results/content/?keywords=ai"}'
# ?token= is optional - the rotator injects/replaces it from APIFY_API_KEYS.
```

## OpenWebUI + Tavily

To route OpenWebUI's Tavily searches through the proxy, override the Tavily
API URL at container startup with `sed`:

```yaml
services:
  open-webui:
    image: ghcr.io/open-webui/open-webui:main
    # ... your existing config ...
    command: >
      bash -c "
        sed -i \"s|https://api.tavily.com|http://api-key-rotator:8788/tavily|g\"
          /app/backend/open_webui/retrieval/web/tavily.py
          /app/backend/open_webui/retrieval/loaders/tavily.py
        && bash start.sh"
```

The `/tavily` prefix is stripped by the proxy before forwarding to
`https://api.tavily.com`, so no other changes are needed.

## Migration note

This project was originally named `firecrawl-rotator`. The rename to
`api-key-rotator` affects:

| Item | Old | New |
|------|-----|-----|
| Docker image | `ghcr.io/<you>/firecrawl-rotator` | `ghcr.io/<you>/api-key-rotator` |
| Compose service name | `firecrawl-rotator` | `api-key-rotator` |
| `FIRECRAWL_API_URL` host | `http://firecrawl-rotator:8788` | `http://api-key-rotator:8788` |
| Binary / healthcheck | `/rotator -healthcheck` | `/api-key-rotator -healthcheck` |

Update your `docker-compose.yml` service name, image tag, healthcheck path,
and `depends_on` / `FIRECRAWL_API_URL` references accordingly.

## Develop

```bash
go test ./...
go build -o api-key-rotator .
FIRECRAWL_API_KEYS=fc-x ./api-key-rotator
```

See `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md` for
the full design.
