#!/usr/bin/env bash
# Build the release archives and SHA256SUMS. Fork-only (see .github/workflows/release.yml).
#
#   package-release.sh <version> <outdir> [os/arch ...]
#
# With no targets: linux, darwin and windows, amd64 and arm64. Each archive
# holds claude-utils-<version>-<os>-<arch>/ with the binaries, LICENSE,
# README.md and scripts/bootstrap.sh (so a downloaded, verified archive can
# bootstrap a host with --prebuilt-dir, without a clone). SHA256SUMS covers
# every archive. GO="sh /path/to/dev.sh go" wraps Go. Run from anywhere.
set -euo pipefail

[ $# -ge 2 ] || { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
VERSION="$1"; OUT="$2"; shift 2
# The same rule as bootstrap.sh --prebuilt-dir and release.yml (v0.3.0, v0.3.0-ev.1).
printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' \
  || { echo "version must look like vX.Y.Z or vX.Y.Z-suffix (e.g. v0.3.0-ev.1)" >&2; exit 2; }
if printf '%s' "$VERSION" | grep -q '[^A-Za-z0-9._+-]'; then echo "version has unexpected characters" >&2; exit 2; fi
if [ $# -gt 0 ]; then TARGETS=("$@"); else TARGETS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64); fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$OUT"; OUT="$(cd "$OUT" && pwd)"
GO="${GO:-go}"
LDFLAGS="-s -w -X github.com/MarimerLLC/claude-utils/internal/version.Override=${VERSION}"
export CGO_ENABLED=0
cd "$ROOT"

ARCHIVES=()
STAGE_ROOT=""
stage_cleanup() { [ -z "$STAGE_ROOT" ] || rm -rf -- "$STAGE_ROOT"; STAGE_ROOT=""; }
trap stage_cleanup EXIT
for target in "${TARGETS[@]}"; do
  GOOS="${target%/*}"; GOARCH="${target#*/}"
  name="claude-utils-${VERSION}-${GOOS}-${GOARCH}"
  STAGE_ROOT="$(mktemp -d "$OUT/.stage.XXXXXX")"   # staged in a fresh temp dir under OUT (a wrapped Go can reach it), the only tree this script removes
  stage="$STAGE_ROOT/$name"
  mkdir -p "$stage/scripts"
  ext=""; [ "$GOOS" != windows ] || ext=".exe"

  echo "::group::build ${target}"
  GOOS="$GOOS" GOARCH="$GOARCH" $GO build -trimpath -ldflags "$LDFLAGS" -o "$stage/claude-memsync${ext}" ./cmd/claude-memsync
  GOOS="$GOOS" GOARCH="$GOARCH" $GO build -trimpath -ldflags "$LDFLAGS" -o "$stage/claude-memmerge${ext}" ./cmd/claude-memmerge
  if [ "$GOOS" = windows ]; then
    # GUI subsystem so launching it from a hotkey never flashes a console.
    GOOS="$GOOS" GOARCH="$GOARCH" $GO build -trimpath -ldflags "${LDFLAGS} -H windowsgui" -o "$stage/claude-focus${ext}" ./cmd/claude-focus
  fi
  echo "::endgroup::"

  cp LICENSE README.md "$stage/"
  cp scripts/bootstrap.sh "$stage/scripts/bootstrap.sh"; chmod 755 "$stage/scripts/bootstrap.sh"

  if [ "$GOOS" = windows ]; then
    rm -f "$OUT/${name}.zip"; (cd "$STAGE_ROOT" && zip -qr "$OUT/${name}.zip" "$name"); ARCHIVES+=("${name}.zip")
  else
    (cd "$STAGE_ROOT" && tar -czf "$OUT/${name}.tar.gz" "$name"); ARCHIVES+=("${name}.tar.gz")
  fi
  stage_cleanup
done

# sha256sum on Linux, shasum on macOS: the same "<hash>  <file>" lines either way.
if command -v sha256sum >/dev/null 2>&1; then SUM=(sha256sum --); else SUM=(shasum -a 256 --); fi
(cd "$OUT" && "${SUM[@]}" "${ARCHIVES[@]}" > SHA256SUMS)
ls -lh "$OUT"
