package related

import (
	"encoding/json"
	"io"
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

func coFacts(names ...string) []RepoFacts {
	var out []RepoFacts
	for _, n := range names {
		out = append(out, RepoFacts{Name: n, Sessions: []Session{sess(0, 9, time.Hour), sess(2, 9, time.Hour)}})
	}
	return out
}

func clientChecker() Checker {
	nodes := map[string]Node{
		"mem-client-x-web": {Kind: "client", Client: "x"},
		"mem-client-x-api": {Kind: "client", Client: "x"},
		"mem-client-x":     {Kind: "client", Client: "x"},
		"mem-client-y":     {Kind: "client", Client: "y"},
	}
	return Checker{Lookup: func(n string) (Node, bool) { v, ok := nodes[n]; v.Name = n; return v, ok }}
}

func TestSuggest_ScopePersonalViewerSeesNothingOfClientPairs(t *testing.T) {
	facts := coFacts("mem-personal", "mem-project-p", "mem-client-x", "mem-client-y")
	r := Suggest(facts, SuggestOptions{Checker: clientChecker(), Scope: "mem-personal"})
	if len(r.Suggestions) != 1 || r.Suggestions[0].To != "mem-project-p" && r.Suggestions[0].From != "mem-project-p" {
		t.Fatalf("got %+v", r.Suggestions)
	}
	if r.Withheld != 0 {
		t.Fatalf("a client pair was counted for a personal viewer: %d", r.Withheld)
	}
	// host-wide, the same candidates are counted
	if all := Suggest(facts, SuggestOptions{Checker: clientChecker()}); all.Withheld != 5 {
		t.Fatalf("host-wide withheld %d", all.Withheld)
	}
}

func TestSuggest_ScopeClientViewerSeesOnlyOwnClient(t *testing.T) {
	facts := coFacts("mem-client-x-web", "mem-client-x-api", "mem-client-y", "mem-project-p", "mem-personal")
	r := Suggest(facts, SuggestOptions{Checker: clientChecker(), Scope: "mem-client-x-web"})
	if len(r.Suggestions) != 1 || r.Suggestions[0].From+r.Suggestions[0].To != "mem-client-x-webmem-client-x-api" && r.Suggestions[0].From+r.Suggestions[0].To != "mem-client-x-apimem-client-x-web" {
		t.Fatalf("got %v", names(r.Suggestions))
	}
	// project and personal pairs are known to the client viewer; the other client is not counted
	if r.Withheld != 2 {
		t.Fatalf("withheld %d, want 2 (project, personal; not client y)", r.Withheld)
	}
}

func TestMayCount(t *testing.T) {
	c := clientChecker()
	for _, tc := range []struct {
		v, o string
		want bool
	}{
		{"mem-personal", "mem-project-p", true},
		{"mem-personal", "mem-client-x", false},
		{"mem-project-p", "mem-orch-x", false},
		{"mem-client-x-web", "mem-client-y", false},
		{"mem-client-x-web", "mem-client-x-api", true},
		{"mem-client-x-web", "mem-project-p", true},
	} {
		if got := c.MayCount(tc.v, tc.o); got != tc.want {
			t.Errorf("MayCount(%s,%s)=%v", tc.v, tc.o, got)
		}
	}
}

func TestEvidence_RedactsInClientContexts(t *testing.T) {
	c := Candidate{From: "mem-client-x-web", To: "mem-client-x-api", Signals: []Signal{
		{Kind: SignalReference, Detail: "go.mod names github.com/xo/secretmod", Safe: "go.mod names another routed repo"},
		{Kind: SignalVocabulary, Detail: "same owner o, shared terms: secretword, other", Safe: "same remote owner, 2 shared terms"},
	}}
	ck := clientChecker()
	same := evText(Evidence(c, ck, "mem-client-x-web"))
	if !strings.Contains(same, "secretmod") || strings.Contains(same, "secretword") || !strings.Contains(same, "2 shared terms") {
		t.Fatalf("same client viewer:\n%s", same)
	}
	cross := Candidate{From: "mem-orch-all", To: "mem-orch-x", Signals: c.Signals}
	other := evText(Evidence(cross, ck, "mem-orch-all"))
	if strings.Contains(other, "secretmod") || strings.Contains(other, "secretword") {
		t.Fatalf("orch-all viewer:\n%s", other)
	}
	all := evText(Evidence(c, ck, ""))
	if !strings.Contains(all, "secretmod") || !strings.Contains(all, "secretword") {
		t.Fatalf("host-wide:\n%s", all)
	}
}

func TestSuggest_DirectedRefusalIsNotFlipped(t *testing.T) {
	// orch-x refers to orch-all: the reverse (orch-all reads orch-x) would pass
	// the policy, but a reference is directional and must not be turned around.
	x := RepoFacts{Name: "mem-orch-x", Keys: []string{"github.com/o/x"}, Refs: []Ref{{Key: "github.com/o/all", File: "go.mod"}}}
	all := RepoFacts{Name: "mem-orch-all", Keys: []string{"github.com/o/all"}}
	r := Suggest([]RepoFacts{x, all}, SuggestOptions{Checker: clientChecker()})
	if len(r.Suggestions) != 0 || r.Withheld != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestStripAndPassing(t *testing.T) {
	facts := append(allSignals("mem-project-a", "mem-project-b"), coFacts("mem-client-x", "mem-project-q")...)
	raw := Raw(facts, 24*time.Hour, 2)
	kept := Passing(raw, clientChecker())
	for _, c := range kept {
		if strings.Contains(c.From+c.To, "client") {
			t.Fatalf("a refused candidate was kept: %+v", c)
		}
	}
	stripped := Strip(kept)
	b, _ := json.Marshal(stripped)
	for _, bad := range []string{"github.com", "clays", "trap", "go.mod", "co-opened", "shared terms"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("evidence %q in the stored form: %s", bad, b)
		}
	}
	if len(stripped) == 0 || stripped[0].Score == 0 || len(stripped[0].Signals) == 0 {
		t.Fatalf("kinds and scores must stay: %s", b)
	}
}

func TestSessionIndex_IncrementalByMtimeAndOffset(t *testing.T) {
	dir := t.TempDir()
	line := func(ts string) string { return `{"cwd":"/w","timestamp":"` + ts + `","message":"x"}` + "\n" }
	writeFile(t, dir, "s.jsonl", line("2026-10-01T10:00:00Z"))
	opens := 0
	orig := openTranscript
	openTranscript = func(p string) (io.ReadSeekCloser, error) { opens++; return os.Open(p) }
	defer func() { openTranscript = orig }()
	ix := &SessionIndex{}
	ss, ok := ix.Update(dir, "f", time.Time{})
	if !ok || len(ss) != 1 || opens != 1 {
		t.Fatalf("first: %v %v opens %d", ss, ok, opens)
	}
	if _, _ = ix.Update(dir, "f", time.Time{}); opens != 1 {
		t.Fatalf("an unchanged file was reopened (%d)", opens)
	}
	// append a complete line and a partial one
	f, _ := os.OpenFile(filepath.Join(dir, "s.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(line("2026-10-01T12:00:00Z") + `{"timestamp":"2026-10-01T13:00:`)
	f.Close()
	ss, _ = ix.Update(dir, "f", time.Time{})
	if opens != 2 || !ss[0].End.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("append: %+v opens %d", ss, opens)
	}
	st := ix.Files["f/s.jsonl"]
	if st.Offset != int64(len(line("2026-10-01T10:00:00Z")+line("2026-10-01T12:00:00Z"))) {
		t.Fatalf("offset %d counts the partial line", st.Offset)
	}
	// the partial line is completed: read from the offset only
	f, _ = os.OpenFile(filepath.Join(dir, "s.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`00Z","cwd":"/w"}` + "\n")
	f.Close()
	ss, _ = ix.Update(dir, "f", time.Time{})
	if !ss[0].End.Equal(time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("completed line not seen: %+v", ss)
	}
	// persisted and reloaded: still not reopened
	p := filepath.Join(t.TempDir(), "ix.json")
	if err := ix.Save(p); err != nil {
		t.Fatal(err)
	}
	opens = 0
	LoadSessionIndex(p).Update(dir, "f", time.Time{})
	if opens != 0 {
		t.Fatalf("reloaded index reopened the file")
	}
	// a passed deadline reads nothing new
	writeFile(t, dir, "t.jsonl", line("2026-10-02T10:00:00Z"))
	ix2 := &SessionIndex{}
	if _, ok := ix2.Update(dir, "f", time.Now().Add(-time.Second)); ok || opens != 0 {
		t.Fatalf("deadline ignored: ok=%v opens=%d", ok, opens)
	}
}

func evText(sg []Signal) string {
	var parts []string
	for _, g := range sg {
		parts = append(parts, string(g.Kind)+": "+g.Detail)
	}
	return strings.Join(parts, "\n")
}
