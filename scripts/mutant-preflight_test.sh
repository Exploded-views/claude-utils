#!/usr/bin/env bash
# Test of scripts/mutant-preflight.sh against fixture repos only: it never
# touches the real shared repo. Stock macOS tools only.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
S="$ROOT/scripts/mutant-preflight.sh"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
W="$(cd "$W" && pwd -P)"
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
G() { git -c core.fsmonitor=false "$@"; }
export CLAUDE_UTILS_SHARED="$W/shared"
G init -q "$W/shared"
G init -q "$W/copy"
G -C "$W/shared" worktree add -q "$W/wt" -b wt 2>/dev/null || { G -C "$W/shared" -c user.name=t -c user.email=t@t commit -q --allow-empty -m i; G -C "$W/shared" worktree add -q "$W/wt" -b wt; }
mkdir -p "$W/cwd"

passes()  { bash "$S" "$@" >"$W/out" 2>&1; }
refuses() { ! bash "$S" "$@" >"$W/out" 2>&1; }

check "a real copy with a clean test cwd passes" passes check "$W/copy" "$W/cwd"
check "a worktree of the shared repo is refused (.git is a file)" refuses check "$W/wt" "$W/cwd"
check "the shared repo itself is refused" refuses check "$W/shared" "$W/cwd"
mkdir -p "$W/shared/sub"; G init -q "$W/shared/sub"
check "a repo nested in the shared repo is refused" refuses check "$W/shared/sub" "$W/cwd"
ln -s "$W/shared/.git" "$W/linkcopy.git"; mkdir -p "$W/linked"; ln -s "$W/shared/.git" "$W/linked/.git"
check "a copy whose .git is a symlink is refused" refuses check "$W/linked" "$W/cwd"
check "a missing copy is refused" refuses check "$W/nope"
# A test cwd inside a repo is refused when git can climb out of it...
mkdir -p "$W/copy/deep"
check "a test cwd that reaches an enclosing repo is refused" refuses check "$W/copy" "$W/copy/deep"
# ...unless the runner's environment sets the ceiling (what the isolated tests do).
check "the same cwd with GIT_CEILING_DIRECTORIES set to its parent passes" env GIT_CEILING_DIRECTORIES="$W/copy" bash "$S" check "$W/copy" "$W/copy/deep"
# A repo's own root resolves inside the cwd.
check "a test cwd that is itself a repo root resolves inside itself and passes" passes check "$W/copy" "$W/copy"
check "default test cwd (fresh temp dir) passes" passes check "$W/copy"

st="$W/state"
check "record writes a sha256" bash -c "bash '$S' record '$st' && [ \$(wc -c <'$st') -ge 64 ]"
check "verify passes while the config is unchanged" passes verify "$st"
G config --file "$W/shared/.git/config" core.fsmonitor /mon
check "verify stops when the shared config changed" refuses verify "$st"
check "verify without a state file is refused" refuses verify "$W/missing"

printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
