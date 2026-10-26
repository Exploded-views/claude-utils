#!/usr/bin/env bash
# Test of scripts/package-release.sh: builds the linux/amd64 archive and checks
# its layout, SHA256SUMS, and that the extracted scripts/bootstrap.sh gets past
# argument parsing with --prebuilt-dir (a dry run in a throwaway HOME).
# Usage: scripts/package-release_test.sh [workdir]   (default <tree>/.bin/package-test)
# GO="sh /path/to/dev.sh go" wraps Go the way the dev container does.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
W="${1:-$ROOT/.bin/package-test}"
VER=v0.0.0-ci
NAME="claude-utils-$VER-linux-amd64"
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }

rm -rf "$W"; mkdir -p "$W/out" "$W/x" "$W/home" "$W/stubs"
GO="${GO:-go}" bash "$ROOT/scripts/package-release.sh" "$VER" "$W/out" linux/amd64 >"$W/package.log" 2>&1 || { tail -20 "$W/package.log"; bad "package-release.sh exits 0"; }
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
check "SHA256SUMS verifies" bash -c "cd '$W/out' && sha256sum -c SHA256SUMS >/dev/null"
check "the binary reports the release version" bash -c "[ \"\$('$D/claude-memsync' version)\" = 'claude-memsync $VER' ]"

# Past argument parsing: fake required flags, throwaway HOME, stub gh that is not logged in.
printf '#!/bin/sh\nexit 1\n' >"$W/stubs/gh"; chmod +x "$W/stubs/gh"
printf 'fake-token\n' >"$W/tok"; chmod 600 "$W/tok"
REC="age1pq1$(printf 'q%.0s' $(seq 1 1100))"
set +e
OUT="$(HOME="$W/home" PATH="$W/stubs:$PATH" bash "$D/scripts/bootstrap.sh" --prebuilt-dir "$D" --dry-run \
  --host ci --role homelab --org example-org --recovery-recipient "$REC" --org-token-file "$W/tok" </dev/null 2>&1)"
RC=$?
set -e
check "extracted bootstrap.sh --prebuilt-dir --dry-run reaches the banner (RC=$RC)" bash -c "printf '%s\n' \"\$1\" | grep -q '^claude-memsync bootstrap: host=ci role=homelab org=example-org commit=$VER'" _ "$OUT"
check "... and no usage or argument error" bash -c "! printf '%s\n' \"\$1\" | grep -qE 'unknown argument|is required|usage'" _ "$OUT"
check "... and the throwaway HOME is untouched" bash -c "[ -z \"\$(find '$W/home' -mindepth 1 | head -1)\" ]"

echo; echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
