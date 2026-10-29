#!/usr/bin/env bash
# Test of scripts/package-release.sh: builds this machine's own archive (linux or
# darwin, amd64 or arm64; TARGET=os/arch overrides) and checks
# its layout, SHA256SUMS, and that the extracted scripts/bootstrap.sh gets past
# argument parsing with --prebuilt-dir (a dry run in a throwaway HOME).
# Usage: scripts/package-release_test.sh [workdir]   (default <tree>/.bin/package-test)
# GO="sh /path/to/dev.sh go" wraps Go the way the dev container does.
# BOOT_BASH=/bin/bash runs the extracted bootstrap.sh with that bash (CI uses
# macOS's own bash 3.2 to prove the script needs nothing newer).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
W="${1:-$ROOT/.bin/package-test}"
VER=v0.3.0-ev.1   # the planned first tag: package-release.sh and bootstrap.sh must both accept it
if [ -z "${TARGET:-}" ]; then
  case "$(uname -s)" in Darwin) tos=darwin ;; *) tos=linux ;; esac
  case "$(uname -m)" in arm64 | aarch64) tarch=arm64 ;; *) tarch=amd64 ;; esac
  TARGET="$tos/$tarch"
fi
NAME="claude-utils-$VER-${TARGET%/*}-${TARGET#*/}"
BOOT_BASH="${BOOT_BASH:-bash}"
if command -v sha256sum >/dev/null 2>&1; then SUMC=(sha256sum -c); else SUMC=(shasum -a 256 -c); fi
GO_BUILD="${GO:-go}"
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }

rm -rf "$W"; mkdir -p "$W/fakegl"
# A fake gitleaks (the real scan is release.yml's and the maintainer's; see the checks below):
# it records its arguments and exits $FAKE_GL_RC (default 0).
cat >"$W/fakegl/gitleaks" <<'FAKE'
#!/bin/sh
echo "$@" >>"${FAKE_GL_LOG:?}"
last=""; for a in "$@"; do last="$a"; done
if [ -d "$last" ]; then
  b="$(basename "$last")"
  (cd "$last" && find . -name '*.strings.txt' | sed "s|^\./||; s|^|$b |") >>"${FAKE_GL_LOG}.strings"
fi
# Like the real one: the scan control tree (named memsync-scan-control) is a finding
# (exit 99 and a one-finding JSON report); other trees are clean (exit 0, "[]"). Overrides:
# FAKE_GL_CLEAN=1 (reports nothing whatever it is given), FAKE_GL_RC (the exit for the real
# archives, 99 = findings), FAKE_GL_CONTROL_RC and FAKE_GL_CONTROL_REPORT (finding|clean|empty|none).
rpath=""; prev=""; for a in "$@"; do [ "$prev" = --report-path ] && rpath="$a"; prev="$a"; done
if [ "$(basename "$last")" = memsync-scan-control ]; then
  rc="${FAKE_GL_CONTROL_RC:-99}"; rep="${FAKE_GL_CONTROL_REPORT:-finding}"
  echo "FAKE-GL-STDERR" >&2
else
  rc="${FAKE_GL_RC:-0}"; rep=clean; [ "$rc" = 99 ] && rep=finding
fi
if [ "${FAKE_GL_CLEAN:-}" = 1 ]; then rc=0; rep=clean; fi
if [ -n "$rpath" ]; then
  case "$rep" in
    finding) echo '[{"RuleID":"fake-rule"}]' >"$rpath" ;;
    clean) echo '[]' >"$rpath" ;;
    empty) : >"$rpath" ;;
  esac
fi
exit "$rc"
FAKE
chmod +x "$W/fakegl/gitleaks"
export FAKE_GL_LOG="$W/gl.log"
ORIGPATH="$PATH"
REALGL="$(command -v gitleaks || true)"   # the real scanner, if this machine has one
PATH="$W/fakegl:$PATH"
mkdir -p "$W/out" "$W/all" "$W/x" "$W/home" "$W/stubs"
GO="${GO:-go}" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/out" "$TARGET" >"$W/package.log" 2>&1 || { tail -20 "$W/package.log"; bad "package-release.sh exits 0"; }
A="$W/out/$NAME.tar.gz"
check "archive exists" [ -f "$A" ]
tar -xzf "$A" -C "$W/x" 2>/dev/null || true
D="$W/x/$NAME"
for f in claude-memsync claude-memmerge LICENSE README.md scripts/bootstrap.sh; do
  check "archive holds $NAME/$f" [ -f "$D/$f" ]
done
check "binaries and bootstrap.sh are executable" bash -c "[ -x '$D/claude-memsync' ] && [ -x '$D/claude-memmerge' ] && [ -x '$D/scripts/bootstrap.sh' ]"
check "bootstrap.sh in the archive is the tree's" cmp -s "$ROOT/scripts/bootstrap.sh" "$D/scripts/bootstrap.sh"
check "no stray top-level entries" bash -c "[ \"\$(tar -tzf '$A' | cut -d/ -f1 | sort -u)\" = '$NAME' ]"
check "SHA256SUMS lists the archive" grep -q "$NAME.tar.gz" "$W/out/SHA256SUMS"
check "SHA256SUMS verifies" bash -c "cd '$W/out' && ${SUMC[*]} SHA256SUMS >/dev/null"
check "the binary reports the release version" bash -c "[ \"\$('$D/claude-memsync' version)\" = 'claude-memsync $VER' ]"

# The budget bench ships as test binaries beside the product archive, so the
# gates can run against the shipped bytes on a host with no Go and no checkout.
BNAME="claude-utils-$VER-budget-${TARGET%/*}-${TARGET#*/}"
BA="$W/out/$BNAME.tar.gz"
check "budget archive exists" [ -f "$BA" ]
mkdir -p "$W/bx"; tar -xzf "$BA" -C "$W/bx" 2>/dev/null || true
BD="$W/bx/$BNAME"
for f in claude-memsync-budget.test route-budget.test README; do
  check "budget archive holds $BNAME/$f" [ -f "$BD/$f" ]
done
check "budget test binaries are executable" bash -c "[ -x '$BD/claude-memsync-budget.test' ] && [ -x '$BD/route-budget.test' ]"
check "budget archive has no stray top-level entries" bash -c "[ \"\$(tar -tzf '$BA' | cut -d/ -f1 | sort -u)\" = '$BNAME' ]"
check "budget README names the run commands" bash -c "grep -q 'MEMSYNC_BUDGET_BIN_DIR' '$BD/README' && grep -q 'MEMSYNC_BUDGET_TAG' '$BD/README' && grep -q 'claude-memsync-budget.test' '$BD/README' && grep -q 'route-budget.test' '$BD/README'"
check "the budget binaries hold the budget gates (-test.list)" bash -c "'$BD/claude-memsync-budget.test' -test.list 'Budget' | grep -q '^TestBudgetB8' && '$BD/route-budget.test' -test.list 'Budget' | grep -q '^TestBudgetB4'"
check "SHA256SUMS lists the product and the budget archive" bash -c "grep -q ' $NAME.tar.gz\$' '$W/out/SHA256SUMS' && grep -q ' $BNAME.tar.gz\$' '$W/out/SHA256SUMS'"

# The packaged budget binaries guard themselves: a wrong tag, one variable alone,
# and (B9) the skip in prebuilt mode, run from outside the checkout.
mkdir -p "$W/guard"
set +e
(cd "$W/guard" && MEMSYNC_BUDGET_BIN_DIR="$D" MEMSYNC_BUDGET_TAG=v9.9.9 "$BD/claude-memsync-budget.test" -test.run '^TestBudgetB' >"$W/g1.log" 2>&1); g1=$?
(cd "$W/guard" && MEMSYNC_BUDGET_BIN_DIR="$D" "$BD/route-budget.test" -test.run '^TestBudgetB' >"$W/g2.log" 2>&1); g2=$?
(cd "$W/guard" && MEMSYNC_BUDGET_TAG="$VER" "$BD/route-budget.test" -test.run '^TestBudgetB' >"$W/g3.log" 2>&1); g3=$?
(cd "$W/guard" && MEMSYNC_BUDGET_BIN_DIR="$D" MEMSYNC_BUDGET_TAG="$VER" "$BD/claude-memsync-budget.test" -test.run '^TestBudgetB9' -test.v >"$W/g4.log" 2>&1); g4=$?
set -e
check "budget binary with a wrong tag exits 1 before any gate (rc=$g1)" bash -c "[ $g1 -eq 1 ] && grep -q 'version mismatch' '$W/g1.log' && ! grep -q -- '--- ' '$W/g1.log'"
check "budget binary with the dir alone exits 1 (rc=$g2)" bash -c "[ $g2 -eq 1 ] && grep -q 'go together' '$W/g2.log'"
check "budget binary with the tag alone exits 1 (rc=$g3)" bash -c "[ $g3 -eq 1 ] && grep -q 'go together' '$W/g3.log'"
check "budget binary, right tag: B9 skips and exits 0 (rc=$g4)" bash -c "[ $g4 -eq 0 ] && grep -q -- '--- SKIP: TestBudgetB9' '$W/g4.log'"
check "budget README and docs select the gates with ^TestBudgetB" bash -c "grep -qF -- \"\$1\" '$BD/README' && grep -qF -- \"\$1\" '$ROOT/docs/releases.md' && ! grep -q -- '-test.run Budget' '$BD/README' '$ROOT/docs/releases.md'" _ "-test.run '^TestBudgetB'"
check "budget README and docs say which gates run which binary" bash -c "grep -q 'B1, B4, B7 and B8' '$BD/README' && grep -q 'B1, B4, B7 and B8' '$ROOT/docs/releases.md' && grep -q 'B2/B3, B5 and B6' '$BD/README' && grep -q 'B2/B3, B5 and B6' '$ROOT/docs/releases.md'"

# Every default target, with a stub Go that only writes its -o file: the release
# set is four budget archives (linux and darwin, amd64 and arm64; none for
# windows), and SHA256SUMS covers them and the six product archives alike.
mkdir -p "$W/stubgo"
cat >"$W/stubgo/go" <<'STUB'
#!/bin/sh
out=""; prev=""
for a in "$@"; do if [ "$prev" = "-o" ]; then out="$a"; fi; prev="$a"; done
[ -z "$out" ] || { echo stub >"$out"; chmod 755 "$out"; }
STUB
chmod +x "$W/stubgo/go"
GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/all" >"$W/all.log" 2>&1 || { tail -5 "$W/all.log"; bad "package-release.sh with every default target exits 0"; }
for bt in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  check "all targets: budget-$bt archive is in the release and in SHA256SUMS" bash -c "[ -f '$W/all/claude-utils-$VER-budget-$bt.tar.gz' ] && grep -q ' claude-utils-$VER-budget-$bt.tar.gz\$' '$W/all/SHA256SUMS'"
done
check "all targets: no windows budget archive" bash -c "! ls '$W/all' | grep -q 'budget-windows'"
check "all targets: SHA256SUMS holds 6 product and 4 budget archives, and verifies" bash -c "[ \"\$(grep -c -- '-budget-' '$W/all/SHA256SUMS')\" = 4 ] && [ \"\$(grep -vc -- '-budget-' '$W/all/SHA256SUMS')\" = 6 ] && cd '$W/all' && ${SUMC[*]} SHA256SUMS >/dev/null"

# The secret scan: gitleaks runs over the extracted contents of every archive
# (product and budget) before SHA256SUMS is written; any finding fails the run
# and leaves nothing to upload. No scanner is an error unless the documented
# local opt-out PACKAGE_RELEASE_SKIP_GITLEAKS=1 is set (release.yml never sets it).
n=0; for a in $(cd "$W/all" && ls | grep '\.\(tar\.gz\|zip\)$'); do n=$((n + 1)); done
: >"$W/gl.log"
GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/scanned" >"$W/scanned.log" 2>&1 || bad "package-release.sh with a clean scan exits 0"
check "the scan ran once per archive ($n) and once on the control" bash -c "[ \"\$(grep -c . '$W/gl.log')\" = $((n + 1)) ] && grep -q 'memsync-scan-control' '$W/gl.log'"
for a in $(cd "$W/all" && ls | grep '\.\(tar\.gz\|zip\)$'); do
  check "the scan covered $a" bash -c "grep -q 'dir.* [^ ]*${a%.tar.gz}' '$W/gl.log' || grep -q 'dir.* [^ ]*${a%.zip}' '$W/gl.log'"
done
check "the scan is gitleaks dir, redacted" bash -c "grep -q '^dir ' '$W/gl.log' && ! grep -qv -- '--redact' '$W/gl.log'"
set +e; FAKE_GL_RC=99 GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/leaky" >"$W/leaky.log" 2>&1; lrc=$?; set -e
check "a finding fails the packaging (rc=$lrc)" [ "$lrc" -ne 0 ]
check "... and leaves no SHA256SUMS and no archive to upload" bash -c "[ ! -e '$W/leaky/SHA256SUMS' ] && [ -z \"\$(ls '$W/leaky' | grep '\.\(tar\.gz\|zip\)$')\" ]"
# The scan checks itself on the release path: a control binary with a fake token must
# be found by the same routine before the real archives are scanned. A scanner that
# reports nothing fails the packaging, and the control never reaches the output.
set +e; FAKE_GL_CLEAN=1 GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/blind" >"$W/blind.log" 2>&1; brc=$?; set -e
check "a scanner that finds nothing in the control fails the packaging (rc=$brc)" bash -c "[ $brc -ne 0 ] && grep -q 'secret scan cannot see inside binaries' '$W/blind.log' && [ ! -e '$W/blind/SHA256SUMS' ]"
check "the control is not in the output" bash -c "! grep -rqs control '$W/scanned/SHA256SUMS' && [ -z \"\$(find '$W/scanned' -name '*control*')\" ]"
# gitleaks exits 1 for a finding and for an error alike unless told otherwise, so the
# script asks for 99 for a finding and a JSON report: only that counts as seen, and
# a scanner error never passes for clean or for detection.
noleft() { # dir: nothing to upload, no stage dirs
  bash -c "[ ! -e '$1/SHA256SUMS' ] && [ -z \"\$(ls -a '$1' | grep -E '\.(tar\.gz|zip)\$|^\.stage')\" ]"
}
check "the scan asks for exit code 99 and a JSON report" bash -c "grep -q -- '--exit-code 99' '$W/gl.log' && grep -q -- '--report-format json' '$W/gl.log' && grep -q -- '--report-path ' '$W/gl.log'"
set +e; FAKE_GL_CONTROL_RC=1 FAKE_GL_CONTROL_REPORT=none GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/e1" >"$W/e1.log" 2>&1; e1=$?; set -e
check "a scanner that errors (exit 1, no report) on the control fails the packaging (rc=$e1)" bash -c "[ $e1 -ne 0 ] && grep -q 'secret scan error on the control (rc=1)' '$W/e1.log'"
check "... and prints the scanner's stderr" grep -q FAKE-GL-STDERR "$W/e1.log"
check "... and leaves nothing behind" noleft "$W/e1"
set +e; FAKE_GL_CONTROL_RC=1 FAKE_GL_CONTROL_REPORT=finding GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/e1b" >"$W/e1b.log" 2>&1; e1b=$?; set -e
check "an error exit (1) on the control fails even when its report holds a finding (rc=$e1b)" bash -c "[ $e1b -ne 0 ] && grep -q 'secret scan error on the control (rc=1)' '$W/e1b.log'"
check "... and leaves nothing behind" noleft "$W/e1b"
set +e; FAKE_GL_CONTROL_RC=99 FAKE_GL_CONTROL_REPORT=empty GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/e2" >"$W/e2.log" 2>&1; e2=$?; FAKE_GL_CONTROL_RC=99 FAKE_GL_CONTROL_REPORT=clean GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/e2b" >"$W/e2b.log" 2>&1; e2b=$?; set -e
check "exit 99 with an empty report on the control fails (rc=$e2)" bash -c "[ $e2 -ne 0 ] && grep -q 'secret scan error on the control (rc=99)' '$W/e2.log'"
check "exit 99 with a report holding no finding on the control fails (rc=$e2b)" bash -c "[ $e2b -ne 0 ] && grep -q 'secret scan error on the control (rc=99)' '$W/e2b.log'"
check "... and leaves nothing behind" noleft "$W/e2"
set +e; FAKE_GL_RC=2 GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/e3" >"$W/e3.log" 2>&1; e3=$?; FAKE_GL_RC=1 GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/e3b" >"$W/e3b.log" 2>&1; e3b=$?; set -e
check "a crash (exit 2) on the real archives after a good control fails (rc=$e3)" bash -c "[ $e3 -ne 0 ] && grep -q 'secret scan error (rc=2)' '$W/e3.log' && ! grep -q 'found something' '$W/e3.log'"
check "an exit 1 on the real archives is an error, not a clean scan (rc=$e3b)" bash -c "[ $e3b -ne 0 ] && grep -q 'secret scan error (rc=1)' '$W/e3b.log'"
check "... and closes its ::group:: (rc=1 and rc=2 and found-something)" bash -c "for l in e3 e3b leaky; do [ \"\$(grep -c '^::group::' '$W/'\$l.log)\" = \"\$(grep -c '^::endgroup::' '$W/'\$l.log)\" ] || exit 1; done; grep -q '^::group::gitleaks' '$W/e3.log'"
check "... and leaves nothing behind" noleft "$W/e3"
SYSPATH="$W/stubgo:/usr/bin:/bin"
set +e; PATH="$SYSPATH" GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/noscan" >"$W/noscan.log" 2>&1; nrc=$?; set -e
check "no gitleaks on PATH fails clearly (rc=$nrc)" bash -c "[ $nrc -ne 0 ] && grep -q 'gitleaks' '$W/noscan.log' && [ ! -e '$W/noscan/SHA256SUMS' ]"
set +e; PATH="$SYSPATH" PACKAGE_RELEASE_SKIP_GITLEAKS=1 GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/optout" >"$W/optout.log" 2>&1; orc=$?; set -e
check "the documented opt-out packages without gitleaks (rc=$orc)" bash -c "[ $orc -eq 0 ] && [ -f '$W/optout/SHA256SUMS' ]"
check "release.yml never sets the opt-out and installs the pinned gitleaks" bash -c "! grep -q PACKAGE_RELEASE_SKIP_GITLEAKS '$ROOT/.github/workflows/release.yml' && grep -q 'GITLEAKS_VERSION: 8.30.1' '$ROOT/.github/workflows/release.yml' && grep -q 'GITLEAKS_SHA256: 551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb' '$ROOT/.github/workflows/release.yml'"

# gitleaks dir skips binary files, so the printable strings of every executable
# (product binaries, claude-focus.exe, both budget .test files, bootstrap.sh) are
# written beside it in the extracted tree and scanned as text.
: >"$W/gl.log.strings"; rm -f "$W/gl.log.strings"
GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/strings1" >"$W/strings1.log" 2>&1 || bad "package-release.sh (strings run) exits 0"
want_strings() { # archive-dir-name file...
  for f in "${@:2}"; do
    check "strings file for $1/$f" grep -qxF "$1 $f.strings.txt" "$W/gl.log.strings"
  done
}
for os_arch in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  want_strings "claude-utils-$VER-$os_arch" claude-memsync claude-memmerge scripts/bootstrap.sh
  want_strings "claude-utils-$VER-budget-$os_arch" claude-memsync-budget.test route-budget.test
done
for os_arch in windows-amd64 windows-arm64; do
  want_strings "claude-utils-$VER-$os_arch" claude-memsync.exe claude-memmerge.exe claude-focus.exe scripts/bootstrap.sh
done

# A secret inside a binary fails the packaging: a tiny program embeds a test-only
# token (assembled here from halves, so this file never holds it whole) and a stub
# Go copies it in as every built binary. Needs the real gitleaks.
if [ -n "$REALGL" ]; then
  mkdir -p "$W/plant/src" "$W/realgl"; ln -sf "$REALGL" "$W/realgl/gitleaks"
  H1=R7kL2mQ9xT4vB8nZ3c; H2=W6yH1jD5fG0sA2eU9p
  printf 'package main\n\nvar t = "%s%s%s"\n\nfunc main() { println(t) }\n' ghp_ "$H1" "$H2" >"$W/plant/src/main.go"
  printf 'package main\n\nvar t = "%s"\n\nfunc main() { println(t) }\n' "just a harmless string for the control" >"$W/plant/src/clean.go.txt"
  mkdir -p "$W/plant/clean"; cp "$W/plant/src/clean.go.txt" "$W/plant/clean/main.go"
  printf 'module planted\n\ngo 1.21\n' >"$W/plant/src/go.mod"; cp "$W/plant/src/go.mod" "$W/plant/clean/go.mod"
  (cd "$W/plant/src" && CGO_ENABLED=0 $GO_BUILD build -o "$W/plant/leaky" .) || bad "build the planted-token program"
  (cd "$W/plant/clean" && CGO_ENABLED=0 $GO_BUILD build -o "$W/plant/clean-bin" .) || bad "build the control program"
  for which in leaky clean-bin; do
    printf '#!/bin/sh\nout=""; prev=""\nfor a in "$@"; do if [ "$prev" = "-o" ]; then out="$a"; fi; prev="$a"; done\n[ -z "$out" ] || { cp "%s" "$out"; chmod 755 "$out"; }\n' "$W/plant/$which" >"$W/plant/go-$which"
    chmod +x "$W/plant/go-$which"
  done
  set +e
  PATH="$W/realgl:$ORIGPATH" GO="$W/plant/go-leaky" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/planted" linux/amd64 >"$W/planted.log" 2>&1; prc=$?
  PATH="$W/realgl:$ORIGPATH" GO="$W/plant/go-clean-bin" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/control" linux/amd64 >"$W/control.log" 2>&1; crc=$?
  set -e
  check "control: a binary with no token packages under the real gitleaks (rc=$crc)" bash -c "[ $crc -eq 0 ] && [ -f '$W/control/SHA256SUMS' ]"
  check "a token inside a binary fails the packaging (rc=$prc)" bash -c "[ $prc -ne 0 ] && grep -q 'gitleaks found something' '$W/planted.log' && [ ! -e '$W/planted/SHA256SUMS' ] && [ -z \"\$(ls '$W/planted' | grep '\.tar\.gz\$')\" ]"
else
  echo "SKIP    token-in-a-binary check: no gitleaks on PATH"
fi

# A budget test binary stamped with another tag, beside product binaries stamped
# with the right one, is refused before any gate (real binaries, not an override).
( cd "$ROOT" && CGO_ENABLED=0 $GO_BUILD test -c -tags budget -trimpath -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=v9.9.9-other" -o "$W/other.test" ./internal/route ) || bad "build a budget test binary with another stamp"
set +e; (cd "$W/guard" && MEMSYNC_BUDGET_BIN_DIR="$D" MEMSYNC_BUDGET_TAG="$VER" "$W/other.test" -test.run '^TestBudgetB' >"$W/g5.log" 2>&1); g5=$?; set -e
check "a test binary stamped v9.9.9-other beside $VER product binaries exits 1 (rc=$g5)" bash -c "[ $g5 -eq 1 ] && grep -q 'this test binary reports' '$W/g5.log' && ! grep -q -- '--- ' '$W/g5.log'"

# One release-version rule everywhere: package-release.sh refuses what bootstrap.sh
# --prebuilt-dir would refuse, and release.yml's tag check is the same rule.
VRE='^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$'
for badv in v1 v1.2 v1.2.3x v1.2.3+meta vfoo v1.2.3-; do
  set +e; "$ROOT/scripts/package-release.sh" "$badv" "$W/badout" bogus/none >"$W/bad.log" 2>&1; brc=$?; set -e
  check "package-release.sh refuses version $badv (rc=$brc)" bash -c "[ $brc -eq 2 ] && grep -q 'version must look like' '$W/bad.log'"
done
check "package-release.sh, bootstrap.sh and release.yml use the same version rule" bash -c "grep -qF -e '$VRE' '$ROOT/scripts/package-release.sh' && grep -qF -e '$VRE' '$ROOT/scripts/bootstrap.sh' && grep -qF -e '$VRE' '$ROOT/.github/workflows/release.yml'"

# Past argument parsing: fake required flags, throwaway HOME, stub gh that is not logged in.
printf '#!/bin/sh\nexit 1\n' >"$W/stubs/gh"; chmod +x "$W/stubs/gh"
printf 'fake-token\n' >"$W/tok"; chmod 600 "$W/tok"
# A REAL-length post-quantum recovery key (about 1,960 characters), made by the
# extracted binary itself: a stub key would not catch a validator that stops at
# a regex repeat count of 255, as macOS's grep does.
mkdir -p "$W/keyhome" "$W/keyrepo.git"
printf '[init]\n\tdefaultBranch = main\n' >"$W/keyhome/.gitconfig"
git init -q --bare -b main "$W/keyrepo.git"
HOME="$W/keyhome" GIT_CONFIG_NOSYSTEM=1 "$D/claude-memsync" init --remote "$W/keyrepo.git" --sync-dir "$W/keyhome/sync" \
  --claude-dir "$W/keyhome/claude" --encrypt --identity "$W/keyhome/id.txt" >/dev/null 2>&1 || bad "the extracted binary makes a post-quantum key"
REC="$(sed -n 's/^# public key: //p' "$W/keyhome/id.txt")"
check "the recovery key is a real-length age1pq1 key (${#REC} characters)" bash -c "case '$REC' in age1pq1*) [ ${#REC} -gt 1900 ] ;; *) false ;; esac"

set +e
OUT="$(HOME="$W/home" PATH="$W/stubs:$PATH" "$BOOT_BASH" "$D/scripts/bootstrap.sh" --prebuilt-dir "$D" --dry-run \
  --host ci --role homelab --org example-org --recovery-recipient "$REC" --org-token-file "$W/tok" </dev/null 2>&1)"
RC=$?
set -e
check "extracted bootstrap.sh --prebuilt-dir --dry-run reaches the banner (RC=$RC)" bash -c "printf '%s\n' \"\$1\" | grep -q '^claude-memsync bootstrap: host=ci role=homelab org=example-org commit=$VER'" _ "$OUT"
check "... past the prerequisite checks (bash ${BASH_VERSION%%(*} runs the test; the script ran under $BOOT_BASH)" bash -c "! printf '%s\n' \"\$1\" | grep -q 'missing prerequisite'" _ "$OUT"
check "... and no usage or argument error" bash -c "! printf '%s\n' \"\$1\" | grep -qE 'unknown argument|is required|usage'" _ "$OUT"
check "... and the throwaway HOME is untouched" bash -c "[ -z \"\$(find '$W/home' -mindepth 1 | head -1)\" ]"

echo; echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
