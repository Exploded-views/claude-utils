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
# ---- heredoc detection: only a real <<WORD operator outside quotes, $(( )) and <<< ----
SQ="'"
BAD='[ -e x ] && echo hi'
# flagged_msg <message regex> <script...>: flagged, and the output says it
flagged_msg() { local re="$1"; shift; flagged "$@" && grep -q -- "$re" "$W/$n/out"; }
# flagged_after: the forbidden test that follows the trigger is flagged as a file test, and no heredoc was assumed
flagged_after() { flagged "$@" && grep -q 'a file test reads a stat error' "$W/$n/out" && ! grep -q 'never closed' "$W/$n/out"; }
check "a << inside a double-quoted string does not start a heredoc (a forbidden test after it is still flagged)" flagged_after "$G1
echo \"<<EOF\"
$BAD"
check "a << inside a single-quoted string does not start a heredoc" flagged_after "$G1
echo '<<EOF'
$BAD"
check "\$((1<<X)) is arithmetic, not a heredoc" flagged_after "$G1
y=\$((1<<X))
$BAD"
check "(( n = 1<<3 )) is arithmetic, not a heredoc" flagged_after "$G1
(( n = 1<<3 ))
$BAD"
check "a <<<word here-string is not a heredoc" flagged_after "$G1
cat <<<word
$BAD"
check "a <<<\"\$x\" here-string is not a heredoc" flagged_after "$G1
read -r a <<<\"\$x\"
$BAD"
check "a # comment mentioning <<EOF does not start a heredoc" flagged_after "$G1
echo hi # cat <<EOF
$BAD"
check "a real heredoc body is skipped and the line after the terminator is checked" flagged_after "$G1
cat <<EOF
$BAD
EOF
$BAD"
check "a real heredoc body is skipped (clean)" clean "$G1
cat <<EOF
$BAD
EOF"
check "<<- ends at a tab-indented terminator" flagged_after "$G1
cat <<-EOF
	body
	EOF
$BAD"
check "<<- body is skipped (clean)" clean "$G1
cat <<-EOF
	$BAD
	EOF"
check "<<'EOF' (quoted delimiter): body skipped, line after flagged" flagged_after "$G1
cat <<${SQ}EOF${SQ}
$BAD
EOF
$BAD"
check "<<'EOF' body is skipped (clean)" clean "$G1
cat <<${SQ}EOF${SQ}
$BAD
EOF"
check "<<\"EOF\" body is skipped and the line after flagged" flagged_after "$G1
cat <<\"EOF\"
$BAD
EOF
$BAD"
check "<<\\EOF body is skipped (clean)" clean "$G1
cat <<\\EOF
$BAD
EOF"
check "two heredocs on one line are both skipped (clean)" clean "$G1
cat <<A <<B
$BAD
A
$BAD
B"
check "a heredoc with a command after the operator: body skipped" clean "$G1
cat <<EOF | sed s/a/b/
$BAD
EOF"
check "a heredoc that never closes is an error, not a silent skip" flagged "$G1
cat <<EOF
$BAD"
check "... and the error says so" flagged_msg 'never closed' "$G1
cat <<EOF
body"
check "an unclosed <<- heredoc (terminator not tab-indented only) still closes" clean "$G1
cat <<-EOF
	body
EOF"
check "a plain << heredoc whose terminator is indented never closes" flagged_msg 'never closed' "$G1
cat <<EOF
body
  EOF"
# ---- more spellings of a file test ----
check "a case arm a) [ -e x ] ;; is a file test" flagged "$G1
case x in
  a) [ -e x ] ;;
esac"
check "a case arm with alternatives a|b) test -f x is a file test" flagged "$G1
case x in
  a|b) test -f x ;;
esac"
check "command test -e x is a file test" flagged "$G1
command test -e x && echo hi"
check "builtin test -e x is a file test" flagged "$G1
builtin test -e x && echo hi"
check "env test -e x is a file test" flagged "$G1
env test -e x && echo hi"
check "/bin/test -e x is a file test" flagged "$G1
/bin/test -e x && echo hi"
check "/usr/bin/test -e x is a file test" flagged "$G1
/usr/bin/test -e x && echo hi"
check "/usr/bin/[ -e x ] is a file test" flagged "$G1
/usr/bin/[ -e x ] && echo hi"
check "time [ -e x ] is a file test" flagged "$G1
time [ -e x ] && echo hi"
check "a backtick [ -e x ] is a file test" flagged "$G1
y=\`[ -e x ] && echo hi\`"
check "a quoted operator [ \"-e\" x ] is a file test" flagged "$G1
[ \"-e\" x ] && echo hi"
check "a single-quoted operator [ ${SQ}-f${SQ} x ] is a file test" flagged "$G1
[ ${SQ}-f${SQ} x ] && echo hi"
check "a variable operator [ \$op x ] is flagged" flagged "$G1
[ \$op x ] && echo hi"
check "a quoted variable operator [ \"\$op\" x ] is flagged" flagged "$G1
[ \"\$op\" x ] && echo hi"
check "a variable operator in test is flagged" flagged "$G1
test \$op x && echo hi"
check "a variable operator in the binary position [ a \$op b ] is flagged" flagged "$G1
[ a \$op b ] && echo hi"
check "a variable operator can say why with fs-ok" clean "$G1
[ \$op x ] && echo hi # fs-ok: the operator is fixed by the caller"
check "string and number comparisons with variables are not flagged" clean "$G1
a=1; b=2
[ \"\$a\" = \"\$b\" ] && [ -z \"\$a\" ] && [ \"\$a\" ] && [ \$# -gt 0 ] && [ \"\$a\" != x ] && [[ \$a == b ]] && echo hi"
for op in S p b c g u k; do
  check "-$op is a file test (stat based)" flagged "$G1
[ -$op x ] && echo hi"
done
for op in nt ot ef; do
  check "a -$op b is a file test (stat based)" flagged "$G1
[ a -$op b ] && echo hi"
done
# ---- the fs-ok reason must say something ----
check "fs-ok with a one-letter reason is flagged" flagged "$G1
[ -e x ] && echo hi # fs-ok: x"
check "fs-ok with a short one-word reason is flagged" flagged "$G1
[ -e x ] && echo hi # fs-ok: because"
check "fs-ok with two short words is flagged" flagged "$G1
[ -e x ] && echo hi # fs-ok: no harm"
check "fs-ok with three words is clean" clean "$G1
[ -e x ] && echo hi # fs-ok: no harm here"
check "fs-ok with a 12-character word is clean" clean "$G1
[ -e x ] && echo hi # fs-ok: twelve-chars"
# ---- @helper ----
HELPREG='foo -
@helper org_gh'
check "an @helper without a definition in guard_fault_test.sh is flagged" flagged "$BASE" "$HELPREG" "$DEFFIX"
check "... the finding names the helper" flagged_msg 'org_gh' "$BASE" "$HELPREG" "$DEFFIX"
check "an @helper defined in guard_fault_test.sh is clean" clean "$BASE" "$HELPREG" "$DEFFIX
org_gh() { gh \"\$@\"; }"
check "an @helper that is only mentioned (not defined) in guard_fault_test.sh is flagged" flagged "$BASE" "$HELPREG" "$DEFFIX
# org_gh() is not here"
# ---- a call through a variable from a guard path ----
for call in '$f x' '"$f" x' '${f} x' 'x=1 $f x' '"${f}" x'; do
  check "a guard path calling a function through a variable ($call) is flagged" flagged "# >>> guards
foo_check() {
  $call || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
done
check "a guard path calling \$(\$f x) is flagged" flagged "# >>> guards
foo_check() {
  local o
  o=\"\$(\$f x)\" || return 1
  echo SAFE:foo
}
# <<< guards
guard foo"
check "a variable as an argument in a guard path is fine" clean "# >>> guards
foo_check() {
  local o
  o=\"\$(echo \"\$f\")\" || return 1
  echo \"\$f\" \$f >/dev/null
  echo SAFE:foo
}
# <<< guards
guard foo"
check "a call through a variable outside a guard path is fine" clean "$BASE
f=echo
\$f hi"
# ---- existence decisions read from command output, outside the guard paths ----
check "[ -z \"\$(cat f 2>/dev/null)\" ] is flagged" flagged "$G1
[ -z \"\$(cat f 2>/dev/null)\" ] && echo hi"
check "[ -n \"\$(cat f 2>/dev/null)\" ] is flagged" flagged "$G1
if [ -n \"\$(cat f 2>/dev/null)\" ]; then echo hi; fi"
check "o=\$(cat f 2>/dev/null) then [ -z \"\$o\" ] is flagged" flagged "$G1
f() {
  o=\"\$(cat f 2>/dev/null)\"
  [ -z \"\$o\" ] && echo hi
}"
check "read ... < f || is flagged" flagged "$G1
read -r l < f || echo none"
check "[ -z \"\$(ls d)\" ] is flagged" flagged "$G1
[ -z \"\$(ls d)\" ] && echo hi"
check "[ -n \"\$(stat -c %s f)\" ] is flagged" flagged "$G1
[ -n \"\$(stat -c %s f)\" ] && echo hi"
check "[ -z \"\$(find d -name x)\" ] is flagged" flagged "$G1
[ -z \"\$(find d -name x)\" ] && echo hi"
check "o=\$(ls d) then [ -z \"\$o\" ] is flagged" flagged "$G1
f() {
  o=\"\$(ls d)\"
  [ -z \"\$o\" ] && echo hi
}"
check "... a marker with a reason on the test allows it" clean "$G1
[ -z \"\$(cat f 2>/dev/null)\" ] && echo hi # fs-ok: only picks a message to print"
check "... and so does one on read < f ||" clean "$G1
read -r l < f || echo none # fs-ok: only picks a message to print"
check "[ -z \"\$o\" ] on output of something else is fine" clean "$G1
f() {
  o=\"\$(echo hi)\"
  [ -z \"\$o\" ] && echo hi
}"
check "the same variable name in another function is not tracked" clean "$G1
f() {
  o=\"\$(ls d)\"
  echo \"\$o\"
}
g() {
  o=\"\$(echo hi)\"
  [ -z \"\$o\" ] && echo hi
}"
check "the repo's own scripts/ pass" bash "$ROOT/scripts/lint-guards.sh"
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
