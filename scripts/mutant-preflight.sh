#!/usr/bin/env bash
# Guard rails for a mutation runner, so a mutant can never touch the shared repo.
#
#   mutant-preflight.sh check  <copy> [testcwd]   refuse unless it is safe to start
#   mutant-preflight.sh record <statefile>        save sha256 of the shared .git/config
#   mutant-preflight.sh verify <statefile>        exit 1 if that sha256 changed
#
# check refuses (exit 1) unless
#   - <copy> is a throwaway COPY: its .git is a real directory (not a worktree's
#     gitfile, not a symlink), and neither <copy> nor its .git resolves to or
#     under the shared repo (default ~/code/claude-utils; CLAUDE_UTILS_SHARED
#     overrides it); and
#   - `git rev-parse --git-dir` run from the test cwd (default: a fresh temp
#     dir), in the caller's environment (so the runner's isolation, such as
#     GIT_CEILING_DIRECTORIES, counts), fails or resolves inside that cwd.
# A runner calls `record` before and `verify` after every mutant, and stops on
# a change.
set -euo pipefail
SHARED="${CLAUDE_UTILS_SHARED:-$HOME/code/claude-utils}"
die() { echo "mutant-preflight: $*" >&2; exit 1; }
real() { (cd "$1" 2>/dev/null && pwd -P); }
sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
shared_cfg() { local d; d="$(real "$SHARED")" || die "shared repo $SHARED not found"; echo "$d/.git/config"; }
under() { case "$1/" in "$2"/*) return 0 ;; esac; return 1; } # $1 equals or is under $2

cmd="${1:-}"; shift || true
case "$cmd" in
check)
  copy="${1:-}"; [ -n "$copy" ] || die "usage: check <copy> [testcwd]"
  rc="$(real "$copy")" || die "$copy is not a directory"
  sh_real="$(real "$SHARED")" || die "shared repo $SHARED not found"
  [ -L "$rc/.git" ] && die "$copy/.git is a symlink"   # fs-ok: a stat error falls to the next refusal
  [ -d "$rc/.git" ] || die "$copy/.git is not a directory (a worktree or not a repo): the runner must work in a throwaway COPY"   # fs-ok: absent refuses
  gd="$(real "$rc/.git")"
  under "$rc" "$sh_real" && die "$copy is the shared repo or inside it"
  under "$gd" "$sh_real/.git" && die "$copy/.git is under the shared repo's .git"
  under "$sh_real" "$rc" && die "$copy contains the shared repo"
  cwd="${2:-}"
  if [ -z "$cwd" ]; then tmpcwd="$(mktemp -d)"; trap 'rm -rf "$tmpcwd"' EXIT; cwd="$tmpcwd"; fi
  cr="$(real "$cwd")" || die "test cwd $cwd is not a directory"
  if out="$(cd "$cr" && git -c core.fsmonitor=false rev-parse --absolute-git-dir 2>/dev/null)"; then
    out="$(real "$out")" || die "git dir $out is not resolvable"
    under "$out" "$cr" || die "git from the test cwd $cr reaches a repo outside it: $out"
  fi
  echo "mutant-preflight: ok"
  ;;
record)
  [ -n "${1:-}" ] || die "usage: record <statefile>"
  sha "$(shared_cfg)" >"$1"
  ;;
verify)
  [ -f "${1:-}" ] || die "usage: verify <statefile> (run record first)"   # fs-ok: absent refuses
  before="$(cat "$1")"; after="$(sha "$(shared_cfg)")"
  [ "$before" = "$after" ] || die "the shared .git/config changed ($before -> $after): STOP"
  ;;
*) die "usage: mutant-preflight.sh check|record|verify ..." ;;
esac
