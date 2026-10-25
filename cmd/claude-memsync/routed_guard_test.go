package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// privateRouted makes a routed host whose repos all run private-only, each with
// the remote given for it, against an API stub. It returns the configs.
func privateRouted(t *testing.T, remotes map[string]string, api http.HandlerFunc) map[string]config.Config {
	t.Helper()
	var names []string
	for n := range remotes {
		names = append(names, n)
	}
	cfgs := routedHost(t, names, "", []map[string]string{{"folder": "-a", "repo": names[0]}})
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "fake-token")
	skipAPIHostCheck = true
	t.Cleanup(func() { skipAPIHostCheck = false })
	old := hostResolver
	hostResolver = func(string) (string, error) { return "github.com", nil }
	t.Cleanup(func() { hostResolver = old })
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	for n, url := range remotes {
		cfg := cfgs[n]
		cfg.RemoteURL, cfg.PrivateOnly, cfg.GitHubAPIURL, cfg.GitHubHost = url, true, srv.URL, "github.com"
		if out, err := exec.Command("git", "-C", cfg.SyncDir, "remote", "add", "origin", url).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		cfgs[n] = cfg
	}
	return cfgs
}

func routedAPI(publicRepo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			_ = json.NewEncoder(w).Encode([]repoJSON{})
			return
		}
		pub := publicRepo != "" && strings.HasSuffix(r.URL.Path, "/"+publicRepo)
		_ = json.NewEncoder(w).Encode(repoJSON{Name: "mem-x", Private: tb(!pub), Visibility: map[bool]string{true: "public", false: "private"}[pub]})
	}
}

// doctor must check every repo the routes file names, not only the primary one.
func TestRouted_DoctorChecksEveryRoutedRepo(t *testing.T) {
	privateRouted(t, map[string]string{
		"a": "git@github.com:Org/mem-project-a.git",
		"b": "git@github.com:Org/mem-project-b.git",
	}, routedAPI("mem-project-b"))
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := set.Primary()
	var out, errb bytes.Buffer
	if code := doctorRunJoined(primary, "Org", set.remotes(), &out, &errb); code == 0 {
		t.Fatalf("a public routed repo that is not the primary must fail doctor: %s %s", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "Org/mem-project-b") || strings.Contains(errb.String(), "mem-project-a") {
		t.Errorf("must name exactly the public repo: %q", errb.String())
	}
	if code := runDoctor([]string{"--org", "Org"}); code == 0 {
		t.Error("runDoctor with a routes file must check every routed repo")
	}
}

// The private-only guard runs for each routed repo at start: one bad repo
// refuses the whole daemon, and nothing is left locked.
func TestRouted_StartRunsPrivateOnlyGuardPerRepo(t *testing.T) {
	cfgs := privateRouted(t, map[string]string{
		"a": "git@github.com:Org/mem-project-a.git",
		"b": "git@github.com:Org/claude-memories.git", // not a mem-* name
	}, routedAPI(""))
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Start(context.Background()); err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("Start = %v, want a refusal naming repo b", err)
	}
	for n, c := range cfgs {
		l, err := lock.Acquire(c.SyncDir)
		if err != nil {
			t.Fatalf("repo %s left locked after a refused start: %v", n, err)
		}
		l.Release()
	}
}

func TestRouted_StartWiresPushAllowedPerRepo(t *testing.T) {
	privateRouted(t, map[string]string{
		"a": "git@github.com:Org/mem-project-a.git",
		"b": "git@github.com:Org/mem-project-b.git",
	}, routedAPI(""))
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	oldRun := runLoop // no syncing, and so no network, in this test
	runLoop = func(ctx context.Context, l *syncpkg.Loop) error { <-ctx.Done(); return nil }
	t.Cleanup(func() { runLoop = oldRun })
	r, err := set.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if len(r.loops) != 2 {
		t.Fatalf("loops = %d", len(r.loops))
	}
	for _, l := range r.loops {
		if l.PushAllowed == nil {
			t.Errorf("loop %s has no push guard", l.Name)
		}
	}
}
