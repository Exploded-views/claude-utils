package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/MarimerLLC/claude-utils/internal/related"
	"github.com/MarimerLLC/claude-utils/internal/route"
)

// relatedVisibility is the private-only check applied to both ends of every
// relation. PLACEHOLDER: the default checks names only; the branch that
// provides the real check replaces this.
var relatedVisibility related.Visibility = related.NameOnly{}

// relatedOpener reads the related clones. PLACEHOLDER for decryption: the
// default reads plaintext only, so an encrypted clone is "not readable on this
// host". The age branch supplies an Opener that can decrypt.
var relatedOpener related.Opener = related.PlainOpener{}

// relCtx is routes.json plus its registry.
type relCtx struct {
	set     *routedSet
	reg     *related.Registry
	checker related.Checker
	root    string
}

// remoteName is the repo name relations use for the routes.json entry key:
// its "remote" field, else the basename of its config's remote URL, else the key.
func (s *routedSet) remoteName(key string) string {
	if r := s.File.Repos[key].Remote; r != "" {
		return r
	}
	if cfg, ok := s.Cfgs[key]; ok && cfg.RemoteURL != "" {
		base := strings.TrimSuffix(strings.TrimRight(cfg.RemoteURL, "/"), ".git")
		if i := strings.LastIndexAny(base, "/:\\"); i >= 0 {
			base = base[i+1:]
		}
		if base != "" {
			return strings.ToLower(base)
		}
	}
	return key
}

// localKey finds the routes.json key of the repo with this remote name.
func (s *routedSet) localKey(remote string) (string, bool) {
	for _, k := range s.File.RepoNames() {
		if s.remoteName(k) == remote {
			return k, true
		}
	}
	return "", false
}

// lookupNode feeds the policy the kind and client routes.json declares.
func (s *routedSet) lookupNode(remote string) (related.Node, bool) {
	k, ok := s.localKey(remote)
	if !ok {
		return related.Node{}, false
	}
	r := s.File.Repos[k]
	return related.Node{Name: remote, Kind: r.Kind, Client: r.Client}, true
}

func loadRelCtx(routesFlag string) (*relCtx, error) {
	path, ok, err := resolveRoutesPath(routesFlag)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("related projects need a routes file (--routes, $%s or ~/.config/claude-memsync/routes.json)", routesEnv)
	}
	set, err := loadRouted(path)
	if err != nil {
		return nil, err
	}
	if set.File.Registry == "" {
		return nil, fmt.Errorf("%s has no \"registry\" (the directory of the registry repo)", path)
	}
	regDir := route.ExpandHome(set.File.Registry)
	reg, err := related.LoadRegistry(regDir)
	if err != nil {
		return nil, err
	}
	rc := &relCtx{set: set, reg: reg, checker: related.Checker{Lookup: set.lookupNode, Visible: relatedVisibility}}
	rc.root = route.ExpandHome(set.File.RelatedRoot)
	if rc.root == "" {
		rc.root = filepath.Join(filepath.Dir(filepath.Clean(regDir)), "related")
	}
	if err := rc.checkRoot(); err != nil {
		return nil, err
	}
	return rc, nil
}

// checkRoot keeps the related root out of every tree the daemon mirrors or
// watches: a read clone must never be inside a sync dir or ~/.claude.
func (rc *relCtx) checkRoot() error {
	root, err := filepath.Abs(rc.root)
	if err != nil {
		return err
	}
	rc.root = root
	var guarded []string
	for _, cfg := range rc.set.Cfgs {
		guarded = append(guarded, cfg.SyncDir, cfg.ClaudeProjectsDir)
	}
	guarded = append(guarded, route.ExpandHome(rc.set.File.Registry))
	home, _ := os.UserHomeDir()
	if home != "" {
		guarded = append(guarded, filepath.Join(home, ".claude"))
	}
	for _, g := range guarded {
		if g == "" {
			continue
		}
		ga, err := filepath.Abs(g)
		if err != nil {
			continue
		}
		if inside(root, ga) || inside(ga, root) {
			return fmt.Errorf("relatedRoot %s overlaps %s, which is synced or read by Claude; read clones must live elsewhere", root, ga)
		}
	}
	return nil
}

// inside reports whether p is dir or below it.
func inside(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// parseInterspersed parses flags that may come after positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos, nil
}

func runRelate(args []string) int { return relateCmd(os.Stdout, os.Stderr, args) }

func relateCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("relate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routesFlag := fs.String("routes", "", "path to routes.json")
	read := fs.Bool("read", false, "read-only fetch clone")
	join := fs.Bool("join", false, "this host must join the related repo fully")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 || *read == *join {
		fmt.Fprintln(stderr, "usage: claude-memsync relate <repo> <related-repo> --read|--join [--routes FILE]")
		return 2
	}
	mode := related.ModeRead
	if *join {
		mode = related.ModeJoin
	}
	rc, err := loadRelCtx(*routesFlag)
	if err != nil {
		fmt.Fprintln(stderr, "relate:", err)
		return 1
	}
	if err := rc.reg.Relate(rc.checker, pos[0], pos[1], mode); err != nil {
		fmt.Fprintln(stderr, "relate: refused:", err)
		return 1
	}
	if err := rc.reg.Save(fmt.Sprintf("relate: %s -> %s (%s)", pos[0], pos[1], mode)); err != nil {
		fmt.Fprintln(stderr, "relate:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s -> %s (%s) recorded in the registry\n", pos[0], pos[1], mode)
	return 0
}

func runUnrelate(args []string) int { return unrelateCmd(os.Stdout, os.Stderr, args) }

func unrelateCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("unrelate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routesFlag := fs.String("routes", "", "path to routes.json")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "usage: claude-memsync unrelate <repo> <related-repo> [--routes FILE]")
		return 2
	}
	rc, err := loadRelCtx(*routesFlag)
	if err != nil {
		fmt.Fprintln(stderr, "unrelate:", err)
		return 1
	}
	if !rc.reg.Unrelate(pos[0], pos[1]) {
		fmt.Fprintf(stderr, "unrelate: no relation %s -> %s\n", pos[0], pos[1])
		return 1
	}
	if err := rc.reg.Save(fmt.Sprintf("unrelate: %s -> %s", pos[0], pos[1])); err != nil {
		fmt.Fprintln(stderr, "unrelate:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s -> %s removed from the registry (a fetched clone stays until you delete it)\n", pos[0], pos[1])
	return 0
}

func runRelated(args []string) int { return relatedCmd(os.Stdout, os.Stderr, args) }

func relatedCmd(stdout, stderr io.Writer, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "sync":
			return relatedSyncCmd(stdout, stderr, args[1:])
		case "context":
			return relatedContextCmd(stdout, stderr, args[1:])
		}
	}
	fs := flag.NewFlagSet("related", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routesFlag := fs.String("routes", "", "path to routes.json")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) > 1 {
		fmt.Fprintln(stderr, "usage: claude-memsync related [<repo>] | related sync | related context --project DIR")
		return 2
	}
	rc, err := loadRelCtx(*routesFlag)
	if err != nil {
		fmt.Fprintln(stderr, "related:", err)
		return 1
	}
	edges, refused := rc.reg.Active(rc.checker)
	n := 0
	for _, e := range edges {
		if len(pos) == 1 && e.From != pos[0] {
			continue
		}
		n++
		fmt.Fprintf(stdout, "%s -> %s (%s)%s\n", e.From, e.To, e.Mode, rc.stateNote(e))
	}
	for _, r := range refused {
		if len(pos) == 1 && r.Edge.From != pos[0] {
			continue
		}
		fmt.Fprintf(stdout, "%s -> %s (%s): refused, ignored: %v\n", r.Edge.From, r.Edge.To, r.Edge.Mode, r.Err)
	}
	if n == 0 && len(refused) == 0 {
		fmt.Fprintln(stdout, "no relations")
	}
	return 0
}

// stateNote says what this host has of an edge.
func (rc *relCtx) stateNote(e related.Edge) string {
	if _, ok := rc.set.localKey(e.To); ok {
		return ": joined on this host"
	}
	if e.Mode == related.ModeJoin {
		return ": join needed"
	}
	c, err := related.NewReadClone(rc.root, e.To, rc.set.File.RelatedRemotes[e.To])
	switch {
	case err != nil || !c.Exists():
		return ": not fetched"
	case !relatedOpener.Readable(c.Dir()):
		return ": not readable on this host"
	}
	return ": read-only clone at " + c.Dir()
}

// joinSteps is what a join relation needs; nothing is automated.
func joinSteps(e related.Edge) string {
	return fmt.Sprintf("%s -> %s is a join relation: this host must become a full member of %s. Steps: (1) claude-memsync init against %s with a write deploy key and its own sync dir, (2) add it to \"repos\" in routes.json with its kind, (3) add a route for the project folder it carries.",
		e.From, e.To, e.To, e.To)
}

// localEdges are the active edges whose owner this host has.
func (rc *relCtx) localEdges() (edges []related.Edge, refused []related.Refusal) {
	ok, refused := rc.reg.Active(rc.checker)
	for _, e := range ok {
		if _, mine := rc.set.localKey(e.From); mine {
			edges = append(edges, e)
		}
	}
	return edges, refused
}

// syncRelated fetches the read clones of every read relation of a repo this
// host has, and reports the state of each line.
func (rc *relCtx) syncRelated() []string {
	edges, refused := rc.localEdges()
	var lines []string
	for _, r := range refused {
		lines = append(lines, fmt.Sprintf("%s -> %s: refused, ignored: %v", r.Edge.From, r.Edge.To, r.Err))
	}
	done := map[string]bool{}
	for _, e := range edges {
		switch {
		case e.Mode == related.ModeJoin:
			if _, joined := rc.set.localKey(e.To); !joined {
				lines = append(lines, joinSteps(e))
			}
			continue
		case done[e.To]:
			continue
		}
		if _, joined := rc.set.localKey(e.To); joined {
			lines = append(lines, fmt.Sprintf("%s: already joined on this host, no read clone", e.To))
			continue
		}
		done[e.To] = true
		url := rc.set.File.RelatedRemotes[e.To]
		if url == "" {
			lines = append(lines, fmt.Sprintf("%s: no URL in relatedRemotes, not fetched", e.To))
			continue
		}
		c, err := related.NewReadClone(rc.root, e.To, url)
		if err == nil {
			err = c.Fetch()
		}
		switch {
		case err != nil:
			lines = append(lines, fmt.Sprintf("%s: fetch failed: %v", e.To, err))
		case !relatedOpener.Readable(c.Dir()):
			lines = append(lines, fmt.Sprintf("%s: not readable on this host (encrypted, this host cannot decrypt it)", e.To))
		default:
			lines = append(lines, fmt.Sprintf("%s: fetched, read-only at %s", e.To, c.Dir()))
		}
	}
	sort.Strings(lines)
	return lines
}

func relatedSyncCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("related sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routesFlag := fs.String("routes", "", "path to routes.json")
	if fs.Parse(args) != nil {
		return 2
	}
	rc, err := loadRelCtx(*routesFlag)
	if err != nil {
		fmt.Fprintln(stderr, "related sync:", err)
		return 1
	}
	for _, l := range rc.syncRelated() {
		fmt.Fprintln(stdout, l)
	}
	return 0
}

// relatedSyncing guards the daemon's background fetch against overlap.
var relatedSyncing atomic.Bool

// syncRelatedInBackground is the daemon's pull-interval fetch. It runs apart
// from the loops: a slow remote must not stall them or Stop.
func (s *routedSet) syncRelatedInBackground() {
	if s.File.Registry == "" || !relatedSyncing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer relatedSyncing.Store(false)
		rc, err := loadRelCtx(s.Path)
		if err != nil {
			log.Printf("related: %v", err)
			return
		}
		for _, l := range rc.syncRelated() {
			log.Printf("related: %s", l)
		}
	}()
}

// owningRepoRemote names (by remote name) the repo that owns project, which is
// a directory or Claude's folder name for one.
func (rc *relCtx) owningRepoRemote(project string) (string, bool) {
	folder := project
	if abs, err := filepath.Abs(project); err == nil && strings.ContainsAny(project, `/\`) {
		folder = route.Escape(abs)
	}
	d := rc.set.Router.Decide(folder)
	if d.Repo == "" {
		return "", false
	}
	return rc.set.remoteName(d.Repo), true
}

// readBlocks gives the read clones of the active read relations of owner.
// Relations that cannot be shown carry a Skipped reason.
func (rc *relCtx) readBlocks(owner string) []related.Block {
	ok, _ := rc.reg.Active(rc.checker)
	var out []related.Block
	for _, e := range ok {
		if e.From != owner || e.Mode != related.ModeRead {
			continue
		}
		if _, joined := rc.set.localKey(e.To); joined {
			continue // already a full member; its notes are in ~/.claude
		}
		c, err := related.NewReadClone(rc.root, e.To, "")
		bl := related.Block{Repo: e.To}
		switch {
		case err != nil || !c.Exists():
			bl.Skipped = "not fetched on this host yet"
		case !relatedOpener.Readable(c.Dir()):
			bl.Skipped = "not readable on this host"
		default:
			bl.Path = c.Dir()
			if ix, err := related.BuildIndex(c.Dir(), relatedOpener); err != nil {
				bl.Skipped = "not readable on this host"
			} else {
				bl.Index = ix
			}
		}
		out = append(out, bl)
	}
	return out
}

func relatedContextCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("related context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routesFlag := fs.String("routes", "", "path to routes.json")
	project := fs.String("project", "", "the project directory (default $CLAUDE_PROJECT_DIR)")
	per := fs.Int("per-relation", related.DefaultBudgets.PerRelation, "character budget for each related repo")
	total := fs.Int("total", related.DefaultBudgets.Total, "character budget for all of them")
	if fs.Parse(args) != nil {
		return 2
	}
	// A SessionStart hook must never block a session: every failure is quiet.
	quiet := func(err error) int {
		fmt.Fprintln(stderr, "related context:", err)
		return 0
	}
	if *per <= 0 || *total <= 0 {
		return quiet(fmt.Errorf("budgets must be positive"))
	}
	if *project == "" {
		*project = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if *project == "" {
		return quiet(fmt.Errorf("--project is required"))
	}
	rc, err := loadRelCtx(*routesFlag)
	if err != nil {
		return quiet(err)
	}
	owner, ok := rc.owningRepoRemote(*project)
	if !ok {
		return 0
	}
	fmt.Fprint(stdout, related.Render(rc.readBlocks(owner), related.Budgets{PerRelation: *per, Total: *total}))
	return 0
}

// relatedHookLines reports the files that changed in the read clones of the
// project's relations since this session last looked: paths only. It also moves
// the session's markers, which the caller commits to only after output went out.
func relatedHookLines(routesFlag, project, session string, limit int) (lines []string, commit func(), err error) {
	rc, err := loadRelCtx(routesFlag)
	if err != nil || rc.set.File.Registry == "" {
		return nil, nil, err
	}
	owner, ok := rc.owningRepoRemote(project)
	if !ok {
		return nil, nil, nil
	}
	type pending struct{ marker, head string }
	var moves []pending
	shown := 0
	for _, bl := range rc.readBlocks(owner) {
		if bl.Skipped != "" {
			continue
		}
		c, err := related.NewReadClone(rc.root, bl.Repo, "")
		if err != nil || c.Head() == "" {
			continue
		}
		marker := filepath.Join(rc.root, ".state", "hook-markers", bl.Repo, session)
		head := c.Head()
		prev, _ := os.ReadFile(marker)
		since := strings.TrimSpace(string(prev))
		moves = append(moves, pending{marker, head})
		if since == "" || since == head {
			continue // first look sets the baseline
		}
		changed, ok := c.ChangedSince(since)
		if !ok {
			continue
		}
		for _, f := range changed {
			if limit > 0 && shown >= limit {
				lines = append(lines, "… more changes in related repos not listed")
				goto done
			}
			shown++
			lines = append(lines, fmt.Sprintf("%s %s (related %s, read-only)", f.Status, quoteIfUnprintable(filepath.Join(c.Dir(), filepath.FromSlash(f.Path))), bl.Repo))
		}
	}
done:
	return lines, func() {
		for _, m := range moves {
			if os.MkdirAll(filepath.Dir(m.marker), 0700) == nil {
				_ = os.WriteFile(m.marker, []byte(m.head+"\n"), 0600)
			}
		}
	}, nil
}

func quoteIfUnprintable(s string) string {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Sprintf("%q", s)
		}
	}
	return s
}
