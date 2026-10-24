package related

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Accepted is this host's pin of the registry edges it trusts. The registry is
// shared: an edge another machine (or a bad merge) added does nothing here
// until someone on this host accepts it, as with a recipient. The pin lives in
// the host's state, never in a synced repo.
type Accepted struct {
	Path  string
	Edges []Edge
}

type acceptedEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Mode Mode   `json:"mode"`
}

// LoadAccepted reads the pin; a missing file pins nothing.
func LoadAccepted(path string) (*Accepted, error) {
	a := &Accepted{Path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	var raw []acceptedEdge
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for _, r := range raw {
		a.Edges = append(a.Edges, Edge{r.From, r.To, r.Mode})
	}
	return a, nil
}

// Has reports an exact pin (same ends and mode).
func (a *Accepted) Has(e Edge) bool {
	for _, x := range a.Edges {
		if x == e {
			return true
		}
	}
	return false
}

// Add pins an edge, replacing a pin of the same ends with another mode.
func (a *Accepted) Add(e Edge) {
	a.Remove(e.From, e.To)
	a.Edges = append(a.Edges, e)
}

// Remove drops the pin of the ends.
func (a *Accepted) Remove(from, to string) {
	out := a.Edges[:0:0]
	for _, x := range a.Edges {
		if x.From != from || x.To != to {
			out = append(out, x)
		}
	}
	a.Edges = out
}

// Save writes the pin.
func (a *Accepted) Save() error {
	sort.Slice(a.Edges, func(i, j int) bool {
		if a.Edges[i].From != a.Edges[j].From {
			return a.Edges[i].From < a.Edges[j].From
		}
		return a.Edges[i].To < a.Edges[j].To
	})
	raw := make([]acceptedEdge, 0, len(a.Edges))
	for _, e := range a.Edges {
		raw = append(raw, acceptedEdge{e.From, e.To, e.Mode})
	}
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0700); err != nil {
		return err
	}
	return os.WriteFile(a.Path, append(b, '\n'), 0600)
}

// Split sorts the registry's allowed edges into the pinned ones and the ones
// still waiting for an accept.
func (a *Accepted) Split(edges []Edge) (ok, pending []Edge) {
	for _, e := range edges {
		if a.Has(e) {
			ok = append(ok, e)
		} else {
			pending = append(pending, e)
		}
	}
	return ok, pending
}

// Gone lists pins whose edge (same ends) is no longer in the registry.
func (a *Accepted) Gone(edges []Edge) []Edge {
	var out []Edge
	for _, p := range a.Edges {
		found := false
		for _, e := range edges {
			if e.From == p.From && e.To == p.To {
				found = true
			}
		}
		if !found {
			out = append(out, p)
		}
	}
	return out
}
