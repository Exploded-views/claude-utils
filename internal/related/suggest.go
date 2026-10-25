package related

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// SignalKind names why an edge is suggested, strongest first.
type SignalKind string

const (
	SignalCoOpen     SignalKind = "co-open"
	SignalReference  SignalKind = "reference"
	SignalVocabulary SignalKind = "vocabulary"
)

func (s Signal) same() Signal { s.Safe = s.Detail; return s }

var signalRank = map[SignalKind]int{SignalCoOpen: 0, SignalReference: 1, SignalVocabulary: 2}

// Signal is one piece of evidence for a candidate edge.
type Signal struct {
	Kind   SignalKind
	Detail string
	// Safe is the same evidence without words or module paths, for a viewer
	// who may not see those (a client context).
	Safe string
}

// Candidate is a suggested edge. The mode is always read.
type Candidate struct {
	From, To string
	Mode     Mode
	Score    int
	Signals  []Signal
	// Directed: a reference gave the direction. Otherwise the pair is
	// symmetric and the direction was chosen by name and policy.
	Directed bool
}

// RepoFacts is what this host knows about one routed repo, all of it computed
// from its own data: transcripts, checkouts, notes headings.
type RepoFacts struct {
	Name     string    // the repo's remote name (mem-*)
	Sessions []Session // sessions of the folders routed to it
	Keys     []string  // remote keys (host/owner/repo) of those folders' checkouts
	Refs     []Ref     // what those checkouts point at
	Headings []string  // MEMORY.md headings of the repo
}

// SuggestOptions are the inputs and filters of Suggest.
type SuggestOptions struct {
	Checker     Checker
	WindowHours int // co-open window (default 24)
	MinDays     int // distinct days a pair must be co-opened (default 2)
	MinScore    int
	Accepted    []Edge
	Declared    []Edge
	Dismissed   [][2]string
	// Scope, when set, is the viewing repo: only candidates with it as an end
	// are considered, and a refusal is counted only when the other end may be
	// known to the viewer (see MayCount).
	Scope string
}

// Result is the outcome of Suggest. Withheld counts candidates the policy
// refused; they are never listed.
type Result struct {
	Suggestions []Candidate
	Withheld    int
}

// Raw computes the candidate edges from the facts, before any filter: no
// policy, acceptance or dismissal has been applied.
func Raw(facts []RepoFacts, window time.Duration, minDays int) []Candidate {
	if minDays <= 0 {
		minDays = 2
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Name < facts[j].Name })

	keyOwner := map[string][]string{}
	for _, f := range facts {
		for _, k := range f.Keys {
			keyOwner[strings.ToLower(k)] = append(keyOwner[strings.ToLower(k)], f.Name)
		}
	}
	directed := map[[2]string]*Candidate{}
	var order [][2]string
	get := func(from, to string) *Candidate {
		k := [2]string{from, to}
		if c, ok := directed[k]; ok {
			return c
		}
		c := &Candidate{From: from, To: to, Mode: ModeRead, Directed: true}
		directed[k] = c
		order = append(order, k)
		return c
	}
	for _, f := range facts {
		for _, r := range f.Refs {
			for _, to := range keyOwner[strings.ToLower(r.Key)] {
				if to == f.Name {
					continue
				}
				c := get(f.Name, to)
				d := fmt.Sprintf("%s names %s", r.File, r.Key)
				dup := false
				for _, s := range c.Signals {
					dup = dup || s.Detail == d
				}
				if !dup {
					c.Signals = append(c.Signals, Signal{Kind: SignalReference, Detail: d, Safe: r.File + " names another routed repo"})
				}
			}
		}
	}

	sym := map[[2]string]*Candidate{}
	var symOrder [][2]string
	attach := func(a, b string, s Signal) {
		if c, ok := directed[[2]string{a, b}]; ok {
			c.Signals = append(c.Signals, s)
			return
		}
		if c, ok := directed[[2]string{b, a}]; ok {
			c.Signals = append(c.Signals, s)
			return
		}
		k := [2]string{a, b}
		c := sym[k]
		if c == nil {
			c = &Candidate{From: a, To: b, Mode: ModeRead}
			sym[k] = c
			symOrder = append(symOrder, k)
		}
		c.Signals = append(c.Signals, s)
	}
	toks := make([]map[string]bool, len(facts))
	for i, f := range facts {
		toks[i] = Tokens(f.Headings)
	}
	for i := range facts {
		for j := i + 1; j < len(facts); j++ {
			a, b := facts[i], facts[j]
			if n := coOpenDays(a.Sessions, b.Sessions, window); n >= minDays {
				attach(a.Name, b.Name, Signal{Kind: SignalCoOpen, Detail: fmt.Sprintf("co-opened on %d days (sessions within %dh of each other)", n, int(window/time.Hour))}.same())
			}
			if owner := sharedOwner(a.Keys, b.Keys); owner != "" {
				var shared []string
				for t := range toks[i] {
					if toks[j][t] {
						shared = append(shared, t)
					}
				}
				sort.Strings(shared)
				if len(shared) >= 2 {
					attach(a.Name, b.Name, Signal{Kind: SignalVocabulary, Detail: fmt.Sprintf("same owner %s, shared terms: %s", owner, strings.Join(firstN(shared, 5), ", ")), Safe: fmt.Sprintf("same remote owner, %d shared terms", len(shared))})
				}
			}
		}
	}
	var out []Candidate
	for _, k := range order {
		out = append(out, *directed[k])
	}
	for _, k := range symOrder {
		out = append(out, *sym[k])
	}
	for i := range out {
		sort.SliceStable(out[i].Signals, func(a, b int) bool {
			return signalRank[out[i].Signals[a].Kind] < signalRank[out[i].Signals[b].Kind]
		})
		out[i].Score = score(out[i].Signals)
	}
	return out
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sharedOwner(a, b []string) string {
	owners := map[string]bool{}
	for _, k := range a {
		if o := ownerOfKey(k); o != "" {
			owners[o] = true
		}
	}
	var hit []string
	for _, k := range b {
		if o := ownerOfKey(k); o != "" && owners[o] {
			hit = append(hit, o)
		}
	}
	sort.Strings(hit)
	if len(hit) == 0 {
		return ""
	}
	return hit[0]
}

// ownerOfKey is the host/owner part of a host/owner/repo key.
func ownerOfKey(k string) string {
	parts := strings.Split(strings.ToLower(k), "/")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

var signalBase = map[SignalKind]int{SignalCoOpen: 60, SignalReference: 40, SignalVocabulary: 10}

// score: the strongest signal sets the base, its strength adds up to 30, and
// each further kind of signal adds 5; at most 100.
func score(sigs []Signal) int {
	best, kinds := 0, map[SignalKind]int{}
	for _, s := range sigs {
		kinds[s.Kind]++
	}
	for k, n := range kinds {
		v := signalBase[k] + 5*minInt(n, 4)
		if k == SignalCoOpen {
			v = signalBase[k] + 5*minInt(coOpenDaysIn(sigs), 6)
		}
		if v > best {
			best = v
		}
	}
	best += 5 * (len(kinds) - 1)
	if best > 100 {
		best = 100
	}
	return best
}

func coOpenDaysIn(sigs []Signal) int {
	for _, s := range sigs {
		if s.Kind == SignalCoOpen {
			var n int
			fmt.Sscanf(s.Detail, "co-opened on %d days", &n)
			return n
		}
	}
	return 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Filter drops what is already related, accepted or dismissed, then withholds
// what the policy refuses (counted, never listed), then applies the minimum
// score. Every listed suggestion has passed Checker.Check in read mode.
func Filter(cands []Candidate, o SuggestOptions) Result {
	have := func(a, b string, both bool) bool {
		for _, es := range [][]Edge{o.Accepted, o.Declared} {
			for _, e := range es {
				if (e.From == a && e.To == b) || (both && e.From == b && e.To == a) {
					return true
				}
			}
		}
		return false
	}
	var res Result
	for _, c := range cands {
		if o.Scope != "" && c.From != o.Scope && c.To != o.Scope {
			continue
		}
		sym := !c.Directed
		if have(c.From, c.To, sym) || dismissed(o.Dismissed, c.From, c.To) {
			continue
		}
		from, to := c.From, c.To
		err := o.Checker.Check(from, to, ModeRead)
		if err != nil && sym {
			// The pair has no natural direction: try the other way.
			from, to = to, from
			err = o.Checker.Check(from, to, ModeRead)
		}
		if err != nil {
			if o.Scope == "" || o.Checker.MayCount(o.Scope, otherEnd(c, o.Scope)) {
				res.Withheld++
			}
			continue
		}
		c.From, c.To = from, to
		if c.Score < o.MinScore {
			continue
		}
		res.Suggestions = append(res.Suggestions, c)
	}
	sort.SliceStable(res.Suggestions, func(i, j int) bool {
		a, b := res.Suggestions[i], res.Suggestions[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	return res
}

func otherEnd(c Candidate, scope string) string {
	if c.From == scope {
		return c.To
	}
	return c.From
}

// Passing keeps the candidates the policy allows (in either direction for a
// symmetric pair). The cache holds only these.
func Passing(cands []Candidate, ck Checker) []Candidate {
	var out []Candidate
	for _, c := range cands {
		ok := ck.Check(c.From, c.To, ModeRead) == nil
		if !ok && !c.Directed {
			ok = ck.Check(c.To, c.From, ModeRead) == nil
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// Strip drops the evidence, keeping only the signal kinds and the score, so a
// stored candidate carries no word or module path.
func Strip(cands []Candidate) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		s := Candidate{From: c.From, To: c.To, Mode: c.Mode, Score: c.Score, Directed: c.Directed}
		for _, g := range c.Signals {
			s.Signals = append(s.Signals, Signal{Kind: g.Kind})
		}
		out = append(out, s)
	}
	return out
}

func isClientKind(n Node) bool { return n.Kind == kindClient || n.Kind == kindOrch }

// MayCount reports whether the viewer may be told that a refused candidate
// with the other end exists: not when the other end is a client or orch repo
// of another client (or when either end cannot be classified).
func (c Checker) MayCount(viewer, other string) bool {
	v, err := c.node(viewer)
	if err != nil {
		return false
	}
	o, err := c.node(other)
	if err != nil {
		return false
	}
	if !isClientKind(o) {
		return true
	}
	return isClientKind(v) && v.Kind == o.Kind && sameClient(v, o)
}

// Evidence is the text of a candidate's signals for the viewer. With no viewer
// (the host-wide listing) it is the full text. For a candidate with a client or
// orch end the vocabulary is only counted, never listed, and module paths are
// shown only when the viewer is of the same client.
func Evidence(c Candidate, ck Checker, viewer string) []Signal {
	redact, refs := false, true
	if viewer != "" {
		v, verr := ck.node(viewer)
		for _, end := range []string{c.From, c.To} {
			n, err := ck.node(end)
			if err != nil || verr != nil || isClientKind(n) {
				redact = true
				if verr != nil || err != nil || !isClientKind(v) || v.Kind != n.Kind || !sameClient(v, n) {
					refs = false
				}
			}
		}
	}
	var out []Signal
	for _, s := range c.Signals {
		t := s.Detail
		switch {
		case redact && s.Kind == SignalVocabulary:
			t = s.Safe
		case redact && s.Kind == SignalReference && !refs:
			t = s.Safe
		}
		if t == "" {
			t = string(s.Kind)
		}
		out = append(out, Signal{Kind: s.Kind, Detail: t})
	}
	return out
}

func dismissed(d [][2]string, a, b string) bool {
	for _, p := range d {
		if (p[0] == a && p[1] == b) || (p[0] == b && p[1] == a) {
			return true
		}
	}
	return false
}

// Suggest computes the candidates from the facts and filters them.
func Suggest(facts []RepoFacts, o SuggestOptions) Result {
	h := o.WindowHours
	if h <= 0 {
		h = 24
	}
	return Filter(Raw(facts, time.Duration(h)*time.Hour, o.MinDays), o)
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`about above after again also another because been before being between both could does done each else from have here into just like make many more most much must never only other over same should since some such than that their them then there these they this those through under until upon very was were what when where which while will with within without would your
		memory memories note notes index project projects feedback reference rules rule user users session sessions claude agent agents using used uses use work working file files repo repos README MEMORY type name description`) {
		m[strings.ToLower(w)] = true
	}
	return m
}()

// Tokens are the distinctive words of some headings: lower case, at least four
// letters, not a stopword, not a number.
func Tokens(headings []string) map[string]bool {
	out := map[string]bool{}
	for _, h := range headings {
		for _, w := range strings.FieldsFunc(strings.ToLower(h), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if len([]rune(w)) < 4 || stopwords[w] || allDigits(w) {
				continue
			}
			out[w] = true
		}
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Dismissed is this host's list of suggestions the user waved away. It lives
// in the host's state beside the accepted-edges pin and is never synced.
type Dismissed struct {
	Path  string
	Pairs [][2]string
}

type dismissedPair struct {
	A string `json:"a"`
	B string `json:"b"`
}

// LoadDismissed reads the list; a missing file dismisses nothing.
func LoadDismissed(path string) (*Dismissed, error) {
	d := &Dismissed{Path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	var raw []dismissedPair
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for _, r := range raw {
		d.Pairs = append(d.Pairs, [2]string{r.A, r.B})
	}
	return d, nil
}

// Add records a dismissal of the pair (either direction).
func (d *Dismissed) Add(a, b string) {
	if !dismissed(d.Pairs, a, b) {
		d.Pairs = append(d.Pairs, [2]string{a, b})
	}
}

// Save writes the list.
func (d *Dismissed) Save() error {
	raw := make([]dismissedPair, 0, len(d.Pairs))
	for _, p := range d.Pairs {
		raw = append(raw, dismissedPair{p[0], p[1]})
	}
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].A != raw[j].A {
			return raw[i].A < raw[j].A
		}
		return raw[i].B < raw[j].B
	})
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.Path), 0700); err != nil {
		return err
	}
	return os.WriteFile(d.Path, append(b, '\n'), 0600)
}
