#!/usr/bin/env bash
# End-to-end test of scripts/bootstrap.sh in a fake HOME with stub gh,
# systemctl, claude, go and ssh-keyscan on PATH, and local bare repos standing
# in for the GitHub remotes. It never touches the real HOME, ~/.ssh, systemd or
# GitHub. Usage: scripts/bootstrap_test.sh [workdir]   (default <tree>/.bin/bootstrap-test;
# it is wiped, and must not be under TMPDIR or /tmp)
#
# claude-memsync is the REAL binary built from this tree (MEMSYNC_BIN and
# MEMMERGE_BIN name prebuilt ones; otherwise it is built with go, or with docker
# on Linux). The "go build" the bootstrap runs installs a thin recording
# wrapper that execs the real binary for every subcommand except:
#   install, start, stop  stubbed (they would register a real service)
#   doctor                stubbed (it calls the live GitHub API)
# and for `init`, whose --private-only check would call the live GitHub API, the
# wrapper drops --private-only before it execs the real binary (the recorded
# log line keeps the original arguments, which the checks assert on). The token
# flags go through, so init saves them in config.json like the real thing.
# app-token is real too: it talks to a loopback fake of the GitHub App API.
#
# BT_QUICK=1 stops after the new-flag sections (prerequisites, pause, token file,
# prebuilt dir), for a fast loop while editing; CI runs everything.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
# The default work dir is inside the tree (untracked), not under TMPDIR: the upgrade
# check needs binaries in a dir the daemon does not take for a throwaway.
BASE="${1:-${BOOTSTRAP_TEST_DIR:-$ROOT/.bin/bootstrap-test}}"
TOKEN="dummy-token-$$-do-not-persist"
ORGTOK="dummy-orgtoken-$$-do-not-persist"
ORG=example-org
PQREC=""   # a post-quantum recovery public key; set once the real binary has made one
PASS=0 FAIL=0 PENDING=0

rm -rf "$BASE"; mkdir -p "$BASE/stubs" "$BASE/remotes"
printf '%s\n' "$ORGTOK" >"$BASE/orgtoken"; chmod 600 "$BASE/orgtoken"
ORGARGS=(--org-token-file "$BASE/orgtoken")

ok()      { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()     { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
pending() { PENDING=$((PENDING + 1)); printf 'PENDING %s\n' "$1"; }
check()   { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }

# --- the real binary ---------------------------------------------------------
REAL="$BASE/real"; mkdir -p "$REAL"
if [ -n "${MEMSYNC_BIN:-}" ] && [ -n "${MEMMERGE_BIN:-}" ]; then
  cp "$MEMSYNC_BIN" "$REAL/claude-memsync"; cp "$MEMMERGE_BIN" "$REAL/claude-memmerge"
elif command -v go >/dev/null; then
  (cd "$ROOT" && go build -o "$REAL/claude-memsync" ./cmd/claude-memsync && go build -o "$REAL/claude-memmerge" ./cmd/claude-memmerge)
elif command -v docker >/dev/null; then
  docker run --rm --user "$(id -u):$(id -g)" -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath -e GOFLAGS=-buildvcs=false \
    -v "$ROOT:/src:ro" -v "$REAL:/out" -w /src golang:1.25 \
    sh -c 'go build -o /out/claude-memsync ./cmd/claude-memsync && go build -o /out/claude-memmerge ./cmd/claude-memmerge'
else
  echo "need MEMSYNC_BIN and MEMMERGE_BIN, go, or docker to build the real claude-memsync" >&2; exit 2
fi
chmod +x "$REAL/claude-memsync" "$REAL/claude-memmerge"

# --- stubs --------------------------------------------------------------------
STUBS="$BASE/stubs"
cat >"$STUBS/gh" <<'EOF'
#!/bin/sh
echo "gh $*" >>"$STUB_DIR/gh.log"
if [ "$GH_TOKEN" = "$STUB_ORG_TOKEN" ]; then m=org; else m=ambient; fi
echo "$* |$m" >>"$STUB_DIR/gh.who"
need_org() { [ "$m" = org ] || { echo "HTTP 401: bad credentials for org call" >&2; exit 1; }; }
S="$STUB_DIR/ghstate"; mkdir -p "$S"
if [ "${STUB_HANG:-0}" = 1 ] && [ "$1" = repo ] && [ "$2" = create ]; then : >"$STUB_DIR/hung"; sleep 20; fi
case "$1 $2" in
  "auth status") exit 0 ;;
  "auth token") echo "$STUB_TOKEN"; exit 0 ;;
esac
case "$1" in
  api)
    if [ "$2" = user/orgs ]; then
      if [ "$m" = org ]; then printf '%s\n' "${STUB_ORG_ORGS:-example-org}"; else printf '%s\n' "${STUB_ORGS:-example-org}"; fi
      exit 0
    fi
    if [ "$2" = installation/repositories ]; then
      need_org
      printf '%s\n' "${STUB_INSTALL_OWNERS:-example-org}"
      exit 0
    fi
    need_org
    f="$S/$(basename "$2").json"
    [ -f "$f" ] && { cat "$f"; exit 0; }
    echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  repo)
    need_org
    case "$2" in
      create)
        name="$(basename "$3")"
        if [ "${STUB_PUBLIC:-0}" = 1 ]; then
          echo '{"private": false, "visibility": "public"}' >"$S/$name.json"
        else
          echo '{"private": true, "visibility": "private"}' >"$S/$name.json"
        fi ;;
      deploy-key)
        if [ "${STUB_DEPLOYKEY_DISABLED:-0}" = 1 ]; then echo "HTTP 422: deploy keys disabled" >&2; exit 1; fi ;;
    esac ;;
esac
exit 0
EOF
cat >"$STUBS/systemctl" <<'EOF'
#!/bin/sh
echo "systemctl $*" >>"$STUB_DIR/systemctl.log"
# record whether the org token leaked into this process env (count only, never the value)
echo "$(basename "$0"):$(env | grep -v '^STUB_' | grep -cF -- "$STUB_ORG_TOKEN")" >>"$STUB_DIR/env.log"
# like the real one: the answer is on stdout, the exit status says enabled/active or not
if [ "${STUB_OLD_TIMER:-0}" = 1 ]; then echo enabled; exit 0; fi
case "$*" in *is-active*) echo inactive ;; *) echo disabled ;; esac
exit 1
EOF
cat >"$STUBS/claude" <<'EOF'
#!/bin/sh
echo "claude $*" >>"$STUB_DIR/claude.log"
# record whether the org token leaked into this process env (count only, never the value)
echo "$(basename "$0"):$(env | grep -v '^STUB_' | grep -cF -- "$STUB_ORG_TOKEN")" >>"$STUB_DIR/env.log"
EOF
cat >"$STUBS/go" <<'EOF'
#!/bin/sh
if [ "$1" = version ]; then echo "go version go${STUB_GOVER:-1.25.1} linux/amd64"; exit 0; fi
echo "go $*" >>"$STUB_DIR/go.log"
# go build -o <out> <pkg>: write a stub binary
out="$3"
# record whether the org token leaked into this process env (count only, never the value)
echo "$(basename "$0"):$(env | grep -v '^STUB_' | grep -cF -- "$STUB_ORG_TOKEN")" >>"$STUB_DIR/env.log"
case "$out" in
  */claude-memmerge) cp "$REAL_DIR/claude-memmerge" "$out" ;;
  *) cp "$STUB_DIR/memsync-wrap" "$out" ;;
esac
chmod +x "$out"
EOF
cat >"$STUBS/memsync-wrap" <<'EOF'
#!/bin/sh
# claude-memsync as the bootstrap sees it: records its arguments (never the
# token) and runs the real binary, except for install/start/stop/doctor.
# record whether the org token leaked into this process env (count only, never the value)
echo "$(basename "$0"):$(env | grep -v '^STUB_' | grep -cF -- "$STUB_ORG_TOKEN")" >>"$STUB_DIR/env.log"
echo "memsync $*" >>"$STUB_DIR/memsync.log"
case "$1" in
  install | start | stop | doctor) exit 0 ;;
  init)
    # init would call the live GitHub API for --private-only; drop it.
    sd=""; prev=""
    for x in "$@"; do [ "$prev" = --sync-dir ] && sd="$x"; prev="$x"; done
    n=$#; i=0
    while [ "$i" -lt "$n" ]; do
      x="$1"; shift; i=$((i + 1))
      case "$x" in
        --private-only) ;;
        *) set -- "$@" "$x" ;;
      esac
    done
    "$REAL_DIR/claude-memsync" "$@"; rc=$?
    if grep -qE 'githubToken(File|Cmd)' "$sd/config.json" 2>/dev/null; then echo "init-saved-token $sd" >>"$STUB_DIR/memsync.log"; fi
    exit "$rc" ;;
  join)
    if [ "$2" = request ] && { [ "${STUB_NOFP:-0}" = 1 ] || [ "${STUB_OLDFP:-0}" = 1 ]; }; then
      "$REAL_DIR/claude-memsync" "$@" | if [ "${STUB_NOFP:-0}" = 1 ]; then grep -v '^fingerprint:'; else sed 's/^fingerprint: .*/fingerprint: 0123456789abcdef/'; fi
      exit 0
    fi ;;
esac
exec "$REAL_DIR/claude-memsync" "$@"
EOF
cat >"$STUBS/ssh-keyscan" <<'EOF'
#!/bin/sh
# record whether the org token leaked into this process env (count only, never the value)
echo "$(basename "$0"):$(env | grep -v '^STUB_' | grep -cF -- "$STUB_ORG_TOKEN")" >>"$STUB_DIR/env.log"
for a; do h="$a"; done
if [ "${STUB_BAD_HOSTKEY:-0}" = 1 ]; then k="$(cat "$STUB_DIR/otherkey")"; else k="AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"; fi
echo "$h ssh-ed25519 $k"
EOF
cat >"$STUBS/secret-tool" <<'EOF'
#!/bin/sh
echo "secret-tool $*" >>"$STUB_DIR/tool.log"
echo "secret-tool:$(env | grep -v '^STUB_' | grep -cF -- "$STUB_ORG_TOKEN")" >>"$STUB_DIR/env.log"
echo "$STUB_ORG_TOKEN"
EOF
chmod +x "$STUBS"/*
ssh-keygen -q -t ed25519 -N '' -f "$BASE/other" && awk '{print $2}' "$BASE/other.pub" >"$BASE/otherkey"

# --- a loopback fake of the GitHub App API (installation lookup + token) ---------
ssh-keygen -q -t rsa -b 2048 -m PEM -N '' -f "$BASE/app" && mv "$BASE/app" "$BASE/app.pem" && chmod 600 "$BASE/app.pem"
cat >"$BASE/fakeapi.py" <<'PY'
import datetime, http.server, json, os, sys
tok, logp, portp = os.environ["FAKE_TOKEN"], sys.argv[1], sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        open(logp, "a").write("GET %s\n" % self.path)
        if self.path == "/orgs/example-org/installation": return self.reply(200, {"id": 7})
        self.reply(404, {})
    def do_POST(self):
        open(logp, "a").write("POST %s\n" % self.path)
        n = int(self.headers.get("Content-Length") or 0); self.rfile.read(n)
        if self.path == "/app/installations/7/access_tokens":
            exp = (datetime.datetime.utcnow() + datetime.timedelta(minutes=59)).strftime("%Y-%m-%dT%H:%M:%SZ")
            return self.reply(201, {"token": tok, "expires_at": exp})
        self.reply(404, {})
s = http.server.HTTPServer(("127.0.0.1", 0), H)
open(portp, "w").write(str(s.server_address[1]))
s.serve_forever()
PY
# The fake API server must not outlive the harness, and must not hold the pipe
# the harness's output goes through (a reader of `bootstrap_test.sh | tee` would
# wait for it forever): its output goes nowhere and one trap kills it.
FAKE_TOKEN="$ORGTOK" python3 "$BASE/fakeapi.py" "$BASE/fakeapi.log" "$BASE/fakeapi.port" </dev/null >/dev/null 2>&1 &
FAKE_PID=$!
cleanup() { kill "$FAKE_PID" 2>/dev/null || true; wait "$FAKE_PID" 2>/dev/null || true; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do [ -s "$BASE/fakeapi.port" ] && break; sleep 0.25; done
[ -s "$BASE/fakeapi.port" ] || { echo "fake GitHub App API did not start" >&2; exit 2; }
APIPORT="$(cat "$BASE/fakeapi.port")"

# --- a commit of this tree to bootstrap from -----------------------------------
git clone -q --bare "$ROOT" "$BASE/utils.git"
SHA="$(git -C "$ROOT" rev-parse HEAD)"

# An age-encrypted mem-personal that already has a member (a seed host, made
# with the real binary), so a new host there has no key and must ask to join.
SEED="$BASE/seed"; mkdir -p "$SEED/home/claude/-proj/memory"
echo "seed note" >"$SEED/home/claude/-proj/memory/note.md"
printf '[init]\n\tdefaultBranch = main\n' >"$SEED/home/.gitconfig"
git init -q --bare -b main "$SEED/mem-personal.git"
HOME="$SEED/home" GIT_CONFIG_NOSYSTEM=1 "$REAL/claude-memsync" init --remote "$SEED/mem-personal.git" \
  --sync-dir "$SEED/home/sync" --claude-dir "$SEED/home/claude" --encrypt --identity "$SEED/id.txt" >/dev/null 2>&1 \
  || { echo "cannot seed the encrypted mem-personal with the real binary" >&2; exit 2; }
# The seed's own (hybrid, post-quantum) public key doubles as the recovery key the tests pass.
PQREC="$(sed -n 's/^# public key: //p' "$SEED/id.txt")"
case "$PQREC" in age1pq1*) ;; *) echo "the seed identity is not post-quantum" >&2; exit 2 ;; esac
REQ=(--org "$ORG" --recovery-recipient "$PQREC")

mkbare() { # name [with-content]
  if [ "${2:-}" = content ]; then cp -a "$SEED/$1.git" "$BASE/remotes/$1.git"; else git init -q --bare -b main "$BASE/remotes/$1.git"; fi
}

# newhome <name>: fresh fake HOME with insteadOf maps for every bare repo.
newhome() {
  H="${HROOT:-$BASE}/$1/home"; STUB_DIR="${HROOT:-$BASE}/$1/stub"
  # The runner's own XDG_CONFIG_HOME (GitHub's ubuntu images export one) must not
  # leak into a fake HOME: the real binary resolves its config dir through it.
  export XDG_CONFIG_HOME="$H/.config"
  rm -rf "${HROOT:-${BASE:?}}/$1"; mkdir -p "$H" "$STUB_DIR"
  cp "$STUBS/memsync-wrap" "$STUB_DIR/"; cp "$BASE/otherkey" "$STUB_DIR/"
  : >"$STUB_DIR/gh.log"; : >"$STUB_DIR/memsync.log"
  rm -rf "$BASE/remotes"; mkdir -p "$BASE/remotes"
  mkbare mem-personal content
  mkdir -p "$STUB_DIR/ghstate"
  echo '{"private": true, "visibility": "private"}' >"$STUB_DIR/ghstate/mem-personal.json"
  for n in mem-infra-homelab mem-project-code-workspace mem-project-ownproj; do mkbare "$n"; done
  printf '[init]\n\tdefaultBranch = main\n' >"$H/.gitconfig"
  for n in mem-personal mem-infra-homelab mem-project-code-workspace mem-project-ownproj; do
    git config --file "$H/.gitconfig" "url.$BASE/remotes/$n.git.insteadOf" "git@gh-$n:$ORG/$n.git"
  done
  git config --file "$H/.gitconfig" "url.$BASE/utils.git.insteadOf" "https://example.invalid/claude-utils.git"
}

# runboot <args...>: run the script in the fake HOME; sets RC and OUT.
runboot() {
  local SHAARGS=(--sha "$SHA"); [ -z "${NO_SHA:-}" ] || SHAARGS=()
  set +e
  OUT="$(HOME="$H" PATH="${BOOT_PATH:-$STUBS:$PATH}" STUB_DIR="$STUB_DIR" STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="${REAL_RUN:-$REAL}" GIT_CONFIG_NOSYSTEM=1 \
    BOOTSTRAP_APP_API_BASE="http://127.0.0.1:$APIPORT" CLAUDE_UTILS_URL="https://example.invalid/claude-utils.git" \
    "$BASH" "$ROOT/scripts/bootstrap.sh" ${SHAARGS[@]+"${SHAARGS[@]}"} --deny-owner Acme-Client --deny-term acmeclient ${ORGARGS[@]+"${ORGARGS[@]}"} "${REQ[@]}" "$@" </dev/null 2>&1)"
  RC=$?
  set -e
}

has()    { grep -qE -- "$2" "$1"; }
hasnt()  { ! grep -qE -- "$2" "$1"; }
mode()   { [ "$(stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1")" = "$2" ]; }
jsonok() { python3 -I -c "$1" "$2"; }

# --- helpers for the new-flag sections ----------------------------------------------
# minipath <omit...>: a bin dir holding only the tools the script needs (the
# stubs for gh, go and friends, the real rest), minus the ones named, so a
# missing tool is really missing.
MININ=0
MINI_TOOLS="cmp git python3 ssh-keygen ssh-keyscan gh systemctl launchctl claude go sed awk grep tr cut sort head tail find dirname basename cat id uname stat mktemp chmod mkdir ln rm cp mv touch date env wc tee readlink sleep sh bash shred dd tar sha256sum"
minipath() {
  local d="$BASE/mini.$MININ" t p; MININ=$((MININ + 1))
  rm -rf "$d"; mkdir -p "$d"
  for t in $MINI_TOOLS; do
    case " $* " in *" $t "*) continue ;; esac
    if [ -x "$STUBS/$t" ]; then ln -s "$STUBS/$t" "$d/$t"
    elif p="$(type -P "$t")" && [ -n "$p" ]; then ln -s "$p" "$d/$t"; fi
  done
  printf '%s' "$d"
}
mkgit() { # dir remote
  mkdir -p "$1"; git -C "$1" init -q; git -C "$1" remote add origin "$2"
}
untouched() { [ -z "$(find "$H" -mindepth 1 -not -name .gitconfig | head -1)" ] && [ ! -s "$STUB_DIR/gh.log" ]; }
quick_exit() { # after the new-flag sections, when BT_QUICK is set
  [ -n "${BT_QUICK:-}" ] || return 0
  echo; echo "passed=$PASS failed=$FAIL pending=$PENDING (BT_QUICK: stopped early)"; [ "$FAIL" -eq 0 ]; exit $?
}

# ============================ recovery key validation ==========================
echo "--- valid_pq_recipient: no regex repeat count (BSD grep refuses anything above 255)"
eval "$(sed -n '/^valid_pq_recipient() {/,/^}/p' "$ROOT/scripts/bootstrap.sh")"
invalid() { ! valid_pq_recipient "$1"; }
check "the key made by the real binary is real length (over 1,900 characters)" [ "${#PQREC}" -gt 1900 ]
check "a real-length post-quantum key is accepted" valid_pq_recipient "$PQREC"
check "a wrong prefix is refused" invalid "age1xx${PQREC#age1pq1}"
check "a classic age1 key is refused" invalid "age1$(printf 'q%.0s' $(seq 1 58))"
check "a character outside the bech32 set is refused (uppercase)" invalid "${PQREC%?}Q"
check "a character outside the bech32 set is refused (the digit 1, in the middle)" invalid "${PQREC:0:900}1${PQREC:901}"
check "a short key is refused" invalid "${PQREC:0:500}"
check "the bare prefix is refused" invalid "age1pq1"
check "an empty value is refused" invalid ""
check "no script under scripts/ has a regex repeat bound above 255 or a GNU-only flag" bash "$ROOT/scripts/lint-portability.sh"

# ============================ prerequisites ====================================
echo "--- prerequisites: one line per missing tool, exit 1, nothing changed (also in --dry-run)"
newhome pq0
BOOT_PATH="$(minipath)" runboot --host testhost --role homelab --dry-run
[ "$RC" -eq 0 ] || echo "$OUT" | tail -8
check "control: a minimal PATH with every tool passes a dry run" [ "$RC" -eq 0 ]
case "$(uname -s)" in Darwin) SVC=launchctl ;; *) SVC=systemctl ;; esac
for mode in --dry-run --yes; do
  for t in git python3 ssh-keygen ssh-keyscan gh $SVC go; do
    newhome pq
    BOOT_PATH="$(minipath "$t")" runboot --host testhost --role homelab "$mode"
    check "without $t ($mode): exit 1 and a 'missing prerequisite: $t' line" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -qE '^missing prerequisite: $t( |\$)'" _ "$OUT"
    check "without $t ($mode): nothing was changed" untouched
  done
done
newhome pq
BOOT_PATH="$(minipath git python3 ssh-keygen gh)" runboot --host testhost --role homelab --yes
check "four tools missing: four lines, one each, then exit 1" bash -c "[ $RC -eq 1 ] && [ \$(printf '%s\n' \"\$1\" | grep -c '^missing prerequisite: ') -eq 4 ]" _ "$OUT"
newhome pq
BOOT_PATH="$(minipath)" STUB_GOVER=1.24.0 runboot --host testhost --role homelab --yes
check "go older than go.mod: its own line, exit 1, nothing changed" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -qE '^missing prerequisite: go 1\.24 is too old \(need >= 1\.25' " _ "$OUT"
check "go too old: nothing was changed" untouched

# ============================ pause for a missing check key ====================
echo "--- missing check key: exit 3 (paused, needs a human), exact instructions, nothing changed"
for mode in --dry-run --yes; do
  newhome pause
  NOKEY="$H/.config/claude-memsync/memsync-check.pem"
  ORGARGS=()
  runboot --host testhost --role homelab "$mode" --org-app memsync-admin --org-app-id 12345 --org-app-key-cmd "cat $BASE/app.pem" \
    --check-app memsync-check --check-app-id 54321 --check-key-file "$NOKEY"
  ORGARGS=(--org-token-file "$BASE/orgtoken")
  check "missing check key ($mode): exit 3" [ "$RC" -eq 3 ]
  check "missing check key ($mode): the UI path, step by step" bash -c "printf '%s\n' \"\$1\" | grep -qF 'GitHub > Settings > Developer settings > GitHub Apps > memsync-check' && printf '%s\n' \"\$1\" | grep -qF '\"Generate a private key\"'" _ "$OUT"
  check "missing check key ($mode): copy to the path, chmod 600, delete the download, rerun" bash -c "printf '%s\n' \"\$1\" | grep -qF 'copy the downloaded .pem to $NOKEY' && printf '%s\n' \"\$1\" | grep -qF 'chmod 600 $NOKEY' && printf '%s\n' \"\$1\" | grep -qi 'delete the download' && printf '%s\n' \"\$1\" | grep -qi 'rerun the same command'" _ "$OUT"
  check "missing check key ($mode): nothing was changed, no gh call, no apps.json" untouched
done
newhome pause2
ORGARGS=()
runboot --host testhost --role homelab --yes --org-app memsync-admin --org-app-id 12345 --org-app-key-cmd "cat $BASE/app.pem" \
  --check-app memsync-check --check-app-id 54321 --check-key-file "relative/key.pem"
ORGARGS=(--org-token-file "$BASE/orgtoken")
check "a relative --check-key-file is an error (exit 1), not a pause" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -q 'must be an absolute path'" _ "$OUT"


# ============================ --delete-org-token-file ==========================
echo "--- --delete-org-token-file: read, then shredded on every exit path"
TFN=0
mktok() { # sets TF: a fresh 0600 token file holding the dummy org token
  TFN=$((TFN + 1)); TF="$BASE/tokfile.$TFN"; printf '%s\n' "$ORGTOK" >"$TF"; chmod 600 "$TF"
}
tokrun() { # extra args: runboot with TF as the org token file, DEL flag on
  ORGARGS=(--org-token-file "$TF" --delete-org-token-file)
  runboot --host testhost --role homelab "$@"
  ORGARGS=(--org-token-file "$BASE/orgtoken")
}
notoken() { ! printf '%s' "$OUT" | grep -qF -- "$ORGTOK"; }
newhome td1; mktok
tokrun --dry-run
check "token file deleted: a successful dry run exits 0" [ "$RC" -eq 0 ]
check "token file deleted: the file is gone after the dry run" [ ! -e "$TF" ]
check "token file deleted: the token is not in the output" notoken
newhome td2; mktok
tokrun --yes
[ "$RC" -eq 0 ] || echo "$OUT" | tail -8
check "token file deleted: a full run exits 0 and the file is gone" bash -c "[ $RC -eq 0 ] && [ ! -e '$TF' ]"
check "token file deleted: the full run used it (gh calls were made with the org token)" bash -c "grep -q '|org' '$STUB_DIR/gh.who'"
check "token file deleted: the token is not in the output, nor on disk in the fake HOME" bash -c "! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK' && ! grep -rIlF -- '$ORGTOK' '$H' '$STUB_DIR/memsync.log'" _ "$OUT"
newhome td3; mktok
STUB_PUBLIC=1 tokrun --yes
check "token file deleted: gone after a forced failure (public repo)" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'NOT private' && [ ! -e '$TF' ]" _ "$OUT"
check "token file deleted: the token is not in the failing output" notoken
newhome td4; mktok
BOOT_PATH="$(minipath shred)" tokrun --dry-run
check "token file deleted: without shred the overwrite-and-remove fallback still removes it" bash -c "[ $RC -eq 0 ] && [ ! -e '$TF' ]"
newhome td5; mktok; chmod 644 "$TF"
tokrun --dry-run
check "token file with a loose mode is refused, and left alone" bash -c "[ $RC -eq 1 ] && printf '%s' \"\$1\" | grep -q 'must be mode 0600 (is 644)' && [ -e '$TF' ]" _ "$OUT"
check "bad mode: the token is not in the output, nothing was changed" bash -c "! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK'" _ "$OUT"
newhome td6; mktok; ln "$TF" "$TF.link"
tokrun --dry-run
check "token file with two hard links is refused, and left alone" bash -c "[ $RC -eq 1 ] && printf '%s' \"\$1\" | grep -q 'one hard link' && [ -e '$TF' ] && [ -e '$TF.link' ]" _ "$OUT"
newhome td7; mktok; ln -s "$TF" "$TF.sym"
ORGARGS=(--org-token-file "$TF.sym" --delete-org-token-file); runboot --host testhost --role homelab --dry-run; ORGARGS=(--org-token-file "$BASE/orgtoken")
check "token file that is a symlink is refused, and left alone" bash -c "[ $RC -eq 1 ] && printf '%s' \"\$1\" | grep -q 'not a plain file' && [ -e '$TF' ]" _ "$OUT"
newhome td8
ORGARGS=(--org-token-cmd "secret-tool read x" --delete-org-token-file); runboot --host testhost --role homelab --dry-run; ORGARGS=(--org-token-file "$BASE/orgtoken")
check "--delete-org-token-file without --org-token-file is a usage error (exit 2)" bash -c "[ $RC -eq 2 ] && printf '%s' \"\$1\" | grep -q 'only valid with --org-token-file'" _ "$OUT"
check "usage error: the shared test token file was not touched" [ -e "$BASE/orgtoken" ]
newhome td9; mktok
SH_OUT="$BASE/sig.out"; rm -f "$SH_OUT"
(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_HANG=1 STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" GIT_CONFIG_NOSYSTEM=1 \
  BOOTSTRAP_APP_API_BASE="http://127.0.0.1:$APIPORT" CLAUDE_UTILS_URL="https://example.invalid/claude-utils.git" \
  exec "$BASH" "$ROOT/scripts/bootstrap.sh" --sha "$SHA" "${REQ[@]}" --host testhost --role homelab --yes --org-token-file "$TF" --delete-org-token-file </dev/null >"$SH_OUT" 2>&1) &
SIGPID=$!
for _ in $(seq 1 120); do [ -e "$STUB_DIR/hung" ] && break; sleep 0.25; done
kill -TERM "$SIGPID" 2>/dev/null || true
pkill -TERM -P "$SIGPID" 2>/dev/null || true   # the stuck child; the trap runs once it is gone
set +e; wait "$SIGPID"; SIGRC=$?; set -e
check "token file deleted: gone after SIGTERM mid-run (exit 143)" bash -c "[ $SIGRC -eq 143 ] && [ ! -e '$TF' ]"
check "token file deleted: the token is not in the signalled run's output" bash -c "! grep -qF -- '$ORGTOK' '$SH_OUT'"


echo "--- XDG_CONFIG_HOME: a non-default config dir holds routes.json, apps.json and the denylists"
newhome xdg
export XDG_CONFIG_HOME="$H/elsewhere"
mkdir -p "$XDG_CONFIG_HOME/claude-memsync"
cp "$BASE/app.pem" "$XDG_CONFIG_HOME/claude-memsync/app.pem"; chmod 600 "$XDG_CONFIG_HOME/claude-memsync/app.pem"
printf '[{"name":"testapp","appId":"12345","org":"example-org","keyFile":"%s","apiBase":"http://127.0.0.1:%s"}]\n' \
  "$XDG_CONFIG_HOME/claude-memsync/app.pem" "$APIPORT" >"$XDG_CONFIG_HOME/claude-memsync/apps.json"
chmod 600 "$XDG_CONFIG_HOME/claude-memsync/apps.json"
ORGARGS=(--org-app testapp)
runboot --host testhost --role homelab --yes
ORGARGS=(--org-token-file "$BASE/orgtoken")
[ "$RC" -eq 0 ] || echo "$OUT" | tail -8
check "XDG_CONFIG_HOME set: the run reads apps.json from there and exits 0" [ "$RC" -eq 0 ]
check "XDG_CONFIG_HOME set: routes.json and the denylists are written there, 0600" bash -c "[ -f '$XDG_CONFIG_HOME/claude-memsync/routes.json' ] && [ -f '$XDG_CONFIG_HOME/claude-memsync/denylist-personal.txt' ]"
check "XDG_CONFIG_HOME set: nothing was written to \$HOME/.config" bash -c "[ ! -e '$H/.config' ]"
check "XDG_CONFIG_HOME set: the daemon-side configs are consistent with it (the binary found routes.json there)" bash -c "grep -q 'route home infra-homelab' '$STUB_DIR/memsync.log'"
newhome xdg2
export XDG_CONFIG_HOME="$H/elsewhere"
mkdir -p "$XDG_CONFIG_HOME/memsync"; echo '{"repo":"git@x:y/z.git"}' >"$XDG_CONFIG_HOME/memsync/config.json"
runboot --host testhost --role homelab --yes
check "XDG_CONFIG_HOME set: the old memsync config is looked for there too, and refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'old memsync is configured'" _ "$OUT"
newhome xdg3

# ============================ keep-out (was: client) ===========================
echo "--- keep-out flags: new names, old names, wording"
newhome ko0
set +e; OUTNONE="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" "$BASH" "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --org-token-file "$BASE/orgtoken" "${REQ[@]}" --host testhost --role homelab --dry-run </dev/null 2>&1)"; NRC=$?; set -e
check "no keep-out flags: exit 0 and exactly the calm note" bash -c "[ $NRC -eq 0 ] && printf '%s\n' \"\$1\" | grep -qx 'keep-out lists: none (routing alone decides what syncs)'" _ "$OUTNONE"
check "no keep-out flags: nothing reads as a warning or an error" bash -c "! printf '%s\n' \"\$1\" | grep -E 'keep-out|client owners' | grep -qE 'WARNING|ERROR|!!!'" _ "$OUTNONE"
HELPOUT="$("$BASH" "$ROOT/scripts/bootstrap.sh" --help 2>&1 || true)"
check "usage text names --keep-out-owner and --keep-out-term, not client owners" bash -c "printf '%s\n' \"\$1\" | grep -q -- '--keep-out-owner' && printf '%s\n' \"\$1\" | grep -q -- '--keep-out-term' && ! printf '%s\n' \"\$1\" | grep -qi 'client owner'" _ "$HELPOUT"
check "the script says keep-out, not client, except for the mem-client-* repo kind" bash -c "! grep -i client '$ROOT/scripts/bootstrap.sh' | grep -viE 'mem-client|orchestrator'"
for how in old new; do
  newhome "ko-$how"
  if [ "$how" = old ]; then KOF=(--deny-owner Some-Owner --deny-term 'a.b' --deny-term other); else KOF=(--keep-out-owner Some-Owner --keep-out-term 'a.b' --keep-out-term other); fi
  runboot --host testhost --role homelab --yes "${KOF[@]}"
  cp "$H/.config/claude-memsync/routes.json" "$BASE/ko-$how.routes" 2>/dev/null && sed -i "s#$H#HOME#g; s#ko-$how#ko#g" "$BASE/ko-$how.routes" || : >"$BASE/ko-$how.routes.missing"
  for k in personal infra; do cp "$H/.config/claude-memsync/denylist-$k.txt" "$BASE/ko-$how.deny-$k" 2>/dev/null || :; done
done
check "--keep-out-owner/-term and --deny-owner/-term give an identical routes.json" bash -c "[ -s '$BASE/ko-old.routes' ] && cmp -s '$BASE/ko-old.routes' '$BASE/ko-new.routes'"
check "... and identical denylist files (terms only, regex-escaped)" bash -c "cmp -s '$BASE/ko-old.deny-personal' '$BASE/ko-new.deny-personal' && cmp -s '$BASE/ko-old.deny-infra' '$BASE/ko-new.deny-infra' && grep -qxF 'a\\.b' '$BASE/ko-new.deny-personal' && grep -qx other '$BASE/ko-new.deny-personal' && ! grep -qi some-owner '$BASE/ko-new.deny-personal'"
newhome ko-pd
mkdir -p "$H/code"
mkgit "$H/code/keepme" "git@github.com:example-org/keepme.git"
mkgit "$H/code/byowner" "git@github.com:Some-Owner/byowner.git"
mkgit "$H/code/byterm" "git@github.com:example-org/has-secretword-inside.git"
runboot --host testmac --role personal-dev --workspace "$H/code" --dry-run --keep-out-owner some-owner --keep-out-term SecretWord
check "keep-out owner (any case) and term (substring, any case) skip a checkout; others are kept" bash -c "printf '%s\n' \"\$1\" | grep -q 'byowner.*REFUSED (keep-out)' && printf '%s\n' \"\$1\" | grep -q 'byterm.*REFUSED (keep-out)' && printf '%s\n' \"\$1\" | grep -q 'mem-project-keepme'" _ "$OUT"


# ============================ --prebuilt-dir ====================================
echo "--- --prebuilt-dir: install given binaries, no clone, no build, no Go"
PB="$BASE/prebuilt"; rm -rf "$PB"; mkdir -p "$PB/real" "$PB/wrap" "$PB/onlyone" "$PB/noexec" "$PB/dev"
cp "$REAL/claude-memsync" "$REAL/claude-memmerge" "$PB/real/"
cp "$STUBS/memsync-wrap" "$PB/wrap/claude-memsync"; cp "$REAL/claude-memmerge" "$PB/wrap/"
cp "$REAL/claude-memsync" "$PB/onlyone/"
cp "$REAL/claude-memsync" "$REAL/claude-memmerge" "$PB/noexec/"; chmod -x "$PB/noexec/claude-memmerge"
printf '#!/bin/sh\necho "claude-memsync dev+0123456789ab"\n' >"$PB/dev/claude-memsync"; cp "$REAL/claude-memmerge" "$PB/dev/"; chmod +x "$PB/dev/claude-memsync"
PBVER="$("$REAL/claude-memsync" version | sed 's/^claude-memsync //')"
PBSHORT="$(printf '%s' "$PBVER" | sed -nE 's/.*[-+]([0-9a-f]{12})(\+dirty|-dirty)?$/\1/p')"
BSHA=""; [ -z "$PBSHORT" ] || BSHA="$(git -C "$ROOT" rev-parse --verify -q "$PBSHORT^{commit}" || true)"
if [ -z "$BSHA" ]; then
  pending "prebuilt: the test binary's version ($PBVER) carries no commit of this tree"
else
  newhome pb1
  BOOT_PATH="$(minipath go)" runboot --host testhost --role homelab --dry-run --sha "$BSHA" --prebuilt-dir "$PB/real"
  check "prebuilt dry run without Go on PATH exits 0" [ "$RC" -eq 0 ]
  check "prebuilt dry run says it installs from the dir, to the sha dir, and clones nothing" bash -c "printf '%s\n' \"\$1\" | grep -q 'Install prebuilt binaries from $PB/real' && printf '%s\n' \"\$1\" | grep -q '.local/lib/claude-memsync/$BSHA' && ! printf '%s\n' \"\$1\" | grep -q 'git clone'" _ "$OUT"
  check "prebuilt dry run changes nothing" bash -c "[ -z \"\$(find '$H' -mindepth 1 -not -name .gitconfig | head -1)\" ] && ! grep -qE 'repo (create|deploy-key)' '$STUB_DIR/gh.log'"
  newhome pb2
  runboot --host testhost --role homelab --yes --sha "$BSHA" --prebuilt-dir "$PB/wrap"
  [ "$RC" -eq 0 ] || echo "$OUT" | tail -8
  check "prebuilt full run exits 0" [ "$RC" -eq 0 ]
  LIBD="$H/.local/lib/claude-memsync/$BSHA"
  check "prebuilt: both binaries installed in the sha dir, executable" bash -c "[ -x '$LIBD/claude-memsync' ] && [ -x '$LIBD/claude-memmerge' ] && cmp -s '$PB/wrap/claude-memmerge' '$LIBD/claude-memmerge'"
  check "prebuilt: ~/.local/bin links point into that dir" bash -c "[ \"\$(readlink '$H/.local/bin/claude-memsync')\" = '$LIBD/claude-memsync' ] && [ \"\$(readlink '$H/.local/bin/claude-memmerge')\" = '$LIBD/claude-memmerge' ]"
  check "prebuilt: no clone, no go build" bash -c "[ ! -e '$H/.local/src' ] && [ ! -e '$STUB_DIR/go.log' ]"
  check "prebuilt: no clone means no plugin step, and the next-step note says so" bash -c "! grep -qs 'plugin' '$STUB_DIR/claude.log' && printf '%s\n' \"\$1\" | grep -q 'plugin step skipped'" _ "$OUT"
  newhome pb3
  NO_SHA=1 BOOT_PATH="$(minipath go)" runboot --host testhost --role homelab --dry-run --prebuilt-dir "$PB/real"
  check "prebuilt without --sha: a release-shaped version is the dir name; no git checkout needed" bash -c "[ $RC -eq 0 ] && printf '%s\n' \"\$1\" | grep -qF '.local/lib/claude-memsync/$PBVER'" _ "$OUT"
  newhome pb4
  runboot --host testhost --role homelab --yes --sha aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --prebuilt-dir "$PB/real"
  check "prebuilt: a version that does not match --sha is refused, nothing installed" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -q 'does not match --sha' && [ ! -e '$H/.local/lib' ] && [ ! -s '$STUB_DIR/gh.log' ]" _ "$OUT"
fi
newhome pb5
runboot --host testhost --role homelab --yes --prebuilt-dir "$PB/onlyone"
check "prebuilt: a missing claude-memmerge is refused" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -q 'claude-memmerge is missing or not executable'" _ "$OUT"
newhome pb6
runboot --host testhost --role homelab --yes --prebuilt-dir "$PB/noexec"
check "prebuilt: a non-executable binary is refused" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -q 'claude-memmerge is missing or not executable'" _ "$OUT"
newhome pb7
NO_SHA=1 runboot --host testhost --role homelab --yes --prebuilt-dir "$PB/dev"
check "prebuilt without --sha: a dev build is refused (not a release version)" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -q 'not a release version'" _ "$OUT"
newhome pb8
runboot --host testhost --role homelab --yes --prebuilt-dir "$PB/does-not-exist"
check "prebuilt: a directory that does not exist is refused" bash -c "[ $RC -eq 1 ] && printf '%s\n' \"\$1\" | grep -q 'is not a directory'" _ "$OUT"

# ============== the paste prompts in docs/bootstrap.md, as written =============
# Each prompt is taken from the doc (<!-- prompt: id --> then its ```text
# fence): the INPUTS names get test values, the ALREADY SET lines run as written,
# and the boot() function plus its "boot --dry-run" / "boot --yes" lines run as a
# real shell snippet with `set -u`, with the prompt's own step 1 (the clone) done
# by hand. A flag the script does not know fails the run (exit 2); an input the
# steps do not use, or a name the steps use but INPUTS does not list, fails a
# check below. The doc and this test cannot drift apart.
ssh-keygen -q -t rsa -b 2048 -m PEM -N '' -f "$BASE/check" && mv "$BASE/check" "$BASE/check.pem" && chmod 600 "$BASE/check.pem"
ADMIN_MARK="$(sed -n 2p "$BASE/app.pem")"
docprompt() { # id: the lines of that prompt's fenced block
  awk -v id="$1" '$0 == "<!-- prompt: " id " -->" {f=1; next} f==1 && /^```text/ {f=2; next} f==2 && /^```/ {exit} f==2 {print}' "$ROOT/docs/bootstrap.md"
}
docsec() { # id start-regex end-regex: the "NAME = value" bullets of a section
  docprompt "$1" | awk -v s="$2" -v e="$3" '$0 ~ s {f=1; next} $0 ~ e {f=0} f && /^- [A-Z_]+ = / {sub(/^- /, ""); print}'
}
docfunc() { docprompt "$1" | awk '/^boot\(\) \{/ {f=1} f {print} f && /^\}/ {exit}'; }
docsteps() { docprompt "$1" | awk '/^STEPS/ {f=1} f'; }
docvalue() { # input name -> test value (the keep-out and workspace lists come from DOC_OWNERS, DOC_TERMS, DOC_WS)
  case "$1" in
    COMMIT) printf '%s' "$SHA" ;; HOST_LABEL) printf testhost ;; ORG) printf '%s' "$ORG" ;; RECOVERY) printf '%s' "$PQREC" ;;
    GITHUB_USER) printf example-user ;; WORKSPACES) printf '%s' "${DOC_WS:-}" ;;
    KEEP_OUT_OWNERS) printf '%s' "${DOC_OWNERS:-}" ;; KEEP_OUT_TERMS) printf '%s' "${DOC_TERMS:-}" ;;
    ADMIN_APP_ID) printf 12345 ;; ADMIN_KEY_CMD) printf 'cat %s' "$BASE/app.pem" ;;
    CHECK_APP_ID) printf 54321 ;; CHECK_KEY_FILE) printf '%s' "$H/.config/claude-memsync/memsync-check.pem" ;;
    TOKEN_FILE) printf '%s' "${TF:-/nonexistent-token}" ;; TAG) printf v0.0.0-doc ;; PLATFORM) printf linux-amd64 ;;
    *) printf 'NO-TEST-VALUE-FOR-%s' "$1" ;;
  esac
}
docsnippet() { # id mode(dry|real): the prompt as a shell snippet
  local line n
  printf 'set -u\n'
  while IFS= read -r line; do
    n="${line%% = *}"
    if [ -n "${DOC_LITERAL:-}" ] && case "$n" in KEEP_OUT_*|WORKSPACES) true ;; *) false ;; esac; then printf '%s=%s\n' "$n" "${line#* = }"   # the doc's own default
    else printf '%s=%q\n' "$n" "$(docvalue "$n")"; fi
  done < <(docsec "$1" '^INPUTS' '^ALREADY SET')
  while IFS= read -r line; do printf '%s=%s\n' "${line%% = *}" "${line#* = }"; done < <(docsec "$1" '^ALREADY SET' '^STEPS')
  docfunc "$1"
  if [ "$2" = dry ]; then docprompt "$1" | grep -x 'boot --dry-run'; else docprompt "$1" | grep -x 'boot --yes'; fi
}
doc_unused() { # id: every INPUTS name is used (as $NAME or ${NAME}) in the steps or in an ALREADY SET derivation
  local n text
  text="$( { docsec "$1" '^ALREADY SET' '^STEPS'; docsteps "$1"; } )"   # a variable, not a pipe: grep -q under pipefail can SIGPIPE its writer
  for n in $(docsec "$1" '^INPUTS' '^ALREADY SET' | sed 's/ = .*//'); do
    grep -qE "\\\$\{?$n([^A-Za-z0-9_]|\$)" <<<"$text" || { echo "unused input $n"; return 1; }
  done
}
doc_undefined() { # id: every $NAME in the steps is defined by INPUTS or ALREADY SET
  local def n used
  def="$( { docsec "$1" '^INPUTS' '^ALREADY SET'; docsec "$1" '^ALREADY SET' '^STEPS'; } | sed 's/ = .*//')"
  used="$(docsteps "$1" | grep -o '\$[A-Z][A-Z_]*' | tr -d '$' | sort -u)"
  for n in $used; do
    grep -qx "$n" <<<"$def" || { echo "undefined $n"; return 1; }
  done
}
DOC_IDS=(homelab-app personal-dev-app homelab-tokenfile crewkit)
docnum() { case "$1" in 1) echo "homelab-app dry" ;; 2) echo "homelab-app real" ;; 3) echo "personal-dev-app dry" ;; 4) echo "personal-dev-app real" ;; esac; }
rundoc() { # n | "id mode": run that prompt in the fake HOME; sets RC and OUT
  local spec snip
  case "$1" in [0-9]) spec="$(docnum "$1")" ;; *) spec="$1" ;; esac
  # shellcheck disable=SC2086
  snip="$(docsnippet $spec || true)"
  [ -n "$snip" ] && printf '%s\n' "$snip" | grep -q '^boot --' || { RC=99; OUT="prompt $spec not found in docs/bootstrap.md"; return; }
  set +e
  OUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" GIT_CONFIG_NOSYSTEM=1 \
    BOOTSTRAP_APP_API_BASE="http://127.0.0.1:$APIPORT" CLAUDE_UTILS_URL="https://example.invalid/claude-utils.git" \
    bash -c "$snip" </dev/null 2>&1)"
  RC=$?
  set -e
}
prompt_setup() { # step 1 of the prompt (the clone) and the host's own check key
  mkdir -p "$H/.local/src" "$H/.config/claude-memsync"
  git clone -q "$BASE/utils.git" "$H/.local/src/claude-utils-$SHA" && git -C "$H/.local/src/claude-utils-$SHA" checkout -q "$SHA"
  cp "$BASE/check.pem" "$H/.config/claude-memsync/memsync-check.pem"; chmod 600 "$H/.config/claude-memsync/memsync-check.pem"
}

echo "--- paste prompts: structure, and the doc against the script's flags"
for id in "${DOC_IDS[@]}"; do
  check "$id: prompt found, with INPUTS, ALREADY SET, STEPS and a boot() function" bash -c "[ -n \"\$1\" ] && printf '%s\n' \"\$2\" | grep -q '^boot() {'" _ "$(docsec "$id" '^INPUTS' '^ALREADY SET')" "$(docfunc "$id")"
  check "$id: the real run line is 'boot --yes' and the dry run line is 'boot --dry-run'" bash -c "printf '%s\n' \"\$1\" | grep -qx 'boot --dry-run' && printf '%s\n' \"\$1\" | grep -qx 'boot --yes'" _ "$(docprompt "$id")"
  check "$id: every input is used by the steps" doc_unused "$id"
  check "$id: the steps use only names that INPUTS or ALREADY SET define" doc_undefined "$id"
  check "$id: every flag in boot() is one the script knows" bash -c "
    for f in \$(printf '%s\n' \"\$1\" | grep -o -- '--[a-z][a-z-]*' | sort -u); do grep -qE -- \"(^|[ |])\$f[ |)]\" '$ROOT/scripts/bootstrap.sh' || { echo \"unknown flag \$f\"; exit 1; }; done" _ "$(docfunc "$id")"
  check "$id: both keep-out lists default to empty and are marked optional" bash -c "printf '%s\n' \"\$1\" | grep -qx -- '- KEEP_OUT_OWNERS = \"\"' && printf '%s\n' \"\$1\" | grep -qx -- '- KEEP_OUT_TERMS = \"\"' && [ \$(printf '%s\n' \"\$1\" | grep -c '(optional; leave empty for none)') -ge 2 ]" _ "$(docprompt "$id")"
  check "$id: the recovery or other long value is not written inline twice" bash -c "[ \$(printf '%s\n' \"\$1\" | grep -c 'age1pq1') -le 1 ]" _ "$(docprompt "$id")"
done
check "the doc tells the reader to use bash, not zsh" grep -q 'bash, not zsh' "$ROOT/docs/bootstrap.md"
check "a flag the script does not know fails the run (the test would notice drift)" bash -c "
  s=\$(printf 'set -u\nboot() { bash \"$ROOT/scripts/bootstrap.sh\" --no-such-flag; }\nboot\n'); out=\$(bash -c \"\$s\" 2>&1); [ \$? -eq 2 ] && printf '%s' \"\$out\" | grep -q 'unknown argument'"

for P in "homelab-app:1:2:personal infra-homelab" "personal-dev-app:3:4:personal"; do
  IFS=: read -r ROLEN DRYN REALN KEYSN <<<"$P"
  echo "--- paste prompt: $ROLEN"
  newhome "doc-$ROLEN"
  prompt_setup
  rundoc "$DRYN"
  [ "$RC" -eq 0 ] || echo "$OUT" | tail -20
  check "$ROLEN prompt: dry run succeeds and shows the plan" bash -c "[ $RC -eq 0 ] && printf '%s' \"\$1\" | grep -q '== Plan' && printf '%s' \"\$1\" | grep -q 'would add the app entry memsync-admin' && printf '%s' \"\$1\" | grep -q 'githubApp=memsync-check'" _ "$OUT"
  check "$ROLEN prompt: empty keep-out lists give the calm note" bash -c "printf '%s\n' \"\$1\" | grep -qx 'keep-out lists: none (routing alone decides what syncs)'" _ "$OUT"
  check "$ROLEN prompt: dry run changes nothing" bash -c "[ ! -e '$H/.config/claude-memsync/apps.json' ] && [ ! -e '$H/.ssh' ] && ! grep -qE 'repo (create|deploy-key)' '$STUB_DIR/gh.log' && [ ! -e '$H/.local/bin/claude-memsync' ]"
  rundoc "$REALN"
  [ "$RC" -eq 0 ] || echo "$OUT" | tail -30
  check "$ROLEN prompt: real run succeeds" [ "$RC" -eq 0 ]
  check "$ROLEN prompt: admin entry gone, check entry kept (key file, fingerprint, no key command), apps.json 0600" jsonok '
import json,sys
a=json.load(open(sys.argv[1]))
assert [x["name"] for x in a]==["memsync-check"], a
e=a[0]
assert e["appId"]=="54321" and e["org"]=="example-org" and e["keyFile"].endswith("/memsync-check.pem") and "keyCmd" not in e
assert e["keyFingerprint"].startswith("SHA256:") and e["apiBase"].startswith("http://127.0.0.1:")' "$H/.config/claude-memsync/apps.json"
  check "$ROLEN prompt: apps.json mode 0600" mode "$H/.config/claude-memsync/apps.json" 600
  check "$ROLEN prompt: recorded fingerprint is the check key's" bash -c "[ \"\$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[0][\"keyFingerprint\"])' '$H/.config/claude-memsync/apps.json')\" = \"\$('$REAL/claude-memsync' app key-fingerprint --key-file '$BASE/check.pem')\" ]"
  for k in $KEYSN; do
    check "$ROLEN prompt: $k config re-checks through the check app, no PAT file or token command" jsonok '
import json,sys
d=json.load(open(sys.argv[1]))
assert d["githubApp"]=="memsync-check" and d["recheckTokenCmd"] is False
assert not any(x in d for x in ("githubTokenFile","githubTokenCmd"))' "$H/.claudesync.d/$k/config.json"
  done
  check "$ROLEN prompt: the admin app minted the org token (key command, per call)" bash -c "[ \$(grep -c 'memsync app-token --app memsync-admin' '$STUB_DIR/memsync.log') -ge 4 ] && grep -qE 'doctor --org example-org --token-cmd .*app-token --app memsync-admin' '$STUB_DIR/memsync.log'"
  check "$ROLEN prompt: every org call used the org token" bash -c "grep -qE '^(repo|api repos)' '$STUB_DIR/gh.who' && ! grep -E '^(repo|api repos)' '$STUB_DIR/gh.who' | grep -v '|org'"
  check "$ROLEN prompt: admin key and org token nowhere on disk, in logs or output" bash -c "! grep -rIlF -- '$ADMIN_MARK' '$H' '$STUB_DIR/memsync.log' '$STUB_DIR/gh.log' && ! printf '%s' \"\$1\" | grep -qF -- '$ADMIN_MARK' && ! grep -rIl -- '$ORGTOK' '$H' '$STUB_DIR/memsync.log' && ! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK' && ! printf '%s' \"\$1\" | grep -q 'BEGIN '" _ "$OUT"
  check "$ROLEN prompt: no daemon-read config names the admin app" bash -c "! grep -rIl 'memsync-admin' '$H/.claudesync.d'"
done

echo "--- paste prompt: the two-item keep-out lists expand into repeated flags"
newhome doc-ko
prompt_setup
DOC_OWNERS="Acme-Client other-owner" DOC_TERMS="acmeclient other.term" rundoc 4
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "two-item lists: the real run succeeds" [ "$RC" -eq 0 ]
DL="$H/.config/claude-memsync/denylist-personal.txt"
check "two-item lists: both terms are in the denylist (regex-escaped)" bash -c "grep -qx acmeclient '$DL' && grep -qxF 'other\\.term' '$DL'"
check "two-item lists: no keep-out owner is in the denylist" bash -c "! grep -qiE 'acme-client|other-owner' '$DL'"
check "two-item lists: no keep-out note about empty lists" bash -c "! printf '%s\n' \"\$1\" | grep -q 'keep-out lists: none'" _ "$OUT"
newhome doc-ws
prompt_setup
mkdir -p "$H/code"
DOC_WS="$H/code" DOC_LITERAL='' rundoc 3
check "WORKSPACES expands into --workspace (dry run, personal-dev)" bash -c "[ $RC -eq 0 ] && printf '%s\n' \"\$1\" | grep -q 'workspace $H/code -> mem-project-code-workspace'" _ "$OUT"
newhome doc-lit
prompt_setup
DOC_LITERAL=1 rundoc 2
check "the doc's own empty defaults (KEEP_OUT_OWNERS=\"\", KEEP_OUT_TERMS=\"\") run as written" bash -c "[ $RC -eq 0 ] || exit 1; printf '%s\n' \"\$1\" | grep -qx 'keep-out lists: none (routing alone decides what syncs)'" _ "$OUT"

echo "--- paste prompt: homelab with an org token file"
newhome doc-tf
prompt_setup
mktok; rundoc homelab-tokenfile\ dry
check "token-file prompt: dry run succeeds and consumes the token file" bash -c "[ $RC -eq 0 ] && [ ! -e '$TF' ]" 
mktok; rundoc homelab-tokenfile\ real
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "token-file prompt: real run succeeds, the file is gone" bash -c "[ $RC -eq 0 ] && [ ! -e '$TF' ]"
check "token-file prompt: org calls used the token, and it is nowhere in the output or on disk" bash -c "grep -qE '^(repo|api repos)' '$STUB_DIR/gh.who' && ! grep -E '^(repo|api repos)' '$STUB_DIR/gh.who' | grep -v '|org' && ! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK' && ! grep -rIlF -- '$ORGTOK' '$H'" _ "$OUT"
check "token-file prompt: init and doctor were handed a token file (never the token)" has "$STUB_DIR/memsync.log" 'doctor --org example-org --token-file '
newhome doc-tf2
prompt_setup
rm -f "$H/.config/claude-memsync/memsync-check.pem"
mktok; rundoc homelab-tokenfile\ real
check "token-file prompt: without the check key it pauses (exit 3) and the token file is still deleted" bash -c "[ $RC -eq 3 ] && [ ! -e '$TF' ]"

echo "--- paste prompt: crewkit (release archive, --prebuilt-dir)"
newhome doc-ck
prompt_setup
CKX="$(bash -c "set -u; HOME='$H'; TAG=v0.0.0-doc; PLATFORM=linux-amd64; $(docsec crewkit '^ALREADY SET' '^STEPS' | sed 's/ = /=/'); printf '%s' \"\$X\"")"
mkdir -p "$CKX/scripts"; cp "$ROOT/scripts/bootstrap.sh" "$CKX/scripts/"; cp "$PB/wrap/claude-memsync" "$PB/wrap/claude-memmerge" "$CKX/"
rm -rf "$H/.local/src/claude-utils-$SHA"
mktok; rundoc crewkit\ dry
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "crewkit prompt: dry run succeeds with no Go, no clone" bash -c "[ $RC -eq 0 ] && printf '%s\n' \"\$1\" | grep -q 'Install prebuilt binaries from'" _ "$OUT"
mktok; rundoc crewkit\ real
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "crewkit prompt: real run succeeds and installs the extracted binaries into the version dir" bash -c "[ $RC -eq 0 ] && [ -x '$H/.local/bin/claude-memsync' ] && readlink '$H/.local/bin/claude-memsync' | grep -q '/lib/claude-memsync/v' && [ ! -e '$H/.local/src/claude-utils-$SHA' ]"
check "crewkit prompt: no go build, token file deleted" bash -c "[ ! -e '$STUB_DIR/go.log' ] && [ ! -e '$TF' ]"

quick_exit

# ============================ homelab ==========================================
echo "--- homelab run"
newhome homelab
runboot --host testhost --role homelab --yes
[ "$RC" -eq 0 ] || { echo "$OUT" | tail -20; }
check "homelab run exits 0" [ "$RC" -eq 0 ]
check "private repo created with --private" has "$STUB_DIR/gh.log" 'repo create example-org/mem-infra-homelab --private'
check "existing mem-personal not re-created" hasnt "$STUB_DIR/gh.log" 'repo create example-org/mem-personal'
check "deploy key added with host title" has "$STUB_DIR/gh.log" 'deploy-key add .*mem-infra-homelab_ed25519.pub -R example-org/mem-infra-homelab -w -t testhost'
check "private key 0600 (personal)" mode "$H/.ssh/mem-personal_ed25519" 600
check "private key 0600 (infra)" mode "$H/.ssh/mem-infra-homelab_ed25519" 600
check "ssh alias on 443 with IdentitiesOnly" bash -c "grep -q 'Host gh-mem-personal' '$H/.ssh/config' && grep -q 'Port 443' '$H/.ssh/config' && grep -q 'IdentitiesOnly yes' '$H/.ssh/config'"
check "github host keys added" bash -c "grep -q '^github.com ssh-ed25519' '$H/.ssh/known_hosts' && grep -q '^\[ssh.github.com\]:443 ssh-ed25519' '$H/.ssh/known_hosts'"
check "routes.json 0600" mode "$H/.config/claude-memsync/routes.json" 600
check "routes.json content" jsonok '
import json,sys
d=json.load(open(sys.argv[1]))
assert d["default"]=="deny" and d["personal"]=="personal"
assert d["repos"]["personal"]["kind"]=="personal" and d["repos"]["infra-homelab"]["kind"]=="infra"
fw=json.dumps(d["foreignWriters"])
for s in ("jsonKeyNonEmpty","memsync.timer","memsync.service","memsync/bin/memsync","memsync@memsync","onlyIfFile"): assert s in fw, s
assert d["routes"][0]["repo"]=="infra-homelab"' "$H/.config/claude-memsync/routes.json"
check "denylists contain the deny term" bash -c "grep -qix acmeclient '$H/.config/claude-memsync/denylist-personal.txt' && grep -qix acmeclient '$H/.config/claude-memsync/denylist-infra.txt'"
check "init has age recovery recipient and private-only" has "$STUB_DIR/memsync.log" "init .*--recovery-recipient $PQREC.*--private-only"
check "home pinned to infra-homelab" has "$STUB_DIR/memsync.log" 'route home infra-homelab --yes'
check "claude plugin calls" bash -c "grep -q 'plugin marketplace add .*claude-utils-$SHA' '$STUB_DIR/claude.log' && grep -q 'plugin install memory-notices@claude-utils' '$STUB_DIR/claude.log'"
check "service install/start/status/doctor" bash -c "grep -q 'memsync install' '$STUB_DIR/memsync.log' && grep -q 'memsync start' '$STUB_DIR/memsync.log' && grep -q 'doctor --org example-org' '$STUB_DIR/memsync.log'"
check "binaries symlinked into ~/.local/bin" [ -L "$H/.local/bin/claude-memsync" ]
check "no client/orch names anywhere" bash -c "! grep -rIiE 'mem-(client|orch)|orch-' '$H/.config' '$H/.ssh/config' '$STUB_DIR/gh.log' '$STUB_DIR/memsync.log'"
check "dummy token never on disk" bash -c "! grep -rIl -- '$TOKEN' '$H' '$STUB_DIR/gh.log' '$STUB_DIR/memsync.log' '$STUB_DIR/claude.log'"
check "output has no private key material" bash -c "! printf '%s' \"\$1\" | grep -q 'BEGIN OPENSSH'" _ "$OUT"
check "join request issued for existing repo" has "$STUB_DIR/memsync.log" 'join request --repo personal --label testhost'
check "init passes the org token file" has "$STUB_DIR/memsync.log" 'init .*--token-file .*/orgtoken'
check "join accept one-liner printed with the grouped fingerprint" bash -c "printf '%s' \"\$1\" | grep -qE 'claude-memsync join accept testhost --repo personal --fingerprint \"([0-9a-f]{4} ){7}[0-9a-f]{4}\"'" _ "$OUT"
check "real init made an encrypted, keyless clone and the join request is in the remote" bash -c "[ -f '$H/.claudesync.d/personal/.age-recipients' ] && git -C '$BASE/remotes/mem-personal.git' ls-tree -r main --name-only | grep -q '^joins/testhost'"
check "init gets the host label and a marked recovery recipient" has "$STUB_DIR/memsync.log" "init .*--host-label testhost --recovery-recipient $PQREC --archive-deletes"
check "first host of a new repo is labelled in its recipients and config" bash -c "grep -q '# host:testhost' '$H/.claudesync.d/infra-homelab/.age-recipients' && grep -q '# recovery' '$H/.claudesync.d/infra-homelab/.age-recipients' && grep -q '$PQREC' '$H/.claudesync.d/infra-homelab/config.json' && grep -q '\"hostLabel\": \"testhost\"' '$H/.claudesync.d/infra-homelab/config.json'"
check "the real init did save the token source (so its removal below means something)" has "$STUB_DIR/memsync.log" '^init-saved-token '
check "after a real init with a token file, no daemon-read config keeps githubTokenCmd or githubTokenFile" bash -c "! grep -rIlE 'githubTokenCmd|githubTokenFile' '$H/.claudesync.d'"
check "real route home pinned the home folder" jsonok '
import json,sys
d=json.load(open(sys.argv[1])); assert d["routes"][0]["repo"]=="infra-homelab" and "folder" in d["routes"][0]' "$H/.config/claude-memsync/routes.json"

echo "--- homelab idempotent re-run"
K1="$(ssh-keygen -lf "$H/.ssh/mem-personal_ed25519.pub")"
INITS1="$(grep -c ' init ' "$STUB_DIR/memsync.log")"; CREATES1="$(grep -c 'repo create' "$STUB_DIR/gh.log")"; DK1="$(grep -c deploy-key "$STUB_DIR/gh.log")"
runboot --host testhost --role homelab --yes
check "re-run exits 0" [ "$RC" -eq 0 ]
check "re-run keeps keys" [ "$K1" = "$(ssh-keygen -lf "$H/.ssh/mem-personal_ed25519.pub")" ]
check "re-run does not init again" [ "$INITS1" = "$(grep -c ' init ' "$STUB_DIR/memsync.log")" ]
check "re-run does not create repos again" [ "$CREATES1" = "$(grep -c 'repo create' "$STUB_DIR/gh.log")" ]
check "re-run leaves routes.json alone (no .bak)" bash -c "[ ! -e '$H/.config/claude-memsync/routes.json.bak' ]"
check "re-run does not add deploy keys again" [ "$DK1" = "$(grep -c deploy-key "$STUB_DIR/gh.log")" ]

echo "--- dry run"
newhome dry
runboot --host testhost --role homelab --dry-run
check "dry run exits 0" [ "$RC" -eq 0 ]
check "dry run creates nothing" bash -c "[ -z \"\$(find '$H' -mindepth 1 -not -name .gitconfig | head -1)\" ]"
check "dry run makes no repo or key calls" hasnt "$STUB_DIR/gh.log" 'repo (create|deploy-key)'

echo "--- org scope"
newhome orgs
STUB_ORGS="$(printf 'example-org\nAcme-Client\nSomeOther')" runboot --host testhost --role homelab --yes
check "extra orgs: warns and proceeds" [ "$RC" -eq 0 ]
check "banner names every extra org and the client org" bash -c "printf '%s' \"\$1\" | grep -q \"WARNING: this host's gh login can reach Acme-Client, SomeOther. An agent host holds an owner-level token with reach beyond the memory repos.\" && printf '%s' \"\$1\" | grep -q 'KEEP-OUT ORG(S) reachable through the gh login: Acme-Client'" _ "$OUT"
check "banner in plan and again in summary" bash -c "[ \$(printf '%s' \"\$1\" | grep -c \"WARNING: this host's gh login can reach\") -ge 3 ]" _ "$OUT"
check "summary repeats the warning" bash -c "printf '%s' \"\$1\" | sed -n '/== j. Summary/,\$p' | grep -q 'WARNING: this host'" _ "$OUT"
check "zero gh calls against any other org" bash -c "! grep -v 'user/orgs' '$STUB_DIR/gh.log' | grep -iE 'acme-client|someother'; [ \$(grep -E 'repo|api repos' '$STUB_DIR/gh.log' | grep -vc 'example-org') -eq 0 ]"
newhome orgs2
STUB_ORGS="$(printf 'example-org\nSomeOther')" runboot --host testhost --role homelab --yes --allow-org someother
check "--allow-org accepts an extra org" [ "$RC" -eq 0 ]
check "the script holds no built-in allowed org or own owner: only --org (and flags) feed the lists" bash -c "! grep -nE '(ALLOW_ORGS|OWN_OWNERS)\\+?=\\([a-z-]' '$ROOT/scripts/bootstrap.sh'"

echo "--- token split"
newhome tok1
runboot --host testhost --role homelab --yes
check "no PAT file: recheckTokenCmd false, no githubTokenFile" jsonok '
import json,sys
d=json.load(open(sys.argv[1])); assert d["recheckTokenCmd"] is False and "githubTokenFile" not in d' "$H/.claudesync.d/personal/config.json"
check "no PAT file: warning printed, file not created" bash -c "printf '%s' \"\$1\" | grep -q 'hourly private re-check is OFF' && [ ! -e '$H/.config/claude-memsync/github-token' ]" _ "$OUT"
check "gh token not in any config" bash -c "! grep -rIl -- '$TOKEN' '$H/.claudesync.d' '$H/.config'"
newhome tok2
mkdir -p "$H/.config/claude-memsync"; printf 'pat-not-the-gh-token\n' >"$H/.config/claude-memsync/github-token"; chmod 600 "$H/.config/claude-memsync/github-token"
runboot --host testhost --role homelab --yes
check "PAT file: githubTokenFile set, recheckTokenCmd false" jsonok '
import json,sys,os
d=json.load(open(sys.argv[1])); assert d["recheckTokenCmd"] is False and d["githubTokenFile"].endswith("/.config/claude-memsync/github-token")' "$H/.claudesync.d/infra-homelab/config.json"
check "no org token file/command saved in a daemon-read config" bash -c "! grep -rIlE 'githubTokenCmd|orgtoken' '$H/.claudesync.d'"
check "PAT file untouched" bash -c "[ \"\$(cat '$H/.config/claude-memsync/github-token')\" = pat-not-the-gh-token ]"
check "PAT case: gh token not in any config" bash -c "! grep -rIl -- '$TOKEN' '$H/.claudesync.d' '$H/.config'"

echo "--- org token"
newhome ot1
STUB_ORGS="$(printf 'example-org\nAcme-Client')" runboot --host testhost --role homelab --yes
check "ambient login sees client org: its own banner line" bash -c "printf '%s' \"\$1\" | grep -q \"WARNING: this host's gh login can reach Acme-Client\"" _ "$OUT"
check "org token cannot reach it: no org-token banner" bash -c "! printf '%s' \"\$1\" | grep -q \"this host's org token can reach\"" _ "$OUT"
check "ambient banner repeated in summary" bash -c "printf '%s' \"\$1\" | sed -n '/== j. Summary/,\$p' | grep -q \"gh login can reach Acme-Client\"" _ "$OUT"
check "every org operation used the org token" bash -c "! grep -E '^(repo|api repos)' '$STUB_DIR/gh.who' | grep -v '|org'"
check "ambient login used only for auth status and its own org listing" bash -c "[ \$(grep -c '|ambient' '$STUB_DIR/gh.who') -le 2 ] && ! grep '|ambient' '$STUB_DIR/gh.who' | grep -E '^(repo|api repos)'"
check "memsync got --token-file, not a token" has "$STUB_DIR/memsync.log" 'doctor --org example-org --token-file .*/orgtoken'
check "org token not in any file, log or output" bash -c "! grep -rIl -- '$ORGTOK' '$H' '$STUB_DIR/gh.log' '$STUB_DIR/gh.who' '$STUB_DIR/memsync.log' '$STUB_DIR/claude.log' && ! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK'" _ "$OUT"
check "org token absent from every non-gh process env" bash -c "[ \$(grep -v ':0\$' '$STUB_DIR/env.log' | wc -l) -eq 0 ]"

newhome ot2
STUB_ORGS="$(printf 'example-org\nAcme-Client')" STUB_ORG_ORGS="$(printf 'example-org\nSomeOther')" runboot --host testhost --role homelab --yes
check "two distinct warnings" bash -c "printf '%s' \"\$1\" | grep -q \"gh login can reach Acme-Client\" && printf '%s' \"\$1\" | grep -q \"org token can reach SomeOther\"" _ "$OUT"
check "summary repeats both" bash -c "printf '%s' \"\$1\" | sed -n '/== j. Summary/,\$p' | grep -c 'WARNING: this host' | grep -qx 2" _ "$OUT"
check "zero gh calls against any other org" bash -c "! grep -v 'user/orgs' '$STUB_DIR/gh.log' | grep -iE 'acme-client|someother'"

newhome ot3
ORGARGS=(--org-token-cmd "secret-tool read vault://vault/item/field")
runboot --host testhost --role homelab --yes
check "cmd variant runs" [ "$RC" -eq 0 ]
check "cmd variant: secret-tool run without a shell, memsync gets --token-cmd" bash -c "grep -q 'secret-tool read vault://vault/item/field' '$STUB_DIR/tool.log' && grep -q 'doctor --org example-org --token-cmd secret-tool read vault://vault/item/field' '$STUB_DIR/memsync.log'"
check "cmd variant: org token not on disk, output or other envs" bash -c "! grep -rIl -- '$ORGTOK' '$H' '$STUB_DIR/gh.log' '$STUB_DIR/memsync.log' && ! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK' && [ \$(grep -v ':0\$' '$STUB_DIR/env.log' | wc -l) -eq 0 ]" _ "$OUT"
check "cmd variant: org calls used the token" bash -c "! grep -E '^(repo|api repos)' '$STUB_DIR/gh.who' | grep -v '|org'"
ORGARGS=()
runboot --host testhost --role homelab --yes
check "org token flag required" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'org-token-file'" _ "$OUT"
check "cmd variant: githubTokenCmd removed from configs, recheckTokenCmd false" bash -c "! grep -rIl githubTokenCmd '$H/.claudesync.d' && grep -q '\"recheckTokenCmd\": false' '$H/.claudesync.d/personal/config.json'"
ORGARGS=(--org-token-file "$BASE/orgtoken")
newhome nofp
STUB_NOFP=1 runboot --host testhost --role homelab --yes
check "missing fingerprint line refuses" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'printed no .fingerprint'" _ "$OUT"
newhome oldfp
STUB_OLDFP=1 runboot --host testhost --role homelab --yes
check "ungrouped 16-hex fingerprint refuses" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'printed no .fingerprint'" _ "$OUT"

echo "--- org app"
appsetup() { # apps.json + key in the fake HOME; the app's apiBase is the loopback fake
  mkdir -p "$H/.config/claude-memsync"
  cp "$BASE/app.pem" "$H/.config/claude-memsync/app.pem"; chmod 600 "$H/.config/claude-memsync/app.pem"
  printf '[{"name":"testapp","appId":"12345","org":"example-org","keyFile":"%s","apiBase":"http://127.0.0.1:%s"}]\n' \
    "$H/.config/claude-memsync/app.pem" "$APIPORT" >"$H/.config/claude-memsync/apps.json"
  chmod 600 "$H/.config/claude-memsync/apps.json"
}
newhome oa1
appsetup
: >"$BASE/fakeapi.log"
ORGARGS=(--org-app testapp)
STUB_ORGS="$(printf 'example-org\nAcme-Client')" runboot --host testhost --role homelab --yes
[ "$RC" -eq 0 ] || { echo "$OUT" | tail -20; }
check "app variant runs" [ "$RC" -eq 0 ]
check "app variant: app-token runs per org call, via the real binary" bash -c "[ \$(grep -c 'memsync app-token --app testapp' '$STUB_DIR/memsync.log') -ge 4 ] && [ \$(grep -c 'POST /app/installations/7/access_tokens' '$BASE/fakeapi.log') -ge 4 ]"
check "app variant: every org operation used the app's token" bash -c "grep -qE '^repo create' '$STUB_DIR/gh.who' && ! grep -E '^(repo|api repos)' '$STUB_DIR/gh.who' | grep -v '|org'"
check "app variant: ambient banner, org-token banner computed after the build" bash -c "printf '%s' \"\$1\" | grep -q \"gh login can reach Acme-Client\" && printf '%s' \"\$1\" | grep -q 'b2. Org token from app testapp'" _ "$OUT"
check "app variant: init and doctor get --token-cmd with the app-token command" bash -c "grep -qE 'init .*--token-cmd .*/claude-memsync app-token --app testapp' '$STUB_DIR/memsync.log' && grep -qE 'doctor --org example-org --token-cmd .*/claude-memsync app-token --app testapp' '$STUB_DIR/memsync.log'"
check "app variant: org token not on disk, output or other envs" bash -c "! grep -rIl -- '$ORGTOK' '$H' '$STUB_DIR/gh.log' '$STUB_DIR/gh.who' '$STUB_DIR/memsync.log' && ! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK' && [ \$(grep -v ':0\$' '$STUB_DIR/env.log' | wc -l) -eq 0 ]" _ "$OUT"
check "app variant: no token command left in a daemon-read config" bash -c "! grep -rIlE 'githubTokenCmd|app-token' '$H/.claudesync.d'"
newhome oa5
appsetup
STUB_INSTALL_OWNERS="$(printf 'example-org\nAcme-Client')" runboot --host testhost --role homelab --yes --org-app testapp
check "app reach: owners of the installation's repositories, not /user/orgs, and it warns" bash -c "grep -q '^api installation/repositories .*|org' '$STUB_DIR/gh.who' && ! grep 'user/orgs' '$STUB_DIR/gh.who' | grep -q '|org' && printf '%s' \"\$1\" | grep -q \"org token can reach Acme-Client\"" _ "$OUT"
newhome oa2
appsetup
ORGARGS=(--org-app testapp)
runboot --host testhost --role homelab --dry-run
check "app dry run: exits 0, would mint, mints nothing" bash -c "[ $RC -eq 0 ] && printf '%s' \"\$1\" | grep -q 'would mint the org token with: claude-memsync app-token --app testapp' && [ ! -e '$H/.local/bin/claude-memsync' ]" _ "$OUT"
newhome oa3
ORGARGS=(--org-app testapp --org-token-file "$BASE/orgtoken")
runboot --host testhost --role homelab --yes
check "app plus token file refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'only one of'" _ "$OUT"
newhome oa4
ORGARGS=(--org-app testapp)
runboot --host testhost --role homelab --yes
check "app without apps.json stops before any repo or key is made" bash -c "[ $RC -ne 0 ] && ! grep -q 'repo create\|deploy-key' '$STUB_DIR/gh.log' && [ ! -e '$H/.ssh/mem-personal_ed25519' ]"
ORGARGS=(--org-token-file "$BASE/orgtoken")
ORGARGS=(--org-token-file "$BASE/does-not-exist")
runboot --host testhost --role homelab --yes
check "unreadable org token stops clearly" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'cannot read the org token'" _ "$OUT"
ORGARGS=(--org-token-file "$BASE/orgtoken")

echo "--- host label, org, xtrace, denylist, quoting"
newhome hl
mid() { # run with the given extra args but without REQ
  set +e
  OUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" bash "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --org-token-file "$BASE/orgtoken" --host testhost --role homelab --yes "$@" </dev/null 2>&1)"
  RC=$?
  set -e
}
mid --recovery-recipient "$PQREC"
check "missing --org is refused, nothing created" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q -- '--org <org> is required' && [ ! -s '$STUB_DIR/gh.log' ] && [ ! -e '$H/.ssh' ]" _ "$OUT"
mid --org "$ORG"
check "missing --recovery-recipient is refused, nothing created" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q -- '--recovery-recipient <age1pq1...> is required' && [ ! -s '$STUB_DIR/gh.log' ] && [ ! -e '$H/.ssh' ]" _ "$OUT"
CLASSIC="age1$(printf 'q%.0s' $(seq 1 58))"
for badrec in "$CLASSIC" "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHcbV3DnP0R7ZqzuZXlzJ4W0eKgQh8WZ1xX3nYF8mO6c" "age1pq1short"; do
  mid --org "$ORG" --recovery-recipient "$badrec"
  check "recovery recipient '${badrec:0:12}...' (not a full age1pq1 key) is refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'must be a post-quantum age1pq1' && [ ! -s '$STUB_DIR/gh.log' ]" _ "$OUT"
done
check "the script holds no default org or recovery key" bash -c "! grep -nE 'AGE_RECOVERY=\"age1|ORG=\"[A-Za-z]' '$ROOT/scripts/bootstrap.sh'"
for bad in TestHost a_b -x "$(printf 'a%.0s' $(seq 1 33))"; do
  runboot --host "$bad" --role homelab --yes
  check "bad --host '$bad' refused before anything is created" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'bad --host label' && [ ! -s '$STUB_DIR/gh.log' ] && [ ! -e '$H/.ssh' ]" _ "$OUT"
done
runboot --host testhost --role homelab --yes --org acme-client
check "--org equal to a keep-out owner (any case) is refused in preflight" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'also a keep-out owner' && [ ! -s '$STUB_DIR/gh.log' ]" _ "$OUT"

newhome xt
for how in "bash -x" "env SHELLOPTS=xtrace bash"; do
  set +e
  XOUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" $how "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --org-token-file "$BASE/orgtoken" "${REQ[@]}" --host testhost --role homelab --yes </dev/null 2>&1)"
  XRC=$?
  set -e
  check "xtrace ($how): refused, and the org token is nowhere in the output" bash -c "[ $XRC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'refusing to run with xtrace' && ! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK' && [ ! -s '$STUB_DIR/gh.log' ]" _ "$XOUT"
done

newhome dl
runboot --host testhost --role homelab --yes --deny-term 'c++' --deny-term '#x' --deny-term 'a.b(c)'
check "deny terms are written as literal RE2 regexes; a leading # is escaped" bash -c "[ $RC -eq 0 ] && grep -qxF 'c\\+\\+' '$H/.config/claude-memsync/denylist-personal.txt' && grep -qxF '\\#x' '$H/.config/claude-memsync/denylist-personal.txt' && grep -qxF 'a\\.b\\(c\\)' '$H/.config/claude-memsync/denylist-personal.txt' && ! grep -q '^#' '$H/.config/claude-memsync/denylist-personal.txt'"
check "the real init accepted that denylist" bash -c "grep -q 'c\\\\+\\\\+' '$H/.claudesync.d/personal/denylist.txt'"

echo "--- shell-style quoting, one table with the Go test"
while IFS="$(printf '\t')" read -r input want; do
  case "$input$want" in '#'*) continue ;; esac
  [ -n "$want" ] || continue
  set +e; got="$(bash "$ROOT/scripts/bootstrap.sh" --split-args-json "$input" 2>/dev/null)"; grc=$?; set -e
  if [ "$want" = ERR ]; then
    check "split '$input' is an error" [ "$grc" -ne 0 ]
  else
    check "split '$input'" python3 -I -c 'import json,sys; sys.exit(0 if json.loads(sys.argv[1]) == json.loads(sys.argv[2]) else 1)' "$got" "$want"
  fi
done <"$ROOT/internal/secretcmd/testdata/splitargs.tsv"
newhome qc
: >"$BASE/tool.log" 2>/dev/null || true
ORGARGS=(--org-token-cmd "secret-tool read 'vault://My Vault/item'")
runboot --host testhost --role homelab --yes
check "quoted --org-token-cmd: the argument with a space reaches the tool whole" bash -c "[ $RC -eq 0 ] && grep -qx 'secret-tool read vault://My Vault/item' '$STUB_DIR/tool.log'"
ORGARGS=(--org-token-cmd "secret-tool read 'unbalanced")
runboot --host testhost --role homelab --yes
check "unbalanced quote in --org-token-cmd is refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'unbalanced quote'" _ "$OUT"
ORGARGS=(--org-token-file "$BASE/orgtoken")

echo "--- deny flags"
newhome nodeny
set +e
NODENY="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" GIT_CONFIG_NOSYSTEM=1 \
  bash "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --repo-url "https://example.invalid/other.git" ${ORGARGS[@]+"${ORGARGS[@]}"} "${REQ[@]}" --host testhost --role homelab --dry-run </dev/null 2>&1)"
set -e
check "no keep-out flags: a calm one-line note, no warning prefix" bash -c "printf '%s' \"\$1\" | grep -qx 'keep-out lists: none (routing alone decides what syncs)' && ! printf '%s' \"\$1\" | grep -q 'client owners'" _ "$NODENY"
check "--repo-url is accepted" bash -c "! printf '%s' \"\$1\" | grep -q 'unknown argument'" _ "$NODENY"
echo "--- refusals"
newhome pub
STUB_PUBLIC=1 runboot --host testhost --role homelab --yes
check "public repo aborts" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'NOT private'" _ "$OUT"
check "public repo: no deploy key, no key generated" bash -c "! grep -q deploy-key '$STUB_DIR/gh.log' && [ ! -e '$H/.ssh/mem-infra-homelab_ed25519' ]"

newhome role1
runboot --host testhost --role client-acme --yes
check "unknown client role refused" [ "$RC" -ne 0 ]
runboot --host testhost --role orch --yes
check "orch role refused" [ "$RC" -ne 0 ]

newhome exist
mkdir -p "$H/.claudesync.d/personal"
runboot --host testhost --role homelab --yes
check "existing sync dir refused" bash -c "[ $RC -ne 0 ] && [ ! -e '$H/.ssh/mem-personal_ed25519' ]"

newhome old
mkdir -p "$H/.config/memsync"; echo '{"repo":"git@x:y/z.git"}' >"$H/.config/memsync/config.json"
runboot --host testhost --role homelab --yes
check "old memsync config refused" [ "$RC" -ne 0 ]

newhome oldtimer
STUB_OLD_TIMER=1 runboot --host testhost --role homelab --yes
check "active memsync.timer refused" [ "$RC" -ne 0 ]

newhome hk
STUB_BAD_HOSTKEY=1 runboot --host testhost --role homelab --yes
check "wrong GitHub host key fingerprint refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'expected SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU'" _ "$OUT"

newhome dk
STUB_DEPLOYKEY_DISABLED=1 runboot --host testhost --role homelab --yes
check "deploy keys disabled gives the org hint" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'deploy_keys_enabled_for_repositories'" _ "$OUT"


# ============================ personal-dev ======================================
echo "--- personal-dev run"
newhome pdev
mkdir -p "$H/code/notes"
mkgit "$H/code/ownproj" "git@github.com:gitgat/ownproj.git"
mkgit "$H/code/thirdlib" "https://github.com/someoneelse/thirdlib.git"
mkgit "$H/code/clientapp" "https://github.com/Acme-Client/clientapp.git"
export H
runboot --host testmac --role personal-dev --workspace "$H/code" --own-owner gitgat --yes
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "personal-dev run exits 0" [ "$RC" -eq 0 ]
check "only the own repo gets a mem-project repo" bash -c "grep 'repo create' '$STUB_DIR/gh.log' | grep -c 'mem-project-' | grep -qx 2 && grep -q 'create example-org/mem-project-ownproj --private' '$STUB_DIR/gh.log' && grep -q 'create example-org/mem-project-code-workspace --private' '$STUB_DIR/gh.log' && ! grep -qE 'thirdlib|clientapp' '$STUB_DIR/gh.log'"
check "third-party repo listed as not synced" bash -c "printf '%s' \"\$1\" | grep -q 'thirdlib.*not synced (third-party)'" _ "$OUT"
check "client-owner repo refused" bash -c "printf '%s' \"\$1\" | grep -q 'clientapp.*REFUSED (keep-out)'" _ "$OUT"
check "home pinned to personal and code pinned to the workspace repo" jsonok '
import json,sys,re,os
d=json.load(open(sys.argv[1]))
esc=lambda p: re.sub(r"[^A-Za-z0-9]","-",p)
home=os.environ["H"]
pins={r["folder"]:r["repo"] for r in d["routes"] if "folder" in r}
assert pins[esc(home)]=="personal", pins
assert pins[esc(home+"/code")]=="project-code-workspace", pins
assert pins.get(esc(home+"/code/notes")) is None
rem={r["remote"]:r["repo"] for r in d["routes"] if "remote" in r}
assert rem=={"github.com/gitgat/ownproj":"project-ownproj"}, rem
assert d["default"]=="deny"
assert set(d["repos"])=={"personal","project-code-workspace","project-ownproj"}, set(d["repos"])' "$H/.config/claude-memsync/routes.json"
check "home pin call recorded" has "$STUB_DIR/memsync.log" 'route home personal --yes'
check "project kind denylist has the deny term" bash -c "grep -qix acmeclient '$H/.config/claude-memsync/denylist-project.txt'"
check "no client/orch/acmeclient repo names" bash -c "! grep -rIiE 'mem-(client|orch)|mem-[a-z-]*acmeclient' '$H/.config' '$H/.ssh/config' '$STUB_DIR/gh.log'"
check "related suggestions placeholder printed" bash -c "printf '%s' \"\$1\" | grep -q 'related suggestions: not built yet'" _ "$OUT"
check "non-tty without --yes refuses" bash -c "
  HOME='$H' PATH='$STUBS:$PATH' STUB_DIR='$STUB_DIR' STUB_TOKEN=x STUB_ORG_TOKEN=$ORGTOK CLAUDE_UTILS_URL=https://example.invalid/claude-utils.git bash '$ROOT/scripts/bootstrap.sh' --sha $SHA --org-token-file '$BASE/orgtoken' --org $ORG --recovery-recipient $PQREC --host m2 --role personal-dev --workspace '$H/code' </dev/null >/dev/null 2>&1; [ \$? -ne 0 ]"

echo "--- personal-dev re-run with a new project"
mkgit "$H/code/extra" "git@github.com:gitgat/extra.git"
git init -q --bare -b main "$BASE/remotes/mem-project-extra.git"
git config --file "$H/.gitconfig" "url.$BASE/remotes/mem-project-extra.git.insteadOf" "git@gh-mem-project-extra:$ORG/mem-project-extra.git"
CREATES2="$(grep -c 'repo create' "$STUB_DIR/gh.log")"
runboot --host testmac --role personal-dev --workspace "$H/code" --own-owner gitgat --yes
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "re-run with a new project exits 0" [ "$RC" -eq 0 ]
check "routes.json regenerated from the plan: new repo and remote route, old ones kept, backup kept" jsonok '
import json,sys,os
d=json.load(open(sys.argv[1]))
rem={r["remote"]:r["repo"] for r in d["routes"] if "remote" in r}
assert rem=={"github.com/gitgat/ownproj":"project-ownproj","github.com/gitgat/extra":"project-extra"}, rem
assert set(d["repos"])=={"personal","project-code-workspace","project-ownproj","project-extra"}, set(d["repos"])
assert os.path.exists(sys.argv[1]+".bak")' "$H/.config/claude-memsync/routes.json"
check "the home pin is back after the regeneration" jsonok '
import json,sys,re,os
d=json.load(open(sys.argv[1]))
pins={r["folder"]:r["repo"] for r in d["routes"] if "folder" in r}
assert pins[re.sub(r"[^A-Za-z0-9]","-",os.environ["H"])]=="personal", pins' "$H/.config/claude-memsync/routes.json"
check "only the new project repo was created, private, and inited" bash -c "[ \$(grep -c 'repo create' '$STUB_DIR/gh.log') -eq $((CREATES2 + 1)) ] && grep -q 'create example-org/mem-project-extra --private' '$STUB_DIR/gh.log' && grep -q 'init --remote git@gh-mem-project-extra' '$STUB_DIR/memsync.log' && [ -d '$H/.claudesync.d/project-extra' ]"
echo "--- personal-dev with no --own-owner: only --org is own"
newhome pdevdef
mkdir -p "$H/code"
mkgit "$H/code/ownproj" "git@github.com:example-org/ownproj.git"
mkgit "$H/code/gitproj" "git@github.com:gitgat/gitproj.git"
mkgit "$H/code/otherproj" "git@github.com:some-other-owner/otherproj.git"
export H
runboot --host testmac --role personal-dev --workspace "$H/code" --yes
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "no-own-owner run exits 0" [ "$RC" -eq 0 ]
check "no built-in own owner in routes: only the --org remote is routed" jsonok '
import json,sys
d=json.load(open(sys.argv[1]))
rem={r["remote"]:r["repo"] for r in d["routes"] if "remote" in r}
assert rem=={"github.com/example-org/ownproj":"project-ownproj"}, rem
assert set(d["repos"])=={"personal","project-code-workspace","project-ownproj"}, set(d["repos"])' "$H/.config/claude-memsync/routes.json"
check "other owners are listed third-party and get no repo" bash -c "printf '%s' \"\$1\" | grep -q 'gitproj.*not synced (third-party)' && printf '%s' \"\$1\" | grep -q 'otherproj.*not synced (third-party)' && ! grep -qE 'gitproj|otherproj' '$STUB_DIR/gh.log'" _ "$OUT"
check "no built-in owner name in the generated config" bash -c "! grep -rIiE 'gitgat' '$H/.config/claude-memsync'"

newhome rtdiff
mkdir -p "$H/.config/claude-memsync"; echo '{"repos":{},"routes":[],"default":"deny"}' >"$H/.config/claude-memsync/routes.json"
set +e
RTOUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" bash "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --org-token-file "$BASE/orgtoken" "${REQ[@]}" --host testhost --role homelab --yes --dry-run </dev/null 2>&1)"
set -e
check "dry run with a differing routes.json says what it would do and leaves the file alone" bash -c "printf '%s' \"\$1\" | grep -q 'writing .*routes.json' && [ \$(cat '$H/.config/claude-memsync/routes.json' | tr -d ' \n') = '{\"repos\":{},\"routes\":[],\"default\":\"deny\"}' ] && [ ! -e '$H/.config/claude-memsync/routes.json.bak' ]" _ "$RTOUT"

echo "--- check app and admin entry: refusals and clean-up"
newhome ck1
prompt_setup
chmod 644 "$H/.config/claude-memsync/memsync-check.pem"
rundoc 2
check "check key file with a loose mode is refused before anything changes" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'must be mode 0600' && ! grep -q 'repo create' '$STUB_DIR/gh.log' && [ ! -e '$H/.config/claude-memsync/apps.json' ]" _ "$OUT"
newhome ck2
prompt_setup
ln "$H/.config/claude-memsync/memsync-check.pem" "$H/.config/claude-memsync/second-link.pem"
rundoc 2
check "check key file with two hard links is refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'one hard link'" _ "$OUT"
newhome ck3
prompt_setup
rm "$H/.config/claude-memsync/memsync-check.pem"; ln -s "$BASE/check.pem" "$H/.config/claude-memsync/memsync-check.pem"
rundoc 2
check "check key file that is a symlink is refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'not a plain file'" _ "$OUT"
newhome ck4
prompt_setup
STUB_PUBLIC=1 rundoc 2
check "a failed run still removes the admin entry" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'NOT private' && ! grep -qs memsync-admin '$H/.config/claude-memsync/apps.json'" _ "$OUT"
newhome ck5
prompt_setup
ORGARGS=()
runboot --host testhost --role homelab --yes --org-app memsync-admin --check-app memsync-check --check-key-file "$H/.config/claude-memsync/memsync-check.pem"
check "--check-app without --check-app-id is refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'together'" _ "$OUT"
ORGARGS=(--org-token-file "$BASE/orgtoken")
newhome ck6
runboot --host testhost --role homelab --yes --org-app-id 12345 --org-app-key-cmd "cat $BASE/app.pem"
check "--org-app-id without --org-app is refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'go with --org-app'" _ "$OUT"

newhome ck7
prompt_setup
printf '[{"name":"memsync-admin","appId":"999","org":"example-org","keyCmd":["cat","/nonexistent"]}]\n' >"$H/.config/claude-memsync/apps.json"; chmod 600 "$H/.config/claude-memsync/apps.json"
rundoc 1
check "dry run with a pre-existing admin entry is refused" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'already has an entry named memsync-admin'" _ "$OUT"
rundoc 2
check "pre-existing admin entry: refused, nothing created, the entry is neither replaced nor deleted" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'already has an entry named memsync-admin' && ! grep -q 'repo create' '$STUB_DIR/gh.log' && grep -q '\"999\"' '$H/.config/claude-memsync/apps.json' && grep -q /nonexistent '$H/.config/claude-memsync/apps.json'" _ "$OUT"

echo "--- check key recorded in the registry"
newhome reg
prompt_setup
mkdir -p "$H/registry"; git -C "$H/registry" init -q -b main
git config --file "$H/.gitconfig" user.name tester; git config --file "$H/.gitconfig" user.email tester@example.invalid
git -C "$H/registry" -c user.name=tester -c user.email=tester@example.invalid commit -q --allow-empty -m init
printf '{"repos":{},"routes":[],"default":"deny","registry":"%s"}\n' "$H/registry" >"$H/.config/claude-memsync/routes.json"
rundoc 2
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "registry configured: the run keeps it and exits 0" bash -c "[ $RC -eq 0 ] && grep -q '\"registry\"' '$H/.config/claude-memsync/routes.json'" _ "$OUT"
check "registry configured: host check-key called with the fingerprint and the app slug" has "$STUB_DIR/memsync.log" 'host check-key --fingerprint SHA256:[^ ]+ --app memsync-check'
check "registry configured: hosts/testhost/check-key committed with the fingerprint, no key" bash -c "git -C '$H/registry' show HEAD:hosts/testhost/check-key | grep -qE 'SHA256:[A-Za-z0-9+/]{43}=' && git -C '$H/registry' show HEAD:hosts/testhost/check-key | grep -q memsync-check && ! git -C '$H/registry' log -p | grep -qF -- '$(sed -n 2p "$BASE/check.pem")'"

# `recipients rekey-pq --all` over two classic repos, end to end with the real
# binary: every note must read back, after the rekey, exactly as it was written,
# from the work-tree re-smudged through the new hybrid filter, and the remotes
# must hold one root commit of hybrid ciphertext.
echo "--- rekey-pq --all: classic repos, plaintext equality"
newhome rk
RKC="$H/claude/projects"
mkdir -p "$RKC/-p1/memory" "$RKC/-p2/memory"
printf 'first note\n' >"$RKC/-p1/memory/one.md"
printf 'second note\nwith two lines\n' >"$RKC/-p1/memory/two.md"
printf -- '- [one](one.md)\n' >"$RKC/-p1/memory/MEMORY.md"
printf 'other project\n' >"$RKC/-p2/memory/three.md"
RKROUTES="$H/routes-rk.json"
RKOK=1
for n in alpha beta; do
  git init -q --bare -b main "$BASE/remotes/rk-$n.git"
  HOME="$H" GIT_CONFIG_NOSYSTEM=1 "$REAL/claude-memsync" init --remote "$BASE/remotes/rk-$n.git" \
    --sync-dir "$H/.claudesync.d/$n" --claude-dir "$RKC" --encrypt --encryption-scheme classic \
    --identity "$H/id-$n.txt" >"$BASE/rk-init-$n.log" 2>&1 || RKOK=0
done
check "rekey: two classic repos initialised with the real binary" [ "$RKOK" -eq 1 ]
printf '{"repos":{"alpha":{"config":"%s","kind":"personal"},"beta":{"config":"%s","kind":"project"}},"routes":[],"default":"deny","personal":"alpha"}\n' \
  "$H/.claudesync.d/alpha/config.json" "$H/.claudesync.d/beta/config.json" >"$RKROUTES"
chmod 600 "$RKROUTES"
rkhash() { # repo: hash of every note in the repo's work-tree
  (cd "$H/.claudesync.d/$1/projects" && find . -type f | LC_ALL=C sort | while read -r f; do printf '%s ' "$f"; cksum <"$f"; done)
}
rkhash_origin() { # repo: the same lines, from origin's ciphertext decrypted with the repo's new identity
  local f t="$BASE/rk-origin.blob"
  git -C "$BASE/remotes/rk-$1.git" ls-tree -r --name-only main | grep '^projects/' | LC_ALL=C sort | while read -r f; do
    git -C "$BASE/remotes/rk-$1.git" cat-file blob "main:$f" >"$t"
    printf './%s ' "${f#projects/}"; (cd "$H/.claudesync.d/$1" && HOME="$H" "$REAL/claude-memsync" age-textconv "$t") | cksum
  done
}
RKB_A="$(rkhash alpha)"; RKB_B="$(rkhash beta)"
check "rekey: the work-trees hold the notes" bash -c "[ -n \"\$1\" ] && [ -n \"\$2\" ]" _ "$RKB_A" "$RKB_B"
set +e
RKOUT="$(HOME="$H" GIT_CONFIG_NOSYSTEM=1 CLAUDE_MEMSYNC_ROUTES="$RKROUTES" "$REAL/claude-memsync" recipients rekey-pq --all \
  --recovery-recipient "$PQREC" --host rk --fresh-root --apply 2>&1)"
RKRC=$?
set -e
[ "$RKRC" -eq 0 ] || echo "$RKOUT" | tail -20
check "rekey --all exits 0" [ "$RKRC" -eq 0 ]
for n in alpha beta; do
  SY="$H/.claudesync.d/$n"; OR="$BASE/remotes/rk-$n.git"
  check "rekey $n: scheme pq, new identity in use, classic identity retired" bash -c "[ \"\$(git -C '$SY' config memage.scheme)\" = pq ] && git -C '$SY' config memage.identity | grep -q 'id-$n-pq.txt' && [ -e '$H/id-$n.txt.classic-retired' ] && [ ! -e '$H/id-$n.txt' ]"
  check "rekey $n: origin has one root commit" bash -c "[ \"\$(git -C '$OR' rev-list --count main)\" = 1 ]"
  check "rekey $n: every note on origin is hybrid-only ciphertext" bash -c "
    files=\$(git -C '$OR' ls-tree -r --name-only main | grep '^projects/'); [ -n \"\$files\" ] || exit 1
    for f in \$files; do b=\$(git -C '$OR' cat-file blob main:\$f | head -c 600); printf '%s' \"\$b\" | grep -aq -- '-> mlkem768x25519' || exit 1; printf '%s' \"\$b\" | grep -aq -- '-> X25519' && exit 1; done; exit 0"
  # re-smudge: remove the notes from the work-tree and check them out again,
  # which decrypts the origin's ciphertext with the new identity.
  rm -rf "$SY/projects"; git -C "$SY" checkout -q -- projects
  if [ "$n" = alpha ]; then WANT="$RKB_A"; else WANT="$RKB_B"; fi
  check "rekey $n: every note decrypts to exactly what it was" [ "$(rkhash "$n")" = "$WANT" ]
  # The same comparison against what origin holds: its blobs decrypted
  # (age-textconv), not the local index or work-tree.
  check "rekey $n: origin's blobs, decrypted, equal the original plaintext" [ "$(rkhash_origin "$n")" = "$WANT" ]
  check "rekey $n: origin's .age-recipients carry the '# host:' and '# recovery' labels" bash -c "
    r=\$(git -C '$OR' show main:.age-recipients) && printf '%s\n' \"\$r\" | grep -q '^# host:rk' && printf '%s\n' \"\$r\" | grep -q '^# recovery' && printf '%s\n' \"\$r\" | grep -q '$PQREC'"
  check "rekey $n: the work-tree is clean" bash -c "[ -z \"\$(git -C '$SY' status --porcelain)\" ]"
done
# Restart the daemon (a foreground run, stopped by timeout, as in the upgrade
# section) on the rekeyed repo, drop one new note in the Claude tree, and check
# it lands on origin as hybrid ciphertext that decrypts back.
echo "--- rekey-pq --all: the restarted daemon syncs a new note as hybrid ciphertext"
printf 'written after the rekey\n' >"$RKC/-p1/memory/after-rekey.md"
HOME="$H" GIT_CONFIG_NOSYSTEM=1 timeout 25 "$REAL/claude-memsync" run -config "$H/.claudesync.d/alpha/config.json" -skip-quiet-window >"$BASE/rk-run.log" 2>&1 || true
RKN="projects/-p1/memory/after-rekey.md"
check "rekey: the new note reached origin" bash -c "git -C '$BASE/remotes/rk-alpha.git' cat-file -e main:$RKN"
check "rekey: ... as hybrid-only ciphertext" bash -c "b=\$(git -C '$BASE/remotes/rk-alpha.git' cat-file blob main:$RKN | head -c 600); printf '%s' \"\$b\" | grep -aq -- '-> mlkem768x25519' && ! printf '%s' \"\$b\" | grep -aq -- '-> X25519'"
check "rekey: ... which decrypts to the note" bash -c "git -C '$BASE/remotes/rk-alpha.git' cat-file blob main:$RKN >'$BASE/rk-new.blob' && [ \"\$(cd '$H/.claudesync.d/alpha' && HOME='$H' '$REAL/claude-memsync' age-textconv '$BASE/rk-new.blob')\" = 'written after the rekey' ]"
check "rekey: the notes read back byte for byte" bash -c "diff <(cat '$RKC/-p1/memory/two.md') '$H/.claudesync.d/alpha/projects/-p1/memory/two.md'"

# An upgrade: a second build is bootstrapped over the first and the daemon
# restarts. git runs whatever program the clone's config names, so the filters
# must end up on the new build, or a newer on-disk format (host labels in
# .age-recipients) makes every clean filter of the old build fail. Both builds
# live outside the temp dirs, which the daemon never writes into git config.
echo "--- upgrade: filters follow the new build"
LDV="github.com/MarimerLLC/claude-utils/internal/version.Override"
buildver() { # outdir version: claude-memsync and claude-memmerge reporting that version
  mkdir -p "$1"
  if command -v go >/dev/null; then
    (cd "$ROOT" && go build -ldflags "-X $LDV=$2" -o "$1/claude-memsync" ./cmd/claude-memsync \
      && go build -ldflags "-X $LDV=$2" -o "$1/claude-memmerge" ./cmd/claude-memmerge)
  elif command -v docker >/dev/null; then
    docker run --rm --user "$(id -u):$(id -g)" -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath -e GOFLAGS=-buildvcs=false \
      -v "$ROOT:/src:ro" -v "$1:/out" -w /src golang:1.25 \
      sh -c "go build -ldflags '-X $LDV=$2' -o /out/claude-memsync ./cmd/claude-memsync && go build -ldflags '-X $LDV=$2' -o /out/claude-memmerge ./cmd/claude-memmerge"
  else
    return 1
  fi
}
under_temp() { # the daemon never repoints git config at a binary under a temp dir
  case "$1" in /tmp | /tmp/* | "${TMPDIR:-/tmp}" | "${TMPDIR:-/tmp}"/*) return 0 ;; *) return 1 ;; esac
}
if ! command -v go >/dev/null && ! command -v docker >/dev/null; then
  pending "upgrade check needs go or docker to build two versions of claude-memsync"
elif under_temp "$BASE"; then
  bad "upgrade check needs the work dir outside TMPDIR and /tmp (the daemon treats binaries there as throwaways): pass one, e.g. scripts/bootstrap_test.sh \$PWD/.bin/bt"
else
  UPD="$BASE/upgrade"; mkdir -p "$UPD"
  for v in A B; do buildver "$UPD/real$v" "harness-$v"; done
  git clone -q "$BASE/utils.git" "$UPD/second"
  git -C "$UPD/second" -c user.name=tester -c user.email=tester@example.invalid commit -q --allow-empty -m "second build"
  SHA2="$(git -C "$UPD/second" rev-parse HEAD)"
  git -C "$UPD/second" push -q origin HEAD:refs/heads/second
  HROOT="$UPD" REAL_RUN="$UPD/realA"
  newhome up
  runboot --host testhost --role homelab --yes
  [ "$RC" -eq 0 ] || echo "$OUT" | tail -20
  check "upgrade: first bootstrap exits 0" [ "$RC" -eq 0 ]
  SYNC="$H/.claudesync.d/infra-homelab"
  check "upgrade: filters start on build A" bash -c "git -C '$SYNC' config --get filter.memage.process | grep -q '$UPD/realA/claude-memsync'"
  REAL_RUN="$UPD/realB" SHA="$SHA2"
  runboot --host testhost --role homelab --yes
  [ "$RC" -eq 0 ] || echo "$OUT" | tail -20
  check "upgrade: second bootstrap exits 0" [ "$RC" -eq 0 ]
  check "upgrade: status warns that the filters run the old build" bash -c "printf '%s' \"\$1\" | grep -q 'harness-A'" _ "$OUT"
  # The daemon's start-up refresh (a foreground run, stopped once it is up).
  HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" timeout 10 "$UPD/realB/claude-memsync" run --skip-quiet-window >"$UPD/run.log" 2>&1 || true
  check "upgrade: the restarted daemon repoints the filter at build B" bash -c "git -C '$SYNC' config --get filter.memage.process | grep -q '$UPD/realB/claude-memsync'"
  check "upgrade: the merge drivers follow" bash -c "git -C '$SYNC' config --get merge.memtext.driver | grep -q '$UPD/realB/claude-memmerge'"
  # After the upgrade a commit and a sync must work end to end through the new
  # build's filter, against recipients that carry "# host:" labels.
  check "upgrade: the recipients file carries a host label" bash -c "grep -q '# host:testhost' '$SYNC/.age-recipients'"
  check "upgrade: a note written through the filter commits" bash -c "
    mkdir -p '$SYNC/projects/-upg/memory' && printf 'written after the upgrade\\n' >'$SYNC/projects/-upg/memory/after.md' &&
    git -C '$SYNC' add -A && git -C '$SYNC' -c user.name=t -c user.email=t@example.invalid commit -q -m 'after upgrade' &&
    git -C '$SYNC' cat-file blob HEAD:projects/-upg/memory/after.md | head -c 40 | grep -aq 'age-encryption.org'"
  check "upgrade: ... and pushes (the push guard accepts it)" bash -c "export HOME='$H' GIT_CONFIG_NOSYSTEM=1; git -C '$SYNC' push -q origin main && git -C '$BASE/remotes/mem-infra-homelab.git' cat-file -e main:projects/-upg/memory/after.md"
  check "upgrade: ... and a pull --rebase finds nothing to do, with the work-tree clean" bash -c "export HOME='$H' GIT_CONFIG_NOSYSTEM=1; git -C '$SYNC' pull -q --rebase origin main && [ -z \"\$(git -C '$SYNC' status --porcelain)\" ]"
  # The daemon itself: a note dropped in the Claude tree is mirrored, committed and pushed.
  FOLD="$(printf '%s' "$H" | sed 's/[^A-Za-z0-9]/-/g')"
  mkdir -p "$H/.claude/projects/$FOLD/memory"
  printf 'synced by the upgraded daemon\n' >"$H/.claude/projects/$FOLD/memory/daemon.md"
  HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" timeout 25 "$UPD/realB/claude-memsync" run --skip-quiet-window >"$UPD/run2.log" 2>&1 || true
  check "upgrade: the daemon on build B syncs a new note to the remote as ciphertext" bash -c "
    git -C '$BASE/remotes/mem-infra-homelab.git' cat-file -e main:projects/$FOLD/memory/daemon.md &&
    git -C '$BASE/remotes/mem-infra-homelab.git' cat-file blob main:projects/$FOLD/memory/daemon.md | head -c 40 | grep -aq 'age-encryption.org'"
  check "upgrade: ... which decrypts back to the note" bash -c "
    git -C '$BASE/remotes/mem-infra-homelab.git' cat-file blob main:projects/$FOLD/memory/daemon.md >'$UPD/daemon.blob' &&
    [ \"\$(cd '$SYNC' && '$UPD/realB/claude-memsync' age-textconv '$UPD/daemon.blob')\" = 'synced by the upgraded daemon' ]"
  unset HROOT REAL_RUN
fi

echo
echo "passed=$PASS failed=$FAIL pending=$PENDING"
[ "$FAIL" -eq 0 ]
