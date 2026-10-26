#!/usr/bin/env bash
# Portability lint for the shell scripts that run on stock macOS (BSD tools,
# bash 3.2). GNU tools must never be a requirement.
#
#   lint-portability.sh [dir]      (default: this tree's scripts/)
#
# Fails (exit 1, "file:line: reason") on, in any *.sh, outside comments and
# python heredocs:
#   - a regex repeat bound above 255 in grep, sed, awk or [[ =~ ]] (macOS's
#     regex library stops at RE_DUP_MAX=255: "maximum repetition exceeds 255")
#   - sed -i without a suffix argument (BSD needs -i '' or -i.bak), grep -P,
#     \s \w \b in grep or sed, readlink -f, xargs -r, find -printf / -regex,
#     stat -c without a stat -f fallback on the line, date -d, base64 -w,
#     mktemp -p
# A line carrying "portability-ok" is skipped; a file with
# "portability-lint: skip-file" in its first lines is skipped.
set -euo pipefail
DIR="${1:-$(cd "$(dirname "$0")" && pwd)}"
[ -d "$DIR" ] || { echo "not a directory: $DIR" >&2; exit 2; }   # fs-ok: usage check, positive: false refuses
exec python3 -I - "$DIR" <<'PY'
import os, re, sys

def words(line):
    return re.split(r"\s+", line.strip())

findings = []
for name in sorted(os.listdir(sys.argv[1])):
    if not name.endswith(".sh"): continue
    lines = open(os.path.join(sys.argv[1], name), errors="replace").read().splitlines()
    if any("portability-lint: skip-file" in l for l in lines[:5]): continue
    term = None
    for n, raw in enumerate(lines, 1):
        if term is not None:
            if raw.strip() == term: term = None
            continue
        m = re.search(r"<<-?\s*['\"]?([A-Za-z_]+)['\"]?", raw)
        if m and "python" in raw:
            term = m.group(1)
        line = raw.strip()
        if not line or line.startswith("#") or "portability-ok" in line: continue
        def bad(msg): findings.append("%s:%d: %s" % (name, n, msg))
        tw = words(line)
        has = lambda w: w in tw or re.search(r"(^|[\s|;&(`$])%s\b" % w, line)
        # repeat bounds: only where a regex is
        if re.search(r"\b(grep|egrep|sed|awk)\b|=~", line):
            for b in re.finditer(r"\\?\{(\d+)(?:,(\d*))?\\?\}", line):
                if any(g and int(g) > 255 for g in b.groups()):
                    bad("regex repeat bound %s exceeds 255 (macOS regex stops at RE_DUP_MAX=255)" % b.group(0))
        # sed -i without a suffix argument
        for i, t in enumerate(tw):
            if t == "sed":
                for j in range(i + 1, len(tw)):
                    u = tw[j]
                    if u in ("|", ";", "&&", "||"): break
                    if u == "--in-place" or re.fullmatch(r"-[A-Za-z]*i", u):
                        nxt = tw[j + 1] if j + 1 < len(tw) else ""
                        if nxt not in ("''", '""'): bad("sed -i needs a suffix argument on BSD (-i '' or -i.bak)")
            if t == "grep":
                for u in tw[i + 1:]:
                    if u in ("|", ";", "&&", "||"): break
                    if u == "--perl-regexp" or re.fullmatch(r"-[A-Za-z]*P[A-Za-z]*", u): bad("grep -P is GNU only")
        if re.search(r"\b(grep|sed)\b", line) and re.search(r"\\[swbSWB](?![A-Za-z])", line):
            bad("\\s, \\w, \\b are not portable in grep/sed (use [[:space:]], [[:alnum:]_])")
        if re.search(r"\breadlink\s+(-[A-Za-z]*f|--canonicalize)", line): bad("readlink -f is GNU only")
        if re.search(r"\bxargs\s+(-[A-Za-z]*r\b|--no-run-if-empty)", line): bad("xargs -r is GNU only")
        if re.search(r"\bfind\b.*\s-(printf|regex)\b", line): bad("find -printf / -regex are GNU only")
        if re.search(r"\bstat\s+(-[A-Za-z]+\s+)*-c\b", line) and not re.search(r"\bstat\s+(-[A-Za-z]+\s+)*-f\b", line):
            bad("stat -c needs a stat -f fallback on the same line")
        if re.search(r"\bdate\s+(-[A-Za-z]*d\b|--date)", line): bad("date -d is GNU only")
        if re.search(r"\bbase64\s+(-[A-Za-z]*w|--wrap)", line): bad("base64 -w is GNU only")
        if re.search(r"\bmktemp\s+(-[A-Za-z]*p\b|--tmpdir)", line): bad("mktemp -p is GNU only")
if findings:
    print("\n".join(findings))
    sys.exit(1)
PY
