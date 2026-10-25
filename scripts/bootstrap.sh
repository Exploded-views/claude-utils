#!/usr/bin/env bash
# claude-memsync host bootstrap.
#
#   bootstrap.sh --host <label> --role <role> [--sha <commit>] [--org Exploded-views]
#                [--workspace <dir>]... [--own-owner <owner>]... [--deny-owner <owner>]...
#                [--deny-term <term>]... [--repo-url <url>]
#                --org-token-file <path> | --org-token-cmd "<argv>" | --org-app <name>
#                [--allow-org <org>]... [--dry-run] [--yes]
#
# Read this script before running it. --dry-run prints every step without doing
# it. Everything lives under $HOME; the script never uses sudo, never prints a
# private key or a token, and never creates or joins a client or orchestrator
# repo. --org-app <name> mints the org token from the GitHub App entry <name> in
# apps.json (claude-memsync app-token --app <name>), once per org call. See
# docs/bootstrap.md.
set -euo pipefail

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
AGE_RECOVERY="age1d6jhrdfc0q3symtxzs6ay8efyvnx7g4mln9jvxckzstkaz4re9vqysptau"
UTILS_URL="${CLAUDE_UTILS_URL:-https://github.com/Exploded-views/claude-utils}"

HOST="" ROLE="" SHA="" ORG="Exploded-views" DRY_RUN=0 YES=0
WORKSPACES=() OWN_OWNERS=() DENY_OWNERS=() DENY_TERMS=() ALLOW_ORGS=() ORG_TOKEN_FILE="" ORG_TOKEN_CMD="" ORG_APP=""

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

# run <cmd...>: print the command, then run it unless --dry-run.
run() {
  printf '  $ %s\n' "$*"
  [ "$DRY_RUN" -eq 1 ] && return 0
  "$@"
}

usage() { sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

done_p() { [ -e "$STATE/$HOST.$1" ]; }
mark()   { [ "$DRY_RUN" -eq 1 ] && return 0; mkdir -p "$STATE"; : >"$STATE/$HOST.$1"; }

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# valid_name <repo>: only mem-* names, never client or orchestrator repos.
valid_name() {
  case "$1" in
    mem-client-* | mem-orch-*) return 1 ;;
  esac
  printf '%s' "$1" | grep -Eq '^mem-[a-z0-9][a-z0-9._-]*$'
}

# is_denied <owner> <repo> (both lowercase): client owner or deny term match.
is_denied() {
  local t
  [ ${#DENY_OWNERS[@]} -eq 0 ] || ! in_list "$1" "${DENY_OWNERS[@]}" || return 0
  for t in ${DENY_TERMS[@]+"${DENY_TERMS[@]}"}; do
    case "$1/$2" in *"$t"*) return 0 ;; esac
  done
  return 1
}

add_repo() { # key kind name
  valid_name "$3" || die "refusing repo name '$3' (only mem-* names; never mem-client-* or mem-orch-*)"
  R_KEYS+=("$1"); R_KINDS+=("$2"); R_NAMES+=("$3")
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
    --workspace)  [ $# -ge 2 ] || usage; WORKSPACES+=("$2"); shift 2 ;;
    --own-owner)  [ $# -ge 2 ] || usage; OWN_OWNERS+=("$(lower "$2")"); shift 2 ;;
    --deny-owner) [ $# -ge 2 ] || usage; DENY_OWNERS+=("$(lower "$2")"); shift 2 ;;
    --deny-term)  [ $# -ge 2 ] || usage; DENY_TERMS+=("$(lower "$2")"); shift 2 ;;
    --repo-url)   [ $# -ge 2 ] || usage; UTILS_URL="$2"; shift 2 ;;
    --org-token-file) [ $# -ge 2 ] || usage; ORG_TOKEN_FILE="$2"; shift 2 ;;
    --org-token-cmd)  [ $# -ge 2 ] || usage; ORG_TOKEN_CMD="$2"; shift 2 ;;
    --org-app)        [ $# -ge 2 ] || usage; ORG_APP="$2"; shift 2 ;;
    --allow-org)  [ $# -ge 2 ] || usage; ALLOW_ORGS+=("$(lower "$2")"); shift 2 ;;
    --dry-run)    DRY_RUN=1; shift ;;
    --yes)        YES=1; shift ;;
    -h | --help)  usage ;;
    *)            die "unknown argument: $1" ;;
  esac
done
[ -n "$HOST" ] || die "--host is required"
n_tok=0
for v in "$ORG_TOKEN_FILE" "$ORG_TOKEN_CMD" "$ORG_APP"; do [ -z "$v" ] || n_tok=$((n_tok + 1)); done
[ "$n_tok" -le 1 ] || die "give only one of --org-token-file, --org-token-cmd or --org-app"
[ "$n_tok" -eq 1 ] || die "--org-token-file <path>, --org-token-cmd \"<argv>\" or --org-app <name> is required: every org operation uses that token, never the ambient gh login"
if [ -n "$ORG_APP" ]; then
  printf '%s' "$ORG_APP" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$' || die "bad --org-app '$ORG_APP'"
  case "$HOME" in *[[:space:]\'\"\\]*) die "--org-app needs a HOME without spaces or quotes (the token command is passed as one string)" ;; esac
  ORG_TOKEN_CMD="$HOME/.local/bin/claude-memsync app-token --app $ORG_APP"
fi
# Arguments that hand the same token to claude-memsync (a file path or a command, never the token).
# An app is handed over as its app-token command, which claude-memsync runs per request.
if [ -n "$ORG_TOKEN_FILE" ]; then ORG_TOKEN_ARGS=(--token-file "$ORG_TOKEN_FILE"); else ORG_TOKEN_ARGS=(--token-cmd "$ORG_TOKEN_CMD"); fi
[ -n "$ROLE" ] || die "--role is required"
printf '%s' "$HOST" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]*$' || die "bad --host label '$HOST'"
printf '%s' "$ORG" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9-]*$' || die "bad --org '$ORG'"
if [ -z "$SHA" ]; then
  SHA="$(git -C "$(dirname "$0")" rev-parse HEAD 2>/dev/null || true)"
  [ -n "$SHA" ] || die "--sha is required when the script is not run from a git checkout"
fi
printf '%s' "$SHA" | grep -Eq '^[0-9a-f]{40}$' || die "--sha must be a full 40-hex commit"
SHORT="$(printf '%s' "$SHA" | cut -c1-12)"
SRC="$HOME/.local/src/claude-utils-$SHA"
LIB="$HOME/.local/lib/claude-memsync/$SHA"
if [ ${#DENY_OWNERS[@]} -eq 0 ] && [ ${#DENY_TERMS[@]} -eq 0 ]; then
  warn "no client owners or terms configured: client repos are only protected by routing"
fi
[ ${#ALLOW_ORGS[@]} -gt 0 ] || ALLOW_ORGS=(exploded-views)
ALLOW_ORGS+=("$(lower "$ORG")")
OWN_OWNERS+=(gitgat "$(lower "$ORG")")

case "$ROLE" in
  homelab)      ROLE_REPOS="$ROLE_homelab_REPOS"; HOME_KEY="$ROLE_homelab_HOME" ;;
  personal-dev) ROLE_REPOS="$ROLE_personal_dev_REPOS"; HOME_KEY="$ROLE_personal_dev_HOME" ;;
  *)            die "unknown role '$ROLE' (known: homelab, personal-dev). Client and orchestrator roles do not exist here." ;;
esac
for spec in $ROLE_REPOS; do
  IFS=: read -r k kind name <<<"$spec"
  add_repo "$k" "$kind" "$name"
done

say "claude-memsync bootstrap: host=$HOST role=$ROLE org=$ORG commit=$SHORT$([ "$DRY_RUN" -eq 1 ] && echo ' (dry run)')"

# ---------------------------------------------------------------------------
# a. Preflight
# ---------------------------------------------------------------------------
version_ge() { # have want (dotted, major.minor)
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n | head -n1)" = "$2" ]
}

# org_token prints the org token. It is only called inside the command
# substitution of org_gh (or the preflight check), so it never outlives a call.
org_token() {
  { set +x; } 2>/dev/null
  if [ -n "$ORG_TOKEN_FILE" ]; then
    [ -r "$ORG_TOKEN_FILE" ] || return 1
    tr -d '\r\n' <"$ORG_TOKEN_FILE"
  else
    local -a argv
    if [ -n "$ORG_APP" ] && [ ! -x "$BIN/claude-memsync" ]; then return 1; fi
    read -r -a argv <<<"$ORG_TOKEN_CMD"
    [ ${#argv[@]} -gt 0 ] || return 1
    "${argv[@]}" | tr -d '\r\n'
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
    if in_list "$lo" ${DENY_OWNERS[@]+"${DENY_OWNERS[@]}"}; then denied="${denied:+$denied, }$o"; fi
  done <<<"$orgs"
  [ -n "$extra" ] || return 0
  local b="!!! WARNING: this host's $label can reach $extra. An agent host holds an owner-level token with client-org reach."
  [ -z "$denied" ] || b="$b
!!! CLIENT ORG(S) reachable through the $label: $denied. This script never creates, touches or keys any repo outside $ORG."
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
  if orgs="$(org_gh api user/orgs --paginate --jq '.[].login' 2>/dev/null)"; then
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

GO_MODE=""
preflight() {
  step "a. Preflight"
  local gv
  command -v git >/dev/null || die "git not found"
  gv="$(git --version | sed -E 's/[^0-9]*([0-9]+\.[0-9]+).*/\1/')"
  version_ge "$gv" 2.26 || die "git $gv is too old (need >= 2.26)"
  for c in gh ssh-keygen ssh-keyscan python3; do
    command -v "$c" >/dev/null || die "$c not found"
  done
  gh auth status >/dev/null 2>&1 || die "gh is not logged in (run: gh auth login)"
  check_org_scope
  if command -v go >/dev/null && version_ge "$(go version | sed -E 's/.*go([0-9]+\.[0-9]+).*/\1/')" 1.25; then
    GO_MODE=go
  elif command -v docker >/dev/null && [ "$(uname -s)" = Linux ]; then
    GO_MODE=docker
  else
    die "need Go >= 1.25 (or docker on Linux). On macOS: brew install go"
  fi
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
  say "  ok (git $gv, gh logged in, build via $GO_MODE)"
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
$ws" seen=""
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    find "$d" -maxdepth 3 \( -name '.?*' ! -name .git -type d \) -prune -o -name .git -print 2>/dev/null
  done <<<"$roots" | sort -u | while IFS= read -r g; do dirname "$g"; done >"${TMPDIR:-/tmp}/bs-checkouts.$$" || true
  while IFS= read -r p; do
    [ -n "$p" ] || continue
    remote="$(git -C "$p" remote get-url origin 2>/dev/null || true)"
    case "$remote" in
      *github.com[:/]*) rest="$(printf '%s' "$remote" | sed -E 's#^.*github\.com[:/]##; s#\.git$##; s#/$##')" ;;
      *) NOT_SYNCED="${NOT_SYNCED}  $p (no github.com origin)
"; continue ;;
    esac
    owner="$(lower "${rest%%/*}")"; repo="$(lower "${rest#*/}")"
    if is_denied "$owner" "$repo"; then
      NOT_SYNCED="${NOT_SYNCED}  $p ($owner/$repo): REFUSED (client owner)
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
  done <"${TMPDIR:-/tmp}/bs-checkouts.$$"
  rm -f "${TMPDIR:-/tmp}/bs-checkouts.$$"
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
step_routes() {
  step "e. routes.json and denylists"
  local i k
  for k in $(printf '%s\n' ${R_KINDS[@]+"${R_KINDS[@]}"} | sort -u); do
    if [ -e "$CFG/denylist-$k.txt" ]; then
      say "  denylist-$k.txt exists (kept)"
    else
      run mkdir -p "$CFG"
      say "  writing $CFG/denylist-$k.txt (${#DENY_TERMS[@]} deny terms)"
      [ "$DRY_RUN" -eq 1 ] || { { [ ${#DENY_TERMS[@]} -eq 0 ] || printf '%s\n' "${DENY_TERMS[@]}"; } >"$CFG/denylist-$k.txt"; chmod 600 "$CFG/denylist-$k.txt"; }
    fi
  done
  if done_p routes; then say "  routes.json already written by this bootstrap"; return 0; fi
  if [ -e "$ROUTES" ] && [ "$YES" -eq 0 ]; then
    die "$ROUTES already exists; refusing to overwrite (pass --yes to replace it, a .bak is kept)"
  fi
  say "  writing $ROUTES (0600, default deny, foreignWriters for the old memsync)"
  [ "$DRY_RUN" -eq 1 ] && return 0
  mkdir -p "$CFG"
  [ ! -e "$ROUTES" ] || cp -p "$ROUTES" "$ROUTES.bak"
  local spec=""
  for i in "${!R_KEYS[@]}"; do spec="${spec}repo|${R_KEYS[$i]}|${R_NAMES[$i]}|${R_KINDS[$i]}
"; done
  spec="$spec$RULE_LINES"
  BS_SPEC="$spec" BS_SYNC_ROOT="$SYNC_ROOT" BS_HOME="$HOME" python3 -I - >"$ROUTES.tmp" <<'PY'
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
print(json.dumps(out, indent=2))
PY
  chmod 600 "$ROUTES.tmp"; mv "$ROUTES.tmp" "$ROUTES"
  mark routes
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
        --recipient "$AGE_RECOVERY" --archive-deletes --private-only \
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
# The daemon's hourly re-check never uses it: recheckTokenCmd is false, and
# githubTokenFile points at a Metadata-read PAT file if the owner put one
# there. This script never creates or writes that file.
step_token_split() {
  step "f2. Daemon re-check token"
  local tf="$CFG/github-token" i k cfg usefile=0
  if [ -f "$tf" ] && [ "$(stat -c %a "$tf" 2>/dev/null || stat -f %Lp "$tf")" = 600 ]; then
    usefile=1; say "  found $tf (0600): the daemon re-check will use it"
  else
    warn "no Metadata-read PAT at $tf (0600): the hourly private re-check is OFF until it exists. Add it with: install -m 600 /dev/stdin $tf  (paste the token, then Ctrl-D), then re-run this bootstrap."
    NEXT_STEPS+=("add the Metadata-read PAT file $tf (mode 0600) to turn the hourly re-check on")
  fi
  for i in "${!R_KEYS[@]}"; do
    k="${R_KEYS[$i]}"; cfg="$SYNC_ROOT/$k/config.json"
    say "  $k: recheckTokenCmd=false$([ "$usefile" -eq 1 ] && echo ", githubTokenFile=$tf")"
    [ "$DRY_RUN" -eq 1 ] && continue
    python3 -I - "$cfg" "$([ "$usefile" -eq 1 ] && echo "$tf")" <<'PY'
import json, sys
p, tf = sys.argv[1], sys.argv[2]
d = json.load(open(p))
d["recheckTokenCmd"] = False
d.pop("githubTokenCmd", None)   # never leave the org-token command in a daemon-read config
d.pop("githubTokenFile", None)  # nor the org-token file; only the Metadata-read PAT below
if tf:
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
step_repos
step_keys
step_routes
step_init
step_token_split
step_home
step_plugin
step_service
step_summary
