# Changelog

## 0.5.6 — 2026-10-10

Full-project audit (dead/redundant code, cross-file conflicts, bugs,
optimizations) with all findings corrected.

### Fixed
- Backend 2xx responses are now rejected with `502` when the body is smaller
  than the 12-byte DNS header. The request side always enforced the 12-byte
  minimum; the response side checked only the 4096-byte maximum and the
  `application/dns-message` content type, so a 0-11 byte 2xx body was relayed
  as-is with status 200.
- `/readyz` ignored the caller's request context: `ready()` always probed
  with `context.Background()`, so a disconnecting client could not cancel an
  in-flight probe. The probe now runs under the request context, and a result
  is only cached while that context is still live, so a client-triggered
  cancellation can no longer mark the backend unhealthy for the failure TTL.
  Probes remain serialized on the readiness mutex by design.

### Changed
- `fixedWindowLimiter` reuses its count map across window rollovers via
  `clear()` instead of allocating a new map per window.
- `serveMetrics` emits the exposition with `fmt.Fprintf` into the existing
  `strings.Builder` instead of repeated string concatenation; output is
  byte-identical.
- `.gitattributes`: dropped the Python/TypeScript/JavaScript rules; the repo
  contains none of those file types.
- Version bumped to 0.5.6 (`VERSION` and Dockerfile `ARG GATEWAY_VERSION`
  changed together, as `check-config.sh` requires).

### Evaluated and rejected
- Reusing a pooled buffer for the upstream POST body instead of
  `append([]byte(nil), body...)` would let the HTTP transport keep reading a
  buffer that another request already recycled — the transport can outlive
  the handler. The fresh copy stays.

### Tests
- `main_test.go`: stub backends that answered success with 1-2 byte bodies
  now answer with the 12-byte `testDNSBody`; added
  `TestShortBackendResponseRejected` (short 2xx -> 502) and
  `TestReadyEndpointHonorsRequestContext` (cancellation aborts the probe and
  never poisons the cache).
- `throughput_test.go`: the benchmark harness backend answers with a 12-byte
  body so the 200 path it measures still passes the new validation.

## 0.5.5 and earlier

No changelog was kept before 0.5.6; see the git history and `README.md` for
the state of earlier releases.
