#!/usr/bin/env bash
# Self-contained test of the job runner in scripts/bootstrap.sh (the lines
# between "# >>> jobrunner" and "# <<< jobrunner"): bounded parallelism with
# plain background jobs and `wait <pid>`, output in start order, no new job
# after a failure, and INT/TERM cleanup of the jobs and of what they started.
# It needs no network, no GitHub and no Go, so it also runs on macOS under the
# system /bin/bash (3.2): CI runs it on ubuntu-latest and macos-latest.
#
# Usage: scripts/jobrunner_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
BS="$HERE/bootstrap.sh"
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }

W="$(mktemp -d)"
export W   # the driver script reads it
cleanup() {
  local f
  for f in "$W"/* "$W"/jd/*; do [ -f "$f" ] && rm -f "$f"; done
  rmdir "$W/jd" 2>/dev/null || true
  rmdir "$W" 2>/dev/null || true
}
trap cleanup EXIT

sed -n '/^# >>> jobrunner/,/^# <<< jobrunner/p' "$BS" >"$W/jr.sh"
if [ ! -s "$W/jr.sh" ]; then
  bad "scripts/bootstrap.sh has a '# >>> jobrunner' ... '# <<< jobrunner' section"
  echo "passed=$PASS failed=$FAIL"; exit 1
fi

# A driver the test starts as its own process: it sources the runner, runs
# <n> jobs of the given kind at most <jobs> at a time, and has the same kind of
# exit trap as bootstrap.sh (kill what is still running, then exit).
cat >"$W/driver.sh" <<'EOF'
set -euo pipefail
. "$W/jr.sh"
JR_MAX="$1" N="$2" KIND="$3"
JR_JD="$W/jd"; mkdir -p "$JR_JD"
job() { # index
  case "$KIND" in
    hang)  echo "$$" >>"$W/pids"; sleep 60 & echo "$!" >>"$W/pids"; echo "started $1" >>"$W/started"; wait "$!" ;;
    order) [ "$1" -ne 0 ] || sleep 1; echo "job $1 output" ;;
    fail)  echo "job $1 output"; case "$1" in 1) sleep 1; exit 3 ;; 0) sleep 4 ;; esac ;;
  esac
}
jr_emit() { printf 'block %s rc=%s: %s\n' "$1" "$2" "$(cat "$JR_JD/$1.out")"; }
on_exit() { local rc=$?; trap - EXIT; jr_kill_all; exit "$rc"; }
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
jr_run job "$N" 0
echo "NOT_STARTED=${JR_NOT_STARTED:-}"
[ "${JR_FAILED:-0}" -eq 0 ]
EOF

drive() { # jobs n kind: runs to completion, output in $W/out
  : >"$W/pids"; : >"$W/started"
  set +e; bash "$W/driver.sh" "$1" "$2" "$3" >"$W/out" 2>&1; DRC=$?; set -e
}

drive 2 4 order
check "output comes in start order although job 0 finishes last" bash -c "[ \"\$(grep -c '^block' '$W/out')\" = 4 ] && [ \"\$(grep '^block' '$W/out' | tr '\n' ' ')\" = 'block 0 rc=0: job 0 output block 1 rc=0: job 1 output block 2 rc=0: job 2 output block 3 rc=0: job 3 output ' ] && [ $DRC -eq 0 ]"

drive 2 4 fail
check "after a failure nothing new starts, the running job finishes, and the exit is nonzero" bash -c "grep -q 'block 0 rc=0' '$W/out' && grep -q 'block 1 rc=3' '$W/out' && ! grep -q 'block 2' '$W/out' && grep -q 'NOT_STARTED=.*2' '$W/out' && [ $DRC -ne 0 ]"

# Interrupts: start the driver in its own process group (job control, as a
# terminal does), wait until jobs are running, signal, then check nothing is left.
interrupt() { # jobs sig expected-rc
  local jb="$1" sg="$2" erc="$3" pid rc n alive t want t0 took
  : >"$W/pids"; : >"$W/started"; want="$jb"; [ "$want" -le 3 ] || want=3
  set -m
  # python3 resets SIGINT to its default first: a shell started in the background of another shell
  # inherits "ignore SIGINT" and cannot trap it (a terminal's Ctrl-C does not start that way)
  (exec python3 -I -c 'import os, signal, sys; signal.signal(signal.SIGINT, signal.SIG_DFL); os.execvp(sys.argv[1], sys.argv[1:])' \
    bash "$W/driver.sh" "$jb" 5 hang </dev/null >"$W/out" 2>&1) &
  pid=$!
  set +m
  for _ in $(seq 1 100); do n="$(wc -l <"$W/started" | tr -d ' ')"; [ "${n:-0}" -ge "$want" ] && break; sleep 0.1; done
  t0=$SECONDS
  kill -"$sg" "$pid" 2>/dev/null || true
  set +e; wait "$pid"; rc=$?; set -e
  took=$((SECONDS - t0))
  alive=0
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    alive=0
    for t in $(cat "$W/pids"); do kill -0 "$t" 2>/dev/null && alive=$((alive + 1)); done
    [ "$alive" -eq 0 ] && break; sleep 0.25
  done
  check "$sg with $jb job(s) running: exit $erc within 3 s (TERM, not the KILL fallback), and every job and its child (sleep) is gone" bash -c "[ $rc -eq $erc ] && [ $took -le 3 ] && [ $alive -eq 0 ] && [ \"\$(wc -l <'$W/pids' | tr -d ' ')\" -ge $((want * 2)) ]"
}
interrupt 1 INT 130
interrupt 3 TERM 143
interrupt 3 INT 130

echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
