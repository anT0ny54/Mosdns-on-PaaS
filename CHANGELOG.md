# Changelog

## 0.5.14 — 2026-10-17

Fixes from a third full-project review: readiness probe sharing without a
result cache, proxy-trust hardening for the rate limiter, dead code removal
and metrics scaffolding. A follow-up review on 2026-10-26 fixed backend-status
edge cases, a readiness-cancellation race, the local Docker image tag, and the
keep-alive repository guard. A second pass hardened empty proxy-CIDR handling,
readiness answer validation and cancellation metrics, and discarded-body headers.
A final full-file review on 2026-10-26 merged duplicated response-validation
code, made the metrics templates compile-time constants, and hardened the
throughput harness against false 503s. A last read-everything pass (every
source, test, script and workflow file) corrected a changelog claim that did
not match the repository, removed a single-use helper and aligned the CI image
tag. The project version remains 0.5.14.

### Fixed
- **An upstream error status could be replaced by a gateway error.** The
  gateway read every response body before classifying its status, so an
  oversized or truncated body could turn a MosDNS `4xx`/`5xx` into `502` or
  `504`, and a stalled redirect body could turn `502` into `504`. Redirects
  are now rejected before reading their body; upstream `4xx`/`5xx` status is
  preserved even when its bounded body is unreadable or oversized.
- **A readiness probe could answer a canceled request.** A cancellation at the
  end of a successful probe could race with its verdict and result in a
  response being counted for a disconnected caller. The owner, waiters, and
  handler now re-check cancellation before returning or writing a response.
- **`make docker-build` defaulted to an invalid image reference.** Its mixed-case
  repository name was rejected by Docker; the default tag is now
  `mosdns-on-paas:local`, which uses a valid lowercase repository name.
- **`/readyz` with a disabled result cache re-probed per caller.** When
  `ReadyCacheTTL` was 0, waiters on a shared in-flight probe started their own
  probe instead of reusing the shared verdict. The probe now carries its
  verdict (or, when aborted by its own caller, an explicit "invalid" mark) so
  waiters share one probe regardless of the cache setting.
- **A direct client could bypass the rate limiter with spoofed headers.** With
  `CLIENT_IP_HEADER` set (the shipped Dockerfile sets `X-Forwarded-For`), any
  client that can reach the container directly — not only through the trusted
  proxy — could rotate spoofed addresses to get fresh buckets and eventually
  exhaust the per-window client cap (denying service to new clients). The new
  `TRUSTED_PROXY_CIDRS` variable restricts which socket peers may set the
  trusted header; when set, headers from any other peer are ignored. A value
  made only of commas (or comma-separated blank entries) now fails closed
  instead of silently meaning “trust every peer.”
- **A malformed readiness DNS answer could report the backend healthy.** The
  probe checked the DNS answer count but not the actual record. It now validates
  the DNS transaction ID in network byte order and requires a complete local
  `A 127.0.0.1` answer for `ready.doh-gateway.invalid`.
- **A canceled successful readiness probe could inflate the success metric.**
  Readiness results are now counted only after the probe owner confirms its
  context is still live; canceled probes increment neither result counter and
  are not cached.
- **An omitted upstream error body could leave stale representation headers.**
  When an oversized or unreadable `4xx`/`5xx` body is dropped, body-specific
  headers such as `Content-Type`, `Content-Encoding` and `Content-Range` are
  also removed while useful headers such as `Retry-After` are preserved.
- **The sustained-load benchmark could measure rejection and connection churn instead of gateway throughput.** It now uses a fixed worker count within the active-request ceiling, an effectively non-limiting test rate limit, and drains response bodies to preserve keep-alive reuse. The opt-in 5,000-RPS test also drains its responses. Its harness also sets a 5 s `QueueWait`: the 16 workers share 8 processing slots, and the 100 ms default could turn ordinary queueing on a slow or heavily loaded host into `503` responses and a false failure.

### Removed (dead code)
- The always-zero `3xx` response-status series (`responses3xxTotal` and its
  `doh_gateway_responses_total{class="3xx"}` output): the gateway never
  redirects and converts backend 1xx/3xx to `502`, so the counter could never
  be non-zero.

### Changed
- `serveMetrics` keeps its HELP/TYPE scaffolding in package-level `const`
  templates (scrape-path cleanup; output is unchanged apart from the removed
  `3xx` series). As constants, `go vet` now also checks their `Fprintf`
  argument counts.
- The three identical `502` rejection branches for a 2xx backend answer
  (oversize, too short, wrong content type) are one combined check, and the
  readiness query builder appends its fixed tail in a single call. Behavior and
  metrics are unchanged.
- The Dockerfile comment on pinned `ENV` values now says that `MOSDNS_CONFIG`
  repeats the entrypoint's default, not the gateway's.
- The keep-alive workflow now enforces its `anT0ny54/Mosdns-on-PaaS` repository
  guard at the job level, so forks are skipped even on manual dispatch. The
  whitespace-only line at the end of the file, which an earlier entry here
  wrongly reported as already removed, is now actually gone.
- The CI smoke-test image is tagged `mosdns-on-paas:ci` instead of the old
  `mosdns-koyeb-doh-gateway:ci`, matching the `mosdns-on-paas:local` tag
  `make docker-build` uses.
- The single-use `badUpstreamRequest` helper is inlined at its one call site
  (same `502` "bad upstream request" response and `backend_error` count), and
  the buffer pool's constructor uses `any` instead of `interface{}`. No
  behavior change.
