#!/usr/bin/env bash
# Fault injection for every guard in scripts/guards.registry.
#
# The guards are the functions <name>_check between "# >>> guards" and
# "# <<< guards" in scripts/bootstrap.sh, run through guard_ok (the helper
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
#     or hangs makes the guard refuse (6 cases).
# No network, no GitHub, no Go; it runs on macOS's bash 3.2 as well.
#
# Usage: scripts/guard_fault_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
BS="$HERE/bootstrap.sh"
REG="$HERE/guards.registry"
PASS=0 FAIL=0 CASES=0
ok()  { PASS=$((PASS + 1)); }
bad() { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }

W="$(mktemp -d)"
cleanup() { rm -rf -- "${W:?}"; }
trap cleanup EXIT

sed -n '/^# >>> guard-helper/,/^# <<< guard-helper/p; /^# >>> guard-py/,/^# <<< guard-py/p; /^# >>> guards/,/^# <<< guards/p' "$BS" >"$W/guards.sh"
if ! grep -q '^guard_ok()' "$W/guards.sh"; then
  bad "scripts/bootstrap.sh has the guard-helper, guard-py and guards sections"
  echo "passed=$PASS failed=$FAIL"; exit 1
fi

# --- what the guards expect from the script around them ---
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
SYNC_ROOT="$W/sync"; STATE="$W/state"; HOST=testhost; ROUTES="$W/routes.json"; XDG_CFG="$W/cfg"
ORG=example; KEEP_OUT_OWNERS=(other); KEEP_OUT_TERMS=(); OWN_OWNERS=(example); GITHUB_FP=""
export BOOTSTRAP_GUARD_TIMEOUT=1 BOOTSTRAP_TEST_OS=Linux
mkdir -p "$SYNC_ROOT" "$STATE" "$XDG_CFG"
org_gh() { cat "$W/repo.json"; }
# shellcheck disable=SC1090
. "$W/guards.sh"

# --- fixtures: GA = the guard's arguments for its real safe path; OKBIN = tools it needs ---
GA=() OKBIN=""
mkgit() { # name: a clean clone of an empty bare remote in the sync root
  [ -d "$W/remote.git" ] || git init -q --bare -b main "$W/remote.git"
  git clone -q "$W/remote.git" "$SYNC_ROOT/$1" 2>/dev/null
  GA=("$SYNC_ROOT/$1")
}
fixture_git_dir() { mkgit git_dir; }
fixture_git_stash() { mkgit git_stash; }
fixture_git_unpushed() { mkgit git_unpushed; }
fixture_git_clean_tree() { mkgit git_clean_tree; }
fixture_git_idle() { mkgit git_idle; }
fixture_daemon_lock() { mkdir -p "$SYNC_ROOT/dl/.state"; : >"$SYNC_ROOT/dl/.state/daemon.lock"; GA=("$SYNC_ROOT/dl"); }
fixture_routes_unnamed() { mkdir -p "$SYNC_ROOT/ru"; printf '{"repos":{}}\n' >"$ROUTES"; GA=("$SYNC_ROOT/ru"); }
fixture_sync_dir_ours() { GA=(absent-key); }
fixture_repo_private() { printf '{"private": true, "visibility": "private"}\n' >"$W/repo.json"; GA=(mem-project-x); }
fixture_token_file_ok() { printf 'tok\n' >"$W/tokfile"; chmod 600 "$W/tokfile"; GA=("$W/tokfile"); }
fixture_check_key_ok() { printf 'key\n' >"$W/chkkey"; chmod 600 "$W/chkkey"; GA=("$W/chkkey"); }
fixture_org_not_kept_out() { GA=(Example); }
fixture_repo_name_ok() { GA=(mem-project-x); }
fixture_host_key_fp() {
  ssh-keygen -q -t ed25519 -N '' -f "$W/hk" >/dev/null 2>&1
  GITHUB_FP="$(ssh-keygen -lf "$W/hk.pub" | awk 'NR==1{print $2}')"
  GA=(github.com "$W/hk.pub")
}
fixture_no_old_config() { mkdir -p "$XDG_CFG/memsync"; printf '{}\n' >"$XDG_CFG/memsync/config.json"; GA=(); }
fixture_no_predecessor_units() {
  mkdir -p "$W/okbin"; printf '#!/bin/sh\necho LoadState=not-found\n' >"$W/okbin/systemctl"; chmod +x "$W/okbin/systemctl"
  OKBIN="$W/okbin"; GA=()
}
fixture_recovery_key_ok() { GA=("age1pq1$(head -c 1100 /dev/zero | tr '\0' q)"); }
fixture_key_free() { GA=("$W/no-such-key"); }
fixture_project_not_kept_out() { GA=(someone project); }

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

while read -r name deps; do
  case "$name" in ''|'#'*) continue ;; esac
  if ! declare -F "fixture_$name" >/dev/null; then bad "$name: no fixture_$name in scripts/guard_fault_test.sh"; continue; fi
  if ! declare -F "${name}_check" >/dev/null; then bad "$name: no ${name}_check function in scripts/bootstrap.sh"; continue; fi
  GA=() OKBIN=""
  "fixture_$name"
  CASES=$((CASES + 1))
  if PATH="${OKBIN:+$OKBIN:}$PATH" guard_ok "$name" -- "${name}_check" ${GA[@]+"${GA[@]}"}; then ok; else bad "$name: the real safe path was refused ($GUARD_WHY)"; fi
  for kind in exit1 exit127 traceback garbage empty wrong_rc no_token token_extra wrong_name hang; do
    CASES=$((CASES + 1))
    if guard_ok "$name" -- fault_cmd "$kind" "$name"; then bad "$name: injected '$kind' was accepted as SAFE"; else ok; fi
  done
  [ "$deps" != "-" ] || continue
  for kind in exit1 exit127 traceback garbage empty hang; do
    CASES=$((CASES + 1))
    # shellcheck disable=SC2086
    fk="$(mkfakes "$kind" $deps)"
    if PATH="$fk:${OKBIN:+$OKBIN:}$PATH" guard_ok "$name" -- "${name}_check" ${GA[@]+"${GA[@]}"}; then
      bad "$name: with ${deps} faked as '$kind' the guard still said SAFE"
    else ok; fi
  done
done <"$REG"

printf 'guards: %s cases, passed=%s failed=%s\n' "$CASES" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
