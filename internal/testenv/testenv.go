// Package testenv builds the real claude-memsync and claude-memmerge binaries
// once per test process, so tests can drive git through the actual filter and
// merge drivers. Tests only; it never touches the network or the user's HOME.
package testenv

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// startEnv is the environment this test process started with, before any test
// pointed HOME at a temp dir.
var startEnv = os.Environ()

// hostTmp is the temp dir this process started with, before SmallTempDir
// swaps TMPDIR.
var hostTmp = os.TempDir()

// HostTempDir returns the temp dir the test process started with, whatever
// SmallTempDir set TMPDIR to since.
func HostTempDir() string { return hostTmp }

var (
	cachesOnce sync.Once
	caches     []string
)

// buildEnv is the environment for a go build run by a test: the process's own,
// plus the module, build and GOPATH directories resolved as at start-up. Tests
// set HOME to a temp dir, and go would otherwise fill <HOME>/go/pkg/mod with
// read-only files that t.TempDir cannot clean up. -modcacherw makes anything
// still downloaded there removable.
func buildEnv(t testing.TB) []string {
	t.Helper()
	cachesOnce.Do(func() {
		cmd := exec.Command("go", "env", "-json", "GOMODCACHE", "GOCACHE", "GOPATH")
		cmd.Env = startEnv
		out, err := cmd.Output()
		if err != nil {
			return
		}
		var m map[string]string
		if json.Unmarshal(out, &m) != nil {
			return
		}
		for _, k := range []string{"GOMODCACHE", "GOCACHE", "GOPATH"} {
			if m[k] != "" {
				caches = append(caches, k+"="+m[k])
			}
		}
	})
	env := append(os.Environ(), caches...)
	flags := strings.TrimSpace(os.Getenv("GOFLAGS") + " -modcacherw")
	return append(env, "GOFLAGS="+flags)
}

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
			cmd.Env = buildEnv(t)
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
			cmd.Env = buildEnv(t)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v", &buildError{pkg, string(b), err})
			}
		}
		verDirs[ver] = d
	}
	return filepath.Join(d, "claude-memsync"), filepath.Join(d, "claude-memmerge")
}

// SmallTempDir points TMPDIR at a fresh directory under /tmp
// when the host's temp dir is too big for route resolution to list (a busy
// macOS $TMPDIR holds hundreds of thousands of entries, so every directory
// below it is unresolved by design). It returns the function that removes it;
// call it before os.Exit.
func SmallTempDir() (cleanup func()) {
	if f, err := os.Open(os.TempDir()); err == nil {
		names, _ := f.Readdirnames(201)
		f.Close()
		if len(names) <= 200 {
			return func() {}
		}
	}
	// Not inside a checkout (a "not a repo" fixture must stay one): /tmp.
	d, err := os.MkdirTemp("/tmp", "gotmp-")
	if err != nil {
		return func() {}
	}
	os.Setenv("TMPDIR", d)
	return func() { os.RemoveAll(d) }
}
