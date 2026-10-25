package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// blockPush makes the bare origin refuse every push; unblockPush lifts it.
func blockPush(t *testing.T, origin string) {
	t.Helper()
	hook := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho blocked >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func unblockPush(t *testing.T, origin string) {
	t.Helper()
	if err := os.Remove(filepath.Join(origin, "hooks", "pre-receive")); err != nil {
		t.Fatal(err)
	}
}

// plainNotes is every note of a sync repo's work-tree, by path.
func plainNotes(t *testing.T, syncDir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	root := filepath.Join(syncDir, "projects")
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		m[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return m
}

// originNotes decrypts every note at origin/main with the identity file.
func originNotes(t *testing.T, origin, idPath string) map[string]string {
	t.Helper()
	ids, err := agecrypt.LoadIdentities(idPath)
	if err != nil {
		t.Fatal(err)
	}
	o := gitwt.New(origin)
	files, _ := o.Run("ls-tree", "-r", "--name-only", "main")
	m := map[string]string{}
	for _, f := range strings.Fields(files) {
		if !strings.HasPrefix(f, "projects/") {
			continue
		}
		b, _ := o.Run("cat-file", "blob", "main:"+f)
		pt, err := agecrypt.Decrypt([]byte(b), ids)
		if err != nil {
			t.Fatalf("%s does not decrypt with %s: %v", f, filepath.Base(idPath), err)
		}
		m[strings.TrimPrefix(f, "projects/")] = string(pt)
	}
	return m
}

// rekeyHost is a routed host with the named classic encrypted repos, each
// holding the same few notes.
func rekeyHost(t *testing.T, names ...string) map[string]config.Config {
	t.Helper()
	cfgs := routedHost(t, names, "", nil)
	mem := filepath.Join(cfgs[names[0]].ClaudeProjectsDir, "p1", "memory")
	if err := os.MkdirAll(mem, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, body := range map[string]string{"one.md": "first note\n", "two.md": "second note\nwith lines\n", "MEMORY.md": "- [one](one.md)\n"} {
		if err := os.WriteFile(filepath.Join(mem, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := map[string]config.Config{}
	for _, n := range names {
		out[n] = encryptRouted(t, cfgs[n])
	}
	return out
}

func originTip(t *testing.T, origin string) string {
	t.Helper()
	o, err := gitwt.New(origin).Run("rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(o)
}
