# Installing from a release

A release gives you the two binaries (and, on Windows, `claude-focus`) without
Go and without a clone. This page is for the person installing one. How a
release is cut is in the README's "Releasing" section; what CI checks about the
archives is in [ci.md](ci.md#release-packaging-fork-only). The full host setup
that uses a release is in [setup.md](setup.md) and [bootstrap.md](bootstrap.md).

## What a release holds

One archive per platform, named `claude-utils-<tag>-<os>-<arch>`:

| `<os>` | `<arch>` | Archive |
|---|---|---|
| `linux` | `amd64`, `arm64` | `claude-utils-<tag>-linux-<arch>.tar.gz` |
| `darwin` | `amd64`, `arm64` | `claude-utils-<tag>-darwin-<arch>.tar.gz` |
| `windows` | `amd64`, `arm64` | `claude-utils-<tag>-windows-<arch>.zip` |

plus one `SHA256SUMS` file that lists every archive (`<hash>  <file>`, the
format `sha256sum -c` reads).

Each archive unpacks into a folder of the same name that holds:

- `claude-memsync` and `claude-memmerge` (`.exe` on Windows; they must stay in
  the same directory),
- `claude-focus.exe` (Windows archives only),
- `LICENSE` and `README.md`,
- `scripts/bootstrap.sh`, so a downloaded and verified archive can set up a host
  without cloning anything.

The binaries are built with `CGO_ENABLED=0 -trimpath`, so they run on a plain
system with no libraries to install.

## The version it reports

The release workflow builds with
`-X github.com/MarimerLLC/claude-utils/internal/version.Override=<tag>`, so
`claude-memsync version` and `claude-memmerge version` print exactly the tag,
for example `claude-memsync v0.3.0` for a tag `v0.3.0`. The tag must look like
`vX.Y.Z`, optionally with a suffix after `.` or `-` (`v0.3.0-ev.1`): the regex
`^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$`, the same one
`scripts/package-release.sh`, the workflow's tag check and `bootstrap.sh
--prebuilt-dir` use, so no tag can be built that bootstrap would then refuse.

A build from source without that flag reports a Go-derived version (a
pseudo-version such as `v0.2.1-0.<time>-<commit>`, or `dev+<commit>`), never a
tag. `bootstrap.sh --prebuilt-dir` uses this: with no `--sha` it accepts only a
`vX.Y.Z` version, and with `--sha <commit>` it accepts a binary whose version
contains the first 12 characters of that commit.

## Install on a Linux or macOS host

The steps use the shell variables below, so each value is written once. Run
them in bash. `bootstrap.sh` is bash 3.2 safe, so macOS's own `/bin/bash`
works. It is a Linux and macOS script (it registers a systemd or launchd
service); on Windows install the two executables by hand as shown at the end.

INPUTS (fill in; nothing else changes)

```sh
TAG=<tag>                       # the release tag, vX.Y.Z (the owner picks it)
PLATFORM=<os-arch>              # linux-amd64, linux-arm64, darwin-amd64 or darwin-arm64
HOST_LABEL=<host-label>         # this host's label: 1-32 of a-z, 0-9, -
ORG=<org>                       # the GitHub organisation that holds the memory repos
RECOVERY=<recovery-recipient>   # your offline post-quantum recovery PUBLIC key, age1pq1...
TOKEN_FILE=<token-file>         # absolute path of the org admin token file (see bootstrap.md, token file)
CHECK_APP_ID=<check-app-id>     # numeric App ID of memsync-check
CHECK_KEY_FILE=<check-key-file> # absolute path of this host's memsync-check key, mode 0600
```

ALREADY SET (derived; no action)

```sh
REPO=https://github.com/Exploded-views/claude-utils
NAME=claude-utils-$TAG-$PLATFORM
WORK=~/.local/src/claude-utils-release-$TAG
X=$WORK/$NAME
```

Optional keep-out owners and terms are not set here; add
`--keep-out-owner <owner>` and `--keep-out-term <term>` to `boot` if you want
them (see [bootstrap.md](bootstrap.md#keep-out-lists-optional-repeatable)).
Left out, the script prints `keep-out lists: none` and goes on.

STEPS

1. Download the archive and the checksum list.

   ```sh
   mkdir -p "$WORK" && cd "$WORK" \
     && curl -fLO "$REPO/releases/download/$TAG/$NAME.tar.gz" \
          -O "$REPO/releases/download/$TAG/SHA256SUMS"
   ```

2. Verify before extracting. It must print `OK` for the archive.

   ```sh
   sha256sum -c --ignore-missing SHA256SUMS
   # macOS, which has no sha256sum:
   grep " $NAME.tar.gz\$" SHA256SUMS | shasum -a 256 -c -
   ```

   Do not extract an archive that does not verify. `SHA256SUMS` comes from the
   same release page, so it proves the download is intact, not who published it.

3. Extract it and read the script before running it.

   ```sh
   tar -xzf "$NAME.tar.gz" && less "$X/scripts/bootstrap.sh"
   ```

4. Define the command once.

   ```sh
   boot() {
     bash "$X/scripts/bootstrap.sh" --prebuilt-dir "$X" --host "$HOST_LABEL" --role homelab \
       --org "$ORG" --recovery-recipient "$RECOVERY" \
       --org-token-file "$TOKEN_FILE" --delete-org-token-file \
       --check-app memsync-check --check-app-id "$CHECK_APP_ID" --check-key-file "$CHECK_KEY_FILE" "$@"
   }
   ```

5. Dry run, and read the plan. It changes nothing and keeps the token file.

   ```sh
   boot --dry-run
   ```

6. Wait for your go. Then run the same command for real; it deletes the token
   file when it finishes, however it ends.

   ```sh
   boot --yes
   ```

`--prebuilt-dir` installs the two binaries from the folder into
`~/.local/lib/claude-memsync/<tag>/` and links them into `~/.local/bin`; it
clones nothing and builds nothing, so Go is not needed. A regular file already
at `~/.local/bin/claude-memsync` is moved aside, not overwritten (see
[setup.md](setup.md#macos-notes)). With no source checkout the script skips the
plugin step and its summary says how to add it later. Exit code 3 means it
paused for a missing check key; do what it prints and rerun. The token file
flow, the exit codes and the other roles are in [bootstrap.md](bootstrap.md).
The same flow is available as a paste prompt, "Crewkit host", in that file.

## Upgrade an installed host by hand

Without bootstrap: verify and extract as above, then

```sh
claude-memsync stop
cp "$X/claude-memsync" "$X/claude-memmerge" ~/.local/bin/    # both, into the same directory
claude-memsync start
claude-memsync version && claude-memsync status
```

On start the daemon repoints each encrypted clone's filter and merge driver at
the running binary, so no `init --force` is needed. Upgrade every machine; a
host whose binary predates a repo's encryption scheme cannot read it.

On Windows, extract the `.zip` and put `claude-memsync.exe` and
`claude-memmerge.exe` in one folder. Bring `claude-focus.exe` along only if you
want the Copilot-key helper ([claude-focus.md](claude-focus.md)).

## Cutting a release

Tagging is the owner's decision: nobody tags or publishes a Release without the
owner's say-so, and this repo's docs do not name the next version. The steps
(create the Release, watch the workflow, recover a missed run) are in the
README's "Releasing" section. Published assets are never overwritten; to redo a
failed upload, delete the leftover assets first and dispatch the workflow again.
