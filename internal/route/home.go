package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// The home folder is the Claude folder of sessions started in the home
// directory. Where it routes is a per-host choice: it is a single exact folder
// pin at the top of routes.json.

// HomeFolder is the escaped name of this user's home directory on this OS
// (-home-<user>, -Users-<user>).
func HomeFolder() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return escape(h), nil
}

// ErrNeedsConfirm marks a change that needs --confirm-move-out-of-client.
var ErrNeedsConfirm = errors.New("pass --confirm-move-out-of-client")

// HasClientRepo reports a client or orch repo among the repos.
func (f File) HasClientRepo() bool {
	for _, r := range f.Repos {
		if r.IsClient() {
			return true
		}
	}
	return false
}

// HomeOpts tunes SetHomePin.
type HomeOpts struct {
	// ConfirmMoveOutOfClient allows taking the folder out of a client or
	// orchestrator repo into a non-client repo.
	ConfirmMoveOutOfClient bool
	// Logf receives the log line of a move out of a client repo (default: none).
	Logf func(format string, args ...any)
}

// HomeChange describes what SetHomePin did. From and To are repo names, ""
// meaning denied.
type HomeChange struct {
	Folder   string
	From, To string
	Changed  bool
}

// HomePin returns the repo an exact pin sends the folder to, and whether there
// is one.
func (f File) HomePin(folder string) (string, bool) {
	for _, r := range f.Routes {
		if r.isPin() && sameFolder(r.Folder, folder) {
			return r.Repo, true
		}
	}
	return "", false
}

// Effective is where the table routes the folder today, by any rule ("" is
// denied). Rules that need the checkout read it from disk.
func (f File) Effective(folder string) string {
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	return r.Decide(folder).Repo
}

// WithHome returns f with the folder's pin set to repo ("" removes it), and
// says what changed. It enforces the guards that hold everywhere: the repo must
// be in repos, and a move out of a client or orch repo into another kind needs
// consent.
func (f File) WithHome(folder, repo string, o HomeOpts) (File, HomeChange, error) {
	ch := HomeChange{Folder: folder, To: repo}
	if folder == "" || hasGlob(folder) {
		return f, ch, fmt.Errorf("home folder %q must name one exact folder (no glob characters)", folder)
	}
	if repo != "" {
		if _, ok := f.Repos[repo]; !ok {
			return f, ch, fmt.Errorf("repo %q is not in repos; known: %s", repo, strings.Join(f.RepoNames(), ", "))
		}
	}
	ch.From = f.Effective(folder)
	if ch.From == "" {
		ch.From, _ = f.HomePin(folder)
	}
	// Pinning the home folder to a non-client repo needs consent when it is
	// leaving a client repo, and on any host that keeps client notes at all
	// (even from "denied": a client's notes may sit in the home folder).
	if toPlain := repo != "" && !f.Repos[repo].IsClient(); toPlain && !o.ConfirmMoveOutOfClient {
		if from, ok := f.Repos[ch.From]; ok && from.IsClient() {
			return f, ch, fmt.Errorf("the home folder is routed to the client repo %q; moving it to the non-client repo %q puts its notes where the catalog and other hosts can see them: %w if that is intended", ch.From, repo, ErrNeedsConfirm)
		}
		if f.HasClientRepo() {
			return f, ch, fmt.Errorf("this host keeps client notes: pinning the home folder to the non-client repo %q: %w", repo, ErrNeedsConfirm)
		}
	}
	routes := make([]Rule, 0, len(f.Routes)+1)
	if repo != "" {
		routes = append(routes, Rule{Folder: folder, Repo: repo})
	}
	for _, r := range f.Routes {
		if r.isPin() && sameFolder(r.Folder, folder) {
			continue
		}
		routes = append(routes, r)
	}
	ch.Changed = !sameRoutes(f.Routes, routes)
	g := f
	g.Routes = routes
	if err := g.validate(); err != nil {
		return f, ch, err
	}
	if o.Logf != nil && ch.Changed {
		if from, ok := f.Repos[ch.From]; ok && from.IsClient() && repo != "" && !f.Repos[repo].IsClient() {
			o.Logf("route: folder %s moved from client repo %q to non-client repo %q; its notes will now sync there, and the old repo keeps a frozen copy", hash8(folder), ch.From, repo)
		}
	}
	return g, ch, nil
}

// SetHomePin edits the routes file at path: it sets (or, for repo "", removes)
// the pin of the home folder, at the top, under the file's lock and through an
// atomic replace. A file that does not parse is left alone.
func SetHomePin(path, folder, repo string, o HomeOpts) (HomeChange, error) {
	var ch HomeChange
	err := EditRoutes(path, func(f File) (File, error) {
		g, c, err := f.WithHome(folder, repo, o)
		ch = c
		return g, err
	})
	return ch, err
}

// ruleKeys are the fields of a rule this version knows.
var ruleKeys = map[string]bool{"folder": true, "path": true, "remote": true, "aliases": true, "repo": true}

// EditRoutes reads path under its lock, applies edit, and writes the result
// atomically if the routes differ. Only the "routes" list may change: every
// other field of the file, known to this version or not, is written back as it
// was read. A rule with a field this version does not know, or an edit that
// changes anything but the routes, is refused rather than dropping data.
func EditRoutes(path string, edit func(File) (File, error)) error {
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer unlock()
	f, err := Load(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	var oldRules []map[string]json.RawMessage
	if err := json.Unmarshal(top["routes"], &oldRules); err != nil && top["routes"] != nil {
		return fmt.Errorf("routes: %w", err)
	}
	for i, r := range oldRules {
		for k := range r {
			if !ruleKeys[k] {
				return fmt.Errorf("route %d has a field %q this version does not know; edit routes.json by hand", i, k)
			}
		}
	}
	g, err := edit(f)
	if err != nil {
		return err
	}
	if err := g.validate(); err != nil {
		return err
	}
	a, b := f, g
	a.Routes, b.Routes = nil, nil
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("internal error: the edit changed more than the routes; nothing written")
	}
	if sameRoutes(f.Routes, g.Routes) {
		return nil
	}
	rb, err := json.Marshal(g.Routes)
	if err != nil {
		return err
	}
	top["routes"] = rb
	return writeJSONAtomic(path, top, ".routes-*.tmp")
}

// sameRoutes reports equal rule lists.
func sameRoutes(a, b []Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].sig() != b[i].sig() {
			return false
		}
	}
	return true
}

// SameRoutes reports equal rule lists.
func SameRoutes(a, b []Rule) bool { return sameRoutes(a, b) }
