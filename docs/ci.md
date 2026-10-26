# Fork CI and the coverage ratchet

`.github/workflows/fork-ci.yml` is **fork-only**. It is not part of upstream
`claude-utils` and is never proposed there. Every job carries
`if: github.repository_owner != 'MarimerLLC'`, so if the file were merged
upstream it would still not run there. Upstream's `ci.yml` and `release.yml` are
untouched. Enable Actions on the fork for it to run.

It runs on push for `main`, `integ/**` and `feat/**`, and on manual dispatch. It
also runs on pull requests into those branches, but only when the PR's head is a
fork of this repository: a PR from a branch in this repo is already covered by the
push run, so it is skipped to avoid running everything twice.

Upstream's `ci.yml` is still in the tree and also runs on the fork for `main`
(push and pull request), alongside this workflow.

Go is pinned to `1.27.2` (the dev container's toolchain), newer than the 1.25.0
floor in `go.mod`, so CI matches what a local run sees. Bump the workflow and the
dev container together.

## What runs

| Job | What it checks |
| --- | --- |
| `vet` | `go vet ./...` for linux and `GOOS=windows` |
| `gofmt` | `gofmt -l .` prints nothing |
| `test` | `go test -race -coverprofile=cover.out ./...`, then the coverage floor check |
| `shellcheck` | `scripts/*.sh` with ShellCheck v0.10.0 (pinned by image digest), warning level and up; then `scripts/lint-portability.sh` (and its test): the scripts must run on stock macOS tools, so no regex repeat bound above 255 (BSD `grep` and bash `[[ =~ ]]` stop there), no `sed -i` without a suffix argument, `grep -P`, `\s`/`\w`/`\b` in `grep`/`sed`, `readlink -f`, `xargs -r`, `find -printf`/`-regex`, `stat -c` without a `stat -f` fallback, `date -d`, `base64 -w`, `mktemp -p`; `portability-ok` on a line opts it out; then `scripts/lint-no-delete.sh` and `scripts/lint-guards.sh` (each with its `_test.sh`): the setup scripts never remove a directory tree, discard git work in place or truncate a config path (except a `mktemp -d` directory they made), and every safety decision is a registered guard (see below) |
| `job-runner` (ubuntu-latest and macos-latest) | `scripts/jobrunner_test.sh` and `scripts/guard_fault_test.sh` under the system bash (3.2 on macOS): the job runner behind `bootstrap.sh --jobs` (order, no new job after a failure, INT/TERM/HUP clean-up of jobs and their children, a second signal, a job reading the tty), and fault injection into every guard |
| `bootstrap-harness` | `bash scripts/bootstrap_test.sh` against the real binaries (`MEMSYNC_BIN`, `MEMMERGE_BIN`) in a fake HOME with stubbed `gh`, `systemctl` and friends; needs only git, python3 and ssh-keygen, which `ubuntu-latest` has |
| `gitleaks` | `gitleaks git --log-opts=HEAD`: every commit reachable from the tested HEAD, not other fetched refs; reviewed test fixtures are listed in `.gitleaksignore` by fingerprint (commit, file, rule, line), so each entry names a commit that must be an ancestor of the tested HEAD |
| `reference-current` | `sh scripts/gen-reference.sh` then `git diff --exit-code docs/reference.md` |
| `release-layout` | `bash scripts/package-release_test.sh`: runs `scripts/package-release.sh` (the script `release.yml` calls) for linux/amd64 and asserts the archive holds the binaries, `LICENSE`, `README.md` and `scripts/bootstrap.sh`, that `SHA256SUMS` verifies, that the binary reports the release version, and that the extracted `bootstrap.sh --prebuilt-dir <extracted> --dry-run` (throwaway HOME, stub `gh`, fake required flags except the recovery key, which is a real-length post-quantum key made by the extracted binary's own `init`) gets past argument parsing and the prerequisite checks, on `ubuntu-latest` (linux/amd64) and `macos-latest` (darwin/arm64, under the system `/bin/bash` 3.2) |
| `workflow-lint` | `scripts/lint-workflows.sh` on `.github/workflows` (see below), after its own test, `scripts/lint-workflows_test.sh`, which feeds it bad fixture workflows |

Permissions are least privilege: the workflow sets `permissions: {}` and each job
asks for `contents: read` only. Checkouts use `persist-credentials: false`. A new
push to the same ref cancels the older run. Every job has a timeout.

gitleaks runs as a binary pinned by version and sha256 rather than through
`gitleaks-action`, which needs a licence for organisations.

## Release packaging (fork-only)

`release.yml` is changed in this fork only (a comment at its top says so): the packaging loop lives in `scripts/package-release.sh <version> <outdir> [os/arch ...]`, which `release.yml` calls. Every archive `claude-utils-<tag>-<os>-<arch>` now also contains `scripts/bootstrap.sh`, so `SHA256SUMS` covers it, and a host can bootstrap from a verified download with `--prebuilt-dir` (see `docs/bootstrap.md`, "Crewkit host"). The `release-layout` job and `scripts/package-release_test.sh` (runnable locally; `GO="sh /path/to/dev.sh go"` wraps Go) test the same script, so the release workflow's packaging is exercised on every push.

## Workflow lint

The house rule is GitHub-hosted runners only and pinned actions. `scripts/lint-workflows.sh [dir]` (bash and python3 standard library only, no YAML library, self-contained so a shared version can replace it) fails, one line per finding, on any `*.yml` or `*.yaml` under `.github/workflows` that has:

- a `runs-on` that is not a GitHub-hosted label: `ubuntu-*`, `windows-*` and `macos-*` (including `-latest` and versioned labels) are allowed, alone, in a list, or as `${{ matrix.<key> }}` whose matrix values are all hosted labels; `self-hosted`, runner groups and anything else are rejected;
- a `pull_request_target` trigger;
- a `uses:` that is not pinned to a full 40-hex commit SHA (`./` local actions are allowed; `docker://` must carry an `@sha256:` digest).

Upstream's `ci.yml` and `release.yml` and this fork's `fork-ci.yml` pass without exemptions. The `workflow-lint` job runs it.

## The coverage ratchet

`tools/coverfloors` is a small Go program (no dependencies) that reads a
`go test -coverprofile` file and compares each package's statement coverage with
`coverage-floors.json`, a map of import path to percent with one decimal.

- `check` fails (exit 1) when a package is more than 0.1 point below its floor, or
  has statements but no floor ("new packages need a floor"). The 0.1 slack absorbs
  rounding and tiny run-to-run differences. Packages with no statements are
  skipped.
- A floor whose package is absent from the profile is reported `not measured` and
  also fails `check`. An empty profile (no blocks) is an input error, exit 2, and
  so is a floors file with a value below 0, above 100 or with more than one
  decimal. The table shows current coverage truncated to tenths, as the ratchet
  computes it.
- `ratchet` raises each floor to the current coverage rounded **down** to 0.1. It
  never lowers a floor and adds new packages. `ratchet -prune` also drops floors for
  packages that are not in the profile (a deleted or renamed package); without it
  they are kept.

Floors were measured with `go test -race`, the way CI runs. A plain run gave the
same floors to 0.1 for every package, so the two agree today; if they ever drift,
the race run is the reference.

### Raising floors

```sh
make ratchet          # tests with coverage, then raise floors
git diff coverage-floors.json
git add coverage-floors.json && git commit -m "ci: raise coverage floors"
```

`make cover-check` runs the check against an existing `cover.out`. If Go runs in a
wrapper, pass it: `make ratchet GO="sh /path/to/dev.sh go"`.

On push, the `test` job also runs the ratchet into a scratch file and, if floors
could rise, uploads it as the `coverage-floors` artifact (kept 14 days) with a
notice in the run. It does **not** open a PR: that would need
`contents: write` and `pull-requests: write` plus the repository setting "Allow
GitHub Actions to create and approve pull requests", which widens the token for a
convenience. Download the artifact and commit it, or run `make ratchet`.

### Adding a package

A new package with statements fails `check` until it has a floor. Run
`make ratchet` and commit the new line in `coverage-floors.json`. Lowering a
floor is a deliberate hand edit in a reviewed change, never done by the tool.

## Performance budgets (planned)

> **Planned, not yet running.** There is no performance job in `fork-ci.yml`
> today. The plan, tracked in
> [issue #5](https://github.com/Exploded-views/claude-utils/issues/5), is a
> `perf-budgets.json` that CI ratchets the way it ratchets coverage floors, with
> budgets for the per-prompt `changes --hook` (warm and cold), `status`,
> `changes`, an idle sync cycle per repo and the age filter per note, after the
> daemon writes a small changes index so that the hook spawns no git. Until it
> lands, only the 1.5 s self-deadline of `changes --hook` bounds that path.
> Likewise, the `internal/sync` coverage floor can flake under host load until
> [issue #6](https://github.com/Exploded-views/claude-utils/issues/6) makes its
> tests deterministic.

## Re-pinning actions

Actions are pinned by full commit SHA with the version in a comment. To update:

```sh
gh api repos/actions/checkout/git/ref/tags/<tag> --jq '.object.type + " " + .object.sha'
```

If the type is `tag` (annotated), dereference it with
`gh api repos/actions/checkout/git/tags/<sha> --jq .object.sha`. Put that commit
SHA and the tag in the `uses:` line, for every use. Do the same for the other
actions. For gitleaks, change `GITLEAKS_VERSION` and `GITLEAKS_SHA256` (the
release's `gitleaks_<version>_linux_x64.tar.gz` digest). For ShellCheck, change
the image digest. Then lint the workflow:

```sh
docker run --rm -v "$PWD:/repo" -w /repo rhysd/actionlint:1.7.12 -color
```

## Code owners

`.github/CODEOWNERS` is fork-only and advisory: it lists who looks at the crypto,
app-auth, bootstrap and CI paths. No branch rule enforces it, so it does not block
a merge.

## Guards: a check that cannot tell must refuse

Every "may we proceed?" decision in the setup scripts (is this sync dir safe to move, is this repo private, is this token file ours, is the host key GitHub's, is a predecessor still running, ...) is a **guard**. The convention, enforced by `scripts/lint-guards.sh`:

- A guard is a function named `<name>_check`. It prints `SAFE:<name>` as its last stdout line only on an explicitly safe outcome (and `REFUSE:<reason>` otherwise), and it never reads safety from a bare `$?`, `if <cmd>; then`, `<cmd> && ...` or `|| true`. It captures a tool's output with `x=$(cmd) || refuse` and compares it with what a working tool must say; where "nothing found" is the safe answer it first asks a control question a working tool answers affirmatively. A Python probe wraps its whole body in `try/except` and prints no token on an exception.
- It is run through `guard <name> -- <name>_check args...` (fatal) or `guard_ok` (returns), which counts it safe only if the exit status is 0 **and** the last line is exactly `SAFE:<name>`. Any other exit status, a missing or wrong token, extra output after it, empty output, a missing tool (127), a signal or a timeout (`BOOTSTRAP_GUARD_TIMEOUT`, default 30 s; polled in the background, no GNU `timeout` needed) is a refusal.
- The guards live between `# >>> guards` and `# <<< guards` in `scripts/bootstrap.sh` (the runner between `# >>> guard-helper` and `# <<< guard-helper`, Python probe sources between `# >>> guard-py` and `# <<< guard-py`) and are listed in `scripts/guards.registry`.
- `scripts/guard_fault_test.sh` has a `fixture_<name>` for each registered guard and injects, per guard: the real safe path (SAFE), ten faults in place of the guard (exit 1, exit 127, a traceback, garbage, empty output, the right token with a wrong exit code, the right exit code without the token, the token followed by more output, another guard's token, a hang) and, for each tool the guard runs, six faults in that tool (exit 1, exit 127, a traceback, garbage, nothing, a hang). Every fault must refuse. The lint fails if a guard is called but not registered, or registered without a function, a call or a fixture.
