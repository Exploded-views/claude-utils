# claude-memsync — syncing Claude Code memories across workstations

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

- Go 1.23+ to build.
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
git clone https://github.com/MarimerLLC/claude-utils.git
cd claude-utils
go build -o bin/ ./cmd/...
```

For a release build that stamps the version into the binary:

```sh
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin/ ./cmd/...
```

A plain `go build` (no ldflags) still produces a working binary —
`claude-memsync version` falls back to the VCS revision Go embeds automatically
(e.g. `dev+a52d68609b6e`). See the README's *Releasing* section for details.

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
  `~/.config/systemd/user/claude-memsync.service` and enables it.
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
| `claude-memsync status` | Print `running (pid N)` or `stopped`. |
| `claude-memsync run` | Run in the foreground (for debugging). |
| `claude-memsync changes --since <rev>` or `--marker <file>` | List memory files changed in the sync repo since a point (`--project`, `--json`, `--limit`, `--update-marker`, `--hook`). See [Telling a running session what changed](#telling-a-running-session-what-changed). |
| `claude-memsync distill` | Rebuild the distilled-memory catalog index (`--prune`, `--dry-run`). See [distilling-memories.md](distilling-memories.md). |
| `claude-memsync version` | Print version. |

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
| `--hook` | Hook mode, see [Hook mode](#hook-mode). Takes only `--config`, `--project` and `--limit`. |
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
claude-memsync archive list [--since 2026-10-01]
claude-memsync archive search <term> [--show]
```

Symlinks in the archive are never followed, and paths matching the denylist
are not printed (they are counted instead). `search` matches file names and contents, case-insensitively, and prints the
matching paths with a count of matching lines. `--show` also prints the lines.
Archived notes are not injected into sessions; search them on demand. With
several repos, each repo's archive is listed or searched in turn.

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
`init --token-file`). The token is never written to `config.json`, logs or
error messages.

| When | Token required | Behaviour |
|---|---|---|
| `init` | yes | Refuses before cloning or pushing if the remote is not a private `mem-*` repo, or if it cannot check. |
| `doctor` | yes | Non-zero exit if it cannot check. |
| daemon start | no | With a token: refuses to start on a non-private repo. Without one: logs once that visibility was verified only at init and by `doctor`, and starts. |

With `privateOnly` on and a token configured, the daemon also re-checks at most
once an hour (conditional request with the ETag; 304 means unchanged). If the
repo is no longer private it stops pushing but keeps committing locally, logs one
line per state change, and `claude-memsync status` says so. A failed API call
keeps the last known state, is logged once, and does not block. Without a token
there is no re-check; schedule `doctor`.

### doctor

```sh
claude-memsync doctor --org <org>
```

Lists the org's repos and exits non-zero, naming each one, if any `mem-*` repo
is public or internal, or if the repo this host joined is not private or lacks
the `mem-` prefix. Public repos without the prefix are ignored. Output is repo
names only. `--org` defaults to `githubOrg` in `config.json`, else the owner of
the remote. `repo create` resolves the org the same way and refuses if none is set.

`claude-memsync doctor --install-timer` writes a daily systemd user timer
(Linux) or launchd agent (macOS) into `~/.config/systemd/user` or
`~/Library/LaunchAgents` (override with `--unit-dir`). It does not enable it;
it prints the command. The timer needs `githubTokenFile`.

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

Config keys: `githubHost`, `recheckMaxFailures`, `recheckMaxAgeHours`, `privateOnly`, `githubTokenEnv`, `githubTokenFile`, `githubApiUrl`, `githubOrg`.

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
- `personal` names the repo that owns no folders. It syncs its work-tree and nothing from `~/.claude/projects`, so it is the home of the distilled catalog. No route may name it.
- `routes` is an ordered list, **first match wins**. Each rule has exactly one matcher:
  - `folder`: Claude's escaped folder name (every character that is not a letter or digit becomes `-`), or a glob over it.
  - `path`: a glob over the project directory. The escaped folder name is resolved to the one existing directory it names.
  - `remote`: a glob over the normalised git remote key of that directory's `origin`, `host/owner/repo` (ssh or https, any case, with or without `.git` or credentials, give the same key).
  - In a glob `*` matches within one segment (`github.com/<you>/*` matches `github.com/<you>/app` but not a nested group) and `**` matches across segments.
- `default` can only be `deny`.

### What is denied

A folder is denied, which means no repo reads, mirrors or writes it, when:

- no rule matches;
- the first matching rule names a repo this host does not have (not in `repos`, or its `config.json` is missing). It does **not** fall through to a later, broader rule. Keep the rules of a client on every host: a host that left the client simply removes the `repos` entry and the client's folders stay denied;
- a `path` or `remote` rule needs the project directory and it cannot be found (the project is not checked out here), has several candidates (the escaping is lossy, so `src/my-app` and `src/my.app` look alike), or has no usable `origin`. The folder is denied **at that rule**; the rules behind it are not tried. Pin such a folder with a `folder` rule placed **before** the path and remote rules.

Order the rules accordingly: `folder` pins first, then `path` rules, then `remote` rules. A rule that cannot place a folder denies it on the spot, so that a client folder whose remote cannot be read is never caught by a broad own-repo rule further down; a pin or a path rule that is placed after a remote rule is therefore never reached for such a folder. A project directory resolves to the origin of the checkout that contains it, so a subdirectory or a linked worktree of a client checkout is the client's. The remote is read with `git remote get-url origin` (so `url.<base>.insteadOf` applies) and ssh host aliases are resolved with `ssh -G`; inherited `GIT_*` environment variables are ignored.

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

There is still one service unit (`claude-memsync`), one process and one PID file. The PID file lives in the personal repo's sync dir (else the first repo's by name). Each sync dir takes a lock (`.state/daemon.lock`) for the life of the process, and a daemon, routed or not, refuses to start when another one holds any of the dirs it needs. The lock is released when the process dies, so a crash leaves nothing stale. One loop failing does not stop the others.

Install the unit as usual (`claude-memsync install --config ~/.claudesync.d/personal/config.json`); with a routes file present, `run` loads every repo of `routes.json` this host has.

### Commands

- `status` lists each repo with the number of folders it owns, the number of denied folders, and the rules an earlier rule of the same kind shadows (a later rule that can never be reached first).
- `stop` clears the pull marker of every repo.
- `changes --hook` reports, for the project directory, the notes changed in each routed repo, each against its own per-session marker. A repo reports only the folders it owns.
- `distill` and `changes` without `--config` use the personal repo's config (the primary one); an explicit `--config` always means that single repo.
- `distill --prune` leaves an entry alone when the folder it came from is not in `~/.claude/projects` on this host: the entry was made on another host, and "file gone" is not the same as "folder never here".

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

**Session context.** Put `claude-memsync related context --project "$CLAUDE_PROJECT_DIR"` in a SessionStart hook. It prints, for the repo that owns the project, one entry per read relation: the repo name, note count, read-only path and the headings of its `MEMORY.md` (never note bodies), within `--per-relation` (default 2000) and `--total` (default 6000) characters. Headings and changed paths that match the owning repo's denylist are counted, never printed. It prints nothing for a project without relations and never fails the session. `changes --hook` also lists, under a "related, read-only repos" label, the paths that changed in those clones since the session last looked. Allow `Read(<relatedRoot>/**)` and nothing wider.
## Encryption at rest

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

### Keys

`init --encrypt` generates a per-host X25519 identity (mode 0600; default
`<user config dir>/claude-memsync/identity.txt`, or `--identity <file>`) and
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

Each edits `.age-recipients`, re-encrypts every file to the new set and commits
(the daemon pushes). The commit is built from HEAD alone: anything else staged
in the sync repo is neither committed nor unstaged. These actions (and
`accept --yes`) refuse to run while the daemon holds the sync dir; stop it first. A removed key cannot read content written after the change,
but it still reads everything it already fetched; rewriting history does not
help once a key has fetched it, so treat removal as "stop future leakage".

### Reviewing recipient changes

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
about 200 bytes per recipient), commit times and counts, branch names, the
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
