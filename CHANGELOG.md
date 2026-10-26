# Changelog (fork)

Changes this fork makes to upstream `claude-utils`, newest first. Each entry is
marked **fork-only** (tied to this fork's layered `mem-*` model) or **upstream
candidate** (useful on its own; not proposed upstream yet). Versions before the
fork are upstream's own releases.

## Unreleased

- **Bootstrap for homelab and crewkit hosts**: up-front prerequisite checks, exit 3 when the check key is missing, `--delete-org-token-file`, `--prebuilt-dir`, `--keep-out-owner`/`--keep-out-term` (old `--deny-*` names still work), release archives that carry `scripts/bootstrap.sh`, and `release-layout` and `workflow-lint` CI jobs; see `docs/bootstrap.md` and `docs/ci.md`. Mac fix included: the recovery-key check used a regex repeat count of 1000, which macOS's `grep` and bash `[[ =~ ]]` refuse above 255, so every valid post-quantum key was rejected on a Mac; it is now plain parameter expansion (`valid_pq_recipient`, bash 3.2 safe), and `scripts/lint-portability.sh` keeps GNU-only tools out. *Fork-only.*
- **Fork CI and coverage ratchet**: a fork-only workflow (vet, gofmt, race tests, shellcheck, bootstrap harness, gitleaks, reference check) and per-package coverage floors that only rise (`make ratchet`); see `docs/ci.md`. *Fork-only.*
- **Post-quantum encryption by default**: new repos use `encryptionScheme: pq` (hybrid ML-KEM-768 + X25519, `age1pq1…`); classic X25519 only on request; `recipients rekey-pq` converts a single-member classic repo, optionally onto one fresh root commit. *Upstream candidate.*
- **PQ follow-ups**: related reads find `identity-pq.<ext>` (and no longer need `identity.txt`) after `rekey-pq`; `bootstrap.sh` has no built-in allowed orgs or own owners (allowed orgs are `--org` plus `--allow-org`; own owners are `--org` plus `--own-owner`, so pass `--own-owner <github-user>` for a personal account); `rekey-pq --all` without `--apply` ends with `dry run: N ok, M would fail` and exits nonzero if any would fail. *Upstream candidate.*
- **rekey-pq hardening**: `--all` stops at the first failure and reports done/failed/not started; a rerun resumes, `--rollback` undoes an unpushed migration; checks run before any local change and `config.json` is saved only after the push; the classic identity is kept while any clone under the sync root uses it; synced-head follows the rekey; also `SchemeOf` falls back to the recipients file, `-h` never acts for `start`/`stop`/`status`/`uninstall`/`version`, `app add --verify` checks before writing, `init` refuses `--github-app` with `--token-cmd` and reuses `identity-pq.txt`. *Upstream candidate.*
- **Explicit token flags win**: `--token-file`, `--token-cmd` and `--github-app` replace every configured token source for `doctor`, `host` and `repo create`. *Upstream candidate.*
- **Safer app registration**: `app register` refuses unreplaced `<…>` placeholders, keeps the key in a `recovered.pem` if storing it fails, and `app add --replace --verify` repairs an entry. *Upstream candidate.*
- **Filters follow the binary**: after an upgrade the daemon repoints each clone's age filter and merge drivers at the running binary. *Upstream candidate.*
- **User guide**: README model overview, `docs/setup.md`, `docs/security.md`, generated `docs/reference.md` with a CI staleness check, this changelog. *Fork-only.*
- **Host bootstrap**: `scripts/bootstrap.sh` sets up a host in one pass (roles, repos, deploy keys, routes, init, join requests, per-host check key) with a dry run first, and a one-prompt paste for new hosts. *Fork-only.*
- **Host lifecycle**: host labels on deploy keys and recipients, `host list` (stale/expired), `host retire` (dry run, `--apply`), `host check-key`, read-only members, `--expires`. *Fork-only.*
- **Related-project suggestions**: `related suggest|accept|dismiss` offer read relations from this host's own session data, within the client boundary. *Fork-only.*
- **GitHub App auth**: `app register` (manifest flow, key to a file or a password manager), `app-token`, `app key-fingerprint`, `githubApp` in config; short-lived installation tokens instead of long-lived PATs. *Upstream candidate.*
- **Join requests and token commands**: `join request|status|list|accept|reject` with a fingerprint a person compares; `--token-cmd` / `githubTokenCmd` read the GitHub token from a command, never saved. *Upstream candidate.*
- **Foreign-writer guard**: the daemon holds while another sync tool may still write into the Claude folders (`foreignWriters`), plus a start-up quiet window. *Upstream candidate.*
- **Home-folder routing**: `route home|publish|inherit`; the home folder pins per host (including to the personal repo), templates shared through the registry with client rules turned into denies. *Fork-only.*
- **age encryption at rest**: clean/smudge filter, decrypting merge driver, pinned and reviewed recipient sets, plaintext push guard, `recipients add|remove|list|accept|label`. *Upstream candidate.*
- **Related projects**: `relate`, `unrelate`, `related [sync|context]`: read-only, never-mirrored copies of related repos and a SessionStart index, with default-deny boundaries. *Fork-only.*
- **Archive, never delete**: `--archive-deletes` moves deleted notes to `archive/<date>/`; `archive list|search`; `distill --prune` archives. *Upstream candidate.*
- **Renamed remotes**: `aliases` on remote rules and a root-commit fallback with a grace period keep routing a renamed or transferred repo. *Upstream candidate.*
- **Several repos per host**: `routes.json` with repo kinds, first-match folder/path/remote rules, default deny, one loop per repo. *Upstream candidate.*
- **Private repos only + doctor**: `mem-*` name allowlist, `repo create` verified private, hourly re-check, `doctor` (and a daily timer), trusted-config rules. *Fork-only.*
- **Pinned releases**: tag-triggered release workflow with `SHA256SUMS`; published assets are never overwritten. *Upstream candidate.*
- **Changes since a commit**: `changes` (and `changes --hook` for Claude Code) list the memory files changed since a marker, so a running session hears about them. *Upstream candidate.*
- **Secret scrub and denylist**: held-back commits for files that look like credentials or match a denylist, with parking around pulls and recovery. *Upstream candidate.*
