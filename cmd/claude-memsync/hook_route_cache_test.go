package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/route"
)

// changes --hook gates the changed folders of its one project through the
// Router: a path rule, the remote rule and the root-commit lookup each ask for
// the directory the folder names. With that directory under one huge temp-shaped
// directory, the hook must read the huge one once, not three times (each read
// is about 10 s on a real $TMPDIR), and print its report inside its 1.5 s
// deadline, past which it prints nothing.
func TestHook_ProjectUnderHugeDirReadsItOnce(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", []map[string]string{
		{"path": "/nowhere-at-all/**", "repo": "projects"},
		{"remote": "github.com/me/*", "repo": "projects"},
	})
	big := filepath.Join(t.TempDir(), "T")
	if err := os.MkdirAll(big, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("tmp.%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proj := filepath.Join(big, "repo")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:me/r.git"}} {
		if out, err := gitRun(proj, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	in := hookJSON("sess", proj, "UserPromptSubmit")
	runHook(t, in, "--hook") // baseline
	addNotes(t, cfgs["projects"].Path(), map[string]string{route.Escape(proj) + "/memory/n.md": "x"})

	var mu sync.Mutex
	reads := 0
	restore := route.SetReadDirForTest(func(dir string, n int) ([]os.DirEntry, error) {
		if dir == big {
			mu.Lock()
			reads++
			mu.Unlock()
		}
		return route.ReadDirN(dir, n)
	})
	defer restore()

	start := time.Now()
	code, out, errs := runHook(t, in, "--hook")
	elapsed := time.Since(start)
	if code != 0 || errs != "" {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	if !strings.Contains(out, "n.md") {
		t.Fatalf("hook printed no report (past its deadline?) after %v: %q", elapsed, out)
	}
	if elapsed > 3*time.Second {
		t.Errorf("hook took %v, want under 3s", elapsed)
	}
	if reads != 1 {
		t.Errorf("big dir read %d times by one hook run, want 1", reads)
	}
	t.Logf("hook %v, big dir reads %d", elapsed, reads)
}

func gitRun(dir string, args ...string) ([]byte, error) {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
}
