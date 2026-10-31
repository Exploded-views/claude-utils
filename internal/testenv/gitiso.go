package testenv

import (
	"os"
	"path/filepath"
)

// srcDir is the cwd this process started in: the package directory under test.
var srcDir, _ = os.Getwd()

// SrcDir is the package directory this test process started in. IsolateGit
// moves the cwd to a temp dir, so a test that reads its package's (or the
// repo's) source files joins their relative paths to this.
func SrcDir() string { return srcDir }

// gitIsoKeys are the variables IsolateGit sets.
var gitIsoKeys = []string{
	"GIT_CEILING_DIRECTORIES", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL",
	"HOME", "USERPROFILE", "XDG_CONFIG_HOME",
}

// IsolateGit confines every git this process starts, whatever its arguments:
//
//   - the cwd becomes dir, and GIT_CEILING_DIRECTORIES its parent, so a git
//     run without -C or Dir (a command whose Dir was dropped) can find no
//     repo above dir;
//   - the system and global git config are off (GIT_CONFIG_NOSYSTEM=1,
//     GIT_CONFIG_GLOBAL=os.DevNull, which is NUL on Windows), and HOME,
//     USERPROFILE and XDG_CONFIG_HOME point at an empty temp dir.
//
// restore puts back the cwd and the variables and removes that home.
func IsolateGit(dir string) (restore func(), err error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if real, err = filepath.Abs(real); err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "gitiso-home-")
	if err != nil {
		return nil, err
	}
	oldWd, err := os.Getwd()
	if err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	type kv struct {
		v  string
		ok bool
	}
	old := map[string]kv{}
	for _, k := range gitIsoKeys {
		v, ok := os.LookupEnv(k)
		old[k] = kv{v, ok}
	}
	restore = func() {
		for k, o := range old {
			if o.ok {
				os.Setenv(k, o.v)
			} else {
				os.Unsetenv(k)
			}
		}
		os.Chdir(oldWd)
		os.RemoveAll(home)
	}
	if err := os.Chdir(real); err != nil {
		restore()
		return nil, err
	}
	for k, v := range map[string]string{
		"GIT_CEILING_DIRECTORIES": filepath.Dir(real),
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CONFIG_GLOBAL":       os.DevNull,
		"HOME":                    home,
		"USERPROFILE":             home,
		"XDG_CONFIG_HOME":         home,
	} {
		os.Setenv(k, v)
	}
	return restore, nil
}

// IsolateGitMain is what a package's TestMain calls (after SmallTempDir): it
// isolates git in a fresh temp dir and returns the function that undoes it.
// Call that before os.Exit.
func IsolateGitMain() (cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "gitiso-")
	if err != nil {
		return nil, err
	}
	restore, err := IsolateGit(dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return func() { restore(); os.RemoveAll(dir) }, nil
}
