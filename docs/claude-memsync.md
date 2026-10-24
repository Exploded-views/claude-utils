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
| `claude-memsync init --remote <url>` | One-time bootstrap. |
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

Set `"projectKey": "remote"` in `~/.claudesync/config.json` (per PC, then
restart the daemon) to identify a project by its git `origin` remote instead:

```json
{ "projectKey": "remote" }
```

| Key | Default | Meaning |
|---|---|---|
| `projectKey` | `"path"` | `"path"`: today's behavior. `"remote"`: a project whose directory is a git checkout with an `origin` remote is stored by remote. Any other value is rejected at startup. |

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
- none (the checkout was deleted or lives on another PC): path key

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
    "-home-alice-src-app": { "dir": "/home/alice/src/app" },
    "-home-alice-scratch": { "key": "path" },
    "-home-alice-src-app-fork": { "key": "github.com/acme/app" }
  }
}
```

- `dir` pins the directory a folder name stands for (fill it in for an
  ambiguous entry). The daemon re-derives the key from it on every refresh.
- `key` overrides the repo-side key outright, or `"path"` to force the path
  key for that project. It wins over everything else.
- `candidates` is written by the daemon and informational.

The cache is read at startup (restart the daemon after editing it), and
entries are re-resolved on the hourly tick and when Claude creates a new
project folder.

### Several local folders for one repo

Worktrees, or two clones of the same repo, give several Claude folders with
one key. They share the single repo-side folder and are kept as a union:

- a file any of them has is copied to the others
- `MEMORY.md` versions are merged section by section, as between PCs
- other files that differ keep the most recently modified version; the older
  one is saved as `<name>.from-remote-<random>` next to the repo-side copy,
  same as a cross-PC collision
- a file is deleted from the repo only once **every** local folder has lost it.
  Deleting it in one worktree alone is undone on the next pass, because the
  daemon can't tell that from a worktree that never received the file yet

### Switching an existing setup

Nothing is moved or deleted. When a PC switches to `remote`, its remote-keyed
projects are seeded into `remotes/...` from the local folders (the source of
truth). The old `projects/<hash>/` folders stay in the repo, are no longer
updated by that PC, and can be removed with `git rm` once every PC has
switched. Switch all PCs that share a repo; a PC left on `path` keeps writing
to `projects/` and won't see the others' memory.

Memory reaches a PC only for projects Claude Code has opened there at least
once, because that is what creates the local folder the daemon syncs into.
Open the repo once on the new PC; the daemon fills the folder in as soon as
it appears.

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
  them as different projects. Set `projectKey` to `remote` to match git
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
