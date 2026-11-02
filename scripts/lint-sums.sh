#!/usr/bin/env bash
# Checksum-verify lint for the docs and scripts. A release's SHA256SUMS lists
# every asset, so a plain `sha256sum -c SHA256SUMS` fails for each file that was
# not downloaded. See docs/releases.md#verify-a-download.
#
#   lint-sums.sh [root]      (default: this repo)
#
# Fails (exit 1, "file:line: reason") when, in any *.md or scripts/*.sh, a line
# runs `sha256sum -c` (or --check) without --ignore-missing, or `shasum ... -c`
# other than the single-line `grep ... | shasum -a 256 -c -` form. Comment lines
# in *.sh are skipped; a line carrying "sums-ok" is skipped; a file with
# "sums-lint: skip-file" in its first lines is skipped.
set -euo pipefail
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
[ -d "$ROOT" ] || { echo "not a directory: $ROOT" >&2; exit 2; }   # fs-ok: usage check, positive: false refuses
exec python3 -I - "$ROOT" <<'PY'
import os, re, sys

root = sys.argv[1]
SKIP_DIRS = {".git", "node_modules", "vendor"}
SUM = re.compile(r"\bsha256sum\s+(?:-\S+\s+)*?(?:-[A-Za-z]*c[A-Za-z]*|--check)(?![A-Za-z-])")
SHASUM = re.compile(r"\bshasum\s+(?:(?:-\S+|\d+)\s+)*(?:-[A-Za-z]*c[A-Za-z]*|--check)(?![A-Za-z-])")
GREP_FORM = re.compile(r"\bgrep\b[^|]*\|\s*shasum\b")

paths = []
for d, dirs, files in os.walk(root):
    dirs[:] = sorted(x for x in dirs if x not in SKIP_DIRS)
    rel = os.path.relpath(d, root)
    for f in sorted(files):
        if f.endswith(".md") or (rel == "scripts" and f.endswith(".sh")):
            paths.append(os.path.normpath(os.path.join(rel, f)))

findings = []
for p in paths:
    lines = open(os.path.join(root, p), errors="replace").read().splitlines()
    if any("sums-lint: skip-file" in l for l in lines[:5]): continue
    for n, raw in enumerate(lines, 1):
        if "sums-ok" in raw: continue
        if p.endswith(".sh") and raw.strip().startswith("#"): continue
        if SUM.search(raw) and "--ignore-missing" not in raw:
            findings.append("%s:%d: sha256sum -c needs --ignore-missing (SHA256SUMS lists files you did not download)" % (p, n))
        if SHASUM.search(raw) and not GREP_FORM.search(raw):
            findings.append("%s:%d: shasum -c must read one line: grep \" $NAME.tar.gz\\$\" SHA256SUMS | shasum -a 256 -c -" % (p, n))
if findings:
    print("\n".join(findings))
    sys.exit(1)
PY
