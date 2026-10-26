#!/usr/bin/env bash
# Test of scripts/lint-workflows.sh against good and bad fixture workflows, and
# against this repo's own .github/workflows (which must pass with no exemptions).
# Usage: scripts/lint-workflows_test.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
PASS=0 FAIL=0
ok()    { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad()   { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }

SHA=0123456789abcdef0123456789abcdef01234567
D64=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
# fixture <name> <runs-on-lines> <extra-steps-and-triggers...>: one workflow file in its own dir
mk() { # name; stdin = whole file
  mkdir -p "$W/$1"; cat >"$W/$1/w.yml"
}
lint() { set +e; LOUT="$(bash "$ROOT/scripts/lint-workflows.sh" "$W/$1" 2>&1)"; LRC=$?; set -e; }
rejects() { # name pattern
  lint "$1"
  [ "$LRC" -eq 1 ] && printf '%s\n' "$LOUT" | grep -qE -- "$2"
}
accepts() { lint "$1"; [ "$LRC" -eq 0 ]; }

mk good <<YML
name: g
on: [push]
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$SHA # v1
      - uses: ./.github/actions/local
      - uses: docker://alpine@sha256:$D64
  b:
    runs-on: [ubuntu-24.04]
    steps: [{run: echo}]
  c:
    runs-on: \${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest, macos-14, windows-2022]
    steps: [{run: echo}]
  d:
    runs-on: windows-latest
    steps: [{run: echo}]
YML
check "hosted labels, pinned SHAs, local action, digest-pinned docker, hosted matrix: accepted" accepts good

mk self <<'YML'
on: push
jobs:
  a:
    runs-on: self-hosted
    steps: [{run: echo}]
YML
check "runs-on: self-hosted is rejected" rejects self 'runs-on.*self-hosted'
mk selfarr <<'YML'
on: push
jobs:
  a:
    runs-on: [self-hosted, linux]
    steps: [{run: echo}]
YML
check "a label array with self-hosted is rejected" rejects selfarr 'runs-on.*self-hosted'
mk selfblock <<'YML'
on: push
jobs:
  a:
    runs-on:
      - ubuntu-latest
      - self-hosted
    steps: [{run: echo}]
YML
check "a block list with self-hosted is rejected" rejects selfblock 'runs-on.*self-hosted'
mk group <<'YML'
on: push
jobs:
  a:
    runs-on:
      group: my-runners
      labels: ubuntu-latest
    steps: [{run: echo}]
YML
check "a runner group is rejected" rejects group 'runs-on.*group'
mk odd <<'YML'
on: push
jobs:
  a:
    runs-on: my-big-box
    steps: [{run: echo}]
YML
check "an unknown label is rejected" rejects odd 'runs-on.*my-big-box'
mk mxbad <<'YML'
on: push
jobs:
  a:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest, self-hosted]
    steps: [{run: echo}]
YML
check "a matrix with a non-hosted value is rejected" rejects mxbad 'matrix.*self-hosted'
mk mxnone <<'YML'
on: push
jobs:
  a:
    runs-on: ${{ matrix.os }}
    steps: [{run: echo}]
YML
check "a matrix expression with no matrix values is rejected" rejects mxnone 'matrix'
mk expr <<'YML'
on: push
jobs:
  a:
    runs-on: ${{ inputs.runner }}
    steps: [{run: echo}]
YML
check "a non-matrix expression is rejected" rejects expr 'runs-on'
mk prt <<'YML'
on:
  pull_request_target:
    branches: [main]
jobs:
  a:
    runs-on: ubuntu-latest
    steps: [{run: echo}]
YML
check "pull_request_target is rejected" rejects prt 'pull_request_target'
mk prtl <<'YML'
on: [push, pull_request_target]
jobs:
  a:
    runs-on: ubuntu-latest
    steps: [{run: echo}]
YML
check "pull_request_target in an inline list is rejected" rejects prtl 'pull_request_target'
mk tag <<'YML'
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
YML
check "uses pinned to a tag is rejected" rejects tag 'uses.*actions/checkout@v4'
mk short <<'YML'
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@0123456789abcdef
YML
check "uses pinned to a short sha is rejected" rejects short 'uses.*checkout'
mk dock <<'YML'
on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: docker://alpine:3.20
YML
check "docker:// without a sha256 digest is rejected" rejects dock 'docker://alpine:3.20'
mk reuse <<'YML'
on: push
jobs:
  a:
    uses: some-org/repo/.github/workflows/x.yml@main
YML
check "a reusable workflow on a branch is rejected" rejects reuse 'uses.*@main'
mk quoted <<YML
on: push
jobs:
  a:
    runs-on: "ubuntu-latest"
    steps:
      - uses: 'actions/checkout@$SHA'
YML
check "quoted values are understood" accepts quoted
mk comment <<'YML'
on: push
# runs-on: self-hosted   (a comment, not a setting)
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo "uses: actions/checkout@v4 and pull_request_target"
YML
check "comments and run text are not mistaken for settings" accepts comment
mkdir -p "$W/two"; cp "$W/good/w.yml" "$W/two/a.yml"; cp "$W/self/w.yml" "$W/two/b.yaml"
check "every .yml and .yaml in the dir is checked" rejects two 'b.yaml'
check "this repo's .github/workflows pass with no exemptions" bash -c "bash '$ROOT/scripts/lint-workflows.sh' '$ROOT/.github/workflows'"

echo; echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
