#!/usr/bin/env bash
# claude-memsync host bootstrap.
#
#   bootstrap.sh --host <label> --role <role> --org <org> --recovery-recipient <age1pq1...>
#                [--sha <commit>] [--prebuilt-dir <dir>]
#                [--workspace <dir>]... [--own-owner <owner>]...
#                [--keep-out-owner <owner>]... [--keep-out-term <term>]...   (optional)
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

HOST="" ROLE="" SHA="" ORG="" AGE_RECOVERY="" DRY_RUN=0 YES=0
WORKSPACES=() OWN_OWNERS=() KEEP_OUT_OWNERS=() KEEP_OUT_TERMS=() ALLOW_ORGS=() ORG_TOKEN_FILE="" ORG_TOKEN_CMD="" ORG_APP=""
ORG_APP_ID="" ORG_APP_KEY_CMD="" CHECK_APP="" CHECK_APP_ID="" CHECK_KEY_FILE="" APP_API_BASE="${BOOTSTRAP_APP_API_BASE:-}" ADMIN_WRITTEN=0

R_KEYS=() R_NAMES=() R_KINDS=()   # planned repos (parallel arrays)
RULE_LINES=""                     # "pin|folder|key" and "remote|host/owner/repo|key"
NOT_SYNCED=""                     # human-readable list
KEYS_CREATED=() PENDING_JOINS=() NEXT_STEPS=()

BIN="$HOME/.local/bin"
CFG="$HOME/.config/claude-memsync"
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

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

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
  for t in sed awk grep tr cut sort head find stat mktemp id uname dirname chmod ln; do
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

# valid_name <repo>: only mem-* names, never client or orchestrator repos.
valid_name() {
  case "$1" in
    mem-client-* | mem-orch-*) return 1 ;;
  esac
  printf '%s' "$1" | grep -Eq '^mem-[a-z0-9][a-z0-9._-]*$'
}

# is_kept_out <owner> <repo> (both lowercase): keep-out owner or term match.
is_kept_out() {
  local t
  [ ${#KEEP_OUT_OWNERS[@]} -eq 0 ] || ! in_list "$1" "${KEEP_OUT_OWNERS[@]}" || return 0
  for t in ${KEEP_OUT_TERMS[@]+"${KEEP_OUT_TERMS[@]}"}; do
    case "$1/$2" in *"$t"*) return 0 ;; esac
  done
  return 1
}

add_repo() { # key kind name
  valid_name "$3" || die "refusing repo name '$3' (only mem-* names; never mem-client-* or mem-orch-*)"
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
  case "$f" in /*) ;; *) die "--check-key-file must be an absolute path" ;; esac
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
# One exit trap for everything that must happen on every exit path (success,
# error, signal): remove the temporary admin app entry, and, with
# --delete-org-token-file, securely delete the org token file.
# ---------------------------------------------------------------------------
DELETE_TOKEN_FILE=0 TOKEN_FILE_ARMED=0
secure_delete() { # file: shred -u if there, else overwrite with zeros and remove
  local f="$1" n
  [ -e "$f" ] || [ -L "$f" ] || return 0
  if command -v shred >/dev/null 2>&1; then
    shred -u -- "$f" 2>/dev/null && return 0
  fi
  if [ -f "$f" ] && [ ! -L "$f" ]; then
    n="$(wc -c <"$f" 2>/dev/null | tr -d ' ')"
    if command -v dd >/dev/null 2>&1; then dd if=/dev/zero of="$f" bs=1 count="${n:-0}" conv=notrunc 2>/dev/null || true; fi
  fi
  rm -f -- "$f"
}
token_file_remove() {
  [ "$TOKEN_FILE_ARMED" -eq 1 ] || return 0
  TOKEN_FILE_ARMED=0
  secure_delete "$ORG_TOKEN_FILE"
  if [ -e "$ORG_TOKEN_FILE" ]; then
    printf 'WARNING: could not delete the org token file %s; delete it by hand\n' "$ORG_TOKEN_FILE" >&2
  fi
}
on_exit() {
  local rc=$?
  trap - EXIT
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
check_token_file() {
  local f="$ORG_TOKEN_FILE" m u n
  case "$f" in /*) ;; *) die "--org-token-file must be an absolute path with --delete-org-token-file" ;; esac
  { [ -f "$f" ] && [ ! -L "$f" ]; } || die "org token file $f is missing or not a plain file"
  m="$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f")"
  u="$(stat -c %u "$f" 2>/dev/null || stat -f %u "$f")"
  n="$(stat -c %h "$f" 2>/dev/null || stat -f %l "$f")"
  [ "$m" = 600 ] || die "org token file $f must be mode 0600 (is $m)"
  [ "$u" = "$(id -u)" ] || die "org token file $f must be owned by you"
  [ "$n" = 1 ] || die "org token file $f must have one hard link (has $n)"
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --host)       [ $# -ge 2 ] || usage; HOST="$2"; shift 2 ;;
    --role)       [ $# -ge 2 ] || usage; ROLE="$2"; shift 2 ;;
    --sha)        [ $# -ge 2 ] || usage; SHA="$2"; shift 2 ;;
    --org)        [ $# -ge 2 ] || usage; ORG="$2"; shift 2 ;;
    --recovery-recipient) [ $# -ge 2 ] || usage; AGE_RECOVERY="$2"; shift 2 ;;
    --workspace)  [ $# -ge 2 ] || usage; WORKSPACES+=("$2"); shift 2 ;;
    --own-owner)  [ $# -ge 2 ] || usage; OWN_OWNERS+=("$(lower "$2")"); shift 2 ;;
    --keep-out-owner | --deny-owner) [ $# -ge 2 ] || usage; KEEP_OUT_OWNERS+=("$(lower "$2")"); shift 2 ;;
    --keep-out-term | --deny-term) [ $# -ge 2 ] || usage; KEEP_OUT_TERMS+=("$(lower "$2")"); shift 2 ;;
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
check_prereqs
[ -n "$HOST" ] || die "--host is required"
[ -n "$ORG" ] || die "--org <org> is required: the GitHub organisation that holds the memory repos (there is no default)"
[ -n "$AGE_RECOVERY" ] || die "--recovery-recipient <age1pq1...> is required: your own offline post-quantum recovery PUBLIC key (generate one as docs/bootstrap.md describes; there is no default)"
printf '%s' "$AGE_RECOVERY" | grep -Eq '^age1pq1[023456789acdefghjklmnpqrstuvwxyz]{1000,}$' || die "--recovery-recipient must be a post-quantum age1pq1... public key (a classic age1... or ssh key is refused: repos are post-quantum by default)"
n_tok=0
for v in "$ORG_TOKEN_FILE" "$ORG_TOKEN_CMD" "$ORG_APP"; do [ -z "$v" ] || n_tok=$((n_tok + 1)); done
[ "$n_tok" -le 1 ] || udie "give only one of --org-token-file, --org-token-cmd or --org-app"
[ "$n_tok" -eq 1 ] || udie "--org-token-file <path>, --org-token-cmd \"<argv>\" or --org-app <name> is required: every org operation uses that token, never the ambient gh login"
if [ "$DELETE_TOKEN_FILE" -eq 1 ]; then
  [ -n "$ORG_TOKEN_FILE" ] || udie "--delete-org-token-file is only valid with --org-token-file <path>"
  check_token_file
  TOKEN_FILE_ARMED=1   # from here on every exit path deletes the file
fi
if [ -n "$ORG_APP" ]; then
  printf '%s' "$ORG_APP" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$' || die "bad --org-app '$ORG_APP'"
  case "$HOME" in *[[:space:]\'\"\\]*) die "--org-app needs a HOME without spaces or quotes (the token command is passed as one string)" ;; esac
  ORG_TOKEN_CMD="$HOME/.local/bin/claude-memsync app-token --app $ORG_APP"
fi
if [ -n "$ORG_APP_ID" ] || [ -n "$ORG_APP_KEY_CMD" ]; then
  [ -n "$ORG_APP" ] || die "--org-app-id / --org-app-key-cmd go with --org-app <name>"
  { [ -n "$ORG_APP_ID" ] && [ -n "$ORG_APP_KEY_CMD" ]; } || die "give both --org-app-id and --org-app-key-cmd"
  printf '%s' "$ORG_APP_ID" | grep -Eq '^[0-9]{1,20}$' || die "bad --org-app-id (numeric)"
  split_args json "$ORG_APP_KEY_CMD" >/dev/null || die "unbalanced quote in --org-app-key-cmd"
fi
if [ -n "$CHECK_APP$CHECK_APP_ID$CHECK_KEY_FILE" ]; then
  { [ -n "$CHECK_APP" ] && [ -n "$CHECK_APP_ID" ] && [ -n "$CHECK_KEY_FILE" ]; } || die "give --check-app, --check-app-id and --check-key-file together"
  printf '%s' "$CHECK_APP" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$' || die "bad --check-app '$CHECK_APP'"
  printf '%s' "$CHECK_APP_ID" | grep -Eq '^[0-9]{1,20}$' || die "bad --check-app-id (numeric)"
  [ "$CHECK_APP" != "$ORG_APP" ] || die "--check-app must differ from --org-app: the admin app never serves the daemon"
  check_key_pause
fi
ORG_TOKEN_ARGV=()
if [ -n "$ORG_TOKEN_CMD" ]; then
  split_args json "$ORG_TOKEN_CMD" >/dev/null || die "unbalanced quote in --org-token-cmd"
  while IFS= read -r -d '' a; do ORG_TOKEN_ARGV+=("$a"); done < <(split_args nul "$ORG_TOKEN_CMD")
  [ ${#ORG_TOKEN_ARGV[@]} -gt 0 ] || die "--org-token-cmd is empty"
fi
# Arguments that hand the same token to claude-memsync (a file path or a command, never the token).
# An app is handed over as its app-token command, which claude-memsync runs per request.
if [ -n "$ORG_TOKEN_FILE" ]; then ORG_TOKEN_ARGS=(--token-file "$ORG_TOKEN_FILE"); else ORG_TOKEN_ARGS=(--token-cmd "$ORG_TOKEN_CMD"); fi
[ -n "$ROLE" ] || die "--role is required"
printf '%s' "$HOST" | grep -Eq '^[a-z0-9][a-z0-9-]{0,31}$' || die "bad --host label '$HOST' (it is the host's label in recipients, deploy keys and join requests: 1-32 characters of a-z, 0-9 and '-', starting with a letter or digit)"
printf '%s' "$ORG" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9-]*$' || die "bad --org '$ORG'"
for o in ${KEEP_OUT_OWNERS[@]+"${KEEP_OUT_OWNERS[@]}"}; do
  [ "$o" != "$(lower "$ORG")" ] || die "--org '$ORG' is also a keep-out owner; refusing"
done
if [ -z "$SHA" ]; then
  SHA="$(git -C "$(dirname "$0")" rev-parse HEAD 2>/dev/null || true)"
  [ -n "$SHA" ] || die "--sha is required when the script is not run from a git checkout"
fi
printf '%s' "$SHA" | grep -Eq '^[0-9a-f]{40}$' || die "--sha must be a full 40-hex commit"
SHORT="$(printf '%s' "$SHA" | cut -c1-12)"
SRC="$HOME/.local/src/claude-utils-$SHA"
LIB="$HOME/.local/lib/claude-memsync/$SHA"
if [ ${#KEEP_OUT_OWNERS[@]} -eq 0 ] && [ ${#KEEP_OUT_TERMS[@]} -eq 0 ]; then
  say "keep-out lists: none (routing alone decides what syncs)"
fi
ALLOW_ORGS+=("$(lower "$ORG")")
OWN_OWNERS+=("$(lower "$ORG")")

case "$ROLE" in
  homelab)      ROLE_REPOS="$ROLE_homelab_REPOS"; HOME_KEY="$ROLE_homelab_HOME" ;;
  personal-dev) ROLE_REPOS="$ROLE_personal_dev_REPOS"; HOME_KEY="$ROLE_personal_dev_HOME" ;;
  *)            die "unknown role '$ROLE' (known: homelab, personal-dev). There is no role for client or orchestrator repos." ;;
esac
for spec in $ROLE_REPOS; do
  IFS=: read -r k kind name <<<"$spec"
  add_repo "$k" "$kind" "$name"
done

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
    if [ -n "$ORG_APP" ] && [ ! -x "$BIN/claude-memsync" ]; then return 1; fi
    [ ${#ORG_TOKEN_ARGV[@]} -gt 0 ] || return 1
    "${ORG_TOKEN_ARGV[@]}" | tr -d '\r\n'
    [ "${PIPESTATUS[0]}" -eq 0 ]
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
BANNER_AMBIENT="" BANNER_TOKEN=""
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
check_token_readable() {
  local t
  t="$(org_token)" || die "cannot read the org token (--org-token-file / --org-token-cmd / --org-app); stopping"
  [ -n "$t" ] || die "the org token is empty; stopping"
  t=""
}
check_ambient_scope() {
  local orgs
  if orgs="$(gh api user/orgs --paginate --jq '.[].login' 2>/dev/null)"; then
    BANNER_AMBIENT="$(reach_banner "gh login" "$orgs")"
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
      BANNER_TOKEN="$(reach_banner "org token" "$orgs")"
    else
      warn "cannot list the repositories the app installation reaches"
    fi
  elif orgs="$(org_gh api user/orgs --paginate --jq '.[].login' 2>/dev/null)"; then
    BANNER_TOKEN="$(reach_banner "org token" "$orgs")"
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

apps_has() { # name: is there an entry of that name?
  python3 -I -c '
import json, sys
try:
    sys.exit(0 if any(a.get("name") == sys.argv[2] for a in json.load(open(sys.argv[1]))) else 1)
except Exception:
    sys.exit(1)' "$APPS" "$1"
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
    if [ -e "$APPS" ] && apps_has "$ORG_APP"; then die "$APPS already has an entry named $ORG_APP; refusing to replace it"; fi
    say "  would add the app entry $ORG_APP to $APPS (key command only), and remove it when the run ends"
    return 0
  fi
  # An entry the owner put there is never replaced, and so never deleted.
  if [ -e "$APPS" ] && apps_has "$ORG_APP"; then
    die "$APPS already has an entry named $ORG_APP; refusing to replace it (and delete it at the end). Drop --org-app-id/--org-app-key-cmd to use that entry, or remove it first."
  fi
  ADMIN_WRITTEN=1
  apps_edit upsert "$ORG_APP" "$(app_entry "$ORG_APP" "$ORG_APP_ID" "" "$(split_args json "$ORG_APP_KEY_CMD")")"
}
admin_entry_remove() {
  [ "$ADMIN_WRITTEN" -eq 1 ] || return 0
  ADMIN_WRITTEN=0
  apps_edit remove "$ORG_APP" || printf 'WARNING: could not remove the %s entry from %s; remove it by hand\n' "$ORG_APP" "$APPS" >&2
}

# check_key_file: the per-host check key must be a plain file of ours, 0600, one link.
check_key_file() {
  local f="$CHECK_KEY_FILE" m u n
  case "$f" in /*) ;; *) die "--check-key-file must be an absolute path" ;; esac
  { [ -f "$f" ] && [ ! -L "$f" ]; } || die "check key file $f is missing or not a plain file"
  m="$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f")"
  u="$(stat -c %u "$f" 2>/dev/null || stat -f %u "$f")"
  n="$(stat -c %h "$f" 2>/dev/null || stat -f %l "$f")"
  [ "$m" = 600 ] || die "check key file $f must be mode 0600 (is $m)"
  [ "$u" = "$(id -u)" ] || die "check key file $f must be owned by you"
  [ "$n" = 1 ] || die "check key file $f must have one hard link (has $n)"
}

preflight() {
  step "a. Preflight"
  gh auth status >/dev/null 2>&1 || die "gh is not logged in (run: gh auth login)"
  [ -z "$CHECK_APP" ] || check_key_file
  admin_entry_add
  check_org_scope
  command -v claude >/dev/null || warn "claude not found on PATH; the plugin step will be skipped"

  # Predecessor writers: refuse while any is active.
  local old="$HOME/.config/memsync/config.json"
  if [ -f "$old" ] && python3 -I - "$old" <<'PY'
import json, sys
try:
    v = json.load(open(sys.argv[1])).get("repo")
except Exception:
    v = "unreadable"
sys.exit(0 if v else 1)
PY
  then
    die "the old memsync is configured ($old has \"repo\"). Disable it first (move the file aside)."
  fi
  if command -v systemctl >/dev/null; then
    for u in memsync.timer memsync.service; do
      if systemctl --user is-enabled "$u" >/dev/null 2>&1 || systemctl --user is-active "$u" >/dev/null 2>&1; then
        die "predecessor unit $u is enabled or active. Stop and disable it first."
      fi
    done
  fi
  say "  ok (gh logged in${GO_MODE:+, build via $GO_MODE})"
}

check_sync_dirs() { # run after the plan is final
  local i k
  for i in "${!R_KEYS[@]}"; do
    k="${R_KEYS[$i]}"
    if [ -e "$SYNC_ROOT/$k" ] && ! done_p "init.$k"; then
      die "$SYNC_ROOT/$k already exists and was not made by this bootstrap; refusing to touch it"
    fi
  done
}

# ---------------------------------------------------------------------------
# personal-dev: workspaces and project discovery
# ---------------------------------------------------------------------------
esc_folder() { printf '%s' "$1" | sed 's/[^A-Za-z0-9]/-/g'; }

in_list() { # needle items...
  local n="$1"; shift
  local x
  for x in "$@"; do [ "$x" = "$n" ] && return 0; done
  return 1
}

discover_projects() {
  step "Plan: workspaces and project discovery"
  local w ws="" d p remote owner repo rest key name
  if [ ${#WORKSPACES[@]} -eq 0 ] && [ -t 0 ]; then
    say "  Workspace parent folders (not git repos), one per line, empty line to finish:"
    while printf '  workspace> ' && IFS= read -r w && [ -n "$w" ]; do WORKSPACES+=("$w"); done
  fi
  for w in ${WORKSPACES[@]+"${WORKSPACES[@]}"}; do
    case "$w" in "~"*) w="$HOME${w#\~}" ;; esac
    [ -d "$w" ] || die "workspace '$w' is not a directory"
    w="$(cd "$w" && pwd -P)"
    [ -e "$w/.git" ] && die "workspace '$w' is itself a git checkout; give its parent folder"
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
    if is_kept_out "$owner" "$repo"; then
      NOT_SYNCED="${NOT_SYNCED}  $p ($owner/$repo): REFUSED (keep-out)
"; continue
    fi
    if ! in_list "$owner" "${OWN_OWNERS[@]}"; then
      NOT_SYNCED="${NOT_SYNCED}  $p ($owner/$repo): not synced (third-party)
"; continue
    fi
    name="mem-project-$repo"; key="project-$repo"
    if ! printf '%s' "$name" | grep -Eq '^mem-project-[a-z0-9][a-z0-9._-]*$'; then
      NOT_SYNCED="${NOT_SYNCED}  $p ($owner/$repo): not synced (repo name unusable)
"; continue
    fi
    in_list "$key" ${R_KEYS[@]+"${R_KEYS[@]}"} && continue
    add_repo "$key" project "$name"
    RULE_LINES="${RULE_LINES}remote|github.com/$owner/$repo|$key
"
    seen="$seen $repo"
  done <<<"$checkouts"
  say "  own projects found:${seen:- none}"
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
  say "  encryption: age on every repo, recovery recipient $AGE_RECOVERY"
  if [ "$DRY_RUN" -eq 1 ] || [ "$YES" -eq 1 ]; then return 0; fi
  [ -t 0 ] || die "not a terminal: pass --yes to accept this plan"
  printf 'Type yes to create these repos and keys: '
  local ans; IFS= read -r ans
  [ "$ans" = yes ] || die "not confirmed; nothing changed"
}

# ---------------------------------------------------------------------------
# b. Source and build
# ---------------------------------------------------------------------------
step_build() {
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
  run mkdir -p "$BIN"
  local b
  for b in claude-memsync claude-memmerge; do
    [ ! -e "$BIN/$b" ] || [ -L "$BIN/$b" ] || die "$BIN/$b exists and is not a symlink"
    run ln -sfn "$LIB/$b" "$BIN/$b"
  done
}

# ---------------------------------------------------------------------------
# c. Repos (private only)
# ---------------------------------------------------------------------------
check_private() { # repo
  local out
  out="$(org_gh api "repos/$ORG/$1")" || die "cannot read repos/$ORG/$1"
  printf '%s' "$out" | python3 -I -c '
import json, sys
d = json.load(sys.stdin)
sys.exit(0 if d.get("private") is True and d.get("visibility") == "private" else 1)' \
    || die "repo $ORG/$1 is NOT private; refusing to continue"
}

step_repos() {
  step "c. Repos in $ORG (must be private)"
  local i name out
  for i in "${!R_NAMES[@]}"; do
    name="${R_NAMES[$i]}"
    valid_name "$name" || die "refusing repo name $name"
    if [ "$DRY_RUN" -eq 1 ]; then
      say "  would check $ORG/$name, create it with --private if missing, and verify private=true"
      continue
    fi
    if out="$(org_gh api "repos/$ORG/$name" 2>&1)"; then
      say "  $name exists"
    elif printf '%s' "$out" | grep -qiE 'not found|404'; then
      run org_gh repo create "$ORG/$name" --private
    else
      die "cannot look up $ORG/$name: $out"
    fi
    check_private "$name"
    say "  $name is private"
  done
}

# ---------------------------------------------------------------------------
# d. Deploy keys
# ---------------------------------------------------------------------------
add_known_host() { # label hostpattern scan-args...
  local label="$1" pat="$2"; shift 2
  local kh="$HOME/.ssh/known_hosts" tmp fp
  if [ -f "$kh" ] && ssh-keygen -F "$pat" -f "$kh" >/dev/null 2>&1; then
    say "  known_hosts already has $label"; return 0
  fi
  [ "$DRY_RUN" -eq 1 ] && { say "  would add $label to known_hosts if its fingerprint is $GITHUB_FP"; return 0; }
  tmp="$(mktemp)"
  ssh-keyscan -t ed25519 "$@" >"$tmp" 2>/dev/null || { rm -f "$tmp"; die "ssh-keyscan failed for $label"; }
  fp="$(ssh-keygen -lf "$tmp" 2>/dev/null | awk 'NR==1{print $2}')"
  if [ "$fp" != "$GITHUB_FP" ]; then
    rm -f "$tmp"; die "host key fingerprint for $label is '$fp', expected $GITHUB_FP; refusing"
  fi
  sed -E "s/^[^ ]+ /$pat /" "$tmp" >>"$kh"
  rm -f "$tmp"; chmod 600 "$kh"
  say "  added $label to known_hosts (fingerprint verified)"
}

step_keys() {
  step "d. Deploy keys"
  local i name key pub out
  run mkdir -p "$HOME/.ssh"
  run chmod 700 "$HOME/.ssh"
  add_known_host "github.com:22" "github.com" github.com
  add_known_host "ssh.github.com:443" "[ssh.github.com]:443" -p 443 ssh.github.com
  for i in "${!R_NAMES[@]}"; do
    name="${R_NAMES[$i]}"; key="$HOME/.ssh/${name}_ed25519"; pub="$key.pub"
    if done_p "keygen.$name"; then
      say "  key for $name already generated"
    else
      [ ! -e "$key" ] || die "$key already exists and was not made by this bootstrap; refusing to overwrite"
      run ssh-keygen -q -t ed25519 -N '' -C "$HOST-$name" -f "$key"
      [ "$DRY_RUN" -eq 1 ] || chmod 600 "$key"
      mark "keygen.$name"
      KEYS_CREATED+=("${name}_ed25519")
    fi
    if ! grep -qE "^Host gh-$name\$" "$HOME/.ssh/config" 2>/dev/null; then
      say "  adding ssh alias gh-$name (ssh.github.com:443)"
      if [ "$DRY_RUN" -eq 0 ]; then
        touch "$HOME/.ssh/config"; chmod 600 "$HOME/.ssh/config"
        {
          printf '\nHost gh-%s\n  HostName ssh.github.com\n  Port 443\n  User git\n  IdentityFile %s\n  IdentitiesOnly yes\n' "$name" "$key"
        } >>"$HOME/.ssh/config"
      fi
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
          die "gh repo deploy-key add failed for $name: $out"
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
  BS_SPEC="$spec" BS_SYNC_ROOT="$SYNC_ROOT" BS_HOME="$HOME" BS_OLD="$ROUTES" python3 -I - <<'PY'
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
out = {"repos": repos, "routes": pins + routes, "default": "deny",
       "foreignWriters": [
           {"file": home + "/.config/memsync/config.json", "jsonKeyNonEmpty": "repo"},
           {"systemdUnit": "memsync.timer"},
           {"systemdUnit": "memsync.service"},
           {"process": "memsync/bin/memsync"},
           {"claudePlugin": "memsync@memsync", "onlyIfFile": home + "/.config/memsync/config.json"}]}
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
  local k t new regen=0
  for k in $(printf '%s\n' ${R_KINDS[@]+"${R_KINDS[@]}"} | sort -u); do
    if [ -e "$CFG/denylist-$k.txt" ]; then
      say "  denylist-$k.txt exists (kept)"
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
      say "  the plan differs from the routes.json written by an earlier run (a new project or workspace): regenerating it; the old one is kept as $ROUTES.bak"
    elif [ "$YES" -eq 0 ]; then
      die "$ROUTES already exists and differs from this plan; refusing to overwrite it (pass --yes to replace it, a .bak is kept)"
    fi
  fi
  say "  writing $ROUTES (0600, default deny, foreignWriters for the old memsync)"
  [ "$DRY_RUN" -eq 1 ] && return 0
  mkdir -p "$CFG"
  [ ! -e "$ROUTES" ] || cp -p "$ROUTES" "$ROUTES.bak"
  printf '%s\n' "$new" >"$ROUTES.tmp"
  chmod 600 "$ROUTES.tmp"; mv "$ROUTES.tmp" "$ROUTES"
  mark routes
  # The rewritten file has lost the home pin; step_home puts it back.
  [ "$regen" -eq 0 ] || rm -f "$STATE/$HOST.home"
}

# ---------------------------------------------------------------------------
# f. init each repo
# ---------------------------------------------------------------------------
step_init() {
  step "f. Init each repo (age-encrypted, private-only)"
  local i k name kind url content out fp
  for i in "${!R_KEYS[@]}"; do
    k="${R_KEYS[$i]}"; name="${R_NAMES[$i]}"; kind="${R_KINDS[$i]}"
    url="git@gh-$name:$ORG/$name.git"
    content=0
    if ! done_p "init.$k"; then
      [ "$DRY_RUN" -eq 1 ] || { git ls-remote "$url" 2>/dev/null | grep -q . && content=1; }
      run "$BIN/claude-memsync" init --remote "$url" --sync-dir "$SYNC_ROOT/$k" --encrypt \
        --host-label "$HOST" --recovery-recipient "$AGE_RECOVERY" --archive-deletes --private-only \
        "${ORG_TOKEN_ARGS[@]}" --denylist "$CFG/denylist-$kind.txt"
      mark "init.$k"
      [ "$content" -eq 1 ] && mark "needjoin.$k"
    else
      say "  $k already initialised"
    fi
    if done_p "needjoin.$k" && ! done_p "join.$k"; then
      say "  $name already had content: this host has no decryption key there yet. Requesting to join."
      if [ "$DRY_RUN" -eq 1 ]; then
        say "  would run: claude-memsync join request --repo $k --label $HOST"
        continue
      fi
      out="$("$BIN/claude-memsync" join request --repo "$k" --label "$HOST" 2>&1)" || die "join request failed: $out"
      printf '%s\n' "$out"
      fp="$(printf '%s\n' "$out" | grep -E '^fingerprint: ([0-9a-f]{4} ){7}[0-9a-f]{4}$' | head -n1 | sed 's/^fingerprint: //' || true)"
      [ -n "$fp" ] || die "join request printed no 'fingerprint: <8 groups of 4 hex>' line; refusing to continue"
      mark "join.$k"
      PENDING_JOINS+=("on a member host: claude-memsync join accept $HOST --repo $k --fingerprint \"$fp\"   (check here: claude-memsync join status --repo $k)")
    fi
  done
}

# Token split: the bootstrap uses the org token (init, repo create, doctor).
# The daemon's hourly re-check never uses it: recheckTokenCmd is false. With
# --check-app the re-check mints from this host's own Metadata-read key
# (githubApp in each config; the key file and its fingerprint in apps.json and
# the registry). Without it, githubTokenFile points at a Metadata-read PAT file
# if the owner put one there. This script never creates or writes that PAT file.
step_token_split() {
  step "f2. Daemon re-check token"
  local tf="$CFG/github-token" i k cfg usefile=0 fp="" slug="$CHECK_APP"
  if [ -n "$CHECK_APP" ]; then
    say "  check app $CHECK_APP (app id $CHECK_APP_ID, org $ORG): key file $CHECK_KEY_FILE"
    if [ "$DRY_RUN" -eq 1 ]; then
      say "  would record the key's fingerprint in $APPS and, if routes.json names a registry, with: claude-memsync host check-key --fingerprint <fingerprint> --app $slug"
    else
      fp="$("$BIN/claude-memsync" app key-fingerprint --key-file "$CHECK_KEY_FILE")" || die "cannot fingerprint the check key"
      printf '%s' "$fp" | grep -Eq '^SHA256:[A-Za-z0-9+/=_-]+$' || die "unexpected key fingerprint output"
      apps_edit upsert "$CHECK_APP" "$(app_entry "$CHECK_APP" "$CHECK_APP_ID" "$CHECK_KEY_FILE" "" "$fp")"
      say "  wrote the $CHECK_APP entry to $APPS (0600) with key fingerprint $fp"
      if python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("registry") else 1)' "$ROUTES" 2>/dev/null; then
        if "$BIN/claude-memsync" host check-key --fingerprint "$fp" --app "$slug"; then :; else
          warn "could not record the check key in the registry; run: claude-memsync host check-key --fingerprint $fp --app $slug (on a writable host)"
          NEXT_STEPS+=("record the check key: claude-memsync host check-key --fingerprint $fp --app $slug")
        fi
      else
        say "  no registry in routes.json: the check key is not recorded there"
        NEXT_STEPS+=("when a registry is configured: claude-memsync host check-key --fingerprint $fp --app $slug")
      fi
    fi
  elif [ -f "$tf" ] && [ "$(stat -c %a "$tf" 2>/dev/null || stat -f %Lp "$tf")" = 600 ]; then
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
  say "    - restart Claude sessions so the memory-notices hooks load"
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
step_repos
step_keys
step_init
step_token_split
step_home
step_plugin
step_service
step_summary
admin_entry_remove
