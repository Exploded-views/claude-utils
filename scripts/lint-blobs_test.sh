#!/usr/bin/env bash
# Test of scripts/lint-blobs.sh: a blob over 1 MB, or a file under cmd/*/ that is
# not .go (testdata/ allowed), is refused both staged and in the tree; everything
# else passes. Stock macOS tools only.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
PASS=0 FAIL=0
ok()  { PASS=$((PASS + 1)); printf 'PASS    %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf 'FAIL    %s\n' "$1"; }
check() { if "${@:2}"; then ok "$1"; else bad "$1"; fi; }
n=0
newrepo() { # a repo with one clean commit, in $W/$n
  n=$((n + 1)); R="$W/$n"; mkdir -p "$R/cmd/x"
  git -C "$R" init -q
  printf 'package main\n' >"$R/cmd/x/main.go"
  git -C "$R" add -A
  git -C "$R" -c user.name=t -c user.email=t@example.com commit -qm init
}
# stage <path> <bytes>: add a file of that size, staged
stage() { mkdir -p "$(dirname "$R/$1")"; head -c "$2" /dev/zero | tr '\0' 'a' >"$R/$1"; git -C "$R" add -- "$1"; }
refused_staged() { (cd "$R" && ! bash "$ROOT/scripts/lint-blobs.sh" --staged >"$R/out" 2>&1) && grep -q "$1" "$R/out"; }
passes_staged() { (cd "$R" && bash "$ROOT/scripts/lint-blobs.sh" --staged >"$R/out" 2>&1); }
refused_tree() { git -C "$R" -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m more && (cd "$R" && ! bash "$ROOT/scripts/lint-blobs.sh" >"$R/out" 2>&1) && grep -q "$1" "$R/out"; }
passes_tree() { git -C "$R" -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m more && (cd "$R" && bash "$ROOT/scripts/lint-blobs.sh" >"$R/out" 2>&1); }

newrepo; stage docs.bin 1048577
check "staged: a blob over 1 MB is refused" refused_staged "docs.bin"
newrepo; stage docs.bin 1048577
check "tree: a blob over 1 MB is refused" refused_tree "docs.bin"
newrepo; stage docs.bin 1048576
check "a blob of exactly 1 MB passes (staged)" passes_staged
newrepo; stage cmd/x/x 10
check "staged: a file under cmd/x/ that is not .go is refused" refused_staged "cmd/x/x"
newrepo; stage cmd/x/x 10
check "tree: a file under cmd/x/ that is not .go is refused" refused_tree "cmd/x/x"
newrepo; stage cmd/x/sub/tool 10
check "a nested non-.go file under cmd/x/ is refused" refused_staged "cmd/x/sub/tool"
newrepo; stage cmd/x/testdata/fixture.bin 10
check "testdata under cmd/x/ is allowed" passes_staged
newrepo; stage cmd/x/more.go 10
check "a .go file under cmd/x/ passes" passes_staged
newrepo; stage README.md 10
check "a small file outside cmd/ passes" passes_staged
newrepo; stage "cmd/x/has space" 10
check "a name with a space is judged too" refused_staged "has space"
newrepo
check "the tree of a clean repo passes" passes_tree
# commit helper: stage + commit in $R
cm() { git -C "$R" -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m "$1"; }
base() { git -C "$R" rev-parse HEAD; }
refused_range() { (cd "$R" && ! bash "$ROOT/scripts/lint-blobs.sh" --range "$B" >"$R/out" 2>&1) && grep -q "$1" "$R/out"; }
passes_range() { (cd "$R" && bash "$ROOT/scripts/lint-blobs.sh" --range "$B" >"$R/out" 2>&1); }

# A binary committed and deleted again is gone from the tree but not from history.
newrepo; B=$(base); stage docs.bin 1048577; cm add; git -C "$R" rm -q docs.bin; cm del
check "tree mode misses a blob added and deleted in the range (why the range check exists)" passes_tree
check "range: a big blob added and then deleted is refused" refused_range "docs.bin"
newrepo; B=$(base); stage cmd/x/claude-x 10; cm add; git -C "$R" rm -q cmd/x/claude-x; cm del
check "range: a non-.go file under cmd/x/ added and then deleted is refused" refused_range "cmd/x/claude-x"
newrepo; B=$(base); stage "cmd/x/has space" 10; cm add; git -C "$R" rm -q "cmd/x/has space"; cm del
check "range: a name with a space is judged too" refused_range "has space"
newrepo; B=$(base); stage cmd/x/more.go 10; cm add; stage docs.txt 100; cm more
check "range: only small files and .go files pass" passes_range
newrepo; stage docs.bin 1048577; cm old; git -C "$R" rm -q docs.bin; cm "gone before the base"; B=$(base); stage cmd/x/more.go 10; cm add
check "range: a big blob added and deleted before the base is not this range's" passes_range
newrepo; stage docs.bin 1048577; cm old; B=$(base); stage cmd/x/more.go 10; cm add
check "range: a big blob still in the tree at HEAD is refused even from before the base" refused_range "docs.bin"
newrepo; B=$(base)
check "range: an empty range passes" passes_range
newrepo; B=$(base); stage docs.bin 1048576; cm add
check "range: exactly 1 MB passes" passes_range

# Paths are judged with the blob they carry, not only new objects: content that
# is already in history can be added at a new path.
newrepo; stage docs.dat 100; cm "old blob"; B=$(base)
mkdir -p "$R/cmd/x"; cp "$R/docs.dat" "$R/cmd/x/data.bin"; git -C "$R" add -- cmd/x/data.bin; cm "same bytes at a new path"
check "range: an existing blob added at cmd/x/data.bin is refused" refused_range "cmd/x/data.bin"
newrepo; stage docs.dat 100; cm "old blob"; B=$(base)
git -C "$R" mv docs.dat cmd/x/data.bin; cm "rename into cmd/x"
check "range: a rename of an old blob into cmd/x/ is refused" refused_range "cmd/x/data.bin"
newrepo; stage docs.dat 100; cm "old blob"; B=$(base)
mkdir -p "$R/cmd/x"; cp "$R/docs.dat" "$R/cmd/x/has space"; git -C "$R" add -- "cmd/x/has space"; cm "spaced"
check "range: an existing blob at a path with a space is refused" refused_range "has space"
newrepo; stage big.bin 1048577; cm "big old blob"; B=$(base)
git -C "$R" mv big.bin moved.bin; cm "rename a big old blob"
check "range: a big old blob moved to a new path is refused" refused_range "moved.bin"
# A merge commit can add a file that is in neither parent (an evil merge).
newrepo; B=$(base)
git -C "$R" checkout -q -b side; stage side.txt 10; cm side
git -C "$R" checkout -q -; stage main.txt 10; cm main
git -C "$R" -c user.name=t -c user.email=t@example.com merge -q --no-commit --no-ff side >/dev/null 2>&1 || true
stage big.bin 1048577; cm "merge"
git -C "$R" rm -q big.bin; cm "delete it again"
check "range: a big file added by a merge commit and deleted later is refused" refused_range "big.bin"
newrepo; B=$(base); git -C "$R" checkout -q -b side; stage side.txt 10; cm side
git -C "$R" checkout -q -; stage main.txt 10; cm main
git -C "$R" -c user.name=t -c user.email=t@example.com merge -q --no-ff -m merge side >/dev/null 2>&1
check "range: a clean merge passes" passes_range

# FAIL CLOSED: a range that cannot be judged is an error, never a pass.
fails_closed() { # dir base pattern: exit non-zero and say why
  (cd "$1" && ! bash "$ROOT/scripts/lint-blobs.sh" --range "$2" >"$1/out" 2>&1) && grep -qi "$3" "$1/out"
}
newrepo; stage a.txt 10; cm second; stage b.txt 10; cm third
check "range: an unknown base is an error" fails_closed "$R" "0123456789abcdef0123456789abcdef01234567" "not a commit"
newrepo; B=$(base); stage a.txt 10; cm second; stage b.txt 10; cm third; SRC=$R
git clone -q --depth 1 "file://$SRC" "$W/shallow1" 2>/dev/null
check "range: a --depth 1 clone is refused (the history is missing)" fails_closed "$W/shallow1" "$B" "shallow"
git clone -q --depth 2 "file://$SRC" "$W/shallow2" 2>/dev/null
check "range: a base past the --depth 2 boundary is refused" fails_closed "$W/shallow2" "$B" "shallow"
newrepo; git -C "$R" checkout -q -b side; stage s.txt 10; cm side; OTHER=$(base)
git -C "$R" checkout -q -; stage m.txt 10; cm main
check "range: a base that is not an ancestor of HEAD is an error" fails_closed "$R" "$OTHER" "ancestor"
check "range: no base is a usage error" fails_closed "$R" "" "usage"

# A git that fails, or answers nonsense, is an error too (a shim stands in for git).
real_git=$(command -v git)
shim() { # dir body: a git on PATH that runs body for the matching subcommand, else the real one
  mkdir -p "$1"
  printf '#!/bin/sh\n%s\nexec %s "$@"\n' "$2" "$real_git" >"$1/git"
  chmod +x "$1/git"
}
errors_with_shim() { # repo mode-args... ; uses $SHIM
  (cd "$R" && ! PATH="$SHIM:$PATH" bash "$ROOT/scripts/lint-blobs.sh" "$@" >"$R/out" 2>&1) && grep -q "lint-blobs:" "$R/out"
}
newrepo; stage a.txt 10; git -C "$R" add -A; SHIM="$W/shim$n"
shim "$SHIM" 'case "$*" in *"cat-file -s"*) echo notanumber; exit 0;; esac'
check "a size that is not a number is an error" errors_with_shim --staged
newrepo; SHIM="$W/shim$n"
shim "$SHIM" 'case "$*" in *ls-tree*) exit 1;; esac'
check "tree: a failing git ls-tree is an error" errors_with_shim
newrepo; B=$(base); stage a.txt 10; cm second; SHIM="$W/shim$n"
shim "$SHIM" 'case "$*" in *diff-tree*) exit 1;; esac'
check "range: a failing git diff-tree is an error" errors_with_shim --range "$B"
newrepo; stage a.txt 10; SHIM="$W/shim$n"
shim "$SHIM" 'case "$*" in *"diff --cached"*) exit 1;; esac'
check "staged: a failing git diff is an error" errors_with_shim --staged

# A gitlink (submodule) is refused: it is not a blob the size rule can judge.
gitlink() { git -C "$R" update-index --add --cacheinfo "160000,$(git -C "$R" rev-parse HEAD),$1"; }
newrepo; gitlink cmd/x/sub
check "staged: a gitlink is refused" refused_staged "cmd/x/sub"
newrepo; gitlink vendor/lib
check "staged: a gitlink anywhere is refused" refused_staged "vendor/lib"
newrepo; B=$(base); gitlink cmd/x/sub; cm gl
check "range: a gitlink is refused" refused_range "cmd/x/sub"
newrepo; gitlink vendor/lib; cm gl
check "tree: a gitlink is refused" refused_tree "vendor/lib"

# A rename is a change too: staged mode must see it.
newrepo; stage big.bin 1048577; cm "big file already in history"
git -C "$R" mv big.bin docs.bin
check "staged: a rename of a 2 MB file is refused" refused_staged "docs.bin"
newrepo; stage big.bin 10; cm small
git -C "$R" mv big.bin cmd/x/big
check "staged: a rename into cmd/x/ of a non-.go file is refused" refused_staged "cmd/x/big"
check "the repo's own tree passes" bash -c "cd '$ROOT' && bash scripts/lint-blobs.sh"
printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
