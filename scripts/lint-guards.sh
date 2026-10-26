#!/usr/bin/env bash
# Guard lint: every "may we proceed?" decision in the setup scripts is a
# registered guard that counts as safe only on positive confirmation, and no
# script asks the filesystem a question a stat error can answer "absent".
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
#   - scripts/guards.registry lists every guard ("<name> <tools or ->"; a line
#     "@helper <name>" allows a guard path to call that function defined outside
#     the guards block), and scripts/guard_fault_test.sh has a fixture_<name>
#     function for each guard, which is what injects the faults.
# Fails (exit 1, "file:line: reason") when:
#   - a guard/guard_ok call names a guard that is not in the registry;
#   - a registry name has no <name>_check function, no call, or no fixture;
#   - a guard path (a *_check function anywhere, or any line between the guards
#     markers) reads safety from a bare $?, from "if <cmd>; then" / "elif <cmd>" /
#     "while <cmd>" / "until <cmd>" / "! <cmd>", from "<cmd> && ..." (a [ test ] on
#     the left is fine) or from case "$(cmd)" in; hides a failure with "|| true" /
#     "|| :" / "|| return 0" / "|| echo SAFE" / "x=$(cmd) || x=..." / "; true)" /
#     "local x=$(cmd)" (the status is lost); continues a command over a backslash
#     line break (each line is checked alone); probes the filesystem with find or
#     ls; or calls a function defined outside the guards block (other than the
#     registry's @helper ones). Capture a tool's output with "x=$(cmd) || refuse"
#     and compare it with what a working tool must say.
#   - ANYWHERE in a script, a file test ([ [[ test with -e -f -d -L -h -a -s -r -w
#     -x -O -G -N, with ! too) is used that does not go through path_state /
#     g_state / g_kind: those read every stat error as "absent". A line may opt
#     out with a real trailing comment "# fs-ok: <reason>" saying why a stat
#     error cannot hurt there (at least 3 words or 12 characters). The operators
#     also include -S -p -b -c -g -u -k -nt -ot -ef; the test may sit behind
#     command/builtin/env/time, a path (/bin/test, /usr/bin/[), a backtick or a
#     case arm "a) [ -e x ]"; a quoted operator counts; a test whose operator is a
#     variable ([ $op x ]) is flagged. eval is flagged anywhere (it hides what runs).
#   - OUTSIDE the guard paths, best effort: an existence decision read from command
#     output ([ -z "$(cat f 2>/dev/null)" ], the output of ls/stat/find tested with
#     [ -z / [ -n directly or through a variable of the same function, "read < f ||")
#     needs the same "# fs-ok: <reason>".
#   - a guard path calls a function through a variable ($f args, $($f args)), or an
#     "@helper" of the registry is not defined in guard_fault_test.sh.
#   - a heredoc (a real <<WORD outside quotes, comments, $(( )) and <<<; <<- and
#     quoted delimiters included) never closes before the end of the file.
# "# guard-ok: <reason>" (a real trailing comment) exempts a line from the
# "x=$(cmd) || x=..." rule only, never from the others. A marker inside a string
# or without a reason is not a marker. A line-based check, not a shell parser.
set -euo pipefail
DIR="${1:-$(cd "$(dirname "$0")" && pwd)}"
[ -d "$DIR" ] || { echo "not a directory: $DIR" >&2; exit 2; }   # fs-ok: usage check, positive: false refuses
exec python3 -I - "$DIR" <<'PY'
import os, re, sys

d = sys.argv[1]
findings = []
scripts = {}
for name in sorted(os.listdir(d)):
    if name.endswith(".sh") and not name.endswith("_test.sh"):
        scripts[name] = open(os.path.join(d, name), errors="replace").read().splitlines()

registry, helpers = {}, {}
reg_path = os.path.join(d, "guards.registry")
if os.path.exists(reg_path):
    for n, raw in enumerate(open(reg_path).read().splitlines(), 1):
        w = raw.split()
        if not w or w[0].startswith("#"):
            continue
        if w[0] == "@helper" and len(w) > 1:
            helpers[w[1]] = n
        else:
            registry[w[0]] = n
fixtures_path = os.path.join(d, "guard_fault_test.sh")
fixtures = open(fixtures_path, errors="replace").read() if os.path.exists(fixtures_path) else ""

CMD = r"(?:^|[;&|{(]\s*|\b(?:then|do|else|if)\s+|!\s*|[A-Za-z_][A-Za-z0-9_]*=\S+\s+)"
call_re = re.compile(CMD + r"guard(?:_ok)?\s+([A-Za-z_][A-Za-z0-9_]*)(?![A-Za-z0-9_=])")
bound_re = re.compile(CMD + r"guard(?:_ok)?\s+[A-Za-z_][A-Za-z0-9_]*\s+--(?:\s|$)")
run_re = re.compile(CMD + r"guard_run\b")
FNDEF = r"^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(\s*\))?\s*\{"
fn_def_re = re.compile(FNDEF)
fn_check_re = re.compile(r"^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)_check\s*(?:\(\s*\))?\s*\{")
FILE_OPS = r"(?:e|f|d|L|h|a|s|r|w|x|O|G|N|S|p|b|c|g|u|k|nt|ot|ef)"
# where a command may start: a line start, ; & | { ( ! ` ), a case arm "x)", or a keyword
PRE = r"(?:^|[;&|{(!`]\s*|\)\s*|\b(?:then|do|else|if|elif|while|until|time)\s+|&&\s*|\|\|\s*)"
# ... and the wrappers that still run the test: command, builtin, env [-x] [VAR=v], time, a path to it
WRAP = r"(?:(?:command|builtin|exec|nice|nohup|time|env)\s+(?:-\S+\s+|[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*)*(?:/(?:usr/)?(?:local/)?bin/)?"
bracket_re = re.compile(PRE + WRAP + r"(\[\[?)\s")
test_re = re.compile(PRE + WRAP + r"test\s")
op_re = re.compile(r"(?:^|\s)-" + FILE_OPS + r"(?=\s)")
varcall_re = re.compile(CMD.replace(r"=\S+\s+", r"=[^\s()]*\s+") + r"[\"']?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?[\"']?(?=\s|;|\)|$)")
# existence decisions read from command output (best effort, outside the guard paths)
outread_re = re.compile(r"\[\[?\s+(?:!\s*)?-[zn]\s+\"?\$\(\s*(?:cat|ls|stat|find)\b")
outassign_re = re.compile(r"^\s*(?:local\s+)?([A-Za-z_][A-Za-z0-9_]*)=\"?\$\(\s*(?:cat|ls|stat|find)\b")
outvar_re = re.compile(r"\[\[?\s+(?:!\s*)?-[zn]\s+\"?\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?\"?\s")
readfile_re = re.compile(r"\bread\b[^;|&<]*<(?!<)\s*[^\s<&|;]+\s*\|\|")

def heredocs_in(line, q):
    """the heredocs a line opens: ([(delimiter, strip_tabs)...], quote state at the end of the line).
    Only a real << operator counts: not inside quotes, a comment, $(( )) / (( )), and not <<<."""
    out, i, n, arith = [], 0, len(line), 0
    while i < n:
        c = line[i]
        if q == "'":
            if c == "'": q = None
            i += 1; continue
        if q == '"':
            if c == "\\": i += 2; continue
            if c == '"': q = None
            i += 1; continue
        if arith:
            if c == "(": arith += 1
            elif c == ")": arith -= 1
            i += 1; continue
        if c == "\\": i += 2; continue
        if c in "'\"": q = c; i += 1; continue
        if c == "#" and (i == 0 or line[i - 1] in " \t;|&("): break
        if line.startswith("((", i): arith = 2; i += 2; continue
        if line.startswith("<<<", i): i += 3; continue
        if line.startswith("<<", i):
            j, strip = i + 2, False
            if j < n and line[j] == "-": strip = True; j += 1
            while j < n and line[j] in " \t": j += 1
            word = ""
            while j < n and line[j] not in " \t;&|()<>":
                if line[j] in "'\"":
                    k = line.find(line[j], j + 1)
                    if k < 0: k = n
                    word += line[j + 1:k]; j = k + 1
                elif line[j] == "\\" and j + 1 < n:
                    word += line[j + 1]; j += 2
                else:
                    word += line[j]; j += 1
            if word: out.append((word, strip))
            i = j; continue
        i += 1
    return out, q

def trailing_comment(line):
    """the text of a real trailing comment (a # after whitespace, outside quotes), or None"""
    q, i, n = None, 0, len(line)
    while i < n:
        c = line[i]
        if q:
            if c == "\\" and q == '"':
                i += 2; continue
            if c == q:
                q = None
        elif c in "\"'":
            q = c
        elif c == "\\":
            i += 2; continue
        elif c == "#" and (i == 0 or line[i - 1] in " \t"):
            return line[i:]
        i += 1
    return None

def marked(line, kind):
    c = trailing_comment(line)
    m = c and re.match(r"#\s*" + kind + r":\s*(\S.*)$", c)
    if not m:
        return False
    if kind == "fs-ok":   # a reason, not a placeholder: at least 3 words or 12 characters
        r = m.group(1).strip()
        return len(r.split()) >= 3 or len(r) >= 12
    return True

def code_of(line):
    c = trailing_comment(line)
    return line[: len(line) - len(c)] if c else line

called, defined = set(), set()

def add(file, n, msg):
    findings.append("%s:%d: %s" % (file, n, msg))

def segs_before_and(line):
    """for each '&&' in the line: the command segment on its left"""
    out = []
    for m in re.finditer(r"&&", line):
        left = line[:m.start()]
        left = re.split(r";|\|\||&&|\bthen\b|\bdo\b|\{|\(", left)[-1].strip()
        out.append(left)
    return out

def unquoted_code(code):
    """the code with string contents dropped, except "$var" (kept as $var) and $( ... ) inside a string"""
    out, i, n, q = [], 0, len(code), None
    while i < n:
        c = code[i]
        if q == "'":
            if c == "'": q = None; out.append("''")
            i += 1; continue
        if q == '"':
            if c == "\\": i += 2; continue
            if code.startswith("$(", i):
                depth, j = 0, i
                while j < n:
                    if code[j] == "(": depth += 1
                    elif code[j] == ")":
                        depth -= 1
                        if depth == 0: break
                    j += 1
                out.append(code[i:j + 1]); i = j + 1; continue
            if c == '"': q = None; out.append('""')
            i += 1; continue
        if c in "'\"":
            m = re.match(r'"(\$\{?[A-Za-z_][A-Za-z0-9_]*\}?)"', code[i:])
            if m: out.append(m.group(1)); i += m.end(); continue
            q = c; i += 1; continue
        out.append(c); i += 1
    return "".join(out)

def var_operator(seg):
    """true if the operator of a [ ] / test segment is a variable ([ $op x ], [ a $op b ])"""
    t = re.sub(r'"(?:[^"\\]|\\.)*"|\'[^\']*\'|\$\([^()]*\)', lambda m: re.sub(r"\s", "_", m.group(0)), seg)
    toks = t.split()
    while toks and toks[0] == "!":
        toks.pop(0)
    if len(toks) == 2:
        op = toks[0]
    elif len(toks) >= 3 and not toks[0].startswith("-"):
        op = toks[1]
    else:
        return False
    return op.lstrip("\"'").startswith(("$", "`"))

def file_tests(code):
    """why the code line has a file test inside [ ], [[ ]] or test (or None)"""
    segs = []
    for m in bracket_re.finditer(code):
        close = "]]" if m.group(1) == "[[" else "]"
        end = code.find(close, m.end())
        segs.append(code[m.end(): end if end >= 0 else len(code)])
    for m in test_re.finditer(code):
        end = re.search(r";|&&|\|\|", code[m.end():])
        segs.append(code[m.end(): m.end() + end.start() if end else len(code)])
    def plain(t):   # a file test's operator is a bare word: drop $( ... ) and quoted strings first
        t = re.sub(r"(?<![\w$])([\"'])(-[A-Za-z]{1,2})\1(?=\s|$)", r"\2", t)   # a quoted operator is still the operator
        t = re.sub(r"\$\([^()]*\)", "", t)
        return re.sub(r"\"[^\"]*\"|'[^']*'", '""', t)
    if any(op_re.search(" " + plain(s_)) for s_ in segs):
        return "a file test reads a stat error as 'absent': ask path_state/g_state/g_kind, or say why with a trailing '# fs-ok: <reason>' (3 words or 12 characters at least)"
    if any(var_operator(s_) for s_ in segs):
        return "a test whose operator is a variable can be a file test in disguise: spell the operator, or say why with a trailing '# fs-ok: <reason>' (3 words or 12 characters at least)"
    return None

# functions defined outside the guard regions (a guard path must not call them)
outside = {}
for name, lines in scripts.items():
    mode = None
    for n, raw in enumerate(lines, 1):
        s = raw.strip()
        if s in ("# >>> guard-helper", "# >>> guard-py", "# >>> guards"): mode = s.split()[2]; continue
        if s in ("# <<< guard-helper", "# <<< guard-py", "# <<< guards"): mode = None; continue
        m = fn_def_re.match(raw)
        if m and mode is None and not fn_check_re.match(raw):
            outside.setdefault(m.group(1), (name, n))

for name, lines in scripts.items():
    mode = None          # None | "guard-helper" | "guard-py" | "guards"
    infn = False         # inside a <name>_check function (anywhere)
    pending = []         # heredocs opened and not yet closed: (delimiter, strip_tabs, line opened)
    q = None             # an open quote carried over from the previous line
    tracked = {}         # variables assigned from cat/ls/stat/find output in this function
    for n, raw in enumerate(lines, 1):
        s = raw.strip()
        if pending:
            delim, strip, _ = pending[0]
            if (raw.lstrip("\t") if strip else raw) == delim: pending.pop(0)
            continue
        if s in ("# >>> guard-helper", "# >>> guard-py", "# >>> guards"): mode = s.split()[2]; continue
        if s in ("# <<< guard-helper", "# <<< guard-py", "# <<< guards"): mode = None; continue
        opened, q = heredocs_in(raw, q)
        hm = [(dl, st, n) for dl, st in opened]
        code = code_of(raw).strip()
        if mode in ("guard-helper", "guard-py"):
            pending.extend(hm)
            continue
        m = fn_check_re.match(raw)
        if m:
            defined.add(m.group(1)); infn = True
        if m and raw.rstrip().endswith("}"):
            infn = False   # a one-line function: the next lines are not part of it
        if not s or s.startswith("#"):
            if infn and raw.startswith("}"): infn = False
            continue
        guard_path = (mode == "guards") or infn
        # --- anywhere in a script ---
        if re.search(r"(?:^|[;&|{(]\s*|\b(?:then|do|else|if)\s+)eval\b", code):
            add(name, n, "eval hides what runs: %s" % s)
        why = file_tests(code)
        if why and not marked(raw, "fs-ok"):
            add(name, n, "%s: %s" % (why, s))
        if not guard_path and not marked(raw, "fs-ok"):
            if outread_re.search(code) or readfile_re.search(code):
                add(name, n, "an existence decision read from command output (a failed cat/ls/stat/find or read < file looks like 'absent'): ask path_state/g_state, or say why with a trailing '# fs-ok: <reason>': %s" % s)
            for vm in outvar_re.finditer(code):
                if vm.group(1) in tracked:
                    add(name, n, "tests the output of cat/ls/stat/find (assigned at line %d) for emptiness, which reads a failure as 'absent': ask path_state/g_state, or say why with a trailing '# fs-ok: <reason>': %s" % (tracked[vm.group(1)], s))
        if not guard_path:
            am = outassign_re.match(code)
            if am and not marked(raw, "fs-ok"):
                tracked[am.group(1)] = n
            elif am:
                tracked.pop(am.group(1), None)
        if raw.startswith("}") or fn_def_re.match(raw):
            tracked.clear()
        for cm in call_re.finditer(s):
            called.add(cm.group(1))
            if cm.group(1) not in registry:
                add(name, n, "guard %s is not in guards.registry" % cm.group(1))
        if run_re.search(s):
            add(name, n, "guard_run is for the helper and the tests only: %s" % s)
        if bound_re.search(s):
            add(name, n, "a guard runs <name>_check and nothing else (guard <name> args..., not 'guard <name> -- <cmd>'): %s" % s)
        # --- guard paths ---
        if guard_path:
            if re.search(r"\$\?", code):
                add(name, n, "a guard path reads safety from a bare $?: %s" % s)
            if re.search(r"\|\|\s*(true\b|:(\s|;|$)|return\s+0\b)", code):
                add(name, n, "a guard path hides a failure with || true / || : / || return 0: %s" % s)
            if re.search(r"\|\|\s*\{[^}]*SAFE", code) or re.search(r"\|\|\s*(?:echo|printf)\b[^;]*SAFE", code):
                add(name, n, "a guard path has a || branch that says SAFE: %s" % s)
            if re.search(r";\s*(?:true|:)\s*\)", code):
                add(name, n, "a guard path masks a status with '; true)': %s" % s)
            if re.search(r"\b(?:local|declare|export|readonly|typeset)\s+(?:-\w+\s+)?[A-Za-z_][A-Za-z0-9_]*=.*\$\(", code):
                add(name, n, "declare-and-assign from a command substitution loses the status (assign on its own line): %s" % s)
            if re.search(r"\)\"?\s*\|\|\s*[A-Za-z_][A-Za-z0-9_]*=", code) and not marked(raw, "guard-ok"):
                add(name, n, "a guard path turns a failure into a value (x=$(cmd) || x=...); say why with a trailing '# guard-ok: <reason>' if it is the stricter path: %s" % s)
            for part in code.split(";"):
                mi = re.match(r"(?:then\s+)?(?:if|elif)\s+(.*)$", part.strip())
                if mi and not re.match(r"(?:!\s*)?(?:\[|\(\()", mi.group(1)):
                    add(name, n, "a guard path decides on 'if <cmd>': %s" % s)
                mw = re.match(r"(?:then\s+|do\s+)?(?:while|until)\s+(.*)$", part.strip())
                if mw and not re.match(r"(?:!\s*)?(?:\[|\(\()", mw.group(1)):
                    add(name, n, "a guard path decides on 'while/until <cmd>': %s" % s)
                if re.match(r"(?:then\s+|do\s+)?!\s+(?!\[)\S", part.strip()):
                    add(name, n, "a guard path negates a command ('! <cmd>'): %s" % s)
            for left in segs_before_and(code):
                left = re.sub(r"^(?:if|elif|while|until)\s+", "", left)
                if left and not re.match(r"(?:!\s*)?(?:\[|\(\()", left):
                    add(name, n, "a guard path uses '<cmd> && ...' (a [ test ] on the left is fine): %s" % s)
            if re.match(r"case\s+\"?\$\(", code.strip()):
                add(name, n, "a guard path decides on case \"$(cmd)\" in: %s" % s)
            if code.rstrip().endswith("\\"):
                add(name, n, "a guard path continues a command over a line break (each line is checked alone): %s" % s)
            if re.search(CMD + r"(?:find|ls)\b", code):
                add(name, n, "a guard path probes the filesystem with find/ls (use path_state/g_state): %s" % s)
            if varcall_re.search(unquoted_code(code)):
                add(name, n, "a guard path calls a function through a variable (what runs is not visible to the lint): %s" % s)
            for fn, where in outside.items():
                if fn in helpers:
                    continue
                if re.search(CMD + re.escape(fn) + r"(?=\s|;|\)|$)", code) or re.search(r"\$\(\s*" + re.escape(fn) + r"(?=\s|;|\))", code):
                    add(name, n, "a guard path calls %s, defined outside the guards block (%s:%d); move it in or list '@helper %s' in guards.registry: %s" % (fn, where[0], where[1], fn, s))
        pending.extend(hm)
        if infn and raw.startswith("}"):
            infn = False
    for delim, strip, opened_at in pending:
        add(name, opened_at, "the heredoc <<%s%s is never closed (no line %s%s before the end of the file), so the rest of the file would be skipped unchecked" % ("-" if strip else "", delim, "with only tabs and " if strip else "equal to ", delim))

used = set(called)
for gname, rn in sorted(registry.items()):
    if gname not in defined:
        add("guards.registry", rn, "%s has no %s_check function" % (gname, gname))
    if gname not in used:
        add("guards.registry", rn, "%s is never run through guard/guard_ok" % gname)
    if not re.search(r"(?m)^fixture_%s\(\)" % re.escape(gname), fixtures):
        add("guards.registry", rn, "%s has no fixture_%s in guard_fault_test.sh (no injection cases)" % (gname, gname))
for hname, hn in sorted(helpers.items()):
    if not re.search(r"(?m)^(?:function\s+)?%s\s*(?:\(\s*\))?\s*\{" % re.escape(hname), fixtures):
        add("guards.registry", hn, "@helper %s is not defined in guard_fault_test.sh (the fault test must define it, or the guard cannot run there)" % hname)
for fn in sorted(defined):
    if fn not in registry:
        add("(scripts)", 0, "%s_check is not in guards.registry" % fn)
if findings:
    print("\n".join(findings))
    sys.exit(1)
PY
