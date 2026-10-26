#!/usr/bin/env bash
# Lint GitHub Actions workflows against the house rules. Self-contained
# (bash + python3 standard library, no YAML library), so it can be swapped for
# a shared version.
#
#   lint-workflows.sh [dir]      (default: .github/workflows next to this script's tree)
#
# Fails (exit 1, one line per finding) on, in any *.yml / *.yaml:
#   - runs-on that is not a GitHub-hosted label: only ubuntu-*, windows-*,
#     macos-* are allowed, alone or in a list, or ${{ matrix.<key> }} whose
#     matrix values are all such labels. self-hosted, runner groups and
#     everything else are rejected.
#   - a pull_request_target trigger.
#   - a uses: that is not pinned to a full 40-hex commit SHA. ./ local actions
#     are allowed; docker:// must carry an @sha256:<64 hex> digest.
set -euo pipefail
DIR="${1:-$(cd "$(dirname "$0")/.." && pwd)/.github/workflows}"
[ -d "$DIR" ] || { echo "not a directory: $DIR" >&2; exit 2; }   # fs-ok: usage check, positive: false refuses
exec python3 -I - "$DIR" <<'PY'
import os, re, sys

HOSTED = re.compile(r"^(ubuntu|windows|macos)-[A-Za-z0-9._-]+$")
SHA = re.compile(r"^[0-9a-f]{40}$")
DIGEST = re.compile(r"@sha256:[0-9a-f]{64}$")
MATRIX = re.compile(r"^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$")

def strip(line):
    # drop a trailing comment (a # preceded by whitespace, outside quotes)
    q = None
    for i, c in enumerate(line):
        if q:
            if c == q: q = None
        elif c in "'\"": q = c
        elif c == "#" and (i == 0 or line[i - 1] in " \t"): return line[:i].rstrip()
    return line.rstrip()

def unq(v):
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "'\"": return v[1:-1]
    return v

def items(v):
    v = v.strip()
    if v.startswith("[") and v.endswith("]"):
        return [unq(x) for x in v[1:-1].split(",") if x.strip()]
    return [unq(v)]

def indent(l): return len(l) - len(l.lstrip(" "))

def block(lines, i):
    """Lines (stripped) indented deeper than lines[i], with their indent."""
    base, out, j = indent(lines[i]), [], i + 1
    while j < len(lines):
        if lines[j].strip() and indent(lines[j]) <= base and not lines[j].lstrip().startswith("- "):
            break
        if lines[j].strip() and indent(lines[j]) <= base and lines[j].lstrip().startswith("- ") and indent(lines[j]) < base:
            break
        out.append(lines[j]); j += 1
    return out

def matrix_values(lines, key):
    """Values of `key` inside any `matrix:` block: key: v, key: [a, b], a block
    list under key:, and the same keys inside `include:` entries (- key: v)."""
    vals, found = [], False
    for i, l in enumerate(lines):
        if l.strip() != "matrix:": continue
        mat = block(lines, i)
        for j, ml in enumerate(mat):
            m = re.match(r"^\s*(?:-\s+)?" + re.escape(key) + r":\s*(.*)$", ml)
            if not m: continue
            found = True
            if m.group(1).strip():
                vals += items(m.group(1))
            else:
                for bl in block(mat, j):
                    bm = re.match(r"^\s*-\s*(.+)$", bl)
                    if bm: vals.append(unq(bm.group(1)))
    return vals if found else None

findings = []
for name in sorted(os.listdir(sys.argv[1])):
    if not name.endswith((".yml", ".yaml")): continue
    raw = open(os.path.join(sys.argv[1], name)).read().splitlines()
    lines = [strip(l) for l in raw]
    def bad(n, msg): findings.append("%s:%d: %s" % (name, n + 1, msg))
    for n, l in enumerate(lines):
        s = l.strip()
        if not s: continue
        s = re.sub(r"^-\s+", "", s)
        # triggers
        if re.match(r"^pull_request_target\s*:", s) or re.match(r"^on\s*:.*\bpull_request_target\b", s):
            bad(n, "pull_request_target trigger is not allowed")
        elif re.match(r"^\[.*\bpull_request_target\b.*\]$", s) or (re.match(r"^-?\s*pull_request_target$", s)):
            bad(n, "pull_request_target trigger is not allowed")
        # uses
        m = re.match(r"^uses\s*:\s*(.+)$", s)
        if m:
            u = unq(m.group(1))
            if u.startswith("./"): pass
            elif u.startswith("docker://"):
                if not DIGEST.search(u): bad(n, "uses: %s must be pinned by @sha256:<digest>" % u)
            else:
                ref = u.rsplit("@", 1)[1] if "@" in u else ""
                if not SHA.match(ref): bad(n, "uses: %s must be pinned to a full 40-hex commit SHA" % u)
        # runs-on
        m = re.match(r"^runs-on\s*:\s*(.*)$", s)
        if m:
            v = m.group(1).strip()
            labels, ok = [], True
            if v:
                labels = items(v)
            else:
                for b in block(lines, n):
                    bs = b.strip()
                    if not bs: continue
                    if bs.startswith("- "): labels.append(unq(bs[2:]))
                    elif re.match(r"^(group|labels)\s*:", bs):
                        bad(n, "runs-on: runner groups and labels maps are not allowed (%s)" % bs); ok = False
                    else:
                        labels.append(unq(bs))
            if not ok: continue
            if not labels: bad(n, "runs-on: empty"); continue
            for lab in labels:
                mm = MATRIX.match(lab)
                if mm:
                    vals = matrix_values(lines, mm.group(1))
                    if not vals: bad(n, "runs-on: %s has no matrix values to check" % lab)
                    else:
                        for x in vals:
                            if not HOSTED.match(x): bad(n, "runs-on: matrix value '%s' is not a GitHub-hosted label" % x)
                elif not HOSTED.match(lab):
                    bad(n, "runs-on: '%s' is not a GitHub-hosted label (ubuntu-*, windows-*, macos-*)" % lab)
if findings:
    print("\n".join(findings))
    sys.exit(1)
PY
