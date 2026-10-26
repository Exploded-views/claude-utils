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

rm -rf "$W"; mkdir -p "$W/out" "$W/x" "$W/home" "$W/stubs"
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
