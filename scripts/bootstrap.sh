#!/usr/bin/env bash
# claude-memsync host bootstrap.
#
#   bootstrap.sh --host <label> --role <role> --org <org> --recovery-recipient <age1pq1...>
#                [--sha <commit>] [--prebuilt-dir <dir>]
#                [--workspace <dir>]... [--own-owner <owner>]...
#                [--keep-out-owner <owner>]... [--keep-out-term <term>]...   (optional)
#                [--project <name>]... | [--skip-project <name>]...   (optional, personal-dev: which
#                  checkouts get a mem-project repo, by repo name as the plan shows it; not both)
#                [--jobs <1-8>]   (per-repo steps at once; default 4)
#                [--repo-url <url>]
#                --org-token-file <path> [--delete-org-token-file]
#                  | --org-token-cmd "<argv>" | --org-app <name>
#                [--org-app-id <id> --org-app-key-cmd "<argv>"]
#                [--check-app <name> --check-app-id <id> --check-key-file <path>]
#                [--allow-org <org>]... [--dry-run] [--yes]
#
# Exit codes: 0 ok, 1 error, 2 usage, 3 paused (needs a human; run it again).
#
# Read this script before running it. --dry-run prints every step without doing
# it. Everything lives under $HOME; the script never uses sudo, never prints a
# private key or a token, and never creates or joins a client or orchestrator
# repo. --org-app <name> mints the org token from the GitHub App entry <name> in
# apps.json (claude-memsync app-token --app <name>), once per org call. With
# --org-app-id and --org-app-key-cmd the script adds that entry (a key command,
# never a key file) and removes it again when it exits: the admin key does not
# stay configured on the host. --check-app points the daemon's hourly re-check
# at this host's own Metadata-read key. See docs/bootstrap.md.
set -euo pipefail

# The org token is read into variables and env of single commands; an xtrace
# would print it. Refuse rather than rely on every read being wrapped.
case $- in
  *x*) printf 'ERROR: refusing to run with xtrace (set -x, bash -x, SHELLOPTS=xtrace): it would print the org token\n' >&2; exit 1 ;;
esac

# ---------------------------------------------------------------------------
# Role table. Edit here. A role lists repos as "key:kind:repo-name" and names
# the repo key the home folder is pinned to. personal-dev also discovers one
# mem-project-<name> repo per own git checkout (see discover_projects).
# Client and orchestrator repos (mem-client-*, mem-orch-*) are never allowed.
# ---------------------------------------------------------------------------
ROLE_homelab_REPOS="personal:personal:mem-personal infra-homelab:infra:mem-infra-homelab"
ROLE_homelab_HOME="infra-homelab"
ROLE_personal_dev_REPOS="personal:personal:mem-personal"
ROLE_personal_dev_HOME="personal"
WORKSPACE_REPO="mem-project-code-workspace"

GITHUB_FP="SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"
UTILS_URL="${CLAUDE_UTILS_URL:-https://github.com/Exploded-views/claude-utils}"

HOST="" ROLE="" SHA="" SHA_GIVEN=0 PREBUILT_DIR="" PB_VER="" ORG="" AGE_RECOVERY="" DRY_RUN=0 YES=0
PROJECTS_SEL=() SKIP_PROJECTS=() JOBS=4
WORKSPACES=() OWN_OWNERS=() KEEP_OUT_OWNERS=() KEEP_OUT_TERMS=() ALLOW_ORGS=() ORG_TOKEN_FILE="" ORG_TOKEN_CMD="" ORG_APP=""
ORG_APP_ID="" ORG_APP_KEY_CMD="" CHECK_APP="" CHECK_APP_ID="" CHECK_KEY_FILE="" APP_API_BASE="${BOOTSTRAP_APP_API_BASE:-}" ADMIN_WRITTEN=0

R_KEYS=() R_NAMES=() R_KINDS=()   # planned repos (parallel arrays)
RULE_LINES=""                     # "pin|folder|key" and "remote|host/owner/repo|key"
NOT_SYNCED=""                     # human-readable list
KEYS_CREATED=() PENDING_JOINS=() NEXT_STEPS=() CHOICE_NOTE=""

BIN="$HOME/.local/bin"
XDG_CFG="${XDG_CONFIG_HOME:-$HOME/.config}"   # where claude-memsync keeps its config: $XDG_CONFIG_HOME, else ~/.config, on every OS
CFG="$XDG_CFG/claude-memsync"
SYNC_ROOT="$HOME/.claudesync.d"
STATE="$HOME/.local/state/claude-memsync-bootstrap"
ROUTES="$CFG/routes.json"

say()  { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
udie() { printf 'ERROR: %s\n' "$*" >&2; exit 2; }   # usage error: exit 2

# run <cmd...>: print the command, then run it unless --dry-run.
run() {
  printf '  $ %s\n' "$*"
  [ "$DRY_RUN" -eq 1 ] && return 0
  "$@"
}

usage() { awk 'NR >= 2 { if ($0 == "#" && NR > 3 && seen++ >= 1) exit; print }' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

done_p() { [ -e "$STATE/$HOST.$1" ]; }
mark()   { [ "$DRY_RUN" -eq 1 ] && return 0; mkdir -p "$STATE"; : >"$STATE/$HOST.$1"; }

# ---------------------------------------------------------------------------
# Guards: every "may we proceed?" decision. The shape that keeps them honest is
# positive confirmation: a check that cannot tell must refuse, never proceed.
#   - A guard is a function <name>_check listed in scripts/guards.registry. It
#     prints "SAFE:<name>" as its last line only on an explicitly safe outcome
#     ("REFUSE:<reason>" otherwise) and never reads safety from a bare $?,
#     "if <cmd>; then", "<cmd> && ..." or "|| true": it captures a tool's output
#     with "x=$(cmd) || refuse" and compares it with what a working tool must
#     say (a control question first, where an empty answer is the safe one).
#   - It is run as "guard <name> args..." (dies) or "guard_ok <name> args..." (returns),
#     which run <name>_check and nothing else: safe only if the exit
#     status is 0 AND the last stdout line is exactly SAFE:<name>. Any other
#     exit status, a missing or wrong token, extra output after it, a signal, a
#     missing tool (127) or a timeout is a refusal. Whether a path exists is asked
#     with path_state (ABSENT only for "no such file"), never with [ -e ] / [ -L ],
#     which read every stat error as "absent". A signal to the script, or to a job,
#     takes the running check down with it (guard_abort).
#   - Blocks below: guard-helper (the runner), guard-py (Python sources for the
#     probes, whole body in try/except: an exception prints no token), guards (the
#     guards and what they use). scripts/lint-guards.sh checks the convention,
#     scripts/guard_fault_test.sh injects faults into every registered guard.
# ---------------------------------------------------------------------------
# >>> guard-helper
GUARD_TIMEOUT="${BOOTSTRAP_GUARD_TIMEOUT:-30}" GUARD_WHY="" GUARD_REASON="" GUARD_PGID=""
# guard_abort: kill the check a guard is waiting for. An exit trap and a job's signal trap call it, so a
# check never outlives the script or the job that asked.
guard_abort() {
  if [ -n "$GUARD_PGID" ]; then
    kill -KILL -- "-$GUARD_PGID" 2>/dev/null || true
    kill -KILL "$GUARD_PGID" 2>/dev/null || true
  fi
  GUARD_PGID=""
}
# guard_run <name> <cmd...>: 0 only for a SAFE:<name> last line with exit status 0 (see above).
# Sets GUARD_WHY (why it refused) and GUARD_REASON (the check's own REFUSE: text, if any). The check runs in
# its own process group, polled by a runner that is read through a pipe; a signal to this shell, or to the
# runner, takes the check down with it.
guard_run() {
  local name="$1" res="" line last body rc n=0 lim
  GUARD_WHY="" GUARD_REASON=""
  shift
  lim=$((GUARD_TIMEOUT * 20))
  exec 9< <(
    set -m
    "$@" 2>/dev/null </dev/null &
    cp=$!
    trap 'kill -KILL -- "-$cp" 2>/dev/null; kill -KILL "$cp" 2>/dev/null; exit 1' INT TERM HUP
    set +m
    printf 'GUARD_PGID:%s\n' "$cp"
    while kill -0 "$cp" 2>/dev/null; do
      if [ "$n" -ge "$lim" ]; then
        kill -TERM -- "-$cp" 2>/dev/null || true
        kill -TERM "$cp" 2>/dev/null || true
        sleep 0.2
        kill -KILL -- "-$cp" 2>/dev/null || true
        kill -KILL "$cp" 2>/dev/null || true
        printf '\nGUARD_TIMEOUT\n'
        exit 0
      fi
      sleep 0.05
      n=$((n + 1))
    done
    wc=0
    wait "$cp" || wc=$?
    printf '\nGUARD_RC:%s\n' "$wc"
  )
  while IFS= read -r -u 9 line || [ -n "$line" ]; do
    case "$line" in
      GUARD_PGID:*) GUARD_PGID="${line#GUARD_PGID:}" ;;
      GUARD_RC:* | GUARD_TIMEOUT) res="$res$line"$'\n'; break ;;
      *) res="$res$line"$'\n' ;;
    esac
  done
  exec 9<&-
  GUARD_PGID=""
  while [ "${res%$'\n'}" != "$res" ]; do res="${res%$'\n'}"; done
  last="${res##*$'\n'}"
  case "$last" in
    GUARD_TIMEOUT) GUARD_WHY="guard $name: no answer within ${GUARD_TIMEOUT}s"; return 1 ;;
    GUARD_RC:*) rc="${last#GUARD_RC:}" ;;
    *) GUARD_WHY="guard $name: no exit status"; return 1 ;;
  esac
  body="${res%$'\n'*}"
  while [ "${body%$'\n'}" != "$body" ]; do body="${body%$'\n'}"; done
  while IFS= read -r line; do
    case "$line" in REFUSE:*) GUARD_REASON="${line#REFUSE:}" ;; esac
  done <<<"$body"
  last="${body##*$'\n'}"
  case "$rc" in
    '' | *[!0-9]*) GUARD_WHY="guard $name: bad exit status"; return 1 ;;
  esac
  if [ "$rc" -eq 127 ]; then GUARD_WHY="guard $name: a tool it needs is missing (exit 127)${GUARD_REASON:+: $GUARD_REASON}"; return 1; fi
  if [ "$rc" -gt 128 ]; then GUARD_WHY="guard $name: killed by signal $((rc - 128))${GUARD_REASON:+: $GUARD_REASON}"; return 1; fi
  if [ "$rc" -ne 0 ]; then GUARD_WHY="guard $name refused (exit $rc)${GUARD_REASON:+: $GUARD_REASON}"; return 1; fi
  if [ "$last" != "SAFE:$name" ]; then GUARD_WHY="guard $name: it did not say SAFE:$name${GUARD_REASON:+: $GUARD_REASON}"; return 1; fi
  return 0
}
# guard_ok <name> [args...]: runs <name>_check with the args, and nothing else; returns 0 or 1.
guard_ok() { local gn="$1"; shift; guard_run "$gn" "${gn}_check" "$@"; }
# guard <name> [args...]: as guard_ok, and a refusal is fatal (exit 1).
guard() { guard_ok "$@" || die "$GUARD_WHY"; }
# <<< guard-helper

# >>> guard-py
G_PY_STATE='import os, stat, sys
verdict = None
try:
    try:
        st = os.lstat(sys.argv[1])
    except FileNotFoundError:
        verdict = "ABSENT"
    if verdict is None:
        verdict = "PRESENT"
        if stat.S_ISLNK(st.st_mode):
            try:
                os.stat(sys.argv[1])
            except FileNotFoundError:
                verdict = "DANGLING"
except BaseException as e:
    verdict = "ERR " + type(e).__name__
print(verdict)
'
G_PY_LOCK='import fcntl, sys
verdict = None
try:
    f = open(sys.argv[1], "rb")
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        verdict = "FREE"
    except BlockingIOError:
        verdict = "HELD"
except BaseException as e:
    verdict = "ERR " + type(e).__name__
print(verdict)
'
G_PY_ROUTES='import json, os, sys
verdict = None
try:
    d = os.path.realpath(sys.argv[2])
    data = json.load(open(sys.argv[1]))
    def strings(o):
        if isinstance(o, str):
            yield o
        elif isinstance(o, dict):
            for v in o.values():
                yield from strings(v)
        elif isinstance(o, list):
            for v in o:
                yield from strings(v)
    verdict = "UNNAMED"
    for v in strings(data):
        if v.startswith(("/", "~")):
            q = os.path.realpath(os.path.expanduser(v))
            if q == d or q.startswith(d + os.sep):
                verdict = "NAMED"
                break
        elif v.endswith(".json") or v.startswith(("./", "../")):
            verdict = "UNRESOLVABLE"
except BaseException as e:
    verdict = "ERR " + type(e).__name__
print(verdict)
'
G_PY_PRIVATE='import json, sys
verdict = None
try:
    d = json.load(sys.stdin)
    if d.get("private") is True and d.get("visibility") == "private":
        verdict = "PRIVATE"
    else:
        verdict = "NOTPRIVATE"
except BaseException as e:
    verdict = "ERR " + type(e).__name__
print(verdict)
'
G_PY_OLDCFG='import json, sys
verdict = None
try:
    v = json.load(open(sys.argv[1])).get("repo")
    verdict = "REPO" if v else "NOREPO"
except BaseException as e:
    verdict = "ERR " + type(e).__name__
print(verdict)
'
G_PY_APPS='import json, sys
verdict = None
try:
    apps = json.load(open(sys.argv[1]))
    if not isinstance(apps, list):
        verdict = "ERR NotAList"
    elif any(isinstance(a, dict) and a.get("name") == sys.argv[2] for a in apps):
        verdict = "HASENTRY"
    else:
        verdict = "NOENTRY"
except BaseException as e:
    verdict = "ERR " + type(e).__name__
print(verdict)
'
# <<< guard-py

# >>> guards
g_refuse() { printf 'REFUSE:%s\n' "$*"; }

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

in_list() { # needle items...
  local n="$1"
  shift
  local x
  for x in "$@"; do [ "$x" = "$n" ] && return 0; done
  return 1
}

# valid_pq_recipient <key>: a hybrid post-quantum age public key, age1pq1 plus
# at least 1000 more bech32 characters (real ones are about 1,960). No regex
# repeat count anywhere: macOS's grep and bash [[ =~ ]] stop at 255. Parameter
# expansion only, so it works in bash 3.2.
valid_pq_recipient() {
  local k="$1" rest
  case "$k" in age1pq1?*) ;; *) return 1 ;; esac
  [ "${#k}" -ge 1000 ] || return 1
  rest="${k#age1pq1}"
  [ -z "${rest//[023456789acdefghjklmnpqrstuvwxyz]/}" ]
}

# path_state <path>: the one way to ask "is it there?". Prints ABSENT only when lstat says
# "no such file" (FileNotFoundError); PRESENT; DANGLING (a symlink whose target is missing);
# anything else (EACCES, ENOTDIR, ELOOP, a failing python3, odd output) is no answer.
# [ -e ] and [ -L ] read every stat error as "absent": they are not used in a guard.
path_state() { python3 -I -c "$G_PY_STATE" "$1"; }
# g_state <path>: path_state, but prints only an answer; returns 1 for no answer.
g_state() {
  local s
  s="$(path_state "$1")" || return 1
  case "$s" in ABSENT | PRESENT | DANGLING) printf '%s' "$s" ;; *) return 1 ;; esac
}
# g_kind <path>: what stat says it is, without following a symlink: file, dir or symlink.
g_kind() {
  local k
  k="$(stat -c %F "$1" 2>/dev/null || stat -f %HT "$1")" || return 1
  case "$k" in
    "regular file" | "Regular File") printf file ;;
    directory | Directory) printf dir ;;
    "symbolic link" | "Symbolic Link") printf symlink ;;
    *) return 1 ;;
  esac
}

# git on a half-made sync dir: never climbing above the sync root into some enclosing repo
g_git() { local d="$1"; shift; GIT_CEILING_DIRECTORIES="$SYNC_ROOT" git -C "$d" "$@"; }
# the control every git guard starts with: a working git says ".git" for a plain clone
g_git_ctl() { local o; o="$(g_git "$1" rev-parse --git-dir)" || return 1; [ "$o" = ".git" ]; }

git_dir_check() { # dir
  g_git_ctl "$1" || { g_refuse "$1: cannot read its git state (a corrupt .git?); refusing to move it"; return 1; }
  echo "SAFE:git_dir"
}
git_stash_check() { # dir
  local o
  g_git_ctl "$1" || { g_refuse "$1: cannot read its git state (a corrupt .git?); refusing to move it"; return 1; }
  o="$(g_git "$1" for-each-ref --format='%(refname)' refs/stash)" || { g_refuse "$1: git could not list its stash; refusing to move it"; return 1; }
  [ -z "$o" ] || { g_refuse "$1 has a stash (work that exists only there); refusing to move it"; return 1; }
  echo "SAFE:git_stash"
}
git_unpushed_check() { # dir
  local o s
  g_git_ctl "$1" || { g_refuse "$1: cannot read its git state (a corrupt .git?); refusing to move it"; return 1; }
  o="$(g_git "$1" log --branches --not --remotes --format=%h)" || { g_refuse "$1: git log failed; refusing to move it"; return 1; }
  [ -z "$o" ] || { g_refuse "$1 began as an init of this bootstrap but holds a commit that is not on any remote (${o%%$'\n'*}); refusing to move it. Look at it with: git -C $1 log --branches --not --remotes"; return 1; }
  s="$(g_git "$1" symbolic-ref -q HEAD)" || s=""   # guard-ok: no branch (or no answer) takes the stricter detached-HEAD path below
  if [ -z "$s" ]; then   # a detached HEAD (or one we cannot read): its commits count too
    o="$(g_git "$1" log HEAD --not --remotes --format=%h)" || { g_refuse "$1: a detached HEAD that git cannot read; refusing to move it"; return 1; }
    [ -z "$o" ] || { g_refuse "$1 has a detached HEAD with a commit that is not on any remote (${o%%$'\n'*}); refusing to move it"; return 1; }
  fi
  echo "SAFE:git_unpushed"
}
git_clean_tree_check() { # dir
  local o
  g_git_ctl "$1" || { g_refuse "$1: cannot read its git state (a corrupt .git?); refusing to move it"; return 1; }
  o="$(g_git "$1" status --porcelain)" || { g_refuse "$1: git status failed; refusing to move it"; return 1; }
  [ -z "$o" ] || { g_refuse "$1 has uncommitted changes; refusing to move it. Look at it with: git -C $1 status"; return 1; }
  echo "SAFE:git_clean_tree"
}
git_idle_check() { # dir
  local n st
  g_git_ctl "$1" || { g_refuse "$1: cannot read its git state (a corrupt .git?); refusing to move it"; return 1; }
  for n in rebase-merge rebase-apply sequencer MERGE_HEAD CHERRY_PICK_HEAD REVERT_HEAD BISECT_LOG; do
    st="$(g_state "$1/.git/$n")" || { g_refuse "$1: cannot tell whether $n exists; refusing to move it"; return 1; }
    [ "$st" = "ABSENT" ] || { g_refuse "$1 is in the middle of a git operation ($n); refusing to move it"; return 1; }
  done
  echo "SAFE:git_idle"
}

# claude-memsync's daemon holds an exclusive flock on <sync dir>/.state/daemon.lock while it serves the dir
daemon_lock_check() { # dir
  local lk="$1/.state/daemon.lock" st o
  st="$(g_state "$lk")" || { g_refuse "could not check the daemon lock of $1 (cannot tell whether $lk exists); refusing to move it"; return 1; }
  case "$st" in ABSENT) echo "SAFE:daemon_lock"; return 0 ;; esac
  o="$(python3 -I -c "$G_PY_LOCK" "$lk")" || { g_refuse "could not check the daemon lock of $1 (python3 failed); refusing to move it"; return 1; }
  case "$o" in
    FREE) echo "SAFE:daemon_lock" ;;
    HELD) g_refuse "$1: its daemon lock is held (claude-memsync is serving it); refusing to move it"; return 1 ;;
    *) g_refuse "could not check the daemon lock of $1 ($o); refusing to move it"; return 1 ;;
  esac
}

# a routes.json this script did not write must not name the dir (any path form: ~, symlinks)
routes_unnamed_check() { # dir
  local o st ms
  st="$(g_state "$ROUTES")" || { g_refuse "cannot tell whether $ROUTES exists; refusing to move $1"; return 1; }
  case "$st" in ABSENT) echo "SAFE:routes_unnamed"; return 0 ;; esac
  ms="$(g_state "$STATE/$HOST.routes")" || ms="UNKNOWN"   # guard-ok: an unreadable mark counts as "not written by this script": the stricter path
  case "$ms" in PRESENT) echo "SAFE:routes_unnamed"; return 0 ;; esac
  o="$(python3 -I -c "$G_PY_ROUTES" "$ROUTES" "$1")" || { g_refuse "cannot read $ROUTES (python3 failed); refusing to move $1"; return 1; }
  case "$o" in
    UNNAMED) echo "SAFE:routes_unnamed" ;;
    NAMED) g_refuse "$1 is named in $ROUTES, which this bootstrap did not write; refusing to move it"; return 1 ;;
    *) g_refuse "cannot tell whether $ROUTES (which this bootstrap did not write) names $1 ($o); refusing to move it"; return 1 ;;
  esac
}

# may this key's sync dir be touched at all? absent, or ours (inited; or begun by this script and interrupted)
sync_dir_ours_check() { # key
  local d="$SYNC_ROOT/$1" st ms k
  st="$(g_state "$d")" || { g_refuse "cannot tell whether $d exists; refusing to touch it"; return 1; }
  case "$st" in ABSENT) echo "SAFE:sync_dir_ours"; return 0 ;; esac
  ms="$(g_state "$STATE/$HOST.init.$1")" || { g_refuse "cannot tell whether $d was made by this bootstrap; refusing to touch it"; return 1; }
  case "$ms" in PRESENT) echo "SAFE:sync_dir_ours"; return 0 ;; esac
  ms="$(g_state "$STATE/$HOST.initing.$1")" || { g_refuse "cannot tell whether $d was begun by this bootstrap; refusing to touch it"; return 1; }
  case "$ms" in
    PRESENT)
      k="$(g_kind "$d")" || { g_refuse "cannot tell what $d is; refusing to touch it"; return 1; }
      [ "$k" = "dir" ] || { g_refuse "$d is not a plain directory; refusing to touch it"; return 1; }
      echo "SAFE:sync_dir_ours"
      return 0 ;;
  esac
  g_refuse "$d already exists and was not made by this bootstrap; refusing to touch it"
  return 1
}

# the apps.json entry the script adds for the run (and deletes at the end) must not be one the owner put there
app_entry_absent_check() { # name
  local st o
  st="$(g_state "$APPS")" || { g_refuse "cannot tell whether $APPS exists; refusing"; return 1; }
  case "$st" in
    ABSENT) echo "SAFE:app_entry_absent"; return 0 ;;
    PRESENT) ;;
    *) g_refuse "cannot read $APPS (a dangling symlink?); refusing"; return 1 ;;
  esac
  o="$(python3 -I -c "$G_PY_APPS" "$APPS" "$1")" || { g_refuse "cannot read $APPS (python3 failed); refusing"; return 1; }
  case "$o" in
    NOENTRY) echo "SAFE:app_entry_absent" ;;
    HASENTRY) g_refuse "$APPS already has an entry named $1; refusing to replace it (and delete it at the end). Drop --org-app-id/--org-app-key-cmd to use that entry, or remove it first."; return 1 ;;
    *) g_refuse "cannot read $APPS ($o); refusing"; return 1 ;;
  esac
}

repo_private_check() { # repo name
  local json v
  json="$(org_gh api "repos/$ORG/$1")" || { g_refuse "cannot read repos/$ORG/$1"; return 1; }
  v="$(printf '%s' "$json" | python3 -I -c "$G_PY_PRIVATE")" || { g_refuse "cannot read the visibility of $ORG/$1"; return 1; }
  [ "$v" = "PRIVATE" ] || { g_refuse "repo $ORG/$1 is NOT private; refusing to continue"; return 1; }
  echo "SAFE:repo_private"
}

# a file that holds a secret: absolute path, a plain file of ours, mode 0600, one link
g_private_file() { # label file
  local lbl="$1" f="$2" k m u n me
  case "$f" in /*) ;; *) g_refuse "$lbl $f must be an absolute path"; return 1 ;; esac
  k="$(g_kind "$f")" || { g_refuse "$lbl $f is missing or not a plain file"; return 1; }
  [ "$k" = "file" ] || { g_refuse "$lbl $f is missing or not a plain file"; return 1; }
  m="$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f")" || { g_refuse "cannot stat $f"; return 1; }
  u="$(stat -c %u "$f" 2>/dev/null || stat -f %u "$f")" || { g_refuse "cannot stat $f"; return 1; }
  n="$(stat -c %h "$f" 2>/dev/null || stat -f %l "$f")" || { g_refuse "cannot stat $f"; return 1; }
  me="$(id -u)" || { g_refuse "cannot tell who you are"; return 1; }
  [ "$m" = 600 ] || { g_refuse "$lbl $f must be mode 0600 (is $m)"; return 1; }
  [ -n "$me" ] && [ "$u" = "$me" ] || { g_refuse "$lbl $f must be owned by you"; return 1; }
  [ "$n" = 1 ] || { g_refuse "$lbl $f must have one hard link (has $n)"; return 1; }
}
token_file_ok_check() { # file
  g_private_file "org token file" "$1" || return 1
  echo "SAFE:token_file_ok"
}
check_key_ok_check() { # file
  g_private_file "check key file" "$1" || return 1
  echo "SAFE:check_key_ok"
}

org_not_kept_out_check() { # org
  local lo o
  [ "$(lower AbC)" = "abc" ] || { g_refuse "cannot lowercase names; refusing"; return 1; }
  lo="$(lower "$1")"
  [ -n "$lo" ] || { g_refuse "cannot lowercase the org name; refusing"; return 1; }
  for o in ${KEEP_OUT_OWNERS[@]+"${KEEP_OUT_OWNERS[@]}"}; do
    [ "$o" != "$lo" ] || { g_refuse "--org '$1' is also a keep-out owner; refusing"; return 1; }
  done
  echo "SAFE:org_not_kept_out"
}

# a checkout may get a memory repo only if it is positively an own one and not kept out (owner, repo: lowercase)
project_not_kept_out_check() { # owner repo
  local o t own=0
  [ "$(lower AbC)" = "abc" ] || { g_refuse "cannot lowercase names; refusing"; return 1; }
  for o in ${KEEP_OUT_OWNERS[@]+"${KEEP_OUT_OWNERS[@]}"}; do
    [ "$o" != "$1" ] || { g_refuse "REFUSED (keep-out)"; return 1; }
  done
  for t in ${KEEP_OUT_TERMS[@]+"${KEEP_OUT_TERMS[@]}"}; do
    case "$1/$2" in *"$t"*) g_refuse "REFUSED (keep-out)"; return 1 ;; esac
  done
  for o in ${OWN_OWNERS[@]+"${OWN_OWNERS[@]}"}; do
    [ "$o" != "$1" ] || own=1
  done
  [ "$own" -eq 1 ] || { g_refuse "not synced (third-party)"; return 1; }
  echo "SAFE:project_not_kept_out"
}

repo_name_ok_check() { # repo name: only mem-* names, never client or orchestrator repos
  local re='^mem-[a-z0-9][a-z0-9._-]*$'
  case "$1" in
    mem-client-* | mem-orch-*) g_refuse "refusing repo name '$1' (only mem-* names; never mem-client-* or mem-orch-*)"; return 1 ;;
  esac
  [[ $1 =~ $re ]] || { g_refuse "refusing repo name '$1' (only mem-* names; never mem-client-* or mem-orch-*)"; return 1; }
  echo "SAFE:repo_name_ok"
}

# the scanned host key must be GitHub's, and be the only key in the scan (every line is appended to known_hosts)
host_key_fp_check() { # label scan-file
  local fps
  fps="$(ssh-keygen -lf "$2" | awk '{print $2}')" || { g_refuse "cannot read the host key of $1"; return 1; }
  [ -n "$fps" ] && [ "$fps" = "$GITHUB_FP" ] || { g_refuse "host key fingerprint for $1 is '$fps', expected exactly one key with $GITHUB_FP; refusing"; return 1; }
  echo "SAFE:host_key_fp"
}

no_old_config_check() { # the old memsync must not be configured
  local old="$XDG_CFG/memsync/config.json" o st
  st="$(g_state "$old")" || { g_refuse "cannot tell whether the old memsync is configured ($old); refusing"; return 1; }
  case "$st" in ABSENT) echo "SAFE:no_old_config"; return 0 ;; esac
  o="$(python3 -I -c "$G_PY_OLDCFG" "$old")" || { g_refuse "cannot read $old (python3 failed); refusing"; return 1; }
  [ "$o" = "NOREPO" ] || { g_refuse "the old memsync is configured ($old has \"repo\", or cannot be read: $o). Disable it first (move the file aside)."; return 1; }
  echo "SAFE:no_old_config"
}

no_predecessor_units_check() { # the old memsync's systemd units must be absent, or disabled and inactive
  local u o os
  os="$(uname -s)" || { g_refuse "cannot tell the operating system; refusing"; return 1; }
  [ "${BOOTSTRAP_GUARD_TEST:-}" != "1" ] || os="${BOOTSTRAP_TEST_OS:-$os}"   # a test-only override
  case "$os" in
    Linux) ;;
    Darwin) echo "SAFE:no_predecessor_units"; return 0 ;;
    *) g_refuse "cannot check for predecessor units on $os; refusing"; return 1 ;;
  esac
  for u in memsync.timer memsync.service; do
    o="$(systemctl --user show -p LoadState -p ActiveState -p UnitFileState "$u")" || { g_refuse "cannot ask systemd about $u; refusing"; return 1; }
    case "$o" in
      *LoadState=not-found*) ;;
      *LoadState=masked*) ;;
      *LoadState=loaded*)
        case "$o" in
          *UnitFileState=disabled* | *UnitFileState=static*) ;;
          *) g_refuse "predecessor unit $u is enabled. Stop and disable it first."; return 1 ;;
        esac
        case "$o" in
          *ActiveState=inactive* | *ActiveState=failed*) ;;
          *) g_refuse "predecessor unit $u is active. Stop and disable it first."; return 1 ;;
        esac ;;
      *) g_refuse "cannot tell the state of predecessor unit $u; refusing"; return 1 ;;
    esac
  done
  echo "SAFE:no_predecessor_units"
}

recovery_key_ok_check() { # key
  valid_pq_recipient "$1" || { g_refuse "--recovery-recipient must be a post-quantum age1pq1... public key (a classic age1... or ssh key is refused: repos are post-quantum by default)"; return 1; }
  echo "SAFE:recovery_key_ok"
}

key_free_check() { # path of the deploy key to make
  local st
  st="$(g_state "$1")" || { g_refuse "cannot tell whether $1 exists; refusing to overwrite it"; return 1; }
  case "$st" in
    ABSENT) echo "SAFE:key_free" ;;
    *) g_refuse "$1 already exists and was not made by this bootstrap; refusing to overwrite it"; return 1 ;;
  esac
}
# <<< guards


version_ge() { # have want (dotted, major.minor)
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n | head -n1)" = "$2" ]
}

# ---------------------------------------------------------------------------
# Prerequisites. Checked first, before any change and in --dry-run too: one
# "missing prerequisite:" line per missing tool, then exit 1.
# ---------------------------------------------------------------------------
BUILD_FROM_SOURCE=1     # --prebuilt-dir turns this off: Go is then not needed
GO_MODE=""
check_prereqs() {
  local missing=0 t gv gmin=1.25 gomod v
  miss() { printf 'missing prerequisite: %s\n' "$*" >&2; missing=$((missing + 1)); }
  if [ "${BASH_VERSINFO[0]}" -lt 3 ] || { [ "${BASH_VERSINFO[0]}" -eq 3 ] && [ "${BASH_VERSINFO[1]}" -lt 2 ]; }; then
    miss "bash ${BASH_VERSINFO[0]}.${BASH_VERSINFO[1]} is too old (need >= 3.2; macOS /bin/bash 3.2 is enough)"
  fi
  for t in sed awk grep tr cut sort head find stat mktemp id uname dirname chmod ln $([ "$BUILD_FROM_SOURCE" -eq 1 ] || echo 'cp cmp'); do
    command -v "$t" >/dev/null 2>&1 || miss "$t (a basic tool the script uses)"
  done
  if command -v git >/dev/null 2>&1; then
    gv="$(git --version | sed -E 's/[^0-9]*([0-9]+\.[0-9]+).*/\1/')"
    version_ge "$gv" 2.26 || miss "git $gv is too old (need >= 2.26)"
  else
    miss "git (to clone and verify the pinned commit and reach the memory repos)"
  fi
  command -v python3 >/dev/null 2>&1 || miss "python3 (JSON edits and argument splitting)"
  command -v ssh-keygen >/dev/null 2>&1 || miss "ssh-keygen (per-repo deploy keys and host-key checks)"
  command -v ssh-keyscan >/dev/null 2>&1 || miss "ssh-keyscan (GitHub host keys)"
  command -v gh >/dev/null 2>&1 || miss "gh (the GitHub CLI; org operations)"
  case "$(uname -s 2>/dev/null)" in
    Linux)  command -v systemctl >/dev/null 2>&1 || miss "systemctl (claude-memsync install registers a systemd user service)" ;;
    Darwin) command -v launchctl >/dev/null 2>&1 || miss "launchctl (claude-memsync install registers a launchd agent)" ;;
  esac
  if [ "$BUILD_FROM_SOURCE" -eq 1 ]; then
    # The Go floor is the go.mod of the tree this script sits in, when there is one.
    gomod="$(dirname "$0")/../go.mod"
    if [ -r "$gomod" ]; then
      v="$(sed -n 's/^go \([0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' "$gomod" 2>/dev/null | head -n1 || true)"
      [ -z "$v" ] || gmin="$v"
    fi
    if command -v go >/dev/null 2>&1 && version_ge "$(go version | sed -E 's/.*go([0-9]+\.[0-9]+).*/\1/')" "$gmin"; then
      GO_MODE=go
    elif command -v docker >/dev/null 2>&1 && [ "$(uname -s 2>/dev/null)" = Linux ]; then
      GO_MODE=docker
    elif command -v go >/dev/null 2>&1; then
      miss "go $(go version | sed -E 's/.*go([0-9]+\.[0-9]+).*/\1/') is too old (need >= $gmin, the go.mod version; or docker on Linux; or --prebuilt-dir)"
    else
      miss "go (need >= $gmin to build from source; or docker on Linux; or --prebuilt-dir; on macOS: brew install go)"
    fi
  fi
  if [ "$missing" -gt 0 ]; then
    printf 'ERROR: %s prerequisite(s) missing; nothing was changed\n' "$missing" >&2
    exit 1
  fi
}



add_repo() { # key kind name
  guard repo_name_ok "$3"
  R_KEYS+=("$1"); R_KINDS+=("$2"); R_NAMES+=("$3")
}

# split_args <json|nul> <line>: split a command line exactly as claude-memsync
# does (internal/secretcmd.SplitArgs): whitespace separates arguments,
# "double quotes" group with \" and \\ as the only escapes, 'single quotes'
# group literally, nothing is expanded, a backslash elsewhere is literal.
# Exits 1 on an unterminated quote. The shared test table is
# internal/secretcmd/testdata/splitargs.tsv.
split_args() {
  python3 -I -c '
import json, sys
mode, line = sys.argv[1], sys.argv[2]
args, cur, have, i = [], [], False, 0
while i < len(line):
    c = line[i]
    if c in " \t\n\r":
        if have:
            args.append("".join(cur)); cur, have = [], False
    elif c == "\x27":
        have = True
        j = line.find("\x27", i + 1)
        if j < 0:
            sys.exit(1)
        cur.append(line[i + 1:j]); i = j
    elif c == "\"":
        have = True
        j, closed = i + 1, False
        while j < len(line):
            if line[j] == "\\" and j + 1 < len(line) and line[j + 1] in "\"\\":
                cur.append(line[j + 1]); j += 2; continue
            if line[j] == "\"":
                closed = True; break
            cur.append(line[j]); j += 1
        if not closed:
            sys.exit(1)
        i = j
    else:
        have = True; cur.append(c)
    i += 1
if have:
    args.append("".join(cur))
if mode == "json":
    print(json.dumps(args, ensure_ascii=False))
else:
    sys.stdout.write("".join(a + "\0" for a in args))
' "$1" "$2"
}

# check_key_pause: the per-host check key has to be minted by a human in the
# GitHub UI. When the file is not there, say exactly how and exit 3 (paused,
# needs a human) before anything is changed; the same command resumes.
check_key_pause() {
  local f="$CHECK_KEY_FILE"
  case "$f" in /*) ;; *) udie "--check-key-file must be an absolute path" ;; esac
  if [ ! -e "$f" ] && [ ! -L "$f" ]; then
    {
      printf 'PAUSED: the check key %s does not exist yet. Nothing has been changed.\n' "$f"
      printf 'A human has to create it:\n'
      printf '  1. In a browser, as an owner of %s: GitHub > Settings > Developer settings > GitHub Apps > %s\n' "$ORG" "$CHECK_APP"
      printf '  2. Under "Private keys" click "Generate a private key"; the browser downloads a .pem.\n'
      printf '  3. On this host: copy the downloaded .pem to %s\n' "$f"
      printf '  4. chmod 600 %s\n' "$f"
      printf '  5. Delete the download from wherever the browser put it.\n'
      printf '  6. Rerun the same command.\n'
    } >&2
    exit 3
  fi
}

# ---------------------------------------------------------------------------
# Job runner: a function for each index 0..n-1 as a background job, at most
# JR_MAX at a time, in plain bash 3.2 (background jobs and `wait <pid>`; no
# `wait -n`, no setsid). Each job runs in its own process group (set -m around
# the `&`), so jr_kill_all can stop a job together with everything it started
# (claude-memsync, git, gh, ssh). Output and exit status are collected per job
# and handed to jr_emit <index> <rc> in index order, whatever finishes first.
# Once a job has failed (it is noticed when the next job is about to start)
# nothing new starts; running jobs finish. scripts/jobrunner_test.sh tests this
# section on its own. The caller defines jr_emit and sets JR_JD (a private
# directory for the per-job files <index>.out and <index>.err).
# >>> jobrunner
JR_MAX=4 JR_JD="" JR_FN="" JR_L=0 JR_NEXT=0 JR_FAILED=0 JR_NOT_STARTED=""
JR_PID=() JR_RC=()

jr_collect() { # index: the job has ended (or is waited for); keep its status
  local rc=0
  wait "${JR_PID[$1]}" || rc=$?
  JR_RC[$1]=$rc
  [ "$rc" -eq 0 ] || JR_FAILED=1
}

jr_poll() { # notice jobs that have ended, without waiting for any
  local i
  for ((i = JR_NEXT; i < JR_L; i++)); do
    [ -z "${JR_RC[$i]-}" ] || continue
    if ! kill -0 "${JR_PID[$i]}" 2>/dev/null; then jr_collect "$i"; fi
  done
}

jr_running() { # prints how many started jobs have not ended
  local i n=0
  for ((i = JR_NEXT; i < JR_L; i++)); do [ -n "${JR_RC[$i]-}" ] || n=$((n + 1)); done
  printf '%s' "$n"
}

jr_wait_oldest() { # wait for the oldest job that is still running
  local i
  for ((i = JR_NEXT; i < JR_L; i++)); do
    if [ -z "${JR_RC[$i]-}" ]; then jr_collect "$i"; return 0; fi
  done
}

jr_flush() { # hand every finished job at the front to jr_emit, in order
  while [ "$JR_NEXT" -lt "$JR_L" ] && [ -n "${JR_RC[$JR_NEXT]-}" ]; do
    jr_emit "$JR_NEXT" "${JR_RC[$JR_NEXT]}"
    JR_NEXT=$((JR_NEXT + 1))
  done
}

jr_start() { # index
  set -m
  ( trap 'if declare -F guard_abort >/dev/null; then guard_abort; fi; exit 143' TERM INT HUP; "$JR_FN" "$1" ) </dev/null >"$JR_JD/$1.out" 2>"$JR_JD/$1.err" &   # job control hands background jobs the tty; a read from it would stop the job
  JR_PID[$1]=$!
  set +m
  JR_L=$(($1 + 1))
}

# jr_run <function> <count> <warm-up 0|1>: with warm-up, job 1 starts only when
# job 0 has ended.
jr_run() {
  local i
  JR_FN="$1" JR_L=0 JR_NEXT=0 JR_FAILED=0 JR_NOT_STARTED="" JR_PID=() JR_RC=()
  for ((i = 0; i < $2; i++)); do
    jr_poll; jr_flush
    if [ "$3" -eq 1 ] && [ "$i" -eq 1 ]; then
      while [ "$(jr_running)" -gt 0 ]; do jr_wait_oldest; jr_poll; jr_flush; done
    fi
    while [ "$(jr_running)" -ge "$JR_MAX" ]; do jr_wait_oldest; jr_poll; jr_flush; done
    if [ "$JR_FAILED" -eq 1 ]; then JR_NOT_STARTED="${JR_NOT_STARTED:+$JR_NOT_STARTED }$i"; continue; fi
    jr_start "$i"
  done
  while [ "$JR_NEXT" -lt "$JR_L" ]; do jr_wait_oldest; jr_poll; jr_flush; done
}

# jr_shield: the first thing an exit trap does, so that a second signal while it
# cleans up cannot cut the clean-up short.
jr_shield() { trap - EXIT; trap '' INT TERM HUP; }

jr_kill_all() { # stop every job still running, with its children; wait for them
  local i n any
  for ((i = JR_NEXT; i < JR_L; i++)); do
    [ -z "${JR_RC[$i]-}" ] || continue
    kill -TERM -- "-${JR_PID[$i]}" 2>/dev/null || true
  done
  # one shared grace period of 5 s for every group, polled until each is gone
  for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    any=0
    for ((i = JR_NEXT; i < JR_L; i++)); do
      [ -z "${JR_RC[$i]-}" ] || continue
      if kill -0 -- "-${JR_PID[$i]}" 2>/dev/null; then any=1; fi
    done
    [ "$any" -eq 0 ] && break
    sleep 0.25 || true
  done
  for ((i = JR_NEXT; i < JR_L; i++)); do
    [ -z "${JR_RC[$i]-}" ] || continue
    if kill -0 -- "-${JR_PID[$i]}" 2>/dev/null; then kill -KILL -- "-${JR_PID[$i]}" 2>/dev/null || true; fi
    wait "${JR_PID[$i]}" 2>/dev/null || true
    JR_RC[$i]=143
  done
}
# <<< jobrunner

# ---------------------------------------------------------------------------
# One exit trap for everything that must happen on every exit path (success,
# error, signal): remove the temporary admin app entry, and, with
# --delete-org-token-file, delete the org token file (a real run only:
# a dry run leaves it, so the same token serves the dry run and the real run).
# ---------------------------------------------------------------------------
DELETE_TOKEN_FILE=0 TOKEN_FILE_ARMED=0
secure_delete() { # file: remove it; where shred exists, overwrite it first as an extra (SSDs and APFS give no guarantee)
  local f="$1"
  [ -e "$f" ] || [ -L "$f" ] || return 0
  if [ -f "$f" ] && [ ! -L "$f" ] && command -v shred >/dev/null 2>&1; then
    shred -u -- "$f" 2>/dev/null || true
  fi
  rm -f -- "$f"
}
token_file_remove() {
  [ "$TOKEN_FILE_ARMED" -eq 1 ] || return 0
  TOKEN_FILE_ARMED=0
  if ! guard_ok token_file_ok "$ORG_TOKEN_FILE"; then   # it may have been swapped for something that is not ours
    printf 'WARNING: not deleting %s: %s\n' "$ORG_TOKEN_FILE" "${GUARD_REASON:-$GUARD_WHY}" >&2
    return 0
  fi
  secure_delete "$ORG_TOKEN_FILE"
  if [ -e "$ORG_TOKEN_FILE" ]; then
    printf 'WARNING: could not delete the org token file %s; delete it by hand\n' "$ORG_TOKEN_FILE" >&2
  fi
}
# run_dir_remove: the run's private temp dir (job output, the minted org token), removed on every exit path.
RUN_DIR=""
run_dir_remove() {
  local f
  [ -n "$RUN_DIR" ] || return 0
  for f in "$RUN_DIR"/*; do [ -e "$f" ] || continue; secure_delete "$f"; done
  rmdir "$RUN_DIR" 2>/dev/null || true
  RUN_DIR=""
}
on_exit() {
  local rc=$?
  jr_shield
  set +e   # clean up as much as possible, whatever fails on the way
  guard_abort   # a check a guard is waiting for
  jr_kill_all   # repo jobs still running, with what they started
  run_dir_remove
  if declare -F admin_entry_remove >/dev/null; then admin_entry_remove; fi
  token_file_remove
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# check_token_file: a token file to be deleted must be a plain file of ours,
# 0600, one link. Anything else is refused and left alone (it may not be ours).
check_token_file() { guard token_file_ok "$ORG_TOKEN_FILE"; }

# check_prebuilt: --prebuilt-dir must hold both binaries, executable, and
# claude-memsync must report the version we expect. Sets PREBUILT_DIR (absolute)
# and PB_VER. Runs the given claude-memsync once, with `version`, which is the
# point: the binary says what it is.
check_prebuilt() {
  local b got
  [ -d "$PREBUILT_DIR" ] || udie "--prebuilt-dir $PREBUILT_DIR is not a directory"
  PREBUILT_DIR="$(cd "$PREBUILT_DIR" && pwd -P)"
  for b in claude-memsync claude-memmerge; do
    { [ -f "$PREBUILT_DIR/$b" ] && [ -x "$PREBUILT_DIR/$b" ]; } || die "--prebuilt-dir: $PREBUILT_DIR/$b is missing or not executable"
  done
  got="$("$PREBUILT_DIR/claude-memsync" version 2>&1)" || die "--prebuilt-dir: '$PREBUILT_DIR/claude-memsync version' failed"
  case "$got" in *$'\n'*) die "--prebuilt-dir: '$PREBUILT_DIR/claude-memsync version' must print exactly one line" ;; esac
  PB_VER="${got#claude-memsync }"
  printf '%s' "$PB_VER" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._+-]*$' || die "--prebuilt-dir: unexpected version output from $PREBUILT_DIR/claude-memsync"
}


# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --host)       [ $# -ge 2 ] || usage; HOST="$2"; shift 2 ;;
    --role)       [ $# -ge 2 ] || usage; ROLE="$2"; shift 2 ;;
    --sha)        [ $# -ge 2 ] || usage; SHA="$2"; SHA_GIVEN=1; shift 2 ;;
    --prebuilt-dir) [ $# -ge 2 ] || usage; PREBUILT_DIR="$2"; shift 2 ;;
    --org)        [ $# -ge 2 ] || usage; ORG="$2"; shift 2 ;;
    --recovery-recipient) [ $# -ge 2 ] || usage; AGE_RECOVERY="$2"; shift 2 ;;
    --workspace)  [ $# -ge 2 ] || usage; WORKSPACES+=("$2"); shift 2 ;;
    --own-owner)  [ $# -ge 2 ] || usage; OWN_OWNERS+=("$(lower "$2")"); shift 2 ;;
    --keep-out-owner | --deny-owner) [ $# -ge 2 ] || usage; KEEP_OUT_OWNERS+=("$(lower "$2")"); shift 2 ;;
    --keep-out-term | --deny-term) [ $# -ge 2 ] || usage; KEEP_OUT_TERMS+=("$(lower "$2")"); shift 2 ;;
    --project)    [ $# -ge 2 ] || usage; PROJECTS_SEL+=("$(lower "$2")"); shift 2 ;;
    --skip-project) [ $# -ge 2 ] || usage; SKIP_PROJECTS+=("$(lower "$2")"); shift 2 ;;
    --jobs)       [ $# -ge 2 ] || usage; JOBS="$2"; shift 2 ;;
    --repo-url)   [ $# -ge 2 ] || usage; UTILS_URL="$2"; shift 2 ;;
    --org-token-file) [ $# -ge 2 ] || usage; ORG_TOKEN_FILE="$2"; shift 2 ;;
    --org-token-cmd)  [ $# -ge 2 ] || usage; ORG_TOKEN_CMD="$2"; shift 2 ;;
    --org-app)        [ $# -ge 2 ] || usage; ORG_APP="$2"; shift 2 ;;
    --org-app-id)      [ $# -ge 2 ] || usage; ORG_APP_ID="$2"; shift 2 ;;
    --org-app-key-cmd) [ $# -ge 2 ] || usage; ORG_APP_KEY_CMD="$2"; shift 2 ;;
    --check-app)       [ $# -ge 2 ] || usage; CHECK_APP="$2"; shift 2 ;;
    --check-app-id)    [ $# -ge 2 ] || usage; CHECK_APP_ID="$2"; shift 2 ;;
    --check-key-file)  [ $# -ge 2 ] || usage; CHECK_KEY_FILE="$2"; shift 2 ;;
    --app-api-base)    [ $# -ge 2 ] || usage; APP_API_BASE="$2"; shift 2 ;;   # GitHub Enterprise, and the test fake
    --allow-org)  [ $# -ge 2 ] || usage; ALLOW_ORGS+=("$(lower "$2")"); shift 2 ;;
    --split-args-json) [ $# -ge 2 ] || usage; split_args json "$2"; exit $? ;;   # test hook
    --delete-org-token-file) DELETE_TOKEN_FILE=1; shift ;;
    --dry-run)    DRY_RUN=1; shift ;;
    --yes)        YES=1; shift ;;
    -h | --help)  usage ;;
    *)            udie "unknown argument: $1" ;;
  esac
done
# ---------------------------------------------------------------------------
# Flag validation. Every missing, malformed or conflicting flag is a usage error
# (exit 2), found before anything is read, paused on, or changed: validate_flags
# right after parsing (bash only, so it works before the prerequisite check),
# validate_flags2 once the tools it needs (python3, git) are known to be there.
# Runtime failures (file modes, existing keys, non-private repos) are exit 1.
# ---------------------------------------------------------------------------
validate_flags() {
  local re n_tok=0 v w
  [ -n "$HOST" ] || udie "--host is required"
  [ -n "$ORG" ] || udie "--org <org> is required: the GitHub organisation that holds the memory repos (there is no default)"
  [ -n "$AGE_RECOVERY" ] || udie "--recovery-recipient <age1pq1...> is required: your own offline post-quantum recovery PUBLIC key (generate one as docs/bootstrap.md describes; there is no default)"
  guard_ok recovery_key_ok "$AGE_RECOVERY" || udie "${GUARD_REASON:-$GUARD_WHY}"
  [ -n "$ROLE" ] || udie "--role is required"
  case "$ROLE" in
    homelab | personal-dev) ;;
    *) udie "unknown role '$ROLE' (known: homelab, personal-dev). There is no role for client or orchestrator repos." ;;
  esac
  re='^[a-z0-9][a-z0-9-]{0,31}$'
  [[ $HOST =~ $re ]] || udie "bad --host label '$HOST' (it is the host's label in recipients, deploy keys and join requests: 1-32 characters of a-z, 0-9 and '-', starting with a letter or digit)"
  re='^[A-Za-z0-9][A-Za-z0-9-]*$'
  [[ $ORG =~ $re ]] || udie "bad --org '$ORG'"
  guard_ok org_not_kept_out "$ORG" || udie "${GUARD_REASON:-$GUARD_WHY}"
  if [ ${#PROJECTS_SEL[@]} -gt 0 ] && [ ${#SKIP_PROJECTS[@]} -gt 0 ]; then
    udie "--project and --skip-project are mutually exclusive: name the projects to keep, or the ones to leave out"
  fi
  if [ "$ROLE" != personal-dev ] && [ $((${#PROJECTS_SEL[@]} + ${#SKIP_PROJECTS[@]})) -gt 0 ]; then
    udie "--project / --skip-project choose among discovered checkouts, which only --role personal-dev has"
  fi
  case "$JOBS" in
    [1-8]) ;;
    *) udie "--jobs must be an integer from 1 to 8 (got '$JOBS')" ;;
  esac
  for v in "$ORG_TOKEN_FILE" "$ORG_TOKEN_CMD" "$ORG_APP"; do [ -z "$v" ] || n_tok=$((n_tok + 1)); done
  [ "$n_tok" -le 1 ] || udie "give only one of --org-token-file, --org-token-cmd or --org-app"
  [ "$n_tok" -eq 1 ] || udie "--org-token-file <path>, --org-token-cmd \"<argv>\" or --org-app <name> is required: every org operation uses that token, never the ambient gh login"
  if [ "$DELETE_TOKEN_FILE" -eq 1 ]; then
    [ -n "$ORG_TOKEN_FILE" ] || udie "--delete-org-token-file is only valid with --org-token-file <path>"
    case "$ORG_TOKEN_FILE" in /*) ;; *) udie "--org-token-file must be an absolute path with --delete-org-token-file" ;; esac
  fi
  re='^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'
  if [ -n "$ORG_APP" ]; then
    [[ $ORG_APP =~ $re ]] || udie "bad --org-app '$ORG_APP'"
  fi
  if [ -n "$ORG_APP_ID" ] || [ -n "$ORG_APP_KEY_CMD" ]; then
    [ -n "$ORG_APP" ] || udie "--org-app-id / --org-app-key-cmd go with --org-app <name>"
    { [ -n "$ORG_APP_ID" ] && [ -n "$ORG_APP_KEY_CMD" ]; } || udie "give both --org-app-id and --org-app-key-cmd"
    re='^[0-9]{1,20}$'
    [[ $ORG_APP_ID =~ $re ]] || udie "bad --org-app-id (numeric)"
  fi
  if [ -n "$CHECK_APP$CHECK_APP_ID$CHECK_KEY_FILE" ]; then
    { [ -n "$CHECK_APP" ] && [ -n "$CHECK_APP_ID" ] && [ -n "$CHECK_KEY_FILE" ]; } || udie "give --check-app, --check-app-id and --check-key-file together"
    re='^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'
    [[ $CHECK_APP =~ $re ]] || udie "bad --check-app '$CHECK_APP'"
    re='^[0-9]{1,20}$'
    [[ $CHECK_APP_ID =~ $re ]] || udie "bad --check-app-id (numeric)"
    [ "$CHECK_APP" != "$ORG_APP" ] || udie "--check-app must differ from --org-app: the admin app never serves the daemon"
    case "$CHECK_KEY_FILE" in /*) ;; *) udie "--check-key-file must be an absolute path" ;; esac
  fi
  for w in ${WORKSPACES[@]+"${WORKSPACES[@]}"}; do
    case "$w" in "~"*) w="$HOME${w#\~}" ;; esac
    [ -d "$w" ] || udie "workspace '$w' is not a directory"
    [ ! -e "$w/.git" ] || udie "workspace '$w' is itself a git checkout; give its parent folder"
  done
  if [ -n "$PREBUILT_DIR" ]; then
    [ -d "$PREBUILT_DIR" ] || udie "--prebuilt-dir $PREBUILT_DIR is not a directory"
    if [ "$SHA_GIVEN" -eq 1 ]; then
      re='^[0-9a-f]{40}$'
      [[ $SHA =~ $re ]] || udie "--sha must be a full 40-hex commit"
    fi
  fi
}
validate_flags2() { # needs python3 and git
  if [ -n "$ORG_APP_KEY_CMD" ]; then split_args json "$ORG_APP_KEY_CMD" >/dev/null || udie "unbalanced quote in --org-app-key-cmd"; fi
  if [ -n "$ORG_TOKEN_CMD" ]; then
    split_args json "$ORG_TOKEN_CMD" >/dev/null || udie "unbalanced quote in --org-token-cmd"
    [ "$(split_args json "$ORG_TOKEN_CMD")" != "[]" ] || udie "--org-token-cmd is empty"
  fi
  if [ -z "$PREBUILT_DIR" ]; then
    if [ -z "$SHA" ]; then
      SHA="$(git -C "$(dirname "$0")" rev-parse HEAD 2>/dev/null || true)"
      [ -n "$SHA" ] || udie "--sha is required when the script is not run from a git checkout"
    fi
    printf '%s' "$SHA" | grep -Eq '^[0-9a-f]{40}$' || udie "--sha must be a full 40-hex commit"
  fi
}
# The guard runner (below) polls with sleep 0.05, and validate_flags already runs guards: check this first.
if ! command -v sleep >/dev/null 2>&1 || ! sleep 0.1 >/dev/null 2>&1; then
  printf 'missing prerequisite: sleep that accepts fractional seconds (the guard runner polls with sleep 0.05)\n' >&2
  printf 'ERROR: 1 prerequisite(s) missing; nothing was changed\n' >&2
  exit 1
fi
validate_flags
[ -z "$PREBUILT_DIR" ] || BUILD_FROM_SOURCE=0   # installing given binaries: no clone, no build, no Go
check_prereqs
validate_flags2
if [ "$DELETE_TOKEN_FILE" -eq 1 ]; then
  check_token_file
  if [ "$DRY_RUN" -eq 1 ]; then
    say "dry run: the org token file would be deleted on a real run"   # a dry run changes nothing
  else
    TOKEN_FILE_ARMED=1   # from here on every exit path deletes the file
  fi
fi
if [ -n "$ORG_APP" ]; then
  case "$HOME" in *[[:space:]\'\"\\]*) die "--org-app needs a HOME without spaces or quotes (the token command is passed as one string)" ;; esac
  ORG_TOKEN_CMD="$HOME/.local/bin/claude-memsync app-token --app $ORG_APP"
fi
if [ -n "$CHECK_APP$CHECK_APP_ID$CHECK_KEY_FILE" ]; then check_key_pause; fi
ORG_TOKEN_ARGV=()
if [ -n "$ORG_TOKEN_CMD" ]; then
  while IFS= read -r -d '' a; do ORG_TOKEN_ARGV+=("$a"); done < <(split_args nul "$ORG_TOKEN_CMD")
fi
# Arguments that hand the same token to claude-memsync (a file path or a command, never the token).
# An app is handed over as its app-token command, which claude-memsync runs per request. Once the
# run has minted the token (mint_run_token) these become a --token-file in the run's private dir.
if [ -n "$ORG_TOKEN_FILE" ]; then ORG_TOKEN_ARGS=(--token-file "$ORG_TOKEN_FILE"); else ORG_TOKEN_ARGS=(--token-cmd "$ORG_TOKEN_CMD"); fi
if [ -n "$PREBUILT_DIR" ]; then
  check_prebuilt
  if [ "$SHA_GIVEN" -eq 1 ]; then
    SHORT="$(printf '%s' "$SHA" | cut -c1-12)"
    LIB_ID="$SHA"
    SRC="$HOME/.local/src/claude-utils-$SHA"
    case "$PB_VER" in *"$SHORT"*) ;; *) die "--prebuilt-dir: $PREBUILT_DIR/claude-memsync reports version '$PB_VER', which does not match --sha $SHORT; refusing" ;; esac
  else
    printf '%s' "$PB_VER" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' \
      || die "--prebuilt-dir: $PREBUILT_DIR/claude-memsync reports version '$PB_VER', which is not a release version (a tagged release stamps vX.Y.Z); pass --sha <commit> for a build of that commit"
    SHORT="$PB_VER"; LIB_ID="$PB_VER"; SRC=""
  fi
else
  SHORT="$(printf '%s' "$SHA" | cut -c1-12)"
  LIB_ID="$SHA"
  SRC="$HOME/.local/src/claude-utils-$SHA"
fi
LIB="$HOME/.local/lib/claude-memsync/$LIB_ID"
if [ ${#KEEP_OUT_OWNERS[@]} -eq 0 ] && [ ${#KEEP_OUT_TERMS[@]} -eq 0 ]; then
  say "keep-out lists: none (routing alone decides what syncs)"
fi
ALLOW_ORGS+=("$(lower "$ORG")")
OWN_OWNERS+=("$(lower "$ORG")")

case "$ROLE" in
  homelab)      ROLE_REPOS="$ROLE_homelab_REPOS"; HOME_KEY="$ROLE_homelab_HOME" ;;
  personal-dev) ROLE_REPOS="$ROLE_personal_dev_REPOS"; HOME_KEY="$ROLE_personal_dev_HOME" ;;
  *)            udie "unknown role '$ROLE' (known: homelab, personal-dev). There is no role for client or orchestrator repos." ;;
esac
for spec in $ROLE_REPOS; do
  IFS=: read -r k kind name <<<"$spec"
  add_repo "$k" "$kind" "$name"
done

[ "$DRY_RUN" -eq 1 ] || { RUN_DIR="$(mktemp -d)"; chmod 700 "$RUN_DIR"; }   # a dry run creates nothing
say "claude-memsync bootstrap: host=$HOST role=$ROLE org=$ORG commit=$SHORT$([ "$DRY_RUN" -eq 1 ] && echo ' (dry run)')"

# ---------------------------------------------------------------------------
# a. Preflight
# ---------------------------------------------------------------------------
# org_token prints the org token. It is only called inside the command
# substitution of org_gh (or the preflight check), so it never outlives a call.
org_token() {
  { set +x; } 2>/dev/null
  if [ -n "$ORG_TOKEN_FILE" ]; then
    [ -r "$ORG_TOKEN_FILE" ] || return 1
    tr -d '\r\n' <"$ORG_TOKEN_FILE"
  else
    # minted once per run: the first call keeps the token in the run's private dir
    if [ -n "$RUN_DIR" ] && [ -s "$RUN_DIR/org-token" ]; then tr -d '\r\n' <"$RUN_DIR/org-token"; return 0; fi
    if [ -n "$ORG_APP" ] && [ ! -x "$BIN/claude-memsync" ]; then return 1; fi
    [ ${#ORG_TOKEN_ARGV[@]} -gt 0 ] || return 1
    local out rc=0
    out="$("${ORG_TOKEN_ARGV[@]}")" || rc=$?
    [ "$rc" -eq 0 ] || return 1
    out="$(printf '%s' "$out" | tr -d '\r\n')"
    if [ -n "$RUN_DIR" ] && [ -n "$out" ]; then ( umask 077; printf '%s\n' "$out" >"$RUN_DIR/org-token"; date +%s >"$RUN_DIR/org-token.at" ); fi
    printf '%s' "$out"
  fi
}

# org_gh: gh with the org token, set only on that one command.
org_gh() {
  { set +x; } 2>/dev/null
  local t
  t="$(org_token)" || die "cannot read the org token (--org-token-file / --org-token-cmd); stopping"
  [ -n "$t" ] || die "the org token is empty; stopping"
  GH_TOKEN="$t" gh "$@"
}

# Org reach, checked twice and reported separately: (a) what the ambient gh
# login can see, (b) what the org token can see. Both warn and proceed (the
# owner accepted this). The script only ever touches repos in --org.
BANNER_AMBIENT="" BANNER_TOKEN="" REACH_ORGS=()
org_banner() {
  [ -z "$BANNER_AMBIENT" ] || printf '\n%s\n' "$BANNER_AMBIENT" >&2
  [ -z "$BANNER_TOKEN" ] || printf '\n%s\n' "$BANNER_TOKEN" >&2
}
reach_banner() { # label orgs-output
  local label="$1" orgs="$2" o lo extra="" denied=""
  while IFS= read -r o; do
    [ -n "$o" ] || continue
    lo="$(lower "$o")"
    in_list "$lo" "${ALLOW_ORGS[@]}" && continue
    extra="${extra:+$extra, }$o"
    if in_list "$lo" ${KEEP_OUT_OWNERS[@]+"${KEEP_OUT_OWNERS[@]}"}; then denied="${denied:+$denied, }$o"; fi
  done <<<"$orgs"
  [ -n "$extra" ] || return 0
  local b="!!! WARNING: this host's $label can reach $extra. An agent host holds an owner-level token with reach beyond the memory repos."
  [ -z "$denied" ] || b="$b
!!! KEEP-OUT ORG(S) reachable through the $label: $denied. This script never creates, touches or keys any repo outside $ORG."
  printf '%s' "$b"
}
# note_reach <orgs-output>: remember the orgs outside the allow list (not in a
# subshell, unlike reach_banner), for the plan's KEEP_OUT_OWNERS suggestion.
note_reach() {
  local o
  while IFS= read -r o; do
    [ -n "$o" ] || continue
    in_list "$(lower "$o")" "${ALLOW_ORGS[@]}" && continue
    in_list "$o" ${REACH_ORGS[@]+"${REACH_ORGS[@]}"} || REACH_ORGS+=("$o")
  done <<<"$1"
}
check_token_readable() {
  local t
  t="$(org_token)" || die "cannot read the org token (--org-token-file / --org-token-cmd / --org-app); stopping"
  [ -n "$t" ] || die "the org token is empty; stopping"
  t=""
}
check_ambient_scope() {
  local orgs
  if orgs="$(gh api user/orgs --paginate --jq '.[].login' 2>/dev/null)"; then
    BANNER_AMBIENT="$(reach_banner "gh login" "$orgs")"; note_reach "$orgs"
  else
    warn "cannot list the orgs the ambient gh login can see"
  fi
}
check_token_scope() {
  local orgs
  if [ -n "$ORG_APP" ]; then
    # An installation token cannot list /user/orgs; the owners of the repos the
    # installation reaches are the orgs it can touch.
    if orgs="$(org_gh api installation/repositories --paginate --jq '.repositories[].owner.login' 2>/dev/null | sort -u)"; then
      BANNER_TOKEN="$(reach_banner "org token" "$orgs")"; note_reach "$orgs"
    else
      warn "cannot list the repositories the app installation reaches"
    fi
  elif orgs="$(org_gh api user/orgs --paginate --jq '.[].login' 2>/dev/null)"; then
    BANNER_TOKEN="$(reach_banner "org token" "$orgs")"; note_reach "$orgs"
  else
    warn "cannot list the orgs the org token can see"
  fi
}
check_org_scope() {
  if [ -n "$ORG_APP" ]; then
    # The app's token comes from the claude-memsync built in step b, so the
    # token itself is checked right after the build (check_app_scope).
    check_ambient_scope
    org_banner
    return 0
  fi
  check_token_readable
  check_ambient_scope
  check_token_scope
  org_banner
}
# check_app_scope: the --org-app counterpart of the token half of
# check_org_scope, run once the binary exists. Warns and proceeds, as there.
check_app_scope() {
  [ -n "$ORG_APP" ] || return 0
  if [ "$DRY_RUN" -eq 1 ]; then
    say "  would mint the org token with: claude-memsync app-token --app $ORG_APP (after the build), and check the orgs it reaches"
    return 0
  fi
  step "b2. Org token from app $ORG_APP"
  check_token_readable
  check_token_scope
  [ -z "$BANNER_TOKEN" ] || printf '\n%s\n' "$BANNER_TOKEN" >&2
  say "  ok"
}

# apps.json helpers. The file is written 0600 and atomically; entries are
# replaced by name and the others kept. Nothing secret goes in: a key command
# or key file path, an app id, a fingerprint.
APPS="$CFG/apps.json"
apps_edit() { # upsert|remove <name> [entry-json]
  BS_APPS="$APPS" BS_OP="$1" BS_NAME="$2" BS_ENTRY="${3:-}" python3 -I - <<'PY'
import json, os, sys
p, op, name = os.environ["BS_APPS"], os.environ["BS_OP"], os.environ["BS_NAME"]
try:
    apps = json.load(open(p))
except FileNotFoundError:
    apps = []
if not isinstance(apps, list):
    sys.exit("apps.json is not a JSON array")
apps = [a for a in apps if a.get("name") != name]
if op == "upsert":
    apps.append(json.loads(os.environ["BS_ENTRY"]))
if not apps and op == "remove":
    if os.path.exists(p):
        os.remove(p)
    sys.exit(0)
os.makedirs(os.path.dirname(p), mode=0o700, exist_ok=True)
tmp = p + ".tmp"
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(apps, f, indent=2); f.write("\n")
os.chmod(tmp, 0o600)
os.replace(tmp, p)
PY
}

app_entry() { # name id keyfile|"" keycmd-json|"" [fingerprint]
  BS_N="$1" BS_I="$2" BS_F="$3" BS_C="$4" BS_P="${5:-}" BS_O="$ORG" BS_B="$APP_API_BASE" python3 -I -c '
import json, os
e = {"name": os.environ["BS_N"], "appId": os.environ["BS_I"], "org": os.environ["BS_O"]}
if os.environ["BS_F"]: e["keyFile"] = os.environ["BS_F"]
if os.environ["BS_C"]: e["keyCmd"] = json.loads(os.environ["BS_C"])
if os.environ["BS_B"]: e["apiBase"] = os.environ["BS_B"]
if os.environ["BS_P"]: e["keyFingerprint"] = os.environ["BS_P"]
print(json.dumps(e))'
}

# The admin app entry exists only for the length of this run.
admin_entry_add() {
  [ -n "$ORG_APP_ID" ] || return 0
  if [ "$DRY_RUN" -eq 1 ]; then
    guard app_entry_absent "$ORG_APP"
    say "  would add the app entry $ORG_APP to $APPS (key command only), and remove it when the run ends"
    return 0
  fi
  # An entry the owner put there is never replaced, and so never deleted.
  guard app_entry_absent "$ORG_APP"
  ADMIN_WRITTEN=1
  apps_edit upsert "$ORG_APP" "$(app_entry "$ORG_APP" "$ORG_APP_ID" "" "$(split_args json "$ORG_APP_KEY_CMD")")"
}
admin_entry_remove() {
  [ "$ADMIN_WRITTEN" -eq 1 ] || return 0
  ADMIN_WRITTEN=0
  apps_edit remove "$ORG_APP" || printf 'WARNING: could not remove the %s entry from %s; remove it by hand\n' "$ORG_APP" "$APPS" >&2
}

# check_key_file: the per-host check key must be a plain file of ours, 0600, one link.
check_key_file() { guard check_key_ok "$CHECK_KEY_FILE"; }

preflight() {
  step "a. Preflight"
  gh auth status >/dev/null 2>&1 || die "gh is not logged in (run: gh auth login)"
  [ -z "$CHECK_APP" ] || check_key_file
  admin_entry_add
  check_org_scope
  command -v claude >/dev/null || warn "claude not found on PATH; the plugin step will be skipped"

  # Predecessor writers: refuse while any is configured or active.
  guard no_old_config 
  guard no_predecessor_units 
  say "  ok (gh logged in${GO_MODE:+, build via $GO_MODE})"
}

# A sync dir that is there without an init mark is refused, unless this script
# began it (the initing mark) and it holds nothing that exists only there: then
# it is moved aside, after the plan is confirmed, just before its init (see
# half_made_aside). Never deleted.
half_made_guards() { # key: every guard that says the half-made dir holds nothing that exists only there
  local d="$SYNC_ROOT/$1" gs
  gs="$(path_state "$d/.git")" || gs=""   # no answer: run the git guards (they refuse on anything they cannot read)
  if [ "$gs" != "ABSENT" ]; then
    guard git_dir "$d"
    guard git_stash "$d"
    guard git_unpushed "$d"
    guard git_clean_tree "$d"
    guard git_idle "$d"
  fi
  guard daemon_lock "$d"
  guard routes_unnamed "$d"
}
check_sync_dirs() { # run after the plan is final
  local i k ds ms
  for i in "${!R_KEYS[@]}"; do
    k="${R_KEYS[$i]}"
    guard sync_dir_ours "$k"
    ds="$(path_state "$SYNC_ROOT/$k")" || ds=""
    ms="$(path_state "$STATE/$HOST.init.$k")" || ms=""
    if [ "$ds" != "ABSENT" ] && [ "$ms" != "PRESENT" ]; then   # begun by this script and interrupted (no answer: look closer)
      half_made_guards "$k"
      warn "$SYNC_ROOT/$k is half-made by an interrupted run of this bootstrap; after you confirm, it is moved aside (to $k.half-made.<time>), never deleted"
    fi
  done
}
# unique_path <path>: <path>, or <path>.1, .2 ... the first that does not exist.
unique_path() {
  local b="$1" i=1
  if [ ! -e "$b" ] && [ ! -L "$b" ]; then printf '%s' "$b"; return 0; fi
  while [ -e "$b.$i" ] || [ -L "$b.$i" ]; do i=$((i + 1)); done
  printf '%s' "$b.$i"
}
# half_made_aside <key>: after confirmation, in the repo's job, just before its init.
# The guards run again here: the dir may have changed since the first check.
half_made_aside() {
  local d="$SYNC_ROOT/$1" to ds ms
  [ "$DRY_RUN" -eq 0 ] || return 0
  ds="$(path_state "$d")" || die "cannot tell whether $d exists; refusing to touch it"
  [ "$ds" != "ABSENT" ] || return 0
  guard sync_dir_ours "$1"
  ms="$(path_state "$STATE/$HOST.initing.$1")" || ms=""
  [ "$ms" = "PRESENT" ] || die "$d is there but was not begun by this bootstrap; refusing to touch it"
  half_made_guards "$1"
  to="$(unique_path "$d.half-made.${BOOTSTRAP_ASIDE_STAMP:-$(date -u +%Y%m%dT%H%M%SZ)}")"
  say "  $d is half-made by an interrupted run: moving it aside to $to"
  mv -n "$d" "$to"
  # mv -n never overwrites; if the target appeared meanwhile the dir would land inside it or stay: check
  if [ -e "$d" ] || [ ! -e "$to" ] || [ -e "$to/$1" ]; then
    die "could not move $d aside to $to cleanly; look at both before running again"
  fi
}

# ---------------------------------------------------------------------------
# personal-dev: workspaces and project discovery
# ---------------------------------------------------------------------------
esc_folder() { printf '%s' "$1" | sed 's/[^A-Za-z0-9]/-/g'; }


discover_projects() {
  step "Plan: workspaces and project discovery"
  local w ws="" d p remote owner repo rest key name x
  local c_repo=() c_name=() c_key=() c_rule=() i sel skipped="" chose=0
  if [ ${#WORKSPACES[@]} -eq 0 ] && [ -t 0 ]; then
    say "  Workspace parent folders (not git repos), one per line, empty line to finish:"
    while printf '  workspace> ' && IFS= read -r w && [ -n "$w" ]; do WORKSPACES+=("$w"); done
  fi
  for w in ${WORKSPACES[@]+"${WORKSPACES[@]}"}; do
    case "$w" in "~"*) w="$HOME${w#\~}" ;; esac
    [ -d "$w" ] || udie "workspace '$w' is not a directory"
    w="$(cd "$w" && pwd -P)"
    [ -e "$w/.git" ] && udie "workspace '$w' is itself a git checkout; give its parent folder"
    ws="$ws$w
"
    RULE_LINES="${RULE_LINES}pin|$(esc_folder "$w")|project-code-workspace
"
    say "  workspace $w -> $WORKSPACE_REPO (folder pin)"
  done
  [ -z "$ws" ] || add_repo project-code-workspace project "$WORKSPACE_REPO"

  local roots="$HOME
$ws" seen="" checkouts
  # Held in a variable, not a temp file: nothing is written, dry run included.
  checkouts="$(
    while IFS= read -r d; do
      [ -n "$d" ] || continue
      find "$d" -maxdepth 3 \( -name '.?*' ! -name .git -type d \) -prune -o -name .git -print 2>/dev/null
    done <<<"$roots" | sort -u | while IFS= read -r g; do dirname "$g"; done
  )" || true
  while IFS= read -r p; do
    [ -n "$p" ] || continue
    remote="$(git -C "$p" remote get-url origin 2>/dev/null || true)"
    case "$remote" in
      *github.com[:/]*) rest="$(printf '%s' "$remote" | sed -E 's#^.*github\.com[:/]##; s#\.git$##; s#/$##')" ;;
      *) NOT_SYNCED="${NOT_SYNCED}  $p (no github.com origin)
"; continue ;;
    esac
    owner="$(lower "${rest%%/*}")"; repo="$(lower "${rest#*/}")"
    if ! guard_ok project_not_kept_out "$owner" "$repo"; then
      NOT_SYNCED="${NOT_SYNCED}  $p ($owner/$repo): ${GUARD_REASON:-not synced (could not be sure it is an own project)}
"; continue
    fi
    name="mem-project-$repo"; key="project-$repo"
    if ! printf '%s' "$name" | grep -Eq '^mem-project-[a-z0-9][a-z0-9._-]*$'; then
      NOT_SYNCED="${NOT_SYNCED}  $p ($owner/$repo): not synced (repo name unusable)
"; continue
    fi
    in_list "$key" ${R_KEYS[@]+"${R_KEYS[@]}"} && continue
    in_list "$repo" ${c_repo[@]+"${c_repo[@]}"} && continue
    c_repo+=("$repo"); c_name+=("$name"); c_key+=("$key"); c_rule+=("remote|github.com/$owner/$repo|$key")
    seen="$seen $repo"
  done <<<"$checkouts"
  say "  own projects found:${seen:- none}"
  # --project / --skip-project: every name must be one of the checkouts found.
  for x in ${PROJECTS_SEL[@]+"${PROJECTS_SEL[@]}"} ${SKIP_PROJECTS[@]+"${SKIP_PROJECTS[@]}"}; do
    chose=1
    in_list "$x" ${c_repo[@]+"${c_repo[@]}"} || die "unknown project '$x' (known:${seen:- none})"
  done
  seen=""
  for i in ${c_repo[@]+"${!c_repo[@]}"}; do
    sel=1
    if [ ${#PROJECTS_SEL[@]} -gt 0 ]; then in_list "${c_repo[$i]}" "${PROJECTS_SEL[@]}" || sel=0; fi
    if [ ${#SKIP_PROJECTS[@]} -gt 0 ]; then ! in_list "${c_repo[$i]}" "${SKIP_PROJECTS[@]}" || sel=0; fi
    if [ "$sel" -eq 1 ]; then
      add_repo "${c_key[$i]}" project "${c_name[$i]}"
      RULE_LINES="${RULE_LINES}${c_rule[$i]}
"
      seen="$seen ${c_repo[$i]}"
    else
      skipped="$skipped ${c_repo[$i]}"
    fi
  done
  if [ "$chose" -eq 1 ]; then
    say "  selected projects:${seen:- none}"
    say "  skipped by choice:${skipped:- none}"
    CHOICE_NOTE="projects: ${#R_KEYS[@]} repos planned; skipped by choice:${skipped:- none}"
  fi
  if [ -n "$NOT_SYNCED" ]; then say "  not synced:"; printf '%s' "$NOT_SYNCED"; fi
  say "  related suggestions: not built yet"
}

confirm_plan() {
  step "Plan"
  org_banner
  local i
  say "  repos (private, in $ORG):"
  for i in "${!R_KEYS[@]}"; do say "    ${R_NAMES[$i]}  (key ${R_KEYS[$i]}, kind ${R_KINDS[$i]}; created if missing)"; done
  say "  deploy keys (one per repo, titled '$HOST'):"
  for i in "${!R_KEYS[@]}"; do say "    ~/.ssh/${R_NAMES[$i]}_ed25519"; done
  say "  routes: home folder -> $HOME_KEY (exact pin); default deny"
  printf '%s' "$RULE_LINES" | while IFS='|' read -r t a b; do
    [ -n "$t" ] || continue
    if [ "$t" = pin ]; then say "    folder $a -> $b"; else say "    remote $a -> $b"; fi
  done
  [ -z "$CHOICE_NOTE" ] || say "  $CHOICE_NOTE"
  say "  encryption: age on every repo, recovery recipient $AGE_RECOVERY"
  if [ ${#REACH_ORGS[@]} -gt 0 ] && [ ${#KEEP_OUT_OWNERS[@]} -eq 0 ]; then
    say "  suggestion: these orgs are reachable and none is kept out; to keep any out, set KEEP_OUT_OWNERS=\"${REACH_ORGS[*]}\" (or pass --keep-out-owner <org>)"
  fi
  if [ "$DRY_RUN" -eq 1 ] || [ "$YES" -eq 1 ]; then return 0; fi
  [ -t 0 ] || die "not a terminal: pass --yes to accept this plan"
  printf 'Type yes to create these repos and keys: '
  local ans; IFS= read -r ans
  [ "$ans" = yes ] || die "not confirmed; nothing changed"
}

# ---------------------------------------------------------------------------
# b. Source and build
# ---------------------------------------------------------------------------
step_install_prebuilt() {
  step "b. Install prebuilt binaries from $PREBUILT_DIR ($SHORT)"
  local b
  for b in claude-memsync claude-memmerge; do
    if [ -e "$LIB/$b" ]; then
      cmp -s "$PREBUILT_DIR/$b" "$LIB/$b" || die "$LIB/$b exists and differs from $PREBUILT_DIR/$b; refusing to overwrite it"
    fi
  done
  if [ -x "$LIB/claude-memsync" ] && [ -x "$LIB/claude-memmerge" ]; then
    say "  binaries present in $LIB"
  else
    run mkdir -p "$LIB"
    for b in claude-memsync claude-memmerge; do
      run cp "$PREBUILT_DIR/$b" "$LIB/$b.tmp"
      run chmod 755 "$LIB/$b.tmp"
      run mv -f "$LIB/$b.tmp" "$LIB/$b"
    done
  fi
  link_binaries
}

link_binaries() {
  run mkdir -p "$BIN"
  local b aside i
  for b in claude-memsync claude-memmerge; do
    if [ -e "$BIN/$b" ] && [ ! -L "$BIN/$b" ]; then
      [ -f "$BIN/$b" ] || die "$BIN/$b exists and is not a regular file or a symlink; refusing to touch it"
      # BOOTSTRAP_ASIDE_STAMP: test hook for the timestamp. Never overwrite an
      # earlier backup: a taken name gets .1, .2, ...
      aside="$BIN/$b.pre-bootstrap-${BOOTSTRAP_ASIDE_STAMP:-$(date -u +%Y%m%dT%H%M%SZ)}"
      if [ -e "$aside" ] || [ -L "$aside" ]; then
        i=1
        while [ -e "$aside.$i" ] || [ -L "$aside.$i" ]; do i=$((i + 1)); done
        aside="$aside.$i"
      fi
      say "  $BIN/$b is a standalone binary, not a symlink: moving it aside to $aside"
      run mv "$BIN/$b" "$aside"
    fi
    run ln -sfn "$LIB/$b" "$BIN/$b"
  done
}

step_build() {
  if [ -n "$PREBUILT_DIR" ]; then step_install_prebuilt; return 0; fi
  step "b. Clone claude-utils at $SHORT and build"
  if [ -d "$SRC/.git" ] && [ "$(git -C "$SRC" rev-parse HEAD)" = "$SHA" ]; then
    say "  clone present at $SRC"
  else
    [ ! -e "$SRC" ] || die "$SRC exists but is not a checkout at $SHA"
    run mkdir -p "$(dirname "$SRC")"
    run git clone --quiet "$UTILS_URL" "$SRC"
    run git -C "$SRC" checkout --quiet "$SHA"
  fi
  if [ "$DRY_RUN" -eq 0 ]; then
    [ "$(git -C "$SRC" rev-parse HEAD)" = "$SHA" ] || die "HEAD of $SRC is not $SHA"
  fi
  if [ -x "$LIB/claude-memsync" ] && [ -x "$LIB/claude-memmerge" ]; then
    say "  binaries present in $LIB"
  else
    run mkdir -p "$LIB"
    if [ "$GO_MODE" = go ]; then
      run sh -c "cd '$SRC' && go build -o '$LIB/claude-memsync' ./cmd/claude-memsync && go build -o '$LIB/claude-memmerge' ./cmd/claude-memmerge"
    else
      run docker run --rm --user "$(id -u):$(id -g)" -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath -e GOFLAGS=-buildvcs=false \
        -v "$SRC:/src" -v "$LIB:/out" -w /src golang:1.25 \
        sh -c 'go build -o /out/claude-memsync ./cmd/claude-memsync && go build -o /out/claude-memmerge ./cmd/claude-memmerge'
    fi
  fi
  link_binaries
}

# ---------------------------------------------------------------------------
# c. Repos (private only)
# ---------------------------------------------------------------------------
check_private() { GUARD_TIMEOUT=120 guard repo_private "$1"; } # repo

# (the per-repo steps c, d and f run as jobs: see repo_ensure_job and repo_keys_init below)

# ---------------------------------------------------------------------------
# d. Deploy keys
# ---------------------------------------------------------------------------
add_known_host() { # label hostpattern scan-args...
  local label="$1" pat="$2"; shift 2
  local kh="$HOME/.ssh/known_hosts" tmp
  if [ -f "$kh" ] && ssh-keygen -F "$pat" -f "$kh" >/dev/null 2>&1; then
    say "  known_hosts already has $label"; return 0
  fi
  [ "$DRY_RUN" -eq 1 ] && { say "  would add $label to known_hosts if its fingerprint is $GITHUB_FP"; return 0; }
  tmp="$(mktemp)"
  ssh-keyscan -t ed25519 "$@" >"$tmp" 2>/dev/null || { rm -f "$tmp"; die "ssh-keyscan failed for $label"; }
  guard_ok host_key_fp "$label" "$tmp" || { rm -f "$tmp"; die "$GUARD_WHY"; }
  sed -E "s/^[^ ]+ /$pat /" "$tmp" >>"$kh"
  rm -f "$tmp"; chmod 600 "$kh"
  say "  added $label to known_hosts (fingerprint verified)"
}

# Serial, before any job starts: ~/.ssh, known_hosts and ~/.ssh/config are
# shared files, so only this one process writes them.
step_ssh() {
  step "d. Host keys"
  run mkdir -p "$HOME/.ssh"
  run chmod 700 "$HOME/.ssh"
  add_known_host "github.com:22" "github.com" github.com
  add_known_host "ssh.github.com:443" "[ssh.github.com]:443" -p 443 ssh.github.com
}

# The ssh aliases, written once every repo is known private (a refused repo gets none).
step_ssh_aliases() {
  step "d2. ssh aliases"
  local i name key
  for i in "${!R_NAMES[@]}"; do
    name="${R_NAMES[$i]}"; key="$HOME/.ssh/${name}_ed25519"
    if ! grep -qE "^Host gh-$name\$" "$HOME/.ssh/config" 2>/dev/null; then
      say "  adding ssh alias gh-$name (ssh.github.com:443)"
      if [ "$DRY_RUN" -eq 0 ]; then
        touch "$HOME/.ssh/config"; chmod 600 "$HOME/.ssh/config"
        {
          printf '\nHost gh-%s\n  HostName ssh.github.com\n  Port 443\n  User git\n  IdentityFile %s\n  IdentitiesOnly yes\n' "$name" "$key"
        } >>"$HOME/.ssh/config"
      fi
    fi
  done
}

# ---------------------------------------------------------------------------
# e. routes.json and denylists
# ---------------------------------------------------------------------------
# re_escape <term>: the term as a literal Go (RE2) regex. A leading # is
# escaped too, so the line is never read as a comment.
re_escape() {
  local e
  e="$(printf '%s' "$1" | sed 's/[][\\.^$*+?(){}|]/\\&/g')"
  case "$e" in '#'*) e="\\$e" ;; esac
  printf '%s' "$e"
}

# plan_routes_json prints the routes.json this plan calls for.
plan_routes_json() {
  local i spec=""
  for i in "${!R_KEYS[@]}"; do spec="${spec}repo|${R_KEYS[$i]}|${R_NAMES[$i]}|${R_KINDS[$i]}
"; done
  spec="$spec$RULE_LINES"
  BS_SPEC="$spec" BS_SYNC_ROOT="$SYNC_ROOT" BS_HOME="$HOME" BS_XDG="$XDG_CFG" BS_OLD="$ROUTES" python3 -I - <<'PY'
import json, os
repos, routes, pins = {}, [], []
personal = None
for line in os.environ["BS_SPEC"].splitlines():
    p = line.split("|")
    if p[0] == "repo":
        repos[p[1]] = {"config": os.path.join(os.environ["BS_SYNC_ROOT"], p[1], "config.json"),
                       "kind": p[3], "remote": p[2]}
        if p[3] == "personal":
            personal = p[1]
    elif p[0] == "pin":
        pins.append({"folder": p[1], "repo": p[2]})
    elif p[0] == "remote":
        routes.append({"remote": p[1], "repo": p[2]})
home = os.environ["BS_HOME"]
xdg = os.environ["BS_XDG"]
out = {"repos": repos, "routes": pins + routes, "default": "deny",
       "foreignWriters": [
           {"file": xdg + "/memsync/config.json", "jsonKeyNonEmpty": "repo"},
           {"systemdUnit": "memsync.timer"},
           {"systemdUnit": "memsync.service"},
           {"process": "memsync/bin/memsync"},
           {"claudePlugin": "memsync@memsync", "onlyIfFile": xdg + "/memsync/config.json"}]}
if personal:
    out["personal"] = personal
try:   # a registry the owner configured survives a regeneration
    reg = json.load(open(os.environ["BS_OLD"])).get("registry")
    if reg:
        out["registry"] = reg
except Exception:
    pass
print(json.dumps(out, indent=2))
PY
}

# routes_same <plan-json>: the routes.json on disk already says this, apart
# from the home-folder pin that `route home` adds later.
routes_same() {
  BS_NEW="$1" BS_OLD="$ROUTES" BS_HOMEPIN="$(esc_folder "$HOME")" python3 -I -c '
import json, os
try:
    old = json.load(open(os.environ["BS_OLD"]))
except Exception:
    raise SystemExit(1)
new = json.loads(os.environ["BS_NEW"])
home = os.environ["BS_HOMEPIN"]
key = lambda r: json.dumps(r, sort_keys=True)
for d in (old, new):
    d["routes"] = sorted((r for r in d.get("routes", []) if r.get("folder") != home), key=key)
raise SystemExit(0 if old == new else 1)'
}

# Runs before any repo or key is created: a plan that no longer matches the
# routes.json from an earlier run (a new project or workspace) is regenerated
# here, or the run stops, rather than creating repos nothing routes to.
step_routes() {
  step "e. routes.json and denylists"
  local k t new regen=0 dls
  for k in $(printf '%s\n' ${R_KINDS[@]+"${R_KINDS[@]}"} | sort -u); do
    dls="$(g_state "$CFG/denylist-$k.txt")" || die "cannot tell whether $CFG/denylist-$k.txt exists; refusing"
    case "$dls" in ABSENT | PRESENT) ;; *) die "$CFG/denylist-$k.txt is a dangling symlink; refusing to write through it" ;; esac
    if [ "$dls" = PRESENT ]; then
      # A re-run adds new keep-out terms (escaped, de-duplicated) and never removes a line.
      local f="$CFG/denylist-$k.txt" esc added=0
      for t in ${KEEP_OUT_TERMS[@]+"${KEEP_OUT_TERMS[@]}"}; do
        esc="$(re_escape "$t")"
        grep -qxF -- "$esc" "$f" && continue
        added=$((added + 1))
        [ "$DRY_RUN" -eq 1 ] && continue
        if [ -s "$f" ] && [ -n "$(tail -c1 "$f")" ]; then printf '\n' >>"$f"; fi
        printf '%s\n' "$esc" >>"$f"
      done
      if [ "$added" -gt 0 ]; then say "  denylist-$k.txt: $([ "$DRY_RUN" -eq 1 ] && echo "would add" || echo added) $added keep-out term(s); existing lines kept"
      else say "  denylist-$k.txt: no new keep-out terms (existing lines kept)"; fi
    else
      run mkdir -p "$CFG"
      say "  writing $CFG/denylist-$k.txt (${#KEEP_OUT_TERMS[@]} keep-out terms)"
      if [ "$DRY_RUN" -eq 0 ]; then
        : >"$CFG/denylist-$k.txt"; chmod 600 "$CFG/denylist-$k.txt"
        for t in ${KEEP_OUT_TERMS[@]+"${KEEP_OUT_TERMS[@]}"}; do re_escape "$t" >>"$CFG/denylist-$k.txt"; printf '\n' >>"$CFG/denylist-$k.txt"; done
      fi
    fi
  done
  new="$(plan_routes_json)"
  if [ -e "$ROUTES" ]; then
    if routes_same "$new"; then say "  routes.json already matches the plan"; return 0; fi
    if done_p routes; then
      regen=1
      say "  the plan differs from the routes.json written by an earlier run (a new project or workspace): regenerating it; the old one is kept as $ROUTES.bak.<time>"
    elif [ "$YES" -eq 0 ]; then
      die "$ROUTES already exists and differs from this plan; refusing to overwrite it (pass --yes to replace it, a timestamped .bak.<time> is kept)"
    fi
  fi
  say "  writing $ROUTES (0600, default deny, foreignWriters for the old memsync)"
  [ "$DRY_RUN" -eq 1 ] && return 0
  mkdir -p "$CFG"
  [ ! -e "$ROUTES" ] || cp -p "$ROUTES" "$(unique_path "$ROUTES.bak.${BOOTSTRAP_ASIDE_STAMP:-$(date -u +%Y%m%dT%H%M%SZ)}")"
  printf '%s\n' "$new" >"$ROUTES.tmp"
  chmod 600 "$ROUTES.tmp"; mv "$ROUTES.tmp" "$ROUTES"
  mark routes
  # The rewritten file has lost the home pin; step_home puts it back.
  [ "$regen" -eq 0 ] || rm -f "$STATE/$HOST.home"
}

# ---------------------------------------------------------------------------
# f. init each repo
# ---------------------------------------------------------------------------
# The per-repo work is two phases, each over all repos: first check or create
# every repo and require it private (a public repo stops the run before any
# key exists), then each repo's deploy key, test and init. A job touches only
# its own repo and no shared file. In a real run it is a background job (see the
# job runner) whose output goes to files in the run's private dir, and what the
# parent needs (a key made, a join to accept) goes to <index>.key and .join
# there; a dry run runs the same functions one after another in this shell.
JOB_IDX=0 FAILED_REPOS=""
job_note() { # key|join text: tell the parent
  if [ -n "$RUN_DIR" ]; then
    printf '%s\n' "$2" >>"$RUN_DIR/$JOB_IDX.$1"
  elif [ "$1" = key ]; then KEYS_CREATED+=("$2")
  else PENDING_JOINS+=("$2"); fi
}

# token_hint <gh output>: after a 401, say so when the minted token is old (they last about an hour).
token_hint() {
  local t=0
  printf '%s' "$1" | grep -qE 'HTTP 401|Bad credentials' || return 0
  [ -n "$RUN_DIR" ] && [ -s "$RUN_DIR/org-token.at" ] || return 0
  t="$(cat "$RUN_DIR/org-token.at")"
  if [ $(($(date +%s) - t + ${BOOTSTRAP_TOKEN_AGE_BONUS:-0})) -gt 3000 ]; then
    printf 'the org token has probably expired (it was minted over 50 minutes ago and lasts about an hour); re-run the same command: the run resumes\n'
  fi
}

repo_ensure_job() { # index
  local i="$1" name out
  name="${R_NAMES[$i]}"
  guard repo_name_ok "$name"
  if [ "$DRY_RUN" -eq 1 ]; then
    say "  would check $ORG/$name, create it with --private if missing, and verify private=true"
  else
    if out="$(org_gh api "repos/$ORG/$name" 2>&1)"; then
      say "  $name exists"
    elif printf '%s' "$out" | grep -qiE 'not found|404'; then
      run org_gh repo create "$ORG/$name" --private
    else
      die "cannot look up $ORG/$name: $out $(token_hint "$out")"
    fi
    check_private "$name"
    say "  $name is private"
  fi
}

repo_keys_init() { # index
  local i="$1" k name kind url content=0 out fp key pub lsout
  JOB_IDX="$i"
  k="${R_KEYS[$i]}"; name="${R_NAMES[$i]}"; kind="${R_KINDS[$i]}"
  url="git@gh-$name:$ORG/$name.git"
  key="$HOME/.ssh/${name}_ed25519"; pub="$key.pub"
  # d. the deploy key
  if done_p "keygen.$name"; then
    say "  key for $name already generated"
  else
    guard key_free "$key"
    run ssh-keygen -q -t ed25519 -N '' -C "$HOST-$name" -f "$key"
    [ "$DRY_RUN" -eq 1 ] || chmod 600 "$key"
    mark "keygen.$name"
    job_note key "${name}_ed25519"
  fi
  if done_p "deploykey.$name"; then
    say "  deploy key for $name already added"
  else
    printf '  $ gh repo deploy-key add %s -R %s/%s -w -t %s\n' "$pub" "$ORG" "$name" "$HOST"
    if [ "$DRY_RUN" -eq 0 ]; then
      if ! out="$(org_gh repo deploy-key add "$pub" -R "$ORG/$name" -w -t "$HOST" 2>&1)"; then
        if printf '%s' "$out" | grep -qiE '422|forbidden|disabled|not allowed'; then
          die "deploy keys look disabled for $ORG. One-time org setting: Organization settings > Repository > (Member privileges / Deploy keys) allow deploy keys on repositories (API field deploy_keys_enabled_for_repositories). Then re-run; this script resumes."
        fi
        die "gh repo deploy-key add failed for $name: $out $(token_hint "$out")"
      fi
      mark "deploykey.$name"
    fi
  fi
  if [ "$DRY_RUN" -eq 0 ]; then
    git ls-remote "git@gh-$name:$ORG/$name.git" >/dev/null 2>&1 \
      || die "git ls-remote over gh-$name failed; is the deploy key accepted?"
    say "  $name reachable over its deploy key"
  else
    say "  would verify with: git ls-remote git@gh-$name:$ORG/$name.git"
  fi
  # f. init. The initing mark says a sync dir of this name was begun by this
  # script, so an interrupted init's half-made dir can be cleared on the re-run.
  if ! done_p "init.$k"; then
    if [ "$DRY_RUN" -eq 0 ]; then
      lsout="$(git ls-remote "$url" 2>/dev/null)" || die "git ls-remote $url failed: cannot tell whether $name already has content (a join may be needed); run the same command again once it works"
      [ -z "$lsout" ] || content=1
    fi
    half_made_aside "$k"
    mark "initing.$k"
    run "$BIN/claude-memsync" init --remote "$url" --sync-dir "$SYNC_ROOT/$k" --encrypt \
      --host-label "$HOST" --recovery-recipient "$AGE_RECOVERY" --archive-deletes --private-only \
      "${ORG_TOKEN_ARGS[@]}" --denylist "$CFG/denylist-$kind.txt"
    mark "init.$k"
    [ "$DRY_RUN" -eq 1 ] || rm -f -- "$STATE/$HOST.initing.$k"
    [ "$content" -eq 1 ] && mark "needjoin.$k"
  else
    say "  $k already initialised"
  fi
  if done_p "needjoin.$k" && ! done_p "join.$k"; then
    say "  $name already had content: this host has no decryption key there yet. Requesting to join."
    if [ "$DRY_RUN" -eq 1 ]; then
      say "  would run: claude-memsync join request --repo $k --label $HOST"
      return 0
    fi
    out="$("$BIN/claude-memsync" join request --repo "$k" --label "$HOST" 2>&1)" || die "join request failed: $out"
    printf '%s\n' "$out"
    if printf '%s' "$out" | grep -q '^this host is already a recipient of this repo'; then   # an earlier run got as far as the push
      say "  $name: this host is already a recipient; no join to accept"
      mark "join.$k"
      return 0
    fi
    fp="$(printf '%s\n' "$out" | grep -E '^fingerprint: ([0-9a-f]{4} ){7}[0-9a-f]{4}$' | head -n1 | sed 's/^fingerprint: //' || true)"
    [ -n "$fp" ] || die "join request printed no 'fingerprint: <8 groups of 4 hex>' line; refusing to continue"
    mark "join.$k"
    job_note join "on a member host: claude-memsync join accept $HOST --repo $k --fingerprint \"$fp\"   (check here: claude-memsync join status --repo $k)"
  fi
}

# jr_emit <index> <rc>: print a finished job's block (header, then its stdout,
# then its stderr) and take over what it reported.
jr_emit() {
  local i="$1" rc="$2" l f
  printf -- '-- %s\n' "${R_NAMES[$i]}"
  cat "$RUN_DIR/$i.out"
  cat "$RUN_DIR/$i.err" >&2
  if [ "$rc" -ne 0 ]; then FAILED_REPOS="${FAILED_REPOS:+$FAILED_REPOS }${R_NAMES[$i]}"; fi
  if [ -s "$RUN_DIR/$i.key" ]; then while IFS= read -r l; do KEYS_CREATED+=("$l"); done <"$RUN_DIR/$i.key"; fi
  if [ -s "$RUN_DIR/$i.join" ]; then while IFS= read -r l; do PENDING_JOINS+=("$l"); done <"$RUN_DIR/$i.join"; fi
  for f in out err key join; do rm -f "$RUN_DIR/$i.$f"; done
}

# run_phase <title> <function> <warm-up 0|1>: the function for every repo, up
# to $JOBS at a time, blocks in plan order. With warm-up the first repo runs
# alone: the first init of a host generates the shared age identity, and two
# inits must not race to make it. After a failure nothing new starts; the jobs
# running finish, every failure is reported, and the run exits 1.
run_phase() {
  step "$1"
  local i n=${#R_NAMES[@]} ns=""
  FAILED_REPOS=""
  if [ "$DRY_RUN" -eq 1 ]; then   # nothing is created: one after another, in this shell
    for ((i = 0; i < n; i++)); do printf -- '-- %s\n' "${R_NAMES[$i]}"; "$2" "$i"; done
    return 0
  fi
  JR_MAX="$JOBS" JR_JD="$RUN_DIR"
  jr_run "$2" "$n" "$3"
  if [ -n "$FAILED_REPOS" ]; then
    for i in $JR_NOT_STARTED; do ns="${ns:+$ns }${R_NAMES[$i]}"; done
    [ -z "$ns" ] || printf 'not attempted after the failure: %s\n' "$ns" >&2
    # shellcheck disable=SC2086
    set -- $FAILED_REPOS
    die "$# repo(s) failed: $FAILED_REPOS"
  fi
}

# mint_run_token: the org token is minted once, in this process, before any job
# starts (org_token keeps it in the run's private dir); init and doctor are then
# handed that file, so a key command or app-token never runs once per job.
mint_run_token() {
  [ -n "$RUN_DIR" ] && [ -z "$ORG_TOKEN_FILE" ] || return 0
  org_token >/dev/null || die "cannot read the org token; stopping"
  [ -s "$RUN_DIR/org-token" ] || die "the org token is empty; stopping"
  ORG_TOKEN_ARGS=(--token-file "$RUN_DIR/org-token")
}

step_repos_keys_init() {
  mint_run_token
  run_phase "c. Repos in $ORG (must be private; up to $JOBS at once, output in plan order)" repo_ensure_job 0
  step_ssh_aliases   # serial, and only now that every repo is known private
  run_phase "f. Per repo: deploy key and init (up to $JOBS at once, output in plan order)" repo_keys_init 1
}

# Token split: the bootstrap uses the org token (init, repo create, doctor).
# The daemon's hourly re-check never uses it: recheckTokenCmd is false. With
# --check-app the re-check mints from this host's own Metadata-read key
# (githubApp in each config; the key file and its fingerprint in apps.json and
# the registry). Without it, githubTokenFile points at a Metadata-read PAT file
# if the owner put one there. This script never creates or writes that PAT file.
step_token_split() {
  step "f2. Daemon re-check token"
  local tf="$CFG/github-token" i k cfg usefile=0 fp="" slug="$CHECK_APP" reg
  if [ -n "$CHECK_APP" ]; then
    say "  check app $CHECK_APP (app id $CHECK_APP_ID, org $ORG): key file $CHECK_KEY_FILE"
    if [ "$DRY_RUN" -eq 1 ]; then
      say "  would record the key's fingerprint in $APPS and, if routes.json names a registry, with: claude-memsync host check-key --fingerprint <fingerprint> --app $slug"
    else
      fp="$("$BIN/claude-memsync" app key-fingerprint --key-file "$CHECK_KEY_FILE")" || die "cannot fingerprint the check key"
      printf '%s' "$fp" | grep -Eq '^SHA256:[A-Za-z0-9+/=_-]+$' || die "unexpected key fingerprint output"
      apps_edit upsert "$CHECK_APP" "$(app_entry "$CHECK_APP" "$CHECK_APP_ID" "$CHECK_KEY_FILE" "" "$fp")"
      say "  wrote the $CHECK_APP entry to $APPS (0600) with key fingerprint $fp"
      reg="$(python3 -I -c 'import json, sys
verdict = None
try:
    verdict = "REGISTRY" if json.load(open(sys.argv[1])).get("registry") else "NOREGISTRY"
except BaseException as e:
    verdict = "ERR " + type(e).__name__
print(verdict)' "$ROUTES")" || die "cannot read the registry setting of $ROUTES; refusing to go on"
      case "$reg" in
        REGISTRY)
          if "$BIN/claude-memsync" host check-key --fingerprint "$fp" --app "$slug"; then :; else
            warn "could not record the check key in the registry; run: claude-memsync host check-key --fingerprint $fp --app $slug (on a writable host)"
            NEXT_STEPS+=("record the check key: claude-memsync host check-key --fingerprint $fp --app $slug")
          fi ;;
        NOREGISTRY) say "  no registry in routes.json: the check key is not recorded there" ;;
        *) die "cannot tell whether $ROUTES names a registry ($reg); refusing to go on" ;;
      esac
    fi
  elif guard_ok token_file_ok "$tf"; then   # the same checks as for the other secret files
    usefile=1; say "  found $tf (0600): the daemon re-check will use it"
  else
    warn "no Metadata-read PAT at $tf (0600): the hourly private re-check is OFF until it exists. Add it with: install -m 600 /dev/stdin $tf  (paste the token, then Ctrl-D), then re-run this bootstrap. Or use --check-app."
    NEXT_STEPS+=("add the Metadata-read PAT file $tf (mode 0600) to turn the hourly re-check on")
  fi
  for i in "${!R_KEYS[@]}"; do
    k="${R_KEYS[$i]}"; cfg="$SYNC_ROOT/$k/config.json"
    if [ -n "$CHECK_APP" ]; then say "  $k: githubApp=$CHECK_APP, recheckTokenCmd=false"
    else say "  $k: recheckTokenCmd=false$([ "$usefile" -eq 1 ] && echo ", githubTokenFile=$tf")"; fi
    [ "$DRY_RUN" -eq 1 ] && continue
    python3 -I - "$cfg" "$([ "$usefile" -eq 1 ] && echo "$tf")" "$CHECK_APP" <<'PY'
import json, sys
p, tf, app = sys.argv[1], sys.argv[2], sys.argv[3]
d = json.load(open(p))
d["recheckTokenCmd"] = False
d.pop("githubTokenCmd", None)   # never leave the org-token command in a daemon-read config
d.pop("githubTokenFile", None)  # nor the org-token file; only the check key or Metadata-read PAT below
d.pop("githubApp", None)
if app:
    d["githubApp"] = app
elif tf:
    d["githubTokenFile"] = tf
json.dump(d, open(p, "w"), indent=2)
PY
  done
}

# ---------------------------------------------------------------------------
# g-i. Home route, plugin, service
# ---------------------------------------------------------------------------
step_home() {
  step "g. Home folder route"
  if done_p home; then say "  already pinned"; return 0; fi
  run "$BIN/claude-memsync" route home "$HOME_KEY" --yes
  mark home
}

step_plugin() {
  step "h. Plugin memory-notices"
  if done_p plugin; then say "  already installed"; return 0; fi
  if [ "$BUILD_FROM_SOURCE" -eq 0 ] && { [ -z "$SRC" ] || [ ! -d "$SRC/.git" ]; }; then
    say "  plugin step skipped: a prebuilt install has no source checkout"
    NEXT_STEPS+=("install the plugin from a clone of the matching commit: claude plugin marketplace add <clone> && claude plugin install memory-notices@claude-utils")
    return 0
  fi
  if [ "$DRY_RUN" -eq 0 ] && ! command -v claude >/dev/null; then
    warn "claude not found; skipping the plugin"
    NEXT_STEPS+=("install Claude Code, then: claude plugin marketplace add $SRC && claude plugin install memory-notices@claude-utils")
    return 0
  fi
  run claude plugin marketplace add "$SRC"
  run claude plugin install memory-notices@claude-utils
  mark plugin
}

DOCTOR_RESULT="not run"
step_service() {
  step "i. Service"
  say "  Note: install and start may need you to approve the action in the Claude session."
  if ! done_p service; then
    run "$BIN/claude-memsync" install
    run "$BIN/claude-memsync" start
    mark service
  else
    say "  service already installed and started"
  fi
  if [ "$DRY_RUN" -eq 0 ]; then
    "$BIN/claude-memsync" status || warn "status reported a problem"
    if "$BIN/claude-memsync" doctor --org "$ORG" "${ORG_TOKEN_ARGS[@]}"; then
      DOCTOR_RESULT="ok"
    else
      DOCTOR_RESULT="FAILED (see output above)"
    fi
  else
    run "$BIN/claude-memsync" status
    run "$BIN/claude-memsync" doctor --org "$ORG" "${ORG_TOKEN_ARGS[@]}"
  fi
}

step_summary() {
  step "j. Summary"
  local i
  say "  host: $HOST   role: $ROLE   commit: $SHORT"
  say "  repos:"
  for i in "${!R_NAMES[@]}"; do say "    $ORG/${R_NAMES[$i]} (${R_KINDS[$i]})"; done
  say "  keys created (names only): ${KEYS_CREATED[*]:-none this run}"
  say "  doctor: $DOCTOR_RESULT"
  org_banner
  if [ ${#PENDING_JOINS[@]} -gt 0 ]; then
    say "  pending join requests (a member host must accept):"
    for i in "${!PENDING_JOINS[@]}"; do say "    ${PENDING_JOINS[$i]}"; done
  fi
  say "  next:"
  say "    - run /reload-plugins in each open Claude session, or restart the sessions, so the memory-notices hooks load"
  for i in ${NEXT_STEPS[@]+"${!NEXT_STEPS[@]}"}; do say "    - ${NEXT_STEPS[$i]}"; done
  [ ${#PENDING_JOINS[@]} -eq 0 ] || say "    - after the join is accepted: claude-memsync status"
}

# ---------------------------------------------------------------------------
preflight
if [ "$ROLE" = personal-dev ]; then discover_projects; fi
check_sync_dirs
confirm_plan
step_build
check_app_scope
step_routes
step_ssh
step_repos_keys_init
step_token_split
step_home
step_plugin
step_service
step_summary
admin_entry_remove
