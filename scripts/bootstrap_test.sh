#!/usr/bin/env bash
# End-to-end test of scripts/bootstrap.sh in a fake HOME with stub gh,
# systemctl, claude, go and ssh-keyscan on PATH, and local bare repos standing
# in for the GitHub remotes. It never touches the real HOME, ~/.ssh, systemd or
# GitHub. Usage: scripts/bootstrap_test.sh [workdir]
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BASE="${1:-${BOOTSTRAP_TEST_DIR:-${TMPDIR:-/tmp}/bootstrap-test}}"
TOKEN="dummy-token-$$-do-not-persist"
ORGTOK="dummy-orgtoken-$$-do-not-persist"
ORG=Exploded-views
PASS=0 FAIL=0 PENDING=0

rm -rf "$BASE"; mkdir -p "$BASE/stubs" "$BASE/remotes"
printf '%s\n' "$ORGTOK" >"$BASE/orgtoken"; chmod 600 "$BASE/orgtoken"
ORGARGS=(--org-token-file "$BASE/orgtoken")

ok()      { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()     { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
pending() { PENDING=$((PENDING + 1)); printf 'PENDING %s\n' "$1"; }
check()   { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }

# --- does the CLI in this tree have join and --token-cmd yet? ---------------
HAVE_CLI=1
grep -rq 'token-cmd' "$ROOT/cmd/claude-memsync" --include='*.go' && grep -rq '"join"' "$ROOT/cmd/claude-memsync" --include='*.go' || HAVE_CLI=0

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
      if [ "$m" = org ]; then printf '%s\n' "${STUB_ORG_ORGS:-Exploded-views}"; else printf '%s\n' "${STUB_ORGS:-Exploded-views}"; fi
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
[ "${STUB_OLD_TIMER:-0}" = 1 ] && exit 0
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
cp "$STUB_DIR/memsync-stub" "$out"; chmod +x "$out"
EOF
cat >"$STUBS/memsync-stub" <<'EOF'
#!/bin/sh
# Stub claude-memsync: records its arguments, never the token.
# record whether the org token leaked into this process env (count only, never the value)
echo "$(basename "$0"):$(env | grep -v '^STUB_' | grep -cF -- "$STUB_ORG_TOKEN")" >>"$STUB_DIR/env.log"
echo "memsync $*" >>"$STUB_DIR/memsync.log"
case "$1" in
  init)
    tc=""; tf=""
    while [ $# -gt 0 ]; do
      [ "$1" = --sync-dir ] && d="$2"
      [ "$1" = --token-cmd ] && tc="$2"
      [ "$1" = --token-file ] && tf="$2"
      shift
    done
    mkdir -p "$d"
    # like the real CLI: init saves the token command or file in the config
    python3 -I - "$d/config.json" "$tc" "$tf" <<'PY'
import json, sys
p, tc, tf = sys.argv[1:4]
c = {}
if tc: c["githubTokenCmd"] = tc.split()
if tf: c["githubTokenFile"] = tf
json.dump(c, open(p, "w"))
PY
    ;;
  join)
    if [ "$2" = status ]; then echo "pending"; exit 0; fi
    if [ "${STUB_NOFP:-0}" = 1 ]; then echo "join request sent"; else printf "join request sent\nfingerprint: 0123456789abcdef\n"; fi ;;
  route)
    if [ "$2" = home ]; then
      R="$HOME/.config/claude-memsync/routes.json"
      f="$(printf '%s' "$HOME" | sed 's/[^A-Za-z0-9]/-/g')"
      python3 -I - "$R" "$f" "$3" <<'PY'
import json, sys
p, folder, repo = sys.argv[1:4]
d = json.load(open(p))
d["routes"].insert(0, {"folder": folder, "repo": repo})
json.dump(d, open(p, "w"), indent=2)
PY
    fi ;;
esac
exit 0
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

# --- a commit of this tree to bootstrap from -----------------------------------
git clone -q --bare "$ROOT" "$BASE/utils.git"
SHA="$(git -C "$ROOT" rev-parse HEAD)"

mkbare() { # name [with-content]
  git init -q --bare "$BASE/remotes/$1.git"
  if [ "${2:-}" = content ]; then
    local w; w="$(mktemp -d)"
    git -C "$w" init -q; echo x >"$w/a"; git -C "$w" add a
    git -C "$w" -c user.name=t -c user.email=t@example.com commit -q -m seed
    git -C "$w" push -q "$BASE/remotes/$1.git" HEAD:refs/heads/main; rm -rf "$w"
  fi
}

# newhome <name>: fresh fake HOME with insteadOf maps for every bare repo.
newhome() {
  H="$BASE/$1/home"; STUB_DIR="$BASE/$1/stub"
  rm -rf "${BASE:?}/$1"; mkdir -p "$H" "$STUB_DIR"
  cp "$STUBS/memsync-stub" "$STUB_DIR/"; cp "$BASE/otherkey" "$STUB_DIR/"
  : >"$STUB_DIR/gh.log"; : >"$STUB_DIR/memsync.log"
  rm -rf "$BASE/remotes"; mkdir -p "$BASE/remotes"
  mkbare mem-personal content
  mkdir -p "$STUB_DIR/ghstate"
  echo '{"private": true, "visibility": "private"}' >"$STUB_DIR/ghstate/mem-personal.json"
  for n in mem-infra-homelab mem-project-code-workspace mem-project-ownproj; do mkbare "$n"; done
  : >"$H/.gitconfig"
  for n in mem-personal mem-infra-homelab mem-project-code-workspace mem-project-ownproj; do
    git config --file "$H/.gitconfig" "url.$BASE/remotes/$n.git.insteadOf" "git@gh-$n:$ORG/$n.git"
  done
  git config --file "$H/.gitconfig" "url.$BASE/utils.git.insteadOf" "https://example.invalid/claude-utils.git"
}

# runboot <args...>: run the script in the fake HOME; sets RC and OUT.
runboot() {
  set +e
  OUT="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" GIT_CONFIG_NOSYSTEM=1 \
    CLAUDE_UTILS_URL="https://example.invalid/claude-utils.git" \
    bash "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --deny-owner Acme-Client --deny-term acmeclient ${ORGARGS[@]+"${ORGARGS[@]}"} "$@" </dev/null 2>&1)"
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
check "private repo created with --private" has "$STUB_DIR/gh.log" 'repo create Exploded-views/mem-infra-homelab --private'
check "existing mem-personal not re-created" hasnt "$STUB_DIR/gh.log" 'repo create Exploded-views/mem-personal'
check "deploy key added with host title" has "$STUB_DIR/gh.log" 'deploy-key add .*mem-infra-homelab_ed25519.pub -R Exploded-views/mem-infra-homelab -w -t testhost'
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
check "init has age recovery recipient and private-only" has "$STUB_DIR/memsync.log" 'init .*--recipient age1d6jhrdfc0q3symtxzs6ay8efyvnx7g4mln9jvxckzstkaz4re9vqysptau.*--private-only'
check "home pinned to infra-homelab" has "$STUB_DIR/memsync.log" 'route home infra-homelab --yes'
check "claude plugin calls" bash -c "grep -q 'plugin marketplace add .*claude-utils-$SHA' '$STUB_DIR/claude.log' && grep -q 'plugin install memory-notices@claude-utils' '$STUB_DIR/claude.log'"
check "service install/start/status/doctor" bash -c "grep -q 'memsync install' '$STUB_DIR/memsync.log' && grep -q 'memsync start' '$STUB_DIR/memsync.log' && grep -q 'doctor --org Exploded-views' '$STUB_DIR/memsync.log'"
check "binaries symlinked into ~/.local/bin" [ -L "$H/.local/bin/claude-memsync" ]
check "no client/orch names anywhere" bash -c "! grep -rIiE 'mem-(client|orch)|orch-' '$H/.config' '$H/.ssh/config' '$STUB_DIR/gh.log' '$STUB_DIR/memsync.log'"
check "dummy token never on disk" bash -c "! grep -rIl -- '$TOKEN' '$H' '$STUB_DIR/gh.log' '$STUB_DIR/memsync.log' '$STUB_DIR/claude.log'"
check "output has no private key material" bash -c "! printf '%s' \"\$1\" | grep -q 'BEGIN OPENSSH'" _ "$OUT"
if [ "$HAVE_CLI" -eq 1 ]; then
  check "join request issued for existing repo" has "$STUB_DIR/memsync.log" 'join request --repo personal --label testhost'
  check "init passes the org token file" has "$STUB_DIR/memsync.log" 'init .*--token-file .*/orgtoken'
else
  pending "join request (CLI in this tree lacks join; asserted against the stub only)"
  pending "init --token-file/--token-cmd (CLI in this tree lacks it; asserted against the stub only)"
  has "$STUB_DIR/memsync.log" 'join request --repo personal --label testhost' && ok "stub saw: join request --repo personal --label testhost"
fi
check "join accept one-liner printed" bash -c "printf '%s' \"\$1\" | grep -q 'claude-memsync join accept testhost --repo personal --fingerprint 0123456789abcdef'" _ "$OUT"

echo "--- homelab idempotent re-run"
K1="$(ssh-keygen -lf "$H/.ssh/mem-personal_ed25519.pub")"
INITS1="$(grep -c ' init ' "$STUB_DIR/memsync.log")"; CREATES1="$(grep -c 'repo create' "$STUB_DIR/gh.log")"; DK1="$(grep -c deploy-key "$STUB_DIR/gh.log")"
runboot --host testhost --role homelab --yes
check "re-run exits 0" [ "$RC" -eq 0 ]
check "re-run keeps keys" [ "$K1" = "$(ssh-keygen -lf "$H/.ssh/mem-personal_ed25519.pub")" ]
check "re-run does not init again" [ "$INITS1" = "$(grep -c ' init ' "$STUB_DIR/memsync.log")" ]
check "re-run does not create repos again" [ "$CREATES1" = "$(grep -c 'repo create' "$STUB_DIR/gh.log")" ]
check "re-run does not add deploy keys again" [ "$DK1" = "$(grep -c deploy-key "$STUB_DIR/gh.log")" ]

echo "--- dry run"
newhome dry
runboot --host testhost --role homelab --dry-run
check "dry run exits 0" [ "$RC" -eq 0 ]
check "dry run creates nothing" bash -c "[ -z \"\$(find '$H' -mindepth 1 -not -name .gitconfig | head -1)\" ]"
check "dry run makes no repo or key calls" hasnt "$STUB_DIR/gh.log" 'repo (create|deploy-key)'

echo "--- org scope"
newhome orgs
STUB_ORGS="$(printf 'Exploded-views\nAcme-Client\nSomeOther')" runboot --host testhost --role homelab --yes
check "extra orgs: warns and proceeds" [ "$RC" -eq 0 ]
check "banner names every extra org and the client org" bash -c "printf '%s' \"\$1\" | grep -q \"WARNING: this host's gh login can reach Acme-Client, SomeOther. An agent host holds an owner-level token with client-org reach.\" && printf '%s' \"\$1\" | grep -q 'CLIENT ORG(S) reachable through the gh login: Acme-Client'" _ "$OUT"
check "banner in plan and again in summary" bash -c "[ \$(printf '%s' \"\$1\" | grep -c \"WARNING: this host's gh login can reach\") -ge 3 ]" _ "$OUT"
check "summary repeats the warning" bash -c "printf '%s' \"\$1\" | sed -n '/== j. Summary/,\$p' | grep -q 'WARNING: this host'" _ "$OUT"
check "zero gh calls against any other org" bash -c "! grep -v 'user/orgs' '$STUB_DIR/gh.log' | grep -iE 'acme-client|someother'; [ \$(grep -E 'repo|api repos' '$STUB_DIR/gh.log' | grep -vc 'Exploded-views') -eq 0 ]"
newhome orgs2
STUB_ORGS="$(printf 'Exploded-views\nSomeOther')" runboot --host testhost --role homelab --yes --allow-org someother
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
STUB_ORGS="$(printf 'Exploded-views\nAcme-Client')" runboot --host testhost --role homelab --yes
check "ambient login sees client org: its own banner line" bash -c "printf '%s' \"\$1\" | grep -q \"WARNING: this host's gh login can reach Acme-Client\"" _ "$OUT"
check "org token cannot reach it: no org-token banner" bash -c "! printf '%s' \"\$1\" | grep -q \"this host's org token can reach\"" _ "$OUT"
check "ambient banner repeated in summary" bash -c "printf '%s' \"\$1\" | sed -n '/== j. Summary/,\$p' | grep -q \"gh login can reach Acme-Client\"" _ "$OUT"
check "every org operation used the org token" bash -c "! grep -E '^(repo|api repos)' '$STUB_DIR/gh.who' | grep -v '|org'"
check "ambient login used only for auth status and its own org listing" bash -c "[ \$(grep -c '|ambient' '$STUB_DIR/gh.who') -le 2 ] && ! grep '|ambient' '$STUB_DIR/gh.who' | grep -E '^(repo|api repos)'"
check "memsync got --token-file, not a token" has "$STUB_DIR/memsync.log" 'doctor --org Exploded-views --token-file .*/orgtoken'
check "org token not in any file, log or output" bash -c "! grep -rIl -- '$ORGTOK' '$H' '$STUB_DIR/gh.log' '$STUB_DIR/gh.who' '$STUB_DIR/memsync.log' '$STUB_DIR/claude.log' && ! printf '%s' \"\$1\" | grep -qF -- '$ORGTOK'" _ "$OUT"
check "org token absent from every non-gh process env" bash -c "[ \$(grep -v ':0\$' '$STUB_DIR/env.log' | wc -l) -eq 0 ]"

newhome ot2
STUB_ORGS="$(printf 'Exploded-views\nAcme-Client')" STUB_ORG_ORGS="$(printf 'Exploded-views\nSomeOther')" runboot --host testhost --role homelab --yes
check "two distinct warnings" bash -c "printf '%s' \"\$1\" | grep -q \"gh login can reach Acme-Client\" && printf '%s' \"\$1\" | grep -q \"org token can reach SomeOther\"" _ "$OUT"
check "summary repeats both" bash -c "printf '%s' \"\$1\" | sed -n '/== j. Summary/,\$p' | grep -c 'WARNING: this host' | grep -qx 2" _ "$OUT"
check "zero gh calls against any other org" bash -c "! grep -v 'user/orgs' '$STUB_DIR/gh.log' | grep -iE 'acme-client|someother'"

newhome ot3
ORGARGS=(--org-token-cmd "secret-tool read vault://vault/item/field")
runboot --host testhost --role homelab --yes
check "cmd variant runs" [ "$RC" -eq 0 ]
check "cmd variant: secret-tool run without a shell, memsync gets --token-cmd" bash -c "grep -q 'secret-tool read vault://vault/item/field' '$STUB_DIR/tool.log' && grep -q 'doctor --org Exploded-views --token-cmd secret-tool read vault://vault/item/field' '$STUB_DIR/memsync.log'"
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
ORGARGS=(--org-token-file "$BASE/does-not-exist")
runboot --host testhost --role homelab --yes
check "unreadable org token stops clearly" bash -c "[ $RC -ne 0 ] && printf '%s' \"\$1\" | grep -q 'cannot read the org token'" _ "$OUT"
ORGARGS=(--org-token-file "$BASE/orgtoken")

echo "--- deny flags"
newhome nodeny
set +e
NODENY="$(HOME="$H" PATH="$STUBS:$PATH" STUB_DIR="$STUB_DIR" STUB_TOKEN="$TOKEN" STUB_ORG_TOKEN="$ORGTOK" GIT_CONFIG_NOSYSTEM=1 \
  bash "$ROOT/scripts/bootstrap.sh" --sha "$SHA" --repo-url "https://example.invalid/other.git" ${ORGARGS[@]+"${ORGARGS[@]}"} --host testhost --role homelab --dry-run </dev/null 2>&1)"
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
check "only the own repo gets a mem-project repo" bash -c "grep 'repo create' '$STUB_DIR/gh.log' | grep -c 'mem-project-' | grep -qx 2 && grep -q 'create Exploded-views/mem-project-ownproj --private' '$STUB_DIR/gh.log' && grep -q 'create Exploded-views/mem-project-code-workspace --private' '$STUB_DIR/gh.log' && ! grep -qE 'thirdlib|clientapp' '$STUB_DIR/gh.log'"
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
  HOME='$H' PATH='$STUBS:$PATH' STUB_DIR='$STUB_DIR' STUB_TOKEN=x STUB_ORG_TOKEN=$ORGTOK CLAUDE_UTILS_URL=https://example.invalid/claude-utils.git bash '$ROOT/scripts/bootstrap.sh' --sha $SHA --org-token-file '$BASE/orgtoken' --host m2 --role personal-dev --workspace '$H/code' </dev/null >/dev/null 2>&1; [ \$? -ne 0 ]"

echo
echo "passed=$PASS failed=$FAIL pending=$PENDING"
[ "$FAIL" -eq 0 ]
