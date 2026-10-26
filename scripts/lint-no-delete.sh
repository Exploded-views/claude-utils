#!/usr/bin/env bash
# No-delete lint for the setup scripts: they must never remove a directory tree.
#
#   lint-no-delete.sh [dir]      (default: this tree's scripts/)
#
# Fails (exit 1, "file:line: reason") on, in any *.sh except *_test.sh, outside
# comments: rm -r, rm -rf, rm -fr, rm -R, rm --recursive, find ... -delete, and
# shutil.rmtree. The one exception is a variable assigned from `mktemp -d` in
# the same script (an optional empty initialisation is fine, any other
# assignment to that name is flagged): `rm -rf -- "$VAR"`, "${VAR}", "${VAR:?}"
# on exactly that variable. Plain `rm -f` of files stays allowed. This is the
# durable guard behind "a half-made sync dir is moved aside, never deleted".
# Also flagged, as ways to destroy work in place: git clean, git reset --hard,
# git checkout [--] . and git restore (except on the bootstrap's own "$SRC"
# clone), and a truncating redirect (> "$VAR", : > "$VAR") to a variable named
# ROUTES, IDENTITY*, CFG*, APPS* or *_FILE unless it was assigned from mktemp.
# A line carrying "no-delete-ok" is skipped. Limit: it is a line-based check,
# not a shell parser; a variable built by other means is not recognised.
set -euo pipefail
DIR="${1:-$(cd "$(dirname "$0")" && pwd)}"
[ -d "$DIR" ] || { echo "not a directory: $DIR" >&2; exit 2; }
exec python3 -I - "$DIR" <<'PY'
import os, re, sys

findings = []
assign_mk = re.compile(r"(?:^|[\s;&|{(])(?:local\s+|declare\s+)?([A-Za-z_][A-Za-z0-9_]*)=[\"']?(?:\$\(|`)\s*mktemp\s+-d")
assign_any = re.compile(r"(?:^|[\s;&|{(])(?:local\s+|declare\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$")
rm_cmd = re.compile(r"(?:^|[\s;&|{(/])rm((?:\s+-[^\s]*)+)(?:\s+(.*))?$")
git_bad = re.compile(r"\bgit\s+(?:-C\s+\S+\s+)?(?:clean\b|reset\s+(?:\S+\s+)*--hard\b|checkout\s+(?:--\s+)?\.(?:\s|$)|restore\b)")
trunc = re.compile(r"(?<![>&0-9])>(?!>)\s*\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?(?=[\s;&|)}]|$)")
guarded_var = re.compile(r"^(?:ROUTES|IDENTITY\w*|CFG\w*|APPS\w*|\w*_FILE)$")
assign_tmp = re.compile(r"(?:^|[\s;&|{(])(?:local\s+|declare\s+)?([A-Za-z_][A-Za-z0-9_]*)=[\"']?(?:\$\(|`)\s*mktemp\b")

for name in sorted(os.listdir(sys.argv[1])):
    if not name.endswith(".sh") or name.endswith("_test.sh"):
        continue
    lines = open(os.path.join(sys.argv[1], name), errors="replace").read().splitlines()
    allowed, temps = set(), set()
    for raw in lines:
        if raw.strip().startswith("#"):
            continue
        m = assign_mk.search(raw)
        if m:
            allowed.add(m.group(1))
        m = assign_tmp.search(raw)
        if m:
            temps.add(m.group(1))
    for n, raw in enumerate(lines, 1):
        line = raw.strip()
        if not line or line.startswith("#") or "no-delete-ok" in line:
            continue
        def bad(msg):
            findings.append("%s:%d: %s" % (name, n, msg))
        if "shutil.rmtree" in line or re.search(r"\brmtree\(", line):  # no-delete-ok
            bad("shutil.rmtree removes a directory tree")  # no-delete-ok
        if re.search(r"\bfind\b.*\s-delete(\s|$)", line):
            bad("find -delete")
        norm = re.sub(r"[\"'\\]", "", line)   # escaped and quoted forms
        if git_bad.search(norm) and "$SRC" not in norm:
            bad("git command that discards work in place (allowed only on the bootstrap's own $SRC clone, or with no-delete-ok and a reason): %s" % line)
        for tm in trunc.finditer(norm):
            if guarded_var.match(tm.group(1)) and tm.group(1) not in temps:
                bad("truncating redirect to $%s (use a temp file and a move, or mark it no-delete-ok with a reason): %s" % (tm.group(1), line))
        for m in rm_cmd.finditer(norm):
            opts, rest = m.group(1).split(), (m.group(2) or "")
            recursive = any(o == "--recursive" or (o.startswith("-") and not o.startswith("--") and re.search(r"[rR]", o)) for o in opts)
            if not recursive:
                continue
            target = rest.split()[0] if rest.split() else ""
            if target == "--" and len(rest.split()) > 1:
                target = rest.split()[1]
            tmatch = re.fullmatch(r"\$(?:\{([A-Za-z_][A-Za-z0-9_]*)(?::\?)?\}|([A-Za-z_][A-Za-z0-9_]*));?", target)
            var = (tmatch.group(1) or tmatch.group(2)) if tmatch else None
            if var is None or var not in allowed:
                bad("recursive rm (only a variable assigned from mktemp -d may be removed this way): %s" % line)
        # an allow-listed temp variable must not be pointed somewhere else
        m = assign_any.search(line)
        if m and m.group(1) in allowed:
            rhs = m.group(2).strip()
            if not (re.match(r"[\"']?(?:\$\(|`)\s*mktemp\s+-d", rhs) or re.match(r"(?:\"\"|'')?(?:\s|;|}|$)", rhs)):
                bad("%s is a mktemp -d variable and is reassigned here" % m.group(1))
if findings:
    print("\n".join(findings))
    sys.exit(1)
PY
