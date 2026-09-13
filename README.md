# prerender

A small server-side rendering service for JavaScript-heavy sites. The path after
the service host is the URL to render:

```
GET https://prerender.sbisecurities.in/https://www.sbisecurities.in/about-us?page=2
```

Rendered HTML is cached in Redis as a plain, readable string.

## Build

```sh
go mod tidy
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o prerender ./cmd/prerender
```

Ship the `prerender` binary and your `.env`. Nothing else is needed.

## Run

```sh
ALLOWED_HOSTS=www.sbisecurities.in ./prerender
```

`ALLOWED_HOSTS` is the only mandatory setting. Without it the service would
fetch and return any URL on the internet, so startup refuses to proceed.
Everything else has a default; see `.env.example`.

Chromium must be installed and on `PATH`, or pointed at with `CHROME_PATH`.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /<target-url>` | Render (or serve from cache) |
| `GET /healthz` | Liveness — 200 whenever the process is running |
| `GET /readyz` | Readiness — checks Redis and Chromium, names what is broken |
| `GET /_metrics` | Counters: in-flight renders, queue depth, browser generation |
| `POST /_purge?url=<target>` | Drop one URL from the cache |

Response headers on a render: `X-Prerender-Cache` (`HIT` / `MISS` /
`COALESCED`), `X-Prerender-Tier`, `X-Prerender-Render-Time-Ms`, `X-Request-ID`.

## Inspecting the cache

Keys are the normalized target URL, not a hash, so the whole cache is readable:

```sh
redis-cli --scan --pattern 'prerender:html:*'
redis-cli GET  'prerender:html:https://www.sbisecurities.in/about-us'
redis-cli TTL  'prerender:html:https://www.sbisecurities.in/about-us'
redis-cli HGETALL 'prerender:meta:https://www.sbisecurities.in/about-us'
```

HTML is stored uncompressed on purpose. Compressing would roughly halve memory
use but make every value unreadable from `redis-cli`.

Set a memory ceiling on Redis so the cache cannot push the box into swap:

```
maxmemory 1gb
maxmemory-policy allkeys-lru
```

## Readiness

The page cannot signal when it is done, so readiness is inferred, in this order:

1. `network_idle` — document complete, zero in-flight requests, quiet for `NETWORK_IDLE_MS`
2. `dom_stable` — document complete, `outerHTML` length unchanged over `STABILITY_CHECKS` polls
3. `max_wait` — `MAX_WAIT_MS` ceiling reached; returns what is there
4. `degraded` — render timed out or errored; returns partial HTML, **never cached**

Watch `ready_tier` in the logs. Mostly `network_idle` means the tuning is right.
A lot of `max_wait` means something on the page never goes quiet — raise
`MAX_WAIT_MS` or add the offending host to `BLOCKED_URL_PATTERNS`.

## What is never cached

Degraded renders, non-2xx upstream responses, and anything smaller than
`MIN_HTML_BYTES`. Serving a partial page once is unfortunate; caching it for
six hours is an SEO problem.

## Logs

Structured JSON on stdout via `log/slog`. Every line for a request carries
`request_id` and `url`. Filter a single request with:

```sh
journalctl -u prerender -o cat | jq -c 'select(.request_id=="a1b2c3d4e5f6")'
```

Key events: `request.start`, `cache.lookup`, `render.start`, `render.end`,
`cache.store`, `request.end`, plus `render.failed`, `cache.read_failed`,
`browser.recycle` and `panic`.
