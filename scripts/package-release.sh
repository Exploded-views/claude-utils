#!/usr/bin/env bash
# Build the release archives and SHA256SUMS. Fork-only (see .github/workflows/release.yml).
#
#   package-release.sh <version> <outdir> [os/arch ...]
#
# With no targets: linux, darwin and windows, amd64 and arm64. Each archive
# holds claude-utils-<version>-<os>-<arch>/ with the binaries, LICENSE,
# README.md and scripts/bootstrap.sh (so a downloaded, verified archive can
# bootstrap a host with --prebuilt-dir, without a clone). For each linux and
# darwin target it also builds claude-utils-<version>-budget-<os>-<arch>.tar.gz:
# the budget gates as `go test -c -tags budget` binaries (claude-memsync-budget.test,
# route-budget.test), to run against the product archive's binaries on a host with
# no Go and no checkout. SHA256SUMS covers every archive of both kinds.
# Before SHA256SUMS is written, gitleaks (from PATH; release.yml installs the
# pinned one) scans the extracted contents of every archive; a finding fails the
# run and removes the archives, so nothing is left to upload. No gitleaks is an
# error unless PACKAGE_RELEASE_SKIP_GITLEAKS=1 (local development only; release.yml
# never sets it).
# GO="sh /path/to/dev.sh go" wraps Go. Run from anywhere.
set -euo pipefail

[ $# -ge 2 ] || { sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
VERSION="$1"; OUT="$2"; shift 2
# The same rule as bootstrap.sh --prebuilt-dir and release.yml (v0.3.0, v0.3.0-ev.1).
printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' \
  || { echo "version must look like vX.Y.Z or vX.Y.Z-suffix (e.g. v0.3.0-ev.1)" >&2; exit 2; }
if printf '%s' "$VERSION" | grep -q '[^A-Za-z0-9._+-]'; then echo "version has unexpected characters" >&2; exit 2; fi
if [ $# -gt 0 ]; then TARGETS=("$@"); else TARGETS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64); fi

if [ "${PACKAGE_RELEASE_SKIP_GITLEAKS:-}" != 1 ] && ! command -v strings >/dev/null 2>&1; then
  echo "strings (binutils) not found on PATH: the secret scan reads the strings of every binary" >&2; exit 2
fi
if [ "${PACKAGE_RELEASE_SKIP_GITLEAKS:-}" != 1 ] && ! command -v gitleaks >/dev/null 2>&1; then
  echo "gitleaks not found on PATH: install it (release.yml pins v8.30.1) or, for local development only, set PACKAGE_RELEASE_SKIP_GITLEAKS=1" >&2; exit 2
fi
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$OUT"; OUT="$(cd "$OUT" && pwd)"
GO="${GO:-go}"
LDFLAGS="-s -w -X github.com/MarimerLLC/claude-utils/internal/version.Override=${VERSION}"
export CGO_ENABLED=0
cd "$ROOT"

ARCHIVES=()
STAGE_ROOT=""
stage_cleanup() { [ -z "$STAGE_ROOT" ] || rm -rf -- "$STAGE_ROOT"; STAGE_ROOT=""; }
# On any failure (a build, the secret scan) nothing is left to upload.
finish() {
  rc=$?
  stage_cleanup
  if [ "$rc" -ne 0 ]; then
    for a in ${ARCHIVES[@]+"${ARCHIVES[@]}"}; do rm -f -- "$OUT/$a"; done
    rm -f -- "$OUT/SHA256SUMS"
  fi
}
trap finish EXIT
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

  # The budget bench for this target (none for windows: the gates are for linux and darwin hosts).
  if [ "$GOOS" != windows ]; then
    bname="claude-utils-${VERSION}-budget-${GOOS}-${GOARCH}"
    STAGE_ROOT="$(mktemp -d "$OUT/.stage.XXXXXX")"
    bstage="$STAGE_ROOT/$bname"
    mkdir -p "$bstage"
    echo "::group::budget tests ${target}"
    GOOS="$GOOS" GOARCH="$GOARCH" $GO test -c -tags budget -trimpath -ldflags "$LDFLAGS" -o "$bstage/claude-memsync-budget.test" ./cmd/claude-memsync
    GOOS="$GOOS" GOARCH="$GOARCH" $GO test -c -tags budget -trimpath -ldflags "$LDFLAGS" -o "$bstage/route-budget.test" ./internal/route
    echo "::endgroup::"
    cat >"$bstage/README" <<README
Budget gates B1-B8 (B9 needs a source build and skips) against the shipped bytes, with no Go and no
checkout. Unpack the product archive claude-utils-${VERSION}-${GOOS}-${GOARCH}.tar.gz (after checking
SHA256SUMS) and this one, then, from any directory:
  MEMSYNC_BUDGET_BIN_DIR=<product dir> MEMSYNC_BUDGET_TAG=${VERSION} ./claude-memsync-budget.test -test.run '^TestBudgetB' -test.v -test.timeout 900s
  MEMSYNC_BUDGET_BIN_DIR=<product dir> MEMSYNC_BUDGET_TAG=${VERSION} ./route-budget.test -test.run '^TestBudgetB' -test.v -test.timeout 900s
Which bytes each gate measures: B1, B4, B7 and B8 run the code inside these test binaries; B2/B3, B5 and B6
run the product archive's claude-memsync. Both must carry the tag, and both are checked: a mismatch fails.
README
    (cd "$STAGE_ROOT" && tar -czf "$OUT/${bname}.tar.gz" "$bname"); ARCHIVES+=("${bname}.tar.gz")
    stage_cleanup
  fi
done

# Secret scan: gitleaks over the extracted contents of every archive, product
# and budget, as a downloader would unpack them. `gitleaks dir` skips binary
# files, so the printable strings of every executable (and *.exe, *.test) are
# written beside it as <file>.strings.txt in the extracted tree and scanned as
# text. Any finding fails the run (the EXIT trap then removes the archives).
# gitleaks exits 1 for a finding and for an error alike, so it is asked for 99 on a
# finding and for a JSON report: scan_tree returns gitleaks's own exit code, and only
# 0 (clean) and 99 (findings) mean the scan worked.
scan_tree() { # dir report
  local d="$1" f
  while IFS= read -r f; do
    strings -a -n 8 -- "$f" >"$f.strings.txt" || return 2
  done < <(find "$d" -type f \( -perm -u+x -o -name '*.exe' -o -name '*.test' \) ! -name '*.strings.txt')
  gitleaks dir --no-banner --redact --exit-code 99 --report-format json --report-path "$2" -c "$ROOT/.gitleaks.toml" "$d"
}
if [ "${PACKAGE_RELEASE_SKIP_GITLEAKS:-}" != 1 ]; then
  STAGE_ROOT="$(mktemp -d "$OUT/.stage.XXXXXX")"
  # The scan checks itself first, on this path: a control executable holding a fake
  # token (assembled from parts, so no source line holds it whole) must be found by
  # the very routine that scans the archives. It is never packaged.
  ctl="$STAGE_ROOT/memsync-scan-control"; mkdir -p "$ctl"
  tok="ghp_""R7kL2mQ9xT4vB8nZ3c""W6yH1jD5fG0sA2eU9p"
  printf '\177ELF\000\000\000%s\000\000\000' "$tok" >"$ctl/control.bin"; chmod 755 "$ctl/control.bin"
  rc=0; scan_tree "$ctl" "$STAGE_ROOT/control.json" >/dev/null 2>&1 || rc=$?
  # Seen only if the scanner reports findings (99) AND the report holds one.
  if [ "$rc" -ne 99 ] || ! grep -q '"RuleID"' "$STAGE_ROOT/control.json" 2>/dev/null; then
    echo "secret scan error on the control (rc=$rc): secret scan cannot see inside binaries" >&2; exit 1
  fi
  for a in "${ARCHIVES[@]}"; do
    case "$a" in
      *.zip) unzip -q "$OUT/$a" -d "$STAGE_ROOT" ;;
      *) tar -xzf "$OUT/$a" -C "$STAGE_ROOT" ;;
    esac
    d="$STAGE_ROOT/${a%.tar.gz}"; d="${d%.zip}"
    echo "::group::gitleaks ${a}"
    rc=0; scan_tree "$d" "$STAGE_ROOT/${a}.json" || rc=$?
    case "$rc" in
      0) ;;
      99) echo "gitleaks found something in $a" >&2; exit 1 ;;
      *) echo "secret scan error (rc=$rc) in $a" >&2; exit 1 ;;
    esac
    echo "::endgroup::"
  done
  stage_cleanup
fi

# sha256sum on Linux, shasum on macOS: the same "<hash>  <file>" lines either way.
if command -v sha256sum >/dev/null 2>&1; then SUM=(sha256sum --); else SUM=(shasum -a 256 --); fi
(cd "$OUT" && "${SUM[@]}" "${ARCHIVES[@]}" > SHA256SUMS)
ls -lh "$OUT"
