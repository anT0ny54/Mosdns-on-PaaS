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
tag. This pass also made GET Base64URL validation canonical without allocations,
made large-window Retry-After rounding overflow-safe, tightened sequence/cache
configuration checks, corrected the README's supervision-poll interval, and
refreshed the pinned Go gateway and Alpine runtime images. A further
full-project pass (every file read in full) fixed a GET read-deadline conflict
with raised timeouts and removed avoidable per-request and per-header
allocations. The project version remains 0.5.14.

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
- **A canonical raw Base64URL GET value could be represented by malformed trailing bits.** GET validation now rejects non-zero unused low bits in the final character without decoding or adding allocations.
- **Very large but valid `RATE_WINDOW` durations could overflow `Retry-After` rounding.** The calculation now rounds without overflowing and clamps to the largest `int` on 32-bit targets.
- **A `GET /dns-query` could lose its answer when `QUEUE_WAIT` or `UPSTREAM_TIMEOUT` was raised.** Only the POST path cleared the connection's read deadline after reading the request. For a GET the server's 5 s `ReadTimeout` stayed armed while the handler ran; when it expired the server's background read canceled the request context, so a request still waiting on MosDNS (queue wait + upstream time above about 5 s) was abandoned and the gateway returned without writing the answer, contradicting the documented behavior that the write deadline and timeouts scale together. A GET now clears the read deadline once its parameter is validated, exactly as a POST does after its body. Covered by `TestGETUpstreamWaitOutlivesServerReadTimeout`.

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
- The MosDNS sequence checker now requires exactly one fallback immediately
  after the cache, rejects unsupported fallback keys and non-boolean
  `cache_everything` values, and validates optional `stat_length` and
  `threshold` types/ranges. The allowlist includes all v4.5.3 fallback fields,
  so valid `stat_length` and `threshold` settings are not falsely rejected.
- Added config-checker regression tests for valid optional fallback settings,
  unsupported keys, non-boolean cache settings, negative or mistyped fallback
  fields, and extra sequence steps; these run as part of `make test`.
- The gateway builder now pins Go 1.27.3 and the runtime pins Alpine 3.24.3,
  both by their multi-architecture index digests.
- README supervision timing now distinguishes the once-per-second crash check
  from the 0.2-second polling used while stopping a child.
- The rate limiter is keyed by `netip.Addr` instead of a formatted string, so
  admitting a request no longer allocates a key string. Grouping is unchanged
  (IPv4 by address, IPv6 by /64, clients whose address cannot be determined
  share one bucket); the limiter tests use `netip.Addr` keys.
- Relaying backend response headers no longer lowercases every header name
  (two allocations per header on every response): hop-by-hop headers are
  matched case-insensitively without allocating, and `Connection` tokens are
  stored and looked up in canonical form. Which headers are dropped is
  unchanged.
- `serveDNS` takes one pooled buffer for both the upstream error-body path and
  the 2xx answer instead of acquiring it separately in each branch.
- `test-check-config.sh` falls back to `python` when `python3` is absent, like
  `check-config.sh` does, so the two scripts accept the same interpreters.
