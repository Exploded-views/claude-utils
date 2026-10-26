package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
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
	if action != "accept" && fp != "" {
		fmt.Fprintf(stderr, "join %s: --fingerprint goes with accept only; --all %s needs none\n", action, action)
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

// joinRepoPlan is what --all found in one repo.
type joinRepoPlan struct {
	name     string
	cfg      config.Config
	status   string // the repo's line; "" until it is settled
	fail     bool   // the label's request is left unanswered here
	do       bool   // to be accepted or rejected
	ran      bool   // the change was attempted
	accepted bool
}

// classifyRequests says whether any version of the request exists and, for an
// accept, whether any version is unusable or carries another fingerprint. A
// version that does not parse counts as a mismatch: it may be a second key.
func classifyRequests(es []agecrypt.JoinEntry, accept bool, fp string) (found bool, mismatch string) {
	for _, e := range es {
		found = true
		switch {
		case !accept:
		case !e.Valid:
			mismatch = "malformed request (not a valid join request; reject it)"
		case agecrypt.NormalizeFingerprint(fp) != e.Join.Fingerprint:
			if mismatch == "" {
				mismatch = "fingerprint mismatch (a request here is " + agecrypt.GroupFingerprint(e.Join.Fingerprint) + "); not accepted"
			}
		}
	}
	return found, mismatch
}

func joinChangeAll(stdout, stderr io.Writer, set *routedSet, action, label, fp string, allowUnlabelled bool) int {
	accept := action == "accept"
	var plans []*joinRepoPlan
	var mismatched []string
	found := 0

	// First look, before the service is touched: both HEAD and the remote tip
	// count, so a request origin holds under another key is seen even in a
	// repo this command will not change.
	for _, n := range set.names() {
		p := &joinRepoPlan{name: n, cfg: set.Cfgs[n]}
		plans = append(plans, p)
		repo := gitwt.New(p.cfg.SyncDir)
		if !repo.IsRepo() {
			p.status = "skipped (missing: not a git clone)"
			continue
		}
		if !agecrypt.Enabled(repo) {
			p.status = "skipped (unencrypted)"
			continue
		}
		if err := joinFetchErr(repo); err != nil {
			// Origin may hold another key under the label that cannot be seen.
			p.fail = true
			if accept {
				mismatched = append(mismatched, n)
				p.status = fmt.Sprintf("cannot fetch (%v); nothing accepted anywhere", err)
			} else {
				p.status = fmt.Sprintf("cannot fetch (%v)", err)
			}
			continue
		}
		ok, mm := classifyRequests(agecrypt.RequestsForLabel(repo, label), accept, fp)
		if !ok {
			p.status = "no request"
			continue
		}
		found++
		switch {
		case mm != "":
			mismatched = append(mismatched, n)
			p.status, p.fail = mm, true
		case p.cfg.ReadOnly:
			p.status, p.fail = "skipped (read-only: this host never commits)", true
		default:
			if why, blocked := agecrypt.RekeyBlocksSync(repo); blocked {
				p.status, p.fail = "skipped (rekey-blocked: "+why+")", true
			} else {
				p.do = true
			}
		}
	}

	refused := false
	refuseAll := func(where []string) {
		refused = true
		// Name each blocking repo with its own cause ("cannot fetch", ...).
		var causes []string
		for _, n := range where {
			for _, p := range plans {
				if p.name == n {
					causes = append(causes, n+": "+strings.SplitN(p.status, " (", 2)[0])
				}
			}
		}
		by := strings.Join(causes, ", ")
		for _, p := range plans {
			if p.do {
				p.do, p.fail = false, true
				p.status = "not accepted (blocked by " + by + ")"
			}
		}
		fmt.Fprintf(stderr, "join accept --all: refusing every repo, blocked by %s: the request from %q cannot be matched to --fingerprint, or origin could not be read; that may mean a second key under one label. Check with the person who runs the new host, or `join list --all`; reject the stray request, then run this again.\n", by, label)
	}
	if len(mismatched) > 0 {
		refuseAll(mismatched)
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
			// Phase 1: bring every candidate up to date and look again; the
			// view that decides is the one under the lock.
			var late []string
			for _, p := range plans {
				if !p.do {
					continue
				}
				repo := gitwt.New(p.cfg.SyncDir)
				if err := agecrypt.SyncFromOrigin(repo); err != nil {
					p.do, p.fail = false, true
					if accept {
						// Origin's state here is unknown: it may hold another key.
						late = append(late, p.name)
						p.status = fmt.Sprintf("cannot sync (%v); nothing accepted anywhere", err)
					} else {
						p.status = fmt.Sprintf("refused (could not bring this clone up to date with origin; nothing was changed: %v)", err)
					}
					continue
				}
				ok, mm := classifyRequests(agecrypt.RequestsForLabel(repo, label), accept, fp)
				switch {
				case !ok:
					p.do, p.fail = false, true
					p.status = "refused (the request is gone after syncing with origin)"
				case mm != "":
					late = append(late, p.name)
					p.do, p.fail = false, true
					p.status = mm
				}
			}
			if len(late) > 0 {
				refuseAll(late)
				return nil
			}
			// Phase 2: the change itself, repo by repo, until interrupted.
			for _, p := range plans {
				if !p.do {
					continue
				}
				if syncInterrupted.Load() {
					break
				}
				p.ran = true
				if err := joinChangeOne(p, label, fp, allowUnlabelled, accept); err != nil {
					p.status, p.fail = err.Error(), true
				} else if accept {
					p.status, p.accepted = "accepted and pushed", true
				} else {
					p.status, p.accepted = "rejected and pushed", true
				}
			}
			return nil
		})
		if err != nil {
			why := "not run (" + err.Error() + ")"
			if errors.Is(err, errInterrupted) {
				why = "not run (interrupted)"
			}
			for _, p := range plans {
				if p.do && !p.ran {
					p.status, p.fail = why, true
				}
			}
			fmt.Fprintf(stderr, "join %s --all: %v\n", action, err)
			if errors.Is(err, lock.ErrHeld) {
				fmt.Fprintln(stderr, "the daemon is running on a sync dir; run `claude-memsync stop`, then this command, then `claude-memsync start`")
			}
			code = 1
		}
	}
	done := 0
	for _, p := range plans {
		fmt.Fprintf(stdout, "%s: %s\n", p.name, p.status)
		if p.fail {
			code = max(code, 1)
		}
		if p.accepted {
			done++
		}
	}
	if accept {
		for _, p := range plans {
			if !p.accepted {
				continue
			}
			if kfp, desc, ok := agecrypt.OwnKeyInfo(gitwt.New(p.cfg.SyncDir)); ok {
				who := desc
				if !strings.HasPrefix(desc, "host:") {
					who = "this host (" + desc + ")"
				}
				fmt.Fprintf(stdout, "%s: accepted by %s, key fingerprint %s; on the new host, `join status` must show this key\n", p.name, who, kfp)
			}
		}
	}
	if found == 0 && !refused {
		fmt.Fprintf(stderr, "join %s --all: no join request from %s in any repo on this host (has it been pulled? see `join list --all`)\n", action, label)
		return 1
	}
	if accept && done > 0 {
		fmt.Fprintln(stdout, "the new host pulls, then runs `claude-memsync recipients accept --yes`")
	}
	return code
}

// joinChangeOne accepts or rejects in one repo whose lock is held and which
// has just been synced with origin. A non-nil error is the repo's status line.
func joinChangeOne(p *joinRepoPlan, label, fp string, allowUnlabelled, accept bool) error {
	repo := gitwt.New(p.cfg.SyncDir)
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
