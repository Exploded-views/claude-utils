#!/usr/bin/env bash
# no-delete-lint: skip-file   (this test's fixtures are the bad patterns on purpose)
# Test of scripts/lint-no-delete.sh: each fixture line must be flagged (or, for
# the "ok" ones, not), and the repo's own scripts/ must pass. Stock macOS tools only.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"
cleanup() {
  local f d
  for f in "$W"/*/*; do [ -f "$f" ] && rm -f "$f"; done
  for d in "$W"/*; do [ -d "$d" ] && rmdir "$d"; done
  rmdir "$W"
}
trap cleanup EXIT
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
n=0
flagged() { # script text: rejected, with a finding on a line of t.sh
  n=$((n + 1)); mkdir -p "$W/$n"; printf '#!/bin/bash\n%s\n' "$1" >"$W/$n/t.sh"
  ! bash "$ROOT/scripts/lint-no-delete.sh" "$W/$n" >"$W/$n/out" 2>&1 && grep -q 't.sh:' "$W/$n/out"
}
clean() { # script text: accepted
  n=$((n + 1)); mkdir -p "$W/$n"; printf '#!/bin/bash\n%s\n' "$1" >"$W/$n/t.sh"
  bash "$ROOT/scripts/lint-no-delete.sh" "$W/$n" >"$W/$n/out" 2>&1
}
check "rm -rf of a sync dir is flagged" flagged 'rm -rf "$SYNC_ROOT/$k"'
check "rm -rf -- of a sync dir is flagged" flagged 'rm -rf -- "${SYNC_ROOT:?}/${k:?}"'
check "rm -fr is flagged" flagged 'rm -fr "$d"'
check "rm -r is flagged" flagged 'rm -r "$d"'
check "rm -R is flagged" flagged 'rm -R "$d"'
check "rm -f -r (split options) is flagged" flagged 'rm -f -r "$d"'
check "rm --recursive is flagged" flagged 'rm --recursive "$d"'
check "rm -rf inside a trap string is flagged" flagged "trap 'rm -rf \"\$stage\"' EXIT"
check "find -delete is flagged" flagged 'find "$d" -name x -delete'
check "shutil.rmtree in an embedded python heredoc is flagged" flagged "python3 - <<'PY'
import shutil
shutil.rmtree('/x')
PY"
check "rm -rf of a variable that is not from mktemp -d is flagged" flagged 'tmp=/var/x
rm -rf "$tmp"'
check "rm -rf of a mktemp -d variable is allowed" clean 'JD="$(mktemp -d)"
rm -rf -- "$JD"'
check "the :? form of a mktemp -d variable is allowed" clean 'T=$(mktemp -d)
rm -rf "${T:?}"'
check "inside a trap string, for a mktemp -d variable, is allowed" clean "tmp=\$(mktemp -d)
trap 'rm -rf \"\$tmp\"' EXIT"
check "a mktemp -d variable reassigned to something else is flagged" flagged 'T=$(mktemp -d)
T=$HOME/x
rm -rf "$T"'
check "a mktemp -d variable can start empty" clean 'T=""
T=$(mktemp -d)
rm -rf "$T"'
check "rm -rf of a mktemp -d variable plus a subpath is flagged (only the whole temp dir)" flagged 'T=$(mktemp -d)
rm -rf "$T/../x"'
check "plain rm -f of a file is fine" clean 'rm -f "$f"; rm -f -- "$a" "$b"'
check "options like --delete-org-token-file and --archive-deletes are not find -delete" clean 'x --delete-org-token-file --archive-deletes'
check "a comment mentioning rm -rf is fine" clean '# never rm -rf the sync dir'
check "the repo's own scripts/ pass" bash "$ROOT/scripts/lint-no-delete.sh"
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
