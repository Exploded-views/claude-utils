package related

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// FileName is the registry file inside the registry repo.
const FileName = "related.json"

// Relation is one related repo and how.
type Relation struct {
	Repo string `json:"repo"`
	Mode Mode   `json:"mode"`
}

// Entry is one repo and the repos it relates to.
type Entry struct {
	Repo    string     `json:"repo"`
	Related []Relation `json:"related"`
}

// Edge is one relation, flattened.
type Edge struct {
	From, To string
	Mode     Mode
}

// Refusal is an edge in the file that the policy does not allow.
type Refusal struct {
	Edge Edge
	Err  error
}

// Registry is related.json of the registry repo.
type Registry struct {
	Dir     string // the registry repo's work-tree
	Entries []Entry
}

// Path is the registry file.
func (r *Registry) Path() string { return filepath.Join(r.Dir, FileName) }

// LoadRegistry reads the registry; a missing file is an empty registry.
func LoadRegistry(dir string) (*Registry, error) {
	r := &Registry{Dir: dir}
	b, err := os.ReadFile(r.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &r.Entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", r.Path(), err)
	}
	return r, nil
}

// Edges flattens the registry, sorted.
func (r *Registry) Edges() []Edge {
	var out []Edge
	for _, e := range r.Entries {
		for _, rel := range e.Related {
			out = append(out, Edge{e.Repo, rel.Repo, rel.Mode})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// Active splits the edges into the ones the policy allows and the ones it
// refuses. This is the load-time enforcement: a hand-edited registry gains
// nothing, every consumer works from Active.
func (r *Registry) Active(c Checker) (ok []Edge, refused []Refusal) {
	for _, e := range r.Edges() {
		if err := c.Check(e.From, e.To, e.Mode); err != nil {
			refused = append(refused, Refusal{e, err})
			continue
		}
		ok = append(ok, e)
	}
	return ok, refused
}

// Relate adds or changes a relation after the policy allows it. It does not
// write; call Save.
func (r *Registry) Relate(c Checker, from, to string, mode Mode) error {
	if err := c.Check(from, to, mode); err != nil {
		return err
	}
	for i := range r.Entries {
		if r.Entries[i].Repo != from {
			continue
		}
		for j := range r.Entries[i].Related {
			if r.Entries[i].Related[j].Repo == to {
				r.Entries[i].Related[j].Mode = mode
				return nil
			}
		}
		r.Entries[i].Related = append(r.Entries[i].Related, Relation{to, mode})
		return nil
	}
	r.Entries = append(r.Entries, Entry{from, []Relation{{to, mode}}})
	return nil
}

// Unrelate removes a relation; false when there was none.
func (r *Registry) Unrelate(from, to string) bool {
	for i := range r.Entries {
		if r.Entries[i].Repo != from {
			continue
		}
		for j := range r.Entries[i].Related {
			if r.Entries[i].Related[j].Repo == to {
				rel := r.Entries[i].Related
				r.Entries[i].Related = append(rel[:j:j], rel[j+1:]...)
				if len(r.Entries[i].Related) == 0 {
					r.Entries = append(r.Entries[:i:i], r.Entries[i+1:]...)
				}
				return true
			}
		}
	}
	return false
}

// Save writes related.json (sorted, stable) and commits it in the registry
// repo. It never pushes: the registry is a normal sync dir and its daemon does.
func (r *Registry) Save(message string) error {
	sort.Slice(r.Entries, func(i, j int) bool { return r.Entries[i].Repo < r.Entries[j].Repo })
	for i := range r.Entries {
		rel := r.Entries[i].Related
		sort.Slice(rel, func(a, b int) bool { return rel[a].Repo < rel[b].Repo })
	}
	entries := r.Entries
	if entries == nil {
		entries = []Entry{}
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(r.Path(), append(b, '\n'), 0600); err != nil {
		return err
	}
	g := gitwt.New(r.Dir)
	if !g.IsRepo() {
		return fmt.Errorf("%s is not a git repo; the registry must be a sync dir", r.Dir)
	}
	if _, err := g.Run("add", "--", gitwt.Literal(FileName)); err != nil {
		return err
	}
	_, err = g.Commit(message)
	return err
}
