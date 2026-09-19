# Changelog

## 8.2.8 — 2026-09-20

### Fixed
- Removed the dead `DOH_PATH_ESCAPED` shell variable: `render_config()` now
  escapes `DOH_PATH` inline, matching how every other substitution is handled.
- The health supervisor now logs a one-time warning when the `mosdns-probe`
  binary is missing, instead of silently skipping every health check cycle.

### Improved
- `mosdns-probe` now shares a single `http.Client` across all probes and uses
  per-request context timeouts, allowing keep-alive connection reuse instead
  of a fresh TCP+TLS handshake per probe on the 0.1 vCPU instance.
- Simplified the probe's goroutine result collection: results are written
  directly into a pre-sized slice instead of passing through a channel.

---

## 8.2.7 — 2026-09-19

### Security
- Fixed proxy-to-MosDNS client-IP forwarding: clear the inbound
  X-Forwarded-For header and replace the server-side peer address with the
  validated client IP before ReverseProxy forwards the request, so MosDNS
  receives exactly one trusted X-Forwarded-For value.

---

## 8.2.6 — 2026-09-19

### Security
- Hardened X-Forwarded-For parsing so only the final Koyeb-appended hop is
  trusted; malformed final hops now fall back to the TCP peer instead of
  trusting an earlier header value.

---

## 8.2.5 — 2026-09-19

### Security
- Hardened public-client IP detection: use the last valid `X-Forwarded-For`
  hop (the Koyeb-certified edge value) instead of trusting client-supplied
  `X-Real-IP` or the first XFF value.

---

## 8.2.4 — 2026-09-19

### Fixed
- Fixed fixed-custom-upstream health selection so the custom endpoint keeps
  `UPSTREAM_1` and `UPSTREAM_2` as its two built-in fallbacks. This removes a
  gap where `UPSTREAM_2` was never probed or selected in arbitrary
  fixed-endpoint mode.

---

## 8.2.3 — 2026-09-19

### Fixed
- Fixed the core resolver path so upstream failover is truly sequential on the
  same DNS query: upstream 1 is attempted only after upstream 0 returns an
  exchange error, and upstream 2 only after upstream 1 fails.
- Replaced the previous nested v4.5.3 fallback construction with a small
  `sequential_forward` executable added to the existing `fast_forward`
  package, avoiding the parallel behavior of `fast_forward` and the
  non-immediate semantics of a fallback node with `fast_fallback=0` and no
  status tracking.
- Avoided pointless health-triggered restarts when every probed upstream is
  unhealthy; the active process is retained until a healthy alternative exists.

### Hardening
- Hardened runtime configuration validation: public/backend ports must be
  valid TCP ports and different from each other; `HEALTH_PATH` and `DOH_PATH`
  must differ; YAML-interpolated string values are restricted to printable
  ASCII and URL/path values are single-quoted safely.
- Rejected query, fragment, and space characters in the HTTP paths so proxy
  routing cannot silently disagree with MosDNS' `url_path`.

---

## 8.2.2 — 2026-09-19

### Fixed
- Reduced each health interval from two identical probe passes to one probe cycle.
- Fixed active-upstream tracking: the supervisor now records the upstream that
  was active **before** probing instead of reading an order mutated by the
  probe itself.
- Fixed shell process-state loss: the health supervisor now runs in the main
  shell, so a successful MosDNS replacement updates the PID monitored by the
  parent process instead of leaving it watching the terminated child.
- Fixed fixed-endpoint mode so the custom `HAGEZI_UPSTREAM` is included in
  every runtime health cycle and remains in the fallback chain after a
  reorder; built-in/custom candidates are also de-duplicated when the fixed
  URL is one of the built-ins.
- Wired the probe helper's `HEALTH_ACTIVE_UPSTREAM` support into the
  supervisor, so `HEALTH_SWITCH_MARGIN_PCT` and `HEALTH_SWITCH_MARGIN_MS` are
  now the single source of truth for healthy-upstream hysteresis.
- Prevented `random` mode from undoing a hysteresis decision that
  intentionally keeps the current healthy upstream first.
- Replaced the old start-new-before-stop swap sequence with stop-then-start.
  MosDNS v4.5.3 opens the HTTP listener with a normal `net.Listen`, so
  overlapping processes can contend for the same backend port. The
  health-triggered restart therefore has a brief interruption, while startup
  failure causes the container to exit for a normal instance restart.

### Added
- Added an explicit `HEALTH_BACKEND_TIMEOUT_MS=1000` image default and passed
  it through `entrypoint.sh` to the DoH proxy.

### Hardening
- Tightened `DOH_RATE_LIMIT` and `GLOBAL_RATE_LIMIT` validation so malformed
  decimals fail fast instead of being silently replaced by the proxy's
  compiled-in default.

### Removed
- Removed the unused `HEALTH_PATH_ESCAPED` shell variable.

---

## 8.2.1 — 2026-09-19

### Fixed
- Reverted the build-stage Go bump from 8.2.0
  (`golang:1.19-alpine3.17` → `golang:1.26-alpine3.24`). It broke the build:
  MosDNS v4.5.3 transitively depends on `github.com/lucas-clemente/quic-go
  v0.30.0` (pulled in by the built-in `forward` plugin even though this config
  only uses `fast_forward`), and that quic-go version has a deliberate
  compile-time guard refusing to build on Go 1.20+. The build stage is back
  on `golang:1.19-alpine3.17`; upgrading past Go 1.19 here requires replacing
  or vendoring that dependency first. The runtime stage (`alpine:3.24`, no Go
  toolchain) remains current.

---

## 8.2.0 — 2026-09-19

### Changed
- Consolidated the README into a single internally consistent document after
  several superseded revisions had drifted into conflicting defaults and
  upstream-rotation descriptions.
- Bumped the runtime-stage Alpine image (`alpine:3.22` → `alpine:3.24`).
- De-duplicated `entrypoint.sh` config rendering into one `render_config()`
  path for startup and health-supervisor swaps. This also fixed a latent
  custom-upstream bug where the swap path could leave a blank `dial_addr:`
  field in the generated MosDNS config.
- Made no functional/plugin changes to the MosDNS configuration itself.
