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
check "\\rm -rf (escaped to dodge aliases) is flagged" flagged '\rm -rf "$d"'
check "a quoted command name is flagged" flagged '"rm" -rf "$d"'
check "a quoted option is flagged" flagged 'rm "-rf" "$d"'
check "rm -r at the end of a line is flagged" flagged 'rm -r'
check "xargs rm -r is flagged" flagged 'printf x | xargs rm -rf'
check "/bin/rm -rf is flagged" flagged '/bin/rm -rf "$d"'
check "git clean is flagged" flagged 'git -C "$d" clean -fdx'
check "git reset --hard is flagged" flagged 'git reset --hard origin/main'
check "git checkout -- . is flagged" flagged 'git checkout -- .'
check "git checkout . is flagged" flagged 'git -C "$d" checkout .'
check "git restore is flagged" flagged 'git restore .'
check "git reset --hard in the bootstrap's own clone is allowed with a reason marker" clean 'git -C "$SRC" reset --hard "$SHA"   # no-delete-ok: the clone this script made'
check "git checkout of a commit is fine" clean 'git -C "$SRC" checkout --quiet "$SHA"'
check "truncating ROUTES with > is flagged" flagged ': > "$ROUTES"'
check "truncating a *_FILE variable is flagged" flagged '>"$TOKEN_FILE"'
check "truncating CFG is flagged" flagged 'echo x > "${CFG}"'
check "truncating IDENTITY is flagged" flagged 'printf "" >"$IDENTITY"'
check "appending with >> is fine" clean 'echo x >> "$ROUTES"'
check "writing a temp next to it is fine" clean 'printf x >"$ROUTES.tmp"'
check "truncating a mktemp file is fine" clean 'T=$(mktemp)
: >"$T_FILE_UNRELATED_NAME"
echo x >"$T"'
check "truncating an allow-listed mktemp variable named *_FILE is fine" clean 'TMP_FILE=$(mktemp)
: >"$TMP_FILE"'
check "every target must be a mktemp -d variable: a second, unlisted target is flagged" flagged 'T=$(mktemp -d)
rm -rf -- "$T" "$SYNC_ROOT"'
check "two mktemp -d variables are fine" clean 'T=$(mktemp -d); U=$(mktemp -d)
rm -rf -- "$T" "$U"'
check "the git -C \"\$SRC\" exemption does not cover \$SRCX" flagged 'git -C "$SRCX" clean -fdx'
check "a trailing comment mentioning \$SRC does not exempt a line" flagged 'git clean -fdx # $SRC'
check "git -C \"\${SRC}\" is the same exemption" clean 'git -C "${SRC}" reset --hard "$SHA"'
check "git -C \"\$SRC\" checkout . is allowed" clean 'git -C "$SRC" checkout .'
check "git -c k=v clean is flagged" flagged 'git -c core.x=1 clean -fd'
check "git --git-dir=... clean is flagged" flagged 'git --git-dir=/x/.git clean -fd'
check "git --work-tree clean is flagged" flagged 'git --work-tree=/x clean -fd'
check "git checkout -f is flagged" flagged 'git checkout -f main'
check "git checkout -- path is flagged" flagged 'git checkout -- notes/a.md'
check "git stash clear is flagged" flagged 'git stash clear'
check "git stash drop is flagged" flagged 'git stash drop'
check "git worktree remove --force is flagged" flagged 'git worktree remove --force /x'
check "&> truncation is flagged" flagged 'echo x &>"$ROUTES"'
check ">| truncation is flagged" flagged 'echo x >|"$ROUTES"'
check "1> truncation is flagged" flagged 'echo x 1>"$ROUTES"'
check "tee without -a is flagged" flagged 'echo x | tee "$ROUTES"'
check "tee -a is fine" clean 'echo x | tee -a "$ROUTES"'
check "cp /dev/null onto a config variable is flagged" flagged 'cp /dev/null "$CFG"'
check "a path below a config variable (\${CFG}/a) is not a truncation of CFG" clean 'echo x > "${CFG}/a"'
check "2>&1 and >/dev/null are fine" clean 'cmd >/dev/null 2>&1; cmd 2>"$ERRS"'
check "plain rm -f of a file is fine" clean 'rm -f "$f"; rm -f -- "$a" "$b"'
check "options like --delete-org-token-file and --archive-deletes are not find -delete" clean 'x --delete-org-token-file --archive-deletes'
check "a comment mentioning rm -rf is fine" clean '# never rm -rf the sync dir'
check "the repo's own scripts/ pass" bash "$ROOT/scripts/lint-no-delete.sh"
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
