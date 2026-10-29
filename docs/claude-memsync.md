# claude-memsync — syncing Claude Code memories across workstations

> See also: [fork overview](fork.md) · [setup guide](setup.md) · [security model](security.md) · [command reference](reference.md) · [changelog](../CHANGELOG.md)

A guide to setting up and running the memory-sync daemon.

## What this is for

Claude Code stores per-project memories under
`~/.claude/projects/<project-hash>/memory/`. If you work across more than one
machine, those memories live only on whichever PC learned them. `claude-memsync`
is a small background daemon that keeps that memory tree in sync across all your
workstations, using a **private git repository as transport**. A custom git
merge driver (`claude-memmerge`) unions `MEMORY.md` section blocks instead of
producing line-level conflicts.

It's a single Go binary, runs on Windows, Linux, and macOS, and needs no server
of its own — just a git remote you can push to.

> Want to reuse environment-level lessons across *different* projects (not just
> sync the same project across machines)? That's a separate capability — see
> [distilling-memories.md](distilling-memories.md).

## Prerequisites

- Go 1.25+ to build (the `go` line in `go.mod`), or a [release archive](releases.md) instead.
- `git` 2.26 or later on PATH at runtime (the daemon shells out to it; older
  versions record a rebase's start in the reflog differently, so the daemon
  could not tell its own stopped rebase from yours and would pause instead).
- A private git remote you can `git push` to from your terminal. The daemon
  inherits your normal git credentials (Git Credential Manager on Windows, SSH
  agent or `~/.gitconfig` credential helpers on Linux/macOS). Confirm `git push`
  works against the remote before installing.
- The same project paths on each PC. Claude's project-hash directory names are
  derived from absolute paths, so if your repos live at the same drive letter /
  mount point on every PC, you're set. (See [Limitations](#limitations--known-issues).)

## Setup

### 1. Create the private remote (once, from any PC)

Any empty private git repo works. With the GitHub CLI:

```sh
gh repo create <you>/claude-sync --private --description "Private Claude Code memory sync"
```

### 2. Build (each PC)

```sh
git clone -b integ/next https://github.com/Exploded-views/claude-utils.git
cd claude-utils
go build -o bin/ ./cmd/...
```

For a release build that stamps the version into the binary:

```sh
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin/ ./cmd/...
```

A plain `go build` (no ldflags) still produces a working binary.
`claude-memsync version` then falls back to the version Go embeds automatically: a
pseudo-version such as `v0.2.1-0.20260508213755-a52d68630b31` (Go 1.24 or later,
in a git checkout), or `dev+<commit>` where the toolchain gives none. See
[How version resolution works](releases.md#how-version-resolution-works) for details.

Both binaries (`claude-memsync` and `claude-memmerge`) end up in `bin/`.
**They must live in the same directory** — `claude-memsync` finds the merge
driver as a sibling. Move both to a stable location if you don't want them tied
to the source checkout, e.g.:

- Windows: `C:\Program Files\claude-memsync\` (or anywhere on PATH)
- Linux/macOS: `~/.local/bin/`

### 3. Initialize the local sync repo (each PC)

```sh
claude-memsync init --remote https://github.com/<you>/claude-sync.git
```

This:

- Clones the remote into `~/.claudesync/`
- Configures the `MEMORY.md` merge driver in the local git config
- Mirrors any existing `~/.claude/projects/<hash>/memory/` content into the
  work-tree
- Writes `~/.claudesync/config.json` (per-PC; never synced)
- Writes `~/.claudesync/.state/manifest.json` (per-PC; never synced)
- Pushes the seed commit if this is the first PC

On a second or third PC where the remote already has content from another
workstation, init handles collisions:

- `MEMORY.md` present on both sides → semantic merge (sections from both PCs are
  unioned). If a section was edited on one side only, that is a conflict here
  (there is no common base): the local version is taken and the mirror copy is
  kept as `<name>.from-remote-<random>`, like any other file. Conflict markers
  are never written.
- Other memory files differing on both sides → mirror copy is preserved as
  `<name>.from-remote-<random>` for manual review; the local version is taken

### 4. Install and start the daemon (each PC)

```sh
claude-memsync install
claude-memsync start
claude-memsync status
```

`install` registers the daemon to auto-start when you log in:

- **Windows**: drops `claude-memsync.vbs` in
  `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\`. The script launches
  the daemon hidden and detached via `WScript.Shell.Run`, so no console window is
  left behind at logon. No admin required. (Task Scheduler was investigated; it
  requires admin even for per-user logon tasks, so the Startup folder is the
  simpler path.)
- **Linux**: writes a systemd user unit at
  `~/.config/systemd/user/claude-memsync.service` (`WantedBy=default.target`)
  and enables it. Re-running `install`, or `start`, repairs a unit an older
  version wrote with `multi-user.target`.
- **macOS**: writes a launchd plist at
  `~/Library/LaunchAgents/claude-memsync.plist`.

All three run as your logged-in user, with full access to your credential vault
and SSH keys. None require root.

## Lifecycle commands

| Command | What it does |
|---|---|
| `claude-memsync init --remote <url> [--denylist <file>]` | One-time bootstrap. `--denylist` installs a [denylist](#denylist) before the first commit. |
| `claude-memsync install` | Register for auto-start at next logon. |
| `claude-memsync uninstall` | Remove the auto-start hook. |
| `claude-memsync start` | Start the daemon now. |
| `claude-memsync stop` | Stop the running daemon. |
| `claude-memsync status` | Print `running (pid N)`, `stopped` or `service: not installed` (a foreground or dry run), then the routing and per-repo state. Exits non-zero only when a repo is in an error state (its loop keeps failing) or the routes file is unusable. |
| `claude-memsync run` | Run in the foreground (for debugging). |
| `claude-memsync changes --since <rev>` or `--marker <file>` | List memory files changed in the sync repo since a point (`--project`, `--json`, `--limit`, `--update-marker`, `--hook`). See [Telling a running session what changed](#telling-a-running-session-what-changed). |
| `claude-memsync distill` | Rebuild the distilled-memory catalog index (`--prune`, `--dry-run`, and with several repos `--repo <name>` or `--config <path>`, `--routes <path>`). See [distilling-memories.md](distilling-memories.md). |
| `claude-memsync version [--json]` | Print the version. See [version output](#version-output). |

## Version output

`claude-memsync version` prints one line, `claude-memsync <version>`, for
example `claude-memsync v0.3.0-ev.1`. `claude-memsync version --json` prints
exactly one JSON object on one line, with these keys in this order:

```json
{"name":"claude-memsync","version":"v0.3.0-ev.1","commit":"<40 hex characters, or empty>","go":"go1.27.2","os":"linux","arch":"amd64"}
```

`version` is the same string the text line shows. `commit` is the VCS revision
Go embedded in the build, and is `""` when the build has none (a build from a
source archive, or with `-buildvcs=false`). `go`, `os` and `arch` come from the
Go runtime. `claude-memmerge version --json` prints the same object with
`"name":"claude-memmerge"`. `version` takes no other argument (anything else
exits 2 without acting).

Both forms are a stable interface for scripts: the text line's format and the
JSON keys will not change without a "breaking" note in the
[CHANGELOG](../CHANGELOG.md).

## How it works

```
~/.claude/projects/<hash>/memory/         (Claude reads + writes here — authoritative)
                  │
                  │  fsnotify watcher, 3s debounce
                  ▼
~/.claudesync/projects/<hash>/memory/     (mirror — git work-tree)
                  │
                  │  git add -A, commit, pull --rebase, push
                  ▼
                <remote>                  (private GitHub repo)
                  │
                  │  ls-remote check on 1h timer (or after local push);
                  │  full pull only when remote SHA actually moved
                  ▼
   propagate pull-driven changes back to ~/.claude/projects/...
```

Idle ticks cost a single `git ls-remote` round-trip — no pull, no push, no merge
driver invocation. The full pull/push cycle runs only when origin's branch SHA
differs from the local `refs/remotes/origin/<branch>` or when there are unpushed
commits.

- The daemon never edits files in `~/.claude/projects/...` while a write is in
  progress — it copies into the mirror first, then any inbound changes from a
  `git pull` are written back atomically.
- The custom merge driver is registered in the local repo config and invoked by
  git via `.gitattributes` (`MEMORY.md merge=claude-memory-index`).
- `MEMORY.md` edited on both PCs is merged by section on every cycle (Reconcile),
  not only at init. A merge that is clean is written to both sides. When it
  conflicts, e.g. a section edited on one side only, which has no common base,
  the `~/.claude` copy wins and the mirror copy is kept as
  `<name>.from-remote-<random>`, once however often the same copy is set aside.
  Conflict markers are never written.
- Two PCs editing the same lines of the same committed file is rare in practice
  (each memory file has a unique filename per topic). When it happens the pull
  never leaves conflict markers in your files; see
  [Rebase conflicts](#rebase-conflicts).

## What gets synced (and what doesn't)

**Synced:** every file directly under
`~/.claude/projects/<project-hash>/memory/` on any PC.

**Not synced:**

- `~/.claude/CLAUDE.md` (your global instructions)
- `~/.claude/agents/`, `~/.claude/commands/`, `~/.claude/skills/`
- `~/.claude/sessions/`, `~/.claude/todos/`, `~/.claude/cache/`,
  `~/.claude/history.jsonl`, `~/.claude/settings.json`, etc.
- `~/.claudesync/config.json` — per-PC (paths embed your machine layout)
- `~/.claudesync/.state/manifest.json` — per-PC (delete-detection state)
- `~/.claudesync/.state/synced-head` and `.state/pull-running` — per-PC (the
  last commit reflected in `~/.claude`, and a marker around the daemon's own
  pull)
- `~/.claudesync/.state/parked.json` and `.state/parked/` — per-PC (in-flight
  [parking](#parking-held-back-files-around-the-pull) state, normally absent;
  `.state/parked.broken-*/` holds a park that was set aside)
- `~/.claudesync/denylist.txt` — per-PC (your [denylist](#secret-scrub-and-denylist))
- `~/.claudesync/daemon.pid` — runtime state
- `~/.claudesync/distilled/DISTILLED.md` — derived index, regenerated per-PC
  (the distilled `<slug>.md` entry files themselves **are** synced)

If you want any of the additional `~/.claude/` content synced, that's a future
enhancement.

## Secret scrub and denylist

A private remote is still a bad home for credentials, and git history is
forever. Before each commit the daemon scans every added or modified file in
the mirror. A file that looks like it holds a secret, or whose contents or
file name match your denylist, is **left out of the commit**. Everything else
in the same cycle commits normally, and the held-back file is checked again on
the next cycle, so it syncs as soon as it no longer matches.

Your copy under `~/.claude/projects/...` is the authoritative one. A file being
held back never changes or deletes it, with one exception: a held-back
`MEMORY.md` that merges cleanly with a peer's change is also written there (see
step 5 of the next section). The mirror copy is not left alone, though.

### Parking held-back files around the pull

A held-back file would otherwise sit modified (or untracked) in the sync
work-tree and collide with `git pull --rebase --autostash` when a peer touches
the same file: autostash conflicts on a changed file and cannot stash an
untracked one, so the pull would fail every cycle. So each cycle, when there is
something to pull or push, the daemon **parks** every uncommitted path in the
work-tree before the pull. This is path-agnostic: `projects/` memory files, the
`distilled/` catalog, and any other layout the work-tree holds. Deletions are
parked too.

1. Each dirty path's bytes are copied to `~/.claudesync/.state/parked/<path>`.
2. `~/.claudesync/.state/parked.json` (the paths, whether each was a file or a
   deletion, and the commit the work-tree was at) is written atomically and
   fsynced. Only then is the work-tree touched.
3. Each path is reset to the last commit, or removed if it was never committed.
4. The pull runs on a clean tree. Pulled changes are copied into `~/.claude`
   except for parked paths, which are skipped by their sync-repo path.
5. Each parked path is put back from its saved bytes, whatever the path. If the
   pull changed it, the incoming version is kept first: a `MEMORY.md` is merged
   3-way with the saved bytes (the same section-level merge as everywhere else,
   against the version from before the pull), but only if your `~/.claude` copy
   still equals the saved bytes. A clean merge is then written to both the
   mirror and `~/.claude`; this is the one case where parking writes to
   `~/.claude`. Otherwise (a conflicting merge, a `MEMORY.md` you changed
   since, a deleted file, any other file) the peer's version is kept as
   `<name>.from-remote-<id>` next to it, so committing your version later
   doesn't silently revert it. The `~/.claude` copy is not consulted for the
   restore: a later watcher event or reconcile syncs it to the mirror as usual.
6. The state is deleted last.

Step 5 runs in a `defer`, so it happens even if the pull or the copy back into
`~/.claude` fails, and it tries every path rather than stopping at the first
error. A path that cannot be restored stays recorded in `parked.json`; the next
cycle retries it before doing anything else, and nothing is committed or parked
over it meanwhile. The startup reconcile and the one on every pull tick are
skipped while a park is unfinished (the log says `reconcile skipped`), because
the parked mirror copies are not your versions.

That would block sync for good if the restore never works, so it gives up in
two cases (see [Recovering parked files](#recovering-parked-files)):

- `parked.json` cannot be parsed, or holds an invalid path or kind: it is moved,
  with `parked/`, to `~/.claudesync/.state/parked.broken-<unix time>/`, logged
  once as `PARKED STATE UNUSABLE`, and the cycle goes on.
- The restore of the same path fails on 3 consecutive cycles (the count is in
  `parked.json`): that path's saved bytes are moved to a new
  `.state/parked.broken-<unix time>/parked/<path>`, logged once as
  `PARKED FILES STUCK`, and sync resumes. For a memory file, the mirror copy is
  then refreshed from your `~/.claude` copy, so a `MEMORY.md` section you had
  removed does not come back.

A `.state/parked/` directory found with no `parked.json` beside it is never
deleted: the next park moves it to `.state/parked.broken-<unix time>/` and logs
it.

In practice:

- A held-back file is briefly absent from, or reverted in, the work-tree
  (`~/.claudesync/...`) during each pull. Don't edit there.
- If the peer deleted a parked file or renamed it, your version stays.
- If the daemon dies mid-cycle, nothing is lost: the park itself does not
  change your `~/.claude` copies, and the parked bytes and `parked.json` are
  on disk before the work-tree is changed. On the next start, before the startup reconcile, the
  daemon finishes the leftover park the same way. Whether the crash came before
  the reset, between the reset and the pull, or after the pull, the files
  return as you had them, and a file the pull did not change gets no spurious
  `.from-remote-*` backup.
- A rebase or merge the daemon's own pull left stopped (a crash) is aborted on
  start in all cases, and again at the start of every cycle, whether or not
  anything is parked: its work-tree holds conflict markers, which must never be
  staged. One the daemon did not start is yours, and is left alone (see
  [Rebase conflicts](#rebase-conflicts)). As a safety net
  the commit step also refuses to stage a file with conflict marker lines
  (`<<<<<<< `, `=======`, `>>>>>>> `) while a merge, a rebase or unmerged
  entries exist. Your local commits are kept.
- The denylist file itself is never parked.

### Recovering parked files

Nothing is deleted when a park is set aside. Look in
`~/.claudesync/.state/parked.broken-<unix time>/`:

- `parked.json` there lists the paths and whether each was a `file` or a
  `deleted` entry. A corrupt one may be unreadable; the saved bytes are still
  under `parked/`.
- `parked/<path>` holds the bytes of each file, with the path it had in
  `~/.claudesync` (for example `parked/projects/<hash>/memory/notes.md`).

For a memory file, your `~/.claude/projects/<hash>/memory/notes.md` is the
authoritative copy and normally still holds your version, so you only need the
saved bytes if that copy was changed afterwards. Compare, and keep what you want:

```
diff ~/.claudesync/.state/parked.broken-<unix time>/parked/projects/<hash>/memory/notes.md \
     ~/.claude/projects/<hash>/memory/notes.md
```

For a path outside `projects/` (the `distilled/` catalog, say) there is no
`~/.claude` copy, so copy the file back:

```
cp ~/.claudesync/.state/parked.broken-<unix time>/parked/distilled/entry.md \
   ~/.claudesync/distilled/entry.md
```

Then delete the `parked.broken-*` directory. They are never cleaned up by the
daemon, so remove old ones once you have checked them: each holds only the bytes
of files that could not be put back (copies of your memory files, possibly ones
you listed in the denylist because they were sensitive) and the `parked.json`
that listed them, so don't leave them lying around. If a path keeps landing there,
the cause is whatever stops it being written back (a file where a directory
should be, a permission, a full disk); fix that first.

### Rebase conflicts

Separate from held-back files: if you and another PC edit the **same lines of
the same committed file**, `git pull --rebase` stops on a conflict. The daemon
does not pick a side. It aborts the rebase (`git rebase --abort`), so the
work-tree never holds conflict markers and none reach `~/.claude`, and it keeps
your local commits as they were. For as long as the conflict is unresolved,
that cycle propagates nothing into `~/.claude` and pushes nothing, and the same
happens on every following cycle: **sync of your other notes is blocked, both
ways, until you resolve it.** The conflict itself changes nothing in `~/.claude`.

The conflict is logged once per cycle, by **count only**, because a path may
match your denylist:

```
pull: rebase conflict in 1 file(s); rebase aborted, local commits kept, nothing propagated or pushed until you resolve it (see docs: Rebase conflicts)
```

To resolve it, **stop the daemon first** (`claude-memsync stop`). This is
required, not a nicety. A cycle that finds a stopped rebase it did not start
takes it to be yours: it skips the whole cycle (nothing is staged, reconciled,
pulled or pushed) and logs `a rebase or merge is in progress in the sync repo
...` once. But a cycle checks at its start and again right before its pull, and
cannot see a rebase you start in between. While the rebase is stopped halfway
the work-tree holds conflict markers and `HEAD` is detached at some commit in
the middle of your resolution. Stopping the daemon takes it out of that race, so
that nothing is staged, parked, recorded or aborted behind your back. What the
daemon does guard, in case you did not, is this. It records no synced `HEAD` (see
below) while a rebase or merge is stopped or `HEAD` is detached, and logs a
detached `HEAD` once. It aborts only a rebase its own pull started. The pull is
a `git fetch`, then a marker, `~/.claudesync/.state/pull-running`, holding a
random nonce, then `git rebase --fork-point --autostash origin/<branch>` run
with `GIT_REFLOG_ACTION="memsync-pull <nonce>"`, which git writes into the
reflog of `HEAD` as the start of that rebase. `--fork-point` is what
`git pull --rebase` does too: commits that a force-push removed upstream (a
purged secret, say) are not replayed and pushed back. Ref logging is switched
on for these two commands whatever your global git config says. If the daemon
ever fails to recognise its own rebase, it errs the safe way: it leaves the
rebase alone and pauses as described below, and `rebase --abort` gets you out.
A stopped rebase is the daemon's only if the newest rebase start in that reflog (`git -C ~/.claudesync reflog`) carries the
marker's nonce. With no marker, or with a nonce that is not the newest start, it
is yours, whatever the timing: that holds for a marker a kill left behind, and
for a rebase you start during the daemon's fetch or after it. A merge is always
yours, since the daemon never merges. The daemon then skips the cycle, logs it
(the log names `git -C ~/.claudesync rebase --abort` and points here), and
`claude-memsync status` shows `syncing paused`. `claude-memsync stop` removes the
marker unless a rebase or merge is stopped.

```
git -C ~/.claudesync pull --rebase --autostash origin main   # stops on the conflict again
git -C ~/.claudesync status                                  # lists the conflicted files
# edit each conflicted file in ~/.claudesync, keeping what you want
git -C ~/.claudesync add <file>
git -C ~/.claudesync rebase --continue
```

`--autostash` is needed when a held-back file is modified in `~/.claudesync`:
without it git refuses to pull with "You have unstaged changes". Repeat `git
add` and `git rebase --continue` until git reports that the rebase is done: if
you have several local commits, it stops again on each one that conflicts. If
a file is a deletion on one side, `git add` keeps it and `git rm` removes it.

When the rebase is done, git applies the autostash. If it prints `Applying
autostash resulted in conflicts` (the peer changed a held-back file you also
changed), your edit is safe in the stash and in `~/.claude`, and the file in
`~/.claudesync` holds conflict markers. Your `~/.claude` copy is the real one,
so drop the conflicted copy and the stash:

```
git -C ~/.claudesync checkout HEAD -- <the held-back file>
git -C ~/.claudesync stash drop
```

If you abandon the resolution, undo it instead of finishing it:

```
git -C ~/.claudesync rebase --abort   # an abandoned rebase
git -C ~/.claudesync merge --abort    # an abandoned merge
```

Both put the branch and the work-tree back where they were; your local commits
are kept, and the conflict is back at the next cycle. Then start the daemon
again (`claude-memsync start`). The next cycle pushes the
result and pulls everything that was waiting. You do not need to edit anything
in `~/.claude`: before a cycle does anything else, the daemon compares the sync
repo's HEAD with the commit it last finished a cycle at
(`~/.claudesync/.state/synced-head`) and copies the files that changed in
between into `~/.claude`, so your resolution and the peer's other edits arrive
there instead of the older `~/.claude` copies being pushed over them. This
catch-up leaves alone a file that is modified in `~/.claudesync` (held back)
and a file whose `~/.claude` copy has changed since that commit: your version
stands and the incoming one is kept beside it in `~/.claudesync` as
`<name>.from-remote-<id>` (written once the copy into `~/.claude` and the
record have succeeded, and not again if the same bytes are already kept there).
A file that is modified in `~/.claudesync` is skipped **without** a backup:
autostash (or the park) already merged the incoming change into that file, so
there is nothing to keep beside it. A `~/.claude` copy that already equals the
incoming version (the daemon died after its own commit, before the record was
written) is not a change of yours and is handled as usual. If the history was rewritten (`git rebase`, `git
reset` by hand) the same applies to the difference between the two commits,
and the log says so. With no record yet (the first start after an upgrade) the
daemon only records the current HEAD.

If the catch-up keeps failing (the log says `catch up with the sync repo` every
cycle, and nothing else syncs), fix what it names first: a file where a
directory should be, a permission, a full disk. If it cannot be fixed, you can
make the daemon forget the record:

```
rm ~/.claudesync/.state/synced-head
```

The next cycle then takes the current HEAD as already reflected in `~/.claude`
and records it, **without copying anything**. Every commit between the old
record and HEAD that was never propagated stays out of `~/.claude` (copy those
files from `~/.claudesync/projects/<hash>/memory/` yourself), and until you have
the Claude copies you want, Reconcile will treat the `~/.claude` copies as yours
and set the other versions aside as `.from-remote-*`, rather than adopt them.

### Built-in rules

| Rule name | Matches |
|---|---|
| `private-key` | a `-----BEGIN ... PRIVATE KEY-----` header |
| `github-token` | `ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_` tokens and `github_pat_` tokens, also directly after `_` (for example `MY_TOKEN_ghp_...`) |
| `anthropic-api-key` | `sk-ant-...`, also directly after `_` |
| `openai-api-key` | `sk-...`, `sk-proj-...`, `sk-svcacct-...` and `sk-admin-...`, also directly after `_` |
| `stripe-key` | `sk_live_...` and `rk_live_...` (live keys only; `sk_test_` is not flagged) |
| `google-api-key` | `AIza` followed by 35 characters |
| `gitlab-token` | `glpat-...` personal access tokens |
| `slack-token` | `xox?-...` tokens, also directly after `_` |
| `aws-access-key` | `AKIA...` / `ASIA...` access key IDs, also directly after `_` |
| `aws-secret-key` | `aws_secret_access_key` set to a 40-character value (a value starting with `<` or `$` is treated as a placeholder) |
| `jwt` | three dot-separated base64url segments starting `eyJ`, also directly after `_` |
| `url-credentials` | `scheme://user:password@host`, or `scheme://:password@host` with no username (`redis://:pw@host`). `$`, `*` and `'` are allowed in the password. A password that is wholly a placeholder is skipped: `${...}`, `<...>`, `{{...}}`, a run of `*`, or a dummy word such as `password`, `pass` or `changeme` |
| `credential-assignment` | `password`, `passwd`, `token`, `secret` or `api_key` (case-insensitive), optionally followed by only `_key`, `_access_key`, `_value`, `_string`, `_str` or `_data`, assigned a quoted value of 16 or more characters without whitespace, with `=` or `:`. A value starting with `<`, `$` or `{` is a placeholder and is skipped. So `client_secret: "..."` and `"api_key": "..."` match, but `tokenizer`, `secretName` and `token_type` do not |

Not covered: unquoted assignments, other vendors' keys, and anything without a
recognizable prefix. These rules are a safety net for accidents, not a
guarantee: use the denylist for anything specific to you.

The rules are deliberately conservative about prose, but they will sometimes
flag a harmless note. A false positive costs you one file that doesn't sync
until you reword it.

The daemon logs the **file name and rule name only**, never the matched text:

```
scrub: holding back projects/-home-<user>-src-<app>/memory/notes.md (rule: github-token)
```

Deletions are not scanned. If a file you edited was already committed in a
clean version and the new version is held back, the repo keeps the old
version.

### Denylist

For things no pattern list can know about (client names, internal hostnames),
create `~/.claudesync/denylist.txt` next to `config.json`: one regular
expression per line, matched case-insensitively against each file's contents
**and its path** (for example `projects/<hash>/memory/notes.md`). Blank lines
and lines starting with `#` are ignored. Syntax is Go's
[RE2](https://github.com/google/re2/wiki/Syntax).

```
# anything mentioning these is not for the shared repo
client-name\.internal
\bproject codename\b
```

You can install one when setting up a PC: `claude-memsync init --remote <url>
--denylist <file>` validates the file and copies it into place before the
first commit, so the initial push is already scrubbed.

A content hit is logged as `denylist:<line number>`. The pattern text is never
logged. A file whose **name** matches is held back even if its contents are
clean, and because the name may itself be what you wanted kept private, it is
not logged either; you will see a count instead:

```
scrub: <held back: path matches denylist> (2)
```

The same rule covers error messages: an error from copying a file, from the
park, or from git (a failed pull or checkout names files in its output) that
mentions a path matching the denylist is logged with `<path redacted>` in place
of just that part. The rest of the message stays, including the exit status and
git's reason. Each pattern is run once over the line as it is and once over a
normalised copy of it (git's `:(literal)` prefixes and the quotes cut out,
backslashes turned into slashes, and the absolute sync and Claude directories
rewritten as the repo-relative path your patterns are written against; those
directories are found whatever the slashes were, and on Windows and macOS
without regard to letter case, so `C:\Users\you\.claudesync\projects\p1\memory\x.md`
is `projects/p1/memory/x.md`), each word of the copy is tried on its own after
splitting on whitespace, quotes and colons, and a match is widened to the whole
word, so a name of any word count, with spaces, is caught. In this search `^`
and `$` also stand for the boundary of a name (whitespace, a quote, a colon, a
comma or a bracket; `$` also a period or a slash, which end a sentence or a
directory name), so `^projects/[^/]+/memory/night owl` finds that path in
the middle of a line. The work is linear in the length of the line. A line over
64 KiB is replaced whole by `<path redacted>`, since it cannot be checked
within a sane amount of work. A word in your denylist that
also appears in the remote URL redacts the URL in the message, not the error.
If the denylist exists but cannot be parsed, no path or file name is logged at
all.

The file is per-PC, never committed (the daemon refuses to stage it, in any
letter case, even if your `.gitignore` predates it) and read on every cycle, so
edits take effect without a restart.

**Warning: a pattern that matches the empty string holds back every file.**
That includes a line that is only `.*`, `a*`, `(x|)` or `^`; a stray
`|` at the end of an alternation does it too, and so does a path match, since
every path contains the empty string. Nothing will commit until the line is
fixed. If nothing is syncing, check the denylist first.

The file must be UTF-8. A leading byte-order mark is ignored. A UTF-16 file
(Notepad's "Unicode" option, PowerShell's `>` redirect on older versions)
cannot be read as patterns, so the daemon commits nothing and logs that the
file must be saved as UTF-8.

If the denylist has an invalid regular expression the daemon commits nothing
until it is fixed, and logs the line number.

### Configuration

Both options go in `~/.claudesync/config.json`:

| Key | Default | Meaning |
|---|---|---|
| `scrub` | `true` | Scan files before committing. `false` restores the old behavior of committing everything. |
| `archiveDeletes` | `false` | Archive instead of delete; see [Archive, never delete](#archive-never-delete). |
| `requireDenylist` | `false` | When `true` and `denylist.txt` does not exist, commit nothing and log why. For setups where "no denylist" means "not configured yet". Ignored when `scrub` is `false`. |

Existing `config.json` files without these keys get the defaults, so scrub is
on after an upgrade, `requireDenylist` is off, and no denylist is needed.

## Telling a running session what changed

Claude Code reads a project's memory when a session starts. If another PC
pushes a note while a session is open for hours, that session never sees it.
`claude-memsync changes` lets a hook say so:

```sh
claude-memsync changes (--since <rev> | --marker <file>) [--project <dir>] [--json] [--limit N] [--update-marker] [--synced]
claude-memsync changes --hook            # for UserPromptSubmit / SessionStart hooks
```

It compares the sync repo's current `HEAD` with an earlier point and lists the
memory files that were added, changed or deleted in between. It only reads the
repo; it never fetches (the daemon does that), so it reflects what this PC has
already pulled and copied into `~/.claude`, or committed itself.

| Flag | Meaning |
|---|---|
| `--since` | A git revision (full or abbreviated SHA, branch, `HEAD~3`). A revision that doesn't resolve is an error (exit 1). Exactly one of `--since` and `--marker` is required. |
| `--marker` | The path of a **marker file** whose first line is the base commit. |
| `--update-marker` | Only valid with `--marker`. Rewrites the marker to the current `HEAD` afterwards (atomically, mode `0600`). A marker that doesn't exist yet is created and nothing is reported: that call just sets the baseline. An empty marker (say, one made with `touch`), or one whose commit is gone (garbage-collected after a rebase, or the repo was re-cloned), is reset the same way: nothing is reported, `"reset": true` is set in the JSON, one warning line goes to stderr, and the exit code is 0. Without `--update-marker` an unresolvable marker is an error. A file whose first line is not a commit id (7 to 64 hex digits) is not a marker: it is never overwritten, and the command fails with `marker <path> does not contain a commit id` (exit 1). If the commit being compared up to (`HEAD`, or the synced one) is an ancestor of the commit the marker names (the repo was reset or force-pushed back), nothing is reported and the marker is moved back to it, so the same report is not repeated on every run. |
| `--project` | Only this project: a project directory (`/home/alice/src/app`) or Claude's folder name for it (`-home-alice-src-app`). |
| `--synced` | Compare with the commit last copied into `~/.claude` (`.state/synced-head`) instead of `HEAD`, when that file exists and names a commit. Without it the CLI uses `HEAD`. Hook mode always does this and rejects the flag. |
| `--json` | Print JSON instead of text. |
| `--limit` | List at most N changes (default 20), then `...N more`. `0` lifts the cap. |
| `--hook` | Hook mode, see [Hook mode](#hook-mode). Takes only `--config`, `--routes`, `--project`, `--limit` and `--deadline`. |
| `--deadline` | With `--hook`: give up after this long (default `1.5s`, or `$CLAUDE_MEMSYNC_HOOK_DEADLINE_MS` in milliseconds); `0` means no deadline. See [Hook mode](#hook-mode). |
| `--config` | Path to `config.json` (default `~/.claudesync/config.json`). |

Text output:

```
2 memory files changed since 91be0aa (head 3f2a9c1)
  changed  /home/alice/.claude/projects/-home-alice-src-app/memory/MEMORY.md
  added    /home/alice/.claude/projects/-home-alice-src-app/memory/feedback_tests.md
```

With `--json`:

```json
{
  "head": "3f2a9c1e...",
  "since": "91be0aa4...",
  "total": 2,
  "hidden": 0,
  "more": 0,
  "busy": false,
  "reset": false,
  "changes": [
    {
      "status": "changed",
      "project": "-home-alice-src-app",
      "file": "MEMORY.md",
      "path": "projects/-home-alice-src-app/memory/MEMORY.md",
      "local": "/home/alice/.claude/projects/-home-alice-src-app/memory/MEMORY.md"
    }
  ]
}
```

`status` is `added`, `changed` or `deleted`; `local` is where Claude Code reads
the file on this PC. `"localKept": true` (and, in text, the note `(the copy on
this PC differs from the synced version: a local edit not yet synced, or an
incoming change that was held back)`) marks a non-deleted change whose local
file does not hold the committed bytes. Either it was edited here since the last
sync, or the daemon did not copy the incoming version (held back by the scrub;
where it kept that version, it sits beside the mirror copy as
`.from-remote-*`). It costs one `git cat-file --batch` for all listed changes.

**Denylist.** A change is left out of the list when its repo path (the path in
the sync repo, as the daemon's commit-time check sees it) matches a pattern in your [denylist](#denylist) (the file the daemon
reads), and the text output says `N changed file(s) whose name matches your
denylist are not listed`; in JSON the entries are omitted and `"hidden": N`
counts them (`total` counts only the listed ones). A peer, which has its own
denylist, can push a file whose name yours would hold back, and the name must
not reach a prompt. If the denylist exists but cannot be read or parsed, no
names are listed at all, only the count (`N changed file(s) are not listed (the
denylist could not be read)`), and one line goes to stderr. With no denylist
file everything is listed. Only files directly under a project's `memory/` folder are
reported, and a rename shows up as a delete plus an add. File and project names
with unprintable characters (a newline, say) are shown quoted in the text
output. Only names and paths are ever printed, never note contents.

The command exits non-zero (with a message on stderr, never the marker file's content) when `--since` or a
marker doesn't resolve (unless it is reset, see above) or the repo has no
commits. While the sync repo is in the middle of a rebase, merge or
cherry-pick, the report is empty with `"busy": true`, the marker is not moved,
and the exit code is 0.

### Hook mode

`--hook` is built for Claude Code hooks, so no `jq` or shell script is needed.
It reads the hook's JSON from stdin (`session_id`, `cwd`, `hook_event_name`)
and behaves like `--marker --update-marker` with a per-session marker at
`<sync dir>/.state/hook-markers/<session_id>`; session ids outside
`[A-Za-z0-9_-]+` are ignored silently. `--project` defaults to
`$CLAUDE_PROJECT_DIR` (the directory Claude Code was started in), then to the
hook's `cwd`, unless you pass it; the `cwd` follows Claude's `cd`, so it is
only the fallback. When something changed it prints

```json
{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"Memory notes changed in the sync repo (this or another machine) since you last looked (read a file before relying on it):\nadded /home/alice/.claude/projects/-home-alice-src-app/memory/feedback_tests.md\nchanged /home/alice/.claude/projects/-home-alice-src-app/memory/MEMORY.md"}}
```

with that framing line, then one `status path` line per change (and `… and N more` past `--limit`,
and the denylist count line),
and nothing at all when nothing changed. The hook reports up to the commit
recorded in `.state/synced-head` (what the daemon has copied into `~/.claude`),
or `HEAD` when there is no such record, and advances its marker to that same
commit; a commit that was pulled but not yet copied is reported by the first
prompt after the copy, never before it. The marker only moves after that
output was written. It never moves backwards, except when history itself
moved back (a reset, a force-push): then the difference is reported once and
the marker follows. The first call in a session just
records the baseline. Any error exits 0 and writes one line to stderr, and so
does a panic, so the hook can never block a prompt. On an error before the
report is printed, nothing goes to stdout; if the marker write fails after the
report was printed, the next prompt repeats the report.

The hook gives itself a deadline of 1500 ms, well inside Claude Code's 10 s
hook timeout (which discards the output of a hook that overruns, after which
the markers would have moved anyway). Change it with `--deadline <duration>`
(for example `--deadline 3s`) or with `CLAUDE_MEMSYNC_HOOK_DEADLINE_MS`
(milliseconds; the flag wins); `0` turns the deadline off. If the reports for
all repos are not computed in time, for instance because the host's disk is
saturated, the hook prints no change notices (only the held-repo alerts below,
if any), moves no change marker, writes one line to stderr
(`changes: hook deadline exceeded; notices deferred to the next prompt`) and
exits 0 at once. It is all or nothing: a repo that finished while another did
not is not reported either, so the next prompt reports every change.

**Held repos.** When the daemon holds a repo because its private-only check
failed (see the table of checks), the hook leads its output with one line per
held repo: ``memsync: repo <name> is HELD by the private-only check since <time>:
<reason>. Nothing syncs for it (no pull, commit or push) until the check passes;
see `claude-memsync status`.`` It reads only the repo's local `.state` files (no
lock, git or network), so the alert goes out even when the deadline cuts the
change report short. Each session is told once per hold: the session's `.held`
marker under `.state/hook-markers/` records which hold it has seen, and it is
written only after the output. Text taken from state files is cut to one line,
with control and invisible format characters removed.

Markers are kept, not deleted when a session ends: a session resumed with the
same id needs its marker to report what changed while it was closed. Instead,
each `--hook` run deletes the markers under `.state/hook-markers/` that were
last written more than 30 days ago (never the current session's).

Register it in `~/.claude/settings.json` (or a plugin's `hooks/hooks.json`,
which has the same shape):

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "claude-memsync changes --hook", "timeout": 10 } ] }
    ],
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "claude-memsync changes --hook", "timeout": 10 } ] }
    ]
  }
}
```

`UserPromptSubmit` alone is enough for the "long-running session" case.
Adding `SessionStart` sets the baseline as soon as the session opens, so the
first prompt reports anything that arrived in between. The `timeout` (seconds)
keeps a stuck `git` from holding up the prompt.

### Limits

- This PC's own edits are reported too: they are commits in the same history.
- `--project` matches path-keyed folder names only. A directory is turned into
  Claude's folder name by the rule as currently understood (every
  non-alphanumeric character becomes `-`), so very long or non-ASCII paths may
  not match. In hook mode the project comes from `$CLAUDE_PROJECT_DIR`, which
  stays at the directory Claude was started in; if that is not set the hook's
  `cwd` is used, and `cwd` follows Claude's `cd`, so after a `cd` into a
  subdirectory it won't match the project's folder name. Pass `--project`
  explicitly if that bites.
- The hook waits for the daemon's copy: it reports up to `.state/synced-head`,
  so the file at `local` has been updated when it is named. The CLI compares up
  to `HEAD` unless you pass `--synced`, so it can name a file the daemon has
  not copied yet.
- With no `synced-head` record (a PC that has not run a daemon cycle since the
  upgrade) the hook falls back to `HEAD`.

## Deletes

Deletes propagate across PCs. The daemon keeps a per-PC manifest
(`~/.claudesync/.state/manifest.json`) listing which Claude-side memory files
were present at the last successful sync. On each reconcile pass, a file that's
in the mirror, missing from Claude, **and** was in the manifest is treated as a
user delete and removed from the mirror; the next push propagates it. A file
that's in the mirror, missing from Claude, but **not** in the manifest is treated
as an inbound new file from another PC and copied into Claude.

This means:

- Delete a memory while the daemon is running → propagates immediately (watcher
  catches it).
- Delete a memory while the daemon is stopped → propagates on next startup
  (manifest-driven reconcile).
- First-ever run on a PC has no manifest, so deletes can't be inferred from prior
  state. The daemon takes the safe path: never infer deletes, bring everything
  together. Subsequent runs work normally.

## Archive, never delete

Off by default. With `archiveDeletes` on, the tool never removes a note from a
memory repo: it moves it. This gives a durable history for people who want one;
upstream behavior (a delete is a delete) is unchanged when the key is off.

Turn it on with `claude-memsync init --archive-deletes`, or set
`"archiveDeletes": true` in `config.json` (a bootstrap for a fleet of hosts
should set it on every host). It is per repo: with [several repos](#several-repos-per-host)
each repo's own `config.json` decides. A repo's loop only touches the folders it
owns, so a note deleted from a folder routed to another repo is never seen, and
never archived, by this one.

What happens:

- **A note deleted from a Claude memory folder** is moved in the repo to
  `archive/<YYYY-MM-DD>/<folder>/<file>` in the same commit that records the
  deletion (git shows a move), and one line is appended to `archive/INDEX.md`:
  date, folder, file, reason (`deleted on a host`) and the archive path. The file name
  carries the first eight hex digits of the content's SHA-256 before the
  extension (`note.3f2a9c1b.md`), so identical copies share one path (two hosts
  archiving the same note make one identical add) and different content never
  collides or overwrites. The deletion is detected at commit time: a pure
  rename (a delete plus a 100% identical add) stays a rename and is not
  archived, while a rename that also edited the note keeps the rename and
  archives the old content. Two hosts that archive the same note
  on the same day leave git a conflict that needs no decision (each moved the
  note to its own hash-named path, which git reads as a rename on both sides,
  or a rename against a delete). When the daemon's own pull stops on conflicts
  and **every** conflicted path is of exactly that shape (an `archive/` copy
  added by either side, or a `projects/<folder>/memory/` note deleted on both),
  it keeps both copies, drops the note and continues the rebase, logging only a
  count. Any other conflict, including one extra path, aborts as described
  under Rebase conflicts. This does not depend on the merge strategy, and
  ordinary renames and edits merge as they always did.
- **Other hosts** pulling that commit remove the note from their `~/.claude`
  copy exactly as for any delete, and never touch `archive/`. The directory is
  never mirrored into `~/.claude`, and routing, Reconcile, the manifest,
  `changes` and the distill worklist only read `projects/<folder>/memory/`, so
  `archive/` is never a Claude folder. The index is merged as a union, so two
  hosts that archive in the same cycle do not conflict.
- **An edit on one host against an archive on another** merges without a
  conflict (git reads the archive as a rename and carries the edit into the
  copy). After every pull the daemon checks that each archive copy that arrived
  holds the bytes its hash name promises. If not, it puts the edit back at the
  note's live path (as `<name>.conflict-<sha8>.<ext>` beside it when that path
  now holds other text), restores the copy, adds an index line ("edited after
  archive, restored"), and commits all together, so the note returns to
  `~/.claude` on every host. If the original text cannot be found, the pull is
  undone and nothing is pushed.
- **`distill --prune`** archives a stale catalog entry to
  `archive/<date>/distilled/` instead of removing it.
- **MEMORY.md** is an index, not a note: a line deleted from it is index content
  and is not archived as a file. (Deleting the whole file is archived like any
  other note.)
- **A file the scrub holds back** is not archived, and its deletion waits with
  it (on every cycle, with its index line and union rule), so a note never
  leaves the tree without its copy.
- A note created and deleted between two commits was never committed; there is
  nothing in the repo to keep.

Reading the archive (the tool only reads it):

```
claude-memsync archive list [--since 2026-10-01] [--repo <name> | --config <path>]
claude-memsync archive search <term> [--show] [--repo <name> | --config <path>]
```

Symlinks in the archive are never followed, and paths matching the denylist
are not printed (they are counted instead). `search` matches file names and contents, case-insensitively, and prints the
matching paths with a count of matching lines. `--show` also prints the lines.
Archived notes are not injected into sessions; search them on demand. With
several repos, each repo's archive is listed or searched in turn and every line is labelled `[repo]`; `--repo <name>` (a key of `routes.json` `repos`) or `--config` reads just one.

**Purging is not a tool command.** The one exception to "never delete" is a
deliberate purge, for example a leaked secret in an archived note. That is a git
history rewrite done by the repo owner by hand (rewrite the history of every
clone, force-push, then have every host re-clone), and the tool has no command,
flag or setting that does it.

## On-disk layout

```
~/.claude/projects/<hash>/memory/         # what Claude reads + writes
    ├── MEMORY.md
    └── <topic>.md ...

~/.claudesync/                            # owned by the daemon
    ├── config.json                       # per-PC (gitignored)
    ├── denylist.txt                      # per-PC, optional (gitignored)
    ├── daemon.pid                        # per-PC (gitignored)
    ├── .git/                             # the sync repo
    ├── .gitattributes                    # MEMORY.md merge=claude-memory-index
    ├── .gitignore                        # excludes config.json, .state/, etc.
    ├── .state/manifest.json              # per-PC delete-detection state
    ├── .state/synced-head                # per-PC, last commit copied into ~/.claude
    ├── .state/paused                     # per-PC, present while syncing waits for a hand-resolved rebase/merge
    ├── .state/pull-running               # per-PC, nonce around the daemon's own pull
    ├── .state/hook-markers/<session>     # per-PC, `changes --hook` baselines
    ├── .state/parked.json, parked/       # per-PC, only during an unfinished park
    ├── .state/parked.broken-<ts>/        # per-PC, a park that was set aside
    ├── distilled/                        # shared distilled-memory catalog
    │   ├── DISTILLED.md                  # derived index (gitignored)
    │   └── <slug>.md ...                 # distilled entries (synced)
    └── projects/<hash>/memory/...        # git work-tree mirror
```

## Auth notes

The daemon shells out to the system `git` binary, so it uses whatever auth is
configured in your environment:

- **HTTPS with Git Credential Manager** (`gh auth login` on Windows populates
  this): zero extra setup.
- **SSH**: ensure `ssh-agent` is running for your user session and your key is
  loaded. On Linux, `systemctl --enable-linger <user>` keeps the user instance
  running across logoffs if you want sync activity while not logged in.
- **PAT in URL** (`https://<token>@github.com/...`): works but the token ends up
  in `~/.claudesync/.git/config`. Not recommended.

Test before installing the daemon:

```sh
git -C ~/.claudesync push
```

If that works without prompting, the daemon will too.

## Private repos only

Nothing on GitHub stops an org owner from making a repo public, so the tool can
enforce privacy itself. This is **opt-in**: set `"privateOnly": true` in
`config.json` or run `init --private-only`. With it off, `init` and the daemon
behave as they always did (any remote name, no token, no checks). `doctor` and
`repo create` always apply the `mem-*` rules.

- **Names.** The tool only creates, joins or checks repos named `mem-registry`,
  `mem-personal`, `mem-project-<x>`, `mem-infra-<x>`, `mem-client-<x>`,
  `mem-orch-<x>` or `mem-orch-all`. Anything else is refused, so it can never
  touch a public project repo.
- **Create.** `claude-memsync repo create [--org <org>] <name>` is the only
  create path. It sends `"private": true` and `"visibility": "private"`
  explicitly, then reads the repo back and reports success only if it is
  private.
- **Verify.** A repo must report `private: true` and `visibility: "private"`.
  Public, `internal`, a missing field or any API error is a refusal (fail
  closed).
- **Remote URLs.** ssh (`git@host:Owner/repo.git`), `ssh://` and https URLs are
  parsed to owner/repo, including per-repo SSH host aliases such as
  `git@gh-mem-personal:Org/mem-personal.git`.

### Where the check runs

A deploy key cannot call the GitHub API, so the check needs a token (a
read-only fine-grained token with Metadata read is enough for checks; creating
a repo needs more). Configure it with the `MEMSYNC_GITHUB_TOKEN` env var, a
different var via `githubTokenEnv`, or a file via `githubTokenFile` (or
`init --token-file`), or a command that prints the token on stdout, such as
your password manager's CLI. The command is `githubTokenCmd`, a JSON array so
that spaces survive (`["<cli>", "read", "<reference with spaces>"]`), or
`--token-cmd "<cli> read '<reference with spaces>'"` on the command line, where
"double" and 'single' quotes group words. It is run directly with no shell (no
expansion of `$VAR`, backticks, globs or `;`), and its stderr is discarded. Run from
a terminal it may take 120 seconds (a password manager can wait for you to approve a
prompt; it prints `waiting for your password manager (approve the prompt if one appears)…`
on stderr first); `CLAUDE_MEMSYNC_SECRET_TIMEOUT` (a Go duration such as `45s` or
`3m`) changes that. The daemon, which nobody can ask, keeps 10 seconds and prints nothing; so do hooks (`changes --hook`, `related context`) and the doctor timer (`doctor --unattended`): 120 s is for a terminal only, and hooks and timers never wait on a password manager. A hook never runs a token or key command and never uses the network (it may still run local `ssh -G`/`git config` lookups to resolve a host alias): it uses only cached related-repo visibility verdicts, and a stale or missing one hides that related repo for the run (one stderr line says so) until a command run by hand refreshes it. When several sources are set the first configured one is
used, in the order file, command, env var; a configured source that fails is an
error, never a silent fall-through to the next. The token is never written to
`config.json`, logs or error messages.

| When | Token required | Behaviour |
|---|---|---|
| `init` | yes | Refuses before cloning or pushing if the remote is not a private `mem-*` repo, or if it cannot check. |
| `doctor` | yes | Non-zero exit if it cannot check. |
| daemon start | no | With a token: a repo that fails the check (not private, or the check itself fails, such as a GitHub App lookup error) is held: its loop does not start, `status` shows `waiting for the private-only check, re-checking with backoff: …`, and the check is retried from 10 s, doubling to every 30 min, until it passes; other repos sync meanwhile and the daemon stays up. A configured token source that gives no token (a failing `githubTokenCmd`, an empty `githubTokenFile`, an unset `githubTokenEnv`, an App that mints nothing) counts as a failed check, not as "no token": the repo is held, and its push guard blocks pushes until the source works again and the API says private. A held repo is released only when the API says private, never by a check skipped for want of a token (for example after `config.json` stops being trusted and its token sources are ignored). With no token source configured at all (or `recheckTokenCmd: false` with only a command): logs once that visibility was verified only at init and by `doctor`, and starts. |

With `privateOnly` on and a token configured, the daemon also re-checks at most
once an hour (conditional request with the ETag; 304 means unchanged). If the
repo is no longer private it stops pushing but keeps committing locally, logs one
line per state change, and `claude-memsync status` says so. A failed API call
keeps the last known state, is logged once, and does not block. Without a token
there is no re-check; schedule `doctor`.

### Token from a command

Instead of a token file or env var, `githubTokenCmd` in `config.json` is an
argv array (no shell), for example `["gh", "auth", "token"]`; its stdout is the
token. It is used when `githubTokenFile` is not set (the file wins; the env var
is the fallback). One-run flags: `--token-cmd "<command>"` on
`init`, `doctor` and `repo create` (shell-style quotes group words; no shell
is run). `doctor` and `repo create` do not save it; `init` saves the command
(`githubTokenCmd`), never the token.

- The command runs when a token is needed, with the timeout described above (120 s
  at a terminal, 10 s in the daemon, `CLAUDE_MEMSYNC_SECRET_TIMEOUT` to change it). Its
  output is held in memory for that request only: never written to disk,
  `config.json`, logs or error text (the command's stdout and stderr are not
  put in errors either).
- The command is honoured only from a trusted `config.json` (see "Which
  config.json keys are trusted" below); a command typed as `--token-cmd` is
  always honoured.
- A command that fails, times out or prints nothing means *no token*: the
  existing no-token behaviour applies (init and doctor refuse; the daemon
  skips the API check).
- The daemon's hourly visibility re-check runs the command too. Set
  `"recheckTokenCmd": false` to keep the daemon from running it (then the
  re-check needs a file or env token, or does not run).

### Which config.json keys are trusted

`config.json` lives in the sync work-tree, so a remote could try to plant one.
The keys that run a command, name a credential or pick an endpoint
(`githubTokenCmd`, `githubTokenFile`, `githubApp`, `githubApiUrl`,
`mergeDriverPath`) are therefore honoured only when `config.json` is owned by
you, has no group or other permission bits (mode 0600), has a single hard link
and is not tracked by git in whatever repo it really lies in (symlinks are
resolved first). Otherwise they are ignored, with one warning in the log that
names the keys, and the tool behaves as if they were unset. Values you type on
the command line (`--token-cmd`, `--token-file`, `--github-app`) are always
honoured. The daemon also refuses to pull a remote tree that tracks per-host
files such as `config.json`.

### doctor

```sh
claude-memsync doctor --org <org> [--token-file <path>] [--config <path>] [--routes <path>]
```

Lists the org's repos and exits non-zero, naming each one, if any `mem-*` repo
is public or internal, or if the repo this host joined is not private or lacks
the `mem-` prefix. Public repos without the prefix are ignored. Output is repo
names only. `--org` defaults to `githubOrg` in `config.json`, else the owner of
the remote. `--token-file` names a file holding the GitHub token for this run, as `init --token-file` does (it is not written to `config.json`; `MEMSYNC_GITHUB_TOKEN` also works); with a routes file it applies to every repo. `--config` checks the one repo that config names. `repo create` resolves the org the same way and refuses if none is set. For every command (`doctor`, `host list|retire|check-key`, `repo create`), a token flag typed on the command line (`--token-file`, `--token-cmd`, `--github-app`) wins over the token sources in `config.json`: a host whose config names the Metadata-only check app can still retire deploy keys with the admin token passed as a flag.

`claude-memsync doctor --install-timer` writes a daily systemd user timer
(Linux) or launchd agent (macOS) into `~/.config/systemd/user` or
`~/Library/LaunchAgents` (override with `--unit-dir`). It does not enable it;
it prints the command. The timer needs `githubTokenFile`, and runs `doctor --unattended`, so it never waits for a password manager (10 s limit, no prompt text).

Remotes must also be on the allowed git host: `github.com`, or `githubHost`
(or the host of `githubApiUrl`) for GitHub Enterprise. https must name it
directly; ssh forms, including aliases like `gh-mem-personal`, are resolved with
`ssh -G` and must land on it. The push URL git actually uses
(`git remote get-url --push origin`, after `insteadOf` rewrites) must equal the
configured remote, checked at `init` and daemon start. Without a token the
daemon still runs the name, host and push-URL checks and skips only the API
check. The hourly re-check also stops pushing after `recheckMaxFailures`
consecutive failures or `recheckMaxAgeHours` without a success (defaults 24 and
24). The timer needs `githubTokenFile` set; install refuses otherwise. `githubHost`
must pair with the API: github.com with api.github.com, or another host with
`githubApiUrl` set to `https://<host>/api/v3` or `https://api.<host>`. The mode
refuses ssh remotes while `core.sshCommand`, `GIT_SSH_COMMAND` or `GIT_SSH` is
set, and the daemon re-checks the push URL and these overrides before every push.

Config keys: `githubApp`, `githubHost`, `recheckMaxFailures`, `recheckMaxAgeHours`, `privateOnly`, `githubTokenEnv`, `githubTokenFile`, `githubTokenCmd`, `recheckTokenCmd`, `githubApiUrl`, `githubOrg`.

### Per-PC files never arrive from the remote

`config.json`, `denylist.txt`, `daemon.pid`, everything under `.state/`,
`distilled/DISTILLED.md` and the temporary names `*.tmp`, `*.tmp.*` and
`*.from-remote-*` are local to a PC (the generated `.gitignore` keeps them out).
If a fetched tree tracks any of them, the pull is refused before anything is
merged: the daemon logs it once, commits nothing from the remote, pushes
nothing, and `claude-memsync status` says `syncing paused, pull refused`. It
resumes by itself once the remote tree no longer tracks them.

## GitHub App authentication

Instead of a long-lived personal access token, the checks (`init`, `doctor`,
`repo create`, the daemon's re-check) can use a GitHub App. The tool signs a
short-lived JWT with the app's private key and exchanges it for an installation
token that expires in an hour. Nothing else changes: the token is only used for
the same API calls as before, and App auth is optional. Without it, the
`MEMSYNC_GITHUB_TOKEN`, `githubTokenFile` and `githubTokenCmd` sources behave
exactly as described under "Where the check runs".

### The config directory

Everything per host and never synced (`routes.json`, `apps.json`, the age
identity, key files) lives in one directory on every OS:
`$XDG_CONFIG_HOME/claude-memsync`, else `~/.config/claude-memsync` (on Windows
too: `%USERPROFILE%\.config\claude-memsync`). Earlier builds kept the age
identity under the OS's own config dir: `~/Library/Application Support/claude-memsync`
on macOS, `%AppData%\claude-memsync` on Windows. An identity still there keeps
being used (a second one is never created) and `status` prints a one-time hint
saying so. The identity stays where it is and keeps working: each clone's git
config holds the identity's absolute path (`memage.identity`), so moving the
file would leave every repo keyless. Do not move or copy it by hand; a safe
`identity move` that rewrites every clone is planned.

### identity backup

```sh
claude-memsync identity backup --store-cmd "<argv>" [--verify-cmd "<argv>"] \
    [--identity <path> | --all | --repo <name> | --config <path>] [--routes <path>] [--dry-run]
```

Stores this host's age identity file or files in your password manager and,
with `--verify-cmd`, proves the stored copy is the same bytes. See
[setup.md](setup.md#back-up-the-host-identity) for the usage and examples.

What it does, in order:

1. Hardens the process once the arguments are valid: core dumps off
   (`RLIMIT_CORE` soft limit 0 on Linux and macOS) and, on Linux,
   `PR_SET_DUMPABLE` 0. Best effort; Windows has no equivalent.
2. Resolves the files: `--identity`, else the distinct `memage.identity` of
   `--repo`'s or `--config`'s clone, else of every clone this host routes.
   For the whole host it adds the default identities related read clones use,
   and any classic identity or `*.classic-retired` that `rekey-pq` keeps beside
   a listed one (exactly `identity-pq.<ext>` to `identity.<ext>`, and
   `<name>.classic-retired`; no other neighbour is guessed). A default file
   that is not an age identity is refused (exit 1), not skipped. Each file is
   labelled (`used by: <repos>`, `host default`, which reads `host default
   (related clones)` when the routes file has a registry, and
   `classic history and rollback`); clones that give nothing print
   `skipped: <dir> (<reason>)`. The configured path is not symlink-resolved.
3. With more than one distinct key (the same bytes at two paths count once and
   are stored once), `--store-cmd` must contain `{fingerprint}` (the grouped
   fingerprint with dashes) or `{name}` (the file's base name), else it exits 2
   before running anything: one fixed item name would be overwritten by each
   file in turn while every one still verified. Both placeholders are replaced
   in every argument of `--store-cmd` and `--verify-cmd` for each file; they
   never contain key material. Two different keys that would expand to the
   same store arguments are refused, and so is a `--verify-cmd` that expands
   the same for any two different keys (each verify would read the same item
   back). Files whose parsed keys are equal are one item even if their comment
   lines differ.
4. Checks every file: a regular file, not a symlink (also checked on the opened
   file), no permission bits beyond 0600, owned by the current user, one hard
   link, and an age classic (`AGE-SECRET-KEY-1…`) or hybrid
   (`AGE-SECRET-KEY-PQ-1…`) identity. The contents are never part of a message.
   **On Windows the mode, owner and hard-link checks are skipped** (Windows has
   no such bits; the file's ACL is not inspected), so only the regular-file,
   symlink and parse checks apply there.
5. Runs `--store-cmd` with the file's bytes on stdin (the argument line is
   split without a shell; interactive timeout 120 s, with the "waiting for your
   password manager" line). Its stdout and stderr are discarded, so to find out
   why a store or verify command fails, run it by hand; an error here names
   only the exit status or the timeout.
6. With `--verify-cmd`, runs it (output at most 64 KiB) and compares the
   SHA-256 of its stdout with the file's. Only the verdict is printed. The
   buffers holding the key are zeroed after use, best effort.
7. Prints `<path>`, the public-key fingerprint (the grouped form `join`
   shows), the result and the reason. After the last store, with a
   `--verify-cmd`, once more than one store was attempted, the final pass
   re-reads the items that verified (even if a later store or verify failed);
   one that no longer matches is reported `OVERWRITTEN by a later store:
   <path>` (on stderr; the result line says `OVERWRITTEN by a later store`). Then a summary:
   `N stored (M verified, K UNVERIFIED, V verify-failed), F store-failed`
   (`V`: a read-back error, a `MISMATCH` or an overwrite; `F`: a failed store
   command or a refused file).

Exit codes: 0 when every file was stored (and verified, when asked; a stored
but UNVERIFIED file still exits 0), 1 on any failure (an unsafe file, a failed
or timed-out command, a mismatch), 2 for a usage error. `--dry-run` runs no
command and prints no summary; it still checks the files.

### apps.json

`~/.config/claude-memsync/apps.json` (or `$XDG_CONFIG_HOME/claude-memsync/`) must
be mode 0600 because it can name a command that gets run. It is a JSON array,
one entry per app on this host:

```json
[
  {"name": "<app-name>", "appId": "<numeric app id>", "org": "<org>",
   "keyFile": "/path/to/<app-name>.pem", "keyFingerprint": "SHA256:<base64>"}
]
```

Give each entry exactly one of `keyFile` (a PEM file that must be mode 0600:
a looser file is refused unread) or `keyCmd` (a command that prints the PEM on
stdout). `keyCmd` is a JSON array, `["<secret-manager-cli>", "read", "<reference
with spaces>"]`, so spaces survive; a plain string is also accepted and split
with shell-style quotes. Either way it is run directly with no shell, its stderr
discarded, and the same timeout as a token command (120 seconds at a terminal so you
can approve your password manager's prompt, 10 in the daemon;
`CLAUDE_MEMSYNC_SECRET_TIMEOUT` overrides it). PKCS#1 and PKCS#8 RSA keys both work.
The key is never stored in `apps.json`.

`apiBase` (default `https://api.github.com`; an `https` URL, set it for GitHub
Enterprise) is the only place the app's JWT is ever sent. It lives here, not in
`config.json`, on purpose: a JWT proves control of the app, and `config.json`
sits in a synced work-tree. The `githubApiUrl` in `config.json` is never used
for minting, and if it names a different API than the app's `apiBase` the tool
refuses to use the app at all.

The tool overwrites the key buffers it controls with zeros once the key is
parsed, as a best effort. It cannot clear copies made by the Go runtime, the
parsed key, swap or the secret manager, so treat the host, not the process
memory, as the boundary.

### One key per host

Give each host its own private key for the app (generate it in the app's
settings page; GitHub cannot create keys through the API) so a lost host can be
cut off by deleting just its key. `keyFingerprint` is optional but recommended:
when set, a key that does not match is refused before any request is made, which
catches a wrong file or a rotated key. `app register` records it for you.

```sh
claude-memsync app key-fingerprint --key-file <pem>
```

prints `SHA256:<base64>`, the value GitHub shows next to the key in the app's
settings (SHA-256 of the DER-encoded public key, standard base64; the same as
GitHub's documented `openssl rsa -in <pem> -pubout -outform DER | openssl sha256
-binary | openssl base64`). Use it to confirm a downloaded key is the one you
meant, or to fill in `keyFingerprint` by hand. The command reads the key (it
need not be mode 0600, since nothing is sent anywhere) but prints only the
fingerprint.

### Using an app

```sh
claude-memsync app-token --app <app-name> [--org <org>] [--repo <name>]... [--perm metadata:read]...
```

prints one installation token on stdout, so another tool can use the command as
its own token command. `--org` overrides the entry's org, and `--repo` and
`--perm` narrow the token below what the installation grants. `--apps-file`
overrides the path.

To use an app for the private-only checks, set `"githubApp": "<app-name>"` in
`config.json` (or `init --github-app <app-name>`, `doctor --github-app
<app-name>`; `init` refuses `--github-app` together with `--token-cmd`, because the
command would override the app it is about to save, so one of the two would be
dropped silently). Precedence among token sources is `githubApp`, then
`githubTokenFile`, then `githubTokenCmd`, then the env var; the first one
configured is used, and if it fails the command fails rather than trying the
next. The daemon keeps one token in memory, never longer than an hour whatever
expiry the server reports, and mints a new one when fewer than five minutes
remain. Tokens, JWTs and keys never appear in logs or errors, and the daily
doctor timer accepts `githubApp` as its token source. Like the other
credential keys, `githubApp` is ignored when `config.json` is not trusted (see
"Which config.json keys are trusted").

### Registering an app

```sh
claude-memsync app register --name <app-name> --org <org> --url <homepage-url> \
  --perm administration:write --perm metadata:read \
  (--key-out <path> | --key-cmd-store "<store command>" --key-cmd "<read command>")
```

uses GitHub's manifest flow, so there is no form to fill in by hand. The command
writes a local HTML file (mode 0600) and prints its path and `file://` URL; open
it in a browser where you are signed in to GitHub as an owner of the org. The
page posts a private app manifest (no webhook, no event subscriptions, only the
`--perm` permissions) to GitHub, which asks you to confirm and then calls back
on `127.0.0.1:<random port>`. A callback whose random `state` is wrong is
refused and ignored (any local program can hit that port), and the command keeps
waiting for the real one; it gives up after 30 minutes. The one-time code is
exchanged unauthenticated, and the command prints the app id, slug and the
install URL (`https://github.com/apps/<slug>/installations/new`). It never opens
a browser and contacts GitHub only after a correct callback.

The private key exists only in that one response. It is written to `--key-out`
(mode 0600; an existing file is refused before anything is created) or piped to
the stdin of `--key-cmd-store`, whose matching read command you give as
`--key-cmd`. Both commands are run directly with no shell; shell-style quotes
group words, e.g. `--key-cmd "<secret-manager-cli> read '<reference with
spaces>'"`. The key is never printed. The apps.json entry (with `apiBase` set
from `--api-url`, if you gave one) is written last, only after the key is safely
stored. If storing fails, the app already exists on GitHub: delete it or add a
key to it in the app's settings. `--github-url` and `--api-url` target GitHub
Enterprise.

## Several repos per host

By default one host has one sync repo (`~/.claudesync`) and every Claude project folder goes into it. A host that works for more than one party can instead keep several notes repos, for example your own projects, one per client, and a personal catalog, and route each Claude project folder to exactly one of them. One daemon runs one sync loop per repo. A folder that no rule routes anywhere is not synced at all.

Without a `routes.json` nothing changes: the daemon behaves exactly as described in the rest of this document.

### Setup

Each repo is a normal sync dir created by `init`. Put them next to each other, never inside another sync dir (`~/.claudesync` is itself a work-tree, and `git add -A` would pick up a nested repo):

```
claude-memsync init --remote git@github.com:<you>/mem-personal.git --sync-dir ~/.claudesync.d/personal
claude-memsync init --remote git@github.com:<you>/mem-project-app.git --sync-dir ~/.claudesync.d/projects
claude-memsync init --remote git@github.com:<you>/mem-client-<name>.git --sync-dir ~/.claudesync.d/client-<name>
```

Then write `~/.config/claude-memsync/routes.json` (or `$XDG_CONFIG_HOME/claude-memsync/routes.json`). It is per host and is never synced. `run`, `status` and `changes --hook` also take `--routes <file>`, and `CLAUDE_MEMSYNC_ROUTES` overrides the location. A `--routes` or environment path that does not exist is an error, not a fallback to single-repo mode.

```json
{
  "repos": {
    "personal": { "config": "~/.claudesync.d/personal/config.json",        "kind": "personal" },
    "projects": { "config": "~/.claudesync.d/projects/config.json",        "kind": "project" },
    "client-a": { "config": "~/.claudesync.d/client-<name>/config.json",   "kind": "client" }
  },
  "personal": "personal",
  "routes": [
    { "folder": "-home-<you>-scratch",            "repo": "projects" },
    { "path":   "~/clients/<name>/**",             "repo": "client-a" },
    { "remote": "github.com/<client-org>/**",     "repo": "client-a" },
    { "remote": "github.com/<you>/*",             "repo": "projects" }
  ],
  "default": "deny"
}
```

- `repos` names the repos this host may have, by their `config.json`, and each says what it is with `kind`: `personal`, `project`, `infra`, `client` or `orch`. A bare path string is refused, and so is a missing or unknown kind. The kind is checked against the repo's remote: a remote named `mem-client-*` must be kind `client`, and `mem-orch-*` must be `orch`, or the routes file does not load. The `personal` repo must have kind `personal`. Only folders routed to a `personal`, `project` or `infra` repo may feed the distilled catalog's worklist; `client` and `orch` never do.
- `personal` names the repo that owns no folders by default. It syncs its work-tree and nothing from `~/.claude/projects`, so it is the home of the distilled catalog. The one exception is an exact `folder` pin (no `*` or `?`): that is the only rule that may name it, and it gives the personal repo that one folder (see the home folder below). A path, remote or glob rule naming it is refused when the file loads.
- `routes` is an ordered list, **first match wins**. Each rule has exactly one matcher:
  - `folder`: Claude's escaped folder name (every character that is not a letter or digit becomes `-`), or a glob over it.
  - `path`: a glob over the project directory. The escaped folder name is resolved to the one existing directory it names.
  - `remote`: a glob over the normalised git remote key of that directory's `origin`, `host/owner/repo` (ssh or https, any case, with or without `.git` or credentials, give the same key).
  - In a glob `*` matches within one segment (`github.com/<you>/*` matches `github.com/<you>/app` but not a nested group) and `**` matches across segments.
- A rule may be `{ "folder"|"path"|"remote": "...", "deny": true }` with no `repo`: a match denies the folder and no later rule is tried.
- Repo names may not contain `:` or start with `deny` or `unavailable`.
- `default` can only be `deny`.

### What is denied

A folder is denied, which means no repo reads, mirrors or writes it, when:

- no rule matches;
- the first matching rule names a repo this host does not have (not in `repos`, or its `config.json` is missing). It does **not** fall through to a later, broader rule. Keep the rules of a client on every host: a host that left the client simply removes the `repos` entry and the client's folders stay denied;
- a `path` or `remote` rule needs the project directory and it cannot be found (the project is not checked out here), has several candidates (the escaping is lossy, so `src/my-app` and `src/my.app` look alike), or has no usable `origin`. The folder is denied **at that rule**; the rules behind it are not tried. Pin such a folder with a `folder` rule placed **before** the path and remote rules.
- a `path` or `remote` rule needs a directory whose path passes through a directory with more than 256 entries (a busy temp dir such as macOS `$TMPDIR`), and the part of its name below that directory has a `-` in the escaped folder name (a `/` below it is a `-` too). Such a directory is never listed (that keeps start-up cost flat), and a `-` may stand for `/`, `-`, `.`, `_`, a space, `@`, `+` or a non-ASCII character, so the name cannot be told apart from an unlisted sibling's (`a@b` next to `a/b`). It is left unresolved and denied rather than guessed: a wrong repo is worse than none. `claude-memsync status` lists the live ones ("ambiguous, not synced"). Under such a directory only a direct child with a plain name (no `-` in it) resolves, by probing; every `/` below the directory is also a `-` in the escaped name, so `T/abc/def` is unresolved too. When one split of the name gives up this way, the whole name is unresolved, even if another split through small directories would find a checkout (a huge `~/code` holding `archive` next to `~/code-archive`: the folder for `~/code/archive/x` is not routed to `~/code-archive/x`, and the folder for `~/code-archive/x` itself is unresolved too: any folder whose escaped name could run through the huge directory is left unresolved). Pin a folder you need with a `folder` rule before the path and remote rules.

Order the rules accordingly: `folder` pins first, then `path` rules, then `remote` rules. A rule that cannot place a folder denies it on the spot, so that a client folder whose remote cannot be read is never caught by a broad own-repo rule further down; a pin or a path rule that is placed after a remote rule is therefore never reached for such a folder. A project directory resolves to the origin of the checkout that contains it, so a subdirectory or a linked worktree of a client checkout is the client's. The remote is read with `git remote get-url origin` (so `url.<base>.insteadOf` applies) and ssh host aliases are resolved with `ssh -G`; inherited `GIT_*` environment variables are ignored.

A denied folder's memory files and `*.jsonl` transcripts are never read for sync, and `related suggest` never lists or opens the folder and counts it toward no signal.

Each denied folder is logged once, by a short hash of its name and the reason, never by name or note contents. Keep rules for your own repos narrow (`github.com/<you>/*`, never `*`): a client's folder can only reach an own repo through an own rule.

Rules are read once, at start; the denial decisions are re-checked on each pull tick, so a new checkout or a changed `origin` is noticed. Restart the daemon after editing `routes.json`.

### Renamed or transferred repos

Renaming a repo, or transferring it to another owner (`github.com/<old-owner>/<repo>` becomes `github.com/<new-owner>/<repo>`), changes the remote key a `remote` rule matches. Handle it by adding the old or the new name as an alias of the rule:

```json
{ "remote": "github.com/<new-owner>/<repo>", "aliases": ["github.com/<old-owner>/<repo>"], "repo": "projects" }
```

`aliases` (remote rules only, lowercased, globs like `remote`) match exactly as the main pattern does. Restart the daemon after editing.

As a safety net, when a folder is first routed by a remote rule the daemon also records the checkout's root commit(s) (`git rev-list --max-parents=0 HEAD`), the remote key, and the names its rules listed, in `identity.json` next to `owners.json`. If the remote key of that same folder later changes:

- the new key still routes to the same repo: the record is updated and nothing else happens;
- the root commits differ from the recorded ones: another repository sits at that path, so nothing is inferred from the record. A key no rule matches is denied and `status` shows `different repository at this path`; a key a rule matches routes as for any new folder;
- the new key matches no rule and the root commits are the recorded ones: the folder keeps routing to its recorded repo, `status` shows `renamed remote: add an alias` with the days left, and a line (folder hash only) is logged once. After `renameGraceDays` (a number in `routes.json`, default 30, `0` denies at once) the folder is denied. The period starts once and is not restarted by further name changes; adding an alias, or a rule matching the folder again, ends it. For a folder of a `client` or `orch` repo this only applies to a rename inside the same owner (`github.com/<client-org>/<old>` to `github.com/<client-org>/<new>`); a move to another owner is denied at once;
- the new key routes to a **different** repo: the folder is never moved by itself. It is denied and `status` shows `remote changed repos` until you edit `routes.json`. The edit is the consent only when it lists a name for that repo that it did not list before (a new pattern or alias for the key; unrelated edits and reordering do not count). To take a folder out of a `client` or `orch` repo the name must be the literal key (a pattern or an alias without `*` or `?`).

The fallback only returns a folder to the repo it was already recorded in, so it never moves a folder into a client or orchestrator repo, or out of one. It needs a record for that folder: a new folder that shares a root commit (a fork, a second clone) gets none. A shallow clone has no usable root commit, so it is recorded without one and is denied after a rename as before. A repo with several roots (merged histories) matches only when the whole sorted set equals the recorded one. Root commits are looked up once per checkout and `HEAD`.

`identity.json` is written under a file lock and merged per folder, so two daemons do not lose each other's records. A file that does not parse is never overwritten: the folders that were routed before are denied and `status` says so, until you fix or delete the file. Only the daemon writes `owners.json` and `identity.json`; `status`, `distill` and the hooks only read.

### How the repos stay apart

Each loop only touches the folders routed to its repo, in every place it handles Claude folders: the start-up and periodic reconcile, the file watcher, the copy of pulled files into `~/.claude`, the catch-up after a manual `git pull`, restores of held-back files, and the manifest of deletes. A note that repo A pulls into `~/.claude` is therefore never seen as new by repo B.

Editing a route moves a folder. The new repo treats the folder's files as Claude-only and copies them in; the old repo stops touching the folder and its copy stays frozen in git until you `git rm` it by hand. Because the old repo's manifest forgets the folder, a later move back restores that repo's files instead of deleting them. A move from a `client` or `orch` repo to one of another kind is logged (`folder <hash> moved from client repo ...`). Each host remembers the previous owners in `owners.json` next to `routes.json`.

### Service, PID and lock

There is still one service unit (`claude-memsync`), one process and one PID file. The PID file lives in the personal repo's sync dir (else the first repo's by name). Each sync dir takes a lock (`.state/daemon.lock`) for the life of the process, and a daemon, routed or not, refuses to start when another one holds any of the dirs it needs. The lock is released when the process dies, so a crash leaves nothing stale. One loop failing does not stop the others, and neither does one repo failing the private-only check: that repo is held and re-checked with backoff (see the table of checks).

Install the unit as usual; with a routes file present, `run` loads every repo of `routes.json` this host has, and `claude-memsync install` needs no legacy `~/.claudesync/config.json`: without `--config` it uses the personal (primary) repo's config (`--config <path>` still wins).

### Commands

- `status` works without an installed service (it prints `service: not installed` and carries on) and lists each repo with the number of folders it owns, the number of denied folders, and the rules an earlier rule of the same kind shadows (a later rule that can never be reached first).
- `stop` clears the pull marker of every repo.
- `changes --hook` reports, for the project directory, the notes changed in each routed repo, each against its own per-session marker. A repo reports only the folders it owns.
- `distill` and `changes` without `--config` use the personal repo's config (the primary one); an explicit `--config` always means that single repo.
- `distill`, `archive` and `recipients` take `--repo <name>` (a key of `routes.json` `repos`, or else the GitHub repo name such as `mem-project-x`, or `owner/name`, matched against each routed repo's remote URL; a name that two repos share is refused with their keys listed, and an exact key always wins) or `--config <path>`, and `--routes <path>`; giving both `--repo` and `--config` is an error. `distill` still defaults to the personal repo and still drops client folders from its worklist. `archive` without either reads every repo and labels each line `[repo]`. `recipients` never assumes a repo: with a routes file and neither flag it refuses and lists the repos. `distill --prune` and every `recipients` action that writes take that repo's sync-dir lock; when the lock is held and the claude-memsync service is installed and running, they stop the service, wait up to 30 s for the lock, do the work and start the service again (see "Adding a host" below for the rules). If the lock is held by anything else they refuse.
- `init` shows the distilled catalog path and the permissions hint only for the personal repo (or in single-repo mode).
- `distill --prune` leaves an entry alone when the folder it came from is not in `~/.claude/projects` on this host: the entry was made on another host, and "file gone" is not the same as "folder never here".

### Home folder and setting up a new host

The home folder is the Claude folder of sessions started in your home directory (escaped like `-home-<user>` or `-Users-<user>`). Where it goes is a per-host choice: an orch repo on one host, an infra repo on a homelab, the personal repo on a Mac, or nowhere. It is always one exact `folder` rule, kept at the top of `routes.json`.

```
claude-memsync route home                  # show the pin and where the folder routes today
claude-memsync route home <repo>           # set or replace the pin (the repo must be in "repos")
claude-memsync route home deny             # write a deny pin: the folder is denied, whatever other rules say
claude-memsync route home <repo> --yes     # same pin, stated non-interactively (never replaces --confirm-move-out-of-client)
claude-memsync route home --unpin          # remove the pin; the folder then routes by the other rules
claude-memsync route home --folder <name>  # use another folder name than the one computed from $HOME
claude-memsync route home --suggest        # print one suggestion and why; changes nothing
claude-memsync route home --ask            # list the repos by kind, read one choice (terminal only)
```

The suggestion comes from hints only: on macOS the personal repo (if configured); else an `orch` repo, then an `infra` repo, configured on this host; else `deny`. A home folder already pinned to the personal repo keeps that pin. `--ask` numbers the repos under personal / project / infra / orch / client / deny, marks the suggestion, and applies the choice you type (a number, a name, or Enter for the suggestion); without a terminal it refuses and tells you to pass the repo. `deny` never removes the pin, so a broader rule behind it cannot take the folder. Any change whose resulting route goes from a `client` or `orch` repo to any other repo (repos carry no client name, so another client's repo counts too), or from denied to a non-client repo on a host that keeps client or orch repos (including `--unpin`), needs `--confirm-move-out-of-client`; a move out of a client repo is logged like any other. The edit is atomic and happens under `routes.json.lock`; restart the daemon afterwards.

To start a new host from another one, publish a template from a host that already works, then inherit it:

```
# on the host that works (the registry repo's work-tree is the "registry" field of routes.json, or --registry)
claude-memsync route publish --as laptop-2

# on the new host, after the registry has synced and its repos exist in routes.json
claude-memsync route inherit laptop-2
claude-memsync route inherit laptop-2 --home --yes
```

`publish` writes `hosts/<label>/routes.json` in the registry repo and commits that one file locally; the registry's own sync pushes it. It holds the rules and the names and kinds of the repos they name, and nothing else: no config paths, sync dirs, remote URLs, keys or tokens. `client` and `orch` repos are left out, and every rule naming one (or a repo this host lacks) is published as a deny: `{"folder": "{home}-<client>*", "deny": true}` keeps the pattern and names no repo, so a host that inherits it denies those folders instead of letting a broader rule catch them. The patterns are kept, so client names can appear in the registry; use a private registry. A client host's own client rules are written by hand on that host. The home pin is stored as "home", and a leading home in a folder or path pattern (`-home-<user>`, `/Users/<user>`) as `{home}`, which `inherit` expands for the local user. Other patterns (remote globs, folder names) are published as written, so do not put a name in them that you would not put in the registry. The label is a name you choose (`^[a-z0-9][a-z0-9-]{0,31}$`); the machine's host name is never used or recorded.

`inherit` keeps the new host's `repos` and every setting of its own and replaces its rules with the template's, keeping only rules whose repo exists in this host's `repos` with the same kind. A rule it cannot take over (its repo is missing here, or has another kind) is printed with the reason and stays as a deny in its place, so a broader rule behind it never catches its folders. A client route is never added for a repo the host lacks. The template's home pin applies to this host's home folder only with `--home`; without it the host's own pin stays. It then shows the changes and asks before writing (`--yes` skips the question; without a terminal and without `--yes` nothing is written). It also routes every folder this host has, and one probe per client or orch rule, under the old and the new rules; if any folder would leave a client or orch repo (a dropped or shadowed client rule), or the home folder would newly go to a non-client repo on a host that keeps client notes, it lists them and refuses without `--confirm-move-out-of-client`. The check is repeated under the file lock before writing. Only the `routes` list is rewritten; every other field of `routes.json` is kept as it was. `route home <repo>` pins without a prompt when the host keeps no client or orch repo (`--yes` states that for scripts and changes nothing else). It likewise needs `--confirm-move-out-of-client` for a non-client target on a host with any client or orch repo, and `--ask` then wants a typed `yes`.

## Migrating from another sync tool

If another tool synced Claude memory folders before claude-memsync, it can keep
writing into them after you think it is off: a timer you disabled but a plugin
hook that still fires, or a daemon that was never stopped. claude-memsync would
then mirror whatever it wrote, including copies filed in the wrong folder, into
the repos the folders route to.

The `foreignWriters` list is the primary guard. The quiet window below is a
weak extra signal.

### foreignWriters

List the predecessor under `foreignWriters` in `routes.json` (applies to every
repo) and/or in a repo's `config.json` (applies to that repo; `init` keeps it
when you re-init). Each entry is exactly one of:

```json
"foreignWriters": [
  {"file": "~/.old-sync/config.json", "jsonKeyNonEmpty": "repo"},
  {"systemdUnit": "old-sync.timer"},
  {"process": "old-sync"},
  {"claudePlugin": "old-sync@marketplace", "onlyIfFile": "~/.old-sync/config.json"}
]
```

- `file`: active if the file exists and, when `jsonKeyNonEmpty` is set, that
  top-level key holds a non-empty value. A file that exists but cannot be parsed
  counts as active.
- `systemdUnit` (Linux only): active if `systemctl --user is-enabled` or
  `is-active` says enabled or active.
- `process` (Linux only): active if a running process's command line contains
  the text. claude-memsync's own process is ignored, but choose text that does
  not also match a claude-memsync command line.
- `claudePlugin`: active if `name@marketplace` is enabled in `settings.json` or
  `settings.local.json` of the Claude config dir (`CLAUDE_CONFIG_DIR`, else
  `~/.claude`). A settings file that exists but cannot be read or parsed counts
  as enabling it. `onlyIfFile` names the plugin's own config: when set and
  missing, the plugin is inert. `unlessFile` is the opposite: an enabled plugin
  is inert while that file exists.

Off Linux only `file` and `claudePlugin` entries are checked; `systemdUnit` and
`process` entries are skipped with a log line. On Linux, a `systemdUnit` or
`process` entry that cannot be checked (no `systemctl`, no `/proc`) refuses the
start like an active one does.

If any entry is active, `run` and `init` exit non-zero with one line naming the
kind and the configured value (never a note name), and `status` shows
`blocked: foreign writer <kind> <value>` (per repo in routed mode).
`--ignore-foreign-writers` starts anyway and logs a warning per entry.

While a routed daemon runs, the list is checked again on every routes reload
and every supervised loop restart. The first time an entry is seen active, the
repo it applies to stops mirroring, committing, pulling and pushing. This is
logged once (`foreign writer seen: restart after cleanup`) and shown in
`status`. The hold is sticky: it does not lift when the writer stops, because
the copies it left behind would then be mirrored. Clean up those copies, then
restart the daemon; the start-up check runs again. The daemon itself keeps
running and the other repos carry on. Notes mirrored before the writer was seen
are not undone.

Single-repo mode has no runtime recheck: the list is checked at `run` and
`init` only.

### Quiet window

A busy host writes memory all the time, so the window is limited to the
migration moment: the first start after `init` for each repo (a marker in
`.state/` records that it has been done). Before the first sync, claude-memsync
snapshots (size, modification time, SHA-256) the memory files of every folder it
owns, waits `startQuietSec` seconds (default 15, in `config.json`; 0 turns it
off), and snapshots again. If anything in an owned folder changed, it refuses
with `owned folder changed during start-up quiet window: something else writes
there`, naming the folder by hash (the first 8 hex digits of the SHA-256 of the
folder name). Stop the other writer and start again.

`init` always runs the window. Later starts skip it; `run --quiet-window`
forces it, and `--skip-quiet-window` (on `run` or `init`) skips it for a
deliberate first start. `--ignore-foreign-writers` does not skip it.

## Related projects

A repo can name other repos whose notes are worth seeing while you work in it, without joining them. Relations are declared in the registry repo (a normal sync dir, for example `mem-registry`), never in the project repos, so a public project repo never names a memory repo.

In `routes.json`:

```json
{
  "registry": "~/.claudesync.d/mem-registry",
  "relatedRoot": "~/.claudesync.d/related",
  "relatedRemotes": { "mem-project-<b>": "git@<read-only-deploy-key-alias>:<org>/mem-project-<b>.git" },
  "repos": {
    "a": { "config": "...", "kind": "project", "remote": "mem-project-<a>" }
  }
}
```

`remote` is the repo's name on the remote (default: the basename of its config's remote URL); relations use it. `client` (optional, client and orch repos only) names the client; default: the part of the repo name after `mem-client-` or `mem-orch-`. A declared value must equal that or be a `<client>-` prefix of it. `relatedRoot` defaults to a `related` dir beside the registry and must neither lie inside nor contain any sync dir, the registry or `~/.claude` (symlinks are resolved).

The registry file is `related.json` in the registry repo: `[{"repo": "mem-project-<a>", "related": [{"repo": "mem-project-<b>", "mode": "read"}]}]`.

```
claude-memsync relate mem-project-<a> mem-project-<b> --read     # or --join
claude-memsync unrelate mem-project-<a> mem-project-<b>
claude-memsync related [mem-project-<a>]                          # list, with this host's state
claude-memsync related sync                                       # fetch the read clones now
claude-memsync related context --project <dir>                    # the session index (below)
```

`relate` and `unrelate` edit the registry and commit it there (the registry's own daemon pushes it).

**read** (the default idea): a fetch-only copy at `<relatedRoot>/<repo>`, fetched from the URL in `relatedRemotes` (in practice a read-only deploy key's ssh alias) on the daemon's pull interval and at `related sync`. The URL must name the related repo (its last path segment, lower-cased, without `.git`), and a later fetch refuses a URL that differs from the one the copy was made from. The copy is never mirrored into `~/.claude`, never watched, and has no push path: it is a separate type with no Push, and its push URL is disabled. Nothing fetched is ever checked out: objects live in a bare store (`<relatedRoot>/.bare/<repo>.git`) and the readable files are written straight from the stored blobs, so no filter, hook or attribute that arrives with the content runs on this host. Each fetch has a deadline (`relatedFetchTimeoutSec`, default 60), never prompts (`GIT_TERMINAL_PROMPT=0`, ssh `BatchMode=yes`), and a failed one is shown as "stale since <time of the last good fetch>" in `related` and in the session context.

If the copy is encrypted (`.age-recipients`) and this host cannot decrypt it, it is listed as "not readable on this host" and skipped. The ciphertext is still fetched and kept (the recipients file is only known after the fetch); it is never shown or indexed.

**Trust.** The registry is shared, so an edge in it does nothing on a host until that host accepts it. Accepted edges (ends and mode) are pinned in `related-accepted.json` beside `routes.json`, never synced. `relate` on a host accepts its own edge; `related accept` shows what is new, changed or gone, and `related accept --yes` pins it. Unaccepted edges are listed as ignored.

**join**: nothing is automated. `related sync` prints the steps for making this host a full member (the normal `init`, a `repos` entry, a route).

**Boundaries (default deny)**, checked when a relation is declared and again whenever the registry is loaded (a hand-edited edge that fails is listed as refused and ignored):

- Both ends must be `mem-*` repos, and private (`mem-` names only until the private-only check lands).
- Relations are among `mem-personal`, `mem-project-*` and `mem-infra-*`, or between two repos of the same client (both kind client, same client name).
- Join needs both ends to be the same kind.
- No edge to or from a `mem-client-*` repo from anything else, and none between different clients.
- `mem-orch-all` may read `mem-orch-<client>`, and nothing else; never join. Every other orch edge is refused.

**Visibility cache.** A cached verdict that says a repo is not private hides it from every command and hook, whatever the token setup, while it is younger than `relatedMaxStaleHours` (default 24, at most 24: a larger value is clamped). Hooks never ask GitHub; the daemon refreshes the verdicts in the background, and a stale or missing one hides the related repo until it does.

**Session context.** Put `claude-memsync related context --project "$CLAUDE_PROJECT_DIR"` in a SessionStart hook. It prints, for the repo that owns the project, one entry per read relation: the repo name, note count, read-only path and the headings of its `MEMORY.md` (never note bodies), within `--per-relation` (default 2000) and `--total` (default 6000) characters. Headings and changed paths that match the owning repo's denylist are counted, never printed. It prints nothing for a project without relations and never fails the session. `changes --hook` also lists, under a "related, read-only repos" label, the paths that changed in those clones since the session last looked. Allow `Read(<relatedRoot>/**)` and nothing wider.
## Related-project suggestions

`claude-memsync related suggest` offers candidate read relations computed from this host's own data. It never changes anything: an edge becomes a relation only when you accept it.

It is scoped to the repo that owns `--project` (default `$CLAUDE_PROJECT_DIR`, resolved as `related context` does): only candidates with that repo at one end are listed or counted, and a refused candidate is counted only when its other end may be known to the viewer (never a client repo to a session outside that client). The viewing repo's denylist applies to everything printed, as well as each end's. For a candidate with a client or orch end the shared vocabulary is shown as a count only, and module paths only to a session of the same client.

```
claude-memsync related suggest [--project DIR] [--json] [--min-score N] [--co-open-hours N]
claude-memsync related accept mem-project-<a> mem-project-<b> [--share]
claude-memsync related dismiss mem-project-<a> mem-project-<b>
```

Signals, strongest first (a suggestion lists every signal that supports it, and its score is set by the strongest):

1. **Co-opened projects.** Two routed project folders whose Claude sessions (`~/.claude/projects/<folder>/*.jsonl`) overlapped, or started within `--co-open-hours` (default 24) of each other, on at least 2 different days. Only each line's `cwd` and `timestamp` are read; message content is never parsed or kept, and a line over 1 MB is skipped unread.
2. **References between checkouts.** The checkout of a routed folder (found the way routing finds it) names another routed repo's remote in `.gitmodules`, `go.mod` (`require` or `replace`), `package.json` dependencies given as git or GitHub references, or a `github.com/<owner>/<repo>` link in its README. Only the matched module or URL is shown. This signal gives the direction: the referring repo reads the referred one.
3. **Same owner, shared vocabulary.** Two repos whose checkouts have the same remote owner and whose `MEMORY.md` headings share at least 2 distinctive words (4 letters or more, stopwords ignored). Weakest.

Every candidate must pass the related-project policy (the same check as `relate`): a candidate it refuses is never shown, only counted ("N withheld by policy"). Edges already accepted on this host, declared in the registry or dismissed are not suggested again, and the mode is always `read`. Anything printed that matches the denylist of either repo is counted ("N hidden by denylist") and not shown. The output never includes note text, transcript text or file contents.

`related accept <from> <to>` checks the policy again and pins the edge in this host's `related-accepted.json`; the registry is untouched. With `--share` it also writes the edge to `related.json` in the registry and commits it there (the registry's sync pushes it). `related dismiss` records the pair in `related-dismissed.json` beside `routes.json`; it applies to this host only. With no `<from> <to>`, `related accept` is the bulk review of registry edges described above.

`related context` (the SessionStart line) adds `N related-memory suggestions; run `claude-memsync related suggest`` when N is above zero for the session's owning repo. It never computes anything: it reads `related-suggest-cache.json` beside `routes.json` and shows nothing when there is none. The cache holds only candidates the policy allows, as repo names, signal kinds and scores (no word, module path or note text); the policy, acceptance, dismissals and denylist are applied afresh on every read. The daemon refreshes the cache on its tick within a time budget (`relatedSuggestBudgetSec`, default 5), reading transcripts incrementally (by mtime and offset, remembered in `related-suggest-index.json`: times only), and an unfinished refresh continues on the next tick.

## Encryption at rest

**`$GIT_DIR/info/attributes` is tool-owned.** In an encrypted clone it holds the filter and merge rules (every file through the age filter, `MEMORY.md` and `archive/INDEX.md` with their own merge drivers). It outranks every `.gitattributes`, so nothing pulled can switch encryption off. The daemon rewrites it, and the filter and merge-driver settings in the clone's git config, at every start; edits made there by hand are lost. The programs the config already names are kept while they exist. A missing one is replaced by the running executable, unless that runs from a temp dir or a go build cache, in which case nothing is rewritten and one line is logged.

`claude-memsync init --encrypt --remote <url>` encrypts every file in the sync
repo with [age](https://age-encryption.org) before it reaches git, so the remote
host stores only ciphertext. It is opt-in: a repo made without `--encrypt` is
unchanged. `--encrypt` supports a new (empty) remote or one that is already
encrypted; it refuses a remote that holds plaintext history.

How it works:

- A git clean/smudge filter (`claude-memsync age-filter`, git's long-running
  filter protocol) encrypts on `git add` and decrypts on checkout, so the work
  tree and the Claude tree stay plaintext. Encryption is deterministic (cached
  by content and recipient set), so checkouts and rebases never leave the clone
  dirty. The filter is `required`: if the binary is missing, git fails instead of
  storing plaintext.
- `claude-memmerge` decrypts the three versions, merges and writes ciphertext
  back. Notes use a plain line merge (`--text`) whose conflict markers are
  readable in the work tree; `MEMORY.md` keeps the semantic merge.
- `.gitattributes`, `.gitignore` and `.age-recipients` (public keys only) stay
  plaintext, at the repo root only; nested copies are encrypted like any file.
  Everything else is encrypted. The filter rules are written to
  `$GIT_DIR/info/attributes`, which outranks every `.gitattributes`, so an
  upstream or nested `.gitattributes` cannot switch encryption off.
- Commit messages are `memsync: N files`: no note names, no host name. A neutral
  author is set when git has none configured.
- Before every push the daemon checks that each blob it would send (outside the
  three files above) starts with `age-encryption.org/v1`, and refuses the push
  otherwise.
- The secret scrub reads the plaintext, not the ciphertext.

### Encryption scheme: post-quantum by default

`config.json` `encryptionScheme` is `pq` (the default for `init --encrypt`) or
`classic` (upstream-compatible; also what a config without the field means).
Under `pq` every recipient is a hybrid ML-KEM-768 + X25519 age key (`age1pq1...`,
about 1,960 characters; identity `AGE-SECRET-KEY-PQ-1...`). `init --encrypt`
generates a hybrid host identity (mode 0600, never printed) and prints only the
public recipient. `--recipient`, `--recovery-recipient`, `recipients add`,
`join accept` and `recipients label recovery` refuse classic X25519 and ssh keys
under `pq` (age itself cannot mix the two), and a classic repo refuses hybrid
keys. The pre-push guard also refuses classic ciphertext from a `pq` repo. A
join request carries the long key, so its size cap is 4 KiB (the format is still
exactly four lines). Choose the other scheme with `init --encryption-scheme`.

Migrate an existing classic repo whose only member is this host:

    claude-memsync recipients rekey-pq --repo <name> | --all \
        --recovery-recipient <age1pq1...> --host <label> [--fresh-root] [--apply]
    claude-memsync recipients rekey-pq --repo <name> | --all --rollback [--discard-local-commits]
    claude-memsync recipients rekey-pq --repo <name> | --all --drop-backup
    claude-memsync recipients retire-classic [--repo <name> | --all] [--apply]

It is a dry run unless `--apply` (daemon stopped): it prints the repos, the
current recipient count and the file counts, and says when history will be
replaced. Before `--apply` changes anything locally it runs every check that can
refuse the push (the private-only and push-URL checks, and the push guard on the
built commit); then it records what a rollback needs in
`.git/memsync-pre-pq.json` (mode 0600, public values only: paths, public keys,
labels, commit ids). `--apply` creates a new hybrid identity beside the old one
(`identity-pq.txt`), sets `.age-recipients` to exactly this host
(`# host:<label>`) and the recovery key (`# recovery`), records the scheme and
re-pins, re-encrypts every file at HEAD in one commit and moves
`.state/synced-head` with it. With `--fresh-root` that commit is an orphan
root, force-pushed with `--force-with-lease` on the old tip. Only after the push
succeeds does it save `encryptionScheme`, `hostLabel` and `recoveryRecipients`
in the config file it was given, check that origin/main holds the migrated tip
(with `--fresh-root`: one commit; always: hybrid ciphertext only; it prints
counts only) and retire the classic identity. A verification failure after the
push is reported as `pushed; verification failed: ...` and is not a push
failure; the classic identity is then kept. It refuses when the recipients
include any key other than this host's and the configured recovery keys, when
the work-tree is dirty or the branch differs from origin/main, and for a
read-only host.

With `--all`, an `--apply` run stops at the first repo that fails and prints
which repos are done, which failed and which were not started. Fix the cause
and rerun the same command: finished repos are skipped, a repo that was
migrated locally but whose push failed (the remote still at the backup tip) has
its push retried, and a crash between moving HEAD and repointing
`memage.identity`/`memage.scheme`/the pin is finished. A rerun must use the same
`--host`, `--recovery-recipient` and `--fresh-root`. The record also holds the
migrated commit (`newCommit`) and a stage (`started`, `committed`, `migrated`,
`published`). A finished record, or an origin/main that contains `newCommit`
(the daemon synced on top), makes a rerun print `already done`, write
`config.json` and the stage if the crash came before they were saved, and
retire the classic identity, whatever the flags; a dry run on a finished repo says the
same and exits 0. Every resume stage repoints `memage.identity`,
`memage.scheme` and the pin again and resets an index that differs from HEAD
(a crash between moving HEAD and updating the index). The record is written
through a temp file, fsynced, renamed, and its directory fsynced; an
unreadable record is reported as such, with the advice not to drop the backup.
`--apply` refuses while `.state/parked.json` or `.state/pull-running` exists.

`--rollback` undoes a migration that was never published: for a repo whose
remote still equals `refs/memsync/pre-pq` it resets the branch to it and
restores `memage.identity`, `memage.scheme`, the pin, `config.json` and the
synced-head record (HEAD is read under the sync lock); it refuses when the
remote has already moved off the backup (or the work-tree is dirty). It
requires HEAD to be `newCommit` (or the backup tip, when there is nothing to
discard; for an older record without `newCommit` the inferred migrated commit
is used): otherwise it lists the commits made since
(count and subjects) and needs `--discard-local-commits`. It first loads the
classic identity, renaming `<name>.classic-retired` back if needed, and refuses
if neither file exists. With `--all`, a published repo is reported as
`published; nothing to roll back`. The new hybrid identity file is left in
place and reused by a later `rekey-pq`.

While a repo's record is not `published`, or the repo is not post-quantum
yet, the daemon does not sync it: each cycle logs
`rekey-pq: not syncing this repo this cycle` and `status` prints a
`rekey-pq: not syncing` line. The interactive changes (`join accept|reject|request`, `recipients add|remove|accept|label`, `distill --prune`) refuse such a repo before they stop the service or take the lock, name it, and say to finish or roll back the rekey first. `join accept` and `join reject` also stop when the clone cannot be brought up to date with origin (a divergence, an unreachable remote) rather than deciding on a stale view. A record written by an older build (no stage, no
`newCommit`) blocks while the repo is not pq, while the cached
`origin/main` is still the backup tip (the push never happened), and while the
migrated commit cannot be established as below. For such a
record the migrated commit is inferred from the history when origin/main has
moved off the backup tip (with `--fresh-root` the roots origin/main has that
the tip does not, else the commits right after the tip), accepting exactly one
whose `.age-recipients` is this host's new key plus the recovery key and which
HEAD contains. A repo where that fails stays held and a rerun says "cannot tell
which commit was the migration; nothing changed". A rerun completes the record (`newCommit`,
`published`) and prints `already done` without touching `config.json`;
`--rollback --discard-local-commits` lists the commits since that commit, and
refuses if it cannot be inferred.

The classic identity is renamed `<name>.classic-retired` only when no git clone
beside the sync directories still names it in `memage.identity`: every
`*/.git/config` under the parent directory of each sync dir (and the default
sync dir) is checked (a clone whose git config git cannot read counts as using the file), paths compared after `~` expansion and symlink resolution,
including clones that are not routed (such as a `*.incident-*` copy). A clone
whose restore record names the file as its classic identity and is not finished
keeps it too (a rollback of that clone would need it). So does a related read clone (under the related root) whose `.age-recipients` still lists the classic identity's recipient, or cannot be read: those decrypt with the host's default identity. If one is found, the
file stays, the clone is named in the output, and a rerun retires it once
nothing uses it. `recipients retire-classic [--repo <name> | --all] [--apply]`
does just that step with no rekey flags and the same scan of all clones: it
finds the file in the repo's record or as the current identity's name without
`-pq`, reports what it would do without `--apply`, and renames it with it.

The pre-migration tip stays at `refs/memsync/pre-pq`; `rekey-pq --drop-backup`
refuses while the rekey is unpublished (a finished record, or for an older
record the inferred migrated commit must be on origin/main, which has moved
off the backup tip, and HEAD must contain it), because the backup is the
rollback point. Dropping it is irreversible for that clone: the classic
history is then unreachable here and no rollback is possible. It deletes the ref and its record, expires the reflogs of HEAD, `main` and
`origin/main` (not the stash) and runs `gc --prune=now`, so the classic
ciphertext leaves the local clone. This removes classic ciphertext locally
only after a `--fresh-root` run: without it the classic history is still the
branch's own history and stays. Other hosts that were members of the old repo
must re-clone.

### Keys

`init --encrypt` generates a per-host identity (hybrid under `pq`, X25519 under
`classic`) (mode 0600; default `identity.txt` in the
[config directory](#the-config-directory), or `--identity <file>`; after a
`rekey-pq` the hybrid one is `identity-pq.txt` beside it) and
prints only the **public** recipient. An existing age identity file, or an
unencrypted SSH ed25519/RSA key (age cannot use ssh-agent, so a passphrase-protected
key does not work for a daemon), can be used instead. Back the identity up
offline: without a listed key the data is unrecoverable.

Pass an **offline recovery recipient** at init (`--recipient <public-key>`,
repeatable) or add one later with `recipients add`. Keep that key's secret off
every synced machine.

### Adding and removing a machine

A second machine runs `init --encrypt` against the same remote. It is *keyless*
until its public key is listed: the daemon commits and pushes nothing, copies
nothing into the Claude tree (it only pulls), and `status` says so. On a machine
that can read the repo:

    claude-memsync recipients add <public-key>
    claude-memsync recipients list
    claude-memsync recipients remove <public-key>

With several repos, name the one: `recipients --repo <name> add <public-key>` (or `--config <path>`); without it the command refuses and lists the repos. Each edits `.age-recipients`, re-encrypts every file to the new set and commits
(the daemon pushes). The commit is built from HEAD alone: anything else staged
in the sync repo is neither committed nor unstaged. These actions (and
`accept --yes`) need the sync dir's lock, which the daemon holds. If the
claude-memsync service is installed and running, the command stops it
(`stopping the claude-memsync service for this change…`), waits up to 30 s for
the lock, makes the change and starts the service again (`restarted`), also when
the change fails, panics or is interrupted. After the start it asks the service whether it runs: a daemon that exited at once (for example refused by the foreign-writer guard) gets a `warning: ... is not running` line pointing at `status`, not `restarted`. `recipients label ... --all` stops and starts the service once for the whole loop. A first Ctrl-C lets the change finish
and then restarts the service; a second restarts it at once and exits. If the lock
is held and the service is not running (another command, a foreground `run`, no
service installed) nothing is stopped or started and the command refuses. A
service that was not running is never started. `rekey-pq` and `host retire` still
refuse; stop the daemon yourself for them. The re-encryption commit is not reported by `changes --hook`: files whose text is the same at both ends are skipped, so a recipients change does not announce every note as changed. A removed key cannot read content written after the change,
but it still reads everything it already fetched; rewriting history does not
help once a key has fetched it, so treat removal as "stop future leakage".

### Adding a host to an existing repo (join requests)

`recipients add` needs the new host's public key to travel to a member by hand.
A join request does that through the repo itself, with a person confirming a
fingerprint. Nothing is ever accepted automatically, and no daemon code path
accepts.

**On the new host** (after `init --encrypt` against the remote, which leaves it
keyless):

    claude-memsync join request --repo <name> --label <host-label>
    claude-memsync join status [--repo <name>]

`--label` matches `^[a-z0-9][a-z0-9-]{0,31}$`. `request` writes
`joins/<host-label>.txt`, commits **only that path** and pushes. The file holds
exactly four lines, a public key and no secret:

    label: <host-label>
    recipient: <this host's age public key>
    fingerprint: <first 128 bits of the sha256 of the recipient, 32 hex characters>
    requested: <UTC RFC3339 time>

It stays plaintext: `joins/*.txt` is exempt in `.gitattributes` and
`$GIT_DIR/info/attributes`. The commit is built in a private index from HEAD
plus that one file, so whatever else is staged is never committed, even though
the host is keyless. The push guard accepts plaintext only for paths matching
`^joins/[a-z0-9-]+\.txt$` whose content matches the format above; anything else
is still refused. The command prints the fingerprint: give it to the person
who will accept. Running it again is harmless (it reports the pending request);
`join status` shows `pending` or `accepted`.

The fingerprint is printed on one line as eight groups of four hex characters:
`fingerprint: 0123 4567 89ab cdef 0123 4567 89ab cdef`. `request` first brings
the clone up to date with origin and refuses (committing and pushing nothing) if
that fails or if the local branch differs from origin in any way: what it
pushes is exactly one new commit on origin's tip that adds
`joins/<host-label>.txt`, and it checks that before the push.

**On a member host** (one that can decrypt the repo):

    claude-memsync join list [--repo <name>]
    claude-memsync join accept <host-label> --repo <name> --fingerprint <fp> [--allow-unlabelled]
    claude-memsync join reject <host-label> [--repo <name>]
    claude-memsync join accept <host-label> --all --fingerprint <fp> [--allow-unlabelled]
    claude-memsync join reject <host-label> --all
    claude-memsync join list --all

`list` fetches and shows each pending request as label, fingerprint and time, then
the label it asks for (`host:<label>`) and `this host` when the key is the local
host's own (a
file that is not in the strict format is listed as INVALID and cannot be
accepted). `status` also says `N join request(s) pending` per repo. `accept`
requires `--fingerprint` to equal, exactly, the fingerprint in the file (the
grouped form or the same 32 characters without spaces; nothing else is
forgiven); without one, or with a different one, it refuses. Compare it with what the new host
printed, over a channel you trust. On success it adds the recipient (as
`recipients add` does: re-encrypts HEAD to the new set), removes the request
file in the same commit, and pushes. The recipient is recorded with the requester's
label (`# host:<label>`), as `recipients add --host` does. `accept` refuses a label that
is this host's own `hostLabel` or is already bound in the pin, and refuses while any
recipient other than a configured recovery key (`recoveryRecipients` in config.json, set by
`init --recovery-recipient <key>` or `recipients label recovery <key> [--repo <name> | --all]`;
a `# recovery` comment alone exempts nothing) has no host label (run
`recipients label self --host <label>` on that host first, or pass `--allow-unlabelled`).
On success `accept` also prints the accepting host's own key:
`accepted by host:<label>, key fingerprint <fp>; on the new host, join status must
show this key` (`this host (unlabelled)` or `(recovery)` when its line has no host
label), so the new host compares one known line. `reject` removes the file in its own
commit and pushes. Like `recipients`, `request`, `accept` and `reject` stop a running
service for the change and start it again; they refuse if the lock is held by
anything else.

`--all` (for `accept`, `reject` and `list`; not with `--repo` or `--config`) acts on every
encrypted repo of the routes file. `list --all` prints one table (repo, label, fingerprint, time,
host). `accept --all` fetches, finds the repos with a request from `<host-label>` and plans before it
changes anything: a repo whose request has a different fingerprint is a mismatch, and **one mismatch
anywhere refuses every repo** (a second key under one label is the likely cause); the
mismatching repos are named. Otherwise each repo is accepted by the same code as the single-repo
`accept` (all its refusals apply, per repo), under one stop and one start of the service for the
set. A read-only repo, a repo with an unfinished rekey (`RekeyBlocksSync`) and an unencrypted repo are
skipped and not touched. One line per repo reports `accepted and pushed`, `no request`,
`fingerprint mismatch`, `refused (<reason>)` or `skipped (...)`. Exit 0 only if every repo with a
request was accepted; 1 on a mismatch, a refusal, a skip of a repo that had a request, or when no repo has a request
from the label; 2 for a usage error. `reject --all` removes the label's request in each such repo
the same way (no mismatch check). `--fingerprint` with `reject` or `list` is refused (exit 2). Every encrypted repo is fetched; a repo whose origin cannot be fetched refuses all (`cannot fetch`). The mismatch check compares HEAD and the remote tip of every repo, a diverged clone included (`list` and `status` still skip a diverged tip), treats a request that does not parse as a mismatch, and is repeated under the lock for the repos that will be changed, after each is synced with origin; one that cannot be synced refuses all (`cannot sync`). Repos that are skipped (read-only, rekey-blocked) are looked at once, before the lock. The check under the lock is local: a request another host pushes afterwards is caught only by that repo's push being rejected, which is reported while earlier repos stay accepted. An interrupt stops between repos and reports what was accepted. After the report, `accept --all` prints the accepting host's own key fingerprint per accepted repo, as the single-repo form does. `list --all` with nothing pending exits 0.

**Back on the new host:** its next pull brings the new `.age-recipients`. Its
own key is then a change to the pinned set, so the existing gate applies: run
`claude-memsync recipients accept --yes`, but **do not run it blindly**: anyone
with write access to the remote could have added keys besides yours. `join
status` on the new host lists the pending recipient set, one `recipient:` line
per key with its fingerprint, the key itself and its label (`host:<label>`,
`recovery` or `unlabelled`), marking new keys and this host's own. Compare those lines with `claude-memsync join list` (fingerprints)
and `claude-memsync recipients list` (keys) on a member host, and accept only
if every key is one you expect. Only then does the host start to commit and
sync.

### Reviewing recipient changes

A review can also list a changed `.gitattributes` (marked `!`, not as a key): the
repo's `.gitattributes` differs from the one seen when the recipients were pinned.
The message names the commit (`git -C <dir> show <commit> -- .gitattributes`). On a
host's first join it is expected, because the repo's own filter lines arrive with the
first pull; anywhere else, find out who changed it before accepting.

Anyone with write access to the remote could add their own key to
`.age-recipients`. Each host therefore **pins** the recipient set it has
reviewed (in its git dir, never committed). Init pins the set it first sees
(trust on first use), and `recipients add|remove` update the pin. The filter
encrypts only to the pinned set. If a pull brings a different
`.age-recipients`, the host logs a warning once, `status` shows
`recipients UNREVIEWED`, and nothing is committed or pushed (pulling continues)
until a person runs `claude-memsync recipients accept` (it shows the added and
removed keys as short fingerprints; add `--yes` to accept). A changed
`.gitattributes` anywhere in the repo is held to the same review. Accepting
re-encrypts everything in HEAD to the new set and commits it, so no blob a newly
admitted host cannot read is pushed afterwards. Ciphertext a host cannot read is
never written into the Claude tree. A machine that was
admitted by another host sees exactly this on its first pull and accepts it.

### What GitHub (or any host) can still see

Encryption hides note contents and the diffs inside them. The host still sees:
file names (project folder hashes and note names), file sizes (plaintext plus
about 200 bytes per classic recipient, or about 1.5 KB per hybrid one), commit times and counts, branch names, the
commit graph, and the recipients' public keys. It cannot search or render the
notes, and its secret scanning sees only ciphertext. Hiding names and timing
would need name hashing or a different tool.

### Limits

- Recipients you add apply to new content; old history stays readable to
  whoever could read it.
- The ciphertext cache (`.git/memage-cache`) holds ciphertext only and is not
  pruned; delete the directory at any time.
- Moving the `claude-memsync` or `claude-memmerge` binary breaks the configured
  paths (re-run `init --force`, or fix the `filter.memage.*` and `merge.*` git
  config).

## Host lifecycle

Hosts are often disposable machines. Each one goes by a **label**, a name you
choose (`^[a-z0-9][a-z0-9-]{0,31}$`, for example `vm-3`; the machine's host name
is never used). Set it with `init --host-label <label>` (saved as `hostLabel` in
config.json). The label ties together what a host leaves behind:

- its **deploy keys** are titled with the label (`gh repo deploy-key add <key-file> -t <label>`);
- its **age recipient** in each repo's `.age-recipients` carries a trailing comment
  `# host:<label>`. `init --encrypt --host-label` writes it for the first host of a repo,
  and `recipients add <public-key> --host <label>` for the next ones; a host that
  predates labels runs `recipients label self --host <label> [--repo <name> | --all]`,
  which labels its own line, commits, pushes, re-pins and sets `hostLabel` (a label already
  bound to another key is refused). Lines without the comment stay valid and are treated
  as unlabelled. **The label is part of what the pin reviews**: it is recorded with the
  key in the set this host pinned, so a label changed or swapped upstream shows up as an
  unreviewed recipients change (`recipients list`, `recipients accept`), and `host retire`
  resolves a label through the pin, never through the comment text;
- the **registry** may hold `hosts/<label>/` (routing template, expiry, check-key record).

### host list

    claude-memsync host list [--org <org>] [--stale-days N] [--json] [--check-key-warn N]

For every `mem-*` repo in the org (never any other repo) it lists the deploy keys
(read/write or read-only, last used), joins them with the labelled recipients of
the repos this host has and with the registry, and prints one row per label: last
use, flags, keys per repo, recipient presence, registry presence, expiry and
check-key fingerprint. `stale` means no key of the host was used in N days (default
30; a key never used is judged by its age). `expired` means the host's published
expiry date has passed. Listing deploy keys needs a token with **Administration:
read** on the repos; the token is read from the usual places (`githubTokenFile`,
`MEMSYNC_GITHUB_TOKEN`, `--token-file`). Output never contains key text.
`doctor` prints a one-line summary of stale and expired hosts when there are any.

### host retire

    claude-memsync host retire <label> [--org <org>]            # plan only (dry run, the default)
    claude-memsync host retire <label> [--org <org>] --apply    # act

Retiring a host closes what it could still reach. The plan lists counts and names;
no key material is ever printed. With `--apply`:

1. **Deploy keys.** Every deploy key titled `<label>` is deleted on every `mem-*`
   repo of the org (nothing else is touched). This needs a token with
   **Administration: write**; without it the command stops at the first refusal with
   a message naming the permission, before anything else is changed.
2. **Recipients.** For each repo this host has, the label is resolved through the
   pin to one key; that recipient is removed, HEAD is re-encrypted for the remaining
   recipients and the commit is made (the daemon pushes it). The plan prints the short
   fingerprint of the key per repo. The command **refuses**, before changing anything,
   when the label is bound to more than one key in a repo or to different keys in
   different repos. It needs the daemon off: the sync-dir locks of every repo it
   writes to are taken first and **held until the command ends**, and if one is held
   the command stops with nothing changed. **History stays readable to the retired
   key and is not rewritten**: it keeps everything it already fetched; only new
   content is closed to it.
3. **Cannot re-encrypt here.** A repo the host had a key for but this host lacks,
   a repo this host cannot decrypt, a repo whose recipients changed upstream and are
   not reviewed here, and a repo where the retired host was the only
   recipient are listed as `cannot re-encrypt here: run retire on a member host, or
   archive the repo`. Nothing is changed there.
4. **Registry.** `hosts/<label>/` is removed from the registry work-tree and the
   removal committed locally (the registry's own sync pushes it). If the host
   recorded a check-App key, the plan and the result print the exact page to delete
   it (`https://<git host>/organizations/<org>/settings/apps/<app-slug>`, from your
   config and the registry) and its fingerprint, as a manual step: GitHub only lets
   you delete App keys in the UI. The fingerprint is printed *as recorded in the
   registry*; verify it in GitHub's UI before deleting, since anyone who can write to
   the registry can change it (the live-key count in `host list` has the same caveat).
   With no check key recorded, nothing is printed.

Retiring this host's own label is refused; run it from another host. The command ends
with a summary: `retire complete`, or a `still pending` list (for example `STILL A
RECIPIENT in mem-x: re-run`). A re-run is idempotent: keys already gone are counted,
recipients already removed are skipped, and once nothing is left it reports `nothing
to retire` and changes nothing.

### Read-only membership

    claude-memsync init --remote <url> --read-only

A read-only member pulls and applies notes but never writes back. The daemon pulls
on its usual interval and copies what arrived into the Claude tree (new notes,
edits and deletions), and that is all: it mirrors nothing from `~/.claude`, makes no
commit and no push. Every command that would commit refuses with a clear message
(`recipients add|remove`, `relate`, `unrelate`, `route publish`, `host check-key`);
`recipients accept` only pins the reviewed set and does not re-encrypt. `status`
shows `read-only`. The setting is `readOnly: true` in config.json; it is kept
across a re-init, so an `init --force` never makes a read-only host writable. The
remote must already hold the repo: a read-only host cannot make its first commit.
A note you write locally on a read-only host stays local, and a pulled version of
the same note replaces it.

Give such a host a deploy key **without** write access (`gh repo deploy-key add
<key-file> -t <label>`, leaving out `-w`), so GitHub itself refuses a push. In
`host list` its keys show as `ro`. When a read-only host is retired, repos it is a
member of are listed under "cannot re-encrypt here" (it never commits).

*TODO(join):* this base has no `join` command. When one exists, `join request
--read-only` should mark the request so the accepting member's output reminds them
to add the deploy key without `-w`.

### Expiry

    claude-memsync init --remote <url> --expires 2026-12-31

For a host that is meant to be disposable, `--expires <YYYY-MM-DD>` (config
`expiresAt`) sets the last day it syncs. The day after (UTC), the daemon holds
exactly as it does for a foreign writer: nothing is mirrored, committed, pulled or
pushed, and it logs why once. `status` says `expired: retire me`, and before the
date it shows `expires on <date>`. A malformed `expiresAt` makes the config fail to
load instead of silently never expiring. To extend a host, edit `expiresAt` and
restart the daemon.

When the host has a `hostLabel` and the routes file names a registry, the daemon
publishes the date at start as `hosts/<label>/expiry` (committed locally; the
registry's own sync pushes it; skipped on a read-only member of the registry), so
`host list` on other hosts flags the host `expired` once the date has passed. The
file is removed with the rest of `hosts/<label>/` by `host retire`.

### Check-App keys

A host may have its own private key for a GitHub App used for checks. Record its
fingerprint (as GitHub shows it, `SHA256:...`) and the App's slug, never the key:

    claude-memsync host check-key --fingerprint SHA256:<fingerprint> --app <app-slug>
    claude-memsync host check-key --clear

This writes `hosts/<label>/check-key` in the registry (committed locally; the
registry's own sync pushes it). `host list` shows it per host and counts the live
keys per App: hosts still in the registry that have a fingerprint. GitHub allows at
most 25 keys per App and they can only be deleted in the GitHub UI, so `host list`
and `doctor` warn from 20 live keys (`--check-key-warn`, or `checkKeyWarn` in
config.json). With no check key recorded, nothing is shown or counted.

## Limitations / known issues

- **Path consistency required**: Claude derives the per-project memory directory
  name by escaping the project's absolute path. PCs that open the same repo at
  different paths (e.g. `C:\src\foo` vs. `D:\dev\foo`) will see them as different
  projects.
- **No live conflict UI**: when two PCs edit the same lines of a file (for
  `MEMORY.md`, overlapping edits within the same section body), the daemon
  aborts the pull and stops syncing until you resolve it by hand; see
  [Rebase conflicts](#rebase-conflicts).
- **Auto-start runs while logged in**: the daemon stops when the user logs off.
  Acceptable since memories don't change while you're away. For 24/7 sync,
  register a system-wide service manually or enable systemd lingering.
- **Stop is a hard kill**: by design — git operations are atomic per command, so
  we don't risk corruption. If a `.git/index.lock` is left behind by an unrelated
  git crash, remove it manually.
