# Host bootstrap

`scripts/bootstrap.sh` sets up claude-memsync on a new host in one pass. It is meant to be read before it runs, and `--dry-run` prints every step without doing it.

## The paste prompts

Paste one into a Claude Code session on the new host after replacing every `<placeholder>`:

| Placeholder | Value |
|---|---|
| `<host-label>` | the host's label: 1-32 of `a-z`, `0-9`, `-` |
| `<org>` | the GitHub organisation that holds the memory repos (required; there is no default) |
| `<recovery-recipient>` | your own offline post-quantum recovery PUBLIC key, `age1pq1...` (required; there is no default; see "Make a recovery key") |
| `<commit>` | the full 40-hex commit of this repo to install |
| `<client-org>`, `<client-term>` | the owner's deny list (a client org and a term in client repo names); never kept in this repo |
| `<admin-app-id>` | numeric App ID of `memsync-admin` |
| `<admin-key-cmd>` | command that prints the `memsync-admin` private key (reads the owner's password manager at run time) |
| `<check-app-id>` | numeric App ID of `memsync-check` |
| `<check-key-file>` | absolute path of this host's own `memsync-check` private key, mode 0600 (see below) |

The admin entry is added to `apps.json` for the run only and removed when the script exits. The check key stays: the daemon's hourly private-only re-check mints from it. Put the check key file on the host before pasting.

### (a) Homelab host

```text
Set up claude-memsync on this host from github.com/Exploded-views/claude-utils, commit <commit> (role homelab, label <host-label>).

1. Clone that exact commit into ~/.local/src/claude-utils-<commit> (git clone https://github.com/Exploded-views/claude-utils ~/.local/src/claude-utils-<commit> && git -C ~/.local/src/claude-utils-<commit> checkout <commit>).
2. Read scripts/bootstrap.sh in full before running anything.
3. Run this with --dry-run and show me the plan:
bash ~/.local/src/claude-utils-<commit>/scripts/bootstrap.sh --host <host-label> --role homelab --sha <commit> --org <org> --recovery-recipient <recovery-recipient> --deny-owner <client-org> --deny-term <client-term> --org-app memsync-admin --org-app-id <admin-app-id> --org-app-key-cmd "<admin-key-cmd>" --check-app memsync-check --check-app-id <check-app-id> --check-key-file <check-key-file> --dry-run
4. Then run it for real:
bash ~/.local/src/claude-utils-<commit>/scripts/bootstrap.sh --host <host-label> --role homelab --sha <commit> --org <org> --recovery-recipient <recovery-recipient> --deny-owner <client-org> --deny-term <client-term> --org-app memsync-admin --org-app-id <admin-app-id> --org-app-key-cmd "<admin-key-cmd>" --check-app memsync-check --check-app-id <check-app-id> --check-key-file <check-key-file> --yes
Stop and tell me before any step that needs me (an approval prompt, a missing key file, a pending join). Never print private keys or tokens, and do not read the key file or run the key command yourself.
```

### (b) Personal-dev macOS host

```text
Set up claude-memsync on this host from github.com/Exploded-views/claude-utils, commit <commit> (role personal-dev, label <host-label>).

1. Clone that exact commit into ~/.local/src/claude-utils-<commit> (git clone https://github.com/Exploded-views/claude-utils ~/.local/src/claude-utils-<commit> && git -C ~/.local/src/claude-utils-<commit> checkout <commit>).
2. Read scripts/bootstrap.sh in full before running anything.
3. Run this with --dry-run and show me the plan:
bash ~/.local/src/claude-utils-<commit>/scripts/bootstrap.sh --host <host-label> --role personal-dev --sha <commit> --org <org> --recovery-recipient <recovery-recipient> --deny-owner <client-org> --deny-term <client-term> --org-app memsync-admin --org-app-id <admin-app-id> --org-app-key-cmd "<admin-key-cmd>" --check-app memsync-check --check-app-id <check-app-id> --check-key-file <check-key-file> --dry-run
4. Then run it for real:
bash ~/.local/src/claude-utils-<commit>/scripts/bootstrap.sh --host <host-label> --role personal-dev --sha <commit> --org <org> --recovery-recipient <recovery-recipient> --deny-owner <client-org> --deny-term <client-term> --org-app memsync-admin --org-app-id <admin-app-id> --org-app-key-cmd "<admin-key-cmd>" --check-app memsync-check --check-app-id <check-app-id> --check-key-file <check-key-file> --yes
For each folder of code checkouts add `--workspace <dir>` to both commands (without it no workspace folder is pinned). Stop and tell me before any step that needs me (an approval prompt, a missing key file, a pending join). Never print private keys or tokens, and do not read the key file or run the key command yourself.
```

Tests: `scripts/bootstrap_test.sh` extracts these command lines from this file and runs them, so the doc and the test cannot drift.

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

3. One private key per host for `memsync-check`: GitHub, Settings, Developer settings, GitHub Apps, `memsync-check`, Generate a private key. The browser downloads a `.pem`. Copy it to the host as `<check-key-file>` (for example `~/.config/claude-memsync/memsync-check.pem`), then on the host `chmod 600` it. It must be owned by you with one hard link, or the script refuses it. Delete the download. A lost or leaked key is revoked on that same page without touching the other hosts.

`claude-memsync app register -h` lists the flags (`--api-url` and `--github-url` for GitHub Enterprise).

## Usage

```
scripts/bootstrap.sh --host <label> --role <role> --org <org> --recovery-recipient <age1pq1...> [--sha <commit>]
                     [--workspace <dir>]... [--own-owner <owner>]... [--deny-owner <owner>]...
                     [--deny-term <term>]... [--repo-url <url>]
                     --org-token-file <path> | --org-token-cmd "<argv>" | --org-app <name>
                     [--org-app-id <id> --org-app-key-cmd "<argv>"]
                     [--check-app <name> --check-app-id <id> --check-key-file <path>]
                     [--dry-run] [--yes]
```

`--org-app-id` with `--org-app-key-cmd` adds the admin app's `apps.json` entry (a key command, never a key file) for the run and removes it when the script exits, whether it succeeded or not. An existing entry of that name is never replaced: the script refuses instead. `--check-app` needs `--check-app-id` and `--check-key-file`; see step 7b.

Roles (table at the top of the script):

| Role | Repos | Home folder |
|---|---|---|
| `homelab` | `mem-personal`, `mem-infra-homelab` | pinned to infra-homelab |
| `personal-dev` | `mem-personal`, `mem-project-code-workspace`, one `mem-project-<name>` per own checkout | pinned to personal |

Unknown roles are refused. No client or orchestrator repo (`mem-client-*`, `mem-orch-*`) can be created or joined by this script.

`personal-dev` also takes `--workspace <dir>` (repeatable; prompted for on a terminal). Each workspace folder that is not itself a git repo is pinned to `mem-project-code-workspace`. Git checkouts directly under a workspace and under `~` (depth 1 to 2) are discovered; those whose origin owner is in the allow list (`gitgat`, the org, plus `--own-owner`) get a `mem-project-<repo>` repo and a remote route. Other owners are listed as third-party and get nothing. Owners in `--deny-owner` (repeatable, no default, case-insensitive) are refused, as is any remote whose owner/repo contains a `--deny-term`. With neither flag the script prints a loud warning (client repos are then only protected by routing) and continues. Folders that are neither pinned nor matched are denied.

## Steps

1. Preflight: git 2.26+, Go 1.25+ (or docker on Linux; on macOS `brew install go`), gh logged in, ssh-keygen, python3. Refuses if a sync dir it did not create exists, or if the old memsync is configured or its timer is active.
1a. Org token: one of `--org-token-file`, `--org-token-cmd` (shell-style quotes, no shell) or `--org-app <name>` is required. `--org-app` (with `--org-app-id`/`--org-app-key-cmd`, or an existing `apps.json` entry) runs `claude-memsync app-token --app <name>` (the GitHub App entry in `apps.json`) for every org call, and hands `init` and `doctor` that same command as `--token-cmd`; the binary is the one built in step 2, so the token's org reach is checked right after the build, before any repo or key is created. Every org operation (gh repo create, gh api, deploy-key add, init, doctor) uses it; for gh it is set as `GH_TOKEN` on that one command only. The ambient gh login is not used for the org.
1a2. `--host` is the host's label: 1-32 characters of `a-z`, `0-9` and `-`, starting with a letter or digit (checked before anything is created). Every `init` gets `--host-label <label>`, so the first host of a new repo has `# host:<label>` on its recipient line, and the owner's recovery key goes in with `--recovery-recipient` (marked `# recovery` and saved as `recoveryRecipients`, the only recipient `join accept` lets go unlabelled). `--org` equal to a `--deny-owner` is refused. The script refuses to run under `bash -x` / `SHELLOPTS=xtrace`, which would print the org token. `--org-token-cmd` is split with the same shell-style quoting as claude-memsync (double and single quotes group, nothing is expanded).
1b. Org scope (checked separately for the ambient gh login and for the org token; for `--org-app` the token's reach is the owners of `installation/repositories`): if the gh login can see orgs outside the allow list (by default the `--org` org; `--allow-org` adds more), a loud WARNING names them, calls out `--deny-owner` orgs, and repeats in the plan and the summary. It proceeds, and only ever touches repos in `--org`.
2. Plan: lists the repos to create, keys to add and routes. You type `yes`, or pass `--yes`.
3. Clone the pinned commit (from `--repo-url`, default `https://github.com/Exploded-views/claude-utils`) to `~/.local/src/claude-utils-<sha>`, verify `HEAD`, build `claude-memsync` and `claude-memmerge` into `~/.local/lib/claude-memsync/<sha>/`, symlink into `~/.local/bin`.
4. Create missing repos with `gh repo create --private`, then require `private: true` and `visibility: private` from the API. Non `mem-*` names are refused.
5. Deploy keys: `~/.ssh/<repo>_ed25519` (no passphrase, 0600), a `Host gh-<repo>` alias on ssh.github.com:443, GitHub's host key only if its fingerprint is `SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU`, `gh repo deploy-key add ... -w -t <host>`, then `git ls-remote`. If the org has deploy keys disabled the script says how to enable them and stops.
6. `routes.json` (0600, default deny, `foreignWriters` for the old memsync) and a `denylist-<kind>.txt` per kind containing each `--deny-term`, one per line.
7. `claude-memsync init` for each repo (post-quantum age encryption with the `--recovery-recipient` key, archive-deletes, private-only, `--token-cmd "gh auth token"`). A repo that already has content leaves this host without a key, so the script runs `join request` and prints the one-liner a member host runs: `claude-memsync join accept <host> --repo <key> --fingerprint "<8 groups of 4 hex>"`; `claude-memsync join status --repo <key>` shows pending or accepted. The fingerprint is parsed from the `fingerprint: xxxx xxxx xxxx xxxx xxxx xxxx xxxx xxxx` line `join request` prints (128 bits, grouped); a missing or differently shaped line stops the script.
7b. Token split: with `--check-app`, the key file must be absolute, a plain file, mode 0600, owned by you with one hard link; its fingerprint (`claude-memsync app key-fingerprint`) goes into the `apps.json` entry `{name, appId, org, keyFile, apiBase, keyFingerprint}` and, when `routes.json` names a registry, into the registry with `claude-memsync host check-key --fingerprint ... --app <check-app>`; every config gets `githubApp: <check-app>`, so the daemon's hourly re-check mints from this host's own check key. Without `--check-app`, each config gets `recheckTokenCmd: false`, `githubTokenCmd` and any saved org token file are removed, and `githubTokenFile` if `~/.config/claude-memsync/github-token` (0600) exists. Otherwise the hourly re-check is off and a warning says so. The script never writes that file.
8. `claude-memsync route home <key> --yes`.
9. Plugin: `claude plugin marketplace add` of the clone, then `claude plugin install memory-notices@claude-utils`.
10. Service: `install`, `start` (these may need your approval in the session), `status`, `doctor`.
11. Summary: repos, key names, pending joins, next steps. Private keys and tokens are never printed.

A re-run whose plan differs from the `routes.json` an earlier run wrote (a new project or workspace) regenerates it before any repo is created, keeping the old one as `routes.json.bak`, and puts the home pin back. Deny terms are written to the denylists as literal regexes (a leading `#` is escaped).

Each completed step leaves a marker under `~/.local/state/claude-memsync-bootstrap/`, so a re-run after a failure resumes and never regenerates keys or re-inits a sync dir.

Related-repo suggestions are not built yet; the script prints a placeholder and calls nothing.

## Tests

`scripts/bootstrap_test.sh` runs the script end to end in a fake HOME with stub `gh`, `systemctl`, `claude`, `go` and `ssh-keyscan` and local bare repos as remotes. `claude-memsync` is the real binary built from the tree behind a recording wrapper: `install`, `start`, `stop` and `doctor` are stubbed (`doctor` needs the live GitHub API) and `init` runs without `--private-only` for the same reason; `app-token` talks to a loopback fake of the GitHub App API. It checks private-only enforcement, key modes, routes (including a re-run that adds a project), denylists, token handling, quoting (one table shared with the Go tests), xtrace refusal, refusals, the personal-dev discovery rules and an idempotent re-run. Set `MEMSYNC_BIN` and `MEMMERGE_BIN` to use prebuilt binaries.
