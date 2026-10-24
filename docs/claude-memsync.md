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
| `requireDenylist` | `false` | When `true` and `denylist.txt` does not exist, commit nothing and log why. For setups where "no denylist" means "not configured yet". Ignored when `scrub` is `false`. |

Existing `config.json` files without these keys get the defaults, so scrub is
on after an upgrade, `requireDenylist` is off, and no denylist is needed.

## Telling a running session what changed

Claude Code reads a project's memory when a session starts. If another PC
pushes a note while a session is open for hours, that session never sees it.
`claude-memsync changes` lets a hook say so:

```sh
claude-memsync changes (--since <rev> | --marker <file>) [--project <dir>] [--json] [--limit N] [--update-marker]
claude-memsync changes --hook            # for UserPromptSubmit / SessionStart hooks
```

It compares the sync repo's current `HEAD` with an earlier point and lists the
memory files that were added, changed or deleted in between. It only reads the
repo; it never fetches (the daemon does that), so it reflects what this PC has
already pulled or committed itself.

| Flag | Meaning |
|---|---|
| `--since` | A git revision (full or abbreviated SHA, branch, `HEAD~3`). A revision that doesn't resolve is an error (exit 1). Exactly one of `--since` and `--marker` is required. |
| `--marker` | The path of a **marker file** whose first line is the base commit. |
| `--update-marker` | Only valid with `--marker`. Rewrites the marker to the current `HEAD` afterwards (atomically, mode `0600`). A marker that doesn't exist yet is created and nothing is reported: that call just sets the baseline. An empty marker (say, one made with `touch`), or one whose commit is gone (garbage-collected after a rebase, or the repo was re-cloned), is reset the same way: nothing is reported, `"reset": true` is set in the JSON, one warning line goes to stderr, and the exit code is 0. Without `--update-marker` an unresolvable marker is an error. A file whose first line is not a commit id (7 to 64 hex digits) is not a marker: it is never overwritten, and the command fails with `marker <path> does not contain a commit id` (exit 1). The marker is never moved backwards: if `HEAD` is an ancestor of the commit it already names (the repo was reset to an older commit), it is left alone. |
| `--project` | Only this project: a project directory (`/home/alice/src/app`) or Claude's folder name for it (`-home-alice-src-app`). |
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
the file on this PC. Only files directly under a project's `memory/` folder are
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
{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"Memory notes changed on another machine since you last looked (read a file before relying on it):\nadded /home/alice/.claude/projects/-home-alice-src-app/memory/feedback_tests.md\nchanged /home/alice/.claude/projects/-home-alice-src-app/memory/MEMORY.md"}}
```

with that framing line, then one `status path` line per change (and `… and N more` past `--limit`),
and nothing at all when nothing changed. The marker only moves after that
output was written, and never backwards. The first call in a session just
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
- There is a brief window between the daemon's pull and its copy into
  `~/.claude`: a change can be reported before the file at `local` has been
  updated.

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
