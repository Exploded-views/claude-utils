package route

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// A routing template is what one host publishes to the registry repo so that
// another host can start from it: the rules and the names and kinds of the
// repos they name, and nothing host-specific. No config paths, no sync dirs, no
// remote URLs, no keys or tokens, no host or user name: the home folder pin is
// marked "home" instead of naming a folder.

const templateVersion = 1

// Template is hosts/<label>/routes.json in the registry repo.
type Template struct {
	Version  int               `json:"version"`
	Repos    map[string]string `json:"repos"` // repo name -> kind
	Personal string            `json:"personal,omitempty"`
	Routes   []TemplateRule    `json:"routes"`
}

// TemplateRule is a Rule, or (Home) the pin of the home folder of whichever
// host reads it.
type TemplateRule struct {
	Home    bool     `json:"home,omitempty"`
	Folder  string   `json:"folder,omitempty"`
	Path    string   `json:"path,omitempty"`
	Remote  string   `json:"remote,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	Repo    string   `json:"repo"`
}

var labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidLabel checks a host label: user-chosen, never the machine's host name.
func ValidLabel(s string) error {
	if !labelRe.MatchString(s) {
		return fmt.Errorf("label %q: use 1-32 characters of a-z, 0-9 and '-', starting with a letter or digit (a name you choose, such as laptop-2)", s)
	}
	return nil
}

// homePlaceholder stands for the publishing user's home in folder and path
// patterns; inherit expands it for the local user.
const homePlaceholder = "{home}"

var (
	homeFolderRe = regexp.MustCompile(`^-(?:home|Users)-[A-Za-z0-9]+`)
	homePathRe   = regexp.MustCompile(`^(?:/(?:home|Users)/[^/]+|[A-Za-z]:/Users/[^/]+)`)
)

func portableFolder(p, homeFolder string) string {
	if homeFolder != "" && len(p) >= len(homeFolder) && sameFolder(p[:len(homeFolder)], homeFolder) &&
		(len(p) == len(homeFolder) || p[len(homeFolder)] == '-') {
		return homePlaceholder + p[len(homeFolder):]
	}
	if m := homeFolderRe.FindString(p); m != "" {
		return homePlaceholder + p[len(m):]
	}
	return p
}

func portablePath(p, homeDir string) string {
	q := filepath.ToSlash(p)
	if h := filepath.ToSlash(homeDir); h != "" && (q == h || strings.HasPrefix(q, h+"/")) {
		return homePlaceholder + q[len(h):]
	}
	if m := homePathRe.FindString(q); m != "" {
		return homePlaceholder + q[len(m):]
	}
	return p
}

// Template builds the template of this table. homeFolder and homeDir are this
// host's home folder and directory: the home pin is published as a "home"
// rule, and a leading home in a folder or path pattern as {home}. Client and
// orch repos, and every rule naming one, are left out: a template is for
// sharing between hosts, and a host's client rules are written by hand on it.
func (f File) Template(homeFolder, homeDir string) Template {
	t := Template{Version: templateVersion, Repos: map[string]string{}, Routes: []TemplateRule{}}
	for n, r := range f.Repos {
		if !r.IsClient() {
			t.Repos[n] = r.Kind
		}
	}
	if _, ok := t.Repos[f.Personal]; ok {
		t.Personal = f.Personal
	}
	for _, r := range f.Routes {
		if _, ok := t.Repos[r.Repo]; !ok {
			continue
		}
		tr := TemplateRule{Folder: r.Folder, Path: r.Path, Remote: r.Remote, Aliases: r.Aliases, Repo: r.Repo}
		switch {
		case r.isPin() && homeFolder != "" && sameFolder(r.Folder, homeFolder):
			tr = TemplateRule{Home: true, Repo: r.Repo}
		case r.Folder != "":
			tr.Folder = portableFolder(r.Folder, homeFolder)
		case r.Path != "":
			tr.Path = portablePath(r.Path, homeDir)
		}
		t.Routes = append(t.Routes, tr)
	}
	return t
}

// InheritReport says what Inherit did not take over.
type InheritReport struct {
	Dropped []string // template rules not taken over, with the reason
	// Leaving lists the folders (by hash) and rule patterns whose route would
	// leave a client or orch repo, and a home folder newly pinned to a
	// non-client repo on a host that keeps client notes. Applying needs
	// --confirm-move-out-of-client.
	Leaving []string
}

// unavailablePrefix names the repo of a deny placeholder: a rule that stands in
// for one that could not be taken over. The router denies a rule naming a repo
// the host does not have, so the folders it matched stay denied instead of
// falling through to a broader rule behind it.
const unavailablePrefix = "unavailable:"

func (tr TemplateRule) expand(homeFolder string) (Rule, bool) {
	r := Rule{Folder: tr.Folder, Path: tr.Path, Remote: tr.Remote, Aliases: tr.Aliases, Repo: tr.Repo}
	if strings.HasPrefix(r.Folder, homePlaceholder) {
		if homeFolder == "" {
			return r, false
		}
		r.Folder = homeFolder + r.Folder[len(homePlaceholder):]
	}
	if strings.HasPrefix(r.Path, homePlaceholder) {
		r.Path = "~" + r.Path[len(homePlaceholder):]
	}
	return r, true
}

// Inherit builds this host's table from a template. Repos and every host
// setting stay the host's own; a rule is kept only when its repo exists here
// with the kind the template says. A rule that cannot be kept stays as a deny
// placeholder in its place, so a broader rule behind it never catches its
// folders. The template's home pin applies to homeFolder only when useHome is
// set; otherwise the host's own pin stays on top. The host's other rules are
// replaced; folders is what the host knows (for the Leaving check).
func (t Template) Inherit(cur File, homeFolder string, useHome bool, folders []string) (File, InheritReport, error) {
	var rep InheritReport
	var head []Rule
	var rest []Rule
	for _, tr := range t.Routes {
		desc := tr.describe()
		if tr.Home {
			kind, inTpl := t.Repos[tr.Repo]
			repo, here := cur.Repos[tr.Repo]
			switch {
			case !useHome:
				rep.Dropped = append(rep.Dropped, desc+": home pin not applied (pass --home to apply it to this host's home folder)")
			case homeFolder == "":
				rep.Dropped = append(rep.Dropped, desc+": this host's home folder is unknown")
			case !inTpl || !here || repo.Kind != kind:
				rep.Dropped = append(rep.Dropped, desc+": this host has no repo "+tr.Repo+" of that kind")
			default:
				head = []Rule{{Folder: homeFolder, Repo: tr.Repo}}
			}
			continue
		}
		r, ok := tr.expand(homeFolder)
		var why string
		kind, inTpl := t.Repos[tr.Repo]
		repo, here := cur.Repos[tr.Repo]
		switch {
		case !ok:
			why = "this host's home folder is unknown"
		case !inTpl:
			why = "the template does not say what " + tr.Repo + " is"
		case !here:
			why = "this host has no repo " + tr.Repo
		case repo.Kind != kind:
			why = fmt.Sprintf("repo %s is kind %s here, %s in the template (kind differs)", tr.Repo, repo.Kind, kind)
		default:
			if err := (File{Repos: cur.Repos, Personal: cur.Personal, Routes: []Rule{r}}).validate(); err != nil {
				why = strings.TrimPrefix(err.Error(), "route 0: ")
			}
		}
		if why != "" {
			rep.Dropped = append(rep.Dropped, desc+": "+why+" (kept as a deny)")
			r.Repo = unavailablePrefix + tr.Repo
		}
		rest = append(rest, r)
	}
	if head == nil && homeFolder != "" {
		for _, r := range cur.Routes {
			if r.isPin() && sameFolder(r.Folder, homeFolder) {
				head = []Rule{r}
				break
			}
		}
	}
	g := cur
	g.Routes = append(append([]Rule{}, head...), rest...)
	if err := g.validate(); err != nil {
		return cur, rep, err
	}
	rep.Leaving = leaving(cur, g, homeFolder, folders)
	return g, rep, nil
}

// probe is a folder to route under two tables. Synthetic probes stand for
// "a folder this rule matches": no checkout is read; the remote or path the
// probe carries (or one that matches no sane rule) is used instead.
type probe struct {
	label  string
	folder string
	synth  bool
	remote string
	path   string
}

func (f File) routeOf(p probe) string {
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	if p.synth {
		r.Resolve = func(string) (string, resolution) {
			if p.remote != "" {
				return p.remote, resolutionOK
			}
			return "probe.invalid/x/y", resolutionOK
		}
		r.Candidates = func(string) []string {
			if p.path != "" {
				return []string{expandHome(p.path)}
			}
			return []string{"/nonexistent/probe-dir"}
		}
	}
	return r.Decide(p.folder).Repo
}

// leaving compares the routes of every known folder, and of one probe per
// client or orch rule pattern, under both tables.
func leaving(cur, next File, home string, folders []string) []string {
	var ps []probe
	seen := map[string]bool{}
	add := func(p probe) {
		k := fmt.Sprintf("%v\x00%s\x00%s\x00%s\x00%v", p.synth, p.folder, p.remote, p.path, p.label)
		if !seen[k] {
			seen[k] = true
			ps = append(ps, p)
		}
	}
	for _, fo := range folders {
		add(probe{label: "folder " + hash8(fo), folder: fo})
	}
	if home != "" {
		add(probe{label: "home folder", folder: home})
	}
	for _, r := range cur.Routes {
		if !cur.Repos[r.Repo].IsClient() {
			continue
		}
		l := "rule " + ruleLine(r)
		switch r.kind() {
		case "folder":
			add(probe{label: l, folder: r.Folder, synth: true})
		case "path":
			add(probe{label: l, folder: "probe", synth: true, path: r.Path})
		default:
			add(probe{label: l, folder: "probe", synth: true, remote: r.Remote})
			for _, a := range r.Aliases {
				add(probe{label: l, folder: "probe", synth: true, remote: a})
			}
		}
	}
	var out []string
	for _, p := range ps {
		o, n := cur.routeOf(p), next.routeOf(p)
		if o == n {
			continue
		}
		name := func(x string) string {
			if x == "" {
				return "denied"
			}
			return x
		}
		switch {
		case cur.Repos[o].IsClient():
			out = append(out, fmt.Sprintf("%s: %s -> %s", p.label, o, name(n)))
		case p.label == "home folder" && o == "" && n != "" && !next.Repos[n].IsClient() && cur.HasClientRepo():
			out = append(out, fmt.Sprintf("home folder: denied -> %s (this host keeps client notes)", n))
		}
	}
	return out
}

func (tr TemplateRule) describe() string {
	if tr.Home {
		return "home folder -> " + tr.Repo
	}
	return ruleLine(Rule{Folder: tr.Folder, Path: tr.Path, Remote: tr.Remote, Repo: tr.Repo})
}

func ruleLine(r Rule) string { return r.kind() + " " + r.pattern() + " -> " + r.Repo }

// DiffRoutes lists the rules removed ("- ") and added ("+ ") going from a to b,
// and a note when only the order changed. It is empty when they are equal.
func DiffRoutes(a, b []Rule) []string {
	if sameRoutes(a, b) {
		return nil
	}
	count := func(rs []Rule) map[string]int {
		m := map[string]int{}
		for _, r := range rs {
			m[r.sig()]++
		}
		return m
	}
	cb := count(b)
	var out []string
	for _, r := range a {
		if cb[r.sig()] > 0 {
			cb[r.sig()]--
			continue
		}
		out = append(out, "- "+ruleLine(r))
	}
	ca2 := count(a)
	for _, r := range b {
		if ca2[r.sig()] > 0 {
			ca2[r.sig()]--
			continue
		}
		out = append(out, "+ "+ruleLine(r))
	}
	if len(out) == 0 {
		out = append(out, "~ same rules, new order (the first match wins)")
	}
	return out
}

func registryDir(f File, override string) (string, error) {
	d := override
	if d == "" {
		d = f.Registry
	}
	if d == "" {
		return "", fmt.Errorf(`no registry: set "registry" in routes.json (the registry repo's work-tree) or pass --registry`)
	}
	d = ExpandHome(d)
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return "", fmt.Errorf("registry %s is not a directory", d)
	}
	return d, nil
}

// PublishTemplate writes hosts/<label>/routes.json in the registry repo from
// the routes file at routesPath and commits that one file locally (the
// registry's own sync pushes it). It returns the file written. Nothing is
// committed when the template did not change.
func PublishTemplate(routesPath, registryOverride, label, homeFolder, homeDir string) (string, error) {
	if err := ValidLabel(label); err != nil {
		return "", err
	}
	f, err := Load(routesPath)
	if err != nil {
		return "", err
	}
	dir, err := registryDir(f, registryOverride)
	if err != nil {
		return "", err
	}
	if !gitwt.New(dir).IsRepo() {
		return "", fmt.Errorf("registry %s is not a git work-tree", dir)
	}
	rel := filepath.ToSlash(filepath.Join("hosts", label, "routes.json"))
	out := filepath.Join(dir, filepath.FromSlash(rel))
	if err := writeJSONAtomic(out, f.Template(homeFolder, homeDir), ".routes-*.tmp"); err != nil {
		return "", err
	}
	g := gitwt.New(dir)
	if _, err := g.Run("add", "--", rel); err != nil {
		return "", err
	}
	st, err := g.Run("status", "--porcelain", "--", rel)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(st) == "" {
		return out, nil
	}
	if _, err := g.Run("commit", "-q", "-m", "routing template for "+label, "--", rel); err != nil {
		return "", err
	}
	return out, nil
}

// ReadTemplate reads hosts/<label>/routes.json from the registry work-tree.
func ReadTemplate(registryDir, label string) (Template, error) {
	var t Template
	if err := ValidLabel(label); err != nil {
		return t, err
	}
	p := filepath.Join(registryDir, "hosts", label, "routes.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return t, fmt.Errorf("no template %q in the registry (%w); has it been published and pulled here?", label, err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("template %s: %w", label, err)
	}
	if t.Version != templateVersion {
		return t, fmt.Errorf("template %s: version %d is not supported", label, t.Version)
	}
	for n, k := range t.Repos {
		if !validKinds[k] {
			return t, fmt.Errorf("template %s: repo %q has unknown kind %q", label, n, k)
		}
	}
	return t, nil
}

// RegistryDir resolves the registry work-tree of f (or the override).
func RegistryDir(f File, override string) (string, error) { return registryDir(f, override) }

// SuggestHome proposes where the home folder should route on this host, from
// hints only, with the reason. "" means deny. It never changes anything.
// have reports whether a repo is configured on this host.
func SuggestHome(f File, goos string, have func(repo string) bool, folder string) (string, string) {
	if pin, ok := f.HomePin(folder); ok && f.isPersonal(pin) {
		return pin, "the home folder is already pinned to the personal repo; keep it"
	}
	if goos == "darwin" {
		if f.Personal != "" && have(f.Personal) {
			return f.Personal, "this is a Mac: the home folder goes to the personal repo"
		}
	}
	var cands []string
	for _, kind := range []string{KindOrch, KindInfra} {
		var names []string
		for n, r := range f.Repos {
			if r.Kind == kind && have(n) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		cands = append(cands, names...)
	}
	if len(cands) > 0 {
		n := cands[0]
		why := fmt.Sprintf("this host has the %s repo %s and no personal pin for the home folder", f.Repos[n].Kind, n)
		if len(cands) > 1 {
			why += fmt.Sprintf(" (%d such repos; first orch, then infra, by name)", len(cands))
		}
		return n, why
	}
	return "", "no personal repo on a Mac, no orch or infra repo on this host: deny the home folder (it stays denied by default)"
}
