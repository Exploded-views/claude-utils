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
- `git` 2.x on PATH at runtime (the daemon shells out to it).
- A private git remote you can `git push` to from your terminal. The daemon
  inherits your normal git credentials (Git Credential Manager on Windows, SSH
  agent or `~/.gitconfig` credential helpers on Linux/macOS). Confirm `git push`
  works against the remote before installing.
- Either the same project paths on each PC (Claude's project-hash directory
  names are derived from absolute paths, so if your repos live at the same
  drive letter / mount point on every PC, you're set), or
  [`projectKey: "remote"`](#matching-projects-by-git-remote-instead-of-path)
  if they don't.

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
  unioned)
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
| `claude-memsync init --remote <url>` | One-time bootstrap. `--project-key path\|remote` sets [how projects are identified](#matching-projects-by-git-remote-instead-of-path); `--force` re-initializes an existing sync dir and keeps its current `projectKey` unless the flag is given. |
| `claude-memsync install` | Register for auto-start at next logon. |
| `claude-memsync uninstall` | Remove the auto-start hook. |
| `claude-memsync start` | Start the daemon now. |
| `claude-memsync stop` | Stop the running daemon. |
| `claude-memsync status` | Print `running (pid N)` or `stopped`. |
| `claude-memsync run` | Run in the foreground (for debugging). |
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
- Conflicts in non-`MEMORY.md` files surface as standard git conflict markers in
  the affected file. Rare in practice because each memory file has a unique
  filename per topic.

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
- `~/.claudesync/.state/projects.json` — per-PC (remote mode's folder cache)
- `~/.claudesync/.state/mode` — per-PC (the `projectKey` mode the sync dir was
  set up for)
- `~/.claudesync/daemon.pid` — runtime state
- `~/.claudesync/distilled/DISTILLED.md` — derived index, regenerated per-PC
  (the distilled `<slug>.md` entry files themselves **are** synced)

If you want any of the additional `~/.claude/` content synced, that's a future
enhancement.

## Matching projects by git remote instead of path

By default a project is identified by the folder name Claude Code gives it,
which is the project's absolute path with every non-alphanumeric character
replaced by `-`. `/home/alice/src/app` on one PC and `/home/bob/code/app` on
another are different projects and never share memory.

Run `claude-memsync init --remote <url> --project-key remote` to identify a
project by its git `origin` remote instead. The mode is chosen when the sync
dir is created and stays (see [Switching modes](#switching-modes)):

```json
{ "projectKey": "remote" }
```

| Key | Default | Meaning |
|---|---|---|
| `projectKey` | `"path"` | `"path"`: today's behavior. `"remote"`: a project whose directory is a git checkout with an `origin` remote is stored by remote. Any other value is rejected at startup. It must match the mode the sync dir was initialised with, see [Switching modes](#switching-modes). |

### Repo layout

A remote-keyed project is stored in the sync repo at

```
remotes/<host>/<owner>/<repo>/memory/<file>
```

for example `remotes/github.com/acme/app/memory/MEMORY.md`. The remote URL is
normalized so that every spelling of the same repository gives the same key:
`git@github.com:Acme/App.git`, `https://github.com/acme/app` and
`ssh://git@github.com:2222/acme/app.git` all become `github.com/acme/app`
(lower-cased, `.git` and credentials removed, port dropped). Hosts that nest
groups keep every segment (`gitlab.example.com/group/sub/app`).

`remotes/` sits beside `projects/` rather than inside it so the two layouts can
never be mistaken for each other, and so that a PC still running in `path`
mode (or an older build) simply ignores it. Projects that have no usable
remote keep their path key under `projects/<hash>/memory/` exactly as before:

- the directory isn't a git checkout, or has no `origin`
- `origin` is a local path or `file://` URL (not the same string on another PC)
- the directory is a subdirectory of a repo rather than its top level (two
  subprojects of a monorepo would otherwise share one memory folder)
- the directory is a linked git worktree (see
  [Worktrees and several clones](#worktrees-and-several-clones))

Normalization is purely textual, so spellings that don't look alike give
different keys even when they reach the same repository:

- an ssh host alias from `~/.ssh/config` (`git@work-github:acme/app.git`) keeps
  the alias as the host
- `ssh.github.com:443` is a different host from `github.com`
- Azure DevOps over ssh (`git@ssh.dev.azure.com:v3/acme/proj/app`) and over
  https (`https://dev.azure.com/acme/proj/_git/app`) have different shapes

PCs that spell a remote differently don't share memory until you join them with
the cache's `key` override (below): give the odd folder the key the other PCs
derive, for example `"key": "github.com/acme/app"`.

Segments are limited to `[a-z0-9._-]`; anything else becomes `_`, and a
segment that Windows reserves as a device name (`con`, `prn`, `aux`, `nul`,
`com1`-`com9`, `lpt1`-`lpt9`, with or without an extension) gets a `_` prefix.
Trailing dots are dropped too, since Windows drops them from a directory name
(`app.` is `app`), and a `key` override in the cache must already be in that
normalized form.
A remote with an `@` left in its path or its host is rejected, because that is
what an unescaped `/` or an `@` in a password leaves behind (`user:p@ss@host:o/r`
is refused rather than read as host `ss@host`).

### Finding the directory behind a folder name

Claude's folder name is lossy: `/`, `.`, `_`, space and `-` all become `-`, so
`-home-alice-src-my-app` could be `src/my-app`, `src/my.app` or `src/my/app`.
To learn the remote the daemon walks the filesystem down from the root,
following only entries whose escaped name can still match, and collects every
directory that escapes to the folder name. Then:

- exactly one match: use it
- several matches that all have the same remote: use it
- several matches with different remotes: **guess nothing**. The project keeps
  its path key and the daemon logs a warning. It records the candidates in the
  cache so you can pick
- none (the checkout was deleted or lives on another PC): the folder keeps
  the key it last had, or its path key if it never had one

On Windows and macOS the walk compares names case-insensitively, as those file
systems do; elsewhere exactly.

A folder's key is **sticky**. The daemon stores the last key it resolved
(`lastKey`) and keeps using it whenever it can't get an answer: the directory
is gone or unmounted, git isn't installed, git refuses with a "dubious
ownership" error, or fails for any other reason. The key only changes on
positive evidence: git ran and reported no `origin`, or a different one. So
deleting a checkout doesn't fork its notes into `projects/<hash>/`.

The result is cached per PC in `~/.claudesync/.state/projects.json` (`.state/`
is git-ignored, so this file is never synced). The file is meant to be
edited by hand:

```json
{
  "version": 1,
  "projects": {
    "-home-alice-src-my-app": {
      "candidates": ["/home/alice/src/my-app", "/home/alice/src/my.app"]
    },
    "-home-alice-src-old": {
      "dir": "/home/alice/src/old",
      "lastKey": "github.com/acme/old"
    },
    "-home-alice-src-app": { "dir": "/home/alice/src/app" },
    "-home-alice-scratch": { "key": "path" },
    "-home-alice-src-app-fork": { "key": "github.com/acme/app" }
  }
}
```

- `dir` pins the directory a folder name stands for (fill it in for an
  ambiguous entry). The daemon re-derives the key from it on every refresh.
- `key` overrides the repo-side key outright, or `"path"` to force the path
  key for that project. It wins over everything else. It must look like what
  normalization produces: at least three `/`-separated segments (host, owner,
  repo), each made of
  `[a-z0-9._-]` and none of them `.` or `..`. An invalid value is ignored with
  a logged warning (the file is left as you wrote it) and the folder is
  resolved as if it were absent, so a typo can't point memory outside
  `remotes/`. A key another folder on this PC already holds is refused too
  (see [Worktrees and several clones](#worktrees-and-several-clones)).
- `lastKey` is **the claim, not a note**. It is the key this folder holds on
  this PC, and no other folder can take that key while it stands. The daemon
  writes it, but editing it moves ownership: set it on a folder and that folder
  holds the key (to the exclusion of whichever folder held it before), clear it
  and the claim is gone. Stop the daemon first, and prefer the `key` override
  for moving a claim (see
  [Worktrees and several clones](#worktrees-and-several-clones)).
- `candidates` is written by the daemon and informational.
- `joinPending` and `resetJoin` are written by the daemon while a folder that
  gained or changed its key waits for its join, and for a reset-join when the
  folder had another key before. See [Joining a key](#joining-a-key).

**Stop the daemon before editing `projects.json`.** The cache is read at
startup, and the daemon rewrites the file whenever resolution changes, so an
edit made while it runs can be overwritten; start the daemon again afterwards.
The daemon writes the file atomically. A leading UTF-8 byte order mark (added by
some Windows editors) is ignored. If the file can't be parsed (a bad hand
edit) or its `version` is not one this build knows (a missing `version` is
accepted), the daemon logs the reason, renames it to
`projects.json.corrupt-<unix time>` and carries on with an empty cache, which it
rebuilds. If it can't be read at all (permission denied, or a directory in its
place) the daemon logs that, carries on with an empty in-memory cache and never
writes the file, so fix the file and restart. Only project folders that
exist under this PC's `~/.claude/projects` are resolved or cached; the hashes
of projects other PCs pushed under `projects/` are not.

A folder that appears while the daemon runs is resolved the first time it is used.
All folders are re-resolved at startup and on the pull interval
(`pullIntervalSec`, default 3600), just before the full reconcile pass. That
refresh is skipped while local changes are still waiting for their debounced
commit; it runs on the next tick instead.

### Worktrees and several clones

Current Claude Code keeps **one** auto-memory directory per repository, shared
by all its worktrees and subdirectories (it belongs to the main checkout). A
session in a linked worktree still creates its own project folder under
`~/.claude/projects`, but that folder only holds transcripts and has no
`memory/`. memsync therefore keys only a plain checkout by remote. A linked
worktree (told apart by `git rev-parse --git-dir` differing from
`--git-common-dir`) or a subdirectory keeps its path key, and nothing is ever
copied into it.

**At most one folder per PC holds a given remote key.** The first folder to
claim a key keeps it. The claim is recorded as that folder's `lastKey` in
`projects.json`, so it survives restarts, and it survives the checkout being
deleted. Any other local folder that resolves to the same key (a second clone of
the repo on this PC) falls back to its path key, `projects/<escaped>/memory/`,
exactly as in `path` mode, and the daemon logs one line saying so. That second
clone's notes stay isolated: they are neither merged into the shared notes nor
overwritten by them.

A folder that holds a remote key then behaves **exactly like a path-keyed
folder**: the same reconcile, the same watcher events, the same pull
propagation, the same delete rules, `rm -rf` of its `memory/` included (see
[Deletes](#deletes)). The only difference is the repo-side directory. There is
no group merge and no per-PC record.

To move the claim to another clone, stop the daemon, set `"key": "path"` on the
folder that holds it and `"key": "github.com/acme/app"` (the key) on the one that
should, and start the daemon. A `key` override is subject to the same rule: it
cannot join a key another folder already holds.

**A claim is released only when nothing of its holder is left**: its Claude
folder (`~/.claude/projects/<hash>`) no longer exists, the directory recorded
for it gives "no such file", and no directory still decodes from its name. Then
its cache entry is dropped and a new clone of the repo can claim the key. If the
Claude folder still exists but the checkout is gone, the claim does **not** move
by itself (the folder may still be in use, or the checkout may be on an
unmounted drive). A clone that wants the key is refused, and the daemon logs
once that the holder's checkout is gone and the exact edit that moves the claim:
stop the daemon, set `"key": "path"` on the holder and `"key": "<the key>"` on
the new clone in `.state/projects.json`, and start it again.

### Joining a key

A folder **gains** a key when it claims it for the first time (a new clone), or
when a claim is moved to it as above. (A folder whose key changes from one
remote key to another is reset instead, see
[Changing a folder's key](#changing-a-folders-key).) Before the daemon acts on anything for that folder (a watcher event, a
copy to the repo, a pulled change), it **joins** the folder to the notes already
stored under the key, once. Otherwise a fresh clone's first `MEMORY.md` would
overwrite the shared index, and a moved claim would let the new holder's stale
notes replace the current ones. The join is not a normal reconcile: the shared
side wins and nothing is deleted.

- `MEMORY.md` is merged section by section (as everywhere else) and written to
  both sides.
- Any other file that differs keeps the shared version, in the repo and in
  Claude. The folder's own version is saved next to the shared copy as
  `<name>.local-before-join-<random>` (kept out of git, local to this PC, never
  cleaned up; look, then delete).
- A file only the repo has is copied into Claude.
- A file only the folder has is copied into the repo.
- The manifest is **not** used: the folder has no history with this key, so a
  file missing on one side says nothing about a delete.

A folder that is waiting for its join is recorded in `.state/projects.json` as
`"joinPending": true` from the moment it gains the key until the join has run.
If the daemon dies in between, the next start joins the folder before any normal
reconcile of it. The daemon clears the flag; leave it alone.

### Changing a folder's key

When a folder that holds a remote key gets another one (the repo's `origin`
changed, say after a rename or a transfer, or the key was edited by hand), the
daemon logs the change with the folder name and both keys, and **resets** the
folder to the new key's notes instead of joining it. The folder's own notes may
have been deleted, or may be stale, in ways nothing can tell from a note that is
new, so none of them is pushed:

- A file only the repo has (under the new key) is copied into Claude.
- A file that differs, `MEMORY.md` included, takes the shared version in Claude
  and the repo; the folder's own copy is saved next to the shared one as
  `<name>.local-before-join-<random>`.
- A file only the folder has is **not** pushed. It is moved out of the Claude
  folder into a `<name>.local-before-join-<random>` backup next to the shared
  notes, so it cannot be imported again either.
- The daemon logs one line per reset folder, with the number of local files set
  aside.

Nothing is lost: it all stays on this PC as local backups, which you can look at
and copy back by hand. The old key's directory in the repo is not touched and is
never removed by the daemon. Because the reset takes the shared notes as they
are, update `origin` on every PC that shares the project at about the same time
and let them sync; a PC that has not updated yet stays on `remotes/<old key>/`
and sees none of the others' changes.

A folder going from the path key to a remote key, or a new claim, is not a reset
(it had no remote key before) and joins the ordinary way.

The daemon never removes an old `projects/<hash>/` copy from the repo. It may be
the live notes of another sync dir that is still in `path` mode, and removing it
would delete them there. Clean up old copies by hand.

`*.from-remote-*` backups (made when a file differs between a PC and the repo
copy) and `*.local-before-join-*` backups (made by a join) exist only on the PC
that produced them, are never synced, and are never cleaned up by the daemon;
delete them by hand once you've looked. They are kept out of git by the sync
repo's `.git/info/exclude`, which is per-PC and never committed: in `remote`
mode `init` and the daemon add the patterns to it (at every start), but only
when the sync dir is itself the top of a git work tree, never to the file of a
repo that merely encloses it. If that file already negates a pattern (say
`!*.from-remote-*`) the pattern is not appended after it; the daemon logs that
and carries on. The tracked `.gitignore` lists them as well, but a sync dir made before
`*.local-before-join-*` existed has an older one, so the daemon does not rely on
it. As a second line of defence a file with a backup name is never copied from
the repo into a Claude folder, and reconcile and joins skip such names in the
Claude folder too, so a backup that an older client pushed does not spread.

### Switching modes

A sync dir is set up for one mode, `path` or `remote`, and keeps it. `init`
records the mode in `~/.claudesync/.state/mode` (per-PC, written atomically). The
daemon refuses to start if `projectKey` in `config.json` differs from it, and so
does `init --force` into the same sync dir with another `--project-key`. The
error says what to do. A sync dir made by an older version has no record and
counts as `path`, so starting it in `remote` is refused too.

**Switching means `init` into a fresh sync dir.** There is no migration: run
`claude-memsync init --remote <url> --project-key remote --sync-dir <new dir>`
on every PC that shares the project, and stop using the old sync dir. A remote-
keyed folder's notes in the new sync dir are filled from the local Claude folder
(the source of truth) and from what other PCs have pushed under `remotes/`. The
old `projects/<hash>/` copies stay in the repo and are not updated by a
remote-mode PC; the daemon logs one line per remote-keyed folder whose old
`projects/<hash>/memory` copy is still in the repo. A PC left in `path` mode
keeps writing to `projects/` and won't see the notes of the others, so switch
every PC that shares a repo and delete no notes until all of them have.

A fresh clone on a new PC is joined to the shared notes (see
[Joining a key](#joining-a-key)): once Claude Code has created the project
folder (opening the repo does that), the next reconcile, or the first watcher
event for it if that comes first, copies the shared notes into it, with or
without a `memory/` directory yet, and merges what the folder already had.

## Deletes

Deletes propagate across PCs. The daemon keeps a per-PC manifest
(`~/.claudesync/.state/manifest.json`) listing which Claude-side memory files
were present at the last successful sync. On each reconcile pass, a file that's
in the mirror, missing from Claude, **and** was in the manifest is treated as a
user delete and removed from the mirror; the next push propagates it. A file
that's in the mirror, missing from Claude, but **not** in the manifest is treated
as an inbound new file from another PC and copied into Claude.

A remote-keyed folder follows exactly the same rules, with only its repo-side
directory (`remotes/<key>/memory/`) differing; see
[Worktrees and several clones](#worktrees-and-several-clones).

This means:

- Delete a memory while the daemon is running → propagates when the change is
  flushed (the watcher catches it).
- Delete a memory while the daemon is stopped → propagates on next startup
  (manifest-driven reconcile).
- First-ever run on a PC has no manifest, so deletes can't be inferred from prior
  state. The daemon takes the safe path: never infer deletes, bring everything
  together. Subsequent runs work normally.
- Removing a whole `memory/` directory (`rm -rf`) is the same as deleting each
  file in it, in both modes: the files are removed from the repo, and from every
  other PC when they pull. Do it only when you mean it.
- Removing a remote-keyed folder's whole **project folder**
  (`~/.claude/projects/<hash>`, say because the clone is gone) is not a delete
  of its notes, which other PCs share. The files' remove events are ignored.
  While its claim is still held (the checkout is there), the daemon restores
  `remotes/<key>/memory` from HEAD before the next commit, once per run of the
  daemon (a folder that comes back and goes away again is a new disappearance).
  When the claim is released (nothing of its holder is left),
  the restore happens at that moment, before any other clone can claim the key
  in the same pass, and only if no live folder holds the key: a new holder's
  join must not be undone by a restore from HEAD. (Path mode is unchanged:
  there the same `rm -rf` deletes the notes.)

## On-disk layout

```
~/.claude/projects/<hash>/memory/         # what Claude reads + writes
    ├── MEMORY.md
    └── <topic>.md ...

~/.claudesync/                            # owned by the daemon
    ├── config.json                       # per-PC (gitignored)
    ├── daemon.pid                        # per-PC (gitignored)
    ├── .git/                             # the sync repo
    ├── .gitattributes                    # MEMORY.md merge=claude-memory-index
    ├── .gitignore                        # excludes config.json, .state/, etc.
    ├── .state/manifest.json              # per-PC delete-detection state
    ├── .state/projects.json              # per-PC folder->directory cache (remote mode)
    ├── distilled/                        # shared distilled-memory catalog
    │   ├── DISTILLED.md                  # derived index (gitignored)
    │   └── <slug>.md ...                 # distilled entries (synced)
    ├── projects/<hash>/memory/...        # git work-tree mirror (path-keyed)
    └── remotes/<host>/<owner>/<repo>/memory/...  # mirror of remote-keyed projects
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

- **Path consistency required (by default)**: Claude derives the per-project
  memory directory name by escaping the project's absolute path. PCs that open
  the same repo at different paths (e.g. `C:\src\foo` vs. `D:\dev\foo`) will see
  them as different projects. Initialise with `--project-key remote` to match git
  checkouts by their `origin` remote instead; see
  [Matching projects by git remote](#matching-projects-by-git-remote-instead-of-path).
- **No live conflict UI**: when the merge driver emits actual conflict markers
  (rare; only on overlapping line edits within the same `MEMORY.md` section
  body), the file is committed and pushed with markers. The next time you edit on
  that PC you'll see them; resolve by hand and save.
- **Auto-start runs while logged in**: the daemon stops when the user logs off.
  Acceptable since memories don't change while you're away. For 24/7 sync,
  register a system-wide service manually or enable systemd lingering.
- **Stop is a hard kill**: by design — git operations are atomic per command, so
  we don't risk corruption. If a `.git/index.lock` is left behind by an unrelated
  git crash, remove it manually.
