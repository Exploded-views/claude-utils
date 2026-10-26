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

import shlex
CONTROL = {";", "&&", "||", "|", "&", ")", "(", "}", "{", "|&", ";;", "\n"}
def scan_rm(text, depth=0):
    """(options, targets) of every rm command in the text, quoted forms and quoted command strings included"""
    try:
        lex = shlex.shlex(text, posix=True, punctuation_chars=True)
        lex.whitespace_split = True
        toks = list(lex)
    except ValueError:
        toks = re.sub(r"[\"'\\]", "", text).split()
    found = []
    for i, t in enumerate(toks):
        if depth < 3 and (" " in t or "\t" in t) and re.search(r"\brm\b", t):
            found += scan_rm(t, depth + 1)
        if t == "rm" or t.endswith("/rm"):
            opts, j = [], i + 1
            while j < len(toks) and toks[j].startswith("-") and toks[j] != "--":
                opts.append(toks[j]); j += 1
            if j < len(toks) and toks[j] == "--":
                j += 1
            targets = []
            while j < len(toks) and toks[j] not in CONTROL:
                targets.append(toks[j].rstrip(";")); j += 1
            found.append((opts, targets))
    return found
assign_mk = re.compile(r"(?:^|[\s;&|{(])(?:local\s+|declare\s+)?([A-Za-z_][A-Za-z0-9_]*)=[\"']?(?:\$\(|`)\s*mktemp\s+-d")
assign_any = re.compile(r"(?:^|[\s;&|{(])(?:local\s+|declare\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$")
rm_cmd = re.compile(r"(?:^|[\s;&|{(/])rm((?:\s+-[^\s]*)+)(?:\s+(.*))?$")
GLOBAL = r"(?:\s+(?:-C\s+\S+|-c\s+\S+|--[A-Za-z-]+(?:=\S+)?|--(?:git-dir|work-tree|namespace)\s+\S+))*"
git_bad = re.compile(r"\bgit" + GLOBAL + r"\s+(?:clean\b|reset\s+(?:\S+\s+)*--hard\b|checkout\s+(?:\S+\s+)*?(?:-f\b|--force\b|--\s+\S|\.(?:\s|$))|restore\b|stash\s+(?:clear|drop)\b|worktree\s+remove\s+(?:\S+\s+)*--force\b)")
git_src = re.compile(r"\bgit\s+-C\s+\$(?:\{SRC\}|SRC)(?![A-Za-z0-9_])")
VAR = r"\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))(?=[\s;&|)]|$)"
trunc = re.compile(r"(?<![>&0-9])(?:&>|>\||1>|>)(?!>)\s*" + VAR)
tee_cmd = re.compile(r"\btee((?:\s+-\S+)*)\s+(.*)$")
cp_null = re.compile(r"\bcp\s+(?:-\S+\s+)*/dev/null\s+" + VAR)
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
        for m in assign_mk.finditer(raw):
            allowed.add(m.group(1))
        for m in assign_tmp.finditer(raw):
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
        gm = git_bad.search(norm)
        if gm:
            sm = git_src.search(norm)
            if not (sm and sm.start() == gm.start()):   # exempt only "git -C $SRC ..." (the clone this script made)
                bad("git command that discards work in place (allowed only as git -C \"$SRC\" on the bootstrap's own clone, or with no-delete-ok and a reason): %s" % line)
        def trunc_var(m):
            return m.group(1) or m.group(2)
        def check_trunc(var):
            if guarded_var.match(var) and var not in temps:
                bad("truncating write to $%s (use a temp file and a move, or mark it no-delete-ok with a reason): %s" % (var, line))
        for tm in trunc.finditer(norm):
            check_trunc(trunc_var(tm))
        for tm in cp_null.finditer(norm):
            check_trunc(trunc_var(tm))
        tmx = tee_cmd.search(norm)
        if tmx and not any(o == "--append" or (o.startswith("-") and not o.startswith("--") and "a" in o) for o in tmx.group(1).split()):
            for tok in tmx.group(2).split():
                vm = re.fullmatch(VAR, tok.rstrip(";"))
                if vm:
                    check_trunc(trunc_var(vm))
        for opts, targets in scan_rm(line):
            recursive = any(o == "--recursive" or (o.startswith("-") and not o.startswith("--") and re.search(r"[rR]", o)) for o in opts)
            if not recursive:
                continue
            ok = bool(targets)
            for target in targets:   # EVERY target must be a mktemp -d variable, whole
                tmatch = re.fullmatch(r"\$(?:\{([A-Za-z_][A-Za-z0-9_]*)(?::\?)?\}|([A-Za-z_][A-Za-z0-9_]*))", target)
                var = (tmatch.group(1) or tmatch.group(2)) if tmatch else None
                if var is None or var not in allowed:
                    ok = False
            if not ok:
                bad("recursive rm (every target must be a variable assigned from mktemp -d): %s" % line)
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
