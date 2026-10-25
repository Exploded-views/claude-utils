package related

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadSessions_OnlyCwdAndTimestamps(t *testing.T) {
	dir := t.TempDir()
	huge := `{"timestamp":"2030-01-01T00:00:00Z","cwd":"/x","message":"` + strings.Repeat("a", maxTranscriptLine+10) + `"}`
	writeFile(t, dir, "s1.jsonl", strings.Join([]string{
		`{"type":"user","cwd":"/work/a","timestamp":"2026-10-01T10:00:00.000Z","message":{"content":"sk-secret-ABC123"}}`,
		`not json at all`,
		`{"type":"summary","summary":"no timestamp"}`,
		huge,
		`{"type":"assistant","cwd":"/work/a","timestamp":"2026-10-01T11:30:00Z"}`,
	}, "\n")+"\n")
	writeFile(t, dir, "notes.txt", "ignored")
	got, err := ReadSessions(dir)
	if err != nil || len(got) != 1 {
		t.Fatalf("sessions %v err %v", got, err)
	}
	s := got[0]
	if s.Cwd != "/work/a" || !s.Start.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) ||
		!s.End.Equal(time.Date(2026, 10, 1, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("session %+v (an oversize line must be skipped)", s)
	}
}

func TestCheckoutRefs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitmodules", "[submodule \"x\"]\n\tpath = x\n\turl = git@github.com:Acme/Lib-One.git\n[submodule \"y\"]\n\turl = https://gitlab.example.com/grp/lib-two.git\n")
	writeFile(t, dir, "go.mod", "module github.com/acme/self\n\ngo 1.22\n\nrequire (\n\tgithub.com/acme/lib-three/v2 v2.1.0\n\texample.org/zzz v1.0.0\n)\n\nreplace github.com/acme/old => github.com/acme/lib-four v1.0.0\nreplace github.com/acme/loc => ../local\n")
	writeFile(t, dir, "package.json", `{"name":"p","dependencies":{"a":"github:acme/lib-five","b":"git+https://github.com/acme/lib-six.git#v1","c":"^1.2.3","d":"acme/lib-seven"},"devDependencies":{"e":"git+ssh://git@github.com/acme/lib-eight.git"}}`)
	writeFile(t, dir, "README.md", "See [docs](https://github.com/acme/lib-nine) and https://example.com/not-github.\n")
	refs := CheckoutRefs(dir)
	have := map[string]string{}
	for _, r := range refs {
		have[r.Key] = r.File
	}
	want := map[string]string{
		"github.com/acme/lib-one":        ".gitmodules",
		"gitlab.example.com/grp/lib-two": ".gitmodules",
		"github.com/acme/lib-three":      "go.mod",
		"github.com/acme/lib-four":       "go.mod",
		"github.com/acme/old":            "go.mod",
		"github.com/acme/loc":            "go.mod",
		"github.com/acme/lib-five":       "package.json",
		"github.com/acme/lib-six":        "package.json",
		"github.com/acme/lib-seven":      "package.json",
		"github.com/acme/lib-eight":      "package.json",
		"github.com/acme/lib-nine":       "README.md",
	}
	for k, f := range want {
		if have[k] != f {
			t.Errorf("ref %s in %q, want %q (all: %v)", k, have[k], f, have)
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected ref %s", k)
		}
	}
}

func TestTokens(t *testing.T) {
	got := Tokens([]string{"# Clays and Trap scoring", "## The rules (feedback)", "# abc 1234 Round-robin"})
	for _, w := range []string{"clays", "trap", "scoring", "round", "robin"} {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	for _, w := range []string{"and", "the", "abc", "rules", "feedback", "1234"} {
		if got[w] {
			t.Errorf("%q should be ignored", w)
		}
	}
}

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func sess(day int, hour int, dur time.Duration) Session {
	s := t0.AddDate(0, 0, day).Add(time.Duration(hour-9) * time.Hour)
	return Session{Start: s, End: s.Add(dur)}
}

func names(c []Candidate) []string {
	var out []string
	for _, x := range c {
		out = append(out, x.From+">"+x.To)
	}
	return out
}

func TestSuggest_CoOpen(t *testing.T) {
	a := RepoFacts{Name: "mem-project-a", Sessions: []Session{sess(0, 9, time.Hour), sess(3, 9, time.Hour), sess(9, 9, time.Hour)}}
	b := RepoFacts{Name: "mem-project-b", Sessions: []Session{sess(0, 20, time.Hour), sess(3, 8, time.Hour)}}
	r := Suggest([]RepoFacts{a, b}, SuggestOptions{Checker: Checker{}})
	if len(r.Suggestions) != 1 || r.Suggestions[0].Signals[0].Kind != SignalCoOpen {
		t.Fatalf("got %+v", r)
	}
	if r.Suggestions[0].From != "mem-project-a" || r.Suggestions[0].To != "mem-project-b" {
		t.Fatalf("direction %+v", r.Suggestions[0])
	}
	// only one day of overlap: nothing
	b1 := RepoFacts{Name: "mem-project-b", Sessions: []Session{sess(0, 20, time.Hour)}}
	if r := Suggest([]RepoFacts{a, b1}, SuggestOptions{Checker: Checker{}}); len(r.Suggestions) != 0 {
		t.Fatalf("one day gave %+v", r.Suggestions)
	}
	// two days but more than the window apart
	far := RepoFacts{Name: "mem-project-b", Sessions: []Session{sess(1, 12, time.Hour), sess(4, 12, time.Hour)}}
	if r := Suggest([]RepoFacts{a, far}, SuggestOptions{Checker: Checker{}}); len(r.Suggestions) != 0 {
		t.Fatalf("sessions 27h apart gave %+v", r.Suggestions)
	}
	// ... unless the window is wider
	if r := Suggest([]RepoFacts{a, far}, SuggestOptions{Checker: Checker{}, WindowHours: 48}); len(r.Suggestions) != 1 {
		t.Fatalf("48h window: %+v", r.Suggestions)
	}
	// overlapping in time counts even when the starts are far apart
	long := RepoFacts{Name: "mem-project-b", Sessions: []Session{sess(0, 0, 30*time.Hour), sess(3, 0, 30*time.Hour)}}
	long.Sessions[0].Start = t0.Add(-30 * time.Hour)
	long.Sessions[0].End = t0.Add(10 * time.Hour)
	long.Sessions[1].Start = t0.AddDate(0, 0, 3).Add(-40 * time.Hour)
	long.Sessions[1].End = t0.AddDate(0, 0, 3).Add(time.Hour)
	if r := Suggest([]RepoFacts{a, long}, SuggestOptions{Checker: Checker{}, WindowHours: 1}); len(r.Suggestions) != 1 {
		t.Fatalf("overlap: %+v", r.Suggestions)
	}
}

func TestSuggest_References(t *testing.T) {
	a := RepoFacts{Name: "mem-project-a", Keys: []string{"github.com/o/a"}, Refs: []Ref{{Key: "github.com/o/b", File: "go.mod"}}}
	b := RepoFacts{Name: "mem-project-b", Keys: []string{"github.com/o/b"}}
	r := Suggest([]RepoFacts{b, a}, SuggestOptions{Checker: Checker{}})
	if len(r.Suggestions) != 1 || r.Suggestions[0].From != "mem-project-a" || r.Suggestions[0].To != "mem-project-b" ||
		r.Suggestions[0].Signals[0].Kind != SignalReference || !strings.Contains(r.Suggestions[0].Signals[0].Detail, "github.com/o/b") {
		t.Fatalf("got %+v", r.Suggestions)
	}
	// a reference to something that is not a routed repo, or to itself
	a.Refs = []Ref{{Key: "github.com/o/zzz", File: "go.mod"}, {Key: "github.com/o/a", File: "README.md"}}
	if r := Suggest([]RepoFacts{b, a}, SuggestOptions{Checker: Checker{}}); len(r.Suggestions) != 0 {
		t.Fatalf("got %+v", r.Suggestions)
	}
}

func TestSuggest_SameOwnerVocabulary(t *testing.T) {
	mk := func(name, owner string, heads ...string) RepoFacts {
		return RepoFacts{Name: name, Keys: []string{"github.com/" + owner + "/" + name}, Headings: heads}
	}
	a := mk("mem-project-a", "me", "# Clays leaderboard", "## Trap scoring")
	b := mk("mem-project-b", "me", "# Scoring for trap and clays")
	r := Suggest([]RepoFacts{a, b}, SuggestOptions{Checker: Checker{}})
	if len(r.Suggestions) != 1 || r.Suggestions[0].Signals[0].Kind != SignalVocabulary {
		t.Fatalf("got %+v", r.Suggestions)
	}
	// one shared word only
	b1 := mk("mem-project-b", "me", "# Clays only")
	if r := Suggest([]RepoFacts{a, b1}, SuggestOptions{Checker: Checker{}}); len(r.Suggestions) != 0 {
		t.Fatalf("one shared token: %+v", r.Suggestions)
	}
	// different owner
	b2 := mk("mem-project-b", "other", "# Scoring for trap and clays")
	if r := Suggest([]RepoFacts{a, b2}, SuggestOptions{Checker: Checker{}}); len(r.Suggestions) != 0 {
		t.Fatalf("different owner: %+v", r.Suggestions)
	}
	// shared words that are stopwords or short
	b3 := mk("mem-project-b", "me", "# The rules and the notes for abc")
	a3 := mk("mem-project-a", "me", "# The rules and the notes for abc")
	if r := Suggest([]RepoFacts{a3, b3}, SuggestOptions{Checker: Checker{}}); len(r.Suggestions) != 0 {
		t.Fatalf("stopwords: %+v", r.Suggestions)
	}
}

// pair with every signal, used by the policy and filter tests.
func allSignals(from, to string) []RepoFacts {
	a := RepoFacts{Name: from, Keys: []string{"github.com/o/a"}, Refs: []Ref{{Key: "github.com/o/b", File: "go.mod"}},
		Sessions: []Session{sess(0, 9, time.Hour), sess(2, 9, time.Hour)}, Headings: []string{"# clays trap"}}
	b := RepoFacts{Name: to, Keys: []string{"github.com/o/b"},
		Sessions: []Session{sess(0, 10, time.Hour), sess(2, 10, time.Hour)}, Headings: []string{"# clays trap"}}
	return []RepoFacts{a, b}
}

func TestSuggest_PolicyWithholds(t *testing.T) {
	nodes := map[string]Node{
		"mem-client-x": {Name: "mem-client-x", Kind: "client", Client: "x"},
		"mem-client-y": {Name: "mem-client-y", Kind: "client", Client: "y"},
	}
	c := Checker{Lookup: func(n string) (Node, bool) { v, ok := nodes[n]; return v, ok }}
	for _, tc := range [][2]string{
		{"mem-client-x", "mem-project-own"}, // client <-> own
		{"mem-project-own", "mem-client-x"},
		{"mem-client-x", "mem-client-y"}, // cross client
		{"mem-personal", "mem-orch-x"},   // orch outside the oversight edge
	} {
		r := Suggest(allSignals(tc[0], tc[1]), SuggestOptions{Checker: c})
		if len(r.Suggestions) != 0 || r.Withheld != 1 {
			t.Errorf("%v: suggestions %+v withheld %d", tc, r.Suggestions, r.Withheld)
		}
	}
	// the reverse direction of a symmetric pair is tried: orch-all reads orch-x
	co := allSignals("mem-orch-all", "mem-orch-aaa")
	co[0].Refs = nil
	co[0].Headings, co[1].Headings = nil, nil
	r := Suggest(co, SuggestOptions{Checker: c})
	if len(r.Suggestions) != 1 || r.Suggestions[0].From != OrchAll {
		t.Fatalf("orch-all: %+v withheld %d", r.Suggestions, r.Withheld)
	}
	// two client repos of the same client are fine
	same := map[string]Node{"mem-client-x-web": {Kind: "client", Client: "x"}, "mem-client-x-api": {Kind: "client", Client: "x"}}
	c2 := Checker{Lookup: func(n string) (Node, bool) { v, ok := same[n]; v.Name = n; return v, ok }}
	if r := Suggest(allSignals("mem-client-x-web", "mem-client-x-api"), SuggestOptions{Checker: c2}); len(r.Suggestions) != 1 {
		t.Fatalf("same client: %+v", r)
	}
}

func TestSuggest_SkipsAcceptedDeclaredDismissedAndMode(t *testing.T) {
	facts := allSignals("mem-project-a", "mem-project-b")
	r := Suggest(facts, SuggestOptions{Checker: Checker{}})
	if len(r.Suggestions) != 1 || r.Suggestions[0].Mode != ModeRead {
		t.Fatalf("baseline %+v", r)
	}
	e := Edge{"mem-project-a", "mem-project-b", ModeRead}
	for name, o := range map[string]SuggestOptions{
		"accepted":  {Accepted: []Edge{e}},
		"declared":  {Declared: []Edge{{"mem-project-a", "mem-project-b", ModeJoin}}},
		"dismissed": {Dismissed: [][2]string{{"mem-project-b", "mem-project-a"}}},
	} {
		o.Checker = Checker{}
		if r := Suggest(facts, o); len(r.Suggestions) != 0 || r.Withheld != 0 {
			t.Errorf("%s still suggested: %+v", name, r)
		}
	}
	// a symmetric pair already related the other way is not suggested back
	if r := Suggest(allSignalsNoRef(), SuggestOptions{Checker: Checker{}, Accepted: []Edge{{"mem-project-b", "mem-project-a", ModeRead}}}); len(r.Suggestions) != 0 {
		t.Fatalf("reverse of accepted: %+v", r.Suggestions)
	}
}

func allSignalsNoRef() []RepoFacts {
	f := allSignals("mem-project-a", "mem-project-b")
	f[0].Refs = nil
	return f
}

func TestSuggest_ScoreOrderAndMin(t *testing.T) {
	strong := allSignals("mem-project-a", "mem-project-b")
	weak := []RepoFacts{
		{Name: "mem-project-c", Keys: []string{"github.com/p/c"}, Headings: []string{"# alpha bravo"}},
		{Name: "mem-project-d", Keys: []string{"github.com/p/d"}, Headings: []string{"# alpha bravo"}},
	}
	r := Suggest(append(strong, weak...), SuggestOptions{Checker: Checker{}})
	if len(r.Suggestions) != 2 || r.Suggestions[0].From != "mem-project-a" || r.Suggestions[0].Score <= r.Suggestions[1].Score {
		t.Fatalf("order %+v", names(r.Suggestions))
	}
	r2 := Suggest(append(allSignals("mem-project-a", "mem-project-b"), weak...), SuggestOptions{Checker: Checker{}, MinScore: r.Suggestions[1].Score + 1})
	if len(r2.Suggestions) != 1 {
		t.Fatalf("min score: %+v", r2.Suggestions)
	}
	// order of signals within one suggestion: co-open, reference, vocabulary
	var kinds []SignalKind
	for _, s := range r.Suggestions[0].Signals {
		kinds = append(kinds, s.Kind)
	}
	if len(kinds) != 3 || kinds[0] != SignalCoOpen || kinds[1] != SignalReference || kinds[2] != SignalVocabulary {
		t.Fatalf("signal order %v", kinds)
	}
}
