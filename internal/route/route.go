// Package route decides which notes repo, if any, may carry a Claude project
// folder on this host. It is the routing table behind several repos per host:
// an ordered rule list, first match wins, default deny.
//
// A folder is denied (no repo carries it) when no rule matches, when the
// matching rule needs a git remote or path that cannot be resolved, or when the
// matching rule names a repo this host does not have. A denial never falls
// through to a later rule: that would let a folder of one client reach a
// broader rule meant for another repo.
package route

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Repo kinds. Every repo must say what it is: the client filter on the
// distill worklist is an allow list of kinds, never "not marked client".
const (
	KindPersonal = "personal"
	KindProject  = "project"
	KindInfra    = "infra"
	KindClient   = "client"
	KindOrch     = "orch"
)

var validKinds = map[string]bool{KindPersonal: true, KindProject: true, KindInfra: true, KindClient: true, KindOrch: true}

// Repo is one entry of the "repos" map: where its config.json is, and what
// kind of repo it is. The JSON form is {"config": "...", "kind": "project"}; a
// bare string is refused, as it would leave the kind unsaid.
type Repo struct {
	Config string `json:"config"`
	Kind   string `json:"kind"`
	// Remote is the repo's name on the remote (mem-project-<a>). Optional:
	// when empty it is derived from the config's remote URL. Relations in the
	// registry name repos this way.
	Remote string `json:"remote,omitempty"`
	// Client names the client of a client or orch repo. Optional: when empty
	// it is derived from the repo name (mem-client-<name>, mem-orch-<name>).
	Client string `json:"client,omitempty"`
}

// IsClient reports a repo holding a client's notes: kind client or orch.
func (r Repo) IsClient() bool { return r.Kind == KindClient || r.Kind == KindOrch }

// feedsCatalog reports the kinds whose notes may enter the personal catalog.
func (r Repo) feedsCatalog() bool {
	return r.Kind == KindPersonal || r.Kind == KindProject || r.Kind == KindInfra
}

func (r *Repo) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return fmt.Errorf(`repo %q: write it as {"config": %q, "kind": "personal|project|infra|client|orch"}; a bare path does not say what the repo is`, s, s)
	}
	type plain Repo
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = Repo(p)
	return nil
}

// Rule routes the folders it matches to Repo. Exactly one of Folder, Path and
// Remote is set.
//
//   - Folder: Claude's escaped folder name, or a glob over it.
//   - Path: a glob over the project directory (the escaped folder name resolved
//     to the one existing directory it names).
//   - Remote: a glob over the normalised git remote key host/owner/repo of that
//     directory's origin.
//
// In a glob "*" matches within one path segment and "**" matches across
// segments.
type Rule struct {
	Folder string `json:"folder,omitempty"`
	Path   string `json:"path,omitempty"`
	Remote string `json:"remote,omitempty"`
	// Aliases are further remote keys (globs, like Remote) that match this
	// rule too. They are only for remote rules: after a rename or transfer of
	// the repo, list the old or the new name here.
	Aliases []string `json:"aliases,omitempty"`
	// Deny makes the rule a denial: the folders it matches go to no repo, and
	// no later rule is tried. It carries no Repo.
	Deny bool   `json:"deny,omitempty"`
	Repo string `json:"repo,omitempty"`
}

// matchesRemote reports whether the remote key matches the rule's pattern or
// one of its aliases.
func (r Rule) matchesRemote(key string) bool {
	if globMatch(r.Remote, key) {
		return true
	}
	for _, a := range r.Aliases {
		if globMatch(a, key) {
			return true
		}
	}
	return false
}

// sig identifies a rule's content, to tell an edited rule from an old one.
func (r Rule) sig() string {
	d := ""
	if r.Deny {
		d = "\x00deny"
	}
	return r.kind() + "\x00" + r.pattern() + "\x00" + strings.Join(r.Aliases, "\x01") + "\x00" + r.Repo + d
}

func (r Rule) kind() string {
	switch {
	case r.Folder != "":
		return "folder"
	case r.Path != "":
		return "path"
	}
	return "remote"
}

func (r Rule) pattern() string {
	switch r.kind() {
	case "folder":
		return r.Folder
	case "path":
		return r.Path
	}
	return r.Remote
}

// File is routes.json.
type File struct {
	// Repos names the repos this host may have, by their config.json.
	Repos map[string]Repo `json:"repos"`
	// Personal names the repo that syncs its work-tree (the catalog) and owns
	// no folders, except one reached through an exact folder pin. Optional.
	Personal string `json:"personal,omitempty"`
	Routes   []Rule `json:"routes"`
	// Default must be "deny" (or empty): the only policy there is.
	Default string `json:"default,omitempty"`
	// RenameGraceDays is how long a folder whose remote was renamed keeps
	// routing to its recorded repo through its root commits, while no rule
	// matches the new name. nil means DefaultRenameGraceDays; 0 denies at once.
	RenameGraceDays *int `json:"renameGraceDays,omitempty"`

	// Registry is the directory of the registry repo (a normal sync dir):
	// related.json declares the relations between repos, and hosts publish
	// their routing templates under hosts/<label>/routes.json. Optional.
	Registry string `json:"registry,omitempty"`
	// RelatedRoot is where the fetch-only clones of related repos live
	// (<RelatedRoot>/<repo>). Default: a "related" dir beside the registry.
	RelatedRoot string `json:"relatedRoot,omitempty"`
	// RelatedRemotes maps a related repo's name to the URL its read-only
	// clone is fetched from (in practice an ssh alias of a read-only deploy
	// key).
	RelatedRemotes map[string]string `json:"relatedRemotes,omitempty"`
	// RelatedOwners lists the GitHub owners, besides this host's own org, that a
	// read clone's URL may name.
	RelatedOwners []string `json:"relatedOwners,omitempty"`
	// RelatedMaxStaleHours is how old a cached visibility verdict may be and
	// still serve when GitHub cannot be reached (default 24).
	RelatedMaxStaleHours int `json:"relatedMaxStaleHours,omitempty"`
	// RelatedFetchTimeoutSec bounds one fetch of a read clone (default 60).
	RelatedFetchTimeoutSec int `json:"relatedFetchTimeoutSec,omitempty"`
	// RelatedMaxMB caps the exported size of one read clone (default 64).
	RelatedMaxMB int `json:"relatedMaxMB,omitempty"`
}

// DefaultRenameGraceDays is the grace period when routes.json does not say.
const DefaultRenameGraceDays = 30

func (f File) graceDays() int {
	if f.RenameGraceDays == nil {
		return DefaultRenameGraceDays
	}
	return *f.RenameGraceDays
}

// Load reads and validates routes.json. A rule may name a repo that is not in
// Repos: the host does not have it, and the rule then denies its folders.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func (f File) validate() error {
	for name, r := range f.Repos {
		if strings.Contains(name, ":") || strings.HasPrefix(name, "deny") || strings.HasPrefix(name, "unavailable") {
			return fmt.Errorf("repo name %q is reserved: no ':' and no name starting with deny or unavailable", name)
		}
		if !validKinds[r.Kind] {
			return fmt.Errorf("repo %q: kind must be one of personal, project, infra, client, orch (got %q)", name, r.Kind)
		}
	}
	if f.Default != "" && f.Default != "deny" {
		return fmt.Errorf(`default must be "deny", not %q`, f.Default)
	}
	if f.Personal != "" {
		pr, ok := f.Repos[f.Personal]
		if !ok {
			return fmt.Errorf("personal repo %q is not in repos", f.Personal)
		}
		if pr.Kind != KindPersonal {
			return fmt.Errorf("personal repo %q must have kind %q", f.Personal, KindPersonal)
		}
	}
	if f.RenameGraceDays != nil && *f.RenameGraceDays < 0 {
		return fmt.Errorf("renameGraceDays must not be negative")
	}
	for i, r := range f.Routes {
		if len(r.Aliases) > 0 && r.Remote == "" {
			return fmt.Errorf("route %d: aliases belong to remote rules only", i)
		}
		for _, a := range r.Aliases {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("route %d: an alias is empty", i)
			}
		}
		n := 0
		for _, s := range []string{r.Folder, r.Path, r.Remote} {
			if s != "" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("route %d: exactly one of folder, path and remote is required", i)
		}
		if r.Deny && r.Repo != "" {
			return fmt.Errorf("route %d: a deny rule carries no repo", i)
		}
		if !r.Deny && r.Repo == "" {
			return fmt.Errorf("route %d: repo is required", i)
		}
		if f.isPersonal(r.Repo) && !r.isPin() {
			return fmt.Errorf("route %d: the personal repo %q can only be a target of a folder rule naming one exact folder (no glob characters), not of a path, remote or glob rule", i, r.Repo)
		}
	}
	return nil
}

// hasGlob reports a glob character in a pattern.
func hasGlob(s string) bool { return strings.ContainsAny(s, "*?[]") }

// isPin reports a folder rule naming one exact folder.
func (r Rule) isPin() bool {
	return r.Folder != "" && r.Path == "" && r.Remote == "" && !hasGlob(r.Folder)
}

// isPersonal reports the personal repo: the one named in Personal, or any repo
// of kind personal.
func (f File) isPersonal(repo string) bool {
	return (f.Personal != "" && repo == f.Personal) || f.Repos[repo].Kind == KindPersonal
}

// Denial reasons.
const (
	// ReasonPersonalNotPinned: the personal repo is only reachable through an
	// exact folder pin.
	ReasonDenied = "a deny rule matches"
	// ReasonPersonalNotPinned: see isPersonal.
	ReasonPersonalNotPinned = "the personal repo needs an exact folder pin"

	ReasonNoRoute     = "no route matches"
	ReasonUnresolved  = "remote or path could not be resolved"
	ReasonRepoMissing = "route names a repo this host does not have"

	// ReasonRemoteChanged: the folder's remote was renamed or moved and the
	// new name routes to a different repo than the folder was recorded in.
	ReasonRemoteChanged = "remote changed repos"
	// ReasonRenameExpired: a renamed remote kept routing through its root
	// commits for the grace period, and no alias was added in time.
	ReasonRenameExpired = "renamed remote: grace period over, add an alias"

	// NoteRenamed marks a folder still routed by its root commits.
	NoteRenamed = "renamed remote: add an alias"

	// ReasonIdentityUnreadable: identity.json cannot be read, so folders that
	// were routed before are held rather than guessed at.
	ReasonIdentityUnreadable = "identity.json is unreadable"
	// ReasonOwnerChanged: a client folder's remote moved to another owner.
	ReasonOwnerChanged = "client repo moved to another owner: add an alias"
	// ReasonDifferentRepo: the checkout at the path holds other history than
	// the one recorded.
	ReasonDifferentRepo = "different repository at this path"
)

// Decision is the outcome for one folder. Repo is "" when it is denied.
type Decision struct {
	Repo   string
	Rule   int // index of the matching rule, -1 when none matched
	Reason string
	// Note is NoteRenamed when the folder is routed by root commits only.
	Note string
	// GraceEnds is when a NoteRenamed folder is denied unless an alias is added.
	GraceEnds time.Time
}

type resolution int

const (
	resolutionNone      resolution = iota // no project directory / no usable origin
	resolutionOK                          // exactly one directory, remote key found
	resolutionAmbiguous                   // several directories escape alike
)

// Router applies a File on one host.
type Router struct {
	cur_ atomic.Pointer[File]
	gen  atomic.Uint64 // bumped on every Replace and Refresh

	// afterCompute, if set, runs between computing a decision and storing it
	// (a test seam for the race with Replace).
	afterCompute func()
	have         func(repo string) bool

	// SSHHost maps an ssh host alias to the real host name (ssh -G). nil
	// leaves hosts as written.
	SSHHost func(host string) string

	// Resolve gives the remote key of a folder's project directory. Tests
	// replace it; the default reads the git origin of the directory.
	Resolve func(folder string) (string, resolution)
	// Candidates lists the directories a folder name may stand for.
	Candidates func(folder string) []string
	// Logf receives the routing log lines (default log.Printf).
	Logf func(format string, args ...any)
	// StatePath, if set, remembers each folder's owner across restarts, so a
	// move between repos can be logged.
	StatePath string

	// Roots gives the root commits (sorted) of a folder's checkout; ok is false
	// when they cannot be told (no checkout, ambiguous, shallow clone).
	Roots func(folder string) (roots []string, ok bool)
	// Now is the clock (default time.Now).
	Now func() time.Time

	// ReadOnly keeps the router from writing owners.json and identity.json:
	// status, distill and the hooks only look. New sets it; the daemon calls
	// Writable.
	ReadOnly bool
	// Git runs git for the root-commit lookup (default: cleaned-env git).
	Git func(dir string, args ...string) (string, error)

	ids       map[string]identity // per-folder record, see identity.go
	idBad     string              // why identity.json could not be read
	dirty     map[string]bool     // folders changed here, not yet saved
	rootMu    sync.Mutex
	rootCache map[string]rootEntry

	mu     sync.Mutex
	cache  map[string]Decision
	logged map[string]bool
	owners map[string]string
	loaded bool
}

// New returns a Router. have reports whether this host has the named repo.
func New(f File, have func(repo string) bool) *Router {
	r := &Router{have: have, cache: map[string]Decision{}, logged: map[string]bool{}, Logf: log.Printf}
	r.cur_.Store(normalized(f))
	r.Candidates = Candidates
	r.SSHHost = sshHostname
	r.Resolve = r.defaultResolve
	r.Roots = r.defaultRoots
	r.Now = time.Now
	r.ReadOnly = true
	return r
}

// Writable lets the router record owners and root-commit identities. Only the
// daemon should call it.
func (r *Router) Writable() { r.ReadOnly = false }

func (r *Router) cur() File { return *r.cur_.Load() }

// normalized returns f with the remote patterns lower-cased, as the remote
// keys they are matched against are.
func normalized(f File) *File {
	routes := make([]Rule, len(f.Routes))
	copy(routes, f.Routes)
	for i := range routes {
		routes[i].Remote = strings.ToLower(routes[i].Remote)
		if len(routes[i].Aliases) > 0 {
			al := make([]string, len(routes[i].Aliases))
			for j, a := range routes[i].Aliases {
				al[j] = strings.ToLower(strings.TrimSpace(a))
			}
			routes[i].Aliases = al
		}
	}
	f.Routes = routes
	return &f
}

// Replace swaps in a new routing table atomically and drops the cached
// decisions. The repos a host runs are fixed at start: a rule naming a repo that
// has no running loop only denies.
func (r *Router) Replace(f File) {
	r.mu.Lock()
	r.cur_.Store(normalized(f))
	r.gen.Add(1)
	r.cache = map[string]Decision{}
	r.reloadIDsLocked()
	r.mu.Unlock()
}

// Refresh drops the cached decisions, so changed checkouts and remotes are
// seen. The daemon calls it on its pull tick.
func (r *Router) Refresh() {
	r.mu.Lock()
	r.gen.Add(1)
	r.cache = map[string]Decision{}
	r.reloadIDsLocked()
	r.mu.Unlock()
}

func (r *Router) defaultResolve(folder string) (string, resolution) {
	c := r.Candidates(folder)
	switch len(c) {
	case 0:
		return "", resolutionNone
	case 1:
		if u, ok := remoteURL(c[0]); ok && u != "" {
			if k, ok := NormalizeRemote(rewriteSSHHost(u, r.SSHHost)); ok {
				return k, resolutionOK
			}
		}
		return "", resolutionNone
	}
	return "", resolutionAmbiguous
}

// Decide routes one folder (Claude's escaped folder name).
func (r *Router) Decide(folder string) Decision {
	r.mu.Lock()
	if d, ok := r.cache[folder]; ok {
		r.mu.Unlock()
		return d
	}
	r.mu.Unlock()

	for {
		gen := r.gen.Load()
		d, key, keyOK := r.decide(folder)
		roots, rootsOK := r.rootsIfNeeded(folder, d, key, keyOK)
		if r.afterCompute != nil {
			r.afterCompute()
		}
		r.mu.Lock()
		// The table changed while this was computed: decide again against the
		// current one, so no caller gets an answer from the old table.
		if r.gen.Load() != gen {
			r.mu.Unlock()
			continue
		}
		d = r.identityLocked(folder, d, key, keyOK, roots, rootsOK)
		r.cache[folder] = d
		r.noteLocked(folder, d)
		r.mu.Unlock()
		return d
	}
}

func (r *Router) decide(folder string) (Decision, string, bool) {
	f := r.cur()
	var key string
	var res resolution
	var resolved bool
	var dirs []string
	var dirsDone bool
	for i, rule := range f.Routes {
		match := false
		switch rule.kind() {
		case "folder":
			match = globFold(rule.Folder, folder, foldCase)
		case "path":
			if !dirsDone {
				dirs, dirsDone = r.Candidates(folder), true
			}
			if len(dirs) != 1 {
				// Cannot tell where the folder is: deny now. A later rule must
				// not catch what an earlier path or remote rule could not place.
				return Decision{Rule: i, Reason: ReasonUnresolved}, "", false
			}
			match = globFold(expandHome(rule.Path), filepath.ToSlash(dirs[0]), foldCase)
		case "remote":
			if !resolved {
				key, res = r.Resolve(folder)
				resolved = true
			}
			if res != resolutionOK {
				return Decision{Rule: i, Reason: ReasonUnresolved}, "", false
			}
			match = rule.matchesRemote(key)
		}
		if !match {
			continue
		}
		if rule.Deny {
			return Decision{Rule: i, Reason: ReasonDenied}, key, res == resolutionOK
		}
		if f.isPersonal(rule.Repo) && !rule.isPin() {
			return Decision{Rule: i, Reason: ReasonPersonalNotPinned}, key, res == resolutionOK
		}
		if _, listed := f.Repos[rule.Repo]; !listed || r.have == nil || !r.have(rule.Repo) {
			return Decision{Rule: i, Reason: ReasonRepoMissing}, key, res == resolutionOK
		}
		return Decision{Repo: rule.Repo, Rule: i}, key, res == resolutionOK
	}
	return Decision{Rule: -1, Reason: ReasonNoRoute}, key, res == resolutionOK
}

// Owns returns the gate for a repo's loop: true for the folders routed to it.
// The personal repo owns only the folder an exact pin names.
func (r *Router) Owns(repo string) func(folder string) bool {
	return func(folder string) bool {
		return r.Decide(folder).Repo == repo
	}
}

// Escape is the folder name Claude Code uses for a project directory.
func Escape(p string) string { return escape(p) }

func hash8(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// noteLocked logs a denial once per folder and reason, and a change of owner.
// Folders are named by a short hash: the folder name can carry a client's name.
func (r *Router) noteLocked(folder string, d Decision) {
	if d.Repo == "" {
		k := folder + "\x00" + d.Reason
		if !r.logged[k] {
			r.logged[k] = true
			r.Logf("route: folder %s denied: %s", hash8(folder), d.Reason)
		}
	}
	if d.Reason == ReasonRemoteChanged || d.Reason == ReasonIdentityUnreadable {
		// Held: the owner record stays, so the move is logged if the user's
		// route edit later accepts it.
		return
	}
	r.loadOwnersLocked()
	prev, had := r.owners[folder]
	if had && prev == d.Repo {
		return
	}
	if !had && d.Repo == "" {
		return // never owned, still not
	}
	switch {
	case had && prev != "" && r.cur().Repos[prev].IsClient() && d.Repo != "" && !r.cur().Repos[d.Repo].IsClient():
		r.Logf("route: folder %s moved from client repo %q to non-client repo %q; its notes will now sync there, and the old repo keeps a frozen copy", hash8(folder), prev, d.Repo)
	case had && prev != "" && d.Repo != "":
		r.Logf("route: folder %s moved from repo %q to repo %q", hash8(folder), prev, d.Repo)
	case had && prev != "":
		r.Logf("route: folder %s is no longer routed to repo %q", hash8(folder), prev)
	}
	if d.Repo == "" {
		delete(r.owners, folder)
	} else {
		r.owners[folder] = d.Repo
	}
	r.saveOwnersLocked()
}

func (r *Router) loadOwnersLocked() {
	if r.loaded {
		return
	}
	r.loaded = true
	r.owners = map[string]string{}
	if r.StatePath == "" {
		return
	}
	if b, err := os.ReadFile(r.StatePath); err == nil {
		_ = json.Unmarshal(b, &r.owners)
		if r.owners == nil {
			r.owners = map[string]string{}
		}
	}
}

func (r *Router) saveOwnersLocked() {
	if r.StatePath == "" || r.ReadOnly {
		return
	}
	if err := writeJSONAtomic(r.StatePath, r.owners, ".owners-*.tmp"); err != nil {
		r.logOnceLocked("", "ownsave", "route: cannot save owners.json: %v", err)
	}
}

// Shadow says rule Rule can never be reached first because rule By, earlier
// in the list and of the same kind, matches everything it does.
type Shadow struct{ Rule, By int }

// Shadowed lists the rules an earlier rule of the same kind already covers.
// The test is static: the earlier pattern, as a glob, matches the later
// pattern's text.
func (r *Router) Shadowed() []Shadow {
	var out []Shadow
	for j, rj := range r.cur().Routes {
		for i := 0; i < j; i++ {
			ri := r.cur().Routes[i]
			if ri.kind() == rj.kind() && globMatch(ri.pattern(), rj.pattern()) {
				out = append(out, Shadow{Rule: j, By: i})
				break
			}
		}
	}
	return out
}

// Summary counts folders per repo and the denied ones.
type Summary struct {
	Owned  map[string]int
	Denied int
	// Renamed lists the folders routed by root commits only (hash, days left);
	// Changed lists the denied folders whose remote moved to another repo, and
	// Expired those whose grace period ran out (both by hash).
	Renamed []RenamedFolder
	Changed []string
	Expired []string
	// Transferred: client folders moved to another owner; Different: another
	// repository at the path; Unreadable: identity.json cannot be read.
	Transferred []string
	Different   []string
	Unreadable  bool
}

// RenamedFolder is a folder kept by its root commits until GraceEnds.
type RenamedFolder struct {
	Hash     string
	DaysLeft int
}

// Summarize routes every folder and counts the outcome.
func (r *Router) Summarize(folders []string) Summary {
	s := Summary{Owned: map[string]int{}}
	for name := range r.cur().Repos {
		s.Owned[name] = 0
	}
	for _, f := range folders {
		d := r.Decide(f)
		switch {
		case d.Reason == ReasonRemoteChanged:
			s.Changed = append(s.Changed, hash8(f))
		case d.Reason == ReasonRenameExpired:
			s.Expired = append(s.Expired, hash8(f))
		case d.Reason == ReasonOwnerChanged:
			s.Transferred = append(s.Transferred, hash8(f))
		case d.Reason == ReasonDifferentRepo:
			s.Different = append(s.Different, hash8(f))
		case d.Note == NoteRenamed:
			left := int((d.GraceEnds.Sub(r.Now()) + 24*time.Hour - 1) / (24 * time.Hour))
			if left < 0 {
				left = 0
			}
			s.Renamed = append(s.Renamed, RenamedFolder{Hash: hash8(f), DaysLeft: left})
		}
		if d.Repo == "" {
			s.Denied++
			continue
		}
		s.Owned[d.Repo]++
	}
	s.Unreadable = r.IdentityProblem() != ""
	return s
}

// RepoNames lists the repos in routes.json, sorted.
func (f File) RepoNames() []string {
	names := make([]string, 0, len(f.Repos))
	for n := range f.Repos {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// File returns the routes the router applies.
func (r *Router) File() File { return r.cur() }

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.ToSlash(h) + p[1:]
		}
	}
	return p
}

// ExpandHome expands a leading "~/" in a config path.
func ExpandHome(p string) string { return filepath.FromSlash(expandHome(p)) }

var globCache sync.Map

// foldCase makes path and folder globs case-insensitive, as the file systems of
// macOS and Windows are by default.
var foldCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// sameFolder compares folder names the way routing does: ignoring case only
// where the file system does.
func sameFolder(a, b string) bool {
	if foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func globMatch(pat, name string) bool { return globFold(pat, name, false) }

// globFold is globMatch, optionally ignoring case.
func globFold(pat, name string, fold bool) bool {
	key := pat
	if fold {
		key = "(?i)" + pat
	}
	return globRe(key, pat, fold).MatchString(name)
}

// globRe compiles pat: "*" matches within one "/" segment, "**" across
// segments, "?" one non-"/" character.
func globRe(key, pat string, fold bool) *regexp.Regexp {
	if v, ok := globCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	var b strings.Builder
	if fold {
		b.WriteString(`(?i)`)
	}
	b.WriteString(`^`)
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				b.WriteString(`.*`)
				i++
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		re = regexp.MustCompile(`^\x00$`)
	}
	globCache.Store(key, re)
	return re
}

// NonClient reports whether the folder is routed to a repo that is not a client
// repo: the only folders whose notes may feed the personal catalog.
func (r *Router) NonClient(folder string) bool {
	d := r.Decide(folder)
	return d.Repo != "" && r.cur().Repos[d.Repo].feedsCatalog()
}
