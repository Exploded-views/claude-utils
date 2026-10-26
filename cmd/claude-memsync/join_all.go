package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// joinAll is `join accept|reject|list --all`: the command for every routed
// repo of this host that is encrypted. See docs/setup.md for the policy.
func joinAll(stdout, stderr io.Writer, action string, sel repoSel, label, fp string, allowUnlabelled bool) int {
	if sel.Repo != "" || sel.Config != "" {
		fmt.Fprintln(stderr, "join: --all does not go with --repo or --config")
		return 2
	}
	switch action {
	case "accept", "reject", "list":
	default:
		fmt.Fprintf(stderr, "join: --all applies to accept, reject and list\n%s\n", joinUsage)
		return 2
	}
	if action == "accept" && agecrypt.NormalizeFingerprint(fp) == "" {
		fmt.Fprintln(stderr, "join accept: refusing to accept without --fingerprint: compare it with the one the requesting host printed")
		return 2
	}
	path, ok, err := resolveRoutesPath(sel.Routes)
	if err != nil || !ok {
		fmt.Fprintln(stderr, "join: --all needs a routes file")
		return 1
	}
	set, err := loadRouted(path)
	if err != nil {
		fmt.Fprintln(stderr, "join:", err)
		return 1
	}
	if action == "list" {
		return joinListAll(stdout, stderr, set)
	}
	return joinChangeAll(stdout, stderr, set, action, label, fp, allowUnlabelled)
}

// joinListAll prints one table: repo, label, fingerprint, requested, host.
func joinListAll(stdout, stderr io.Writer, set *routedSet) int {
	code, rows := 0, 0
	for _, n := range set.names() {
		repo := gitwt.New(set.Cfgs[n].SyncDir)
		if !repo.IsRepo() || !agecrypt.Enabled(repo) {
			continue
		}
		joinFetch(repo)
		es, err := agecrypt.ListJoins(repo)
		if err != nil {
			fmt.Fprintf(stderr, "join list: %s: %v\n", n, err)
			code = 1
			continue
		}
		sort.Slice(es, func(i, k int) bool { return es[i].Path < es[k].Path })
		own, _ := agecrypt.IdentityRecipient(agecrypt.IdentityPath(repo))
		for _, e := range es {
			rows++
			if !e.Valid {
				fmt.Fprintf(stdout, "%s\t%s\tINVALID (will not be accepted; reject it)\t-\n", n, e.Join.Label)
				continue
			}
			tail := "\thost:" + e.Join.Label
			if own != "" && e.Join.Recipient == own {
				tail += "\tthis host"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s%s\n", n, e.Join.Label, agecrypt.GroupFingerprint(e.Join.Fingerprint), e.Join.Requested.Format(time.RFC3339), tail)
		}
	}
	if rows == 0 && code == 0 {
		fmt.Fprintln(stdout, "no join requests pending")
	}
	return code
}

// joinRepoPlan is what --all found in one repo before anything is changed.
type joinRepoPlan struct {
	name   string
	cfg    config.Config
	entry  agecrypt.JoinEntry
	found  bool
	status string // set when the repo is settled without being changed
	fail   bool   // that status leaves the label's request unanswered
	do     bool   // to be accepted or rejected
}

func joinChangeAll(stdout, stderr io.Writer, set *routedSet, action, label, fp string, allowUnlabelled bool) int {
	accept := action == "accept"
	var plans []*joinRepoPlan
	var mismatched []string
	found := 0
	for _, n := range set.names() {
		p := &joinRepoPlan{name: n, cfg: set.Cfgs[n]}
		plans = append(plans, p)
		repo := gitwt.New(p.cfg.SyncDir)
		if !repo.IsRepo() || !agecrypt.Enabled(repo) {
			p.status = "skipped (unencrypted)"
			continue
		}
		joinFetch(repo)
		es, err := agecrypt.ListJoins(repo)
		if err != nil {
			p.status, p.fail = "refused ("+err.Error()+")", true
			continue
		}
		for _, e := range es {
			if e.Join.Label == label {
				p.entry, p.found = e, true
			}
		}
		if !p.found {
			p.status = "no request"
			continue
		}
		found++
		switch {
		case accept && p.entry.Valid && agecrypt.NormalizeFingerprint(fp) != p.entry.Join.Fingerprint:
			mismatched = append(mismatched, n)
			p.status = "fingerprint mismatch (the request here is " + agecrypt.GroupFingerprint(p.entry.Join.Fingerprint) + "); not accepted"
			p.fail = true
		case p.cfg.ReadOnly:
			p.status, p.fail = "skipped (read-only: this host never commits)", true
		default:
			if why, blocked := agecrypt.RekeyBlocksSync(repo); blocked {
				p.status, p.fail = "skipped (rekey-blocked: "+why+")", true
			} else if accept && !p.entry.Valid {
				p.status, p.fail = "refused (the request is malformed; reject it)", true
			} else {
				p.do = true
			}
		}
	}

	// A request under the same label with a different key, in any repo, may be
	// a second key: nothing is accepted anywhere.
	if len(mismatched) > 0 {
		for _, p := range plans {
			if p.do {
				p.do, p.fail = false, true
				p.status = "not accepted (the request from " + label + " in " + strings.Join(mismatched, ", ") + " has a different fingerprint)"
			}
		}
		fmt.Fprintf(stderr, "join accept --all: refusing every repo: the request from %q in %s has a different fingerprint than --fingerprint, which may mean a second key under one label. Check with the person who runs the new host, or `join list --all`; reject the stray request, then run this again.\n", label, strings.Join(mismatched, ", "))
	}

	var dirs []string
	for _, p := range plans {
		if p.do {
			dirs = append(dirs, p.cfg.SyncDir)
		}
	}
	code := 0
	if len(dirs) > 0 {
		// One stop and one start of the service for the whole set.
		err := withSyncLocksStopping(stderr, dirs, func() error {
			for _, p := range plans {
				if !p.do {
					continue
				}
				if err := joinChangeOne(p, label, fp, allowUnlabelled, accept); err != nil {
					p.status, p.fail = err.Error(), true
				}
			}
			return nil
		})
		if err != nil {
			for _, p := range plans {
				if p.do && p.status == "" {
					p.status, p.fail = "refused ("+err.Error()+")", true
				}
			}
			code = recipientsFail(stderr, fmt.Errorf("join: %w", err))
		}
	}
	done := 0
	for _, p := range plans {
		if p.status == "" {
			if accept {
				p.status = "accepted and pushed"
			} else {
				p.status = "rejected and pushed"
			}
			done++
		}
		fmt.Fprintf(stdout, "%s: %s\n", p.name, p.status)
		if p.fail {
			code = max(code, 1)
		}
	}
	if found == 0 {
		fmt.Fprintf(stderr, "join %s --all: no join request from %s in any repo on this host (has it been pulled? see `join list --all`)\n", action, label)
		return 1
	}
	if accept && done > 0 {
		fmt.Fprintln(stdout, "the new host pulls, then runs `claude-memsync recipients accept --yes`")
	}
	return code
}

// joinChangeOne accepts or rejects in one repo whose lock is held. A non-nil
// error is the repo's status line; "" status means it went through.
func joinChangeOne(p *joinRepoPlan, label, fp string, allowUnlabelled, accept bool) error {
	repo := gitwt.New(p.cfg.SyncDir)
	if err := agecrypt.SyncFromOrigin(repo); err != nil {
		return fmt.Errorf("refused (could not bring this clone up to date with origin; nothing was changed: %v)", err)
	}
	if err := agecrypt.WriteInfoAttributes(repo); err != nil {
		return fmt.Errorf("refused (%v)", err)
	}
	var err error
	if accept {
		err = agecrypt.AcceptJoin(repo, label, fp, agecrypt.AcceptOpts{OwnLabel: p.cfg.HostLabel, AllowUnlabelled: allowUnlabelled, RecoveryKeys: p.cfg.RecoveryRecipients})
	} else {
		err = agecrypt.RejectJoin(repo, label)
	}
	if err != nil {
		return fmt.Errorf("refused (%v)", err)
	}
	if err := joinPush(p.cfg, repo, false); err != nil {
		return fmt.Errorf("committed locally but the push failed (%v); the daemon pushes it on its next cycle", err)
	}
	return nil
}
