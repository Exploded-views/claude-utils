package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// A git started WITHOUT -C or Dir (what a mutant that drops a command's Dir
// does) uses the process cwd. Once a test process is isolated, that cwd must
// never lead to an enclosing repo.
func TestIsolatedGitWithoutDirDoesNotReachAnEnclosingRepo(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	// Set up the outer repo with -C, so nothing here depends on the cwd.
	if b, err := exec.Command("git", "-c", "core.fsmonitor=false", "-C", outer, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, b)
	}
	saved := os.Environ()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			if k, v, ok := strings.Cut(kv, "="); ok {
				os.Setenv(k, v)
			}
		}
		os.Chdir(wd)
	})
	restore, err := IsolateGit(inner)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()

	cmd := exec.Command("git", "-c", "core.fsmonitor=false", "rev-parse", "--git-dir") // no -C, no Dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git run from %s found the enclosing repo: %s", inner, out)
	}
}

func TestIsolateGitSetsTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	saved := os.Environ()
	wd, _ := os.Getwd()
	restore, err := IsolateGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	cwd, _ := os.Getwd()
	home := os.Getenv("HOME")
	got := map[string]string{}
	for _, k := range gitIsoKeys {
		got[k] = os.Getenv(k)
	}
	restore()
	if rw, _ := os.Getwd(); rw != wd {
		t.Errorf("restore left cwd %s, want %s", rw, wd)
	}
	if _, err := os.Stat(home); err == nil {
		t.Errorf("restore left the temp home %s", home)
	}
	for _, kv := range saved {
		k, v, _ := strings.Cut(kv, "=")
		for _, g := range gitIsoKeys {
			if k == g && os.Getenv(k) != v {
				t.Errorf("restore left %s=%q, want %q", k, os.Getenv(k), v)
			}
		}
	}
	if rc, _ := filepath.EvalSymlinks(cwd); rc != real {
		t.Errorf("cwd = %s, want %s", cwd, real)
	}
	want := map[string]string{
		"GIT_CEILING_DIRECTORIES": filepath.Dir(real),
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CONFIG_GLOBAL":       os.DevNull,
		"HOME":                    home,
		"USERPROFILE":             home,
		"XDG_CONFIG_HOME":         home,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if home == "" || strings.HasPrefix(home, real) {
		t.Errorf("home %q must be a separate temp dir", home)
	}
}

// This process itself runs isolated (TestMain): its cwd is in a temp dir, git
// config from the user and system is off, and git finds no repo from here.
func TestThisProcessRunsIsolated(t *testing.T) {
	for k, w := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull} {
		if got := os.Getenv(k); got != w {
			t.Errorf("%s = %q, want %q", k, got, w)
		}
	}
	if os.Getenv("GIT_CEILING_DIRECTORIES") == "" {
		t.Error("GIT_CEILING_DIRECTORIES is unset")
	}
	if out, err := exec.Command("git", "rev-parse", "--git-dir").CombinedOutput(); err == nil {
		t.Errorf("git from the test cwd found a repo: %s", out)
	}
}

// Every package whose tests may exec git wires IsolateGitMain through TestMain.
func TestEveryGitTestingPackageIsolatesGit(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	execs := regexp.MustCompile(`"git"|gitwt\.|testenv\.Binaries|exec\.Command|proc\.(Group|Command|Spawn)`)
	type pkg struct{ git, wired bool }
	pkgs := map[string]*pkg{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(b)
		k := filepath.Dir(p)
		if pkgs[k] == nil {
			pkgs[k] = &pkg{}
		}
		if execs.MatchString(s) {
			pkgs[k].git = true
		}
		if strings.Contains(s, "func TestMain(") && regexp.MustCompile(`\bIsolateGitMain\(\)`).MatchString(s) {
			pkgs[k].wired = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for dir, p := range pkgs {
		if !p.git {
			continue
		}
		n++
		if !p.wired {
			rel, _ := filepath.Rel(root, dir)
			t.Errorf("%s: its tests exec git but no TestMain calls testenv.IsolateGitMain", rel)
		}
	}
	if n < 10 {
		t.Errorf("only %d git-running packages found; the walk is broken", n)
	}
}

func TestSrcDirIsThePackageDirNotTheIsolatedCwd(t *testing.T) {
	if _, err := os.Stat(filepath.Join(SrcDir(), "gitiso.go")); err != nil {
		t.Errorf("SrcDir %q is not the package dir: %v", SrcDir(), err)
	}
	if cwd, _ := os.Getwd(); cwd == SrcDir() {
		t.Error("the cwd is still the package dir: the process is not isolated")
	}
}
