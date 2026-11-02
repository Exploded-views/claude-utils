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
check "combined flags -wc are flagged" flagged docs/a.md 'sha256sum -wc SHA256SUMS'
check "a leading long flag before -c is flagged" flagged docs/a.md 'sha256sum --strict -c SHA256SUMS'
check "--status --check is flagged" flagged docs/a.md 'sha256sum --status --check SHA256SUMS'
check "shasum combined flags -bc are flagged" flagged docs/a.md 'shasum -a 256 -bc SHA256SUMS'
check "shasum --algorithm 256 --check is flagged" flagged docs/a.md 'shasum --algorithm 256 --check SHA256SUMS'
check "a backslash-continued sha256sum is flagged" flagged docs/a.md 'sha256sum \
  -c SHA256SUMS'
check "a backslash-continued shasum is flagged" flagged docs/a.md 'shasum -a 256 \
  -c SHA256SUMS'
check "the flag after the file is flagged" flagged docs/a.md 'sha256sum SHA256SUMS -c'
check "gsha256sum -c is flagged" flagged docs/a.md 'gsha256sum -c SHA256SUMS'
check "--ignore-missing in a trailing comment does not count" flagged scripts/a.sh 'sha256sum -c SHA256SUMS   # add --ignore-missing'
check "--ignore-missing as a prefix of another word does not count" flagged docs/a.md 'sha256sum -c --ignore-missing-not SHA256SUMS'
check "grep then shasum reading a file is flagged" flagged docs/a.md 'grep x f | shasum -a 256 -c SHA256SUMS'
check "grep then sha256sum reading a file is flagged" flagged docs/a.md 'grep x f | sha256sum -c SHA256SUMS'
check "a command not fed by grep, reading stdin, is flagged" flagged docs/a.md 'echo x | sha256sum -c -'
check "grep | sha256sum -c - (stdin) is accepted" clean docs/a.md 'grep " $NAME.tar.gz\$" SHA256SUMS | sha256sum -c -'
check "a near miss (-c-style prose) is accepted" clean docs/a.md 'the sha256sum -c-style output'
check "sha256sum piped to wc -c is accepted" clean docs/a.md 'sha256sum f | wc -c'
check "a continued command with --ignore-missing is accepted" clean docs/a.md 'sha256sum \
  -c --ignore-missing SHA256SUMS'
count_ok() { # the summary counts honoured opt-outs
  local d; d=$(fixture scripts/a.sh 'sha256sum -c S   # sums-ok: one')
  printf '%s\n' 'sha256sum -c T   # sums-ok: two' 'sha256sum -c --ignore-missing T   # sums-ok: not needed' >"$d/docs/b.md"
  printf '%s\n' '# sums-lint: skip-file' 'sha256sum -c S' >"$d/scripts/c.sh"
  bash "$ROOT/scripts/lint-sums.sh" "$d" >"$d/out" 2>&1 && grep -qx 'lint-sums: OK (2 lines and 1 files opted out)' "$d/out"
}
check "the summary counts opted-out lines and files" count_ok
zero_ok() {
  local d; d=$(fixture docs/a.md 'sha256sum -c --ignore-missing S')
  bash "$ROOT/scripts/lint-sums.sh" "$d" >"$d/out" 2>&1 && grep -qx 'lint-sums: OK (0 lines and 0 files opted out)' "$d/out"
}
check "the summary says 0 when nothing is opted out" zero_ok
check "this repo passes" bash "$ROOT/scripts/lint-sums.sh"

echo; echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
