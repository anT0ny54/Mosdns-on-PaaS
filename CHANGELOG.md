# Changelog

## 0.5.8 — 2026-10-14

Audit follow-up: docs synced with the 0.5.7 code, dead supervision code removed,
shutdown bounded, validator robustness, one hot-path allocation eliminated.

### Fixed
- README.md version reference still said 0.5.6 while `VERSION` and the
  Dockerfile's `ARG GATEWAY_VERSION` carried 0.5.7, and the Architecture section
  still described the pre-0.5.7 `kill -0` 1-second polling loop instead of the
  current `wait -n` supervision. Both corrected (now 0.5.8), and the liveness
  tradeoff is now stated: entrypoint supervision detects exit, not wedges, so
  platforms must probe `/readyz` (HTTP) to catch a wedged backend.
- `check-config.sh` error message said the timeout-chain check "would be
  skipped" when `defaultUpstreamTO` could not be read; the check in fact fails
  (exit 1). Message corrected.
- `check-config.sh` IPv6 edge case: `[fe80::1%eth0]:8081` kept the zone id in
  the hostname and failed the loopback check with a confusing message; the zone
  id is now stripped before matching.
- `check-config.sh` `UPSTREAM_TIMEOUT` parsing now also accepts `m` and `h`
  (in addition to `ms`/`s`), and the `main.go` fallback regex accepts
  `time.Minute`/`time.Hour`, matching every duration form the gateway itself
  would accept.

### Removed
- `entrypoint.sh`: the two `kill -0` pre-checks before `wait -n` in the
  supervision loop. They could never fire -- an exited child is an unreaped
  zombie at that point and `kill -0` succeeds on zombies, so the checks only
  passed in the normal case. `wait -n` is the reaper; the post-reap `kill -0`
  is the detector. Behavior unchanged.
- `check-config.sh`: the duplicate `Dockerfile.is_file()` call; the result is
  cached.
- `mosdns.yaml`: the `enable_pipeline: true` keys. They are a no-op for DoH
  upstreams in MosDNS v4.5.3 (TCP/DoT only), and `check-config.sh` now treats
  the key as optional (validated when present) instead of forcing a dead key
  into the config.

### Hardened
- `entrypoint.sh` shutdown: `terminate_children` waited unboundedly on MosDNS
  (which has no drain timeout), so a hung MosDNS hung the container with no
  escalation (no `timeout` command in the image). Each child now gets TERM
  plus a bounded grace period -- 15 s for the gateway (its drain timeout is
  10 s), 5 s for MosDNS -- enforced by a watchdog that escalates to
  `kill -KILL`, cancelled as soon as the child is reaped so a clean exit is
  not delayed.

### Changed
- Unknown gateway paths now return a plain-text `404 not found` via `writeText`
  instead of Go's HTML `http.NotFound` page, consistent with every other
  gateway-generated error and the `nosniff` header.
- `VERSION` / `ARG GATEWAY_VERSION` bumped to 0.5.8 (together, as
  `check-config.sh` requires).

### Optimized
- `serveDNS` no longer copies POST bodies (`append([]byte(nil), body...)`).
  The pooled request buffer is held out of the pool for the duration of the
  upstream exchange and returned after `resp.Body.Close()` (or on any error
  path), which is safe because closing the response body ends the transport's
  use of the request body. Removes the last hot-path allocation the buffer
  pool existed to avoid.
- `getenvBool` in `throughput_test.go` reads the environment variable once
  instead of twice.
- `TestActiveRequestCap` in `main_test.go` now sets `queueWait` explicitly, so
  a future handler-ordering change fails loudly instead of racing on
  `time.NewTimer(0)`.

### Evaluated and left as-is
- Double `resp.Body.Close()` in `serveDNS`: the explicit close returns the
  connection to the pool before the slow client write; the defer is a safety
  net and double-close is documented safe.
- `responses3xxTotal` stays exposed in `/metrics` for Prometheus schema
  stability even though the gateway never emits 3xx.
- `connectionHeaderTokens` allocates a map per relayed response, but only when
  a `Connection` header is present (nil fast-path otherwise); measured as
  negligible under GOMAXPROCS=1.
- The single-mutex `fixedWindowLimiter` is irrelevant with GOMAXPROCS=1 (one
  OS thread); not sharded, deliberately.

