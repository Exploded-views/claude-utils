#!/usr/bin/env bash
# sums-lint: skip-file   (this test's fixtures are the bad patterns on purpose)
# Test of scripts/lint-sums.sh: a plain checksum check is flagged, the
# --ignore-missing and grep|shasum forms are not, and the repo itself passes.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
# fixture <file> <line>: a tree holding one file with that line; prints its dir.
fixture() {
  local d; d=$(mktemp -d "$W/f.XXXXXX"); mkdir -p "$d/scripts" "$d/docs"
  printf '%s\n' "$2" >"$d/$1"
  echo "$d"
}
flagged() { # file line: the lint rejects it and names file:1
  local d; d=$(fixture "$1" "$2")
  ! bash "$ROOT/scripts/lint-sums.sh" "$d" >"$d/out" 2>&1 && grep -q "$1:1:" "$d/out"
}
clean() { # file line: the lint accepts it
  local d; d=$(fixture "$1" "$2")
  bash "$ROOT/scripts/lint-sums.sh" "$d" >"$d/out" 2>&1
}
check "plain sha256sum -c SHA256SUMS in a .md is flagged" flagged docs/a.md 'sha256sum -c SHA256SUMS'
check "plain sha256sum -c in a script is flagged" flagged scripts/a.sh 'sha256sum -c SHA256SUMS'
check "sha256sum --check is flagged" flagged docs/a.md 'sha256sum --check SHA256SUMS'
check "a prose mention of sha256sum -c is flagged" flagged README.md 'the format `sha256sum -c` reads'
check "plain shasum -a 256 -c is flagged" flagged docs/a.md 'shasum -a 256 -c SHA256SUMS'
check "an indented fenced plain command is flagged" flagged docs/a.md '   sha256sum -c SHA256SUMS'
check "sha256sum -c --ignore-missing is accepted" clean docs/a.md 'sha256sum -c --ignore-missing SHA256SUMS'
check "--ignore-missing before the file is accepted" clean docs/a.md 'sha256sum --ignore-missing -c SHA256SUMS'
check "the grep|shasum single-line form is accepted" clean docs/a.md 'grep " $NAME.tar.gz\$" SHA256SUMS | shasum -a 256 -c -'
check "the README one-liner with both forms is accepted" clean README.md 'sha256sum -c --ignore-missing SHA256SUMS     # macOS: grep " $NAME.tar.gz\$" SHA256SUMS | shasum -a 256 -c -'
check "a line marked sums-ok is accepted" clean scripts/a.sh 'sha256sum -c SHA256SUMS   # sums-ok: every archive is present'
check "a shell comment line is not scanned" clean scripts/a.sh '# sha256sum -c SHA256SUMS is what NOT to do'
check "sha256sum without -c is accepted" clean scripts/a.sh 'sha256sum -- a.tar.gz > SHA256SUMS'
check "a file marked skip-file is not scanned" clean scripts/a.sh '# sums-lint: skip-file
sha256sum -c SHA256SUMS'
check "this repo passes" bash "$ROOT/scripts/lint-sums.sh"

echo; echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
