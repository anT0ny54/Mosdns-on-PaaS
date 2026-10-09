# Changelog

## 0.5.7 — 2026-10-10

Follow-up audit fixes (container supervision + test correctness).

### Fixed
- `entrypoint.sh`: the supervision loop polled `kill -0` on a 1s sleep, but
  `kill -0` succeeds on a zombie, so an exited mosdns/doh-gateway was never
  detected and never reaped — the container hung until the platform health
  check killed it and graceful shutdown never ran. The loop now blocks in
  `wait -n`, which reaps the exited child immediately (so `kill -0` on it
  correctly fails afterwards) and reacts to a child death instantly instead
  of up to 1s late. `tini` is now PID 1 as well: it forwards signals to the
  entrypoint and reaps processes reparented to PID 1.
- `main_test.go`: `TestReadyEndpointHonorsRequestContext` left
  `readyCacheTTL` at its zero value, so `ready()` took the non-caching
  early-return path and the `readyAt.IsZero()` assertion passed without the
  cache-poisoning guard (`ctx.Err() == nil`) ever executing. The test now
  sets `readyCacheTTL = defaultReadyCacheTTL` so the code path under test is
  actually exercised.

### Changed
- Dockerfile: `apk add tini` + `ENTRYPOINT ["/sbin/tini", "--", ...]`;
  version bumped to 0.5.7 (`VERSION` and `ARG GATEWAY_VERSION` changed
  together, as `check-config.sh` requires).

### Evaluated and rejected (unchanged from 0.5.6 audit)
- `responses3xxTotal` stays exposed in `/metrics` for Prometheus schema
  completeness even though the gateway never emits 3xx.
- `enable_pipeline: true` in `mosdns.yaml` stays: MosDNS v4.5.3 only applies
  it to TCP/DoT upstreams, and `check-config.sh` requires the key.
- The double `resp.Body.Close()` in `serveDNS` stays: the explicit close
  returns the connection to the pool before the slow client write, the defer
  is a safety net, and double-close is documented as safe.
- `connectionHeaderTokens`, `extractClientIP`, and `rateLimitKey`
  micro-allocations were measured as negligible; left as is.
