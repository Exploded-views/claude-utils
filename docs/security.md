# claude-memsync security model

What claude-memsync protects, against whom, and where the limits are. Setup
steps are in [setup.md](setup.md); the detailed behaviour of each check is in
[claude-memsync.md](claude-memsync.md).

## Assets and parties

- **Assets:** the contents of your notes, especially client notes; the
  separation between contexts (a client's notes never reach a host or repo
  outside that client); and the keys that grant access.
- **GitHub** stores the repos. Treat it as honest but curious, and as a
  possible future breach: it should hold nothing it can read.
- **Hosts** are the machines that run the daemon. Some are yours and trusted,
  some are disposable VMs or agent hosts that run code you did not write. Any
  one of them may be compromised.
- **You**, the owner, approve every new member and hold the recovery key.

## What GitHub sees

Every file except `.gitattributes`, `.gitignore`, `.age-recipients` and
pending `joins/<label>.txt` requests (public keys only) is age ciphertext
before it reaches git, and the daemon checks every blob before each push and
refuses to send plaintext. GitHub, and anyone who breaches it, sees:

- file names (project folder names, note names) and sizes;
- commit times, counts, branch names and the commit graph;
- the repo names, and the recipients' public keys and host labels;
- commit messages, which are only `memsync: N files`.

It cannot read, search or render the notes, and its secret scanning sees only
ciphertext. Choose repo names, codenames and host labels you are happy for
GitHub to see.

## Keys and what each one can do

| Key | Where | Can |
|---|---|---|
| Host age identity (`AGE-SECRET-KEY-PQ-1…`) | one per host, mode 0600 | decrypt every repo this host is a recipient of, including all history it can fetch |
| Deploy key, write | one per host per repo, titled `<host-label>` | push to that one repo; nothing else |
| Deploy key, read-only | read-only members, related-repo read copies | fetch that one repo; GitHub refuses pushes |
| `memsync-check` App key | one per host | mint an hour-long token with Metadata: read (is a repo private?) |
| `memsync-admin` App key | your password manager only | create repos, add and delete deploy keys (Administration: read and write) |
| Recovery identity | your password manager / offline only | decrypt every repo it was a recipient of |

**Deploy keys are scoped to one repo each.** A host that holds keys for
`mem-personal` and `mem-project-<name>` cannot reach `mem-client-<client-codename>`
at all, not even to fetch ciphertext. Read-only members get keys without write
access, so the refusal is GitHub's, not the tool's.

**Why the admin key never stays on a host.** Administration: write on every
repo of the org can delete repos, change their visibility to public, and add a
deploy key for anyone. It is the one credential that would undo the whole
design. So it is read from your password manager at the moment it is needed,
by `--key-cmd`, for a single run (the bootstrap adds its `apps.json` entry for
the run and removes it on exit); installation tokens minted from it expire
within an hour. No daemon ever uses it.

**Per-host check keys.** The check App can change nothing, but each host still
gets its own private key, so a lost host is cut off by deleting that key
without touching the others. GitHub allows at most **25 private keys per App**
and only lets you delete them in its web UI. `host check-key` records each
host's key fingerprint in the registry, and `host list` and `doctor` warn from
20 live keys. Retire hosts you no longer use, and delete their key in the UI.

## A compromised host

Assume an attacker controls one host fully, including its keys.

**It can:**

- read every repo it is a recipient of, now and in all the history it can
  fetch;
- push to every repo it has a write deploy key for: forge or delete notes
  (archived copies remain), add junk, or edit `.age-recipients` or
  `.gitattributes` in those repos;
- use its check key to learn which `mem-*` repos exist and whether they are
  private.

**It cannot:**

- **Read repos it is not a recipient of**, or fetch repos it has no deploy key
  for.
- **Add itself, or anyone, to another repo** without a member's consent. A
  join request is a plaintext file a person must accept on a member after
  comparing a fingerprint out of band; no daemon code path accepts.
- **Get its key used by other hosts quietly.** Every host pins the recipient
  set it reviewed, including host labels, and encrypts only to that pinned
  set. A pull that changes `.age-recipients`, a label, or any `.gitattributes`
  stops that host from committing (`status` says `recipients UNREVIEWED`)
  until a person runs `recipients accept` and reads the diff.
- **Switch encryption off elsewhere.** The filter rules live in each clone's
  `$GIT_DIR/info/attributes`, which outranks any `.gitattributes`, and are
  rewritten at every start. The filter is `required`: without the binary, git
  fails rather than storing plaintext.
- **Change routing on other hosts.** `routes.json` is per host and never
  synced. A routing template in the registry applies only when a person runs
  `route inherit` on a host, and client rules are published as denies. A
  relation in the registry does nothing on a host until that host accepts it.
- **Plant per-host files.** If a pulled tree tracks `config.json`, the
  denylist, the PID file or anything under `.state/`, the pull is refused
  before anything merges. The `config.json` keys that run commands or name
  credentials are honoured only from a file that is yours, mode 0600, with one
  hard link and untracked.
- **Create repos, make them public, or add deploy keys.** Its check key has
  Metadata: read only.

**Containing it:** `host retire <host-label> --apply` from another member
deletes its deploy keys, removes its recipient and re-encrypts the current
tree; then delete its check key in the UI. Everything it already fetched stays
readable to it: rotate any secret its notes named.

## Harvest now, decrypt later

Ciphertext on GitHub lasts. Someone who copies it today can store it until a
quantum computer can break X25519, and then read every note. Memory notes stay
sensitive for years (client systems, people, decisions), so the default is a
**hybrid** scheme: each file key is wrapped with ML-KEM-768 (post-quantum) and
X25519 together, and an attacker must break both. Classic X25519 is an explicit
opt-in. age refuses to encrypt one file to a mix of hybrid and classic
recipients, so a classic key can never silently weaken a hybrid repo. The cost
is size: each hybrid recipient adds about 1.5 KB per file, harmless at
note sizes.

## Old history

Every change to the recipient set (a join, a retire, a rekey) re-encrypts the
**current tree only**. Earlier commits stay encrypted to the keys they had:

- a removed host still decrypts all history it was a recipient of;
- after `rekey-pq`, older commits are still classic ciphertext, exposed to
  harvest-now-decrypt-later.

`rekey-pq` can replace the history with one fresh root commit and force-push
it. That stops the remote from serving old commits on its branches, but it does
not erase them: GitHub keeps unreachable objects fetchable **by SHA** until its
own garbage collection removes them (and cached views, forks or other clones
may keep them indefinitely). Ask GitHub support to purge them if it matters,
and assume anything already fetched is kept.

## The recovery key

- Generate it as a hybrid key and pipe it into your password manager
  (`age-keygen -pq | <password-manager store command>`, then derive the public
  key with `<password-manager read command> | age-keygen -y`): the secret never
  lands on disk, and only the public `age1pq1…` line is ever given to the tool.
- It is a recipient of every repo, so it decrypts everything: never put the
  secret on a synced machine or an agent host.
- It is the only recipient `join accept` lets go without a host label, and only
  when it is configured as a recovery recipient on that host.
- Keep a second offline copy. Without the recovery key and with every host
  gone, the notes are unrecoverable by design.

## Planned: break-glass passphrase strength

The planned `join self --passphrase` unlocks a passphrase-wrapped copy of the
recovery identity (age scrypt). That copy would decrypt every repo, so its only
protection against an offline attacker who obtains it is the passphrase plus
the scrypt work factor. The design therefore:

- keeps the wrapped copy in a separate repo that no host holds a deploy key
  for, readable only with your own GitHub login;
- generates the passphrase, at least **10 words from a 7,776-word list**:
  10 × log2(7776) ≈ 129 bits, beyond offline guessing even with a large
  scrypt-cracking budget and comparable to the security of the keys it
  protects (human-chosen phrases are refused: they carry far less entropy
  than they appear to);
- reads it from a hidden terminal prompt only, never from argv, the
  environment or a file, and keeps the unlocked identity in memory only.

This feature is not built yet; see [setup.md](setup.md#break-glass-join-self-planned).

## Other guards

- **Private repos only.** Repos are created with `private: true` and read back;
  `init`, the daemon's hourly re-check and `doctor` refuse a `mem-*` repo that
  is public or internal, failing closed on any API error. Only `mem-*` names
  are ever created, listed or joined.
- **Push URL and host checks.** The push URL git really uses must equal the
  configured remote and resolve to the allowed git host; ssh command overrides
  are refused.
- **Secret scrub and denylist.** Files that look like they hold credentials, or
  match your denylist, are held back from commits.
- **Related repos are inert.** A read copy is never checked out (no filters or
  hooks from it run), never mirrored into Claude and has no push path.
- **Foreign-writer guard.** If another sync tool may still write into the Claude
  folders, the daemon holds instead of mirroring its output.
