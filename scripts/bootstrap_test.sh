#!/usr/bin/env bash
# End-to-end test of scripts/bootstrap.sh in a fake HOME with stub gh,
# systemctl, claude, go and ssh-keyscan on PATH, and local bare repos standing
# in for the GitHub remotes. It never touches the real HOME, ~/.ssh, systemd or
# GitHub. Usage: scripts/bootstrap_test.sh [workdir]
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
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BASE="${1:-${BOOTSTRAP_TEST_DIR:-${TMPDIR:-/tmp}/bootstrap-test}}"
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
if [ "$1" = version ]; then echo "go version go1.25.1 linux/amd64"; exit 0; fi
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
FAKE_TOKEN="$ORGTOK" python3 "$BASE/fakeapi.py" "$BASE/fakeapi.log" "$BASE/fakeapi.port" &
FAKE_PID=$!
trap 'kill $FAKE_PID 2>/dev/null || true' EXIT
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
  set +e
  OUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="${REAL_RUN:-$REAL}" GIT_CONFIG_NOSYSTEM=1 \
    BOOTSTRAP_APP_API_BASE="http://127.0.0.1:$APIPORT" CLAUDE_UTILS_URL="https://example.invalid/claude-utils.git" \
    bash "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --deny-owner Acme-Client --deny-term acmeclient ${ORGARGS[@]+"${ORGARGS[@]}"} "${REQ[@]}" "$@" </dev/null 2>&1)"
  RC=$?
  set -e
}

has()    { grep -qE -- "$2" "$1"; }
hasnt()  { ! grep -qE -- "$2" "$1"; }
mode()   { [ "$(stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1")" = "$2" ]; }
jsonok() { python3 -I -c "$1" "$2"; }

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
check "banner names every extra org and the client org" bash -c "printf '%s' \"\$1\" | grep -q \"WARNING: this host's gh login can reach Acme-Client, SomeOther. An agent host holds an owner-level token with client-org reach.\" && printf '%s' \"\$1\" | grep -q 'CLIENT ORG(S) reachable through the gh login: Acme-Client'" _ "$OUT"
check "banner in plan and again in summary" bash -c "[ \$(printf '%s' \"\$1\" | grep -c \"WARNING: this host's gh login can reach\") -ge 3 ]" _ "$OUT"
check "summary repeats the warning" bash -c "printf '%s' \"\$1\" | sed -n '/== j. Summary/,\$p' | grep -q 'WARNING: this host'" _ "$OUT"
check "zero gh calls against any other org" bash -c "! grep -v 'user/orgs' '$STUB_DIR/gh.log' | grep -iE 'acme-client|someother'; [ \$(grep -E 'repo|api repos' '$STUB_DIR/gh.log' | grep -vc 'example-org') -eq 0 ]"
newhome orgs2
STUB_ORGS="$(printf 'example-org\nSomeOther')" runboot --host testhost --role homelab --yes --allow-org someother
check "--allow-org accepts an extra org" [ "$RC" -eq 0 ]

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
check "--org equal to a --deny-owner (any case) is refused in preflight" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'also a --deny-owner' && [ ! -s '$STUB_DIR/gh.log' ]" _ "$OUT"

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
check "no deny flags: loud warning" bash -c "printf '%s' \"\$1\" | grep -q 'no client owners or terms configured: client repos are only protected by routing'" _ "$NODENY"
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
mkgit() { # dir remote
  mkdir -p "$1"; git -C "$1" init -q; git -C "$1" remote add origin "$2"
}
mkgit "$H/code/ownproj" "git@github.com:gitgat/ownproj.git"
mkgit "$H/code/thirdlib" "https://github.com/someoneelse/thirdlib.git"
mkgit "$H/code/clientapp" "https://github.com/Acme-Client/clientapp.git"
export H
runboot --host testmac --role personal-dev --workspace "$H/code" --yes
[ "$RC" -eq 0 ] || echo "$OUT" | tail -20
check "personal-dev run exits 0" [ "$RC" -eq 0 ]
check "only the own repo gets a mem-project repo" bash -c "grep 'repo create' '$STUB_DIR/gh.log' | grep -c 'mem-project-' | grep -qx 2 && grep -q 'create example-org/mem-project-ownproj --private' '$STUB_DIR/gh.log' && grep -q 'create example-org/mem-project-code-workspace --private' '$STUB_DIR/gh.log' && ! grep -qE 'thirdlib|clientapp' '$STUB_DIR/gh.log'"
check "third-party repo listed as not synced" bash -c "printf '%s' \"\$1\" | grep -q 'thirdlib.*not synced (third-party)'" _ "$OUT"
check "client-owner repo refused" bash -c "printf '%s' \"\$1\" | grep -q 'clientapp.*REFUSED (client owner)'" _ "$OUT"
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
runboot --host testmac --role personal-dev --workspace "$H/code" --yes
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
newhome rtdiff
mkdir -p "$H/.config/claude-memsync"; echo '{"repos":{},"routes":[],"default":"deny"}' >"$H/.config/claude-memsync/routes.json"
set +e
RTOUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" bash "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --org-token-file "$BASE/orgtoken" "${REQ[@]}" --host testhost --role homelab --yes --dry-run </dev/null 2>&1)"
set -e
check "dry run with a differing routes.json says what it would do and leaves the file alone" bash -c "printf '%s' \"\$1\" | grep -q 'writing .*routes.json' && [ \$(cat '$H/.config/claude-memsync/routes.json' | tr -d ' \n') = '{\"repos\":{},\"routes\":[],\"default\":\"deny\"}' ] && [ ! -e '$H/.config/claude-memsync/routes.json.bak' ]" _ "$RTOUT"

# ============== the paste prompts in docs/bootstrap.md, as written =============
# The command lines are taken from the doc (the first two under (a), the next
# two under (b): dry run, then real), the placeholders are substituted, and they
# run with the prompt's own step 1 (the clone) done by hand. The doc and this
# test cannot drift apart.
ssh-keygen -q -t rsa -b 2048 -m PEM -N '' -f "$BASE/check" && mv "$BASE/check" "$BASE/check.pem" && chmod 600 "$BASE/check.pem"
ADMIN_MARK="$(sed -n 2p "$BASE/app.pem")"
docline() { # n: the nth command line of the doc
  awk -v n="$1" '/^bash ~\/\.local\/src\/claude-utils-<commit>\/scripts\/bootstrap\.sh /{c++; if (c == n) print}' "$ROOT/docs/bootstrap.md"
}
fill() { # line: placeholders -> test values
  local l="$1"
  l="${l//<commit>/$SHA}"; l="${l//<host-label>/testhost}"; l="${l//<client-org>/Acme-Client}"; l="${l//<client-term>/acmeclient}"
  l="${l//<admin-app-id>/12345}"; l="${l//<admin-key-cmd>/cat $BASE/app.pem}"
  l="${l//<org>/$ORG}"; l="${l//<recovery-recipient>/$PQREC}"
  l="${l//<check-app-id>/54321}"; l="${l//<check-key-file>/$H/.config/claude-memsync/memsync-check.pem}"
  printf '%s' "$l"
}
rundoc() { # n: run the nth doc line in the fake HOME; sets RC and OUT
  local cmd; cmd="$(fill "$(docline "$1")")"
  [ -n "$cmd" ] || { RC=99; OUT="doc line $1 not found"; return; }
  set +e
  OUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" REAL_DIR="$REAL" GIT_CONFIG_NOSYSTEM=1 \
    BOOTSTRAP_APP_API_BASE="http://127.0.0.1:$APIPORT" CLAUDE_UTILS_URL="https://example.invalid/claude-utils.git" \
    bash -c "$cmd" </dev/null 2>&1)"
  RC=$?
  set -e
}
prompt_setup() { # step 1 of the prompt (the clone) and the host's own check key
  mkdir -p "$H/.local/src" "$H/.config/claude-memsync"
  git clone -q "$BASE/utils.git" "$H/.local/src/claude-utils-$SHA" && git -C "$H/.local/src/claude-utils-$SHA" checkout -q "$SHA"
  cp "$BASE/check.pem" "$H/.config/claude-memsync/memsync-check.pem"; chmod 600 "$H/.config/claude-memsync/memsync-check.pem"
}
check "doc has four command lines, two per prompt" bash -c "[ -n \"\$1\" ] && [ -n \"\$2\" ] && [ -n \"\$3\" ] && [ -n \"\$4\" ] && [ -z \"\$5\" ]" _ "$(docline 1)" "$(docline 2)" "$(docline 3)" "$(docline 4)" "$(docline 5)"
check "each prompt's first line is the dry run, the second is not" bash -c "case \"\$1\" in *' --dry-run') ;; *) exit 1 ;; esac; case \"\$2\" in *' --dry-run') exit 1 ;; esac; case \"\$3\" in *' --dry-run') ;; *) exit 1 ;; esac; case \"\$4\" in *' --dry-run') exit 1 ;; esac" _ "$(docline 1)" "$(docline 2)" "$(docline 3)" "$(docline 4)"

for P in "homelab:1:2:personal infra-homelab" "personal-dev:3:4:personal"; do
  IFS=: read -r ROLEN DRYN REALN KEYSN <<<"$P"
  echo "--- paste prompt: $ROLEN"
  newhome "doc-$ROLEN"
  prompt_setup
  rundoc "$DRYN"
  [ "$RC" -eq 0 ] || echo "$OUT" | tail -20
  check "$ROLEN prompt: dry run succeeds and shows the plan" bash -c "[ $RC -eq 0 ] && printf '%s' \"\$1\" | grep -q '== Plan' && printf '%s' \"\$1\" | grep -q 'would add the app entry memsync-admin' && printf '%s' \"\$1\" | grep -q 'githubApp=memsync-check'" _ "$OUT"
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
if ! command -v go >/dev/null && ! command -v docker >/dev/null; then
  pending "upgrade check needs go or docker to build two versions of claude-memsync"
else
  UPD="$(mktemp -d "$HOME/.memsync-harness.XXXXXX")"
  trap 'rm -rf "$UPD"' EXIT
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
  unset HROOT REAL_RUN
fi

echo
echo "passed=$PASS failed=$FAIL pending=$PENDING"
[ "$FAIL" -eq 0 ]
