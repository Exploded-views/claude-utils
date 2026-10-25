// Package hosts is the lifecycle view of the machines that share memory repos:
// which deploy keys, recipients and registry entries belong to which host
// label, and which hosts have gone stale or expired.
//
// A host is known by its label (route.ValidLabel). Its deploy keys are titled
// with the label, its age recipient may carry a trailing "# host:<label>"
// comment, and the registry may hold hosts/<label>/. This package only joins
// those facts; it makes no network calls and holds no key material.
package hosts

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/ghguard"
)

// Defaults.
const (
	DefaultStaleDays    = 30
	DefaultCheckKeyWarn = 20
	// MaxAppKeys is how many private keys GitHub allows per App.
	MaxAppKeys = 25
)

// CheckKey is the public record of a host's check-App key: its fingerprint as
// GitHub shows it and the App's slug. Never the key.
type CheckKey struct {
	Fingerprint string `json:"fingerprint"`
	App         string `json:"app"`
}

// RegHost is what the registry holds under hosts/<label>/.
type RegHost struct {
	Label    string
	Expiry   time.Time // zero: none published
	CheckKey *CheckKey
	Problem  string // an unreadable expiry or check-key file, for display
}

// Input is everything Build joins.
type Input struct {
	Keys         map[string][]ghguard.DeployKey // mem-* repo -> its deploy keys
	Local        map[string][]string            // repos this host has -> labels in their recipients
	Registry     map[string]RegHost
	HaveRegistry bool
	Now          time.Time
	StaleDays    int // 0: DefaultStaleDays
	CheckKeyWarn int // 0: DefaultCheckKeyWarn
}

// KeyRef is one deploy key of a host.
type KeyRef struct {
	Repo      string
	ID        int64
	ReadOnly  bool
	CreatedAt time.Time
	LastUsed  time.Time
}

// Host is one label with everything known about it.
type Host struct {
	Label      string
	Keys       []KeyRef
	LastUsed   time.Time       // newest use of any of its keys
	Recipient  map[string]bool // per local repo: is a recipient line labelled with it present
	InRegistry bool
	Expiry     time.Time
	CheckKey   *CheckKey
	Problem    string
	Stale      bool
	Expired    bool
}

// AppKeys counts the live check keys of one App.
type AppKeys struct {
	App  string
	Live int
	Warn bool
}

// Report is the joined view.
type Report struct {
	Hosts         []Host
	LocalRepos    []string
	RegistryKnown bool
	CheckKeys     []AppKeys
	checkWarn     int
}

// Build joins the inputs into one Host per label.
func Build(in Input) Report {
	stale := in.StaleDays
	if stale <= 0 {
		stale = DefaultStaleDays
	}
	warnAt := in.CheckKeyWarn
	if warnAt <= 0 {
		warnAt = DefaultCheckKeyWarn
	}
	rep := Report{RegistryKnown: in.HaveRegistry, checkWarn: warnAt}
	byLabel := map[string]*Host{}
	get := func(l string) *Host {
		h := byLabel[l]
		if h == nil {
			h = &Host{Label: l, Recipient: map[string]bool{}}
			byLabel[l] = h
		}
		return h
	}
	for repo, keys := range in.Keys {
		for _, k := range keys {
			h := get(k.Title)
			h.Keys = append(h.Keys, KeyRef{Repo: repo, ID: k.ID, ReadOnly: k.ReadOnly, CreatedAt: k.CreatedAt, LastUsed: k.LastUsed})
		}
	}
	for repo, labels := range in.Local {
		rep.LocalRepos = append(rep.LocalRepos, repo)
		for _, l := range labels {
			get(l).Recipient[repo] = true
		}
	}
	sort.Strings(rep.LocalRepos)
	for l, r := range in.Registry {
		h := get(l)
		h.InRegistry = true
		h.Expiry, h.CheckKey, h.Problem = r.Expiry, r.CheckKey, r.Problem
	}
	today := in.Now.UTC().Truncate(24 * time.Hour)
	live := map[string]int{}
	for _, h := range byLabel {
		for _, repo := range rep.LocalRepos {
			h.Recipient[repo] = h.Recipient[repo] // present as false when absent
		}
		sort.Slice(h.Keys, func(i, j int) bool { return h.Keys[i].Repo < h.Keys[j].Repo })
		newest := time.Time{}
		used := false
		for _, k := range h.Keys {
			if !k.LastUsed.IsZero() {
				used = true
				if k.LastUsed.After(h.LastUsed) {
					h.LastUsed = k.LastUsed
				}
			}
			if k.CreatedAt.After(newest) {
				newest = k.CreatedAt
			}
		}
		if len(h.Keys) > 0 {
			ref := h.LastUsed
			if !used {
				ref = newest // never used: judge by how long ago it was created
			}
			h.Stale = !ref.IsZero() && in.Now.Sub(ref) > time.Duration(stale)*24*time.Hour
		}
		h.Expired = !h.Expiry.IsZero() && today.After(h.Expiry.UTC().Truncate(24*time.Hour))
		if h.InRegistry && h.CheckKey != nil && h.CheckKey.Fingerprint != "" {
			live[h.CheckKey.App]++
		}
		rep.Hosts = append(rep.Hosts, *h)
	}
	sort.Slice(rep.Hosts, func(i, j int) bool { return rep.Hosts[i].Label < rep.Hosts[j].Label })
	for app, n := range live {
		rep.CheckKeys = append(rep.CheckKeys, AppKeys{App: app, Live: n, Warn: n >= warnAt})
	}
	sort.Slice(rep.CheckKeys, func(i, j int) bool { return rep.CheckKeys[i].App < rep.CheckKeys[j].App })
	return rep
}

// Warnings are the check-key capacity warnings, one per App at or over the
// threshold.
func (r Report) Warnings() []string {
	var out []string
	for _, a := range r.CheckKeys {
		if a.Warn {
			out = append(out, fmt.Sprintf("check app %q: %d live keys (warning from %d); GitHub allows at most %d keys per app and they can only be deleted in the GitHub UI: retire hosts you no longer use",
				a.App, a.Live, r.checkWarn, MaxAppKeys))
		}
	}
	return out
}

// Summary is doctor's one line, or "" when nothing is stale or expired.
func (r Report) Summary() string {
	var stale, expired []string
	for _, h := range r.Hosts {
		if h.Stale {
			stale = append(stale, h.Label)
		}
		if h.Expired {
			expired = append(expired, h.Label)
		}
	}
	if len(stale) == 0 && len(expired) == 0 {
		return ""
	}
	s := fmt.Sprintf("hosts: %d stale", len(stale))
	if len(stale) > 0 {
		s += " (" + strings.Join(stale, ", ") + ")"
	}
	s += fmt.Sprintf(", %d expired", len(expired))
	if len(expired) > 0 {
		s += " (" + strings.Join(expired, ", ") + ")"
	}
	return s
}
