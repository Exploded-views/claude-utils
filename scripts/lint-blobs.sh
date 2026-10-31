#!/usr/bin/env bash
# Refuse a blob over 1 MB, a gitlink (submodule), and any file under cmd/*/ that
# is not a .go file (cmd/*/testdata/ is allowed): a built binary must never be
# committed.
#
#   lint-blobs.sh --range BASE   every path/blob pair added or changed by a commit
#                                 in BASE..HEAD (deleted files included), plus the
#                                 tree at HEAD (CI: the guarantee)
#   lint-blobs.sh --staged       the files staged for commit (the pre-commit
#                                 hook: a convenience, it can be skipped)
#   lint-blobs.sh                the tracked tree at HEAD
#
# The CI run over the whole range is the guarantee; the hook only saves a round
# trip (it is off without core.hooksPath and --no-verify skips it). A binary
# that was committed and deleted again is gone from the tree but not from
# history, so only the range check sees it.
#
# It fails closed: a range it cannot judge (no such base, a base that is not an
# ancestor of HEAD, a shallow repository, a git command that fails, a size that
# is not a number) is an error (exit 2), never a pass. A finding exits 1.
set -euo pipefail
MAX=1048576
bad=0
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
fail() {
	echo "lint-blobs: $*" >&2
	exit 2
}
judge() { # path size
	case $2 in '' | *[!0-9]*) fail "the size of $1 is not a number: $2" ;; esac
	if [ "$2" -gt "$MAX" ]; then
		echo "$1: $2 bytes, over 1 MB"
		bad=1
	fi
	case $1 in
	cmd/*/testdata/*) ;;
	cmd/*/*.go) ;;
	cmd/*/*)
		echo "$1: not a .go file under cmd/*/ (a built binary?)"
		bad=1
		;;
	esac
}
entry() { # path mode blob-sha
	if [ "$2" = 160000 ]; then
		echo "$1: a gitlink (submodule) is not allowed"
		bad=1
		return
	fi
	local size
	size=$(git cat-file -s "$3") || fail "cannot read the size of $1 ($3)"
	judge "$1" "$size"
}
# raw_file FILE: a `git diff --raw -z` / `git diff-tree -z` output (": old new oldsha newsha status NUL path NUL").
raw_file() {
	local meta path m
	while IFS= read -r -d '' meta; do
		IFS= read -r -d '' path || fail "truncated git output"
		IFS=' ' read -r -a m <<<"$meta"
		[ "${#m[@]}" -ge 5 ] || fail "unexpected git output: $meta"
		entry "$path" "${m[1]}" "${m[3]}"
	done <"$1"
}
# tree_file FILE: a `git ls-tree -r -z` output ("mode type sha TAB path NUL").
tree_file() {
	local rec meta path m
	while IFS= read -r -d '' rec; do
		meta=${rec%%$'\t'*}
		path=${rec#*$'\t'}
		IFS=' ' read -r -a m <<<"$meta"
		[ "${#m[@]}" -ge 3 ] || fail "unexpected git output: $rec"
		entry "$path" "${m[0]}" "${m[2]}"
	done <"$1"
}
tree_at_head() {
	git ls-tree -r -z HEAD >"$tmp" || fail "git ls-tree failed"
	tree_file "$tmp"
}
case "${1:-}" in
--range)
	base=${2:-}
	[ -n "$base" ] || { echo "usage: lint-blobs.sh --range BASE" >&2; exit 2; }
	shallow=$(git rev-parse --is-shallow-repository) || fail "git rev-parse failed"
	[ "$shallow" = false ] || fail "this is a shallow repository: the history of $base..HEAD is missing (fetch it all, for example fetch-depth: 0)"
	git cat-file -e "$base^{commit}" 2>/dev/null || fail "base $base is not a commit here"
	git merge-base --is-ancestor "$base" HEAD || fail "base $base is not an ancestor of HEAD"
	commits=$(git rev-list "$base..HEAD") || fail "git rev-list failed"
	# Every (path, blob) pair a commit adds or changes, so content already in
	# history cannot be added at a new path unjudged; merge commits are diffed
	# against each parent (-m), renames are an add (no rename detection).
	for commit in $commits; do
		git diff-tree -r -z -m --no-commit-id --no-renames --diff-filter=ACMRT --root "$commit" >"$tmp" || fail "git diff-tree failed for $commit"
		raw_file "$tmp"
	done
	# and the whole tree at HEAD, which catches anything the walk misses
	tree_at_head
	;;
--staged)
	git diff --cached --raw -z --no-renames --diff-filter=ACMRT >"$tmp" || fail "git diff --cached failed"
	raw_file "$tmp"
	;;
*)
	tree_at_head
	;;
esac
exit "$bad"
