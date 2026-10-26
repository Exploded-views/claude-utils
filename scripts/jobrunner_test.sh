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
JR_JD="$W/jd"; mkdir -p "$JR_JD" "$W/rundir"; : >"$W/secret"; : >"$W/rundir/token"   # stand-ins for the token file and the run dir
job() { # index
  case "$KIND" in
    read)  read -r line || true; echo "read-ok $1" ;;
    grace) echo "started $1" >>"$W/started"
           sh -c 'trap "sleep 1; echo cleaned >>\"$W/cleaned\"; exit 0" TERM; while :; do sleep 0.2; done' &
           wait "$!" || true ;;
    hang)  echo "$$" >>"$W/pids"; sleep 60 & echo "$!" >>"$W/pids"; echo "started $1" >>"$W/started"; wait "$!" ;;
    order) [ "$1" -ne 0 ] || sleep 1; echo "job $1 output" ;;
    fail)  echo "job $1 output"; case "$1" in 1) sleep 1; exit 3 ;; 0) sleep 4 ;; esac ;;
  esac
}
jr_emit() { printf 'block %s rc=%s: %s\n' "$1" "$2" "$(cat "$JR_JD/$1.out")"; }
on_exit() {
  local rc=$?
  jr_shield                  # a second signal must not cut the clean-up short
  jr_kill_all
  sleep 0.5                  # the rest of the clean-up takes a while
  rm -f "$W/secret" "$W/rundir/token"; rmdir "$W/rundir"
  : >"$W/cleanup.done"
  exit "$rc"
}
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
interrupt() { # jobs sig expected-rc [second-sig, sent 0.1 s later]
  local jb="$1" sg="$2" erc="$3" sg2="${4:-}" pid rc n alive t want t0 took
  : >"$W/pids"; : >"$W/started"; rm -f "$W/cleanup.done"; want="$jb"; [ "$want" -le 3 ] || want=3
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
  if [ -n "$sg2" ]; then sleep 0.1; kill -"$sg2" "$pid" 2>/dev/null || true; fi
  set +e; wait "$pid"; rc=$?; set -e
  took=$((SECONDS - t0))
  alive=0
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    alive=0
    for t in $(cat "$W/pids"); do kill -0 "$t" 2>/dev/null && alive=$((alive + 1)); done
    [ "$alive" -eq 0 ] && break; sleep 0.25
  done
  check "$sg${sg2:+ then $sg2 0.1 s later} with $jb job(s) running: the clean-up ran to the end (secret, run dir removed, done)" bash -c "[ -e '$W/cleanup.done' ] && [ ! -e '$W/secret' ] && [ ! -d '$W/rundir' ]"
  check "$sg${sg2:+ then $sg2} with $jb job(s) running: exit $erc within 3 s (TERM, not the KILL fallback), and every job and its child (sleep) is gone" bash -c "[ $rc -eq $erc ] && [ $took -le 3 ] && [ $alive -eq 0 ] && [ \"\$(wc -l <'$W/pids' | tr -d ' ')\" -ge $((want * 2)) ]"
}
interrupt 1 INT 130
interrupt 3 TERM 143
interrupt 3 INT 130
interrupt 2 INT 130 INT
interrupt 2 INT 130 TERM
interrupt 2 INT 130 HUP

# A job that reads its stdin under a terminal (job control gives background jobs
# the tty, and a read from it stops the job with SIGTTIN, so wait hangs): it must
# get end-of-file instead. A bounded run under a pty.
cat >"$W/pty.py" <<'PY'
import os, pty, select, signal, sys, time
pid, fd = pty.fork()
if pid == 0:
    os.execvp("bash", ["bash", sys.argv[1] + "/driver.sh", "2", "2", "read"])
out, end = b"", time.time() + 20
while time.time() < end:
    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            d = os.read(fd, 4096)
        except OSError:
            d = b""
        out += d
    done, st = os.waitpid(pid, os.WNOHANG)
    if done:
        sys.stdout.write(out.decode("utf-8", "replace")); sys.exit(os.WEXITSTATUS(st) if os.WIFEXITED(st) else 98)
os.kill(pid, signal.SIGKILL); os.waitpid(pid, 0)
sys.stdout.write(out.decode("utf-8", "replace")); sys.exit(99)
PY
set +e; python3 -I "$W/pty.py" "$W" >"$W/ptyout" 2>&1; PRC=$?; set -e
check "a job that reads stdin under a terminal gets end-of-file (it does not stop and hang): both jobs finish, exit 0" bash -c "[ $PRC -eq 0 ] && grep -q 'read-ok 0' '$W/ptyout' && grep -q 'read-ok 1' '$W/ptyout'"

# A child that needs a second to clean up after TERM gets the time (the grace
# period is for the whole group, not just the job's leader process).
: >"$W/started"; rm -f "$W/cleaned"
set -m
(exec python3 -I -c 'import os, signal, sys; signal.signal(signal.SIGINT, signal.SIG_DFL); os.execvp(sys.argv[1], sys.argv[1:])' \
  bash "$W/driver.sh" 2 2 grace </dev/null >"$W/out" 2>&1) &
GPID=$!
set +m
for _ in $(seq 1 100); do n="$(wc -l <"$W/started" | tr -d ' ')"; [ "${n:-0}" -ge 2 ] && break; sleep 0.1; done
sleep 0.3
kill -TERM "$GPID" 2>/dev/null || true
set +e; wait "$GPID"; set -e
check "a child with a 1 s TERM handler finishes its clean-up before the group is killed (both jobs)" bash -c "[ \$(wc -l <'$W/cleaned' | tr -d ' ') -eq 2 ]"

echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
