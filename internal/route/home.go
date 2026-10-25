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
	// Unpin removes the pin instead of writing one: the folder then routes by
	// the other rules. With repo "" and Unpin unset, a deny pin is written.
	Unpin bool
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

// WithHome returns f with the folder's pin set to repo ("" writes a deny pin; Unpin removes the pin), and
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
	routes := make([]Rule, 0, len(f.Routes)+1)
	switch {
	case o.Unpin:
	case repo == "":
		routes = append(routes, Rule{Folder: folder, Deny: true})
	default:
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
	// The consent check runs on the resulting route, whatever the edit was:
	// out of a client or orch repo into a non-client one, or from denied into
	// one on a host that keeps client notes (a client's notes may sit in the
	// home folder).
	ch.To = g.Effective(folder)
	if ch.To != "" && ch.To != ch.From {
		// Repos carry no client name, so any move from a client or orch repo to
		// another repo counts, whether the target is a plain repo or another
		// client's.
		fromClient := f.Repos[ch.From].IsClient()
		toClient := g.Repos[ch.To].IsClient()
		if !o.ConfirmMoveOutOfClient {
			if fromClient {
				return f, ch, fmt.Errorf("the home folder is routed to the client repo %q; this would route it to the repo %q, which may belong to another party or be visible to the catalog and other hosts: %w if that is intended", ch.From, ch.To, ErrNeedsConfirm)
			}
			if ch.From == "" && !toClient && f.HasClientRepo() {
				return f, ch, fmt.Errorf("this host keeps client notes: routing the home folder to the non-client repo %q: %w", ch.To, ErrNeedsConfirm)
			}
		}
		if fromClient && ch.Changed && o.Logf != nil {
			if toClient {
				o.Logf("route: folder %s moved from client repo %q to another client repo %q; its notes will now sync there, and the old repo keeps a frozen copy", hash8(folder), ch.From, ch.To)
			} else {
				o.Logf("route: folder %s moved from client repo %q to non-client repo %q; its notes will now sync there, and the old repo keeps a frozen copy", hash8(folder), ch.From, ch.To)
			}
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
var ruleKeys = map[string]bool{"folder": true, "path": true, "remote": true, "aliases": true, "deny": true, "repo": true}

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
