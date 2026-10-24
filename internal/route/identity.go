package route

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Root-commit identity: a safety net for a renamed or transferred repo.
//
// When a folder is first routed by its remote key, the checkout's root
// commit(s) are recorded beside the key and the owner. If the key later
// changes, the root commits tell whether the folder still holds the same
// project:
//
//   - the new key routes to the same repo: the record is updated, nothing else;
//   - the new key matches no rule and the root commits equal the record: the
//     folder keeps routing to the recorded repo for the grace period
//     (renameGraceDays), flagged in status until an alias is added;
//   - the new key routes to another repo: the folder is denied ("remote
//     changed repos") until the user edits routes.json, which is the consent.
//
// The fallback only ever returns a folder to the repo it was recorded in, so it
// can neither bring a folder into a client repo nor take one out of it, and it
// only applies to a folder with a record, never to a new folder.

// ruleSnap is what a rule matched when the folder was last confirmed: the
// names (patterns and aliases) it listed for a repo.
type ruleSnap struct {
	Repo  string   `json:"repo"`
	Names []string `json:"names"`
}

type identity struct {
	Key          string     `json:"key"`
	Repo         string     `json:"repo"`
	Client       bool       `json:"client,omitempty"`
	Roots        []string   `json:"roots,omitempty"`
	Rules        []ruleSnap `json:"rules,omitempty"`
	RenamedSince int64      `json:"renamedSince,omitempty"`
}

func (r *Router) identityPath() string {
	if r.StatePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(r.StatePath), "identity.json")
}

// readIDs parses identity.json. A missing file is an empty table; a file that
// does not parse is an error and must never be overwritten.
func readIDs(p string) (map[string]identity, error) {
	m := map[string]identity{}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		if err == nil {
			err = errors.New("null")
		}
		return nil, err
	}
	return m, nil
}

func (r *Router) loadIDsLocked() {
	if r.ids != nil {
		return
	}
	r.ids = map[string]identity{}
	r.idBad = ""
	if p := r.identityPath(); p != "" {
		m, err := readIDs(p)
		if err != nil {
			r.idBad = err.Error()
			r.logOnceLocked("", "idbad", "route: identity.json is unreadable (%v); folders with a record are denied until it is fixed", err)
			return
		}
		r.ids = m
	}
}

// reloadIDsLocked re-reads identity.json on the next use, so a file the user
// fixed is picked up. Without a file the records live in memory only.
func (r *Router) reloadIDsLocked() {
	if r.identityPath() != "" && len(r.dirty) == 0 {
		r.ids, r.idBad = nil, ""
		delete(r.logged, "\x00idbad")
	}
}

// IdentityProblem says why identity.json could not be read, "" when it can.
func (r *Router) IdentityProblem() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadIDsLocked()
	return r.idBad
}

// saveIDsLocked writes the folders changed here (dirty) into identity.json
// under a file lock, merged with what another process wrote meanwhile. A file
// that does not parse is left alone.
func (r *Router) saveIDsLocked() {
	p := r.identityPath()
	if p == "" || r.ReadOnly || len(r.dirty) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		r.logOnceLocked("", "idsave", "route: cannot save identity.json: %v", err)
		return
	}
	unlock, err := lockFile(p + ".lock")
	if err != nil {
		r.logOnceLocked("", "idsave", "route: cannot lock identity.json: %v", err)
		return
	}
	defer unlock()
	merged, err := readIDs(p)
	if err != nil {
		r.idBad = err.Error()
		r.logOnceLocked("", "idbad", "route: identity.json is unreadable (%v); not overwritten", err)
		return
	}
	for f := range r.dirty {
		merged[f] = r.ids[f]
	}
	if err := writeJSONAtomic(p, merged, ".identity-*.tmp"); err != nil {
		r.logOnceLocked("", "idsave", "route: cannot save identity.json: %v", err)
		return
	}
	r.ids = merged
	r.dirty = map[string]bool{}
}

// Test seams for the rename step, which can fail transiently on Windows while
// another process has the target open.
var (
	renameFn   = os.Rename
	retrySleep = time.Sleep
)

// writeJSONAtomic writes v to path through a temp file of its own (so two
// daemons never write the same one), retrying the rename, and reports failure.
func writeJSONAtomic(path string, v any, pattern string) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(f.Name())
		return errors.Join(werr, cerr)
	}
	var rerr error
	for i := 0; i < 6; i++ {
		if rerr = renameFn(f.Name(), path); rerr == nil {
			return nil
		}
		retrySleep(time.Duration(i+1) * 20 * time.Millisecond)
	}
	os.Remove(f.Name())
	return rerr
}

func snapshot(f File) []ruleSnap {
	out := make([]ruleSnap, 0, len(f.Routes))
	for _, r := range f.Routes {
		names := []string{r.kind() + ":" + r.pattern()}
		for _, a := range r.Aliases {
			names = append(names, r.kind()+":"+a)
		}
		out = append(out, ruleSnap{Repo: r.Repo, Names: names})
	}
	return out
}

// matchingName is the pattern or alias of rule that catches the folder.
func matchingName(rule Rule, key string) string {
	if rule.kind() != "remote" {
		return rule.kind() + ":" + rule.pattern()
	}
	if globMatch(rule.Remote, key) {
		return "remote:" + rule.Remote
	}
	for _, a := range rule.Aliases {
		if globMatch(a, key) {
			return "remote:" + a
		}
	}
	return ""
}

// consented reports whether the user's route edit, not an old rule, sends the
// folder to the repo of rule d.Rule: the pattern or alias that catches the new
// key was not listed for that repo when the folder was last confirmed. A move
// out of a client or orchestrator repo also needs that name to be the literal
// key, never a glob.
func consented(rec identity, f File, d Decision, key string) bool {
	if d.Rule < 0 || d.Rule >= len(f.Routes) {
		return false
	}
	rule := f.Routes[d.Rule]
	name := matchingName(rule, key)
	for _, s := range rec.Rules {
		if s.Repo == rule.Repo && slices.Contains(s.Names, name) {
			return false
		}
	}
	if rec.Client {
		if rule.kind() != "remote" || strings.ContainsAny(name, "*?") || strings.TrimPrefix(name, "remote:") != key {
			return false
		}
	}
	return true
}

// owner is the key without its last segment (host/owner, or host/group/sub).
func ownerOf(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[:i]
	}
	return key
}

// rootsIfNeeded reads root commits only when the identity logic will use them:
// to record a folder, or to check a folder whose key changed. Git runs outside
// the router's lock.
func (r *Router) rootsIfNeeded(folder string, d Decision, key string, keyOK bool) ([]string, bool) {
	if !keyOK || r.Roots == nil {
		return nil, false
	}
	r.mu.Lock()
	r.loadIDsLocked()
	rec, has := r.ids[folder]
	r.mu.Unlock()
	if !(has && rec.Key != key) && !(d.Repo != "" && (!has || len(rec.Roots) == 0)) {
		return nil, false
	}
	return r.Roots(folder)
}

// identityLocked applies the rename rules to a decision. r.mu is held.
func (r *Router) identityLocked(folder string, d Decision, key string, keyOK bool, roots []string, rootsOK bool) Decision {
	if !keyOK {
		return d
	}
	r.loadIDsLocked()
	f := r.cur()
	if r.idBad != "" {
		// Fail closed: with the records unreadable, a folder that was routed
		// before cannot be told from a renamed one.
		r.loadOwnersLocked()
		if _, was := r.owners[folder]; was && d.Repo != "" {
			return Decision{Rule: d.Rule, Reason: ReasonIdentityUnreadable}
		}
		return d
	}
	var rp *identity
	if v, ok := r.ids[folder]; ok {
		rp = &v
	}
	if !rootsOK {
		roots = nil
	}
	set := func(n identity) {
		r.ids[folder] = n
		if r.dirty == nil {
			r.dirty = map[string]bool{}
		}
		r.dirty[folder] = true
		r.saveIDsLocked()
	}
	// A different clone at this path: the roots say so before any name does.
	different := rp != nil && rp.Key != key && len(rp.Roots) > 0 && len(roots) > 0 && !slices.Equal(roots, rp.Roots)
	switch {
	case d.Repo != "":
		n := identity{Key: key, Repo: d.Repo, Client: f.Repos[d.Repo].IsClient(), Roots: roots, Rules: snapshot(f)}
		if rp != nil && !different {
			if len(n.Roots) == 0 {
				n.Roots = rp.Roots
			}
			if rp.Key != key && rp.Repo != d.Repo && !consented(*rp, f, d, key) {
				// An old rule, not the user's edit, sends the renamed remote
				// elsewhere: never auto-move.
				r.logOnceLocked(folder, "changed", "route: folder %s: remote changed repos; denied until routes.json is edited", hash8(folder))
				return Decision{Rule: d.Rule, Reason: ReasonRemoteChanged}
			}
		}
		if rp == nil || !identityEqual(*rp, n) {
			set(n)
		}
	case d.Reason == ReasonNoRoute && rp != nil && rp.Key != key:
		if different {
			r.logOnceLocked(folder, "different", "route: folder %s: %s", hash8(folder), ReasonDifferentRepo)
			return Decision{Rule: -1, Reason: ReasonDifferentRepo}
		}
		rec := *rp
		target, listed := f.Repos[rec.Repo]
		if !listed || rec.Repo == f.Personal || r.have == nil || !r.have(rec.Repo) ||
			target.IsClient() != rec.Client || len(rec.Roots) == 0 || !slices.Equal(roots, rec.Roots) {
			return d
		}
		if rec.Client && ownerOf(rec.Key) != ownerOf(key) {
			r.logOnceLocked(folder, "owner", "route: folder %s: %s", hash8(folder), ReasonOwnerChanged)
			return Decision{Rule: -1, Reason: ReasonOwnerChanged}
		}
		now := r.Now()
		if rec.RenamedSince == 0 || rec.RenamedSince > now.Unix() {
			rec.RenamedSince = now.Unix()
			set(rec)
		}
		end := time.Unix(rec.RenamedSince, 0).Add(time.Duration(f.graceDays()) * 24 * time.Hour)
		if !now.Before(end) {
			r.logOnceLocked(folder, "expired", "route: folder %s: %s", hash8(folder), ReasonRenameExpired)
			return Decision{Rule: -1, Reason: ReasonRenameExpired}
		}
		r.logOnceLocked(folder, "renamed", "route: folder %s: remote name changed, still routed to repo %q by its root commits until %s; add an alias to routes.json", hash8(folder), rec.Repo, end.Format("2006-01-02"))
		return Decision{Repo: rec.Repo, Rule: -1, Note: NoteRenamed, GraceEnds: end}
	}
	return d
}

func identityEqual(a, b identity) bool {
	return a.Key == b.Key && a.Repo == b.Repo && a.Client == b.Client && a.RenamedSince == b.RenamedSince &&
		slices.Equal(a.Roots, b.Roots) && slices.EqualFunc(a.Rules, b.Rules, func(x, y ruleSnap) bool {
		return x.Repo == y.Repo && slices.Equal(x.Names, y.Names)
	})
}

func (r *Router) logOnceLocked(folder, what, format string, args ...any) {
	k := folder + "\x00" + what
	if r.logged[k] {
		return
	}
	r.logged[k] = true
	r.Logf(format, args...)
}

type rootEntry struct {
	roots []string
	ok    bool
}

// defaultRoots reads the root commits of the folder's one checkout. The answer,
// shallow clones included, is cached per checkout and HEAD, so a tick costs one
// rev-parse until HEAD moves.
func (r *Router) defaultRoots(folder string) ([]string, bool) {
	c := r.Candidates(folder)
	if len(c) != 1 {
		return nil, false
	}
	git := r.Git
	if git == nil {
		git = gitOut
	}
	head, err := git(c[0], "rev-parse", "HEAD")
	if err != nil || head == "" {
		return nil, false
	}
	k := c[0] + "\x00" + head
	r.rootMu.Lock()
	e, hit := r.rootCache[k]
	r.rootMu.Unlock()
	if !hit {
		e.roots, e.ok = rootCommitsWith(git, c[0])
		r.rootMu.Lock()
		if r.rootCache == nil || len(r.rootCache) > 4096 {
			r.rootCache = map[string]rootEntry{}
		}
		r.rootCache[k] = e
		r.rootMu.Unlock()
	}
	return e.roots, e.ok
}

func rootCommits(dir string) ([]string, bool) { return rootCommitsWith(gitOut, dir) }

// rootCommitsWith lists the root commits of the checkout containing dir,
// sorted. A shallow clone has no usable roots (git would name its cut-off
// commits).
func rootCommitsWith(git func(string, ...string) (string, error), dir string) ([]string, bool) {
	if s, err := git(dir, "rev-parse", "--is-shallow-repository"); err != nil || s != "false" {
		return nil, false
	}
	out, err := git(dir, "rev-list", "--max-parents=0", "HEAD")
	if err != nil || out == "" {
		return nil, false
	}
	roots := strings.Fields(out)
	slices.Sort(roots)
	return roots, true
}
