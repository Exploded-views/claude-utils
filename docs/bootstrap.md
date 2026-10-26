# Host bootstrap

`scripts/bootstrap.sh` sets up claude-memsync on a new host in one pass. It is meant to be read before it runs, and `--dry-run` prints every step without doing it.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | error (a missing prerequisite, a refused input, a failed step) |
| 2 | usage (an unknown or malformed argument) |
| 3 | paused: it needs a human, then run the same command again (today: the `--check-key-file` does not exist yet; the output says exactly what to do) |

## Prerequisites

Checked first, before any change, and also in `--dry-run`: one `missing prerequisite: <tool> (<why>)` line per missing item on stderr, then exit 1 with nothing changed. The list: bash 3.2 or later (macOS's `/bin/bash` is enough), the basic tools the script calls (`sed`, `awk`, `grep`, `find`, `stat` and friends), `git` 2.26 or later, `python3`, `ssh-keygen`, `ssh-keyscan`, `gh`, `systemctl` on Linux or `launchctl` on macOS (the service step), and, only when building from source, Go at or above the `go.mod` version (or docker on Linux; on macOS `brew install go`). With `--prebuilt-dir` Go is not needed.

## How the paste prompts are written

Every flow below has the same shape, so a value is written once and used by name:

1. `INPUTS`: one `NAME = <placeholder>` per value you fill in, with a one-line description. Values that are optional say so and default to empty.
2. `ALREADY SET, FOR REFERENCE`: values the prompt derives itself. No action needed.
3. `STEPS`: they refer to the names only, as shell variables (`"$ORG"`). A long value (the recovery key is about 1,960 characters) is set once, in its name.
4. The command that runs twice is defined once as the shell function `boot`. The dry run comes first; after you say go, the same command with `--yes` instead of `--dry-run`.

Run the steps in **bash**, not zsh: the keep-out lists are space-separated and are split into repeated flags by bash's word splitting, which zsh does not do. (On macOS run `bash` first.)

`scripts/bootstrap_test.sh` extracts each prompt from this file, substitutes test values for the inputs, and runs it as a shell snippet, so this file and the script cannot drift: a flag the script does not know, an input the steps do not use, or a name the steps use but INPUTS does not list fails the test.

### Keep-out lists (optional, repeatable)

On a laptop there may be work to keep out of the memory repos: a client, an employer, or just a separate project. Both lists are optional; leave `KEEP_OUT_OWNERS` and `KEEP_OUT_TERMS` empty for none (the script then prints `keep-out lists: none (routing alone decides what syncs)`, which is a note, not a warning).

| Flag | What it does |
|---|---|
| `--keep-out-owner <owner>` (repeatable; old name `--deny-owner`) | A discovered git checkout whose origin owner matches (case-insensitive) gets no repo and no route, so default-deny applies. The owner is also flagged in the token-reach warning if the gh login or the org token can reach that org. |
| `--keep-out-term <term>` (repeatable; old name `--deny-term`) | A checkout whose `owner/repo` contains the term (case-insensitive substring) is skipped the same way. Each term is also written, regex-escaped, to every `denylist-<kind>.txt`. At commit time the daemon uses that file to hold back any note whose content or repo-relative path matches: the note stays local, is never committed or pushed, and is logged without its path. |

Keep-out owners are **not** written to the denylist. Only terms are.

Placeholders below are `<angle brackets>`. Put real values only in the session you paste into, never in this repository.

## The paste prompts

Paste one into a Claude Code session on the new host after replacing every `<placeholder>` in INPUTS.

| Input | Value |
|---|---|
| `COMMIT` | the full 40-hex commit of this repo to install |
| `HOST_LABEL` | the host's label: 1-32 of `a-z`, `0-9`, `-` |
| `ORG` | the GitHub organisation that holds the memory repos (required; there is no default) |
| `RECOVERY` | your own offline post-quantum recovery PUBLIC key, `age1pq1...` (required; see "Make a recovery key") |
| `KEEP_OUT_OWNERS`, `KEEP_OUT_TERMS` | optional space-separated lists, see above |
| `ADMIN_APP_ID` | numeric App ID of `memsync-admin` |
| `ADMIN_KEY_CMD` | command that prints the `memsync-admin` private key (reads the owner's password manager at run time) |
| `CHECK_APP_ID` | numeric App ID of `memsync-check` |
| `CHECK_KEY_FILE` | absolute path of this host's own `memsync-check` private key, mode 0600 (see below) |

The admin entry is added to `apps.json` for the run only and removed when the script exits. The check key stays: the daemon's hourly private-only re-check mints from it. If the check key file is not there yet the script pauses with exit 3 and prints how to create it (GitHub, Settings, Developer settings, GitHub Apps, `memsync-check`, "Generate a private key"; copy the `.pem` to the path; `chmod 600`; delete the download); do that and run the same command again.

### (a) Homelab host

<!-- prompt: homelab-app -->
```text
Set up claude-memsync on this host from github.com/Exploded-views/claude-utils (role homelab). Use bash, not zsh.

INPUTS (fill these in; nothing else changes)
- COMMIT = <commit>
    the full 40-hex commit to install
- HOST_LABEL = <host-label>
    this host's label
- ORG = <org>
    the GitHub organisation that holds the memory repos
- RECOVERY = <recovery-recipient>
    my offline post-quantum recovery PUBLIC key (age1pq1..., about 1,960 characters)
- KEEP_OUT_OWNERS = ""
    (optional; leave empty for none) space-separated GitHub owners to keep out
- KEEP_OUT_TERMS = ""
    (optional; leave empty for none) space-separated terms to keep out of repo names and notes
- ADMIN_APP_ID = <admin-app-id>
    numeric App ID of memsync-admin
- ADMIN_KEY_CMD = <admin-key-cmd>
    command that prints the memsync-admin private key (never run it yourself)
- CHECK_APP_ID = <check-app-id>
    numeric App ID of memsync-check
- CHECK_KEY_FILE = <check-key-file>
    absolute path of this host's memsync-check key (mode 0600)

ALREADY SET, FOR REFERENCE (no action needed)
- REPO = https://github.com/Exploded-views/claude-utils
- SRC = ~/.local/src/claude-utils-$COMMIT

STEPS
1. git clone "$REPO" "$SRC" && git -C "$SRC" checkout "$COMMIT"
2. Read "$SRC/scripts/bootstrap.sh" in full before running anything.
3. Define this once:
boot() {
  local a=() x
  for x in $KEEP_OUT_OWNERS; do a+=(--keep-out-owner "$x"); done
  for x in $KEEP_OUT_TERMS; do a+=(--keep-out-term "$x"); done
  bash "$SRC/scripts/bootstrap.sh" --host "$HOST_LABEL" --role homelab --sha "$COMMIT" \
    --org "$ORG" --recovery-recipient "$RECOVERY" ${a[@]+"${a[@]}"} \
    --org-app memsync-admin --org-app-id "$ADMIN_APP_ID" --org-app-key-cmd "$ADMIN_KEY_CMD" \
    --check-app memsync-check --check-app-id "$CHECK_APP_ID" --check-key-file "$CHECK_KEY_FILE" "$@"
}
4. Dry run, and show me the plan:
boot --dry-run
5. After I say go, the same command with --yes instead of --dry-run:
boot --yes
Stop and tell me before any step that needs me (an approval prompt, a pause with exit 3, a pending join). Never print private keys or tokens, and do not read the key file or run the key command yourself.
```

### (b) Personal-dev macOS host

<!-- prompt: personal-dev-app -->
```text
Set up claude-memsync on this host from github.com/Exploded-views/claude-utils (role personal-dev). Use bash, not zsh.

INPUTS (fill these in; nothing else changes)
- COMMIT = <commit>
    the full 40-hex commit to install
- HOST_LABEL = <host-label>
    this host's label
- ORG = <org>
    the GitHub organisation that holds the memory repos
- RECOVERY = <recovery-recipient>
    my offline post-quantum recovery PUBLIC key (age1pq1..., about 1,960 characters)
- GITHUB_USER = <github-user>
    my personal GitHub account; checkouts it owns get a mem-project repo
- WORKSPACES = ""
    (optional; leave empty for none) space-separated folders (no spaces in the paths) that hold code checkouts; each is pinned to the workspace repo
- KEEP_OUT_OWNERS = ""
    (optional; leave empty for none) space-separated GitHub owners to keep out
- KEEP_OUT_TERMS = ""
    (optional; leave empty for none) space-separated terms to keep out of repo names and notes
- ADMIN_APP_ID = <admin-app-id>
    numeric App ID of memsync-admin
- ADMIN_KEY_CMD = <admin-key-cmd>
    command that prints the memsync-admin private key (never run it yourself)
- CHECK_APP_ID = <check-app-id>
    numeric App ID of memsync-check
- CHECK_KEY_FILE = <check-key-file>
    absolute path of this host's memsync-check key (mode 0600)

ALREADY SET, FOR REFERENCE (no action needed)
- REPO = https://github.com/Exploded-views/claude-utils
- SRC = ~/.local/src/claude-utils-$COMMIT

STEPS
1. git clone "$REPO" "$SRC" && git -C "$SRC" checkout "$COMMIT"
2. Read "$SRC/scripts/bootstrap.sh" in full before running anything.
3. Define this once:
boot() {
  local a=() x
  for x in $KEEP_OUT_OWNERS; do a+=(--keep-out-owner "$x"); done
  for x in $KEEP_OUT_TERMS; do a+=(--keep-out-term "$x"); done
  for x in $WORKSPACES; do a+=(--workspace "$x"); done
  bash "$SRC/scripts/bootstrap.sh" --host "$HOST_LABEL" --role personal-dev --sha "$COMMIT" \
    --org "$ORG" --own-owner "$GITHUB_USER" --recovery-recipient "$RECOVERY" ${a[@]+"${a[@]}"} \
    --org-app memsync-admin --org-app-id "$ADMIN_APP_ID" --org-app-key-cmd "$ADMIN_KEY_CMD" \
    --check-app memsync-check --check-app-id "$CHECK_APP_ID" --check-key-file "$CHECK_KEY_FILE" "$@"
}
4. Dry run, and show me the plan:
boot --dry-run
5. After I say go, the same command with --yes instead of --dry-run:
boot --yes
Stop and tell me before any step that needs me (an approval prompt, a pause with exit 3, a pending join). Never print private keys or tokens, and do not read the key file or run the key command yourself.
```

### (c) Homelab host without a password manager (token file)

A homelab box such as an always-on home server often has no password manager, so the admin key is never put on it. Instead the owner mints a short-lived admin token **on their own machine** and copies it over as a 0600 file; the script reads it, uses it for the run, and deletes it (`--delete-org-token-file`).

On your own machine (where the `memsync-admin` entry and its key live), mint the token and copy it over. The token expires in about an hour, so do this just before you paste the prompt; a token printed to the terminal is never needed, so write it straight to the file:

```text
umask 077
claude-memsync app-token --app memsync-admin --org <org> > /tmp/org-token
scp /tmp/org-token <host>:<token-file>      # lands as a 0600 file owned by you on the host
shred -u /tmp/org-token                      # or rm -P on macOS
```

On the host the file must be mode 0600, owned by you, with one hard link, and not a symlink; otherwise the script refuses and leaves it alone (check with `ls -l <token-file>`; if the mode is wrong, `chmod 600` it). The script reads it, never prints it and never copies it, and deletes it on every exit path of a real run (success, error, or a signal) with `shred -u`, or by overwriting and removing it where `shred` is missing. The same token serves the dry run and the real run: only the real run deletes it. It expires in about an hour, so if the review takes longer, mint and copy a new one before saying go. If the check key is not there yet the script pauses (exit 3) before it touches the token file.

<!-- prompt: homelab-tokenfile -->
```text
Set up claude-memsync on this host from github.com/Exploded-views/claude-utils (role homelab), using an org token file I copied here. Use bash, not zsh.

INPUTS (fill these in; nothing else changes)
- COMMIT = <commit>
    the full 40-hex commit to install
- HOST_LABEL = <host-label>
    this host's label
- ORG = <org>
    the GitHub organisation that holds the memory repos
- RECOVERY = <recovery-recipient>
    my offline post-quantum recovery PUBLIC key (age1pq1..., about 1,960 characters)
- KEEP_OUT_OWNERS = ""
    (optional; leave empty for none) space-separated GitHub owners to keep out
- KEEP_OUT_TERMS = ""
    (optional; leave empty for none) space-separated terms to keep out of repo names and notes
- TOKEN_FILE = <token-file>
    absolute path of the 1-hour org admin token I copied here (mode 0600); the script deletes it
- CHECK_APP_ID = <check-app-id>
    numeric App ID of memsync-check
- CHECK_KEY_FILE = <check-key-file>
    absolute path of this host's memsync-check key (mode 0600)

ALREADY SET, FOR REFERENCE (no action needed)
- REPO = https://github.com/Exploded-views/claude-utils
- SRC = ~/.local/src/claude-utils-$COMMIT

STEPS
1. git clone "$REPO" "$SRC" && git -C "$SRC" checkout "$COMMIT"
2. Read "$SRC/scripts/bootstrap.sh" in full before running anything.
3. Define this once:
boot() {
  local a=() x
  for x in $KEEP_OUT_OWNERS; do a+=(--keep-out-owner "$x"); done
  for x in $KEEP_OUT_TERMS; do a+=(--keep-out-term "$x"); done
  bash "$SRC/scripts/bootstrap.sh" --host "$HOST_LABEL" --role homelab --sha "$COMMIT" \
    --org "$ORG" --recovery-recipient "$RECOVERY" ${a[@]+"${a[@]}"} \
    --org-token-file "$TOKEN_FILE" --delete-org-token-file \
    --check-app memsync-check --check-app-id "$CHECK_APP_ID" --check-key-file "$CHECK_KEY_FILE" "$@"
}
4. Dry run, and show me the plan (the token file is kept; it expires about an hour after I minted it, so tell me if this takes longer and I will copy a new one):
boot --dry-run
5. After I say go, the same command with --yes instead of --dry-run:
boot --yes
Stop and tell me before any step that needs me (a pause with exit 3, a pending join). Never print or read the token file or any key.
```

### (d) Crewkit host: a downloaded release, no clone and no Go

The release archives carry `scripts/bootstrap.sh`. A host that should not build anything downloads the archive, verifies it against `SHA256SUMS`, extracts it and runs the script with `--prebuilt-dir` pointing at the extracted folder: the binaries in it are installed, nothing is cloned or built, and Go is not needed. The version the binary reports (the tag) must be release-shaped, or match `--sha` if you pass one. There is no source checkout, so the plugin step is skipped and the summary says how to add it later.

<!-- prompt: crewkit -->
```text
Set up claude-memsync on this host from a release archive of github.com/Exploded-views/claude-utils (role homelab); no clone, no build. Use bash, not zsh.

INPUTS (fill these in; nothing else changes)
- TAG = <tag>
    the release tag, vX.Y.Z
- PLATFORM = <os-arch>
    linux-amd64, linux-arm64, darwin-amd64 or darwin-arm64
- HOST_LABEL = <host-label>
    this host's label
- ORG = <org>
    the GitHub organisation that holds the memory repos
- RECOVERY = <recovery-recipient>
    my offline post-quantum recovery PUBLIC key (age1pq1..., about 1,960 characters)
- KEEP_OUT_OWNERS = ""
    (optional; leave empty for none) space-separated GitHub owners to keep out
- KEEP_OUT_TERMS = ""
    (optional; leave empty for none) space-separated terms to keep out of repo names and notes
- TOKEN_FILE = <token-file>
    absolute path of the 1-hour org admin token I copied here (mode 0600); the script deletes it
- CHECK_APP_ID = <check-app-id>
    numeric App ID of memsync-check
- CHECK_KEY_FILE = <check-key-file>
    absolute path of this host's memsync-check key (mode 0600)

ALREADY SET, FOR REFERENCE (no action needed)
- REPO = https://github.com/Exploded-views/claude-utils
- NAME = claude-utils-$TAG-$PLATFORM
- WORK = ~/.local/src/claude-utils-release-$TAG
- X = $WORK/$NAME

STEPS
1. mkdir -p "$WORK" && cd "$WORK" && curl -fLO "$REPO/releases/download/$TAG/$NAME.tar.gz" -O "$REPO/releases/download/$TAG/SHA256SUMS"
2. Verify before extracting (it must print OK for the archive; on macOS use shasum -a 256 -c):
   sha256sum -c --ignore-missing SHA256SUMS
3. tar -xzf "$NAME.tar.gz", then read "$X/scripts/bootstrap.sh" in full before running anything.
4. Define this once:
boot() {
  local a=() x
  for x in $KEEP_OUT_OWNERS; do a+=(--keep-out-owner "$x"); done
  for x in $KEEP_OUT_TERMS; do a+=(--keep-out-term "$x"); done
  bash "$X/scripts/bootstrap.sh" --prebuilt-dir "$X" --host "$HOST_LABEL" --role homelab \
    --org "$ORG" --recovery-recipient "$RECOVERY" ${a[@]+"${a[@]}"} \
    --org-token-file "$TOKEN_FILE" --delete-org-token-file \
    --check-app memsync-check --check-app-id "$CHECK_APP_ID" --check-key-file "$CHECK_KEY_FILE" "$@"
}
5. Dry run, and show me the plan (the token file is kept; it expires about an hour after I minted it, so tell me if this takes longer and I will copy a new one):
boot --dry-run
6. After I say go, the same command with --yes instead of --dry-run:
boot --yes
Stop and tell me before any step that needs me (a pause with exit 3, a pending join). Never print or read the token file or any key.
```

## Make a recovery key (once, on the owner's own machine)

Every repo is encrypted to this host and to one offline recovery key, so the data stays readable if every host is lost. Repos are post-quantum by default (hybrid ML-KEM-768 + X25519 age keys), so the recovery key must be a hybrid `age1pq1...` key. Generate it on your own machine, never on an agent host, and store the secret in your password manager straight from the pipe:

```
go install filippo.io/age/cmd/age-keygen@v1.3.2
age-keygen -pq | <password-manager store command>
<password-manager read command> | age-keygen -y     # prints the age1pq1... line: that is <recovery-recipient>
```

`age-keygen -pq` writes the secret key to stdout, straight into the password manager, and nothing to disk. `age-keygen -y` reads that secret key back from the read command and prints only the public key. Do not rely on `age-keygen` printing the public key on stderr (it does not on every setup), and do not use `-o /dev/stdout`: `age-keygen` opens `-o` files exclusively and refuses an existing path. The public key is about 1,960 characters long; pass it whole.

A repo created before this change is classic (X25519). Migrate it with `claude-memsync recipients rekey-pq`; see `docs/claude-memsync.md`. `init --encryption-scheme classic` keeps the upstream format.

## Register the GitHub Apps (once, on the owner's Mac)

Build the binary from a clone of this repo, then register both apps in the org. Each `register` prints a local file to open in a browser where you are signed in as an org owner, waits for GitHub's callback, and records the app in `~/.config/claude-memsync/apps.json`.

```
git clone https://github.com/Exploded-views/claude-utils ~/.local/src/claude-utils-<commit>
git -C ~/.local/src/claude-utils-<commit> checkout <commit>
cd ~/.local/src/claude-utils-<commit> && go build -o ~/.local/bin/claude-memsync ./cmd/claude-memsync
```

1. `memsync-admin` (repository Administration: write, Metadata: read). The key goes straight into the password manager and is never written to a file; `--key-cmd-store` reads the key on stdin, `--key-cmd` is the command that prints it back (use that as `<admin-key-cmd>`):

   ```
   claude-memsync app register --name memsync-admin --org <org> --url <repo-url> \
     --perm administration:write --perm metadata:read \
     --key-cmd-store "<store cmd>" --key-cmd "<read cmd>"
   ```

   Then install the app on **all repositories** of the org (the page `register` prints). The Mac's own `apps.json` entry keeps the `keyCmd`; the bootstrap adds the same kind of entry on each host and removes it again.

2. `memsync-check` (Metadata: read only). One key file for the Mac:

   ```
   claude-memsync app register --name memsync-check --org <org> --url <repo-url> \
     --perm metadata:read --key-out ~/.config/claude-memsync/memsync-check.pem
   ```

   Install it on all repositories too. Note its App ID (`<check-app-id>`).

   If `register` fails to store or write the key after the app was created, it saves the key to a new `~/.config/claude-memsync/<name>.recovered.pem` (mode 0600; a numeric suffix is added if that name is taken), prints that path, writes the `apps.json` entry (`appId`, `org`, and `keyFile` pointing at the recovered file) and exits 1. Store the key where it belongs, then delete the recovered file and fix the entry. The key itself is never printed. `register`, `app add` and `app token` also refuse (exit 2, before creating anything) a flag value that still holds a literal `<placeholder>`.

   For an app that already exists (created by hand, or recovered as above), record it without registering again:

   ```
   claude-memsync app add --name memsync-admin --app-id <admin-app-id> --org <org> \
     (--key-cmd "<read cmd>" | --key-file <key-file>) [--api-url <api-url>] [--replace] [--verify]
   ```

   The key file must be mode 0600, yours, with one hard link. The key fingerprint is recorded when the key is readable. An existing entry is refused unless `--replace`. `--verify` mints a token against the org and prints only `MINT-OK`; it runs before anything is written, so a failed verify (with `--replace` too) leaves `apps.json` and the old entry as they were.

3. One private key per host for `memsync-check`: GitHub, Settings, Developer settings, GitHub Apps, `memsync-check`, Generate a private key. The browser downloads a `.pem`. Copy it to the host as `<check-key-file>` (for example `~/.config/claude-memsync/memsync-check.pem`), then on the host `chmod 600` it. It must be owned by you with one hard link, or the script refuses it. Delete the download. A lost or leaked key is revoked on that same page without touching the other hosts.

`claude-memsync app register -h` lists the flags (`--api-url` and `--github-url` for GitHub Enterprise).

## Usage

```
scripts/bootstrap.sh --host <label> --role <role> --org <org> --recovery-recipient <age1pq1...>
                     [--sha <commit>] [--prebuilt-dir <dir>]
                     [--workspace <dir>]... [--own-owner <owner>]...
                     [--keep-out-owner <owner>]... [--keep-out-term <term>]...
                     [--repo-url <url>]
                     --org-token-file <path> [--delete-org-token-file]
                       | --org-token-cmd "<argv>" | --org-app <name>
                     [--org-app-id <id> --org-app-key-cmd "<argv>"]
                     [--check-app <name> --check-app-id <id> --check-key-file <path>]
                     [--dry-run] [--yes]
```

`--org-app-id` with `--org-app-key-cmd` adds the admin app's `apps.json` entry (a key command, never a key file) for the run and removes it when the script exits, whether it succeeded or not. An existing entry of that name is never replaced: the script refuses instead. `--check-app` needs `--check-app-id` and `--check-key-file`; see step 7b. If the check key file does not exist yet, the script exits 3 (paused) right after the prerequisite checks, before anything is changed, and prints how to create it.

`--delete-org-token-file` is valid only with `--org-token-file <path>` (otherwise exit 2). The file must be absolute, a plain file (not a symlink), mode 0600, owned by you, with one hard link, or the script refuses and leaves it alone. The token is read from it as before, never printed and never copied anywhere else; on every exit path (success, error, a signal) an exit trap deletes the file with `shred -u`, or overwrites it and removes it where `shred` is not installed. A `--dry-run` never deletes it (a dry run changes nothing); it prints `dry run: the org token file would be deleted on a real run`, so the same token serves the dry run and the real run.

`--prebuilt-dir <dir>` installs `<dir>/claude-memsync` and `<dir>/claude-memmerge` into `~/.local/lib/claude-memsync/<sha-or-version>/` (and links them into `~/.local/bin`) and skips the clone and the build, so Go is not a prerequisite. The script refuses if the directory or either binary is missing or not executable, or if `<dir>/claude-memsync version` is not what is expected: it must contain the first 12 hex characters of `--sha` when `--sha` is given; without `--sha` it must be a release version (`vX.Y.Z`, what `release.yml` stamps into the binaries), and that version names the install directory. An existing, different binary in that directory is never overwritten. Without a source checkout the plugin step is skipped and the summary says how to add it.

`--keep-out-owner` and `--keep-out-term` are optional and repeatable (see "Keep-out lists" above); `--deny-owner` and `--deny-term` are their old names and still work.

Roles (table at the top of the script):

| Role | Repos | Home folder |
|---|---|---|
| `homelab` | `mem-personal`, `mem-infra-homelab` | pinned to infra-homelab |
| `personal-dev` | `mem-personal`, `mem-project-code-workspace`, one `mem-project-<name>` per own checkout | pinned to personal |

Unknown roles are refused. No `mem-client-*` or `mem-orch-*` repo can be created or joined by this script.

`personal-dev` also takes `--workspace <dir>` (repeatable; prompted for on a terminal). Each workspace folder that is not itself a git repo is pinned to `mem-project-code-workspace`. Git checkouts directly under a workspace and under `~` (depth 1 to 2) are discovered; those whose origin owner is an own owner (the `--org` org, plus each `--own-owner`; there are no built-in defaults, so pass `--own-owner <github-user>` for your personal GitHub account) get a `mem-project-<repo>` repo and a remote route. Other owners are listed as third-party and get nothing. Owners in `--keep-out-owner` (repeatable, no default, case-insensitive) are refused, as is any remote whose owner/repo contains a `--keep-out-term`. With neither flag the script prints the note `keep-out lists: none (routing alone decides what syncs)` and continues. Folders that are neither pinned nor matched are denied.

## Steps

1. Prerequisites (see above; before anything changes, also in `--dry-run`), then preflight: gh logged in. Refuses if a sync dir it did not create exists, or if the old memsync is configured or its timer is active.
1a. Org token: one of `--org-token-file`, `--org-token-cmd` (shell-style quotes, no shell) or `--org-app <name>` is required. `--org-app` (with `--org-app-id`/`--org-app-key-cmd`, or an existing `apps.json` entry) runs `claude-memsync app-token --app <name>` (the GitHub App entry in `apps.json`) for every org call, and hands `init` and `doctor` that same command as `--token-cmd`; the binary is the one built in step 2, so the token's org reach is checked right after the build, before any repo or key is created. Every org operation (gh repo create, gh api, deploy-key add, init, doctor) uses it; for gh it is set as `GH_TOKEN` on that one command only. The ambient gh login is not used for the org.
1a2. `--host` is the host's label: 1-32 characters of `a-z`, `0-9` and `-`, starting with a letter or digit (checked before anything is created). Every `init` gets `--host-label <label>`, so the first host of a new repo has `# host:<label>` on its recipient line, and the owner's recovery key goes in with `--recovery-recipient` (marked `# recovery` and saved as `recoveryRecipients`, the only recipient `join accept` lets go unlabelled). `--org` equal to a keep-out owner is refused. The script refuses to run under `bash -x` / `SHELLOPTS=xtrace`, which would print the org token. `--org-token-cmd` is split with the same shell-style quoting as claude-memsync (double and single quotes group, nothing is expanded).
1b. Org scope (checked separately for the ambient gh login and for the org token; for `--org-app` the token's reach is the owners of `installation/repositories`): if the gh login can see orgs outside the allow list (the `--org` org alone, with no built-in extras; `--allow-org` adds more), a loud WARNING names them, calls out keep-out owners, and repeats in the plan and the summary. It proceeds, and only ever touches repos in `--org`.
2. Plan: lists the repos to create, keys to add and routes. You type `yes`, or pass `--yes`.
3. Clone the pinned commit (from `--repo-url`, default `https://github.com/Exploded-views/claude-utils`) to `~/.local/src/claude-utils-<sha>`, verify `HEAD`, build `claude-memsync` and `claude-memmerge` into `~/.local/lib/claude-memsync/<sha>/`, symlink into `~/.local/bin`. With `--prebuilt-dir`: install the given binaries instead (see Usage); no clone, no build.
4. Create missing repos with `gh repo create --private`, then require `private: true` and `visibility: private` from the API. Non `mem-*` names are refused.
5. Deploy keys: `~/.ssh/<repo>_ed25519` (no passphrase, 0600), a `Host gh-<repo>` alias on ssh.github.com:443, GitHub's host key only if its fingerprint is `SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU`, `gh repo deploy-key add ... -w -t <host>`, then `git ls-remote`. If the org has deploy keys disabled the script says how to enable them and stops.
6. `routes.json` (0600, default deny, `foreignWriters` for the old memsync) and a `denylist-<kind>.txt` per kind containing each `--keep-out-term`, one per line (terms only, never owners).
7. `claude-memsync init` for each repo (post-quantum age encryption with the `--recovery-recipient` key, archive-deletes, private-only, `--token-cmd "gh auth token"`). A repo that already has content leaves this host without a key, so the script runs `join request` and prints the one-liner a member host runs: `claude-memsync join accept <host> --repo <key> --fingerprint "<8 groups of 4 hex>"`; `claude-memsync join status --repo <key>` shows pending or accepted. The fingerprint is parsed from the `fingerprint: xxxx xxxx xxxx xxxx xxxx xxxx xxxx xxxx` line `join request` prints (128 bits, grouped); a missing or differently shaped line stops the script.
7b. Token split: with `--check-app`, the key file must be absolute, a plain file, mode 0600, owned by you with one hard link; its fingerprint (`claude-memsync app key-fingerprint`) goes into the `apps.json` entry `{name, appId, org, keyFile, apiBase, keyFingerprint}` and, when `routes.json` names a registry, into the registry with `claude-memsync host check-key --fingerprint ... --app <check-app>`; every config gets `githubApp: <check-app>`, so the daemon's hourly re-check mints from this host's own check key. Without `--check-app`, each config gets `recheckTokenCmd: false`, `githubTokenCmd` and any saved org token file are removed, and `githubTokenFile` if `~/.config/claude-memsync/github-token` (0600) exists. Otherwise the hourly re-check is off and a warning says so. The script never writes that file.
8. `claude-memsync route home <key> --yes`.
9. Plugin: `claude plugin marketplace add` of the clone, then `claude plugin install memory-notices@claude-utils`.
10. Service: `install`, `start` (these may need your approval in the session), `status`, `doctor`.
11. Summary: repos, key names, pending joins, next steps. Private keys and tokens are never printed.

A re-run whose plan differs from the `routes.json` an earlier run wrote (a new project or workspace) regenerates it before any repo is created, keeping the old one as `routes.json.bak`, and puts the home pin back. Keep-out terms are written to the denylists as literal regexes (a leading `#` is escaped).

Each completed step leaves a marker under `~/.local/state/claude-memsync-bootstrap/`, so a re-run after a failure resumes and never regenerates keys or re-inits a sync dir.

Related-repo suggestions are not built yet; the script prints a placeholder and calls nothing.

## Tests

`scripts/bootstrap_test.sh` runs the script end to end in a fake HOME with stub `gh`, `systemctl`, `claude`, `go` and `ssh-keyscan` and local bare repos as remotes. `claude-memsync` is the real binary built from the tree behind a recording wrapper: `install`, `start`, `stop` and `doctor` are stubbed (`doctor` needs the live GitHub API) and `init` runs without `--private-only` for the same reason; `app-token` talks to a loopback fake of the GitHub App API. It checks the prerequisite checks (a PATH without each tool, in a dry run and a real run), the exit-3 pause, `--delete-org-token-file` on every exit path (including a signal), `--prebuilt-dir`, the keep-out flags and wording, private-only enforcement, key modes, routes (including a re-run that adds a project), denylists, token handling, quoting (one table shared with the Go tests), xtrace refusal, refusals, the personal-dev discovery rules, an idempotent re-run, the paste prompts above (extracted from this file and run as shell snippets, with empty and two-item keep-out lists), and `recipients rekey-pq --all` end to end (labels on origin's `.age-recipients`, origin's blobs decrypted, a restarted daemon syncing a new note as hybrid ciphertext). Set `MEMSYNC_BIN` and `MEMMERGE_BIN` to use prebuilt binaries; `BT_QUICK=1` stops after the new-flag sections for a fast loop.

`scripts/package-release_test.sh` builds the linux/amd64 release archive and checks its layout, `SHA256SUMS` and the extracted script with `--prebuilt-dir --dry-run`; see `docs/ci.md`.
