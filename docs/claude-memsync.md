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
| `claude-memsync changes --since <rev-or-marker>` | List memory files changed in the sync repo since a point (`--project`, `--json`, `--limit`, `--update-marker`). See [Telling a running session what changed](#telling-a-running-session-what-changed). |
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
- `~/.claudesync/daemon.pid` — runtime state
- `~/.claudesync/distilled/DISTILLED.md` — derived index, regenerated per-PC
  (the distilled `<slug>.md` entry files themselves **are** synced)

If you want any of the additional `~/.claude/` content synced, that's a future
enhancement.

## Telling a running session what changed

Claude Code reads a project's memory when a session starts. If another PC
pushes a note while a session is open for hours, that session never sees it.
`claude-memsync changes` lets a hook say so:

```sh
claude-memsync changes --since <git-sha-or-marker> [--project <dir>] [--json] [--limit N] [--update-marker]
```

It compares the sync repo's current `HEAD` with an earlier point and lists the
memory files that were added, changed or deleted in between. It only reads the
repo; it never fetches (the daemon does that), so it reflects what this PC has
already pulled or committed itself.

| Flag | Meaning |
|---|---|
| `--since` | Required. Any git revision (full or abbreviated SHA, branch, `HEAD~3`), or the path of a **marker file** whose first line is a revision. |
| `--project` | Only this project: a project directory (`/home/alice/src/app`) or Claude's folder name for it (`-home-alice-src-app`). |
| `--json` | Print JSON instead of text. |
| `--limit` | List at most N changes (default 20), then `...N more`. `0` lifts the cap. |
| `--update-marker` | When `--since` is a marker file, rewrite it to the current `HEAD` afterwards. A marker that doesn't exist yet is created and nothing is reported: that call just sets the baseline. |
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
reported. Edits this PC made itself are included, since they are commits in the
same history. The command exits non-zero (with a message on stderr) when
`--since` doesn't resolve or the repo has no commits; a hook should treat that
as "nothing to say".

### Example hook

This hook keeps one marker per Claude Code session and, on each prompt, adds
the notes that changed since the previous prompt to the model's context. Save it as
`~/.claude/hooks/memsync-changes.sh` and `chmod +x` it. It needs `jq`; on
Windows run it under Git Bash.

```sh
#!/bin/sh
input=$(cat)
sid=$(printf '%s' "$input" | jq -r '.session_id')
cwd=$(printf '%s' "$input" | jq -r '.cwd')
event=$(printf '%s' "$input" | jq -r '.hook_event_name')
marker="${TMPDIR:-/tmp}/claude-memsync-seen-$sid"

# First call in a session only records the baseline; later calls report
# what changed since the previous call, then move the marker forward.
out=$(claude-memsync changes --since "$marker" --update-marker \
        --project "$cwd" --json 2>/dev/null) || exit 0
printf '%s' "$out" | jq -e '.total > 0' >/dev/null || exit 0

printf '%s' "$out" | jq --arg event "$event" '{
  hookSpecificOutput: {
    hookEventName: $event,
    additionalContext: (
      "Memory notes changed since you last looked (read a file if it matters):\n"
      + ([.changes[] | "- \(.status): \(.local)"] | join("\n"))
      + (if .more > 0 then "\n...\(.more) more" else "" end)
    )
  }
}'
```

Register it in `~/.claude/settings.json` (or a plugin's `hooks/hooks.json`,
which has the same shape):

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "~/.claude/hooks/memsync-changes.sh" } ] }
    ],
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "~/.claude/hooks/memsync-changes.sh" } ] }
    ]
  }
}
```

Notes:

- `UserPromptSubmit` alone is enough for the "long-running session" case.
  Adding `SessionStart` sets the baseline as soon as the session opens, so the
  first prompt reports anything that arrived in between.
- To hear about changes since the **last session** in a project rather than
  since the last prompt, key the marker on the project instead of the session
  (for example `marker=$HOME/.claudesync/.state/seen-$(printf '%s' "$cwd" | tr -c '[:alnum:]' '-')`).
- `--project` expects the project directory. If your sessions often run from a
  subdirectory, `cwd` won't match Claude's folder name for the project; omit
  `--project` to see every project's changes.

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
