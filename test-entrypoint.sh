#!/bin/sh
# Runs the real entrypoint.sh against fake mosdns/doh-gateway children and
# checks the supervision behavior. Usage: sh ./test-entrypoint.sh
# Set TEST_SHELL to run the entrypoint under another shell (e.g. busybox sh).
set -u

DIR=$(cd "$(dirname "$0")" && pwd)
SHELL_UNDER_TEST="${TEST_SHELL:-sh}"
TMP=$(mktemp -d)
trap 'kill $(cat "$TMP"/*.pid 2>/dev/null) 2>/dev/null; rm -rf "$TMP"' EXIT
fail=0

# Fake children record their PID, then behave as requested via $MODE files.
cat > "$TMP/fake" <<'FAKE'
#!/bin/sh
name=$(basename "$0")
echo $$ > "$FAKE_DIR/$name.pid"
mode=$(cat "$FAKE_DIR/$name.mode" 2>/dev/null || echo run)
case "$mode" in
  exit) sleep "${FAKE_EXIT_AFTER:-1}"; exit 3 ;;
  stubborn) trap '' TERM; while :; do sleep 1; done ;;
  *) trap 'exit 0' TERM; while :; do sleep 1 & wait $!; done ;;
esac
FAKE
chmod +x "$TMP/fake"
cp "$TMP/fake" "$TMP/mosdns"
cp "$TMP/fake" "$TMP/gateway"

run_entry() { # $1 = mosdns mode, $2 = gateway mode
  rm -f "$TMP"/*.pid
  echo "$1" > "$TMP/mosdns.mode"
  echo "$2" > "$TMP/gateway.mode"
  FAKE_DIR="$TMP" MOSDNS_BIN="$TMP/mosdns" GATEWAY_BIN="$TMP/gateway" \
    MOSDNS_DIR="$TMP/dir" MOSDNS_CONFIG="$TMP/cfg" \
    $SHELL_UNDER_TEST "$DIR/entrypoint.sh" >"$TMP/out" 2>&1 &
  ENTRY=$!
  i=0
  while [ ! -s "$TMP/mosdns.pid" ] || [ ! -s "$TMP/gateway.pid" ]; do
    i=$((i + 1)); [ "$i" -gt 50 ] && break; sleep 0.1
  done
}

wait_exit() { # $1 = max seconds; sets RC, or RC=timeout
  i=0
  while kill -0 "$ENTRY" 2>/dev/null; do
    i=$((i + 1))
    if [ "$i" -gt $(($1 * 10)) ]; then RC=timeout; kill -KILL "$ENTRY" 2>/dev/null; return; fi
    sleep 0.1
  done
  wait "$ENTRY"; RC=$?
}

check() { # $1 = name, $2 = expected rc
  if [ "$RC" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1 (rc=$RC, want $2)"; fail=1; fi
}

# After the entrypoint has exited, neither fake child may still be running.
# This is what proves "stops the other child" and "KILL after the grace period"
# (an exit code alone cannot). Polled briefly in case a process is still going
# away; the entrypoint reaps its children, so a finished one is not a zombie.
children_gone() {
  i=0
  while :; do
    alive=0
    for f in "$TMP/mosdns.pid" "$TMP/gateway.pid"; do
      p=$(cat "$f" 2>/dev/null) || continue
      if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then alive=1; fi
    done
    [ "$alive" = 0 ] && return 0
    i=$((i + 1)); [ "$i" -gt 20 ] && return 1
    sleep 0.1
  done
}

check_gone() { # $1 = name
  if children_gone; then echo "ok   $1: no child left running"; else echo "FAIL $1: a child is still running"; fail=1; fi
}

# 1. TERM stops both children and exits 0.
run_entry run run
kill -TERM "$ENTRY"; wait_exit 10
check "TERM shutdown exits 0" 0
check_gone "TERM shutdown exits 0"

# 2. MosDNS killed by a signal: entrypoint must notice, stop the gateway, exit 1.
run_entry run run
kill -KILL "$(cat "$TMP/mosdns.pid")"; wait_exit 10
check "SIGKILLed MosDNS detected" 1
check_gone "SIGKILLed MosDNS detected"

# 3. Gateway killed by a signal.
run_entry run run
kill -KILL "$(cat "$TMP/gateway.pid")"; wait_exit 10
check "SIGKILLed gateway detected" 1
check_gone "SIGKILLed gateway detected"

# 4. A child that exits on its own.
run_entry run exit
wait_exit 10
check "exiting gateway detected" 1
check_gone "exiting gateway detected"

# 5. A child that ignores TERM is KILLed after the grace period.
run_entry stubborn run
sleep 0.5
kill -TERM "$ENTRY"; wait_exit 15
check "stubborn child escalated to KILL" 0
check_gone "stubborn child escalated to KILL"

# 6. MosDNS exits on its own while the gateway ignores TERM: the gateway is
#    KILLed after its grace period and the exit code stays 1.
run_entry exit stubborn
wait_exit 15
check "exiting MosDNS, stubborn gateway escalated to KILL" 1
check_gone "exiting MosDNS, stubborn gateway escalated to KILL"

# 7. A TERM that arrives during the crash teardown must not turn exit 1 into 0.
run_entry exit stubborn
sleep 2
kill -TERM "$ENTRY" 2>/dev/null; wait_exit 15
check "TERM during crash teardown keeps exit 1" 1
check_gone "TERM during crash teardown keeps exit 1"

exit "$fail"
