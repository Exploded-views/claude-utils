// Package testenv builds the real claude-memsync and claude-memmerge binaries
// once per test process, so tests can drive git through the actual filter and
// merge drivers. Tests only; it never touches the network or the user's HOME.
package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	once         sync.Once
	dir          string
	buildErr     error
	memsyncPath  string
	memmergePath string
)

// Binaries returns the paths of freshly built claude-memsync and claude-memmerge.
func Binaries(t testing.TB) (memsync, memmerge string) {
	t.Helper()
	once.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "..", "..")
		dir, buildErr = os.MkdirTemp("", "memsync-bin-")
		if buildErr != nil {
			return
		}
		memsyncPath = filepath.Join(dir, "claude-memsync")
		memmergePath = filepath.Join(dir, "claude-memmerge")
		for out, pkg := range map[string]string{memsyncPath: "./cmd/claude-memsync", memmergePath: "./cmd/claude-memmerge"} {
			cmd := exec.Command("go", "build", "-o", out, pkg)
			cmd.Dir = root
			if b, err := cmd.CombinedOutput(); err != nil {
				buildErr = &buildError{pkg, string(b), err}
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("build binaries: %v", buildErr)
	}
	return memsyncPath, memmergePath
}

type buildError struct {
	pkg, out string
	err      error
}

func (e *buildError) Error() string { return "go build " + e.pkg + ": " + e.err.Error() + "\n" + e.out }

var (
	verMu   sync.Mutex
	verDirs = map[string]string{}
)

// BinariesVersion returns freshly built claude-memsync and claude-memmerge (in
// one directory) whose `version` output carries ver, so a test can stand two
// builds side by side. Built once per version and process.
func BinariesVersion(t testing.TB, ver string) (memsync, memmerge string) {
	t.Helper()
	verMu.Lock()
	defer verMu.Unlock()
	d, ok := verDirs[ver]
	if !ok {
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "..", "..")
		var err error
		if d, err = os.MkdirTemp("", "memsync-bin-v-"); err != nil {
			t.Fatal(err)
		}
		ld := "-X github.com/MarimerLLC/claude-utils/internal/version.Override=" + ver
		for out, pkg := range map[string]string{"claude-memsync": "./cmd/claude-memsync", "claude-memmerge": "./cmd/claude-memmerge"} {
			cmd := exec.Command("go", "build", "-ldflags", ld, "-o", filepath.Join(d, out), pkg)
			cmd.Dir = root
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v", &buildError{pkg, string(b), err})
			}
		}
		verDirs[ver] = d
	}
	return filepath.Join(d, "claude-memsync"), filepath.Join(d, "claude-memmerge")
}
