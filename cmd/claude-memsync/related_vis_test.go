package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// fakeAPI is a GitHub stub: private unless the repo is in public, slow when
// delay is set, and it counts the requests per path.
type fakeAPI struct {
	mu     sync.Mutex
	public map[string]bool
	delay  time.Duration
	hits   map[string]int
	srv    *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{public: map[string]bool{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		d, pub := f.delay, f.public[r.URL.Path]
		f.mu.Unlock()
		time.Sleep(d)
		vis := "private"
		if pub {
			vis = "public"
		}
		_ = json.NewEncoder(w).Encode(repoJSON{Name: "x", Private: tb(!pub), Visibility: vis})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }
func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.hits {
		n += c
	}
	return n
}

// visHost is a related host whose primary repo has a token and the fake API.
func visHost(t *testing.T, api *fakeAPI, remotes map[string]string, owners ...string) *relHost {
	t.Helper()
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, remotes)
	skipCloneURLCheck = false
	skipAPIHostCheck = true
	t.Cleanup(func() { skipAPIHostCheck = false })
	old := hostResolver
	hostResolver = func(string) (string, error) { return "github.com", nil }
	t.Cleanup(func() { hostResolver = old })
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "fake-token")
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	cfg := h.cfgs["a"]
	cfg.GitHubAPIURL, cfg.GitHubHost, cfg.GitHubOrg = api.srv.URL, "github.com", "Org"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	p := os.Getenv(routesEnv)
	b, _ := os.ReadFile(p)
	var rj map[string]any
	_ = json.Unmarshal(b, &rj)
	if len(owners) > 0 {
		rj["relatedOwners"] = owners
	}
	b, _ = json.Marshal(rj)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

func visSyncOut(t *testing.T) string {
	t.Helper()
	_, out, _ := run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	return out
}

// A read clone is fetched only from a URL on the allowed git host, under this
// host's org or an explicitly allowed owner, and private. Nothing is fetched
// otherwise (the fetch would reach for the network; refusals come first).
func TestVis_CloneURLIsCheckedBeforeAnyFetch(t *testing.T) {
	api := newFakeAPI(t)
	api.set(func() { api.public["/repos/Evil/mem-project-b"] = true })
	for name, tc := range map[string]struct {
		url    string
		owners []string
		want   string
	}{
		"other owner":      {"git@github.com:Evil/mem-project-b.git", nil, "not this host's org"},
		"non-github host":  {"https://gitlab.com/Org/mem-project-b.git", nil, "refused"},
		"allowed but open": {"git@github.com:Evil/mem-project-b.git", []string{"Evil"}, "not private"},
	} {
		t.Run(name, func(t *testing.T) {
			visHost(t, api, map[string]string{"mem-project-b": tc.url}, tc.owners...)
			if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
				t.Fatalf("relate: %s", errs)
			}
			out := visSyncOut(t)
			if !strings.Contains(out, "mem-project-b: refused") || !strings.Contains(out, tc.want) {
				t.Fatalf("want a refusal containing %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestVis_CloneURLOfThisOrgAndPrivateIsAccepted(t *testing.T) {
	api := newFakeAPI(t)
	h := visHost(t, api, nil)
	_ = h
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.vis.CheckCloneURL("mem-project-b", "git@github.com:Org/mem-project-b.git", nil); err != nil {
		t.Fatalf("own org, private: %v", err)
	}
	if err := rc.vis.CheckCloneURL("mem-project-b", "git@github.com:org/mem-project-b.git", nil); err != nil {
		t.Fatalf("owner compare is case-insensitive: %v", err)
	}
	if err := rc.vis.CheckCloneURL("mem-project-b", "git@github.com:Org/mem-project-c.git", nil); err == nil {
		t.Fatal("a URL naming another repo must be refused")
	}
}

// Hooks and the session context are offline: they rely on cached verdicts only
// and look only at the owner's edges; with no verdict an edge fails closed and
// the context says so. A non-hook command asks the API and fills the cache.
func TestVis_HooksCheckOnlyOwnEdgesAndUseTheCache(t *testing.T) {
	api := newFakeAPI(t)
	visHost(t, api, nil)
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatalf("relate: %s", errs)
	}
	if code, _, errs := relate("mem-project-x", "mem-project-y", "--read"); code != 0 {
		t.Fatalf("the foreign edge must be in the registry for this test: %s", errs)
	}
	api.set(func() { api.hits = map[string]int{} })
	cache := visCachePathOf(t)
	os.RemoveAll(cache)

	// No verdict: the edge is unverified, nothing is asked, and only the
	// owner's repos are ever mentioned.
	_, ctx, errs := contextFor("/proj/x")
	if !strings.Contains(ctx, "unverified") || api.count() != 0 {
		t.Fatalf("a hook with no cached verdict must not ask the API (%d hits):\n%s", api.count(), ctx)
	}
	if strings.Contains(errs+ctx, "mem-project-x") || strings.Contains(errs+ctx, "mem-project-y") {
		t.Errorf("a hook looked at an edge the owner is not an end of:\n%s\n%s", ctx, errs)
	}

	// A non-hook command asks the API and fills the cache; a hook then serves from it.
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"mem-project-a", "mem-project-b"} {
		if err := rc.vis.CheckPrivate(r); err != nil {
			t.Fatal(err)
		}
	}
	before := api.count()
	if before == 0 {
		t.Fatal("the non-hook check should have asked the API")
	}
	_, ctx, errs = contextFor("/proj/x")
	if !strings.Contains(ctx, "mem-project-b") || strings.Contains(ctx, "unverified") {
		t.Fatalf("context:\n%s", ctx)
	}
	if b, _ := os.ReadFile(cache); strings.Contains(string(b), "mem-project-x") || strings.Contains(string(b), "mem-project-y") || strings.Contains(errs, "mem-project-x") {
		t.Errorf("an edge the owner is not an end of was checked: %s %s", b, errs)
	}
	if api.count() != before {
		t.Errorf("a hook must not call the API (%d -> %d)", before, api.count())
	}

	// An expired verdict (beyond the TTL, within the cap) still serves a hook.
	old := visTTL
	visTTL = 0
	t.Cleanup(func() { visTTL = old })
	_, ctx, _ = contextFor("/proj/x")
	if strings.Contains(ctx, "unverified") || !strings.Contains(ctx, "mem-project-b") {
		t.Fatalf("a cached verdict must serve a hook:\n%s", ctx)
	}

	// No verdict at all: fail closed, said plainly.
	os.RemoveAll(cache)
	_, ctx, _ = contextFor("/proj/x")
	if !strings.Contains(ctx, "unverified") {
		t.Fatalf("want an unverified edge:\n%s", ctx)
	}
}

func visCachePathOf(t *testing.T) string {
	t.Helper()
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	return rc.vis.cachePath
}

// A related repo renamed on GitHub: the registry edge keeps its old name, so a
// relatedRemotes URL with the new name is refused (a relation to one repo can
// never fetch another) until the edge is re-made under the new name.
func TestVis_RenamedRelatedRepoURLIsRefusedUntilRerelated(t *testing.T) {
	api := newFakeAPI(t)
	visHost(t, api, map[string]string{"mem-project-b": "git@github.com:Org/mem-project-b2.git"})
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatalf("relate: %s", errs)
	}
	out := visSyncOut(t)
	if !strings.Contains(out, "mem-project-b: refused") || !strings.Contains(out, "mem-project-b2") {
		t.Fatalf("a renamed remote must be refused, naming the new name:\n%s", out)
	}
}

// A local repo whose remote was renamed keeps its relations when routes.json
// pins its remote name: the edge still finds the repo on this host.
func TestVis_LocalRepoRenamedRemoteKeepsRelationsViaPinnedName(t *testing.T) {
	api := newFakeAPI(t)
	h := visHost(t, api, nil)
	cfg := h.cfgs["a"]
	cfg.RemoteURL = "git@github.com:Org/mem-project-a-renamed.git"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rc.set.localKey("mem-project-a"); !ok {
		t.Fatal("routes.json pins the remote name: the repo must still be found under it")
	}
}

// A cached verdict serves for a hook when the API is down only while it is
// younger than visMaxStale; older ones fail closed.
func TestVis_StaleVerdictBeyondTheCapFailsClosed(t *testing.T) {
	api := newFakeAPI(t)
	visHost(t, api, nil)
	oldQ := quickTimeout
	quickTimeout = 50 * time.Millisecond
	t.Cleanup(func() { quickTimeout = oldQ })
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatalf("relate: %s", errs)
	}
	path := visCachePathOf(t)
	api.set(func() { api.delay = 300 * time.Millisecond })
	for _, tc := range []struct {
		age  time.Duration
		want bool // unverified
	}{{23 * time.Hour, false}, {25 * time.Hour, true}} {
		at := time.Now().Add(-tc.age)
		m := map[string]verdict{
			"org/mem-project-a": {Private: true, At: at},
			"org/mem-project-b": {Private: true, At: at},
		}
		b, _ := json.Marshal(m)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_, ctx, _ := contextFor("/proj/x")
		if got := strings.Contains(ctx, "unverified"); got != tc.want {
			t.Errorf("verdict %v old: unverified=%v, want %v\n%s", tc.age, got, tc.want, ctx)
		}
	}
}
