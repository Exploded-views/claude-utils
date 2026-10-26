#!/usr/bin/env bash
# Test of scripts/lint-guards.sh: fixtures for each finding, and the repo's own
# scripts/ must pass. Stock macOS tools only.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"
cleanup() { rm -rf -- "${W:?}"; }
trap cleanup EXIT
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
n=0
DEFFIX='fixture_foo() { :; }'
# fixture <script text> [registry text] [fixtures text]: a dir with t.sh, guards.registry, guard_fault_test.sh
fixture() {
  n=$((n + 1)); mkdir -p "$W/$n"
  printf '#!/bin/bash\n%s\n' "$1" >"$W/$n/t.sh"
  printf '%s\n' "${2-foo -}" >"$W/$n/guards.registry"
  printf '%s\n' "${3-$DEFFIX}" >"$W/$n/guard_fault_test.sh"
}
flagged() { fixture "$@"; ! bash "$ROOT/scripts/lint-guards.sh" "$W/$n" >"$W/$n/out" 2>&1 && grep -q 't.sh:\|guards.registry' "$W/$n/out"; }
clean()   { fixture "$@"; bash "$ROOT/scripts/lint-guards.sh" "$W/$n" >"$W/$n/out" 2>&1; }
# a minimal, correct script: one guard, called once, no forbidden constructs
BASE='# >>> guards
foo_check() {
  local o
  o="$(echo hi)" || { echo REFUSE:x; return 1; }
  [ "$o" = hi ] || { echo REFUSE:x; return 1; }
  echo SAFE:foo
}
# <<< guards
guard foo'
check "a correct minimal script passes" clean "$BASE"
check "a guard call whose name is not in the registry is flagged" flagged "$BASE
guard bar"
check "guard_ok calls are checked too" flagged "$BASE
guard_ok bar || true"
check "a registry name without a <name>_check function is flagged" flagged "# >>> guards
# <<< guards
guard foo"
check "a registry name without a fixture in guard_fault_test.sh is flagged" flagged "$BASE" "foo -" "fixture_other() { :; }"
check "a registry name that no guard call uses is flagged" flagged "# >>> guards
foo_check() { echo SAFE:foo; }
# <<< guards"
check "bare \$? in a guard function is flagged" flagged "# >>> guards
foo_check() {
  cmd1
  [ \$? -eq 0 ] || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "|| true in a guard function is flagged" flagged "# >>> guards
foo_check() {
  cmd1 || true
  echo SAFE:foo
}
# <<< guards
guard foo"
check "|| : in a guard function is flagged" flagged "# >>> guards
foo_check() {
  cmd1 || :
  echo SAFE:foo
}
# <<< guards
guard foo"
check "if <cmd>; then in a guard function is flagged" flagged "# >>> guards
foo_check() {
  if git rev-parse --verify refs/stash; then return 1; fi
  echo SAFE:foo
}
# <<< guards
guard foo"
check "elif <cmd> in a guard function is flagged" flagged "# >>> guards
foo_check() {
  if [ -z x ]; then :; elif grep -q x f; then return 1; fi
  echo SAFE:foo
}
# <<< guards
guard foo"
check "<cmd> && ... in a guard function is flagged" flagged "# >>> guards
foo_check() {
  git rev-parse --git-dir && echo ok
  echo SAFE:foo
}
# <<< guards
guard foo"
check "if [ ... ]; then and [ ... ] && [ ... ] are fine" clean "# >>> guards
foo_check() {
  if [ -z x ] && [ -z y ]; then return 1; fi
  [ -n a ] && [ -n b ] || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "a *_check function outside the guards block is a guard path too" flagged "foo_check() {
  cmd1 || true
  echo SAFE:foo
}
guard foo"
check "the same constructs inside the guard-helper block are fine" clean "# >>> guard-helper
guard_ok() { wait \$p || rc=\$?; cmd || true; if cmd; then :; fi; }
# <<< guard-helper
$BASE"
check "the same constructs inside the guard-py block are fine" clean "# >>> guard-py
G_PY='if x: pass
    if y: pass'
# <<< guard-py
$BASE"
check "code in the guards block but outside a function is a guard path" flagged "# >>> guards
cmd1 || true
foo_check() { echo SAFE:foo; }
# <<< guards
guard foo"
check "a guard bound to an arbitrary command (guard <name> -- <cmd>) is flagged" flagged "$BASE
guard foo -- echo SAFE:foo"
check "guard_run outside the helper is flagged" flagged "$BASE
guard_run foo echo SAFE:foo"
check "|| return 0 in a guard function is flagged" flagged "# >>> guards
foo_check() {
  cmd1 || return 0
  echo SAFE:foo
}
# <<< guards
guard foo"
check "a || branch that says SAFE is flagged" flagged "# >>> guards
foo_check() {
  cmd1 || { echo SAFE:foo; return 0; }
  echo SAFE:foo
}
# <<< guards
guard foo"
check "x=\$(cmd) || x=\"\" (a failure turned into an empty answer) is flagged" flagged "# >>> guards
foo_check() {
  local o
  o=\"\$(cmd1)\" || o=\"\"
  echo SAFE:foo
}
# <<< guards
guard foo"
check "... unless the line says why with guard-ok" clean "# >>> guards
foo_check() {
  local o
  o=\"\$(cmd1)\" || o=\"\"   # guard-ok: the empty answer takes the stricter path
  echo SAFE:foo
}
# <<< guards
guard foo"
check "while <cmd> in a guard function is flagged" flagged "# >>> guards
foo_check() {
  while read -r l; do :; done < f
  echo SAFE:foo
}
# <<< guards
guard foo"
check "case \"\$(cmd)\" in in a guard function is flagged" flagged "# >>> guards
foo_check() {
  case \"\$(cmd1)\" in
    \"\") echo SAFE:foo ;;
  esac
}
# <<< guards
guard foo"
check "a backslash continuation in a guard function is flagged" flagged "# >>> guards
foo_check() {
  cmd1 \\
    || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "[ -e x ] in a guard function is flagged (use path_state)" flagged "# >>> guards
foo_check() {
  [ -e x ] || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "[ ! -L x ] in a guard function is flagged" flagged "# >>> guards
foo_check() {
  [ ! -L x ] || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "test -e x is flagged" flagged "# >>> guards
foo_check() {
  test -e x || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "[ ! -d x ] is flagged" flagged "# >>> guards
foo_check() {
  [ ! -d x ] || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "a positive [ -d x ] / [ -f x ] is flagged too: every file test goes through path_state or says why (fs-ok)" flagged "# >>> guards
foo_check() {
  [ -d x ] || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "a one-line foo_check() { ...; } does not make the rest of the file a guard path" clean "# >>> guards
foo_check() { echo SAFE:foo; }
# <<< guards
guard foo
other() {
  cmd1 || true
}"
# ---- the fs-ok / guard-ok markers are real trailing comments, with a reason ----
G1='# >>> guards
foo_check() {
  echo SAFE:foo
}
# <<< guards
guard foo'
check "[ -e x ] with a trailing fs-ok: <reason> is clean" clean "$G1
[ -e x ] && echo hi # fs-ok: nothing depends on it"
check "[ -e x ] without a marker is flagged anywhere in a script" flagged "$G1
[ -e x ] && echo hi"
check "fs-ok without a reason is flagged" flagged "$G1
[ -e x ] && echo hi # fs-ok:"
check "fs-ok inside a string is not a marker" flagged "$G1
[ -e x ] && echo \"# fs-ok: because\""
check "fs-ok in the middle of a comment text without the colon form is flagged" flagged "$G1
[ -e x ] && echo hi # this is not fs-ok really"
check "[[ -s \$1 ]] is a file test" flagged "$G1
[[ -s \$1 ]] && echo hi"
check "[ -a x ] is a file test" flagged "$G1
[ -a x ] && echo hi"
check "test -d x is a file test" flagged "$G1
test -d x && echo hi"
check "[ ! -f x ] is a file test" flagged "$G1
[ ! -f x ] && echo hi"
check "! [ -d x ] is a file test" flagged "$G1
! [ -d x ] && echo hi"
check "-r -w -x -O -G -N -L -h are file tests" flagged "$G1
[ -w x ] && [ -x y ] && [ -O z ] && [ -G a ] && [ -N b ] && [ -h c ] && echo hi"
check "a file test inside a python heredoc is not shell" clean "$G1
python3 - <<'PY'
if [ -e x ]:
    pass
PY"
check "path_state is the way" clean "$G1
st=\"\$(path_state x)\" || exit 1"
check "-e as an option of another command is not a file test" clean "$G1
grep -e foo file; sed -e s/a/b/ file; ls -d x"
check "guard-ok exempts the x=\$(cmd) || x= rule only" flagged "# >>> guards
foo_check() {
  local o
  o=\"\$(cmd1)\" || o=\"\"   # guard-ok: stricter path
  guard_run foo echo SAFE:foo   # guard-ok: please
  echo SAFE:foo
}
# <<< guards
guard foo"
check "guard-ok does not exempt guard foo -- <cmd>" flagged "$G1
guard foo -- echo SAFE:foo # guard-ok: no"
check "guard-ok in a string does not exempt the x=\$(cmd) || x= rule" flagged "# >>> guards
foo_check() {
  local o
  o=\"\$(cmd1)\" || o=\"# guard-ok: nope\"
  echo SAFE:foo
}
# <<< guards
guard foo"
check "guard-ok without a reason does not exempt" flagged "# >>> guards
foo_check() {
  local o
  o=\"\$(cmd1)\" || o=\"\"   # guard-ok
  echo SAFE:foo
}
# <<< guards
guard foo"
# ---- more ways round it ----
check "[ -f x ] || echo SAFE:foo is flagged" flagged "# >>> guards
foo_check() {
  [ -f x ] || echo SAFE:foo
}
# <<< guards
guard foo"
check "grep -q x f || echo SAFE:foo is flagged" flagged "# >>> guards
foo_check() {
  grep -q x f || echo SAFE:foo
}
# <<< guards
guard foo"
check "! grep ... || ... is flagged" flagged "# >>> guards
foo_check() {
  ! grep -q x f || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "until <cmd> is flagged" flagged "# >>> guards
foo_check() {
  until cmd1; do :; done
  echo SAFE:foo
}
# <<< guards
guard foo"
check "o=\$(ls ...; true) is flagged" flagged "# >>> guards
foo_check() {
  local o
  o=\"\$(ls -A d 2>/dev/null; true)\"
  echo SAFE:foo
}
# <<< guards
guard foo"
check "local o=\$(cmd) (declare and assign hides the status) is flagged" flagged "# >>> guards
foo_check() {
  local o=\"\$(cmd1)\"
  echo SAFE:foo
}
# <<< guards
guard foo"
check "find / ls in a guard path is a filesystem probe and is flagged" flagged "# >>> guards
foo_check() {
  local o
  o=\"\$(find d -type f)\" || return 1
  [ -z \"\$o\" ] || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "a guard path that calls a function defined outside the guards block is flagged" flagged "helper() { :; }
# >>> guards
foo_check() {
  helper x || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "eval is flagged" flagged "$G1
eval \"guard\"\"_run foo echo SAFE:foo\""
check "function foo_check { is a guard function too" flagged "# >>> guards
function foo_check {
  cmd1 || true
  echo SAFE:foo
}
# <<< guards
guard foo"
check "foo_check () { with a space is a guard function too" flagged "# >>> guards
foo_check () {
  cmd1 || true
  echo SAFE:foo
}
# <<< guards
guard foo"
check "the repo's own scripts/ pass" bash "$ROOT/scripts/lint-guards.sh"
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
