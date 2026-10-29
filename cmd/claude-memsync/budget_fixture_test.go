//go:build budget

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/route"
)

// budgetTree is the owner's Mac in miniature: a $TMPDIR-shaped directory with
// many entries, about 600 project folders naming deleted directories under it,
// 300 real checkouts, and 100 other folders, for five repos.
type budgetTree struct {
	tmp     string   // the big $TMPDIR-shaped dir
	missing []string // folder names of deleted dirs under tmp
	real    []string // folder names of real checkouts under tmp
	other   []string // folder names of dirs outside tmp
}

// newBudgetTree builds the tree under root with nFiller entries in tmp. A
// checkout is a minimal .git (HEAD, config with an origin, empty objects and
// refs) written directly: git reads it, and 300 `git init`s would dominate the
// set-up time.
func newBudgetTree(tb testing.TB, root string, nFiller int) *budgetTree {
	tb.Helper()
	b := &budgetTree{tmp: filepath.Join(root, "private", "var", "folders", "ab", "cdef", "T")}
	if err := os.MkdirAll(b.tmp, 0o700); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < nFiller; i++ {
		if err := os.WriteFile(filepath.Join(b.tmp, fmt.Sprintf("tmp.%06d", i)), nil, 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	for i := 0; i < 600; i++ {
		b.missing = append(b.missing, route.Escape(filepath.Join(b.tmp, fmt.Sprintf("gone%03d", i), "sub")))
	}
	for i := 0; i < 300; i++ {
		dir := filepath.Join(b.tmp, fmt.Sprintf("checkout%03d", i))
		writeMiniRepo(tb, dir, fmt.Sprintf("git@github.com:me/r%03d.git", i))
		b.real = append(b.real, route.Escape(dir))
	}
	for i := 0; i < 100; i++ {
		dir := filepath.Join(root, "other", fmt.Sprintf("o%03d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			tb.Fatal(err)
		}
		b.other = append(b.other, route.Escape(dir))
	}
	return b
}

func writeMiniRepo(tb testing.TB, dir, origin string) {
	tb.Helper()
	g := filepath.Join(dir, ".git")
	for _, d := range []string{"objects", "refs/heads"} {
		if err := os.MkdirAll(filepath.Join(g, d), 0o700); err != nil {
			tb.Fatal(err)
		}
	}
	files := map[string]string{
		"HEAD":   "ref: refs/heads/main\n",
		"config": "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = " + origin + "\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(g, n), []byte(c), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
}

// budgetRoutes is the routing table of the fixture: a path rule that places
// nothing (so every folder needs its directory), then remote rules for five
// repos.
func budgetRoutes() []map[string]string {
	return []map[string]string{
		{"path": "/nowhere-at-all/**", "repo": "projects"},
		{"remote": "github.com/me/r0*", "repo": "projects"},
		{"remote": "github.com/me/r1*", "repo": "client-a"},
		{"remote": "github.com/me/r2*", "repo": "client-b"},
		{"remote": "github.com/me/*", "repo": "client-c"},
	}
}

var budgetRepos = []string{"personal", "projects", "client-a", "client-b", "client-c"}
