package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env { // the last entry wins, as in os/exec
		if strings.HasPrefix(kv, key+"=") {
			v = strings.TrimPrefix(kv, key+"=")
		}
	}
	return v
}

// Tests run with HOME set to a temp dir. A go build under that HOME would
// download modules into <temp HOME>/go/pkg/mod, read-only files that t.TempDir
// cannot remove ("unlinkat ...: permission denied" on CI). The build must keep
// the caches the test process was started with.
func TestBuildEnv_KeepsStartupCachesUnderTempHome(t *testing.T) {
	for _, k := range []string{"GOMODCACHE", "GOCACHE", "GOPATH"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("GOFLAGS", "")
	env := buildEnv(t)
	for _, k := range []string{"GOMODCACHE", "GOCACHE", "GOPATH"} {
		v := envValue(env, k)
		if v == "" {
			t.Errorf("%s is not passed to go build", k)
		} else if strings.HasPrefix(filepath.Clean(v), filepath.Clean(tmp)) {
			t.Errorf("%s = %s lies under the temp HOME %s", k, v, tmp)
		}
	}
	if !strings.Contains(envValue(env, "GOFLAGS"), "-modcacherw") {
		t.Errorf("GOFLAGS = %q lacks -modcacherw", envValue(env, "GOFLAGS"))
	}
}
