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

// Template builds the template of this table; homeFolder is this host's home
// folder, whose pin is published as a "home" rule.
func (f File) Template(homeFolder string) Template {
	t := Template{Version: templateVersion, Repos: map[string]string{}, Personal: f.Personal, Routes: []TemplateRule{}}
	for n, r := range f.Repos {
		t.Repos[n] = r.Kind
	}
	for _, r := range f.Routes {
		tr := TemplateRule{Folder: r.Folder, Path: r.Path, Remote: r.Remote, Aliases: r.Aliases, Repo: r.Repo}
		if r.isPin() && homeFolder != "" && strings.EqualFold(r.Folder, homeFolder) {
			tr = TemplateRule{Home: true, Repo: r.Repo}
		}
		t.Routes = append(t.Routes, tr)
	}
	return t
}

// InheritReport says what Inherit did not take over.
type InheritReport struct {
	Dropped []string // template rules left out, with the reason
	// LostClient lists this host's rules to client or orch repos that the new
	// table does not have.
	LostClient []string
	// HomeOut is set when the home folder would leave a client or orch repo for
	// a non-client one ("from -> to").
	HomeOut string
}

// Inherit builds this host's table from a template. Repos and every host
// setting stay the host's own; only rules whose repo exists here, with the kind
// the template says, are kept, so a client route is never added for a repo
// the host lacks. The template's home pin is applied to homeFolder only when
// useHome is set; otherwise the host's own home pin stays on top. The host's
// other existing rules are replaced.
func (t Template) Inherit(cur File, homeFolder string, useHome bool) (File, InheritReport, error) {
	var rep InheritReport
	var head []Rule
	var rest []Rule
	for _, tr := range t.Routes {
		desc := tr.describe()
		kind, inTpl := t.Repos[tr.Repo]
		repo, here := cur.Repos[tr.Repo]
		switch {
		case !inTpl:
			rep.Dropped = append(rep.Dropped, desc+": the template does not say what "+tr.Repo+" is")
			continue
		case !here:
			rep.Dropped = append(rep.Dropped, desc+": this host has no repo "+tr.Repo)
			continue
		case repo.Kind != kind:
			rep.Dropped = append(rep.Dropped, fmt.Sprintf("%s: repo %s is kind %s here, %s in the template (kind differs)", desc, tr.Repo, repo.Kind, kind))
			continue
		}
		if tr.Home {
			if !useHome {
				rep.Dropped = append(rep.Dropped, desc+": home pin not applied (pass --home to apply it to this host's home folder)")
				continue
			}
			if homeFolder == "" {
				rep.Dropped = append(rep.Dropped, desc+": this host's home folder is unknown")
				continue
			}
			head = []Rule{{Folder: homeFolder, Repo: tr.Repo}}
			continue
		}
		r := Rule{Folder: tr.Folder, Path: tr.Path, Remote: tr.Remote, Aliases: tr.Aliases, Repo: tr.Repo}
		if err := (File{Repos: cur.Repos, Personal: cur.Personal, Routes: []Rule{r}}).validate(); err != nil {
			rep.Dropped = append(rep.Dropped, desc+": "+strings.TrimPrefix(err.Error(), "route 0: "))
			continue
		}
		rest = append(rest, r)
	}
	if head == nil && homeFolder != "" {
		for _, r := range cur.Routes {
			if r.isPin() && strings.EqualFold(r.Folder, homeFolder) {
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
	have := map[string]bool{}
	for _, r := range g.Routes {
		have[r.sig()] = true
	}
	for _, r := range cur.Routes {
		if cur.Repos[r.Repo].IsClient() && !have[r.sig()] {
			rep.LostClient = append(rep.LostClient, ruleLine(r))
		}
	}
	if homeFolder != "" {
		from, to := cur.Effective(homeFolder), g.Effective(homeFolder)
		if cur.Repos[from].IsClient() && to != "" && !g.Repos[to].IsClient() {
			rep.HomeOut = from + " -> " + to
		}
	}
	return g, rep, nil
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
	if !gitwt.New(d).IsRepo() {
		return "", fmt.Errorf("registry %s is not a git work-tree", d)
	}
	return d, nil
}

// PublishTemplate writes hosts/<label>/routes.json in the registry repo from
// the routes file at routesPath and commits that one file locally (the
// registry's own sync pushes it). It returns the file written. Nothing is
// committed when the template did not change.
func PublishTemplate(routesPath, registryOverride, label, homeFolder string) (string, error) {
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
	rel := filepath.ToSlash(filepath.Join("hosts", label, "routes.json"))
	out := filepath.Join(dir, filepath.FromSlash(rel))
	if err := writeJSONAtomic(out, f.Template(homeFolder), ".routes-*.tmp"); err != nil {
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
