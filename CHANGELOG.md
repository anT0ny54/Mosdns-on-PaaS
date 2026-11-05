# Changelog

## 0.5.15 — 2026-10-26

Full-project review pass (every source, test, script, workflow, metadata and
documentation file read in full). This entry records only the changes made in
this pass.

### Fixed
- **`statusRecorder` forwarded duplicate `WriteHeader` calls.** Relayed
  upstream error responses copy the backend's headers and then write the
  gateway's own status; because the recorder passed every `WriteHeader` through
  even after the first one, `net/http` logged "superfluous
  response.WriteHeader" for each such response. The recorder now swallows
  duplicates; the first status written still wins and remains the counted one.

### Modification log — 2026-11-05

Third review pass on the 0.5.15 tree (every file read in full; version unchanged).

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

#### Changed
- `test-check-config.sh` checks why each config is rejected (a diagnostic per
  case) and fails on any Python traceback, instead of accepting any nonzero exit.
- The readiness probe's loopback address is a shared value, so each probe no
  longer allocates one.
- Test helpers are shared rather than repeated: the throughput gate and the
  benchmark use one POST helper, and a `serveOnLoopback` helper replaces the
  duplicated listener setup.
- New regression case: a `206` backend answer is rejected with `502`.
