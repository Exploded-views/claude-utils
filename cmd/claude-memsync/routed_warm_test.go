//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/route"
)

// Start does not wait for the start-up resolution beyond warmDeadline: with
// more hung git calls than the 4 slots, it returns on time with its loops
// running, and the folders still unresolved are decided as git answers.
func TestStart_WarmIsBoundedByADeadline(t *testing.T) {
	useFakeHost(t)
	cfgs := routedHost(t, []string{"projects"}, "", []map[string]string{{"remote": "github.com/me/*", "repo": "projects"}})
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	work := t.TempDir()
	for i := 0; i < 9; i++ { // more than the 4 git slots
		d := filepath.Join(work, "w"+strconv.Itoa(i))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:me/w" + strconv.Itoa(i) + ".git"}} {
			if out, err := exec.Command(realGit, append([]string{"-C", d}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		if err := os.MkdirAll(filepath.Join(cfgs["projects"].ClaudeProjectsDir, route.Escape(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	pids := filepath.Join(bin, "pids")
	// Hangs only for the project directories; everything else is the real git.
	script := "#!/bin/sh\ncase \"$*\" in *\"-C " + work + "/\"*) echo $$ >> " + pids + "; exec sleep 60;; esac\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // stop any stray fake git by its exact PID
		b, _ := os.ReadFile(pids)
		for _, s := range strings.Fields(string(b)) {
			if p, err := strconv.Atoi(s); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	t.Cleanup(route.SetGitTimeoutForTest(2 * time.Second))
	prev := warmDeadline
	warmDeadline = 300 * time.Millisecond
	t.Cleanup(func() { warmDeadline = prev })
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r, err := set.Start(context.Background())
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if b, _ := os.ReadFile(pids); len(strings.Fields(string(b))) < 4 {
		t.Fatalf("only %d resolution gits hung; the fixture does not exercise the semaphore", len(strings.Fields(string(b))))
	}
	if took > 1500*time.Millisecond {
		t.Errorf("Start took %v with every project git hung, want about the 300ms warm deadline", took)
	}
	if len(r.loops) != 1 {
		t.Errorf("%d loops running, want 1", len(r.loops))
	}
}
