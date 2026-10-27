package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/related"
)

// relHost is a fake host with a registry repo, an owning project repo and the
// routes.json that ties them to the related root.
type relHost struct {
	cfgs  map[string]config.Config
	root  string // relatedRoot
	home  string
	remot map[string]string
}

// newRelHost builds the host. keys maps routes.json key -> remote name+kind.
func newRelHost(t *testing.T, own map[string][2]string, remotes map[string]string) *relHost {
	t.Helper()
	skipCloneURLCheck = true // local bare repositories stand in for GitHub
	t.Cleanup(func() { skipCloneURLCheck = false })
	keys := []string{"registry"}
	for k := range own {
		keys = append(keys, k)
	}
	routes := []map[string]string{{"folder": "-proj-x", "repo": firstKey(own)}}
	for k := range own {
		routes = append(routes, map[string]string{"folder": "-owner-" + k, "repo": k})
	}
	cfgs := routedHost(t, keys, "", routes)
	home, _ := os.UserHomeDir()
	// Clone paths are reported resolved; on macOS the temp HOME sits under the
	// /var -> /private/var symlink, so build the expected root the same way.
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	p := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &rj); err != nil {
		t.Fatal(err)
	}
	repos := rj["repos"].(map[string]any)
	repos["registry"].(map[string]any)["remote"] = "mem-registry"
	for k, v := range own {
		e := repos[k].(map[string]any)
		e["remote"], e["kind"] = v[0], v[1]
	}
	h := &relHost{cfgs: cfgs, home: home, root: filepath.Join(home, ".claudesync.d", "related"), remot: remotes}
	rj["registry"] = cfgs["registry"].SyncDir
	rj["relatedRoot"] = h.root
	rj["relatedRemotes"] = remotes
	b, _ = json.Marshal(rj)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return h
}

func firstKey(m map[string][2]string) string {
	best := ""
	for k := range m {
		if best == "" || k < best {
			best = k
		}
	}
	return best
}

func bareRemote(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	bare := filepath.Join(root, name+".git")
	if _, err := gitwt.New(root).Run("init", "--bare", "-b", "main", bare); err != nil {
		t.Fatal(err)
	}
	pushToBare(t, bare, files)
	return bare
}

func pushToBare(t *testing.T, bare string, files map[string]string) {
	t.Helper()
	work := t.TempDir()
	w := gitwt.New(work)
	if err := w.Clone(bare); err != nil {
		t.Fatal(err)
	}
	w.Run("checkout", "-B", "main")
	for rel, body := range files {
		p := filepath.Join(work, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte(body), 0600)
	}
	w.AddAll()
	if _, err := w.Commit("notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
}

func run(f func(o, e *bytes.Buffer) int) (code int, out, errs string) {
	var o, e bytes.Buffer
	code = f(&o, &e)
	return code, o.String(), e.String()
}

func relate(args ...string) (int, string, string) {
	return run(func(o, e *bytes.Buffer) int { return relateCmd(o, e, args) })
}
func relatedC(args ...string) (int, string, string) {
	return run(func(o, e *bytes.Buffer) int { return relatedCmd(o, e, args) })
}

func TestRelate_RecordsListsAndCommits(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatalf("relate: %d %s", code, errs)
	}
	if code, _, errs := relate("--join", "mem-project-a", "mem-project-c"); code != 0 { // flags first also work
		t.Fatalf("relate join: %d %s", code, errs)
	}
	out, _ := gitwt.New(h.cfgs["registry"].SyncDir).Run("log", "--format=%s")
	if strings.Count(out, "relate:") != 2 {
		t.Fatalf("registry commits:\n%s", out)
	}
	code, list, _ := relatedC("mem-project-a")
	if code != 0 || !strings.Contains(list, "mem-project-a -> mem-project-b (read): not fetched") ||
		!strings.Contains(list, "mem-project-c (join): join needed") {
		t.Fatalf("related list:\n%s", list)
	}
	if _, list, _ := relatedC("mem-project-zzz"); !strings.Contains(list, "no relations") {
		t.Fatalf("filter:\n%s", list)
	}
	if code, _, _ := run(func(o, e *bytes.Buffer) int { return unrelateCmd(o, e, []string{"mem-project-a", "mem-project-b"}) }); code != 0 {
		t.Fatal("unrelate failed")
	}
	if _, list, _ := relatedC(); strings.Contains(list, "mem-project-b") {
		t.Fatalf("still listed:\n%s", list)
	}
	if code, _, _ := run(func(o, e *bytes.Buffer) int { return unrelateCmd(o, e, []string{"mem-project-a", "mem-project-b"}) }); code == 0 {
		t.Fatal("unrelate of a missing relation succeeded")
	}
}

func TestRelate_RefusedEdges(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	cases := [][]string{
		{"mem-project-a", "mem-client-x", "--read"},  // project -> client
		{"mem-client-x", "mem-project-a", "--read"},  // client -> own/other kind
		{"mem-client-x", "mem-client-y", "--read"},   // cross client
		{"mem-project-a", "mem-infra-e", "--join"},   // join across kinds
		{"mem-project-a", "plain-repo", "--read"},    // non-mem
		{"notes-a", "mem-project-a", "--read"},       // non-mem
		{"mem-orch-all", "mem-orch-x", "--join"},     // orch-all join
		{"mem-orch-all", "mem-client-x", "--read"},   // orch-all -> client repo
		{"mem-orch-x", "mem-orch-y", "--read"},       // orch -> orch
		{"mem-project-a", "mem-project-a", "--read"}, // self
	}
	for _, args := range cases {
		code, _, errs := relate(args...)
		if code == 0 || !strings.Contains(errs, "refused") {
			t.Errorf("%v: code %d %q, want a refusal", args, code, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(h.cfgs["registry"].SyncDir, related.FileName)); err == nil {
		t.Fatal("a refusal wrote the registry")
	}
	// usage errors
	if code, _, _ := relate("mem-project-a", "mem-project-b"); code != 2 {
		t.Fatal("mode is required")
	}
}

func TestRelate_OrchAllReadsOrchX(t *testing.T) {
	newRelHost(t, map[string][2]string{"oa": {"mem-orch-all", "orch"}}, nil)
	if code, _, errs := relate("mem-orch-all", "mem-orch-x", "--read"); code != 0 {
		t.Fatalf("orch-all -> orch-x read refused: %s", errs)
	}
}

func TestRelate_ClientFieldAllowsSameClient(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"w": {"mem-client-a-web", "client"}, "p": {"mem-client-a-api", "client"}, "o": {"mem-client-b", "client"}}, nil)
	// Without a client field the names derive "a-web" vs "a-api": different.
	if code, _, _ := relate("mem-client-a-web", "mem-client-a-api", "--read"); code == 0 {
		t.Fatal("different derived clients related")
	}
	// Declare the client explicitly.
	p := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &rj)
	repos := rj["repos"].(map[string]any)
	repos["w"].(map[string]any)["client"] = "a"
	repos["p"].(map[string]any)["client"] = "a"
	b, _ = json.Marshal(rj)
	os.WriteFile(p, b, 0600)
	if code, _, errs := relate("mem-client-a-web", "mem-client-a-api", "--read"); code != 0 {
		t.Fatalf("same client refused: %s", errs)
	}
	if code, _, _ := relate("mem-client-a-web", "mem-client-b", "--read"); code == 0 {
		t.Fatal("cross-client accepted")
	}
	_ = h
}

func TestRelated_HandEditedRegistryIsEnforcedAtLoad(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}, "c": {"mem-client-x", "client"}}, nil)
	body := `[{"repo":"mem-client-x","related":[{"repo":"mem-client-y","mode":"read"}]}]`
	os.WriteFile(filepath.Join(h.cfgs["registry"].SyncDir, related.FileName), []byte(body), 0600)
	_, list, _ := relatedC()
	if !strings.Contains(list, "refused, ignored") {
		t.Fatalf("hand-edited cross-client edge not refused:\n%s", list)
	}
	_, out, _ := run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	if !strings.Contains(out, "refused, ignored") {
		t.Fatalf("sync did not skip it:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.root, "mem-client-y")); err == nil {
		t.Fatal("refused edge was fetched")
	}
}

func TestRelatedSync_ReadCloneNotMirroredNotWatchedNotPushed(t *testing.T) {
	bare := bareRemote(t, "mem-project-b", map[string]string{"projects/p/memory/MEMORY.md": "# Notes\n", "projects/p/memory/n.md": "BODY"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatal(errs)
	}
	before, _ := gitwt.New(bare).Run("for-each-ref")
	code, out, _ := run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	if code != 0 || !strings.Contains(out, "mem-project-b: fetched, read-only at "+filepath.Join(h.root, "mem-project-b")) {
		t.Fatalf("sync: %d\n%s", code, out)
	}
	clone := filepath.Join(h.root, "mem-project-b")
	if _, err := os.Stat(filepath.Join(clone, "projects/p/memory/n.md")); err != nil {
		t.Fatal("no clone")
	}
	// Not mirrored: nothing in ~/.claude/projects, and the clone is in no sync dir.
	claude := h.cfgs["a"].ClaudeProjectsDir
	if ents, _ := os.ReadDir(claude); len(ents) != 0 {
		t.Fatalf("read clone was mirrored into %s: %v", claude, ents)
	}
	for k, cfg := range h.cfgs {
		if inside(clone, cfg.SyncDir) {
			t.Fatalf("clone inside the sync dir of %s", k)
		}
	}
	// Not watched: the daemon runs a loop per routes.json repo, and the clone is not one.
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	for k, cfg := range set.Cfgs {
		if cfg.SyncDir == clone {
			t.Fatalf("read clone became repo %s", k)
		}
	}
	if _, ok := set.localKey("mem-project-b"); ok {
		t.Fatal("a read relation became a local repo")
	}
	// Never pushed, though the remote is writable.
	after, _ := gitwt.New(bare).Run("for-each-ref")
	if before != after {
		t.Fatal("the remote changed")
	}
	if _, err := gitwt.New(clone).Run("push", "origin", "HEAD:main"); err == nil {
		t.Fatal("manual push worked")
	}
}

func TestRelatedRoot_InsideSyncDirOrClaudeIsRefused(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(h.cfgs["a"].SyncDir, link); err != nil {
		t.Skip("no symlinks")
	}
	for _, bad := range []string{
		filepath.Join(link, "related"),         // a symlink into a sync dir
		filepath.Join(h.home, ".claudesync.d"), // contains the sync dirs
		h.home,                                 // contains everything
		filepath.Join(h.cfgs["a"].SyncDir, "related"),
		filepath.Join(h.home, ".claude", "projects", "related"),
		h.cfgs["a"].ClaudeProjectsDir,
	} {
		p := os.Getenv(routesEnv)
		var rj map[string]any
		b, _ := os.ReadFile(p)
		json.Unmarshal(b, &rj)
		rj["relatedRoot"] = bad
		b, _ = json.Marshal(rj)
		os.WriteFile(p, b, 0600)
		if _, err := loadRelCtx(""); err == nil {
			t.Errorf("relatedRoot %s accepted", bad)
		}
	}
}

func TestRelatedSync_JoinPrintsStepsAndFetchesNothing(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	relate("mem-project-a", "mem-project-b", "--join")
	_, out, _ := run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	if !strings.Contains(out, "full member of mem-project-b") || !strings.Contains(out, "claude-memsync init") {
		t.Fatalf("no join steps:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.root, "mem-project-b")); err == nil {
		t.Fatal("join fetched a clone")
	}
}

func TestRelatedSync_MissingURLAndFetchFailureAreReported(t *testing.T) {
	newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-c": filepath.Join(t.TempDir(), "mem-project-c.git")})
	relate("mem-project-a", "mem-project-b", "--read")
	relate("mem-project-a", "mem-project-c", "--read")
	_, out, _ := run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	if !strings.Contains(out, "mem-project-b: no URL") || !strings.Contains(out, "mem-project-c: fetch failed") {
		t.Fatalf("out:\n%s", out)
	}
}

func contextFor(project string, extra ...string) (int, string, string) {
	return run(func(o, e *bytes.Buffer) int {
		return relatedContextCmd(o, e, append([]string{"--project", project}, extra...))
	})
}

func TestContext_IndexAndBudgets(t *testing.T) {
	var heads strings.Builder
	for i := 0; i < 60; i++ {
		heads.WriteString("# Heading " + strings.Repeat("h", 30) + "\n")
	}
	bare := bareRemote(t, "mem-project-b", map[string]string{
		"projects/p/memory/MEMORY.md": heads.String() + "- [n](n.md) — BODYHOOK\n",
		"projects/p/memory/n.md":      "SECRETBODY", "projects/p/memory/m.md": "SECRETBODY",
	})
	bare2 := bareRemote(t, "mem-project-c", map[string]string{"MEMORY.md": "# Second\n", "x.md": "y"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}},
		map[string]string{"mem-project-b": bare, "mem-project-c": bare2})
	relate("mem-project-a", "mem-project-b", "--read")
	relate("mem-project-a", "mem-project-c", "--read")
	run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })

	// The project is routed by directory (escaped like Claude does) ...
	code, out, errs := contextFor("/proj/x")
	if code != 0 || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	for _, want := range []string{"mem-project-b, 2 notes, read-only at " + filepath.Join(h.root, "mem-project-b"), "# Heading", "mem-project-c, 1 note", "# Second"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"SECRETBODY", "BODYHOOK"} {
		if strings.Contains(out, bad) {
			t.Errorf("leaks %s", bad)
		}
	}
	// ... or by folder name; budgets bind.
	_, small, _ := contextFor("-proj-x", "--per-relation", "200", "--total", "260")
	if !strings.Contains(small, "…") || strings.Contains(small, "# Second") || !strings.Contains(small, "left out: context budget") {
		t.Errorf("budgets not applied:\n%s", small)
	}
	if len([]rune(small)) > 260+130 {
		t.Errorf("total budget exceeded: %d", len([]rune(small)))
	}
	// A project no repo owns, or a repo with no relations, prints nothing.
	if _, none, _ := contextFor("/elsewhere"); none != "" {
		t.Errorf("unrouted project got context:\n%s", none)
	}
	// A broken invocation never blocks the session.
	if code, _, errs := contextFor("/proj/x", "--total", "0"); code != 0 || errs == "" {
		t.Errorf("bad budget: %d %q", code, errs)
	}
}

func TestContext_EncryptedCloneNotReadable(t *testing.T) {
	bare := bareRemote(t, "mem-project-b", map[string]string{related.EncryptedMarker: "age1abc\n", "MEMORY.md": "# CIPHERTEXT-HEADING\n", "n.md": "x"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	relate("mem-project-a", "mem-project-b", "--read")
	_, out, _ := run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	if !strings.Contains(out, "not readable on this host") {
		t.Fatalf("sync:\n%s", out)
	}
	_, ctx, _ := contextFor("/proj/x")
	if !strings.Contains(ctx, "mem-project-b: not readable on this host") || strings.Contains(ctx, "CIPHERTEXT") || strings.Contains(ctx, h.root) {
		t.Fatalf("context:\n%s", ctx)
	}
	if _, list, _ := relatedC(); !strings.Contains(list, "not readable on this host") {
		t.Fatalf("list:\n%s", list)
	}
	// With a decrypting opener the clone is readable.
	old := relatedOpener
	defer func() { relatedOpener = old }()
	relatedOpener = allowOpener{}
	_, ctx, _ = contextFor("/proj/x")
	if !strings.Contains(ctx, "CIPHERTEXT-HEADING") {
		t.Fatalf("hook opener ignored:\n%s", ctx)
	}
}

type allowOpener struct{}

func (allowOpener) Readable(string) bool              { return true }
func (allowOpener) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

func TestHook_ListsChangesInRelatedReadClones(t *testing.T) {
	bare := bareRemote(t, "mem-project-b", map[string]string{"projects/p/memory/n.md": "one"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	relate("mem-project-a", "mem-project-b", "--read")
	run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })

	hook := func() (int, string, string) {
		return runHook(t, hookJSON("sess1", "/proj/x", "UserPromptSubmit"), "--hook", "--project", "/proj/x")
	}
	// The first look only sets the baseline.
	if code, out, errs := hook(); code != 0 || out != "" {
		t.Fatalf("baseline: %d %q %q", code, out, errs)
	}
	pushToBare(t, bare, map[string]string{"projects/p/memory/new.md": "SECRETBODY"})
	// Nothing is reported until the daemon (or sync) fetches.
	if _, out, _ := hook(); out != "" {
		t.Fatalf("reported before fetch: %q", out)
	}
	run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	code, out, errs := hook()
	if code != 0 || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	var reply hookReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	ctx := reply.HookSpecificOutput.AdditionalContext
	want := "added " + filepath.Join(h.root, "mem-project-b", "projects", "p", "memory", "new.md") + " (related mem-project-b, read-only)"
	if !strings.Contains(ctx, want) || !strings.Contains(ctx, "related, read-only repos") || strings.Contains(ctx, "SECRETBODY") {
		t.Fatalf("context:\n%s\nwant %s", ctx, want)
	}
	if strings.Contains(ctx, framingLine) {
		t.Fatalf("own-repo framing printed with no own changes:\n%s", ctx)
	}
	// Reported once.
	if _, out, _ := hook(); out != "" {
		t.Fatalf("repeated: %q", out)
	}
}

// ---- review round 2 ----

func syncOut() string {
	_, out, _ := run(func(o, e *bytes.Buffer) int { return relatedSyncCmd(o, e, nil) })
	return out
}

func TestRelatedSync_URLMustNameTheRepo(t *testing.T) {
	other := bareRemote(t, "mem-client-x", map[string]string{"a.md": "secret"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": other})
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatal(errs)
	}
	out := syncOut()
	if !strings.Contains(out, "mem-project-b: refused") {
		t.Fatalf("a project relation with a client URL was not refused:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.root, "mem-project-b")); err == nil {
		t.Fatal("fetched")
	}
}

// Two owners on one host: the other owner's relations never appear.
func TestOwnerFilter_ContextAndHook(t *testing.T) {
	bb := bareRemote(t, "mem-project-b", map[string]string{"MEMORY.md": "# B-HEADING\n", "n.md": "x"})
	bc := bareRemote(t, "mem-project-c", map[string]string{"MEMORY.md": "# C-HEADING\n", "n.md": "x"})
	h := newRelHost(t, map[string][2]string{"p": {"mem-project-p", "project"}, "q": {"mem-project-q", "project"}},
		map[string]string{"mem-project-b": bb, "mem-project-c": bc})
	relate("mem-project-p", "mem-project-b", "--read")
	relate("mem-project-q", "mem-project-c", "--read")
	syncOut()
	_, ctxQ, _ := contextFor("/owner/q")
	if !strings.Contains(ctxQ, "mem-project-c") || strings.Contains(ctxQ, "mem-project-b") || strings.Contains(ctxQ, "B-HEADING") {
		t.Fatalf("Q's context:\n%s", ctxQ)
	}
	_, ctxP, _ := contextFor("/owner/p")
	if !strings.Contains(ctxP, "mem-project-b") || strings.Contains(ctxP, "mem-project-c") {
		t.Fatalf("P's context:\n%s", ctxP)
	}
	hook := func(proj string) string {
		_, out, _ := runHook(t, hookJSON("s-"+strings.ReplaceAll(proj, "/", ""), proj, "UserPromptSubmit"), "--hook", "--project", proj)
		return out
	}
	hook("/owner/q")
	pushToBare(t, bb, map[string]string{"new-b.md": "x"})
	pushToBare(t, bc, map[string]string{"new-c.md": "x"})
	syncOut()
	out := hook("/owner/q")
	if !strings.Contains(out, "new-c.md") || strings.Contains(out, "new-b.md") || strings.Contains(out, "mem-project-b") {
		t.Fatalf("Q's hook output:\n%s", out)
	}
	_ = h
}

func TestSync_OnlyFetchesForReposThisHostHas(t *testing.T) {
	bare := bareRemote(t, "mem-project-b", map[string]string{"a.md": "x"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	relate("mem-project-zzz", "mem-project-b", "--read") // an edge of a repo this host does not have
	syncOut()
	if _, err := os.Stat(filepath.Join(h.root, "mem-project-b")); err == nil {
		t.Fatal("fetched for a repo this host does not have")
	}
}

func TestJoinedRelationHasNoReadClone(t *testing.T) {
	bare := bareRemote(t, "mem-project-j", map[string]string{"MEMORY.md": "# J\n", "a.md": "x"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}, "j": {"mem-project-j", "project"}}, map[string]string{"mem-project-j": bare})
	relate("mem-project-a", "mem-project-j", "--read")
	out := syncOut()
	if !strings.Contains(out, "mem-project-j: already joined") {
		t.Fatalf("sync:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.root, "mem-project-j")); err == nil {
		t.Fatal("cloned a joined repo")
	}
	if _, ctx, _ := contextFor("/owner/a"); ctx != "" {
		t.Fatalf("a joined repo is already in ~/.claude; context should be empty:\n%s", ctx)
	}
}

func TestAccept_RegistryEdgesNeedALocalAccept(t *testing.T) {
	bare := bareRemote(t, "mem-project-b", map[string]string{"MEMORY.md": "# B\n", "n.md": "x"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	// Another machine's edge arrives through the registry repo.
	body := `[{"repo":"mem-project-a","related":[{"repo":"mem-project-b","mode":"read"}]}]`
	os.WriteFile(filepath.Join(h.cfgs["registry"].SyncDir, related.FileName), []byte(body), 0600)
	_, list, _ := relatedC()
	if !strings.Contains(list, "not accepted on this host") {
		t.Fatalf("list:\n%s", list)
	}
	if out := syncOut(); !strings.Contains(out, "not accepted") {
		t.Fatalf("sync:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.root, "mem-project-b")); err == nil {
		t.Fatal("an unaccepted edge was fetched")
	}
	if _, ctx, _ := contextFor("/owner/a"); ctx != "" {
		t.Fatalf("an unaccepted edge reached the context:\n%s", ctx)
	}
	accept := func(args ...string) string {
		_, out, _ := run(func(o, e *bytes.Buffer) int { return relatedAcceptCmd(o, e, args) })
		return out
	}
	if out := accept(); !strings.Contains(out, "+ mem-project-a -> mem-project-b (read)") || !strings.Contains(out, "--yes") {
		t.Fatalf("diff:\n%s", out)
	}
	if out := syncOut(); strings.Contains(out, "fetched") {
		t.Fatal("showing the diff accepted it")
	}
	accept("--yes")
	if out := syncOut(); !strings.Contains(out, "mem-project-b: fetched") {
		t.Fatalf("after accept:\n%s", out)
	}
	if out := accept(); !strings.Contains(out, "nothing to accept") {
		t.Fatalf("second accept:\n%s", out)
	}
	// A mode change is a new edge to accept; a removed one is shown.
	os.WriteFile(filepath.Join(h.cfgs["registry"].SyncDir, related.FileName),
		[]byte(`[{"repo":"mem-project-a","related":[{"repo":"mem-project-b","mode":"join"}]}]`), 0600)
	if out := accept(); !strings.Contains(out, "(join) (was read)") {
		t.Fatalf("mode change:\n%s", out)
	}
	os.WriteFile(filepath.Join(h.cfgs["registry"].SyncDir, related.FileName), []byte(`[]`), 0600)
	if out := accept(); !strings.Contains(out, "- mem-project-a -> mem-project-b (read) no longer in the registry") {
		t.Fatalf("removed:\n%s", out)
	}
	accept("--yes")
	if out := accept(); !strings.Contains(out, "nothing to accept") {
		t.Fatalf("pin not pruned:\n%s", out)
	}
}

func TestDenylist_HidesRelatedHeadingsAndHookPaths(t *testing.T) {
	bare := bareRemote(t, "mem-project-b", map[string]string{"MEMORY.md": "# Public plan\n# ACME pricing\n", "n.md": "x"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	os.WriteFile(h.cfgs["a"].DenylistPath(), []byte("acme\n"), 0600)
	relate("mem-project-a", "mem-project-b", "--read")
	syncOut()
	_, ctx, _ := contextFor("/owner/a")
	if strings.Contains(strings.ToLower(ctx), "acme") || !strings.Contains(ctx, "# Public plan") || !strings.Contains(ctx, "1 heading(s) hidden") {
		t.Fatalf("context:\n%s", ctx)
	}
	hook := func() string {
		_, out, _ := runHook(t, hookJSON("sd", "/owner/a", "UserPromptSubmit"), "--hook", "--project", "/owner/a")
		return out
	}
	hook()
	pushToBare(t, bare, map[string]string{"acme-note.md": "x", "fine.md": "x"})
	syncOut()
	out := hook()
	if strings.Contains(strings.ToLower(out), "acme") || !strings.Contains(out, "fine.md") || !strings.Contains(out, "1 changed file(s) in related repos") {
		t.Fatalf("hook:\n%s", out)
	}
	// An unreadable denylist hides everything.
	os.WriteFile(h.cfgs["a"].DenylistPath(), []byte("(\n"), 0600)
	_, ctx, _ = contextFor("/owner/a")
	if strings.Contains(ctx, "Public plan") {
		t.Fatalf("headings printed with an unusable denylist:\n%s", ctx)
	}
}

func TestStale_FailedFetchIsShownAndSyncFlagClears(t *testing.T) {
	bare := bareRemote(t, "mem-project-b", map[string]string{"MEMORY.md": "# B\n", "n.md": "x"})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	relate("mem-project-a", "mem-project-b", "--read")
	syncOut()
	os.RemoveAll(bare)
	out := syncOut()
	if !strings.Contains(out, "fetch failed") || !strings.Contains(out, "stale since 20") {
		t.Fatalf("sync:\n%s", out)
	}
	_, ctx, _ := contextFor("/owner/a")
	if !strings.Contains(ctx, ", stale since 20") {
		t.Fatalf("context:\n%s", ctx)
	}
	if _, list, _ := relatedC(); !strings.Contains(list, "stale since 20") {
		t.Fatalf("list:\n%s", list)
	}
	_ = h
}

func TestBackgroundSync_TimeoutClearsTheGuard(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fakessh")
	os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0700)
	t.Setenv("GIT_SSH_COMMAND", script)
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}},
		map[string]string{"mem-project-b": "git@alias:org/mem-project-b.git"})
	p := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &rj)
	rj["relatedFetchTimeoutSec"] = 1
	b, _ = json.Marshal(rj)
	os.WriteFile(p, b, 0600)
	relate("mem-project-a", "mem-project-b", "--read")
	set, err := loadRouted(p)
	if err != nil {
		t.Fatal(err)
	}
	set.syncRelatedInBackground()
	deadline := time.Now().Add(20 * time.Second)
	for relatedSyncing.Load() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if relatedSyncing.Load() {
		t.Fatal("a hung fetch kept the guard set")
	}
	if _, err := os.Stat(filepath.Join(h.root, ".state", "failed", "mem-project-b")); err != nil {
		t.Fatal("timeout not recorded as stale")
	}
}

func TestHider_ByPathAndByContentScan(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	os.WriteFile(h.cfgs["a"].DenylistPath(), []byte("^secret-dir/\n"), 0600)
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	hide := rc.hider("mem-project-a")
	// By path: the denylist pattern names the path.
	if !hide("secret-dir/n.md") || hide("public-dir/n.md") {
		t.Fatal("path hiding wrong")
	}
	// By content: a credential in a heading is hidden by the built-in scan.
	if !hide(`# deploy token = "abcdefghijklmnopqrstuvwx"`) || hide("# deploy notes") {
		t.Fatal("content scan wrong")
	}
	// With no denylist at all the content scan still applies.
	os.Remove(h.cfgs["a"].DenylistPath())
	hide = rc.hider("mem-project-a")
	if !hide(`# api_key = "abcdefghijklmnopqrstuvwx"`) || hide("secret-dir/n.md") {
		t.Fatal("no-denylist hiding wrong")
	}
}

func TestSync_SizeCapIsConfigurable(t *testing.T) {
	big := strings.Repeat("x", 1<<20+10)
	bare := bareRemote(t, "mem-project-b", map[string]string{"a.md": big})
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, map[string]string{"mem-project-b": bare})
	p := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &rj)
	rj["relatedMaxMB"] = 1
	b, _ = json.Marshal(rj)
	os.WriteFile(p, b, 0600)
	relate("mem-project-a", "mem-project-b", "--read")
	if out := syncOut(); !strings.Contains(out, "limit") {
		t.Fatalf("sync:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.root, "mem-project-b")); err == nil {
		t.Fatal("oversize tip exported")
	}
}
