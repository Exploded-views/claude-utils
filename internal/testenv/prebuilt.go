package testenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/version"
)

// Prebuilt mode runs the budget gates against binaries built elsewhere (the
// release archive), on a host with no Go toolchain and no checkout.
const (
	// EnvBinDir holds claude-memsync and claude-memmerge, as a release archive unpacks them.
	EnvBinDir = "MEMSYNC_BUDGET_BIN_DIR"
	// EnvTag is the release tag both binaries must report from `version`.
	EnvTag = "MEMSYNC_BUDGET_TAG"
)

// releaseTagRE is the release tag rule of package-release.sh, release.yml and
// bootstrap.sh --prebuilt-dir: prebuilt mode only certifies release-stamped bytes.
var releaseTagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$`)

// selfVersion is the running test binary's own version. B1, B4, B7 and B8 run
// this binary's code in-process, so it must be the tag too.
var selfVersion = version.String

var (
	prebuiltMu    sync.Mutex
	prebuiltCache = map[string]bool{} // dir|tag pairs whose versions were already checked
)

// prebuiltBinaries reads the two variables. With neither set it reports
// on=false (source mode). With one alone, or with binaries that do not report
// the tag, it is an error: a bad prebuilt set-up must fail, never fall back to
// a build or skip.
func prebuiltBinaries(getenv func(string) string) (ms, mm string, on bool, err error) {
	dir, tag := getenv(EnvBinDir), getenv(EnvTag)
	if dir == "" && tag == "" {
		return "", "", false, nil
	}
	if dir == "" || tag == "" {
		return "", "", true, fmt.Errorf("%s and %s go together: set both or neither (dir=%q tag=%q)", EnvBinDir, EnvTag, dir, tag)
	}
	if !releaseTagRE.MatchString(tag) {
		return "", "", true, fmt.Errorf("%s=%q is not a release tag (vX.Y.Z or vX.Y.Z-suffix)", EnvTag, tag)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", "", true, err
	}
	ms, mm = filepath.Join(dir, "claude-memsync"), filepath.Join(dir, "claude-memmerge")
	prebuiltMu.Lock()
	defer prebuiltMu.Unlock()
	if prebuiltCache[dir+"|"+tag] {
		return ms, mm, true, nil
	}
	for _, b := range []string{ms, mm} {
		out, err := exec.Command(b, "version").Output()
		if err != nil {
			return "", "", true, fmt.Errorf("%s version: %v", b, err)
		}
		want := filepath.Base(b) + " " + tag
		if got := strings.TrimSpace(string(out)); got != want {
			return "", "", true, fmt.Errorf("version mismatch: %s reports %q, want %q (%s)", b, got, want, EnvTag)
		}
	}
	prebuiltCache[dir+"|"+tag] = true
	return ms, mm, true, nil
}

// harnessPaths are the places the gates read and write under a HOME: the
// routes file and config directory the binaries compute from it, and the
// directories the environment points at.
func harnessPaths(home string) []string {
	return []string{
		filepath.Join(home, ".config", "claude-memsync", "routes.json"),
		filepath.Join(home, ".config", "claude-memsync"),
		filepath.Join(home, ".claudesync.d"),
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, "tmp"),
	}
}

// resolve evaluates symlinks in the longest existing prefix of p.
func resolve(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func under(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

// checkIsolated refuses a temp HOME or any harness path that resolves under
// the original HOME, or a harness path that is not under the temp HOME.
func checkIsolated(orig, home string, paths []string) error {
	var o string
	if orig != "" {
		o = resolve(orig)
	}
	h := resolve(home)
	for _, p := range append([]string{home}, paths...) {
		r := resolve(p)
		if o != "" && under(r, o) {
			return fmt.Errorf("refusing to run: %s resolves under the original HOME %s", p, orig)
		}
		if !under(r, h) {
			return fmt.Errorf("refusing to run: %s is not under the temp HOME %s", p, home)
		}
	}
	return nil
}

// isolateHome makes a fresh HOME under base and points HOME, USERPROFILE, the
// XDG directories and TMPDIR into it, for this process and every binary it
// starts. paths returns the harness paths to check (harnessPaths when nil).
func isolateHome(orig, base string, paths func(home string) []string) (home string, cleanup func(), err error) {
	if paths == nil {
		paths = harnessPaths
	}
	if home, err = os.MkdirTemp(base, "memsync-budget-home-"); err != nil {
		return "", nil, err
	}
	if err := checkIsolated(orig, home, paths(home)); err != nil {
		os.RemoveAll(home)
		return "", nil, err
	}
	set := map[string]string{
		"HOME": home, "USERPROFILE": home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"TMPDIR":          filepath.Join(home, "tmp"),
	}
	// Nothing inherited may steer git, ssh or the project lookup of a binary under test.
	set["GIT_CONFIG_GLOBAL"] = filepath.Join(home, ".gitconfig")
	set["GIT_CONFIG_NOSYSTEM"] = "1"
	set["GIT_SSH_COMMAND"] = "false" // the fixtures use file:// remotes; ssh must never be reached
	var unset []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, mine := set[k]; strings.HasPrefix(k, "GIT_") && !mine {
			unset = append(unset, k)
		}
	}
	unset = append(unset, "CLAUDE_PROJECT_DIR", "SSH_AUTH_SOCK")
	if err := os.WriteFile(set["GIT_CONFIG_GLOBAL"], nil, 0o600); err != nil {
		os.RemoveAll(home)
		return "", nil, err
	}
	old := map[string]*string{}
	remember := func(k string) {
		if _, done := old[k]; done {
			return
		}
		if cur, ok := os.LookupEnv(k); ok {
			c := cur
			old[k] = &c
		} else {
			old[k] = nil
		}
	}
	for k, v := range set {
		if strings.HasPrefix(k, "XDG_") || k == "TMPDIR" {
			if err := os.MkdirAll(v, 0o700); err != nil {
				os.RemoveAll(home)
				return "", nil, err
			}
		}
		remember(k)
		os.Setenv(k, v)
	}
	for _, k := range unset {
		remember(k)
		os.Unsetenv(k)
	}
	return home, func() {
		for k, v := range old {
			if v == nil {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, *v)
			}
		}
		os.RemoveAll(home)
	}, nil
}

// prebuiltSetup is what a budget test binary does before any gate: in prebuilt
// mode it checks both binaries report the tag and moves HOME and friends into a
// fresh temp dir. In source mode it does nothing.
func prebuiltSetup(getenv func(string) string, orig, base string) (cleanup func(), err error) {
	_, _, on, err := prebuiltBinaries(getenv)
	if err != nil {
		return nil, err
	}
	if !on {
		return func() {}, nil
	}
	if orig == "" {
		return nil, errors.New("HOME is not set in the start environment: cannot tell the original HOME from the temp one, refusing to run")
	}
	if got, tag := selfVersion(), getenv(EnvTag); got != tag {
		return nil, fmt.Errorf("this test binary reports version %q, want %q (%s): B1, B4, B7 and B8 run its own code, so use the budget archive of the same release", got, tag, EnvTag)
	}
	_, cleanup, err = isolateHome(orig, base, nil)
	return cleanup, err
}

// PrebuiltSetup is called from TestMain before m.Run; a non-nil error means
// exit 1 without running a gate. The original HOME is the one this process
// started with.
func PrebuiltSetup() (cleanup func(), err error) {
	orig := ""
	for _, kv := range startEnv {
		if strings.HasPrefix(kv, "HOME=") {
			orig = strings.TrimPrefix(kv, "HOME=")
		}
	}
	base := os.TempDir()
	if b, ok := pickBase(tempCandidates()); ok {
		base = b
	}
	return prebuiltSetup(os.Getenv, orig, base)
}

// prebuiltOrFatal is Binaries' prebuilt branch.
func prebuiltOrFatal(t testing.TB) (ms, mm string, on bool) {
	t.Helper()
	ms, mm, on, err := prebuiltBinaries(os.Getenv)
	if err != nil {
		t.Fatalf("prebuilt budget mode: %v", err)
	}
	return ms, mm, on
}

// Prebuilt reports whether the budget gates run against prebuilt binaries.
func Prebuilt() bool { return os.Getenv(EnvBinDir) != "" || os.Getenv(EnvTag) != "" }
