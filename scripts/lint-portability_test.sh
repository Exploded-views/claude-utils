#!/usr/bin/env bash
# portability-lint: skip-file   (this test's fixtures are the bad patterns on purpose)
# Test of scripts/lint-portability.sh: each fixture line must be flagged (or, for
# the "ok" ones, not), and the repo's own scripts/ must pass. Stock macOS tools only.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
n=0
flagged() { # line: the one-line script is rejected
  n=$((n + 1)); mkdir -p "$W/$n"; printf '#!/bin/sh\n%s\n' "$1" >"$W/$n/t.sh"
  ! bash "$ROOT/scripts/lint-portability.sh" "$W/$n" >"$W/$n/out" 2>&1 && grep -q 't.sh:2:' "$W/$n/out"
}
clean() { # line: the one-line script is accepted
  n=$((n + 1)); mkdir -p "$W/$n"; printf '#!/bin/sh\n%s\n' "$1" >"$W/$n/t.sh"
  bash "$ROOT/scripts/lint-portability.sh" "$W/$n" >"$W/$n/out" 2>&1
}
K='abcdefghijklmnopqrstuvwxyz'
check "grep -E with a repeat bound of 1000 (BSD grep stops at 255)" flagged "printf x | grep -Eq '^a[$K]{1000,}\$'"
check "a bound of 256 is flagged" flagged "grep -E 'a{256}' f"
check "an upper bound of 300 is flagged" flagged "grep -E 'a{1,300}' f"
check "sed with a bound over 255 is flagged" flagged "sed -E 's/a{400}/b/' f"
check "a BRE bound over 255 is flagged" flagged "grep 'a\\{999\\}' f"
check "awk with a bound over 255 is flagged" flagged "awk '/a{500}/' f"
check "[[ =~ ]] with a bound over 255 is flagged (same system regex library)" flagged "[[ \$x =~ ^a{1000}\$ ]]"
check "bounds up to 255 are fine" clean "grep -E '^a[$K]{1,255}\$' f"
check "small bounds are fine" clean "printf x | grep -Eq '^[0-9]{1,20}\$'"
check "sed -i with no suffix argument is flagged" flagged "sed -i 's/a/b/' f"
check "sed -E -i is flagged" flagged "sed -E -i 's/a/b/' f"
check "sed -i '' (BSD) is accepted" clean "sed -i '' 's/a/b/' f"
check "sed -i.bak is accepted" clean "sed -i.bak 's/a/b/' f"
check "grep -P is flagged" flagged "grep -P 'a' f"
check "grep -oP is flagged" flagged "grep -oP 'a' f"
check "\\s in grep -E is flagged" flagged "grep -E 'a\\s+b' f"
check "\\w in sed is flagged" flagged "sed -E 's/\\w+/x/' f"
check "\\b in grep -E is flagged" flagged "grep -E '\\bword\\b' f"
check "readlink -f is flagged" flagged "readlink -f x"
check "xargs -r is flagged" flagged "ls | xargs -r rm"
check "find -printf is flagged" flagged "find . -printf '%p\\n'"
check "find -regex is flagged" flagged "find . -regex '.*'"
check "stat -c with no stat -f fallback is flagged" flagged "stat -c %a f"
check "stat -c with a stat -f fallback on the line is accepted" clean "m=\$(stat -c %a f 2>/dev/null || stat -f %Lp f)"
check "date -d is flagged" flagged "date -d yesterday"
check "base64 -w is flagged" flagged "base64 -w0 f"
check "mktemp -p is flagged" flagged "mktemp -p /x"
check "a comment line is not flagged" clean "# sed -i 's/a/b/' f is what NOT to do"
check "a line marked portability-ok is not flagged" clean "sed -i 's/a/b/' f   # portability-ok: GNU only on this CI step"
check "a python heredoc body is not scanned" bash -c "mkdir -p '$W/py' && printf '#!/bin/sh\npython3 - <<'\"'\"'PY'\"'\"'\nimport re\nre.compile(r\"a\\\\s{1000}\")\nPY\n' >'$W/py/t.sh' && bash '$ROOT/scripts/lint-portability.sh' '$W/py'"
check "a file marked skip-file is not scanned" bash -c "mkdir -p '$W/sk' && printf '#!/bin/sh\n# portability-lint: skip-file\nsed -i x f\n' >'$W/sk/t.sh' && bash '$ROOT/scripts/lint-portability.sh' '$W/sk'"
check "this repo's scripts/ pass" bash "$ROOT/scripts/lint-portability.sh"

echo; echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
