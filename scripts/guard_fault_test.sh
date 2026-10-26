#!/usr/bin/env bash
# Fault injection for every guard in scripts/guards.registry.
#
# The guards are the functions <name>_check between "# >>> guards" and
# "# <<< guards" in scripts/bootstrap.sh, run through guard/guard_ok (the helper
# between "# >>> guard-helper" and "# <<< guard-helper"): a guard counts as safe
# only if it exits 0 AND its last stdout line is exactly SAFE:<name>; anything
# else is a refusal. For each guard this test checks
#   - the real safe path in a fixture gives SAFE (one case), and
#   - the guard command replaced by a fault gives a refusal: exit 1, exit 127,
#     a Python traceback, garbage, empty output, the right token with a wrong
#     exit code, the right exit code without the token, the token followed by
#     more output, another guard's token, and a hang (10 cases), and
#   - for a guard that runs tools, each tool replaced by a fake first on PATH that
#     fails with exit 1, exit 127, a traceback, prints garbage, prints nothing,
#     or hangs makes the guard refuse (6 cases), and
#   - for a guard that probes whether a path exists, the path's parent made
#     unsearchable (chmod 000: the stat fails with EACCES) and a path below a
#     regular file (ENOTDIR) make it refuse, never read the error as "absent"
#     (2 cases; the chmod case is skipped, and says so, when running as root).
# Then: a signal during a hanging guard (INT and TERM, at top level and inside a
# job) ends the run within 2 s and leaves no process behind, and a few specific
# cases (two keys in one host-key scan, the test-OS override).
# No network, no GitHub, no Go; it runs on macOS's bash 3.2 as well.
#
# Usage: scripts/guard_fault_test.sh
# shellcheck disable=SC2034  # the globals below are read by the guards sourced from bootstrap.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
BS="$HERE/bootstrap.sh"
REG="$HERE/guards.registry"
PASS=0 FAIL=0 CASES=0 SKIPPED=0
ok()  { PASS=$((PASS + 1)); }
bad() { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }

W="$(mktemp -d)"
cleanup() { chmod -R u+rwx "${W:?}" 2>/dev/null || true; rm -rf -- "${W:?}"; }
trap cleanup EXIT

sed -n '/^# >>> guard-helper/,/^# <<< guard-helper/p; /^# >>> guard-py/,/^# <<< guard-py/p; /^# >>> guards/,/^# <<< guards/p' "$BS" >"$W/guards.sh"
sed -n '/^# >>> jobrunner/,/^# <<< jobrunner/p' "$BS" >"$W/jr.sh"
if ! grep -q '^guard_ok()' "$W/guards.sh" || ! grep -q '^guard_run()' "$W/guards.sh"; then
  bad "scripts/bootstrap.sh has the guard-helper (guard_run, guard_ok), guard-py and guards sections"
  echo "passed=$PASS failed=$FAIL"; exit 1
fi

# --- what the guards expect from the script around them ---
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
SYNC_ROOT="$W/sync"; STATE="$W/state"; HOST=testhost; ROUTES="$W/routes.json"; XDG_CFG="$W/cfg"; APPS="$W/apps.json"
ORG=example; KEEP_OUT_OWNERS=(other); KEEP_OUT_TERMS=(); OWN_OWNERS=(example); GITHUB_FP=""
export BOOTSTRAP_GUARD_TIMEOUT=1 BOOTSTRAP_GUARD_TEST=1 BOOTSTRAP_TEST_OS=Linux
mkdir -p "$SYNC_ROOT" "$STATE" "$XDG_CFG"
org_gh() { gh "$@"; }
printf 'a regular file\n' >"$W/afile"
# shellcheck disable=SC1090
. "$W/guards.sh"

# --- fixtures: GA = the guard's arguments for its real safe path; OKBIN = tools it needs;
#     PARENTS = directories whose chmod 000 must make the guard refuse; notdir_<name> = a stat error (ENOTDIR) ---
GA=() OKBIN="" PARENTS=()
mkgit() { # name: a clean clone of an empty bare remote in the sync root
  [ -d "$W/remote.git" ] || git init -q --bare -b main "$W/remote.git"
  git clone -q "$W/remote.git" "$SYNC_ROOT/$1" 2>/dev/null
  GA=("$SYNC_ROOT/$1")
}
fixture_git_dir() { mkgit git_dir; }
fixture_git_stash() { mkgit git_stash; }
fixture_git_unpushed() { mkgit git_unpushed; }
fixture_git_clean_tree() { mkgit git_clean_tree; }
fixture_git_idle() { mkgit git_idle; PARENTS=("$SYNC_ROOT/git_idle/.git"); }
fixture_daemon_lock() { mkdir -p "$SYNC_ROOT/dl/.state"; : >"$SYNC_ROOT/dl/.state/daemon.lock"; GA=("$SYNC_ROOT/dl"); PARENTS=("$SYNC_ROOT/dl/.state"); }
notdir_daemon_lock() { GA=("$W/afile"); }
fixture_routes_unnamed() { mkdir -p "$W/rt" "$SYNC_ROOT/ru"; ROUTES="$W/rt/routes.json"; printf '{"repos":{}}\n' >"$ROUTES"; GA=("$SYNC_ROOT/ru"); PARENTS=("$W/rt"); }
notdir_routes_unnamed() { ROUTES="$W/afile/routes.json"; }
fixture_sync_dir_ours() { mkdir -p "$SYNC_ROOT/hm-key"; : >"$STATE/$HOST.initing.hm-key"; GA=(hm-key); PARENTS=("$SYNC_ROOT"); }
notdir_sync_dir_ours() { SYNC_ROOT="$W/afile"; }
fixture_app_entry_absent() { mkdir -p "$W/ap"; printf '[]\n' >"$W/ap/apps.json"; APPS="$W/ap/apps.json"; GA=(memsync-admin); PARENTS=("$W/ap"); }
notdir_app_entry_absent() { APPS="$W/afile/apps.json"; }
fixture_repo_private() {
  mkdir -p "$W/okgh"
  printf '#!/bin/sh\necho "{\\"private\\": true, \\"visibility\\": \\"private\\"}"\n' >"$W/okgh/gh"; chmod +x "$W/okgh/gh"
  OKBIN="$W/okgh"; GA=(mem-project-x)
}
fixture_token_file_ok() { printf 'tok\n' >"$W/tokfile"; chmod 600 "$W/tokfile"; GA=("$W/tokfile"); }
fixture_check_key_ok() { printf 'key\n' >"$W/chkkey"; chmod 600 "$W/chkkey"; GA=("$W/chkkey"); }
fixture_org_not_kept_out() { GA=(Example); }
fixture_repo_name_ok() { GA=(mem-project-x); }
fixture_host_key_fp() {
  ssh-keygen -q -t ed25519 -N '' -f "$W/hk" >/dev/null 2>&1
  GITHUB_FP="$(ssh-keygen -lf "$W/hk.pub" | awk 'NR==1{print $2}')"
  GA=(github.com "$W/hk.pub")
}
fixture_no_old_config() { mkdir -p "$XDG_CFG/memsync"; printf '{}\n' >"$XDG_CFG/memsync/config.json"; GA=(); PARENTS=("$XDG_CFG/memsync"); }
notdir_no_old_config() { XDG_CFG="$W/afile"; }
fixture_no_predecessor_units() {
  mkdir -p "$W/okbin"; printf '#!/bin/sh\necho LoadState=not-found\n' >"$W/okbin/systemctl"; chmod +x "$W/okbin/systemctl"
  OKBIN="$W/okbin"; GA=()
}
fixture_recovery_key_ok() { GA=("age1pq1$(head -c 1100 /dev/zero | tr '\0' q)"); }
fixture_key_free() { mkdir -p "$W/kf"; GA=("$W/kf/no-such-key"); PARENTS=("$W/kf"); }
notdir_key_free() { GA=("$W/afile/x"); }
fixture_project_not_kept_out() { GA=(example project); }

fault_cmd() { # kind guard-name: stands in for the guard
  case "$1" in
    exit1) exit 1 ;;
    exit127) exit 127 ;;
    traceback) printf 'Traceback (most recent call last):\n  File "<stdin>", line 1, in <module>\nValueError: boom\n'; exit 1 ;;
    garbage) echo "xyzzy plugh"; exit 0 ;;
    empty) exit 0 ;;
    wrong_rc) echo "SAFE:$2"; exit 3 ;;
    no_token) echo "everything is fine"; exit 0 ;;
    token_extra) printf 'SAFE:%s\nand then more\n' "$2"; exit 0 ;;
    wrong_name) echo "SAFE:other_guard"; exit 0 ;;
    hang) sleep 30 ;;
  esac
}
mkfakes() { # kind tool...: prints a dir whose tools fail in the given way
  local kind="$1" d t; shift
  d="$(mktemp -d "$W/fake.XXXXXX")"
  for t in "$@"; do
    case "$kind" in
      exit1) printf '#!/bin/sh\nexit 1\n' >"$d/$t" ;;
      exit127) printf '#!/bin/sh\nexit 127\n' >"$d/$t" ;;
      traceback) printf '#!/bin/sh\necho "Traceback (most recent call last):" >&2\necho "ValueError: boom" >&2\nexit 1\n' >"$d/$t" ;;
      garbage) printf '#!/bin/sh\necho "xyzzy plugh"\nexit 0\n' >"$d/$t" ;;
      empty) printf '#!/bin/sh\nexit 0\n' >"$d/$t" ;;
      hang) printf '#!/bin/sh\nexec sleep 30\n' >"$d/$t" ;;
    esac
    chmod +x "$d/$t"
  done
  printf '%s' "$d"
}
snap() { S_SR="$SYNC_ROOT"; S_RT="$ROUTES"; S_AP="$APPS"; S_XC="$XDG_CFG"; }
unsnap() { SYNC_ROOT="$S_SR"; ROUTES="$S_RT"; APPS="$S_AP"; XDG_CFG="$S_XC"; }
IS_ROOT=0; [ "$(id -u)" -ne 0 ] || IS_ROOT=1

while read -r name deps; do
  case "$name" in ''|'#'*) continue ;; esac
  if ! declare -F "fixture_$name" >/dev/null; then bad "$name: no fixture_$name in scripts/guard_fault_test.sh"; continue; fi
  if ! declare -F "${name}_check" >/dev/null; then bad "$name: no ${name}_check function in scripts/bootstrap.sh"; continue; fi
  GA=() OKBIN="" PARENTS=()
  snap
  "fixture_$name"
  CASES=$((CASES + 1))
  if PATH="${OKBIN:+$OKBIN:}$PATH" guard_ok "$name" ${GA[@]+"${GA[@]}"}; then ok; else bad "$name: the real safe path was refused ($GUARD_WHY)"; fi
  for kind in exit1 exit127 traceback garbage empty wrong_rc no_token token_extra wrong_name hang; do
    CASES=$((CASES + 1))
    if guard_run "$name" fault_cmd "$kind" "$name"; then bad "$name: injected '$kind' was accepted as SAFE"; else ok; fi
  done
  if [ "$deps" != "-" ]; then
    for kind in exit1 exit127 traceback garbage empty hang; do
      CASES=$((CASES + 1))
      # shellcheck disable=SC2086
      fk="$(mkfakes "$kind" $deps)"
      if PATH="$fk:${OKBIN:+$OKBIN:}$PATH" guard_ok "$name" ${GA[@]+"${GA[@]}"}; then
        bad "$name: with ${deps} faked as '$kind' the guard still said SAFE"
      else ok; fi
    done
  fi
  # probes of "is this path there?": a stat error is not "absent"
  if [ "${#PARENTS[@]}" -gt 0 ]; then
    CASES=$((CASES + 1))
    if [ "$IS_ROOT" -eq 1 ]; then
      SKIPPED=$((SKIPPED + 1)); printf 'SKIP    %s: chmod 000 does not block root; the unsearchable-parent case is skipped\n' "$name"
    else
      for d in "${PARENTS[@]}"; do chmod 000 "$d"; done
      if PATH="${OKBIN:+$OKBIN:}$PATH" guard_ok "$name" ${GA[@]+"${GA[@]}"}; then bad "$name: with a parent dir unsearchable (chmod 000) the guard still said SAFE"; else ok; fi
      for d in "${PARENTS[@]}"; do chmod 755 "$d"; done
    fi
  fi
  if declare -F "notdir_$name" >/dev/null; then
    CASES=$((CASES + 1))
    "notdir_$name"
    if PATH="${OKBIN:+$OKBIN:}$PATH" guard_ok "$name" ${GA[@]+"${GA[@]}"}; then bad "$name: with a path below a regular file (ENOTDIR) the guard still said SAFE"; else ok; fi
  fi
  unsnap
done <"$REG"

# --- specific cases ---
CASES=$((CASES + 1))
snap; fixture_host_key_fp; cat "$W/hk.pub" "$W/hk.pub" >"$W/two.pub"
if guard_ok host_key_fp github.com "$W/two.pub"; then bad "host_key_fp: a scan with two key lines was accepted"; else ok; fi
unsnap
if [ "$(uname -s)" = Linux ]; then
  CASES=$((CASES + 1))
  fk="$(mkfakes exit1 systemctl)"
  if PATH="$fk:$PATH" BOOTSTRAP_GUARD_TEST= BOOTSTRAP_TEST_OS=Darwin guard_ok no_predecessor_units; then bad "no_predecessor_units: BOOTSTRAP_TEST_OS skipped the guard without the test flag"; else ok; fi
fi

# --- a signal during a hanging guard: top level and inside a job ---
cat >"$W/sigdrv.sh" <<'EOF'
set -euo pipefail
. "$W/guards.sh"
. "$W/jr.sh"
die() { echo "ERROR: $*" >&2; exit 1; }
hang_check() { echo "$$" >>"$W/gpids"; sleep 60 & echo "$!" >>"$W/gpids"; echo "ready" >>"$W/gready"; wait; }
jobfn() { guard hang; }
jr_emit() { :; }
on_exit() { local rc=$?; jr_shield; guard_abort; jr_kill_all; : >"$W/exited"; exit "$rc"; }
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export BOOTSTRAP_GUARD_TIMEOUT=30
case "$1" in
  top) guard hang ;;
  job) JR_JD="$W/jd"; JR_MAX=1; mkdir -p "$JR_JD"; jr_run jobfn 1 0 ;;
esac
EOF
export W
sigcase() { # mode signal expected-rc
  local mode="$1" sg="$2" erc="$3" pid rc t0 took alive t n
  : >"$W/gpids"; : >"$W/gready"; rm -f "$W/exited"
  set -m
  (exec python3 -I -c 'import os, signal, sys; signal.signal(signal.SIGINT, signal.SIG_DFL); os.execvp(sys.argv[1], sys.argv[1:])' \
    bash "$W/sigdrv.sh" "$mode" </dev/null >"$W/sigout" 2>&1) &
  pid=$!
  set +m
  for _ in $(seq 1 100); do n="$(wc -l <"$W/gready" | tr -d ' ')"; [ "${n:-0}" -ge 1 ] && break; sleep 0.1; done
  sleep 0.2
  t0=$SECONDS
  kill -"$sg" "$pid" 2>/dev/null || true
  set +e; wait "$pid"; rc=$?; set -e
  took=$((SECONDS - t0))
  alive=0
  for _ in 1 2 3 4 5 6 7 8; do
    alive=0
    for t in $(cat "$W/gpids"); do kill -0 "$t" 2>/dev/null && alive=$((alive + 1)); done
    [ "$alive" -eq 0 ] && break; sleep 0.25
  done
  CASES=$((CASES + 1))
  if [ "$rc" -eq "$erc" ] && [ "$took" -le 2 ] && [ "$alive" -eq 0 ] && [ -e "$W/exited" ]; then ok
  else bad "$sg during a hanging guard ($mode): rc=$rc (want $erc), took ${took}s (want <= 2), $alive process(es) left, cleanup ran: $([ -e "$W/exited" ] && echo yes || echo no)"; fi
}
sigcase top INT 130
sigcase top TERM 143
sigcase job INT 130
sigcase job TERM 143

printf 'guards: %s cases (%s skipped), passed=%s failed=%s\n' "$CASES" "$SKIPPED" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
