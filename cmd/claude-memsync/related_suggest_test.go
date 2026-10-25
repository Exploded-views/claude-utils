package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/related"
	"github.com/MarimerLLC/claude-utils/internal/route"
)

// sugSpec is one routed repo of a suggestion host: its routes.json key, remote
// name, kind, the origin URL of its checkout and files in that checkout.
type sugSpec struct {
	key, remote, kind, url, client string
	files                          map[string]string
}

type sugHost struct {
	*relHost
	dir    map[string]string // key -> checkout dir
	folder map[string]string // key -> Claude project folder
	pairs  int
}

func newSugHost(t *testing.T, specs ...sugSpec) *sugHost {
	t.Helper()
	own := map[string][2]string{}
	for _, s := range specs {
		own[s.key] = [2]string{s.remote, s.kind}
	}
	h := &sugHost{relHost: newRelHost(t, own, nil), dir: map[string]string{}, folder: map[string]string{}}
	var routes []map[string]string
	base := t.TempDir()
	for _, s := range specs {
		d := filepath.Join(base, "co-"+s.key)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		g := gitwt.New(d)
		if err := g.Init("main"); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", d, "remote", "add", "origin", s.url).CombinedOutput(); err != nil {
			t.Fatalf("remote add: %v %s", err, out)
		}
		for n, body := range s.files {
			if err := os.WriteFile(filepath.Join(d, n), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		h.dir[s.key] = d
		h.folder[s.key] = route.Escape(d)
		// Claude has a project folder for every checkout it was opened in.
		if err := os.MkdirAll(filepath.Join(h.home, ".claude", "projects", h.folder[s.key]), 0o700); err != nil {
			t.Fatal(err)
		}
		routes = append(routes, map[string]string{"folder": h.folder[s.key], "repo": s.key})
	}
	p := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &rj); err != nil {
		t.Fatal(err)
	}
	rj["routes"] = routes
	for _, sp := range specs {
		if sp.client != "" {
			rj["repos"].(map[string]any)[sp.key].(map[string]any)["client"] = sp.client
		}
	}
	b, _ = json.Marshal(rj)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// session writes a fake transcript for the repo's folder with a secret-looking
// string in the message content and in a field the parser must not read.
func (h *sugHost) session(t *testing.T, key, name, startTS, endTS string) {
	t.Helper()
	dir := filepath.Join(h.home, ".claude", "projects", h.folder[key])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(ts string) string {
		return fmt.Sprintf(`{"type":"user","cwd":%q,"timestamp":%q,"extra":"ZZ-EXTRA-SECRET","message":{"role":"user","content":"ghp_TRANSCRIPTSECRET0123456789"}}`, h.dir[key], ts)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(line(startTS)+"\n"+line(endTS)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// coOpen gives both repos a session on two different days; every pair gets
// days of its own, so pairs do not co-open by accident.
func (h *sugHost) coOpen(t *testing.T, a, b string) {
	h.pairs++
	d1, d2 := 1+h.pairs*8, 5+h.pairs*8
	ts := func(day, hour, min int) string {
		return fmt.Sprintf("2026-%02d-%02dT%02d:%02d:00Z", 10+(day-1)/28, 1+(day-1)%28, hour, min)
	}
	n := fmt.Sprintf("p%d", h.pairs)
	h.session(t, a, n+"a", ts(d1, 9, 0), ts(d1, 10, 0))
	h.session(t, b, n+"a", ts(d1, 11, 0), ts(d1, 12, 0))
	h.session(t, a, n+"b", ts(d2, 9, 0), ts(d2, 10, 0))
	h.session(t, b, n+"b", ts(d2, 9, 30), ts(d2, 10, 30))
}

// suggest is the host-wide listing.
func suggest(args ...string) (int, string, string) {
	return suggestRaw(append([]string{"--all"}, args...)...)
}

func suggestRaw(args ...string) (int, string, string) {
	return run(func(o, e *bytes.Buffer) int { return relatedCmd(o, e, append([]string{"suggest"}, args...)) })
}

// refresh is what the daemon's tick does for the session-start count.
func refresh(t *testing.T, budget time.Duration) bool {
	t.Helper()
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	return rc.refreshSuggestCache(budget)
}

func twoProjects(t *testing.T) *sugHost {
	return newSugHost(t,
		sugSpec{key: "a", remote: "mem-project-a", kind: "project", url: "https://github.com/me/app-a.git"},
		sugSpec{key: "b", remote: "mem-project-b", kind: "project", url: "https://github.com/me/app-b.git"})
}

func TestSuggest_CoOpen_IgnoresTranscriptContent(t *testing.T) {
	h := twoProjects(t)
	h.coOpen(t, "a", "b")
	code, out, errs := suggest()
	if code != 0 || !strings.Contains(out, "mem-project-a -> mem-project-b") || !strings.Contains(out, "co-opened on 2 days") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	_, js, _ := suggest("--json")
	for _, s := range []string{out, errs, js} {
		for _, bad := range []string{"TRANSCRIPTSECRET", "EXTRA-SECRET"} {
			if strings.Contains(s, bad) {
				t.Fatalf("transcript content leaked (%s):\n%s", bad, s)
			}
		}
	}
	var parsed struct {
		Suggestions []struct {
			From, To, Mode string
			Score          int
			Signals        []struct{ Kind, Detail string }
		}
		Withheld, Hidden int
	}
	if err := json.Unmarshal([]byte(js), &parsed); err != nil || len(parsed.Suggestions) != 1 ||
		parsed.Suggestions[0].Mode != "read" || parsed.Suggestions[0].Signals[0].Kind != "co-open" {
		t.Fatalf("json %v: %s", err, js)
	}
}

func TestSuggest_NoSignalNoSuggestion(t *testing.T) {
	h := twoProjects(t)
	h.session(t, "a", "s1", "2026-10-01T09:00:00Z", "2026-10-01T10:00:00Z")
	h.session(t, "b", "s1", "2026-10-01T11:00:00Z", "2026-10-01T12:00:00Z") // one day only
	_, out, _ := suggest()
	if strings.Contains(out, "->") || !strings.Contains(out, "no suggestions") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestSuggest_References(t *testing.T) {
	h := newSugHost(t,
		sugSpec{key: "a", remote: "mem-project-a", kind: "project", url: "git@github.com:me/app-a.git",
			files: map[string]string{"go.mod": "module github.com/me/app-a\n\nrequire github.com/other/app-b v1.0.0\n", "README.md": "SECRET-README-BODY"}},
		sugSpec{key: "b", remote: "mem-project-b", kind: "project", url: "https://github.com/other/app-b.git"})
	_, out, _ := suggest()
	if !strings.Contains(out, "mem-project-a -> mem-project-b") || !strings.Contains(out, "go.mod names github.com/other/app-b") {
		t.Fatalf("out:\n%s", out)
	}
	if strings.Contains(out, "SECRET-README-BODY") || strings.Contains(out, "v1.0.0") {
		t.Fatalf("file contents leaked:\n%s", out)
	}
	// negative: the go.mod names something that is not a routed repo
	os.WriteFile(filepath.Join(h.dir["a"], "go.mod"), []byte("module x\nrequire github.com/other/unrouted v1.0.0\n"), 0o600)
	if _, out, _ := suggest(); strings.Contains(out, "->") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestSuggest_SameOwnerVocabulary(t *testing.T) {
	h := twoProjects(t)
	os.WriteFile(filepath.Join(h.cfgs["a"].SyncDir, "MEMORY.md"), []byte("# Clays leaderboard\n- [n](n.md) — BODYTEXT\n## Trap scoring\n"), 0o600)
	os.WriteFile(filepath.Join(h.cfgs["b"].SyncDir, "MEMORY.md"), []byte("# Scoring trap rounds\n"), 0o600)
	_, out, _ := suggest()
	if !strings.Contains(out, "mem-project-a -> mem-project-b") || !strings.Contains(out, "same owner github.com/me") || !strings.Contains(out, "scoring, trap") {
		t.Fatalf("out:\n%s", out)
	}
	if strings.Contains(out, "BODYTEXT") {
		t.Fatalf("note text leaked:\n%s", out)
	}
	// a different owner: no suggestion
	os.WriteFile(filepath.Join(h.cfgs["b"].SyncDir, "config.json.keep"), nil, 0o600) // noop
	h2 := newSugHost(t,
		sugSpec{key: "a", remote: "mem-project-a", kind: "project", url: "https://github.com/me/app-a.git"},
		sugSpec{key: "b", remote: "mem-project-b", kind: "project", url: "https://github.com/someone/app-b.git"})
	os.WriteFile(filepath.Join(h2.cfgs["a"].SyncDir, "MEMORY.md"), []byte("# Clays leaderboard\n# Trap scoring\n"), 0o600)
	os.WriteFile(filepath.Join(h2.cfgs["b"].SyncDir, "MEMORY.md"), []byte("# Scoring trap rounds\n"), 0o600)
	if _, out, _ := suggest(); strings.Contains(out, "->") {
		t.Fatalf("different owners:\n%s", out)
	}
}

func TestSuggest_MinScore(t *testing.T) {
	h := twoProjects(t)
	h.coOpen(t, "a", "b")
	if _, out, _ := suggest("--min-score", "99"); strings.Contains(out, "->") {
		t.Fatalf("out:\n%s", out)
	}
	if _, out, _ := suggest("--min-score", "1"); !strings.Contains(out, "->") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestSuggest_PolicyWithholdsClientCandidates(t *testing.T) {
	h := newSugHost(t,
		sugSpec{key: "a", remote: "mem-project-a", kind: "project", url: "https://github.com/me/app-a.git"},
		sugSpec{key: "x", remote: "mem-client-xcorp", kind: "client", url: "https://github.com/xcorp/site.git"},
		sugSpec{key: "y", remote: "mem-client-ycorp", kind: "client", url: "https://github.com/ycorp/site.git"})
	h.coOpen(t, "a", "x") // client <-> own
	h.coOpen(t, "x", "y") // cross-client
	h.coOpen(t, "a", "y") // client <-> own
	code, out, _ := suggest()
	if code != 0 || strings.Contains(out, "->") || strings.Contains(out, "xcorp") || strings.Contains(out, "ycorp") ||
		!strings.Contains(out, "3 withheld by policy") {
		t.Fatalf("out:\n%s", out)
	}
	_, js, _ := suggest("--json")
	if strings.Contains(js, "xcorp") || strings.Contains(js, "ycorp") {
		t.Fatalf("client repo named in json:\n%s", js)
	}
}

func TestAccept_OneEdge_PinOnlyUnlessShared(t *testing.T) {
	h := twoProjects(t)
	h.coOpen(t, "a", "b")
	regDir := h.cfgs["registry"].SyncDir
	before, _ := gitwt.New(regDir).Run("rev-parse", "HEAD")
	code, out, errs := relatedC("accept", "mem-project-a", "mem-project-b")
	if code != 0 {
		t.Fatalf("accept: %d %s %s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(regDir, related.FileName)); err == nil {
		t.Fatal("accept without --share wrote the registry")
	}
	after, _ := gitwt.New(regDir).Run("rev-parse", "HEAD")
	if before != after {
		t.Fatal("accept without --share committed in the registry")
	}
	acc, err := related.LoadAccepted(acceptedPath(os.Getenv(routesEnv)))
	if err != nil || !acc.Has(related.Edge{From: "mem-project-a", To: "mem-project-b", Mode: related.ModeRead}) {
		t.Fatalf("pin: %v %+v", err, acc)
	}
	if _, out, _ := suggest(); strings.Contains(out, "->") {
		t.Fatalf("accepted edge suggested again:\n%s", out)
	}
}

func TestAccept_Share_WritesAndCommitsRegistryLocally(t *testing.T) {
	h := twoProjects(t)
	h.coOpen(t, "a", "b")
	regDir := h.cfgs["registry"].SyncDir
	if code, _, errs := relatedC("accept", "--share", "mem-project-a", "mem-project-b"); code != 0 {
		t.Fatalf("accept --share: %s", errs)
	}
	reg, err := related.LoadRegistry(regDir)
	if err != nil || len(reg.Edges()) != 1 || reg.Edges()[0] != (related.Edge{From: "mem-project-a", To: "mem-project-b", Mode: related.ModeRead}) {
		t.Fatalf("registry %v %v", err, reg.Edges())
	}
	log, _ := gitwt.New(regDir).Run("log", "--format=%s")
	if !strings.Contains(log, "relate: mem-project-a -> mem-project-b (read)") {
		t.Fatalf("no commit:\n%s", log)
	}
	if st, _ := gitwt.New(regDir).Run("status", "--porcelain", "--", related.FileName); strings.TrimSpace(st) != "" {
		t.Fatalf("related.json left uncommitted:\n%s", st)
	}
}

func TestAccept_RechecksPolicy(t *testing.T) {
	h := newSugHost(t,
		sugSpec{key: "a", remote: "mem-project-a", kind: "project", url: "https://github.com/me/app-a.git"},
		sugSpec{key: "x", remote: "mem-client-xcorp", kind: "client", url: "https://github.com/xcorp/site.git"})
	regDir := h.cfgs["registry"].SyncDir
	for _, share := range []bool{false, true} {
		args := []string{"accept", "mem-project-a", "mem-client-xcorp"}
		if share {
			args = []string{"accept", "--share", "mem-project-a", "mem-client-xcorp"}
		}
		code, _, errs := relatedC(args...)
		if code == 0 || !strings.Contains(errs, "refused") {
			t.Fatalf("share=%v: %d %q", share, code, errs)
		}
	}
	if _, err := os.Stat(acceptedPath(os.Getenv(routesEnv))); err == nil {
		t.Fatal("a refused accept wrote the pin")
	}
	if _, err := os.Stat(filepath.Join(regDir, related.FileName)); err == nil {
		t.Fatal("a refused accept wrote the registry")
	}
	if code, _, _ := relatedC("accept", "mem-project-a"); code != 2 {
		t.Fatal("one argument is a usage error")
	}
}

func TestDismiss_SuppressesPerHost(t *testing.T) {
	h := twoProjects(t)
	h.coOpen(t, "a", "b")
	regDir := h.cfgs["registry"].SyncDir
	if code, _, errs := relatedC("dismiss", "mem-project-a", "mem-project-b"); code != 0 {
		t.Fatalf("dismiss: %s", errs)
	}
	if _, out, _ := suggest(); strings.Contains(out, "->") {
		t.Fatalf("dismissed edge suggested:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(regDir, related.FileName)); err == nil {
		t.Fatal("dismiss touched the registry")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv(routesEnv)), "related-dismissed.json")); err != nil {
		t.Fatal("no dismissal record")
	}
}

func TestSuggest_DeclaredEdgeNotResuggested(t *testing.T) {
	h := twoProjects(t)
	h.coOpen(t, "a", "b")
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatal(errs)
	}
	if _, out, _ := suggest(); strings.Contains(out, "->") {
		t.Fatalf("declared edge suggested:\n%s", out)
	}
}

func TestSuggest_DenylistHidesNames(t *testing.T) {
	h := twoProjects(t)
	h.coOpen(t, "a", "b")
	os.WriteFile(h.cfgs["a"].DenylistPath(), []byte("project-b\n"), 0o600)
	code, out, _ := suggest()
	if code != 0 || strings.Contains(out, "project-b") || strings.Contains(out, "->") || !strings.Contains(out, "1 hidden by denylist") {
		t.Fatalf("out:\n%s", out)
	}
	_, js, _ := suggest("--json")
	if strings.Contains(js, "project-b") {
		t.Fatalf("json:\n%s", js)
	}
	// an unusable denylist hides everything
	os.WriteFile(h.cfgs["a"].DenylistPath(), []byte("(\n"), 0o600)
	if _, out, _ := suggest(); strings.Contains(out, "->") {
		t.Fatalf("unusable denylist:\n%s", out)
	}
}

func TestContext_SuggestionLine(t *testing.T) {
	h := newSugHost(t,
		sugSpec{key: "a", remote: "mem-project-a", kind: "project", url: "https://github.com/me/app-a.git"},
		sugSpec{key: "b", remote: "mem-project-b", kind: "project", url: "https://github.com/me/app-b.git"},
		sugSpec{key: "c", remote: "mem-project-c", kind: "project", url: "https://github.com/zed/app-c.git"},
		sugSpec{key: "x", remote: "mem-client-xcorp", kind: "client", url: "https://github.com/xcorp/site.git"})
	h.coOpen(t, "a", "b")
	h.coOpen(t, "a", "x") // withheld: never counted, never named
	if _, out, _ := contextFor(h.dir["a"]); strings.Contains(out, "related-memory suggestions") {
		t.Fatalf("session start computed or invented a count with no cache:\n%s", out)
	}
	refresh(t, 0)
	code, out, errs := contextFor(h.dir["a"])
	if code != 0 || !strings.Contains(out, "1 related-memory suggestions; run `claude-memsync related suggest`") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	if strings.Contains(out, "xcorp") || strings.Contains(out, "mem-project-b") {
		t.Fatalf("names in the line:\n%s", out)
	}
	if _, out, _ := contextFor(h.dir["c"]); strings.Contains(out, "related-memory suggestions") {
		t.Fatalf("unrelated owner got a line:\n%s", out)
	}
	if code, _, _ := relatedC("dismiss", "mem-project-a", "mem-project-b"); code != 0 {
		t.Fatal("dismiss")
	}
	if _, out, _ := contextFor(h.dir["a"]); strings.Contains(out, "related-memory suggestions") {
		t.Fatalf("line after dismissal:\n%s", out)
	}
}

func fiveRepoHost(t *testing.T) *sugHost {
	return newSugHost(t,
		sugSpec{key: "a", remote: "mem-project-a", kind: "project", url: "https://github.com/me/app-a.git"},
		sugSpec{key: "c", remote: "mem-project-c", kind: "project", url: "https://github.com/me/app-c.git"},
		sugSpec{key: "x1", remote: "mem-client-xcorp-web", kind: "client", client: "xcorp", url: "https://github.com/xcorp/web.git",
			files: map[string]string{"go.mod": "module m\nrequire github.com/xcorp/api v1.0.0\n"}},
		sugSpec{key: "x2", remote: "mem-client-xcorp-api", kind: "client", client: "xcorp", url: "https://github.com/xcorp/api.git"},
		sugSpec{key: "y", remote: "mem-client-ycorp", kind: "client", url: "https://github.com/ycorp/site.git"})
}

func TestSuggest_ScopedToTheOwningRepo(t *testing.T) {
	h := fiveRepoHost(t)
	h.coOpen(t, "a", "c")
	h.coOpen(t, "a", "x1")
	h.coOpen(t, "x1", "x2")
	h.coOpen(t, "x1", "y")
	// a project session: its own pair only; client pairs are neither shown nor counted
	_, out, _ := suggestRaw("--project", h.dir["a"])
	if !strings.Contains(out, "mem-project-a -> mem-project-c") || strings.Contains(out, "xcorp") || strings.Contains(out, "ycorp") ||
		strings.Contains(out, "withheld") || !strings.Contains(out, "1 suggestions") {
		t.Fatalf("project viewer:\n%s", out)
	}
	// a client-x session: client-x pairs only; the project pair is counted, the other client is not
	_, out, _ = suggestRaw("--project", h.dir["x1"])
	if !strings.Contains(out, "xcorp") || strings.Contains(out, "ycorp") || strings.Contains(out, "mem-project-c") ||
		!strings.Contains(out, "1 suggestions; 1 withheld by policy") {
		t.Fatalf("client viewer:\n%s", out)
	}
	if !strings.Contains(out, "go.mod names github.com/xcorp/api") {
		t.Fatalf("same-client module path should show:\n%s", out)
	}
	// no project and no --all: refused
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if code, _, _ := suggestRaw(); code != 2 {
		t.Fatal("suggest without a project listed the host")
	}
	// the environment variable stands in for --project
	t.Setenv("CLAUDE_PROJECT_DIR", h.dir["a"])
	if _, out, _ := suggestRaw(); !strings.Contains(out, "mem-project-a -> mem-project-c") {
		t.Fatalf("env project:\n%s", out)
	}
	// host-wide shows every pair and counts the refused ones
	_, all, _ := suggestRaw("--all")
	if strings.Contains(all, "ycorp") || !strings.Contains(all, "2 withheld by policy") {
		t.Fatalf("--all:\n%s", all)
	}
}

func TestSuggest_ClientEvidenceIsRedactedForOtherViewers(t *testing.T) {
	h := fiveRepoHost(t)
	h.coOpen(t, "x1", "x2")
	os.WriteFile(filepath.Join(h.cfgs["x1"].SyncDir, "MEMORY.md"), []byte("# Sprocket calibration\n# Widget tolerance\n"), 0o600)
	os.WriteFile(filepath.Join(h.cfgs["x2"].SyncDir, "MEMORY.md"), []byte("# Sprocket calibration widget\n# Widget tolerance\n"), 0o600)
	_, out, _ := suggestRaw("--project", h.dir["x2"])
	if strings.Contains(out, "sprocket") || strings.Contains(out, "widget") || !strings.Contains(out, "shared terms") {
		t.Fatalf("client vocabulary should be a count only:\n%s", out)
	}
	_, all, _ := suggestRaw("--all")
	if !strings.Contains(all, "sprocket") {
		t.Fatalf("host-wide listing keeps the words:\n%s", all)
	}
}

func TestSuggest_DenylistOnEitherEndAndTheViewer(t *testing.T) {
	h := fiveRepoHost(t)
	h.coOpen(t, "a", "c")
	// the To end's denylist (a -> c)
	os.WriteFile(h.cfgs["c"].DenylistPath(), []byte("project-a\n"), 0o600)
	if _, out, _ := suggest(); strings.Contains(out, "project-a") || !strings.Contains(out, "1 hidden by denylist") {
		t.Fatalf("To-end denylist ignored:\n%s", out)
	}
	os.Remove(h.cfgs["c"].DenylistPath())
	// the viewing repo's own denylist, naming only the other end
	os.WriteFile(h.cfgs["a"].DenylistPath(), []byte("project-c\n"), 0o600)
	_, out, _ := suggestRaw("--project", h.dir["a"])
	if strings.Contains(out, "project-c") || !strings.Contains(out, "1 hidden by denylist") {
		t.Fatalf("viewer denylist ignored:\n%s", out)
	}
}

func TestSessionStart_NeverComputes_RefreshHonoursBudget(t *testing.T) {
	h := fiveRepoHost(t)
	h.coOpen(t, "a", "c")
	calls := 0
	orig := updateSessions
	updateSessions = func(ix *related.SessionIndex, dir, folder string, dl time.Time) ([]related.Session, bool) {
		calls++
		time.Sleep(40 * time.Millisecond)
		return orig(ix, dir, folder, dl)
	}
	defer func() { updateSessions = orig }()
	// cache miss: the session start reads nothing
	if _, out, _ := contextFor(h.dir["a"]); calls != 0 || strings.Contains(out, "suggestions") {
		t.Fatalf("session start touched the sources (%d calls):\n%s", calls, out)
	}
	// a refresh whose budget runs out stops, writes no cache, and keeps its index
	if refresh(t, 50*time.Millisecond) {
		t.Fatal("refresh finished inside a budget it cannot meet")
	}
	if calls == 0 || calls >= 5 {
		t.Fatalf("slow source called %d times", calls)
	}
	if _, err := os.Stat(suggestCachePath(os.Getenv(routesEnv))); err == nil {
		t.Fatal("an unfinished refresh wrote the cache")
	}
	// ... and a full one finishes, after which session start shows the count with no reads
	updateSessions = orig
	if !refresh(t, time.Minute) {
		t.Fatal("refresh did not finish")
	}
	calls = 0
	updateSessions = func(ix *related.SessionIndex, dir, folder string, dl time.Time) ([]related.Session, bool) {
		calls++
		return orig(ix, dir, folder, dl)
	}
	if _, out, _ := contextFor(h.dir["a"]); calls != 0 || !strings.Contains(out, "1 related-memory suggestions") {
		t.Fatalf("calls %d\n%s", calls, out)
	}
}

func TestSuggestCache_HoldsNoEvidenceAndRechecksPolicy(t *testing.T) {
	h := fiveRepoHost(t)
	h.coOpen(t, "a", "c")
	h.coOpen(t, "a", "x1") // refused: never cached
	os.WriteFile(filepath.Join(h.cfgs["a"].SyncDir, "MEMORY.md"), []byte("# Sprocket calibration\n# Widget tolerance\n"), 0o600)
	os.WriteFile(filepath.Join(h.cfgs["c"].SyncDir, "MEMORY.md"), []byte("# Sprocket widget\n"), 0o600)
	refresh(t, 0)
	b, err := os.ReadFile(suggestCachePath(os.Getenv(routesEnv)))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"sprocket", "widget", "github.com", "go.mod", "co-opened"} {
		if strings.Contains(strings.ToLower(string(b)), bad) {
			t.Fatalf("cache holds %q: %s", bad, b)
		}
	}
	if strings.Contains(string(b), `"From":"mem-project-a","To":"mem-client`) || strings.Contains(string(b), `"From":"mem-client-xcorp-web","To":"mem-project-a"`) {
		t.Fatalf("a refused pair was cached: %s", b)
	}
	// policy is checked again on read: a cache edited to hold a refused pair gives no count
	forged := `{"At":"` + time.Now().Format(time.RFC3339) + `","Cands":[{"From":"mem-project-a","To":"mem-client-xcorp-web","Mode":"read","Score":99,"Signals":[{"Kind":"co-open"}],"Directed":false}]}`
	os.WriteFile(suggestCachePath(os.Getenv(routesEnv)), []byte(forged), 0o600)
	if _, out, _ := contextFor(h.dir["a"]); strings.Contains(out, "suggestions") {
		t.Fatalf("forged cache counted:\n%s", out)
	}
}
