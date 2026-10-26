# Setting up claude-memsync

This guide takes you from nothing to several machines sharing encrypted,
layered memory. Read [the README](../README.md) first for the model (repo
kinds, routing, default deny) and [security.md](security.md) for what each
key can do. Every flag shown here is in [reference.md](reference.md).

Placeholders: `<your-org>` is the GitHub org you create for memory,
`<host-label>` the name you give a machine (1 to 32 of `a-z`, `0-9`, `-`,
never its real host name), `<client-codename>` a neutral name for a client,
`<password-manager store command>` / `<password-manager read command>` your
password manager's CLI commands that store a secret from stdin and print it
back.

Contents:

- [What you need](#what-you-need)
- [First machine](#first-machine)
- [Each extra machine](#each-extra-machine)
- [Throwaway VMs](#throwaway-vms)
- [Retiring a host](#retiring-a-host)
- [Break-glass: join self (planned)](#break-glass-join-self-planned)
- [Upgrading the binary](#upgrading-the-binary)
- [Converting a classic repo to post-quantum](#converting-a-classic-repo-to-post-quantum)

## What you need

- git 2.26 or later, Go 1.25 or later (to build), `gh` logged in, `ssh-keygen`,
  `python3` (the bootstrap uses it), and `age-keygen` 1.3 or later (for
  `-pq`; `go install filippo.io/age/cmd/age-keygen@v1.3.2`).
- A password manager with a CLI that can store a secret read from stdin and
  print it back. The recovery key and the admin App's key live there and
  nowhere else.
- The same project paths on every machine: Claude names a project's memory
  folder after its absolute path.

## First machine

Do this once, on a machine you own and trust (not an agent host).

### 1. Create the org and its settings

Create a GitHub organization used only for memory, for example
`<your-org>`. Some of the settings below are only offered on paid plans;
set the ones your plan has. In the org's settings:

- Base permission: **No permission**.
- Repository creation: members may create **private** repositories only.
- Forking of private repositories: **off**.
- Deploy keys: **allowed** on repositories (the bootstrap stops and says so if
  they are disabled).

The org may also hold public repos; the tool only ever lists, creates or
touches repos whose names start with `mem-`.

### 2. Build the binaries

```sh
git clone <fork-url> ~/.local/src/claude-utils
cd ~/.local/src/claude-utils
go build -o ~/.local/bin/ ./cmd/claude-memsync ./cmd/claude-memmerge
claude-memsync version
```

Both binaries must sit in the same directory.

### 3. Register the two GitHub Apps

The tool talks to the GitHub API only to create repos, check that they are
private, and manage deploy keys. It uses two Apps so the powerful key never
lives on a working host.

**`memsync-admin`** (repository Administration: read and write, Metadata:
read). It creates repos and adds and deletes deploy keys. Its private key goes
straight into your password manager and never touches disk:

```sh
claude-memsync app register --name memsync-admin --org <your-org> --url <homepage-url> \
  --perm administration:write --perm metadata:read \
  --key-cmd-store "<password-manager store command>" \
  --key-cmd "<password-manager read command>"
```

**`memsync-check`** (Metadata: read only). Every host uses it for the
private-only checks; it can change nothing. Each host gets its **own** private
key, so a lost host is cut off by deleting one key:

```sh
claude-memsync app register --name memsync-check --org <your-org> --url <homepage-url> \
  --perm metadata:read --key-out ~/.config/claude-memsync/memsync-check.pem
```

`register` writes a local page, prints its path, and waits; open it in a
browser signed in as an org owner and confirm. Then install each App on **all
repositories** of the org (the command prints the install link) and note both
App IDs. The entries land in `~/.config/claude-memsync/apps.json`.

For every further host, generate another `memsync-check` key in the GitHub UI
(Settings, Developer settings, GitHub Apps, `memsync-check`, Generate a
private key), copy the downloaded `.pem` to that host, `chmod 600` it, and
delete the download. GitHub allows at most 25 keys per App and only lets you
delete them in the UI; `host list` and `doctor` warn from 20.

### 4. Make the recovery key

The recovery key is one more recipient of every repo. It is how you read your
notes when every machine is gone, so its secret half must never sit on a
synced machine. You need `age-keygen` 1.3 or later; older builds (for
example a package-manager build before 1.3) have no `-pq`. Pin it:

```sh
go install filippo.io/age/cmd/age-keygen@v1.3.2
```

1. Generate a post-quantum hybrid key and pipe it straight into your password
   manager, so the secret never touches disk:

   ```sh
   age-keygen -pq | <password-manager store command reading stdin>
   ```

2. Derive the public key from the stored copy (do not rely on capturing
   `age-keygen`'s stderr; it can come out empty):

   ```sh
   <password-manager read command> | age-keygen -y > recovery-pq.pub
   ```

3. Check it: one line, starting with `age1pq1`, about 1,960 characters.

   ```sh
   wc -l recovery-pq.pub; wc -c recovery-pq.pub; head -c 7 recovery-pq.pub; echo
   ```

The public key is the only part the tool needs (pass it as
`--recovery-recipient`). Never redirect the secret to a file or paste it
anywhere. Optionally also keep an offline copy (paper or a hardware token).

### 5. Create the repos and the first host

The recommended path is the bootstrap script, which runs every step below on
a machine with a dry run first. It is described in
[bootstrap.md](bootstrap.md); read `scripts/bootstrap.sh` before running it.

Run it in bash (not zsh). Set the inputs once, define `boot` once, dry-run,
then run the same function with `--yes`:

```sh
# INPUTS (fill in)
ORG=<org>                          # the GitHub organisation that holds the memory repos
HOST_LABEL=<host-label>            # this host's label
RECOVERY=<recovery-recipient>      # your offline post-quantum recovery PUBLIC key, age1pq1...
GITHUB_USER=<github-user>          # your personal GitHub account
ADMIN_APP_ID=<admin-app-id>
ADMIN_KEY_CMD="<password-manager read command>"
CHECK_APP_ID=<check-app-id>
CHECK_KEY_FILE=<check-key-file>    # absolute path, mode 0600
KEEP_OUT_OWNERS=""                 # (optional; leave empty for none) space-separated owners to keep out
KEEP_OUT_TERMS=""                  # (optional; leave empty for none) space-separated terms to keep out

boot() {
  local a=() x
  for x in $KEEP_OUT_OWNERS; do a+=(--keep-out-owner "$x"); done
  for x in $KEEP_OUT_TERMS; do a+=(--keep-out-term "$x"); done
  bash scripts/bootstrap.sh --host "$HOST_LABEL" --role personal-dev --org "$ORG" \
    --recovery-recipient "$RECOVERY" --own-owner "$GITHUB_USER" ${a[@]+"${a[@]}"} \
    --org-app memsync-admin --org-app-id "$ADMIN_APP_ID" --org-app-key-cmd "$ADMIN_KEY_CMD" \
    --check-app memsync-check --check-app-id "$CHECK_APP_ID" --check-key-file "$CHECK_KEY_FILE" "$@"
}
boot --dry-run
# after checking the plan, the same command with --yes instead of --dry-run:
boot --yes
```

`--org` and `--recovery-recipient` are required and have no defaults; the
script refuses to run without them. The recovery recipient must be the
hybrid (`age1pq1...`) public key of the recovery identity you hold.
`--own-owner <github-user>` names your personal GitHub account so its checkouts
get project repos; the only owner counted as yours by default is `--org`.
`--keep-out-owner` and `--keep-out-term` are optional and repeatable: a
checkout whose origin owner matches an owner is skipped (no repo, no route), as
is one whose `owner/repo` contains a term; terms are also written to the
denylists the daemon checks at commit time. If the check key file is not there
yet the script exits 3 (paused) before changing anything and says how to create
it. Exit codes, the token-file flow for a host without a password manager, and
the release-archive flow are in [bootstrap.md](bootstrap.md).

The manual path, for one repo (repeat per repo):

```sh
# a private mem-* repo, verified private after creation
claude-memsync repo create --org <your-org> \
  --token-cmd "claude-memsync app-token --app memsync-admin" mem-personal

# a deploy key for this host and repo (write access), behind its own ssh alias
ssh-keygen -t ed25519 -N '' -f ~/.ssh/mem-personal_ed25519
GH_TOKEN="$(claude-memsync app-token --app memsync-admin)" \
  gh repo deploy-key add ~/.ssh/mem-personal_ed25519.pub -R <your-org>/mem-personal -w -t <host-label>
# ~/.ssh/config:  Host gh-mem-personal / HostName ssh.github.com / Port 443 /
#                 User git / IdentityFile ~/.ssh/mem-personal_ed25519 / IdentitiesOnly yes

# the encrypted sync dir
claude-memsync init --remote git@gh-mem-personal:<your-org>/mem-personal.git \
  --sync-dir ~/.claudesync.d/personal --encrypt --host-label <host-label> \
  --recovery-recipient <age1pq1...> --archive-deletes --private-only \
  --github-app memsync-check
```

`init --encrypt` creates this host's own hybrid identity in
`~/.config/claude-memsync/` (or `$XDG_CONFIG_HOME/claude-memsync/`; mode 0600)
and prints only its public key. On a Mac the identity is there too, except on
installs made before this rule, where it stays in
`~/Library/Application Support/claude-memsync/` and keeps working (`status` hints
at moving it; see "The config directory" in [claude-memsync.md](claude-memsync.md)).
With several repos, write
`~/.config/claude-memsync/routes.json` (see "Several repos per host" in
[claude-memsync.md](claude-memsync.md#several-repos-per-host)), pin the home
folder, and start the service:

```sh
claude-memsync route home personal --yes
claude-memsync install && claude-memsync start
claude-memsync status
claude-memsync doctor --org <your-org>
```

`doctor --install-timer` writes a daily check of every `mem-*` repo's
visibility.

## Each extra machine

### 1. Bootstrap from one prompt

On the new machine, put its own `memsync-check` key file in place (step 3
above), then paste the bootstrap prompt from [bootstrap.md](bootstrap.md)
into a Claude Code session there, with your placeholders filled in. The
session clones a pinned commit, reads the script, shows you the dry run, and
runs it after you agree. The prompts list their INPUTS first and refer to them
by name, so each value is written once. The admin App's key is read from your password
manager for the run only and is never written to the host.

For every repo that already has content, the new host cannot decrypt anything
yet. The bootstrap runs a join request and prints a fingerprint.

### 2. The join request

What the bootstrap does, if you are doing it by hand:

```sh
claude-memsync join request --repo <repo> --label <host-label>
```

This pushes one plaintext file, `joins/<host-label>.txt`, holding the new
host's public key and a fingerprint, and prints:

```text
fingerprint: 0123 4567 89ab cdef 0123 4567 89ab cdef
```

`claude-memsync join status --repo <repo>` shows `pending` until a member
accepts.

### 3. Compare fingerprints and accept, on a member

On a machine that already reads the repo:

```sh
claude-memsync join list --repo <repo>
```

Compare the fingerprint it shows with the one the new host printed, over a
channel you trust (read it off both screens yourself, not from a message an
agent relays). Only if they match:

```sh
claude-memsync stop
claude-memsync join accept <host-label> --repo <repo> --fingerprint "0123 4567 89ab cdef 0123 4567 89ab cdef"
claude-memsync start
```

`accept` adds the key with its host label, re-encrypts the current tree to the
new set, removes the request file and pushes. It refuses while the daemon holds
the sync dir (hence `stop`), refuses a mismatched fingerprint, and refuses
while some recipient other than a configured recovery key has no host label.
`join reject <host-label> --repo <repo>` discards a request.

### 4. Accept the recipient set, on the new host

The new host's next pull brings the new recipient list, which it has not
reviewed yet, so it still holds off. Look before you accept:

```sh
claude-memsync join status --repo <repo>       # every recipient, new ones marked
```

Compare those lines with `join list` and `recipients --repo <repo> list` on
the member. Anyone who can push to the repo could have added a key besides
yours. Accept only if every key is one you expect:

```sh
claude-memsync stop
claude-memsync recipients --repo <repo> accept --yes
claude-memsync start
```

The host then starts to commit and sync.

## Throwaway VMs

A VM that should only read notes, and only for a while:

```sh
claude-memsync init --remote git@gh-mem-personal:<your-org>/mem-personal.git \
  --sync-dir ~/.claudesync.d/personal --encrypt --read-only \
  --host-label <host-label> --expires 2026-12-31
```

- **Read-only.** The daemon pulls and applies notes, and never commits or
  pushes. Give it a deploy key **without** write access (leave out `-w` in
  `gh repo deploy-key add`) so GitHub itself refuses a push.
- **Expiry.** From the day after `--expires` (UTC) the daemon stops syncing and
  `status` says `expired: retire me`. With a registry, the date is published
  so other hosts flag it in `host list`.
- **Getting its key accepted.** A read-only key cannot push a join request.
  Send the public key `init` printed to a member over a trusted channel and add
  it there with its label, then accept on the VM (on a read-only host this only
  pins the set):

  ```sh
  # on a member
  claude-memsync recipients --repo personal add <age1pq1...> --host <host-label>
  # on the VM
  claude-memsync recipients --repo personal accept --yes
  ```

  Compare the key's fingerprint on both sides (`recipients list` on the VM
  shows the change it is about to accept).

Do not give a VM the client repos unless it does that client's work, and
retire it when you are done.

## Retiring a host

From any other member host (a host cannot retire itself):

```sh
claude-memsync host list --org <your-org>                 # who is there, stale or expired
claude-memsync host retire <host-label> --org <your-org>  # dry run: the plan only
claude-memsync stop
claude-memsync host retire <host-label> --org <your-org> --apply
claude-memsync start
```

`--apply` deletes every deploy key titled `<host-label>` on every `mem-*`
repo, removes the host's recipient from each repo this host can re-encrypt
(and lists the ones it cannot, to finish on another member), and removes
`hosts/<host-label>/` from the registry. It needs a token with Administration:
write. The token is taken from the config's sources, `githubApp` first: on a
host whose config names `memsync-check` (Metadata only), point it at the admin
App or a token with that permission for this run.

Two steps are manual:

- **Delete the host's `memsync-check` key** in the GitHub UI. The plan prints
  the exact settings page and the fingerprint recorded in the registry; check
  that fingerprint against what GitHub shows before deleting.
- **Treat what it read as read.** A retired key still decrypts every commit it
  ever fetched. If the host was lost or compromised, rotate any secret its
  notes mentioned.

A re-run is safe: done steps are skipped, and it ends with `retire complete`
or a `still pending` list.

## Break-glass: join self (planned)

> **Planned, not yet available.** `claude-memsync join self` will let a host
> join when no other member is online, by unlocking the recovery identity in
> memory: from your password manager (`--recovery-cmd`) or from a
> passphrase-wrapped copy kept in a separate repo that no host has a deploy
> key for. The passphrase must be generated, not chosen by hand: 10 or more
> random words, checked against an entropy estimate. Until it ships, a host with no member
> to accept it needs the recovery key on a trusted machine to run
> `recipients add` there.

## Upgrading the binary

```sh
cd ~/.local/src/claude-utils && git fetch && git checkout <commit-or-tag>
go build -o ~/.local/bin/ ./cmd/claude-memsync ./cmd/claude-memmerge
claude-memsync stop
claude-memsync start
claude-memsync version && claude-memsync status
```

Or download a release archive and check it against `SHA256SUMS` first (see
"Releasing" in the README). On start the daemon rewrites each encrypted
clone's filter and merge-driver settings to point at the binary that is
running, so an upgrade that moved the binary (for example a new versioned
directory from the bootstrap) needs no `init --force`. A binary run from a temp
dir or a Go build cache is never written into the git config. Upgrade every
machine; a host whose binary predates a repo's encryption scheme cannot read
it.

## Converting a classic repo to post-quantum

New repos are post-quantum (`encryptionScheme: pq`). A repo made earlier with
classic X25519 keys is converted on its one member host:

```sh
claude-memsync stop
# dry run: prints what would change
claude-memsync recipients rekey-pq --repo <repo> --recovery-recipient <age1pq1...> --host <host-label> --fresh-root
# then the same command with --apply (or --all instead of --repo for every repo)
claude-memsync recipients rekey-pq --repo <repo> --recovery-recipient <age1pq1...> --host <host-label> --fresh-root --apply
claude-memsync start
```

Try one repo first with `--repo`, and only then use `--all`. Do not run
`status` (or `git status` in a sync directory) with an old binary between the
rekey and the restart: an old binary's filter cannot read the new hybrid
ciphertext and would show every note as changed. Upgrade the binary, then
`claude-memsync start`.

`rekey-pq` refuses a repo with more than one member host: retire the others,
convert, then let them join again with fresh hybrid keys. It also refuses a
dirty work tree or unpushed commits, and drops pending join requests. It runs
every check that could refuse the push before it changes anything, writes a
new hybrid identity (`identity-pq.*`) beside the old one and points the repo at
it, swaps the whole recipient set (age cannot mix hybrid and classic recipients
in one file), and re-encrypts the current tree. Optionally it replaces the
history with one fresh root commit, so the remote no longer serves classically
encrypted notes; see the caveats on force-pushes in
[security.md](security.md#old-history). `config.json` is saved only after the
push succeeds.

If something fails part way:

- With `--all`, an `--apply` run **stops at the first repo that fails** and
  lists which repos are done, which failed and which were not started.
- **Rerun the same command** (same `--host`, `--recovery-recipient`,
  `--fresh-root`). Finished repos are skipped; a repo that was migrated locally
  but whose push failed gets its push retried; a run that crashed between
  moving HEAD and repointing the identity, scheme and pin is finished (every
  resume repoints again and resets an index left on the old blobs). A dry run
  on such a repo says the earlier apply was interrupted and how to finish or
  undo it.
- A rerun after the daemon has synced on top of the migration reports
  `already done` and retires the classic identity (the record names the
  migrated commit; an origin/main that contains it counts as published). If
  the crash came before `config.json` and the stage were saved, the rerun
  writes both. It never fails the commit-count check. A record from an older
  build names no commit and no stage: for a repo that is already post-quantum,
  with origin/main past the backup tip, the migrated commit is inferred from
  the history (with `--fresh-root`: the roots origin/main has that the backup
  tip does not; otherwise the commits right after the backup tip), but only a
  commit whose `.age-recipients` is exactly this host's new key and the
  recovery key counts, exactly one must, and this clone's HEAD must contain it.
  If that cannot be established the rerun stops with "cannot tell which commit
  was the migration; nothing changed" (exit 1) and the daemon stays held. Then
  the record is completed, and the flags and `config.json` are
  not touched.
- To give up on a repo that was **not pushed**, run
  `claude-memsync recipients rekey-pq --repo <repo> --rollback`: it resets the
  branch to the pre-migration tip and restores the identity, scheme, pin and
  `config.json`. If commits were made after the rekey it refuses, listing their
  count and subjects; add `--discard-local-commits` to drop them. It needs the
  classic identity: it renames `<id>.classic-retired` back, and refuses if
  neither file exists. Once the remote has moved off the backup, rollback is
  refused (with `--all` such a repo is reported as `published; nothing to roll
  back` and the run goes on); the restore record is `.git/memsync-pre-pq.json`.
- While a rekey is unfinished (a record that is not `published`, or a repo that
  is not post-quantum yet) the daemon syncs nothing in that repo: it logs
  `rekey-pq: not syncing this repo` every cycle and `status` shows the same.
  `--apply` also refuses to start while `.state/parked.json` or
  `.state/pull-running` exists: start the daemon, let one cycle finish so it
  recovers, stop it and rerun.
- If the record itself is unreadable the message says so and names
  `refs/memsync/pre-pq`: do **not** `--drop-backup`, it is all that is left of
  the classic history. Inspect the repo and finish or undo the migration by hand.
- If the push succeeded but the check afterwards failed, the output says
  `pushed; verification failed`; look at origin/main before relying on it.

The old classic identity file is renamed `*.classic-retired` only when no git
clone beside the sync directories (routed or not, such as a `*.incident-*`
copy) still points at it; otherwise it is kept, the clone is named, and a rerun
retires it later; a clone whose unfinished restore record names the file also
keeps it. To retire it once nothing uses it, without any rekey flags, run
`claude-memsync recipients retire-classic [--repo <repo> | --all] [--apply]`
(a dry run without `--apply`). The pre-migration tip stays at `refs/memsync/pre-pq` until
`rekey-pq --repo <repo> --drop-backup`; that removes the classic ciphertext from
the local clone (after a `--fresh-root` run) without touching stash entries.

Classic X25519 is chosen on purpose only, with
`init --encrypt --encryption-scheme classic` (saved as
`encryptionScheme: classic`). Under `pq`, classic and ssh keys are refused
everywhere a key is added.
