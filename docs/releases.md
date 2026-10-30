# Installing from a release

A release gives you the two binaries (and, on Windows, `claude-focus`) without
Go and without a clone. This page is for the person installing one. How a
release is cut is in [Cutting a release](#cutting-a-release); what CI checks about the
archives is in [ci.md](ci.md#release-packaging-fork-only). The full host setup
that uses a release is in [setup.md](setup.md) and [bootstrap.md](bootstrap.md).

## Published releases

| Tag | Commit | Notes |
|---|---|---|
| [v0.5.0-ev.1](https://github.com/Exploded-views/claude-utils/releases/tag/v0.5.0-ev.1) | `cc093f6` | Pre-release: a faster routed daemon and changes hook with a performance budget enforced by tests, budget archives to run that budget against the shipped binaries, git maintenance kept inside deadlines, and privateOnly repos held unless the daemon can verify them. After upgrading, some folders under very large directories show as "ambiguous, not synced" in `status` (pin them with a `folder` rule), and a `privateOnly` repo needs a token source that gives a token (see the changelog). |
| [v0.4.0-ev.1](https://github.com/Exploded-views/claude-utils/releases/tag/v0.4.0-ev.1) | `80543d3` | Pre-release: routing for hosts that reach GitHub over port 443, `join --all`, `identity backup`, selectable host bootstrap, autostart and install re-run fixes. After upgrading, rewrite any `routes.json` rule or alias that uses `ssh.github.com/…` as `github.com/…`; on Linux re-run `claude-memsync install` or `start`; on macOS re-install the launchd agent (see the changelog). |
| [v0.3.0-ev.1](https://github.com/Exploded-views/claude-utils/releases/tag/v0.3.0-ev.1) | `37fa564` | First release of the fork (pre-release): post-quantum encryption by default, one config directory, host bootstrap with `--prebuilt-dir`, fork CI. |

## What a release holds

One archive per platform, named `claude-utils-<tag>-<os>-<arch>`:

| `<os>` | `<arch>` | Archive |
|---|---|---|
| `linux` | `amd64`, `arm64` | `claude-utils-<tag>-linux-<arch>.tar.gz` |
| `darwin` | `amd64`, `arm64` | `claude-utils-<tag>-darwin-<arch>.tar.gz` |
| `windows` | `amd64`, `arm64` | `claude-utils-<tag>-windows-<arch>.zip` |

and, for each linux and darwin platform, a budget archive
`claude-utils-<tag>-budget-<os>-<arch>.tar.gz` (see [The budget bench](#the-budget-bench)),

plus one `SHA256SUMS` file that lists every archive, product and budget (`<hash>  <file>`, the
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

## The budget bench

`claude-utils-<tag>-budget-<os>-<arch>.tar.gz` (linux and darwin, amd64 and
arm64; none for Windows) unpacks into a folder of the same name with
`claude-memsync-budget.test`, `route-budget.test` and a `README` with the run
commands. They are `go test -c -tags budget` binaries of `./cmd/claude-memsync`
and `./internal/route`, built with the same `CGO_ENABLED=0 -trimpath` as the
product, so the gates B1-B7 can run against the shipped binaries on a host with
no Go and no checkout. Verify both archives against `SHA256SUMS`, unpack both,
then from any directory:

```sh
MEMSYNC_BUDGET_BIN_DIR=<product dir> MEMSYNC_BUDGET_TAG=<tag> ./claude-memsync-budget.test -test.run '^TestBudgetB' -test.v -test.timeout 900s
MEMSYNC_BUDGET_BIN_DIR=<product dir> MEMSYNC_BUDGET_TAG=<tag> ./route-budget.test -test.run '^TestBudgetB' -test.v -test.timeout 900s
```

Which bytes each gate measures: B1, B4, B7 and B8 run the code inside the
budget test binaries (same source and flags as the product, in-process), and
B2/B3, B5 and B6 run the product archive's `claude-memsync`. Both are checked
against the tag, so a budget archive from another release beside this product
archive fails before any gate. Set both variables or neither (one alone fails);
the tag must be a release tag (the regex below), never `dev`. Both product binaries must
print `claude-memsync <tag>` / `claude-memmerge <tag>` from `version`, or the run
fails before any gate, and so must the test binary's own version. The test binary then moves `HOME`, the `XDG_*`
directories and `TMPDIR` into a fresh temp directory, points `GIT_CONFIG_GLOBAL` at a file there, sets `GIT_CONFIG_NOSYSTEM=1` and a failing `GIT_SSH_COMMAND`, drops inherited `GIT_*`, `CLAUDE_PROJECT_DIR` and `SSH_AUTH_SOCK`, and refuses to run if `HOME` was unset or any
path it would use resolves under the original `HOME`. B9 skips in this mode (it
needs a source build and is not part of B1-B7). Read the `--- PASS/FAIL/SKIP`
lines and the load-average lines; an INVALID skip means the host was too busy.

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

Only the owner tags or publishes a Release, and this repo's docs do not name
the next version. The fork's
releases are cut from `integ/next`, so pass that branch as the target.

Versions follow [SemVer](https://semver.org), `vMAJOR.MINOR.PATCH`, with an
`-ev.N` suffix for the fork's own releases. While on `0.x`:

| Change | Bump |
|---|---|
| Bug fix, internal refactor, doc update | patch |
| Behavior change, new feature, default change, new config field | minor |
| Breaking change to config format, on-disk layout or CLI surface | minor while pre-1.0, called out in the tag message |

After 1.0 the usual SemVer rules apply, and a `v2+` needs a `/v2` suffix in the
Go module path.

1. Land all changes on the release branch and confirm CI is green.
2. Create a GitHub Release for the version. Either:

   ```sh
   gh release create <tag> \
     --title "<tag>" \
     --notes "- <bullet summary of user-visible changes>" \
     --target <branch>
   ```

   or use the GitHub UI (Releases, Draft a new release, choose the tag
   and the target branch, fill in title and notes, **Publish**).

3. Publishing the Release fires `.github/workflows/release.yml`, which runs
   `scripts/package-release.sh`: it cross-compiles both binaries for
   {linux, darwin, windows} x {amd64, arm64} (plus `claude-focus.exe` for
   Windows), stamps the version in via `-ldflags`, packages each target as
   `claude-utils-<tag>-<os>-<arch>.{tar.gz,zip}`, generates `SHA256SUMS`, and
   uploads everything to the Release.

4. Watch the run finish:

   ```sh
   gh run watch
   gh release view <tag>      # confirms the assets are attached
   ```

5. Upgrade your own machines as in [Upgrade an installed host by
   hand](#upgrade-an-installed-host-by-hand), verifying the archive first.

### If the release workflow didn't fire

GitHub's `release: published` event is occasionally missed, usually when the
Release was created from a pre-existing tag via the API. Two recovery paths:

- **Manual dispatch** (preferred): trigger the workflow against the existing
  tag without touching the Release.

  ```sh
  gh workflow run release.yml --field tag=<tag>
  gh run watch
  ```

  It runs the same build and upload steps against the existing Release.

- **Recreate the Release**: `gh release delete <tag> --cleanup-tag=false`,
  then create it again, which can make GitHub fire the event. Use this only if
  you also need to fix the release notes.

Manual dispatch also builds archives for a tag that has none yet, or after a
failed run.

The upload never overwrites an asset that is already attached, so a published
archive and its `SHA256SUMS` never change under a tag. If a failed run left
some assets behind, delete them first (`gh release delete-asset <tag> <name>`),
then dispatch again.

### Building locally without a release

For dev iteration, a plain `go build -o bin/ ./cmd/...` is enough. The binaries
report a VCS-derived version (a pseudo-version, or `dev+<sha>` on older
toolchains). To stamp a specific version into a local build:

```sh
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin/ ./cmd/...
```

PowerShell equivalent:

```powershell
$VERSION = git describe --tags --always --dirty
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin\ .\cmd\...
```

The module path stays `github.com/MarimerLLC/claude-utils` in this fork.

### How version resolution works

`internal/version.String()` returns the first of:

1. The build-time `Override` set via `-ldflags "-X .../internal/version.Override=<v>"` (preferred for releases).
2. `runtime/debug.BuildInfo.Main.Version`: populated automatically when installed via `go install <path>@<tag>`, and by Go 1.24 or later for a plain `go build` inside a git checkout. Yields `v0.1.7` for tagged installs, or a pseudo-version like `v0.2.1-0.20260508213755-a52d68630b31` (with `+dirty` appended for uncommitted changes) for builds past a tag or of untagged commits.
3. `vcs.revision` from the embedded build settings, when the toolchain gives no main-module version: yields `dev+<short-sha>` (or `dev+<short-sha>-dirty` if the working tree had uncommitted changes at build time).
4. The literal `dev`: only seen if Go embedded no VCS info (for example when building from outside a git checkout).

So a plain `go build` gives a useful `version` with no build script.
