#!/usr/bin/env bash
# run-budget.sh [go test args...]: run the budget suite (-tags budget) in the
# linux/arm64 CI image, with the checkout and the host's module cache mounted
# read-only (nothing is downloaded). Wrap it in heavy.sh to hold the lock.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
img=claude-utils-ci:local
docker build -q --platform linux/arm64 -f "$root/tools/docker/Dockerfile.ci" -t "$img" "$root/tools/docker" >/dev/null
[ $# -gt 0 ] || set -- ./cmd/claude-memsync ./internal/route -run Budget -v -count=1 -timeout 600s
mod=$(go env GOMODCACHE)
exec docker run --rm --platform linux/arm64 \
	-v "$root":/src:ro -v "$mod":/go/pkg/mod:ro \
	-e GOFLAGS=-mod=readonly -e GOPROXY=off -e GOCACHE=/tmp/gocache -e TMPDIR=/tmp/t \
	"$img" sh -c 'mkdir -p /tmp/t && exec go test -tags budget "$@"' sh "$@"
