# Changelog

## 0.5.15

### Modification log — 2026-11-13 (second pass)

#### Changed
- **`test-entrypoint.sh` cleanup no longer signals unrelated processes.** The
  EXIT trap sent `KILL` to every PID recorded in the test's pid files. A PID
  can be reused after its fake child has exited, so cleanup could have
  killed an unrelated process. Cleanup now signals a recorded PID only while
  that process still carries this run's `FAKE_DIR` in its environment
  (`exec` keeps the environment, so the stubborn fake is covered too).

#### Verified (no code change needed)
- Current tree re-read in full; the only files that differ from the
  previous pass are `main.go`, `README.md` and `CHANGELOG.md`.
- Full suite passes: `go vet`, `go test -race`, `check-config.sh` and its
  regression cases, and `test-entrypoint.sh` under `sh` and BusyBox ash.
- Missing child binaries (MosDNS or gateway not found) make the entrypoint
  exit with status 1 promptly and leave no child running.
- Allocation profile of the DoH POST path: the gateway's own code makes one
  small allocation per request (the status recorder) plus the required body
  copy; the rest is `net/http`. No further code optimization is worthwhile.
- Dead-code scan (every top-level Go and Python identifier): nothing
  unreferenced outside the test functions the runner calls by name.
