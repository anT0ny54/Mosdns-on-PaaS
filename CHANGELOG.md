# Changelog

## 0.5.13 — 2026-10-14

Fixes from a second full-project review: upstream racing, base-image updates,
cross-file drift, dead code and CI coverage.

### Fixed
- **The primary upstream race was half disabled.** MosDNS v4.5.3 trusts only
  the first upstream of a `fast_forward` and drops non-NOERROR answers
  (NXDOMAIN, SERVFAIL) from the others, so for those answers
  `wurzn.hagezi.org` could never win and queries waited for `root` or the
  400 ms fallback. `wurzn` is now `trusted: true`, and `check-config.sh`
  fails if any upstream after the first in a `fast_forward` is not trusted.
- **Dependabot never updated the base images.** It cannot read
  ARG-templated `FROM` lines (dependabot-core #2057), so every pinned digest
  stayed frozen. The `FROM` lines are now literal (tag + digest), and
  `dependabot.yml` ignores golang minor/major bumps, which keeps the MosDNS
  builder on Go 1.19 and leaves gateway Go minor bumps to a manual change
  that also updates `go.mod` and `ci.yml`.
- **The MosDNS server timeout was hard-coded in the startup warning.** It is
  now `mosdnsServerTimeout` in `main.go`, and `check-config.sh` requires it to
  equal `servers[0].timeout`.
- **`Retry-After` could be wrong at a window boundary.** The limiter read the
  clock twice (once outside the lock). `admit` now returns the decision and
  the `Retry-After` value from one clock reading, under the lock.
- **The write deadline used constants** instead of the gateway's own
  body-read, queue and upstream timeouts.
- **`X-Real-IP` was trusted as a fallback** although only `CLIENT_IP_HEADER`
  was configured. Only the configured header is trusted now; a missing or
  invalid value falls back to the socket peer.
- **README:** the readiness probe ID is incrementing, not random.

### Removed (dead code and config)
- `gatewayOptions.ReadyClient` and `gatewayOptions.BodyReadTimeout` (never set).
- The `X-Forwarded-For` header sent to MosDNS (MosDNS never read it);
  `check-config.sh` now rejects `get_user_ip_from_header`.
- The second bounded-read implementation: request, answer and readiness
  bodies all use `readDNSMessage` with the pooled buffer.
- From `mosdns.yaml`: `max_conns: 2` (the DoH default), `always_standby: false`
  and `log.file: ""` (defaults). `check-config.sh` validates `max_conns`,
  `idle_timeout` and `always_standby` only when present.
- Build args `MOSDNS_GO_VERSION`, `GATEWAY_GO_VERSION`, `ALPINE_VERSION` and
  the `*_DIGEST` args (replaced by literal `FROM` lines). `check-config.sh`
  reads the gateway Go version from the `gateway-builder` `FROM` line and
  checks that the `mosdns-builder` stage stays on Go 1.19.

### Changed
- CI runs the entrypoint test under BusyBox ash too (new
  `make entrypoint-test-busybox`) and smoke-tests the built image: `/readyz`,
  `/healthz`, a local DoH query, `404`, `build_info` version and a `docker
  stop` within 10 s.
- Tests: shared `postReq`/`okBackend`/`regGateway` helpers replace about 25
  copies of the request setup and the duplicate regression helpers; the
  metrics tests and the readiness-rejection tests are merged into one test
  each; the constant-only checks are dropped. New tests cover `Retry-After`
  for key-cap rejections and the untrusted `X-Real-IP`.
- `test-entrypoint.sh`: the TERM-ignoring fake child now `exec`s `sleep`, so
  a KILL no longer leaves an orphaned `sleep` behind.
- `entrypoint.sh` comment corrected (the loop forks one `sleep` per second);
  `make run` no longer repeats the default `PORT=8080`.

### Verified
`gofmt`, `go vet`, staticcheck (all checks) and deadcode are clean; `go test
-race` passes (83.2% coverage); the entrypoint test passes under dash and
BusyBox ash; `check-config.sh` passes and rejects each new failure case. The
gateway and the entrypoint were run against a real MosDNS v4.5.3 built with Go
1.19.13 and this `mosdns.yaml`: `/readyz` 200, the local DoH answer, a real
upstream query (repeat served from cache in under 1 ms), NXDOMAIN relayed,
clean stop in 0.4 s. Not verified here: the Docker build itself (no Docker in
the review environment); CI builds and smoke-tests it on push.

