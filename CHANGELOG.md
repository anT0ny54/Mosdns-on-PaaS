# Changelog

## 0.5.9 — 2026-10-14

Audit follow-up: buffer-pool race on upstream errors fixed properly, write
deadline now covers the body-read budget, backend latency measured end-to-end,
MosDNS source commit pinned, CI added.

### Fixed
- **P1: the POST buffer pool could be reused too early after an upstream
  error.** The error path assumed the RoundTripper contract guarantees the
  transport is done with the request body when `Client.Do` returns; Go
  documents the body may be closed asynchronously. The outgoing POST body is
  now wrapped in a close-signaling `pooledRequestBody`, and the pooled buffer
  is only returned to `dnsMessageBufferPool` after the transport's close has
  been observed. Every exit path funnels through `returnBodyBuffer`, which
  waits on the close signal; the wait is deadlock-free because the transport
  always closes the request body eventually, on success and on error. The
  zero-copy optimization is retained. Regression test:
  `TestPooledBodyHeldUntilTransportCloses` (a RoundTripper that delays closing
  the request body must delay the handler); the whole suite also runs under
  `-race` in CI.
- **P1: custom timeouts could drop the connection instead of the promised
  503/504.** `serverWriteTimeout` computed `max(8s, QUEUE_WAIT +
  UPSTREAM_TIMEOUT + 2s)`, but the write deadline starts when the request
  headers are read, so a slow POST body consumed part of the budget already.
  It now computes `max(8s, READ_TIMEOUT + QUEUE_WAIT + UPSTREAM_TIMEOUT + 2s)`.
  Covered by `TestServerWriteTimeoutIncludesReadBudget` and
  `TestWriteTimeoutCoversSlowBodyAndRaisedTimeouts` (a 2.5s slow upload plus a
  2s upstream timeout must still end in `504`, not a dropped connection).
- **P2: backend latency recorded response-header time, not full response
  time.** A backend returning headers quickly but stalling or truncating the
  body looked healthy in the average-latency gauge. `backendLatencySamples` /
  `backendLatencyNanos` are now updated only after the response body has been
  fully read; timeouts, header failures and body failures no longer
  contribute. Covered by `TestBackendLatencyRecordedAfterBodyRead`.

### Changed
- Dockerfile: `MOSDNS_COMMIT` is now mandatory and defaults to
  `760a660192fdf996463b024d1d0e19bd66b6ed31`, the exact commit behind the
  `v4.5.3` tag. The build fails when the pin is unset and when the tag
  resolves to a different commit, so an unexpected tag change can no longer
  silently produce a different binary.
- Added `.github/workflows/ci.yml`: `make test` (Go + Python/PyYAML), a
  separate `go test -race ./...` job for the concurrency-sensitive paths
  (buffer pool, limiter, queue), and a `docker build` job, on every push to
  `main` and every pull request.

### Docs
- README: the Architecture write-deadline formula now includes the 5 s
  body-read budget; the `/metrics` table documents end-to-end backend latency;
  the `UPSTREAM_TIMEOUT` row, the Build and test section (race gate + CI), and
  the Version section (mandatory commit pin, 0.5.9) reflect the new reality.
- `VERSION` / `ARG GATEWAY_VERSION` bumped to 0.5.9 (together, as
  `check-config.sh` requires).

### Evaluated and left as-is
- Separate counters for response-header vs response-body failures were
  considered and not added: moving the latency sample to after the body read
  removes the misleading signal those counters would have diagnosed.
- All 0.5.8 "evaluated and left as-is" items remain unchanged (double
  `resp.Body.Close()` as early-reuse + safety net, `responses3xxTotal` for
  schema stability, `connectionHeaderTokens` nil fast-path, single-mutex
  limiter under GOMAXPROCS=1, entrypoint `wait -n` supervision).

