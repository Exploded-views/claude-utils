# Host bootstrap

`scripts/bootstrap.sh` sets up claude-memsync on a new host in one pass. It is meant to be read before it runs, and `--dry-run` prints every step without doing it.

## The paste prompt

Paste this into a Claude Code session on the new host, filling in the commit, label and role:

> Run the claude-memsync bootstrap from github.com/Exploded-views/claude-utils at commit `<sha>` for host `<label>`, role `<role>`, with `--deny-owner <client-org> --deny-term <client-term>`: clone that exact commit to `~/.local/src/claude-utils-<sha>`, read `scripts/bootstrap.sh` in full before running it, run it with `--dry-run` first and show me the plan, then run it for real. Stop and ask me before any step that needs my approval. Never print private keys or tokens.

The real `<client-org>` and `<client-term>` values come from the owner's private prompt, never from this repo.

## Usage

```
scripts/bootstrap.sh --host <label> --role <role> [--sha <commit>] [--org Exploded-views]
                     [--workspace <dir>]... [--own-owner <owner>]... [--deny-owner <owner>]...
                     [--deny-term <term>]... [--repo-url <url>]
                     [--dry-run] [--yes]
```

Roles (table at the top of the script):

| Role | Repos | Home folder |
|---|---|---|
| `homelab` | `mem-personal`, `mem-infra-homelab` | pinned to infra-homelab |
| `personal-dev` | `mem-personal`, `mem-project-code-workspace`, one `mem-project-<name>` per own checkout | pinned to personal |

Unknown roles are refused. No client or orchestrator repo (`mem-client-*`, `mem-orch-*`) can be created or joined by this script.

`personal-dev` also takes `--workspace <dir>` (repeatable; prompted for on a terminal). Each workspace folder that is not itself a git repo is pinned to `mem-project-code-workspace`. Git checkouts directly under a workspace and under `~` (depth 1 to 2) are discovered; those whose origin owner is in the allow list (`gitgat`, the org, plus `--own-owner`) get a `mem-project-<repo>` repo and a remote route. Other owners are listed as third-party and get nothing. Owners in `--deny-owner` (repeatable, no default, case-insensitive) are refused, as is any remote whose owner/repo contains a `--deny-term`. With neither flag the script prints a loud warning (client repos are then only protected by routing) and continues. Folders that are neither pinned nor matched are denied.

## Steps

1. Preflight: git 2.26+, Go 1.25+ (or docker on Linux; on macOS `brew install go`), gh logged in, ssh-keygen, python3. Refuses if a sync dir it did not create exists, or if the old memsync is configured or its timer is active.
1a. Org token: `--org-token-file` or `--org-token-cmd` (argv split on spaces, no shell) is required. Every org operation (gh repo create, gh api, deploy-key add, init, doctor) uses it; for gh it is set as `GH_TOKEN` on that one command only. The ambient gh login is not used for the org.
1b. Org scope (checked separately for the ambient gh login and for the org token): if the gh login can see orgs outside the allow list (default `Exploded-views`; `--allow-org` adds more), a loud WARNING names them, calls out `--deny-owner` orgs, and repeats in the plan and the summary. It proceeds, and only ever touches repos in `--org`.
2. Plan: lists the repos to create, keys to add and routes. You type `yes`, or pass `--yes`.
3. Clone the pinned commit (from `--repo-url`, default `https://github.com/Exploded-views/claude-utils`) to `~/.local/src/claude-utils-<sha>`, verify `HEAD`, build `claude-memsync` and `claude-memmerge` into `~/.local/lib/claude-memsync/<sha>/`, symlink into `~/.local/bin`.
4. Create missing repos with `gh repo create --private`, then require `private: true` and `visibility: private` from the API. Non `mem-*` names are refused.
5. Deploy keys: `~/.ssh/<repo>_ed25519` (no passphrase, 0600), a `Host gh-<repo>` alias on ssh.github.com:443, GitHub's host key only if its fingerprint is `SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU`, `gh repo deploy-key add ... -w -t <host>`, then `git ls-remote`. If the org has deploy keys disabled the script says how to enable them and stops.
6. `routes.json` (0600, default deny, `foreignWriters` for the old memsync) and a `denylist-<kind>.txt` per kind containing each `--deny-term`, one per line.
7. `claude-memsync init` for each repo (age encryption with the recovery recipient, archive-deletes, private-only, `--token-cmd "gh auth token"`). A repo that already has content leaves this host without a key, so the script runs `join request` and prints the one-liner a member host runs: `claude-memsync join accept <host> --repo <key> --fingerprint <16 hex>`; `claude-memsync join status --repo <key>` shows pending or accepted. A missing `fingerprint:` line stops the script.
7b. Token split: each config gets `recheckTokenCmd: false`, `githubTokenCmd` and any saved org token file are removed, and `githubTokenFile` if `~/.config/claude-memsync/github-token` (0600) exists. Otherwise the hourly re-check is off and a warning says so. The script never writes that file.
8. `claude-memsync route home <key> --yes`.
9. Plugin: `claude plugin marketplace add` of the clone, then `claude plugin install memory-notices@claude-utils`.
10. Service: `install`, `start` (these may need your approval in the session), `status`, `doctor`.
11. Summary: repos, key names, pending joins, next steps. Private keys and tokens are never printed.

Each completed step leaves a marker under `~/.local/state/claude-memsync-bootstrap/`, so a re-run after a failure resumes and never regenerates keys or re-inits a sync dir.

Related-repo suggestions are not built yet; the script prints a placeholder and calls nothing.

## Tests

`scripts/bootstrap_test.sh` runs the script end to end in a fake HOME with stub `gh`, `systemctl`, `claude`, `go` and `ssh-keyscan` and local bare repos as remotes. It checks private-only enforcement, key modes, routes, denylists, token handling, refusals, the personal-dev discovery rules and an idempotent re-run. Assertions that depend on `join` and `--token-cmd` print `PENDING` while the CLI in the tree lacks them.
