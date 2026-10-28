# This fork: encrypted, layered claude-memsync

The top-level [README](../README.md) introduces the fork and its install and
quickstart. This page is the overview of the model this fork adds to
`claude-memsync`: repo kinds, routing, default deny and encryption. Setup
steps are in [setup.md](setup.md), the threat model in
[security.md](security.md), the mechanics of each feature in
[claude-memsync.md](claude-memsync.md), every flag in
[reference.md](reference.md) (generated from `--help`), release downloads in
[releases.md](releases.md), host bootstrap in [bootstrap.md](bootstrap.md),
CI in [ci.md](ci.md), and user-visible changes in
[../CHANGELOG.md](../CHANGELOG.md).

The fork turns `claude-memsync` from "one private repo per person" into an
encrypted, layered memory store for people who work across several machines
and several confidential contexts. The other tools (`distill`, `claude-focus`)
are as upstream ships them.

## Relationship to upstream

This repo is a fork of
[MarimerLLC/claude-utils](https://github.com/MarimerLLC/claude-utils), under
the same MIT licence and copyright.

- `main` mirrors upstream's `main` and carries no fork changes.
- `integ/next` holds the fork's work. Releases are tagged from it with an
  `-ev.N` suffix (`v0.3.0-ev.1`) that keeps them apart from upstream's tags.
- The Go module path stays `github.com/MarimerLLC/claude-utils`.
- Upstream's `ci.yml` stays; the fork's own checks are in `fork-ci.yml`, whose
  jobs are skipped in upstream's repository. Files the fork changes for its
  own needs, such as `release.yml`, say so in a header comment.
- [CHANGELOG.md](../CHANGELOG.md) lists every fork change and marks it
  *upstream candidate* or *fork-only*. That label describes the change; it is
  not a plan to send it upstream, and none has been proposed.

## Layered memory

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
a project path, or match the project's git remote (`github.com/<org>/*`).
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

The config that is never synced (`routes.json`, `apps.json`, the age identity)
lives in one directory on every OS, `${XDG_CONFIG_HOME:-~/.config}/claude-memsync`;
see [setup.md](setup.md#the-config-directory).

## Encryption

Every `mem-*` repo is encrypted with [age](https://age-encryption.org) on the
machine, through a git clean/smudge filter, so GitHub stores only ciphertext.
The work tree and the Claude folders stay plain text.

- **Post-quantum by default.** New repos use `encryptionScheme: pq`: hybrid
  ML-KEM-768 + X25519 recipients (`age1pq1…`, about 1,960 characters;
  identities `AGE-SECRET-KEY-PQ-1…`). Classic X25519 is available only as an
  explicit option, `init --encrypt --encryption-scheme classic`, and age
  refuses to encrypt one file to a mix of the two, so a repo is either all
  hybrid or all classic. `recipients rekey-pq` converts a classic repo (see
  [setup](setup.md#converting-a-classic-repo-to-post-quantum)); the daemon
  syncs nothing in a repo whose conversion is unfinished.
- **Protected:** the contents of every note and every diff inside them.
- **Not protected:** file names (project folder names and note names), file
  sizes, commit times and counts, branch names, the commit graph, the
  recipients' public keys, and the repo names themselves. Pick repo and note
  names you would not mind GitHub seeing.
- **Old history after a rekey.** Adding, removing or replacing recipients
  re-encrypts the current tree only; earlier commits stay encrypted to the
  keys they had. A force-push of a fresh root does not erase old objects at
  once. Details and caveats: [security.md](security.md#old-history).
- **Every change to the recipient set is reviewed.** A host encrypts only to
  the recipients it has pinned; a pull that brings new keys, changed host
  labels or a changed `.gitattributes` stops that host from committing until a
  person accepts the change.

## Quickstart (fork)

```sh
git clone -b integ/next https://github.com/Exploded-views/claude-utils.git && cd claude-utils
go build -o ~/.local/bin/ ./cmd/claude-memsync ./cmd/claude-memmerge
# once per org: create the org, register the two GitHub Apps, make the recovery key
#   -> setup.md "First machine"
age-keygen -pq | <password-manager store command>   # then: <password-manager read command> | age-keygen -y
# then bootstrap the first host (dry run first), and any later host from the paste prompt:
#   -> setup.md step 5, bootstrap.md
claude-memsync status && claude-memsync doctor
```

A host that should not build anything can install from a release archive
instead; see [releases.md](releases.md).

## Fork-only parts of the tree

| Path | Purpose |
|---|---|
| `internal/agecrypt` | age filter, recipients, pinning and join requests |
| `internal/route` | `routes.json`: repo kinds, rules, default deny |
| `internal/scrub` | Secret scrub and denylist |
| `internal/archive` | Archive-instead-of-delete |
| `internal/related` | Related repos and suggestions |
| `internal/ghguard`, `internal/ghapp` | Private-only checks and GitHub App tokens |
| `internal/hosts` | Host list and retire |
| `internal/foreign` | Foreign-writer guard |
| `scripts/bootstrap.sh` | One-pass host bootstrap |
| `scripts/package-release.sh` | Builds the release archives and `SHA256SUMS` |
| `scripts/gen-reference.sh` | Regenerates `docs/reference.md` from `--help` |
| `scripts/lint-portability.sh`, `scripts/lint-workflows.sh` | CI lints, see [ci.md](ci.md) |

## Known follow-ups

Open work, tracked as [GitHub issues](https://github.com/Exploded-views/claude-utils/issues).
None of it is in a release; the rest of these docs describe what ships.

| Issue | What |
|---|---|
| [#2](https://github.com/Exploded-views/claude-utils/issues/2) | Consistent repo keys across hosts. Keys in `routes.json` can differ between hosts, so a command printed on one host can fail on another; `--repo` already accepts the GitHub repo name for that reason |
| [#5](https://github.com/Exploded-views/claude-utils/issues/5) | A precomputed changes index, so per-prompt hooks spawn no git, with performance budgets in CI (see [ci.md](ci.md#performance-budgets-planned)) |
| [#8](https://github.com/Exploded-views/claude-utils/issues/8), [#10](https://github.com/Exploded-views/claude-utils/issues/10) | No crash loop when the private-only start check cannot reach a verdict, and a `release <repo>` command to lift a hold on purpose |

Also planned and not built: `join self` (break-glass join with no member
online), see [setup.md](setup.md#break-glass-join-self-planned).
