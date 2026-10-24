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
| `claude-memsync init --remote <url> [--denylist <file>]` | One-time bootstrap. `--denylist` installs a [denylist](#denylist) before the first commit. |
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

Your copy under `~/.claude/projects/...` is the authoritative one and is never
changed or deleted because a file is held back. The mirror copy is not left
alone, though. A held-back file would otherwise sit modified in the mirror
work-tree and collide with `git pull --rebase --autostash` when a peer touches
the same file. So each cycle, before pulling, the daemon **parks** every
uncommitted memory file in the mirror: it resets the mirror copy to the last
committed version (or removes it if it was never committed), pulls, skips the
parked files when copying pulled changes back into `~/.claude`, and then
refills the mirror from your copy. In practice:

- A held-back file is briefly absent from, or reverted in, the mirror
  work-tree (`~/.claudesync/projects/...`) during each pull. Don't edit there.
- If the pull brought in a peer's change to a parked file, that change is not
  copied over your version. For `MEMORY.md` it is merged with your copy (the
  same section-level merge as everywhere else); if the merge conflicts, or for
  any other file, the peer's version is kept as `<name>.from-remote-<id>` in
  the mirror, so committing your version later doesn't silently revert it.
- If the peer deleted a parked file or renamed it, your copy stays.
- If the daemon dies mid-cycle your copy is untouched, and the next start
  re-mirrors it.
- Files outside `projects/<hash>/memory/` (such as the `distilled/` catalog)
  have no separate authoritative copy and are not parked.

Built-in rules:

| Rule name | Matches |
|---|---|
| `private-key` | a `-----BEGIN ... PRIVATE KEY-----` header |
| `github-token` | `ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_` tokens and `github_pat_` tokens, also directly after `_` (for example `MY_TOKEN_ghp_...`) |
| `anthropic-api-key` | `sk-ant-...` |
| `openai-api-key` | `sk-...`, `sk-proj-...`, `sk-svcacct-...` and `sk-admin-...` |
| `stripe-key` | `sk_live_...` and `rk_live_...` (live keys only; `sk_test_` is not flagged) |
| `google-api-key` | `AIza` followed by 35 characters |
| `gitlab-token` | `glpat-...` personal access tokens |
| `slack-token` | `xox?-...` tokens |
| `aws-access-key` | `AKIA...` / `ASIA...` access key IDs |
| `aws-secret-key` | `aws_secret_access_key` set to a 40-character value (a value starting with `<` or `$` is treated as a placeholder) |
| `jwt` | three dot-separated base64url segments starting `eyJ` |
| `url-credentials` | `scheme://user:password@host`. Obvious placeholders are skipped: `user:password@`, `:pass@`, `:changeme@`, `<...>`, `${...}` and `{{...}}` |
| `credential-assignment` | `password`, `passwd`, `token`, `secret` or `api_key` (case-insensitive), optionally followed by only `_key`, `_access_key`, `_value`, `_string`, `_str` or `_data`, assigned a quoted value of 16 or more characters without whitespace, with `=` or `:`. So `client_secret: "..."` and `"api_key": "..."` match, but `tokenizer`, `secretName` and `token_type` do not |

Not covered: unquoted assignments, other vendors' keys, and anything without a
recognizable prefix. These rules are a safety net for accidents, not a
guarantee: use the denylist for anything specific to you.

The rules are deliberately conservative about prose, but they will sometimes
flag a harmless note. A false positive costs you one file that doesn't sync
until you reword it.

The daemon logs the **file name and rule name only**, never the matched text:

```
scrub: holding back projects/-home-alice-src-app/memory/notes.md (rule: github-token)
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
acme-corp\.internal
\bproject nightjar\b
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
