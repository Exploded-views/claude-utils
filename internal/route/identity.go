package route

import (
	"encoding/json"
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

type identity struct {
	Key          string   `json:"key"`
	Repo         string   `json:"repo"`
	Client       bool     `json:"client,omitempty"`
	Roots        []string `json:"roots,omitempty"`
	Rules        []string `json:"rules,omitempty"` // sigs of the rules when last confirmed
	RenamedSince int64    `json:"renamedSince,omitempty"`
}

func (r *Router) identityPath() string {
	if r.StatePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(r.StatePath), "identity.json")
}

func (r *Router) loadIDsLocked() {
	if r.ids != nil {
		return
	}
	r.ids = map[string]identity{}
	if p := r.identityPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &r.ids)
			if r.ids == nil {
				r.ids = map[string]identity{}
			}
		}
	}
}

func (r *Router) saveIDsLocked() {
	if p := r.identityPath(); p != "" {
		writeJSONAtomic(p, r.ids, ".identity-*.tmp")
	}
}

func writeJSONAtomic(path string, v any, pattern string) {
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	// A temp file of its own, so two daemons never write the same one.
	if f, err := os.CreateTemp(filepath.Dir(path), pattern); err == nil {
		_, werr := f.Write(b)
		cerr := f.Close()
		if werr == nil && cerr == nil && os.Rename(f.Name(), path) == nil {
			return
		}
		os.Remove(f.Name())
	}
}

func ruleSigs(f File) []string {
	out := make([]string, len(f.Routes))
	for i, r := range f.Routes {
		out[i] = r.sig()
	}
	return out
}

// rootsIfNeeded reads root commits only when the identity logic will use them:
// to record a folder, or to check a folder whose key changed. Git runs outside
// the router's lock.
func (r *Router) rootsIfNeeded(folder string, d Decision, key string, keyOK bool) ([]string, bool) {
	if !keyOK {
		return nil, false
	}
	r.mu.Lock()
	r.loadIDsLocked()
	rec, has := r.ids[folder]
	r.mu.Unlock()
	need := (d.Repo != "" && (!has || len(rec.Roots) == 0)) ||
		(d.Repo == "" && d.Reason == ReasonNoRoute && has && rec.Key != key)
	if !need || r.Roots == nil {
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
	sigs := ruleSigs(f)
	rec, has := r.ids[folder]
	if !rootsOK {
		roots = nil
	}
	store := func(n identity) {
		if !has || !identityEqual(rec, n) {
			r.ids[folder] = n
			r.saveIDsLocked()
		}
	}
	switch {
	case d.Repo != "":
		n := identity{Key: key, Repo: d.Repo, Client: f.Repos[d.Repo].IsClient(), Roots: roots, Rules: sigs}
		if has {
			if len(n.Roots) == 0 {
				n.Roots = rec.Roots
			}
			if rec.Key != key && rec.Repo != d.Repo && d.Rule >= 0 && slices.Contains(rec.Rules, f.Routes[d.Rule].sig()) {
				// An old rule, not the user's edit, sends the renamed remote
				// elsewhere: never auto-move.
				r.logOnceLocked(folder, "changed", "route: folder %s: remote changed repos; denied until routes.json is edited", hash8(folder))
				return Decision{Rule: d.Rule, Reason: ReasonRemoteChanged}
			}
		}
		store(n)
	case d.Reason == ReasonNoRoute && has && rec.Key != key:
		target := f.Repos[rec.Repo]
		_, listed := f.Repos[rec.Repo]
		if !listed || rec.Repo == f.Personal || r.have == nil || !r.have(rec.Repo) ||
			target.IsClient() != rec.Client || len(rec.Roots) == 0 || !slices.Equal(roots, rec.Roots) {
			return d
		}
		now := r.Now()
		if rec.RenamedSince == 0 {
			rec.RenamedSince = now.Unix()
			r.ids[folder] = rec
			r.saveIDsLocked()
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
		slices.Equal(a.Roots, b.Roots) && slices.Equal(a.Rules, b.Rules)
}

func (r *Router) logOnceLocked(folder, what, format string, args ...any) {
	k := folder + "\x00" + what
	if r.logged[k] {
		return
	}
	r.logged[k] = true
	r.Logf(format, args...)
}

// defaultRoots reads the root commits of the folder's one checkout.
func (r *Router) defaultRoots(folder string) ([]string, bool) {
	c := r.Candidates(folder)
	if len(c) != 1 {
		return nil, false
	}
	return rootCommits(c[0])
}

// rootCommits lists the root commits of the checkout containing dir, sorted. A
// shallow clone has no usable roots (git would name its cut-off commits).
func rootCommits(dir string) ([]string, bool) {
	if s, err := gitOut(dir, "rev-parse", "--is-shallow-repository"); err != nil || s != "false" {
		return nil, false
	}
	out, err := gitOut(dir, "rev-list", "--max-parents=0", "HEAD")
	if err != nil || out == "" {
		return nil, false
	}
	roots := strings.Fields(out)
	slices.Sort(roots)
	return roots, true
}
