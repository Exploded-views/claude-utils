package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A daemon cycle (commit the mirror, fetch, rebase, push, propagate) runs no
// program that the sync repo's own .git/config names: not an fsmonitor, a hook
// (core.hooksPath), a gpg program, or the upload-pack of a local remote. The
// cycle still succeeds. The config is set after the repo exists, so only the
// cycle's own git calls can run the canary.
func TestDaemonCycle_RepoConfigCannotRunPrograms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the canaries are shell scripts")
	}
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	script := func(key string) string {
		p := filepath.Join(dir, key+".sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+key+" >> "+ran+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"pre-commit", "post-commit", "post-checkout", "post-merge", "post-rewrite", "reference-transaction", "post-index-change", "pre-push"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\necho hooksPath >> "+ran+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{
		"core.fsmonitor": script("fsmonitor"), "core.hooksPath": hooks, "commit.gpgSign": "true", "gpg.program": script("gpg"),
		"remote.origin.uploadpack": script("uploadpack"), "core.askpass": script("askpass"),
		"core.alternateRefsCommand": script("alternateRefs"), "diff.external": script("diffExternal"),
	} {
		mustGit(t, f.repo, "config", k, v)
	}

	f.editLocal("clean.md", "a\nlocal\n")
	f.peerPush("peer1.md", "peer one\n")
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Errorf("the cycle failed: %v", err)
	}
	f.loop.pullTick(f.repo, f.roots)
	if !f.originHas("projects/p1/memory/clean.md") {
		t.Error("the cycle did not push")
	}
	if b, err := os.ReadFile(ran); err == nil {
		t.Errorf("repo-local config ran programs during the cycle:\n%s", b)
	}
}
