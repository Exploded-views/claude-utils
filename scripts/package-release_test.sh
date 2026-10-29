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
exit "${FAKE_GL_RC:-0}"
FAKE
chmod +x "$W/fakegl/gitleaks"
export FAKE_GL_LOG="$W/gl.log"
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
check "budget README and docs select the gates with ^TestBudgetB" bash -c "grep -q \"-test.run '^TestBudgetB'\" '$BD/README' && grep -q \"-test.run '^TestBudgetB'\" '$ROOT/docs/releases.md' && ! grep -q -- '-test.run Budget' '$BD/README' '$ROOT/docs/releases.md'"
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
check "the scan ran once per archive ($n)" bash -c "[ \"\$(grep -c . '$W/gl.log')\" = $n ]"
for a in $(cd "$W/all" && ls | grep '\.\(tar\.gz\|zip\)$'); do
  check "the scan covered $a" bash -c "grep -q 'dir.* [^ ]*${a%.tar.gz}' '$W/gl.log' || grep -q 'dir.* [^ ]*${a%.zip}' '$W/gl.log'"
done
check "the scan is gitleaks dir, redacted" bash -c "grep -q '^dir ' '$W/gl.log' && ! grep -qv -- '--redact' '$W/gl.log'"
set +e; FAKE_GL_RC=1 GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/leaky" >"$W/leaky.log" 2>&1; lrc=$?; set -e
check "a finding fails the packaging (rc=$lrc)" [ "$lrc" -ne 0 ]
check "... and leaves no SHA256SUMS and no archive to upload" bash -c "[ ! -e '$W/leaky/SHA256SUMS' ] && [ -z \"\$(ls '$W/leaky' | grep '\.\(tar\.gz\|zip\)$')\" ]"
SYSPATH="$W/stubgo:/usr/bin:/bin"
set +e; PATH="$SYSPATH" GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/noscan" >"$W/noscan.log" 2>&1; nrc=$?; set -e
check "no gitleaks on PATH fails clearly (rc=$nrc)" bash -c "[ $nrc -ne 0 ] && grep -q 'gitleaks' '$W/noscan.log' && [ ! -e '$W/noscan/SHA256SUMS' ]"
set +e; PATH="$SYSPATH" PACKAGE_RELEASE_SKIP_GITLEAKS=1 GO="$W/stubgo/go" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/optout" >"$W/optout.log" 2>&1; orc=$?; set -e
check "the documented opt-out packages without gitleaks (rc=$orc)" bash -c "[ $orc -eq 0 ] && [ -f '$W/optout/SHA256SUMS' ]"
check "release.yml never sets the opt-out and installs the pinned gitleaks" bash -c "! grep -q PACKAGE_RELEASE_SKIP_GITLEAKS '$ROOT/.github/workflows/release.yml' && grep -q 'GITLEAKS_VERSION: 8.30.1' '$ROOT/.github/workflows/release.yml' && grep -q 'GITLEAKS_SHA256: 551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb' '$ROOT/.github/workflows/release.yml'"

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
