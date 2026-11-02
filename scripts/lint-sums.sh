#!/usr/bin/env bash
# Checksum-verify lint for the docs and scripts. A release's SHA256SUMS lists
# every asset, so a plain `sha256sum -c SHA256SUMS` fails for each file that was
# not downloaded. See docs/releases.md#verify-a-download.
#
#   lint-sums.sh [root]      (default: this repo)
#
# Fails (exit 1, "file:line: reason") when, in any *.md or scripts/*.sh, a
# command (sha256sum, gsha256sum or shasum, flags in any position, lines joined
# across a trailing backslash) checks a list (-c or --check) without the
# --ignore-missing token on that same command, unless it is the single-line
# form `grep ... | shasum -a 256 -c -` (or | sha256sum -c -), which reads stdin.
# A command ends at |, ;, &, a backtick, ) or a # comment. Comment lines in
# *.sh are skipped; a line carrying "sums-ok" is skipped; a file with
# "sums-lint: skip-file" in its first lines is skipped. The last line says how
# many lines and files were opted out, so a marker never hides silently.
set -euo pipefail
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
[ -d "$ROOT" ] || { echo "not a directory: $ROOT" >&2; exit 2; }   # fs-ok: usage check, positive: false refuses
exec python3 -I - "$ROOT" <<'PY'
import os, re, sys

root = sys.argv[1]
SKIP_DIRS = {".git", "node_modules", "vendor"}
CMD = re.compile(r"(?<![\w-])(g?sha256sum|shasum)(?!\w)")
END = re.compile(r"[|;&`)]|\s#")
CHECK = re.compile(r"-[A-Za-z]*c[A-Za-z]*|--check")
FROM_GREP = re.compile(r"\bgrep\b[^|]*\|\s*$")

paths = []
for d, dirs, files in os.walk(root):
    dirs[:] = sorted(x for x in dirs if x not in SKIP_DIRS)
    rel = os.path.relpath(d, root)
    for f in sorted(files):
        if f.endswith(".md") or (rel == "scripts" and f.endswith(".sh")):
            paths.append(os.path.normpath(os.path.join(rel, f)))

def logical(lines):
    """(first line number, text) with backslash-continued lines joined."""
    out, cur, start = [], "", 0
    for n, raw in enumerate(lines, 1):
        if not cur: start = n
        if raw.endswith("\\"):
            cur += raw[:-1] + " "
            continue
        out.append((start, cur + raw)); cur = ""
    if cur: out.append((start, cur))
    return out

def problem(text):
    """The reason this logical line is a bad checksum check, or None."""
    for m in CMD.finditer(text):
        seg = text[m.end():]
        e = END.search(seg)
        seg = seg[:e.start()] if e else seg
        toks = seg.split()
        if not any(CHECK.fullmatch(t) for t in toks): continue
        if m.group(1).endswith("sha256sum") and "--ignore-missing" in toks:
            continue
        if toks and toks[-1] == "-" and FROM_GREP.search(text[:m.start()]):
            continue
        if m.group(1) == "shasum":
            return "a shasum check must read one grep-selected line from stdin (see docs/releases.md#verify-a-download)"
        return "a %s check needs --ignore-missing (SHA256SUMS lists files you did not download)" % m.group(1)
    return None

findings, opt_lines, opt_files = [], 0, 0
for p in paths:
    lines = open(os.path.join(root, p), errors="replace").read().splitlines()
    if any("sums-lint: skip-file" in l for l in lines[:5]):
        opt_files += 1
        continue
    for n, text in logical(lines):
        if p.endswith(".sh") and text.strip().startswith("#"): continue
        why = problem(text)
        if why is None: continue
        if "sums-ok" in text: opt_lines += 1
        else: findings.append("%s:%d: %s" % (p, n, why))
if findings: print("\n".join(findings))
print("lint-sums: %s (%d lines and %d files opted out)" % ("FAILED" if findings else "OK", opt_lines, opt_files))
sys.exit(1 if findings else 0)
PY
