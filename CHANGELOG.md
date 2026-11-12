# Changelog

## 0.5.15

### Modification log — 2026-11-13

Merged-tree review and hardening (every project file read in full; project version unchanged).

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

- **The readiness probe accepted incomplete or extended DNS answers.** It now
  requires exactly one answer record, no authority/additional records, and no
  trailing bytes after the loopback A record. Regression cases cover each form.
- **Known-oversized backend bodies could occupy a worker until timeout.** The
  gateway now rejects a declared oversized success answer immediately; for
  upstream 4xx/5xx it preserves the status and useful headers while dropping
  oversized representation data without waiting for the body.
- **Backend URLs accepted unsafe or unusable authority components.** Startup now
  rejects URL user-info, all fragments (including an empty trailing `#`), empty
  explicit ports, and ports outside 1-65535, preventing accidental credential
  forwarding and URLs that can only fail when a request is dialed.
- **The config validator could crash on a malformed fallback tag.** A list or
  mapping tag was used in a set lookup before its type was checked, producing a
  Python traceback. Fallback tags are now checked as non-empty strings first.
- **The config validator silently accepted duplicate YAML keys.** PyYAML's
  default safe loader uses the last value, which can hide conflicting settings.
  The validator now rejects duplicate explicit mapping keys while keeping safe
  loading and YAML merge overrides.
- **MosDNS upstream URL checks were weaker than gateway backend URL checks.**
  The validator now rejects upstream user-info, all fragments (including an
  empty trailing `#`), and explicit empty ports; regression cases cover each.
- **Runtime backend URL validation did not reject raw whitespace consistently.**
  `newGateway` now rejects raw whitespace/control characters before parsing,
  matching the configuration validator and preventing ambiguous URL handling.
- **An oversized declared readiness response was read before rejection.** The
  `/readyz` probe now rejects `Content-Length` values above the DNS message
  limit immediately and closes the response without reading its body.
- **Readiness accepted DNS responses with an invalid opcode or reserved flag.**
  The probe now requires the standard QUERY opcode and a clear reserved Z bit,
  with regression cases for both malformed headers.
- **The Dockerfile cross-check disagreed with gateway startup validation.** It
  accepted backend user-info and fragments, allowing `make check-config` to pass
  a configuration that the gateway would refuse at startup. The checker now
  rejects user-info, fragments (including an empty `#`), and explicit empty
  ports, matching the Go constructor.
- **The readiness-record check accepted trailing garbage.** It now requires the
  complete five-field `IN A` record instead of accepting any record whose first
  five tokens looked correct.

- **Duplicate readiness A records passed static validation but failed runtime
  readiness.** The config checker now requires exactly one matching `IN A`
  record for the probe name, aligning its result with `/readyz`'s strict one-answer
  DNS parser; a duplicate-record regression fixture covers this case.
- **A backend answer completing at client cancellation could still be counted.**
  The gateway now checks the request context after the response is received and
  after a successful DNS body read, so a disconnect at EOF writes/counts no
  response and contributes no backend-latency sample. A deterministic transport
  regression covers the cancellation-at-EOF race.

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

- The config validator now rejects unreferenced plugin definitions and duplicate
  fallback plugin references; regression fixtures cover both cases.
- Each config validation now starts Python only once (the interpreter-version
  check runs inside the validator), reducing repeated process overhead in the
  configuration regression suite.
- Duplicate local-answer plugin and EDNS-filter references are rejected as
  redundant; regression coverage now also exercises malformed fallback tags,
  duplicate YAML keys, readiness-record tails, and Dockerfile backend URL parity.
- Docker's gateway builder and Alpine runtime pins were refreshed to Go 1.27.4
  and Alpine 3.24.4 patch releases; MosDNS remains on its required Go 1.19 base.
- **`MOSDNS_DOH_URL` whitespace was silently trimmed before validation.** The
  environment loader now preserves every non-empty value verbatim, so leading
  and trailing whitespace/control characters are rejected; an unset or empty
  value still selects the default backend.
- **Backend URL scheme handling disagreed between Go and the config checker.**
  Go now treats HTTP(S) schemes case-insensitively and normalizes them to
  lowercase, matching the validator and URI rules.
- **Backend URL secrets could appear in startup logs and validation errors.**
  Validation messages no longer echo the supplied URL, and the startup log
  reports only the backend host instead of its path or query string.
- **C1 control characters were not rejected consistently.** Go and Python now
  reject all Unicode control characters, including non-whitespace C1 values;
  the Dockerfile backend URL receives the same raw-character check.
- **Malformed UTF-8 could produce a Python traceback in config validation.**
  The validator now reports a clear file-encoding error for the YAML and the
  cross-checked project files.
- Makefile `VERSION` and `BUILD_DATE` values are lazily evaluated, avoiding
  unnecessary `cat` and `date` subprocesses for targets that do not build.
- **Body-abandonment regressions depended on a 500 ms wall-clock threshold.**
  Non-200 2xx, declared-oversize and redirect response tests now use a
  deterministic transport; they assert that rejected bodies are never read,
  are closed exactly once, and that redirect headers are not relayed. This
  removes scheduler-dependent failures under race instrumentation.
