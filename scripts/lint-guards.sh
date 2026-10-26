#!/usr/bin/env bash
# Guard lint: every "may we proceed?" decision in the setup scripts is a
# registered guard that counts as safe only on positive confirmation.
#
#   lint-guards.sh [dir]      (default: this tree's scripts/)
#
# The convention (docs/ci.md):
#   - a guard is a function named <name>_check; run it as "guard <name> args..."
#     or "guard_ok <name> args..." (the helper between "# >>> guard-helper" and
#     "# <<< guard-helper"), which runs <name>_check and nothing else (no
#     "guard <name> -- <cmd>", no guard_run outside the helper) and accepts only
#     exit status 0 plus a last stdout line of exactly SAFE:<name>;
#   - guards and what they use live between "# >>> guards" and "# <<< guards"
#     (Python probe sources between "# >>> guard-py" and "# <<< guard-py"; the
#     helper and the probe sources are not guard paths);
#   - scripts/guards.registry lists every guard ("<name> <tools or ->"), and
#     scripts/guard_fault_test.sh has a fixture_<name> function for each, which
#     is what injects the faults.
# Fails (exit 1, "file:line: reason") when:
#   - a guard/guard_ok call names a guard that is not in the registry;
#   - a registry name has no <name>_check function, no call, or no fixture;
#   - a guard path (a *_check function anywhere, or any line between the guards
#     markers) reads safety from a bare $?, from "if <cmd>; then" / "elif <cmd>" /
#     "while <cmd>", from "<cmd> && ..." (a [ test ] on the left is fine) or from
#     case "$(cmd)" in, hides a failure with "|| true" / "|| :" / "|| return 0" /
#     "x=$(cmd) || x=..." / a "||" branch that says SAFE, continues a command over
#     a backslash line break, or asks whether a path exists with [ -e ] / [ -L ] /
#     [ -h ] / [ ! -d ] (use path_state: those read every stat error as "absent").
#     Capture the tool's output with "x=$(cmd) || refuse" and compare it with what
#     a working tool must say. "guard-ok: <reason>" on a line opts it out.
# A line carrying "guard-ok" is skipped. A line-based check, not a shell parser.
set -euo pipefail
DIR="${1:-$(cd "$(dirname "$0")" && pwd)}"
[ -d "$DIR" ] || { echo "not a directory: $DIR" >&2; exit 2; }
exec python3 -I - "$DIR" <<'PY'
import os, re, sys

d = sys.argv[1]
findings = []
scripts = {}
for name in sorted(os.listdir(d)):
    if name.endswith(".sh") and not name.endswith("_test.sh"):
        scripts[name] = open(os.path.join(d, name), errors="replace").read().splitlines()

registry = {}
reg_path = os.path.join(d, "guards.registry")
if os.path.exists(reg_path):
    for n, raw in enumerate(open(reg_path).read().splitlines(), 1):
        w = raw.split()
        if w and not w[0].startswith("#"):
            registry[w[0]] = n
fixtures_path = os.path.join(d, "guard_fault_test.sh")
fixtures = open(fixtures_path, errors="replace").read() if os.path.exists(fixtures_path) else ""

CMD = r"(?:^|[;&|{(]\s*|\b(?:then|do|else|if)\s+|!\s*|[A-Za-z_][A-Za-z0-9_]*=\S+\s+)"
call_re = re.compile(CMD + r"guard(?:_ok)?\s+([A-Za-z_][A-Za-z0-9_]*)(?![A-Za-z0-9_=])")
bound_re = re.compile(CMD + r"guard(?:_ok)?\s+[A-Za-z_][A-Za-z0-9_]*\s+--(?:\s|$)")
run_re = re.compile(CMD + r"guard_run\b")
fn_re = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)_check\(\)\s*\{")
called, defined = set(), set()

def add(file, n, msg):
    findings.append("%s:%d: %s" % (file, n, msg))

def segs_before_and(line):
    """for each '&&' in the line: the command segment on its left"""
    out = []
    for m in re.finditer(r"&&", line):
        left = line[:m.start()]
        left = re.split(r";|\|\||&&|\bthen\b|\bdo\b|\{|\(", left)[-1].strip()
        left = re.sub(r"^(?:if|elif|while|until)\s+", "", left)
        out.append(left)
    return out

for name, lines in scripts.items():
    mode = None          # None | "helper" | "py" | "guards"
    infn = False         # inside a <name>_check function (anywhere)
    for n, raw in enumerate(lines, 1):
        s = raw.strip()
        if s == "# >>> guard-helper": mode = "helper"; continue
        if s == "# <<< guard-helper": mode = None; continue
        if s == "# >>> guard-py": mode = "py"; continue
        if s == "# <<< guard-py": mode = None; continue
        if s == "# >>> guards": mode = "guards"; continue
        if s == "# <<< guards": mode = None; continue
        if mode in ("helper", "py"):
            continue
        m = fn_re.match(raw)
        if m:
            defined.add(m.group(1)); infn = True
        if m and raw.rstrip().endswith("}"):
            infn = False   # a one-line function: the next lines are not part of it
        if not s or s.startswith("#") or "guard-ok" in s:
            if infn and raw.startswith("}"): infn = False
            continue
        if run_re.search(s):
            add(name, n, "guard_run is for the helper and the tests only: %s" % s)
        if bound_re.search(s):
            add(name, n, "a guard runs <name>_check and nothing else (guard <name> args..., not 'guard <name> -- <cmd>'): %s" % s)
        for cm in call_re.finditer(s):
            called.add(cm.group(1))
            if cm.group(1) not in registry:
                add(name, n, "guard %s is not in guards.registry" % cm.group(1))
        if mode == "guards" or infn:
            if re.search(r"\$\?", s):
                add(name, n, "a guard path reads safety from a bare $?: %s" % s)
            if re.search(r"\|\|\s*(true\b|:(\s|;|$)|return\s+0\b)", s):
                add(name, n, "a guard path hides a failure with || true / || : / || return 0: %s" % s)
            if re.search(r"\|\|\s*\{[^}]*SAFE", s):
                add(name, n, "a guard path has a || branch that says SAFE: %s" % s)
            if re.search(r"\)\"?\s*\|\|\s*[A-Za-z_][A-Za-z0-9_]*=", s):
                add(name, n, "a guard path turns a failure into a value (x=$(cmd) || x=...); say why with guard-ok if it is the stricter path: %s" % s)
            if re.match(r"(?:then\s+)?while\s", s):
                add(name, n, "a guard path decides on 'while <cmd>': %s" % s)
            if re.match(r"case\s+\"?\$\(", s):
                add(name, n, "a guard path decides on case \"$(cmd)\" in: %s" % s)
            if s.endswith("\\"):
                add(name, n, "a guard path continues a command over a line break (each line is checked alone): %s" % s)
            if re.search(r"(?<![\w-])-(?:e|L|h)(?=\s)", s) or re.search(r"!\s+-[dfrwxsSbcpgukO](?=\s)", s):
                add(name, n, "a guard path asks whether a path exists with a file test (use path_state; -e/-L/-h/! -d read a stat error as absent): %s" % s)
            for part in s.split(";"):
                mi = re.match(r"(?:then\s+)?(?:if|elif)\s+(.*)$", part.strip())
                if mi and not re.match(r"(?:!\s*)?(?:\[|\(\()", mi.group(1)):
                    add(name, n, "a guard path decides on 'if <cmd>': %s" % s)
            for left in segs_before_and(s):
                if left and not re.match(r"(?:!\s*)?(?:\[|\(\()", left):
                    add(name, n, "a guard path uses '<cmd> && ...' (a [ test ] on the left is fine): %s" % s)
        if infn and raw.startswith("}"):
            infn = False

used = set(called)
for gname, rn in sorted(registry.items()):
    if gname not in defined:
        add("guards.registry", rn, "%s has no %s_check function" % (gname, gname))
    if gname not in used:
        add("guards.registry", rn, "%s is never run through guard/guard_ok" % gname)
    if not re.search(r"(?m)^fixture_%s\(\)" % re.escape(gname), fixtures):
        add("guards.registry", rn, "%s has no fixture_%s in guard_fault_test.sh (no injection cases)" % (gname, gname))
for fn in sorted(defined):
    if fn not in registry:
        add("(scripts)", 0, "%s_check is not in guards.registry" % fn)
if findings:
    print("\n".join(findings))
    sys.exit(1)
PY
