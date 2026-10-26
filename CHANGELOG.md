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

### Changed
- `secondsToWindowEnd` no longer recomputes the platform's largest `int` on
  every rate-limited request: the value is computed once at init into
  `maxIntValue`.
