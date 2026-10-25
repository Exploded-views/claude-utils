package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/related"
)

// suggestCacheTTL is how long the session-start count may reuse the signals of
// an earlier run. Policy, acceptance and dismissal are applied afresh each time.
const suggestCacheTTL = time.Hour

func dismissedPath(routesPath string) string {
	return filepath.Join(filepath.Dir(routesPath), "related-dismissed.json")
}

func suggestCachePath(routesPath string) string {
	return filepath.Join(filepath.Dir(routesPath), "related-suggest-cache.json")
}

// gatherFacts reads this host's own data for every routed repo: the sessions
// and checkouts of the folders routed to it, and its notes headings. Nothing
// leaves the host and nothing but times, remote keys, matched references and
// headings is kept.
func (rc *relCtx) gatherFacts() []related.RepoFacts {
	byRepo := map[string]*related.RepoFacts{}
	seen := map[string]bool{}
	var dirs []string
	for _, k := range rc.set.names() {
		d := rc.set.Cfgs[k].ClaudeProjectsDir
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, pd := range dirs {
		ents, err := os.ReadDir(pd)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			folder := e.Name()
			d := rc.set.Router.Decide(folder)
			if d.Repo == "" {
				continue
			}
			name := rc.set.remoteName(d.Repo)
			f := byRepo[name]
			if f == nil {
				f = &related.RepoFacts{Name: name}
				byRepo[name] = f
			}
			if ss, err := related.ReadSessions(filepath.Join(pd, folder)); err == nil {
				f.Sessions = append(f.Sessions, ss...)
			}
			if c := rc.set.Router.Candidates(folder); len(c) == 1 {
				if key, _ := rc.set.Router.Resolve(folder); key != "" {
					f.Keys = append(f.Keys, key)
				}
				f.Refs = append(f.Refs, related.CheckoutRefs(c[0])...)
			}
		}
	}
	var out []related.RepoFacts
	for name, f := range byRepo {
		if k, ok := rc.set.localKey(name); ok {
			if ix, err := related.BuildIndex(rc.set.Cfgs[k].SyncDir, rc.opener()); err == nil {
				f.Headings = ix.Headings
			}
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// suggestFilter is the part of the options that comes from the host's state.
func (rc *relCtx) suggestFilter(minScore int) (related.SuggestOptions, error) {
	dis, err := related.LoadDismissed(dismissedPath(rc.set.Path))
	if err != nil {
		return related.SuggestOptions{}, fmt.Errorf("related-dismissed.json: %w", err)
	}
	return related.SuggestOptions{
		Checker:   rc.checker,
		MinScore:  minScore,
		Accepted:  rc.acc.Edges,
		Declared:  rc.reg.Edges(),
		Dismissed: dis.Pairs,
	}, nil
}

type suggestCache struct {
	At    time.Time
	Cands []related.Candidate
}

func (rc *relCtx) writeSuggestCache(c []related.Candidate) {
	b, err := json.Marshal(suggestCache{At: time.Now(), Cands: c})
	if err != nil {
		return
	}
	p := suggestCachePath(rc.set.Path)
	if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

func (rc *relCtx) readSuggestCache() ([]related.Candidate, bool) {
	b, err := os.ReadFile(suggestCachePath(rc.set.Path))
	if err != nil {
		return nil, false
	}
	var c suggestCache
	if json.Unmarshal(b, &c) != nil || time.Since(c.At) > suggestCacheTTL || time.Since(c.At) < -time.Minute {
		return nil, false
	}
	return c.Cands, true
}

// shown is the suggestions that may be printed, with the denylist applied: a
// suggestion whose endpoints or evidence match the denylist of either end's
// repo is counted and dropped.
func (rc *relCtx) shown(res related.Result) (out []related.Candidate, hidden int) {
	hiders := map[string]func(string) bool{}
	hide := func(repo, text string) bool {
		h, ok := hiders[repo]
		if !ok {
			h = rc.hider(repo)
			hiders[repo] = h
		}
		return h(text)
	}
	for _, c := range res.Suggestions {
		var parts []string
		parts = append(parts, c.From, c.To)
		for _, s := range c.Signals {
			parts = append(parts, s.Detail)
		}
		text := strings.Join(parts, "\n")
		if hide(c.From, text) || hide(c.To, text) {
			hidden++
			continue
		}
		out = append(out, c)
	}
	return out, hidden
}

// suggestions computes the visible suggestions. A cached run is used when
// useCache is set and one is fresh.
func (rc *relCtx) suggestions(window time.Duration, minDays, minScore int, useCache bool) (vis []related.Candidate, withheld, hidden int, err error) {
	o, err := rc.suggestFilter(minScore)
	if err != nil {
		return nil, 0, 0, err
	}
	cands, ok := []related.Candidate(nil), false
	if useCache {
		cands, ok = rc.readSuggestCache()
	}
	if !ok {
		cands = related.Raw(rc.gatherFacts(), window, minDays)
		if window == 24*time.Hour && minDays == 2 {
			rc.writeSuggestCache(cands)
		}
	}
	res := related.Filter(cands, o)
	vis, hidden = rc.shown(res)
	return vis, res.Withheld, hidden, nil
}

type suggestJSON struct {
	From    string       `json:"from"`
	To      string       `json:"to"`
	Mode    related.Mode `json:"mode"`
	Score   int          `json:"score"`
	Signals []signalJSON `json:"signals"`
}

type signalJSON struct {
	Kind   related.SignalKind `json:"kind"`
	Detail string             `json:"detail"`
}

func relatedSuggestCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("related suggest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routesFlag := fs.String("routes", "", "path to routes.json")
	asJSON := fs.Bool("json", false, "print JSON")
	minScore := fs.Int("min-score", 0, "only suggestions scoring at least this")
	hours := fs.Int("co-open-hours", 24, "sessions of two projects opened within this many hours count as co-opened")
	if fs.Parse(args) != nil {
		return 2
	}
	if *hours <= 0 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: claude-memsync related suggest [--json] [--min-score N] [--co-open-hours N] [--routes FILE]")
		return 2
	}
	rc, err := loadRelCtx(*routesFlag)
	if err != nil {
		fmt.Fprintln(stderr, "related suggest:", err)
		return 1
	}
	rc.stderr = stderr
	rc.vis.quick = true
	vis, withheld, hidden, err := rc.suggestions(time.Duration(*hours)*time.Hour, 2, *minScore, false)
	if err != nil {
		fmt.Fprintln(stderr, "related suggest:", err)
		return 1
	}
	if *asJSON {
		out := struct {
			Suggestions []suggestJSON `json:"suggestions"`
			Withheld    int           `json:"withheld"`
			Hidden      int           `json:"hidden"`
		}{Suggestions: []suggestJSON{}, Withheld: withheld, Hidden: hidden}
		for _, c := range vis {
			s := suggestJSON{From: c.From, To: c.To, Mode: c.Mode, Score: c.Score, Signals: []signalJSON{}}
			for _, g := range c.Signals {
				s.Signals = append(s.Signals, signalJSON{g.Kind, g.Detail})
			}
			out.Suggestions = append(out.Suggestions, s)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	for _, c := range vis {
		fmt.Fprintf(stdout, "%s -> %s (%s)  score %d\n", c.From, c.To, c.Mode, c.Score)
		for _, s := range c.Signals {
			fmt.Fprintf(stdout, "  - %s: %s\n", s.Kind, s.Detail)
		}
	}
	head := "no suggestions"
	if len(vis) > 0 {
		head = fmt.Sprintf("%d suggestions", len(vis))
	}
	if withheld > 0 {
		head += fmt.Sprintf("; %d withheld by policy", withheld)
	}
	if hidden > 0 {
		head += fmt.Sprintf("; %d hidden by denylist", hidden)
	}
	fmt.Fprintln(stdout, head)
	if len(vis) > 0 {
		fmt.Fprintln(stdout, "adopt one with: claude-memsync related accept <from> <to> [--share]; wave one away with: claude-memsync related dismiss <from> <to>")
	}
	return 0
}

// suggestionLine is the SessionStart line for the owning repo: how many
// suggestions involve it. Nothing when there are none.
func (rc *relCtx) suggestionLine(owner string) string {
	vis, _, _, err := rc.suggestions(24*time.Hour, 2, 0, true)
	if err != nil {
		return ""
	}
	n := 0
	for _, c := range vis {
		if c.From == owner || c.To == owner {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d related-memory suggestions; run `claude-memsync related suggest`", n)
}

func relatedDismissCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("related dismiss", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routesFlag := fs.String("routes", "", "path to routes.json")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 2 {
		fmt.Fprintln(stderr, "usage: claude-memsync related dismiss <from> <to> [--routes FILE]")
		return 2
	}
	for _, r := range pos {
		if err := (related.NameOnly{}).CheckPrivate(r); err != nil {
			fmt.Fprintln(stderr, "related dismiss:", err)
			return 1
		}
	}
	rc, err := loadRelCtx(*routesFlag)
	if err != nil {
		fmt.Fprintln(stderr, "related dismiss:", err)
		return 1
	}
	dis, err := related.LoadDismissed(dismissedPath(rc.set.Path))
	if err != nil {
		fmt.Fprintln(stderr, "related dismiss:", err)
		return 1
	}
	dis.Add(pos[0], pos[1])
	if err := dis.Save(); err != nil {
		fmt.Fprintln(stderr, "related dismiss:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s <-> %s dismissed on this host\n", pos[0], pos[1])
	return 0
}

// acceptOne adopts one edge as a read relation on this host: the policy is
// checked again, the edge is pinned here, and only with share is it also
// written (and committed, never pushed) to the registry.
func acceptOne(stdout, stderr io.Writer, rc *relCtx, from, to string, share bool) int {
	if err := rc.checker.Check(from, to, related.ModeRead); err != nil {
		fmt.Fprintln(stderr, "related accept: refused:", err)
		return 1
	}
	e := related.Edge{From: from, To: to, Mode: related.ModeRead}
	if share {
		if err := rc.reg.Relate(rc.checker, from, to, related.ModeRead); err != nil {
			fmt.Fprintln(stderr, "related accept: refused:", err)
			return 1
		}
		if err := rc.reg.Save(fmt.Sprintf("relate: %s -> %s (%s)", from, to, related.ModeRead)); err != nil {
			fmt.Fprintln(stderr, "related accept:", err)
			return 1
		}
	}
	rc.acc.Add(e)
	if err := rc.acc.Save(); err != nil {
		fmt.Fprintln(stderr, "related accept:", err)
		return 1
	}
	if share {
		fmt.Fprintf(stdout, "%s -> %s (read) accepted on this host and recorded in the registry (committed; the registry's sync pushes it)\n", from, to)
	} else {
		fmt.Fprintf(stdout, "%s -> %s (read) accepted on this host only; add --share to record it in the registry\n", from, to)
	}
	return 0
}
