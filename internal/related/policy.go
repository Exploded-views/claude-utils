// Package related implements related projects: relations between memory repos,
// declared in the registry repo, and read-only access to the repos they name.
//
// A relation is default deny. Only the edges Check allows exist, and Check runs
// both when a relation is declared and whenever the registry is loaded for a
// sync, so a hand-edited registry cannot open a boundary.
package related

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Mode is how a repo relates to another.
type Mode string

const (
	// ModeRead: a fetch-only clone under the related root, never mirrored.
	ModeRead Mode = "read"
	// ModeJoin: the host must become a full member; nothing is automated.
	ModeJoin Mode = "join"
)

// Repo kinds, as in routes.json.
const (
	kindPersonal = "personal"
	kindProject  = "project"
	kindInfra    = "infra"
	kindClient   = "client"
	kindOrch     = "orch"
)

// OrchAll is the all-work orchestrator's repo: the only repo that may relate
// across a client boundary, and only by read, to a mem-orch-<client> repo.
const OrchAll = "mem-orch-all"

var nameRe = regexp.MustCompile(`^mem-[a-z0-9][a-z0-9._-]*$`)

var errPublic = errors.New("repo is not private")

// Node is a repo as the policy sees it.
type Node struct {
	Name   string
	Kind   string
	Client string
}

// NodeFromName derives a node from the naming scheme alone. The kind is ""
// for a name that is not a repo kind a relation may touch (mem-registry).
func NodeFromName(name string) Node {
	n := Node{Name: name}
	switch {
	case name == "mem-personal":
		n.Kind = kindPersonal
	case strings.HasPrefix(name, "mem-project-"):
		n.Kind = kindProject
	case strings.HasPrefix(name, "mem-infra-"):
		n.Kind = kindInfra
	case strings.HasPrefix(name, "mem-client-"):
		n.Kind, n.Client = kindClient, strings.TrimPrefix(name, "mem-client-")
	case strings.HasPrefix(name, "mem-orch-"):
		n.Kind, n.Client = kindOrch, strings.TrimPrefix(name, "mem-orch-")
	}
	return n
}

// Visibility is the private-only check on a repo. A separate branch supplies
// the real one (it asks the host whether the repo is private).
type Visibility interface {
	CheckPrivate(repo string) error
}

// NameOnly is the default Visibility.
//
// PLACEHOLDER: it checks the mem-* name only and cannot tell a public repo
// from a private one. The private-only implementation replaces it.
type NameOnly struct{}

// CheckPrivate accepts any mem-* name.
func (NameOnly) CheckPrivate(repo string) error {
	if !nameRe.MatchString(repo) {
		return fmt.Errorf("%q is not a mem-* repo name", repo)
	}
	return nil
}

// Checker decides which relations may exist. The zero value works: names only.
type Checker struct {
	// Lookup gives the routes.json entry of a repo name, if the host has one:
	// it may name the kind and the client explicitly.
	Lookup func(name string) (Node, bool)
	// Visible is the private-only check (default NameOnly).
	Visible Visibility
}

func (c Checker) node(name string) (Node, error) {
	n := NodeFromName(name)
	if c.Lookup != nil {
		if e, ok := c.Lookup(name); ok {
			if n.Kind != "" && e.Kind != "" && e.Kind != n.Kind {
				return n, fmt.Errorf("%s is named like a %s repo but declared as %s", name, n.Kind, e.Kind)
			}
			if n.Kind == "" {
				n.Kind = e.Kind
			}
			if e.Client != "" {
				n.Client = e.Client
			}
		}
	}
	return n, nil
}

// Check returns nil when the relation from -> to in the given mode may exist.
func (c Checker) Check(from, to string, mode Mode) error {
	if mode != ModeRead && mode != ModeJoin {
		return fmt.Errorf("mode must be read or join, not %q", mode)
	}
	vis := c.Visible
	if vis == nil {
		vis = NameOnly{}
	}
	for _, r := range []string{from, to} {
		if !nameRe.MatchString(r) {
			return fmt.Errorf("%q: relations only join mem-* repos (and the name must be a plain repo name)", r)
		}
		if err := vis.CheckPrivate(r); err != nil {
			return fmt.Errorf("%s: %w", r, err)
		}
	}
	if from == to {
		return fmt.Errorf("%s cannot relate to itself", from)
	}
	a, err := c.node(from)
	if err != nil {
		return err
	}
	b, err := c.node(to)
	if err != nil {
		return err
	}
	if from == OrchAll {
		switch {
		case b.Kind == kindClient:
			return fmt.Errorf("%s may not relate to a client repo (%s); it only reads mem-orch-<client> repos", from, to)
		case b.Kind != kindOrch || to == OrchAll:
			return fmt.Errorf("%s may only relate to a mem-orch-<client> repo, not %s", from, to)
		case mode != ModeRead:
			return fmt.Errorf("%s -> %s must be read: the all-work orchestrator never joins a client's repos", from, to)
		}
		return nil
	}
	if a.Kind == kindOrch || b.Kind == kindOrch {
		return fmt.Errorf("%s -> %s: orch repos relate only as mem-orch-all -> mem-orch-<client>, read", from, to)
	}
	if a.Kind == "" || b.Kind == "" {
		return fmt.Errorf("%s -> %s: a relation joins personal, project, infra or same-client repos; one end has no usable kind", from, to)
	}
	if a.Kind == kindClient || b.Kind == kindClient {
		switch {
		case a.Kind != b.Kind:
			return fmt.Errorf("%s -> %s: a client repo relates only to repos of the same client, never to a %s repo", from, to, otherKind(a, b))
		case a.Client == "" || a.Client != b.Client:
			return fmt.Errorf("%s -> %s: client repos of different clients never relate", from, to)
		}
	}
	if mode == ModeJoin && a.Kind != b.Kind {
		return fmt.Errorf("%s -> %s: join across kinds (%s, %s) is refused", from, to, a.Kind, b.Kind)
	}
	return nil
}

func otherKind(a, b Node) string {
	if a.Kind == kindClient {
		return b.Kind
	}
	return a.Kind
}
