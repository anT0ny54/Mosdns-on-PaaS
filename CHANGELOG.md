# Changelog

## 0.5.15

### Modification log — 2026-11-13 (seventh pass)

#### Changed
- **Upstream URL validation now matches MosDNS v4.5.3.** `check-config.sh`
  previously accepted arbitrary URL schemes. Explicit schemes are now limited
  to `udp://`, `tcp://`, `tls://` and `https://`, which are the protocols
  handled by the pinned upstream implementation; this project's validator
  continues to require an explicit scheme even though MosDNS defaults a
  scheme-less address to UDP. Unsupported schemes such as `ftp://` and
  `http://` now fail during static validation.
- **Transport-specific no-op settings are rejected.** `enable_pipeline` is
  accepted only for TCP/DoT upstreams, and `idle_timeout` is rejected for UDP
  upstreams. Both options were previously accepted in combinations where
  MosDNS silently ignores them.
- **DoH media types must be unambiguous.** The gateway now requires exactly one
  `Content-Type: application/dns-message` value on POST requests and successful
  backend/readiness responses. Duplicate or conflicting values are rejected
  instead of trusting whichever value `Header.Get` returns first. Only HTTP
  optional whitespace (space and horizontal tab) is accepted around the value.
- **Invalid successful-backend headers are rejected before body reads.** A
  wrong or duplicate media type, a declared body larger than 4096 bytes, or a
  declared body shorter than the 12-byte DNS header now yields `502` without
  consuming the body or holding a processing slot until its timeout.
- **Readiness rejects impossible declared DNS lengths early.** `/readyz` now
  rejects a successful response whose declared `Content-Length` is between 0
  and 11 bytes before reading it, matching the public DoH data path. Unknown
  lengths remain supported and are validated while streaming. A regression test
  verifies that an undersized declared readiness body is closed without reads.
- **Canceled queue wait wins a timer race.** If a request context and the queue
  timer become ready together, the handler rechecks cancellation before writing
  `503` or incrementing the queue-timeout counter. A repeated regression test
  exercises the simultaneous-ready case.
- **Entrypoint sleep PID signaling is hardened.** On Linux, teardown verifies
  that `SLEEP_PID` still belongs to the entrypoint and is live before sending
  a signal, preventing a just-reaped PID from being reused by an unrelated
  process. The non-Linux fallback retains the existing `kill -0` behavior.
- **Readiness DNS names are compared label by label.** The DNS wire decoder
  now preserves label boundaries instead of joining labels with dots. This
  prevents a valid single label containing the text `ready.doh-gateway.invalid`
  from being mistaken for the expected three-label probe owner name.
- **Every entrypoint child uses the direct-child PID guard.** When `/proc` is
  readable, supervision and teardown verify that MosDNS, the gateway, and the
  supervision sleep are still live direct children before treating them as
  alive or sending signals. The documented `kill -0` fallback remains for
  systems without readable `/proc`. The redundant generic liveness helper was
  removed, eliminating inconsistent PID-reuse handling between child types.
- **Regression coverage updated.** Added an unusable-readiness-answer case for
  a single dotted DNS label and retained the existing startup, shutdown, and
  cross-shell entrypoint scenarios for the stricter child checks.
- **Entrypoint regression harness now reaps reliably.** `wait_exit` recognizes
  zombie entrypoint processes as exited before calling `wait`, avoiding false
  timeouts caused by polling `kill -0` alone. Missing-child tests verify the
  shell's missing-executable diagnostic and ensure any child that did start is
  gone; they no longer assume the other background child always wins a startup
  scheduling race.
- **README and regression coverage updated.** Added tests for the readiness
  Content-Length guard and queue cancellation/timer race, and documented the
  PID guard, early readiness rejection, and more reliable entrypoint tests.

#### Verified
- Re-read the complete 23-file merged project before making changes. The
  round-three overlay applied cleanly to the supplied main archive; no
  unexpected files or path conflicts were present.
- `gofmt`, shell syntax checks, `check-config.sh mosdns.yaml`, the 52-case
  configuration-validator regression, and all nine entrypoint scenarios under
  POSIX `sh` and BusyBox `sh` pass.
- `go vet`, `go test ./...`, `go test -race ./...`, and 25 repeated runs of
  the new dotted-label readiness regression pass in an isolated verification
  copy after aligning its Go version declaration with the locally installed
  Go 1.23.2. The submitted project still requires Go 1.27; that
  toolchain could not be downloaded in this offline environment, so these are
  compatibility checks, not a native Go 1.27 run. Docker is not installed,
  so an image build was not run.
- No further unused production paths were identified beyond the redundant
  generic process-liveness helper removed in this pass. The changes in this
  patch are limited to label-preserving readiness validation, consistent
  direct-child PID checks, regression coverage, and the related documentation.
