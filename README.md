# claude-utils

[![CI](https://github.com/MarimerLLC/claude-utils/actions/workflows/ci.yml/badge.svg)](https://github.com/MarimerLLC/claude-utils/actions/workflows/ci.yml)

Cross-workstation tooling for Claude Code.

This is a fork of the upstream `claude-utils`. Its `claude-memsync` has grown
from "one private repo per person" into an encrypted, layered memory store
for people who work across several machines and several confidential
contexts. The other tools are as upstream ships them.

- **claude-memsync** (this fork): keeps Claude Code memories
  (`~/.claude/projects/<folder>/memory/`) in sync across your machines through
  private GitHub repos, routes each project to the right repo, and encrypts
  every note with post-quantum age before it leaves the machine. Described
  below; setup in [docs/setup.md](docs/setup.md).
- **[Distilling environment memories](docs/distilling-memories.md)**: lift the
  *transferable* lessons (shell/OS quirks, CLI gotchas, toolchain, your standing
  preferences) out of one project and reuse them in any other, existing or new,
  via a shared catalog and two Claude skills (`/distill`, `/distill-apply`).
- **[claude-focus](docs/claude-focus.md)** (Windows only): brings the Claude
  desktop app to the front (launching it if needed). Bind it to the Copilot key
  with PowerToys so that key opens Claude instead of Copilot.

## claude-memsync

Claude Code keeps what it learns about a project in a memory folder on the
machine where it learned it. `claude-memsync` is a background daemon (one Go
binary plus the `claude-memmerge` merge driver, for Linux, macOS and Windows)
that mirrors those folders into git repos and back, so every machine you use
sees the same notes. `MEMORY.md` indexes are merged section by section rather
than line by line, deleted notes are archived instead of lost, and a secret
scrub plus your own denylist keep credentials and named terms out of the
commits.

### Layered memory

Not every note belongs on every machine. Notes live in several private repos,
each of one **kind**, all named `mem-*` so the tool can never touch any other
repo:

| Kind | Repo | Holds | Lives on |
|---|---|---|---|
| personal | `mem-personal` | you and how you work: standing preferences, rules, the distilled catalog | every host |
| project | `mem-project-<name>` | one of your own projects, keyed by its git remote | hosts that work on it |
| infra | `mem-infra-<env>` | one environment you run: its services, network, quirks | hosts that manage it |
| client | `mem-client-<client-codename>` | one client's systems, people and decisions | only hosts doing that client's work |
| orchestrator | `mem-orch-<client-codename>` (later `mem-orch-all`) | a coordinating agent's own state for one client, kept apart from the client's technical notes | the coordinating host only |

A small `mem-registry` repo holds what hosts share about each other: routing
templates, relations between repos, expiry dates and check-key records. Name
client repos by a codename, never by the client's name.

**Routing.** Each host has its own `routes.json` (never synced) listing the
repos it holds and an ordered list of rules: pin an exact Claude folder, match
a project path, or match the project's git remote (`github.com/<your-org>/*`).
The first matching rule wins and sends that folder to exactly one repo.

**Default deny.** A folder no rule matches is not synced at all. A rule that
cannot decide (the checkout is missing, has no readable `origin`, or names a
repo this host does not have) denies the folder on the spot instead of falling
through to a broader rule.

**Client boundary.** A client or orchestrator folder never leaks into another
repo by accident: moving a folder out of a client repo needs an explicit
`--confirm-move-out-of-client`, templates published to the registry replace
client rules with denies, client folders never feed the shared catalog, and
relations between repos may not cross from one client to another or between a
client and your own repos. The only cross-client edge allowed is a read-only
view from `mem-orch-all` into each `mem-orch-<client-codename>`.

Hosts can also **read** related repos without joining them (a fetch-only copy
that is never mirrored into Claude and never pushed), get suggestions for
such relations from their own session history, and be added or retired as
members with a reviewed join flow.

### Encryption

Every `mem-*` repo is encrypted with [age](https://age-encryption.org) on the
machine, through a git clean/smudge filter, so GitHub stores only ciphertext.
The work tree and the Claude folders stay plain text.

- **Post-quantum by default.** New repos use `encryptionScheme: pq`: hybrid
  ML-KEM-768 + X25519 recipients (`age1pq1…`, identities
  `AGE-SECRET-KEY-PQ-1…`). Classic X25519 is available only as an explicit
  option, and age refuses to encrypt one file to a mix of the two, so a repo is
  either all hybrid or all classic. `recipients rekey-pq` converts a classic
  repo (see [setup](docs/setup.md#converting-a-classic-repo-to-post-quantum)).
- **Protected:** the contents of every note and every diff inside them.
- **Not protected:** file names (project folder names and note names), file
  sizes, commit times and counts, branch names, the commit graph, the
  recipients' public keys, and the repo names themselves. Pick repo and note
  names you would not mind GitHub seeing.
- **Old history after a rekey.** Adding, removing or replacing recipients
  re-encrypts the current tree only. Every earlier commit stays encrypted to
  the keys it had, readable by any key that was a recipient then, and (for a
  classic repo) exposed to a future quantum attacker. `rekey-pq` can replace
  the history with one fresh root commit, but a force-push does not erase the
  old objects at once: GitHub keeps them reachable by SHA until its garbage
  collection runs, and any clone or fork that fetched them keeps them for
  good.
- **Every change to the recipient set is reviewed.** A host encrypts only to
  the recipients it has pinned; a pull that brings new keys, changed host
  labels or a changed `.gitattributes` stops that host from committing until a
  person accepts the change.

The threat model is in [docs/security.md](docs/security.md).

### Quickstart

<!-- verify-flags: bootstrap.sh --recovery-recipient -->
```sh
git clone <fork-url> claude-utils && cd claude-utils
go build -o ~/.local/bin/ ./cmd/claude-memsync ./cmd/claude-memmerge
# once per org: create the org, register the two GitHub Apps, make the recovery key
#   -> docs/setup.md "First machine"
age-keygen -pq | <password-manager store command>   # public key (age1pq1...) on stderr
bash scripts/bootstrap.sh --host <host-label> --role personal-dev --org <your-org> \
  --recovery-recipient <age1pq1...> --org-app memsync-admin --check-app memsync-check ... --dry-run
# review the plan, run again with --yes, then on any later host: the paste prompt
claude-memsync status && claude-memsync doctor
```

Full steps, including adding machines, throwaway VMs and retiring a host, are in
[docs/setup.md](docs/setup.md). Every command and flag is in
[docs/reference.md](docs/reference.md) (generated from `--help`). The detailed
behaviour of each feature is in [docs/claude-memsync.md](docs/claude-memsync.md)
and the host bootstrap in [docs/bootstrap.md](docs/bootstrap.md). Fork changes
are listed in [CHANGELOG.md](CHANGELOG.md).

## Project layout

| Path | Purpose |
|---|---|
| `cmd/claude-memsync` | Daemon binary: lifecycle + watcher + sync loop |
| `cmd/claude-memmerge` | Git custom merge driver for `MEMORY.md` |
| `cmd/claude-focus` | Windows-only: focus or launch the Claude desktop app from a hotkey |
| `internal/sync` | Mirror, reconcile, watcher loop, manifest |
| `internal/merge` | Section-block parser + 3-way semantic merge |
| `internal/distill` | Distilled-memory catalog index + reconcile |
| `internal/gitwt` | Wrapper around the `git` CLI scoped to the sync work-tree |
| `internal/config` | Config load/save (`~/.claudesync/config.json`) |
| `internal/proc` | Cross-platform helper that hides child-process console windows on Windows |
| `internal/version` | Version resolution from `-ldflags` override or embedded VCS info |
| `internal/agecrypt` | age filter, recipients, pinning and join requests (fork) |
| `internal/route` | `routes.json`: repo kinds, rules, default deny (fork) |
| `internal/scrub` | Secret scrub and denylist (fork) |
| `internal/archive` | Archive-instead-of-delete (fork) |
| `internal/related` | Related repos and suggestions (fork) |
| `internal/ghguard`, `internal/ghapp` | Private-only checks and GitHub App tokens (fork) |
| `internal/hosts` | Host list and retire (fork) |
| `internal/foreign` | Foreign-writer guard (fork) |
| `scripts/bootstrap.sh` | One-pass host bootstrap (fork) |
| `scripts/gen-reference.sh` | Regenerates `docs/reference.md` from `--help` (fork) |
| `skills/` | `/distill` and `/distill-apply` Claude Code skills |
| `.githooks/` | Shared git hooks (pre-commit gofmt check) |

## Development

The repo ships a pre-commit hook that runs the same `gofmt` check as CI, so
formatting problems surface locally instead of after a push. Enable it once per
clone:

```sh
git config core.hooksPath .githooks
```

It only inspects staged `.go` files and blocks the commit if any are not
gofmt-clean, printing the `gofmt -w …` command to fix them. (`go vet` and the
test suite stay in CI, which runs them across Linux/macOS/Windows.)

## Releasing

Versioning follows [SemVer](https://semver.org): `vMAJOR.MINOR.PATCH`.
We're pre-1.0, which means the daemon's behavior, config format, and
on-disk layout may still change between minor versions; bumping the
patch is reserved for fixes that don't change observable behavior.

Bump rules while on `0.x`:

| Change | Bump |
|---|---|
| Bug fix, internal refactor, doc update | patch (`v0.1.7` → `v0.1.8`) |
| Behavior change, new feature, default change, new config field | minor (`v0.1.x` → `v0.2.0`) |
| Breaking change to config format, on-disk layout, or CLI surface | minor while pre-1.0 — but call it out clearly in the tag message |

After 1.0 the standard SemVer rules kick in (breaking → major, additive → minor, fix → patch). At that point a `v2+` would also need a `/v2` suffix in the Go module path.

### Cutting a release

1. Land all changes on `main` and confirm CI is green.
2. Create a GitHub Release for the version. Either:

   ```sh
   gh release create v0.1.8 \
     --title "v0.1.8" \
     --notes "- <bullet summary of user-visible changes>" \
     --target main
   ```

   …or use the GitHub UI (Releases → Draft a new release → choose tag
   `v0.1.8`, target `main`, fill in title and notes, **Publish**).

3. Publishing the Release fires `.github/workflows/release.yml`, which
   cross-compiles both binaries for {linux, darwin, windows} ×
   {amd64, arm64} (plus `claude-focus.exe` for Windows), stamps the version in via `-ldflags`, packages each
   target as `claude-utils-v0.1.8-<os>-<arch>.{tar.gz,zip}`, generates
   `SHA256SUMS`, and uploads everything to the Release. ~1 minute.

4. Watch the run finish:

   ```sh
   gh run watch
   gh release view v0.1.8     # confirms the assets are attached
   ```

5. To upgrade your own machine, download the appropriate archive and
   `SHA256SUMS` from the Release page. In the download directory, check
   the archive before extracting it:

   ```sh
   sha256sum -c --ignore-missing SHA256SUMS   # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
   ```

   Then extract it and swap the binaries:

   ```sh
   claude-memsync stop
   # extract archive, copy bin/* to your install location
   claude-memsync start
   claude-memsync version     # confirms the upgrade landed
   ```

### If the release workflow didn't fire

GitHub's `release: published` event is occasionally missed — usually
when the Release was created from a pre-existing tag via API. Two
recovery paths:

- **Manual dispatch** (preferred): trigger the workflow against the
  existing tag without touching the Release.

  ```sh
  gh workflow run release.yml --field tag=v0.1.8
  gh run watch
  ```

  This re-uses the same build/upload steps and the existing Release
  picks up the assets.

- **Recreate the Release**: `gh release delete v0.1.8 --cleanup-tag=false`
  then recreate it. Sometimes nudges GitHub into firing the event
  properly. Manual dispatch is simpler — prefer it unless you also
  need to fix release notes.

The same `workflow_run` invocation works for retroactively building
archives for any tag that doesn't yet have a Release, or for any
Release whose previous build run failed.

The upload never overwrites an asset that is already attached, so a
published archive and its `SHA256SUMS` never change under a tag. If a
failed run left some assets behind, delete them first
(`gh release delete-asset v0.1.8 <name>`), then dispatch again.

### Building locally without a release

For dev iteration, a plain `go build -o bin/ ./cmd/...` is enough — the
binaries will report a VCS-derived `dev+<sha>` version. To stamp a
specific version into a local build:

```sh
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin/ ./cmd/...
```

PowerShell equivalent:

```powershell
$VERSION = git describe --tags --always --dirty
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin\ .\cmd\...
```

### How version resolution works

`internal/version.String()` returns the first of:

1. The build-time `Override` set via `-ldflags "-X .../internal/version.Override=<v>"` (preferred for releases).
2. `runtime/debug.BuildInfo.Main.Version` — populated automatically when installed via `go install <path>@<tag>`. Yields `v0.1.7` for tagged installs, or a pseudo-version like `v0.0.0-20260508213755-a52d68630b31` for untagged commits.
3. `vcs.revision` from the embedded build settings — yields `dev+<short-sha>` (or `dev+<short-sha>-dirty` if the working tree had uncommitted changes at build time).
4. The literal `dev` — only seen if Go embedded no VCS info (e.g. building from outside a git checkout).

This means a plain `go build` always produces a binary whose `version` subcommand says something useful — no Makefile or build script required for development.

## License

MIT — see [LICENSE](LICENSE).
