# Changelog

## Unreleased

Review pass over the whole project (code, scripts, Dockerfile, CI, docs). No
runtime change: `main.go`, `entrypoint.sh`, the Dockerfile, `mosdns.yaml`,
`check-config.sh`, `VERSION` and the Go tests are untouched, so the image
behaves exactly as 0.5.11. To release this as 0.5.12, bump `VERSION` and the
Dockerfile's `ARG GATEWAY_VERSION` together (`check-config.sh` requires it) and
rename this heading.

### Changed
- `test-entrypoint.sh` now also checks, after every scenario, that neither fake
  child is still running. Before, it only compared the entrypoint's exit code,
  so it could not detect the entrypoint exiting 1 while leaving the other
  child alive, or skipping the `KILL` escalation for a child that ignores
  `TERM`, even though the README and the 0.5.11 notes described both as
  checked. Verified by mutation: removing the `KILL` step and removing the
  "stop the other child" step each make the test fail (the second mutation
  passed the old test).
- CI: `actions/checkout` is `@v5` in all three jobs (the Keep-Alive workflow
  already used it), and the two `actions/setup-go` steps set `cache: false`.
  The module has no dependencies and no `go.sum`, so the cache step could only
  warn that it had nothing to key on.

### Docs
- README: the write deadline is described correctly. It is the full request
  budget (5 s body read + `QUEUE_WAIT` + `UPSTREAM_TIMEOUT` + 2 s) with an 8 s
  floor, which is 9.6 s with the default settings; the old wording implied 8 s
  was the default. The code was already right.
- README: the entrypoint test description lists what is actually asserted
  (exit code and no surviving child).
- README: new "Repository automation" section for the Keep-Alive workflow and
  `keep-alive.txt`, which were undocumented, and the CI cache setting.

### Reviewed and left as-is
- **Dead code:** none found. Every function and constant in `main.go` is used.
  The nil-client fallbacks in `backendReady` are reached by tests that build a
  gateway without a `readyClient`; the per-gateway `maxRequestBytes` and
  `maxResponseBytes` fields are set by the tests; the always-zero `3xx` series
  and the double `resp.Body.Close()` stay for the reasons given under 0.5.11.
- **Config and version conflicts:** none. `check-config.sh` passes, and the
  Go version (`go.mod`, Dockerfile, CI), `VERSION` and the Dockerfile's
  `GATEWAY_VERSION`, the `MOSDNS_DOH_URL` port and path, and the timeout chain
  (400 ms < 2 s < 2.5 s) all agree.
- **Client-IP fallback (behavior to be aware of):** with `CLIENT_IP_HEADER`
  set, a request that has no valid value in that header falls back to
  `X-Real-IP`, which a client can forge when the container is reachable without
  the trusted proxy. Behind the proxy this cannot be triggered, because the
  proxy always appends a valid address. The behavior is documented, covered by
  `TestClientIPHeaderInvalidFallsBackToRealIP`, and unchanged.
- **Rate-limiter key cap:** once `RATE_LIMIT_CLIENTS` distinct keys are in a
  window, new clients are denied until the window rolls over. This is the
  documented trade-off for bounded memory.

### Not verified in this review
- The Go test suite, `go vet` and the Docker build were not run (no Go
  toolchain or network in the review environment). `check-config.sh` and
  `test-entrypoint.sh` were run and pass under `dash`; neither was run under
  BusyBox ash or in CI.

## 0.5.11 — 2026-10-14

Supervision fix: the entrypoint now notices a child killed by a signal, a
readiness-probe metrics fix, and consistency checks for the Go version.

### Fixed
- **The entrypoint did not detect a child killed by a signal.** It waited with
  `wait -n`, which BusyBox ash does not return from when a child dies from a
  signal (OOM kill, SIGSEGV, SIGKILL). If MosDNS was killed, the container kept
  running with only the gateway and nothing restarted it. The loop now polls
  both children once per second and treats an exited, killed or zombie process
  (state read from `/proc`, `kill -0` only if `/proc` is missing) as gone; it
  then stops the other child and exits 1, as before. Shutdown no longer uses a
  background watchdog subshell (which could add up to 15 s when the child was
  already dead): each child gets `TERM`, is polled every 0.2 s for its grace
  period (gateway 15 s, MosDNS 5 s), is `KILL`ed if still alive, and is reaped.
- **`/readyz` counted a probe that the client abandoned.** When the client
  disconnected mid-probe, the aborted probe was added to
  `doh_gateway_ready_probe_total{result="not_ready"}` and a `503` was written
  and counted for a client that was gone. A canceled probe is no longer counted
  as a failure and `/readyz` returns without writing a response in that case.
  Real failures (unreachable backend, bad status, wrong content type, invalid
  answer, probe timeout) are counted as before. Regression test:
  `TestReadyProbeCanceledByClientIsNotCounted`.

### Added
- `test-entrypoint.sh` (`make entrypoint-test`, part of `make test` and CI): runs
  the real `entrypoint.sh` against fake children and checks TERM shutdown
  (exit 0), a SIGKILLed MosDNS, a SIGKILLed gateway, a child that exits on its
  own (each exit 1), and KILL escalation for a child that ignores TERM.
- `entrypoint.sh` reads optional `MOSDNS_BIN` and `GATEWAY_BIN` (defaults are
  the installed paths) so the test can substitute fake children.
- Startup warning when `MAX_CONCURRENT_REQUESTS` is greater than
  `MAX_ACTIVE_REQUESTS`: requests are rejected before they can queue, so
  `QUEUE_WAIT` never applies. Behavior is unchanged.
- `check-config.sh` verifies that the Go version in `go.mod`, the Dockerfile's
  `GATEWAY_GO_VERSION` and every `go-version` in `.github/workflows/ci.yml`
  agree. They were documented as "keep in sync" but not checked.

### Changed
- The two identical "bad upstream request" paths in the gateway share one
  helper (`badUpstreamRequest`); the response (`502`, short text body) and the
  backend-error count are unchanged.
- CI declares `permissions: contents: read` and cancels runs superseded by a
  newer push to the same ref.
- `.dockerignore` also excludes `test-entrypoint.sh`, the `doh-gateway` binary
  and `.env` from the build context.
- `.gitattributes` forces LF for the Dockerfile, Makefile, `VERSION` and
  `.dockerignore`, so a Windows checkout cannot break `cat VERSION` or `make`.
- The tini comment in the Dockerfile now sits next to `ENTRYPOINT` and
  describes the polling supervision.
- `VERSION` / `ARG GATEWAY_VERSION` bumped to 0.5.11 (together, as
  `check-config.sh` requires).

### Docs
- README: entrypoint supervision (polling, signal/zombie detection, grace
  periods, `MOSDNS_BIN`/`GATEWAY_BIN`), `/readyz` cancellation and metrics,
  the capacity warning, the new `check-config.sh` Go-version check, the
  entrypoint test, the CI settings, and the 0.5.11 version.

### Evaluated and left as-is
- No other production code was removed; the remaining apparent redundancy is
  load-bearing or deliberate: the always-zero `3xx` response series (keeps the
  metric set stable), the double `resp.Body.Close()` (early release plus safety
  net), and `maxRequestBytes` / `maxResponseBytes` (effectively constants, both
  set to the DNS message limit).

