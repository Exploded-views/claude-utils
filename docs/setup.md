# Setting up claude-memsync

This guide takes you from nothing to several machines sharing encrypted,
layered memory. Read [fork.md](fork.md) first for the model (repo kinds,
routing, default deny) and [security.md](security.md) for what each key can do.
Every flag shown here is in [reference.md](reference.md).

Placeholders: `<org>` is the GitHub org you create for memory,
`<host-label>` the name you give a machine (1 to 32 of `a-z`, `0-9`, `-`,
never its real host name), `<client-codename>` a neutral name for a client,
`<password-manager store command>` / `<password-manager read command>` your
password manager's CLI commands that store a secret from stdin and print it
back.

Contents:

- [What you need](#what-you-need)
- [The config directory](#the-config-directory)
- [macOS notes](#macos-notes)
- [First machine](#first-machine)
- [Each extra machine](#each-extra-machine)
- [Throwaway VMs](#throwaway-vms)
- [Retiring a host](#retiring-a-host)
- [Break-glass: join self (planned)](#break-glass-join-self-planned)
- [Upgrading the binary](#upgrading-the-binary)
- [Converting a classic repo to post-quantum](#converting-a-classic-repo-to-post-quantum)

To install from a downloaded release archive instead of building (no Go, no
clone), see [releases.md](releases.md).

## What you need

- git 2.26 or later, Go 1.25 or later (to build; not needed when installing a
  [release archive](releases.md)), `gh` logged in, `ssh-keygen`, `ssh-keyscan`,
  `python3` (the bootstrap uses it), and `age-keygen` 1.3 or later (for
  `-pq`; `go install filippo.io/age/cmd/age-keygen@v1.3.2`). On a host the
  bootstrap also needs `systemctl` (Linux) or `launchctl` (macOS) to register
  the service. The bootstrap checks all of this first and in a dry run, names
  every missing tool, and changes nothing ([bootstrap.md](bootstrap.md#prerequisites)).
- A password manager with a CLI that can store a secret read from stdin and
  print it back. The recovery key and the admin App's key live there and
  nowhere else. A host without one can use a short-lived token file instead
  ([bootstrap.md](bootstrap.md#c-homelab-host-without-a-password-manager-token-file)).
  When a key or token command waits for the password manager (a prompt to
  approve), the command prints `waiting for your password manager (approve the
  prompt if one appears)…` and waits up to 120 s at a terminal; set
  `CLAUDE_MEMSYNC_SECRET_TIMEOUT` (a Go duration such as `3m`) to change that.
  The daemon, which nobody can answer, keeps 10 s; hooks and the doctor timer
  never wait either (they use cached verdicts and the short limit).
- The same project paths on every machine: Claude names a project's memory
  folder after its absolute path.

## The config directory

What is per host and never synced (`routes.json`, `apps.json`, the age identity
and key files) lives in one directory on every OS:
`${XDG_CONFIG_HOME:-~/.config}/claude-memsync`, on Windows too
(`%USERPROFILE%\.config\claude-memsync`). Earlier builds kept the age identity
in the OS's own config directory, `~/Library/Application Support/claude-memsync`
on macOS and `%AppData%\claude-memsync` on Windows. An identity still there is
read in place (a second one is never created), and `status` prints one hint
saying so. The identity stays where it is and keeps working; do not move it by
hand, because each clone's git config holds its absolute path (a safe
`identity move` is planned). Details: "The config directory" in [claude-memsync.md](claude-memsync.md#the-config-directory).

## macOS notes

- **Nothing to install beyond git, `gh`, `python3` and Go (or a release
  archive).** The stock `/bin/bash` 3.2 and the BSD tools (`grep`, `sed`, `awk`,
  `stat`, `find`) are enough; no GNU coreutils, no Homebrew bash. CI runs the
  bootstrap's checks under `/bin/bash` 3.2 on a macOS runner and a lint keeps
  GNU-only flags out of `scripts/` ([ci.md](ci.md)).
- **Run the paste prompts' commands in bash, not zsh.** The keep-out lists are
  split into repeated flags by bash's word splitting, which zsh does not do.
  Type `bash` first.
- **Keep the password manager unlocked and in view** while the bootstrap or an
  `app` command runs: the admin key is read from it at run time and its prompt
  can need you. The command waits 120 s for it (above).
- **A standalone binary in `~/.local/bin` is moved aside automatically.** If
  `claude-memsync` or `claude-memmerge` there is a regular file (not a link),
  the bootstrap renames it to `<name>.pre-bootstrap-<UTC time>` (a taken name
  gets `.1`, `.2`, …) and links its own build in its place. A link is simply
  re-pointed; anything else is refused. The dry run lists the move.
- **`/reload-plugins` after install.** The bootstrap installs the
  `memory-notices` plugin; run `/reload-plugins` in each open Claude session,
  or restart the sessions, so its hooks load. The summary says so.
- **Where the identity lives.** New installs: `~/.config/claude-memsync/`
  (or `$XDG_CONFIG_HOME/claude-memsync/`), same as `routes.json`. A Mac set up
  before that rule has it in `~/Library/Application Support/claude-memsync/`; it
  keeps working there; leave it in place (above).

## First machine

Do this once, on a machine you own and trust (not an agent host).

### 1. Create the org and its settings

Create a GitHub organization used only for memory, for example
`<org>`. Some of the settings below are only offered on paid plans;
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
claude-memsync app register --name memsync-admin --org <org> --url <homepage-url> \
  --perm administration:write --perm metadata:read \
  --key-cmd-store "<password-manager store command>" \
  --key-cmd "<password-manager read command>"
```

**`memsync-check`** (Metadata: read only). Every host uses it for the
private-only checks; it can change nothing. Each host gets its **own** private
key, so a lost host is cut off by deleting one key:

```sh
claude-memsync app register --name memsync-check --org <org> --url <homepage-url> \
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

Run it in bash (not zsh). The same shape is used by the paste prompts in
[bootstrap.md](bootstrap.md): INPUTS first, then what is already set, then steps
that use only the names. The command that runs twice is defined once.

INPUTS (fill in; nothing else changes)

```sh
ORG=<org>                          # the GitHub organisation that holds the memory repos
HOST_LABEL=<host-label>            # this host's label
RECOVERY=<recovery-recipient>      # your offline post-quantum recovery PUBLIC key, age1pq1...
GITHUB_USER=<github-user>          # your personal GitHub account
ADMIN_APP_ID=<admin-app-id>        # numeric App ID of memsync-admin
ADMIN_KEY_CMD="<password-manager read command>"   # prints the memsync-admin private key
CHECK_APP_ID=<check-app-id>        # numeric App ID of memsync-check
CHECK_KEY_FILE=<check-key-file>    # absolute path of this host's memsync-check key, mode 0600
KEEP_OUT_OWNERS=""                 # optional, empty for none: space-separated GitHub owners to keep out
KEEP_OUT_TERMS=""                  # optional, empty for none: space-separated terms to keep out
```

ALREADY SET: nothing; the script is the checkout you are in.

STEPS

1. Define the command once.

   ```sh
   boot() {
     local a=() x
     for x in $KEEP_OUT_OWNERS; do a+=(--keep-out-owner "$x"); done
     for x in $KEEP_OUT_TERMS; do a+=(--keep-out-term "$x"); done
     bash scripts/bootstrap.sh --host "$HOST_LABEL" --role personal-dev --org "$ORG" \
       --recovery-recipient "$RECOVERY" --own-owner "$GITHUB_USER" ${a[@]+"${a[@]}"} \
       --org-app memsync-admin --org-app-id "$ADMIN_APP_ID" --org-app-key-cmd "$ADMIN_KEY_CMD" \
       --check-app memsync-check --check-app-id "$CHECK_APP_ID" --check-key-file "$CHECK_KEY_FILE" "$@"
   }
   ```

2. Dry run and read the plan.

   ```sh
   boot --dry-run
   ```

3. Wait for your go. Then the same command with `--yes`.

   ```sh
   boot --yes
   ```

`--org` and `--recovery-recipient` are required and have no defaults; the
script refuses to run without them. The recovery recipient must be the
hybrid (`age1pq1...`) public key of the recovery identity you hold.
`--own-owner <github-user>` names your personal GitHub account so its checkouts
get project repos; the only owner counted as yours by default is `--org`.

**Keep-out lists are optional.** `--keep-out-owner` and `--keep-out-term` are
repeatable, and both may be left out. An owner makes a discovered checkout whose
origin owner matches it (case-insensitive) get no repo and no route, so default
deny applies. A term does the same for a checkout whose `owner/repo` contains
it (case-insensitive substring), and is also written, regex-escaped, to the
`denylist-<kind>.txt` files the daemon checks at commit time: a note whose
content or repo-relative path matches is held back (it stays local and is never
committed or pushed). Owners are **not** written to the denylist; only terms
are. With neither flag the script prints the calm note
`keep-out lists: none (routing alone decides what syncs)` and goes on; that is
not a warning. (`--deny-owner` and `--deny-term` are the old names and still
work.)

**Exit codes:** 0 ok, 1 error (a missing prerequisite, a refused input, a
failed step), 2 usage (an unknown or malformed argument), 3 paused. Exit 3
means it needs a human before it can go on, today because the
`--check-key-file` does not exist yet; it says exactly what to do and changes
nothing. Do that and run the same command again. A host without a password
manager uses a token file instead of `--org-app` (`--org-token-file <path>
--delete-org-token-file`; a dry run keeps the file, a real run deletes it, the
token expires about an hour after it was minted); the flow, the full prerequisite
list and the release-archive flow are in [bootstrap.md](bootstrap.md) and
[releases.md](releases.md).

The manual path, for one repo (repeat per repo):

```sh
# a private mem-* repo, verified private after creation
claude-memsync repo create --org <org> \
  --token-cmd "claude-memsync app-token --app memsync-admin" mem-personal

# a deploy key for this host and repo (write access), behind its own ssh alias
ssh-keygen -t ed25519 -N '' -f ~/.ssh/mem-personal_ed25519
GH_TOKEN="$(claude-memsync app-token --app memsync-admin)" \
  gh repo deploy-key add ~/.ssh/mem-personal_ed25519.pub -R <org>/mem-personal -w -t <host-label>
# ~/.ssh/config:  Host gh-mem-personal / HostName ssh.github.com / Port 443 /
#                 User git / IdentityFile ~/.ssh/mem-personal_ed25519 / IdentitiesOnly yes

# the encrypted sync dir
claude-memsync init --remote git@gh-mem-personal:<org>/mem-personal.git \
  --sync-dir ~/.claudesync.d/personal --encrypt --host-label <host-label> \
  --recovery-recipient <age1pq1...> --archive-deletes --private-only \
  --github-app memsync-check
```

`init --encrypt` creates this host's own hybrid identity in
`${XDG_CONFIG_HOME:-~/.config}/claude-memsync/` (mode 0600) and prints only its
public key; see [the config directory](#the-config-directory) for an identity
left in the older macOS or Windows location.
With several repos, write
`~/.config/claude-memsync/routes.json` (see "Several repos per host" in
[claude-memsync.md](claude-memsync.md#several-repos-per-host)), pin the home
folder, and start the service:

```sh
claude-memsync route home personal --yes
claude-memsync install && claude-memsync start
claude-memsync status
claude-memsync doctor --org <org>
```

`doctor --install-timer` writes a daily check of every `mem-*` repo's
visibility.

### Back up the host identity

The host identity is the only copy of the key that opens this host's notes
(the recovery key is a second way in, but only for repos that list it). Lose
the file and the host cannot read its own repos. Put a copy in your password
manager with `identity backup`. Run `--dry-run` first: it lists every file
that would be stored, why it is on the list, and with which command, and runs
nothing.

A host usually has more than one identity file, and a manager that replaces an
item by name would keep only the last. So when more than one file is found,
the item's title must differ per file: put `{fingerprint}` (the identity's
public-key fingerprint, with dashes for spaces) or `{name}` (the file's base
name) in the title, in both the store and the verify command. Without one of
them, the command refuses to run (exit 2) and stores nothing.

For 1Password (`op`), with placeholders for your own vault:

```sh
claude-memsync identity backup --dry-run \
  --store-cmd "op document create - --title memsync-identity-{fingerprint} --vault <vault>" \
  --verify-cmd "op document get memsync-identity-{fingerprint} --vault <vault>"
```

Drop `--dry-run` to do it. For any other manager, give the command that
creates an item from stdin and the command that prints it back, each as you
would type it (they are split into arguments and never run through a shell,
so `$VAR`, pipes and `~` are not expanded):

```sh
claude-memsync identity backup \
  --store-cmd "<manager-cli> create-from-stdin --title memsync-identity-{fingerprint}" \
  --verify-cmd "<manager-cli> print --title memsync-identity-{fingerprint}"
```

The command hands the identity file's bytes to the store command on its
standard input and nowhere else: not in an argument, the environment, the
output or a log. With `--verify-cmd` it runs that command, which must print the
stored bytes on stdout, and compares SHA-256 digests; each file ends in
`verified (sha256 matches)` or `MISMATCH`. Without `--verify-cmd` it warns that
the backup is **UNVERIFIED**: the store command only reported success, nothing
read the copy back. Each file prints its path, its public-key fingerprint
(grouped as in `join`), the result, and why it is on the list. The last line
of a run is a summary such as
`3 stored (3 verified, 0 UNVERIFIED), 0 failed`. The exit status is 0 only if
every file was stored (and verified, when asked); a stored but UNVERIFIED file
still exits 0, so read the summary. Your manager may prompt you to approve; the
command waits up to 120 seconds (`CLAUDE_MEMSYNC_SECRET_TIMEOUT` changes that).

Which files:

- By default, every distinct `memage.identity` named by this host's clones,
  plus the host's default identities that related read clones decrypt with
  (`identity-pq.*` and `identity.*` in the config directory, if present), plus
  any classic identity or `*.classic-retired` file that `rekey-pq` keeps. The
  classic file is the key to the pre-migration history and the rollback point,
  so keep it until you have dropped that backup (`rekey-pq --drop-backup`).
  Each is labelled: `used by: <repos>`, `related clones`, or
  `classic history and rollback`.
- A clone that is not a git repo, or has no `memage.identity` (it is not
  encrypted), prints `skipped: <dir> (<reason>)`; nothing is skipped silently.
- `--repo <name>` or `--config <path>` backs up that one clone's identity (and
  its classic files); `--identity <path>` names one file. `--all` is the
  default and may be omitted. Clones that share a file store it once.
- A `memage.identity` that is a symlink is refused, like `--identity`: name the
  real file.
- A file with any permission beyond 0600, not owned by you, with more than one
  hard link, or that does not parse as an age identity is refused, and the
  store command is not run for it.
- The verify command must print the exact bytes. A manager that adds or trims a
  trailing newline reports `MISMATCH`; the stored copy may still be fine, so
  restore it once by hand and compare to be sure.
- If a store or verify command fails, run it by hand to see why: its stderr is
  discarded on purpose, so the key can never reach a log through it.
- Run it again after `rekey-pq`, which creates a new hybrid identity file and
  leaves the classic one (renamed `*.classic-retired` once no clone uses it).

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
claude-memsync join accept <host-label> --repo <repo> --fingerprint "0123 4567 89ab cdef 0123 4567 89ab cdef"
```

`--repo` takes the routes key or the GitHub repo name (`mem-project-x`, or
`owner/name`). If the daemon is running as a service, `accept` stops it, waits
for the lock, makes the change and starts it again, on every exit path; it never
starts a service that was not running. When it is done it prints
`accepted by host:<label>, key fingerprint <fp>`; the new host's `join status`
must show that key. `accept` adds the key with its host label, re-encrypts the current tree to the
new set, removes the request file and pushes. It refuses if something other than the service holds
the sync dir, refuses a mismatched fingerprint, and refuses
while some recipient other than a configured recovery key has no host label.
`join reject <host-label> --repo <repo>` discards a request.

### 4. Accept the recipient set, on the new host

The new host's next pull brings the new recipient list, which it has not
reviewed yet, so it still holds off. Look before you accept:

```sh
claude-memsync join status --repo <repo>       # every recipient with its label, new ones marked
```

Each line carries the key's label (`host:<label>`, `recovery` or `unlabelled`)
and `this host` on your own key. A `.gitattributes` change is listed as a
change to review, not as a key; it is expected on a first join.

Compare those lines with `join list` and `recipients --repo <repo> list` on
the member. Anyone who can push to the repo could have added a key besides
yours. Accept only if every key is one you expect:

```sh
claude-memsync recipients --repo <repo> accept --yes
```

The host then starts to commit and sync.

## Throwaway VMs

A VM that should only read notes, and only for a while:

```sh
claude-memsync init --remote git@gh-mem-personal:<org>/mem-personal.git \
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
claude-memsync host list --org <org>                 # who is there, stale or expired
claude-memsync host retire <host-label> --org <org>  # dry run: the plan only
claude-memsync stop
claude-memsync host retire <host-label> --org <org> --apply
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
[releases.md](releases.md)). On start the daemon rewrites each encrypted
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
