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
| `shellcheck` | `scripts/*.sh` with ShellCheck v0.10.0 (pinned by image digest), warning level and up |
| `bootstrap-harness` | `bash scripts/bootstrap_test.sh` against the real binaries (`MEMSYNC_BIN`, `MEMMERGE_BIN`) in a fake HOME with stubbed `gh`, `systemctl` and friends; needs only git, python3 and ssh-keygen, which `ubuntu-latest` has |
| `gitleaks` | `gitleaks git --log-opts=HEAD`: every commit reachable from the tested HEAD, not other fetched refs; reviewed test fixtures are listed in `.gitleaksignore` by fingerprint (commit, file, rule, line), so each entry names a commit that must be an ancestor of the tested HEAD |
| `reference-current` | `sh scripts/gen-reference.sh` then `git diff --exit-code docs/reference.md` |
| `release-layout` | `bash scripts/package-release_test.sh`: runs `scripts/package-release.sh` (the script `release.yml` calls) for linux/amd64 and asserts the archive holds the binaries, `LICENSE`, `README.md` and `scripts/bootstrap.sh`, that `SHA256SUMS` verifies, that the binary reports the release version, and that the extracted `bootstrap.sh --prebuilt-dir <extracted> --dry-run` (fake required flags, throwaway HOME, stub `gh`) gets past argument parsing |
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
