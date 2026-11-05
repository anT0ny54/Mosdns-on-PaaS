# Changelog

## 0.5.15

### Modification log — 2026-11-05

Fifth review pass on the 0.5.15 tree (every file read in full; version unchanged).

#### Fixed
- **A non-200 2xx backend answer was relayed to the client.** A DoH answer is
  `200 OK` (RFC 8484), but any 2xx with a valid DNS body was passed through.
  Such answers now return `502`, and the `/readyz` probe requires `200` too.
- **A background `sleep` outlived the entrypoint on shutdown.** Each supervision
  cycle left a `sleep 1` behind when a signal arrived mid-cycle. Teardown now
  cancels it; the PID is cleared after every normal wait, so a stale PID is
  never signalled.
- **A client that disconnected mid-upload got a 400.** The gateway wrote and
  counted a `400` no client could receive. Such a read now returns silently; a
  timed-out read still gets `400` with `Connection: close`.
- **`check-config.sh` accepted a readiness record in any DNS class.** The probe
  is a class `IN` query, so a `CH` record never answers it. The check now
  requires `IN`.
- **Documentation said the readiness record was `A`.** README and the
  `mosdns.yaml` header now say `IN A 127.0.0.1`, and the README states that
  `PORT` and `MOSDNS_DOH_URL` are exempt from the invalid-value rule.
- **`test-entrypoint.sh` cleanup could leave a process running.** The EXIT trap
  sent `TERM`, which the "stubborn" fake child ignores. It now uses `KILL`.
- **Graceful shutdown could expire before a default request finished.** A POST can
  spend 2 s reading its body, 100 ms waiting for a slot, and 2.5 s upstream (4.6 s
  total), exceeding the gateway's former 4 s drain timeout. The timeout is now 5 s,
  still within the entrypoint's 6 s gateway grace period. A regression test checks
  both bounds.
- **The config validator accepted listener user-info as part of a bind address.**
  Addresses such as `user:pass@127.0.0.1:8081` could be misread as loopback even
  though they are not valid `host:port` listener addresses. The validator now
  rejects user-info and has a regression case.
- **A non-200 2xx backend response could time out as 504.** The gateway read
  its body before rejecting the status, so a stalled `206` response inherited
  the body timeout. Status is now classified before reading the body: only
  `200` is accepted as a DoH answer; statuses outside standard `400`-`599`
  become immediate `502`, while standard 4xx/5xx retain their relayed status.
  Stalled-`206` and 6xx regression tests cover this.
- **The config validator accepted parser-normalized whitespace.** Python's URL
  parser can remove tabs and newlines before validation, accepting malformed
  listener and upstream addresses. Raw whitespace and control characters are
  now rejected, with listener and upstream regression cases.

#### Changed
- `test-check-config.sh` checks why each config is rejected (a diagnostic per
  case) and fails on any Python traceback, instead of accepting any nonzero exit.
- The readiness probe's loopback address is a shared value, so each probe no
  longer allocates one.
- Test helpers are shared rather than repeated: the throughput gate and the
  benchmark use one POST helper, and a `serveOnLoopback` helper replaces the
  duplicated listener setup.
- New regression case: a `206` backend answer is rejected with `502`.
- An `api` mapping without `api.http` is accepted because the API listener is
  optional; a supplied `api.http` address is still validated as loopback-only.
- Backend response bodies are closed exactly once. The early close remains before
  releasing the processing slot, preserving connection reuse before the client
  response write; the deferred close is now only a fallback.
- The `/dns-query` endpoint table now distinguishes valid `200` DNS answers,
  relayed upstream `4xx`/`5xx` statuses, and gateway-generated `502` responses.
- `main_test.go` is now `gofmt`-clean (the previous check found one missing blank
  line).
