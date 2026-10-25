package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/ghguard"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/hosts"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/MarimerLLC/claude-utils/internal/route"
)

type planKey struct {
	Repo     string
	ID       int64
	ReadOnly bool
}

type cannotItem struct{ Repo, Reason string }

// retirePlan is everything `host retire` would do for one label.
type retirePlan struct {
	Label, Org string
	Keys       []planKey
	Reencrypt  []localRepo
	Cannot     []cannotItem
	Notes      []string
	InRegistry bool
	CheckKey   *hosts.CheckKey
}

func (p retirePlan) empty() bool {
	return len(p.Keys) == 0 && len(p.Reencrypt) == 0 && len(p.Cannot) == 0 && !p.InRegistry
}

// planRetire reads the org and this host's repos and decides, without changing
// anything, what retiring label involves. Only mem-* repos are ever considered.
func (hc *hostCtx) planRetire(c *ghguard.Client, org, label string) (retirePlan, error) {
	plan := retirePlan{Label: label, Org: org}
	repos, err := c.ListMemRepos(org)
	if err != nil {
		return plan, err
	}
	keysOn := map[string][]planKey{}
	for _, r := range repos {
		ks, err := c.ListDeployKeys(org, r)
		if err != nil {
			return plan, permissionHint(fmt.Errorf("deploy keys of %s: %w", r, err), "Administration: read")
		}
		for _, k := range ks {
			if k.Title == label {
				keysOn[r] = append(keysOn[r], planKey{Repo: r, ID: k.ID, ReadOnly: k.ReadOnly})
				plan.Keys = append(plan.Keys, planKey{Repo: r, ID: k.ID, ReadOnly: k.ReadOnly})
			}
		}
	}
	local := map[string]localRepo{}
	names := map[string]bool{}
	for _, r := range repos {
		names[r] = true
	}
	for _, l := range hc.local {
		if ghguard.ValidName(l.Name) != nil {
			continue
		}
		local[l.Name] = l
		names[l.Name] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		l, isLocal := local[n]
		if !isLocal {
			if len(keysOn[n]) > 0 {
				plan.Cannot = append(plan.Cannot, cannotItem{n, "this host does not have the repo, so its recipients cannot be changed from here"})
			}
			continue
		}
		repo := gitwt.New(l.Dir)
		if !repo.IsRepo() || !agecrypt.Enabled(repo) {
			continue // nothing encrypted, nothing to re-encrypt
		}
		lines, err := agecrypt.ListRecipients(l.Dir)
		if err != nil {
			plan.Cannot = append(plan.Cannot, cannotItem{n, "its recipients cannot be read here"})
			continue
		}
		has, others := false, 0
		for _, ln := range lines {
			if agecrypt.LabelOf(ln) == label {
				has = true
			} else {
				others++
			}
		}
		switch {
		case !has:
			if len(keysOn[n]) > 0 {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s: no recipient is labelled %s; if its line has no label, remove it with `recipients remove <public-key>`", n, label))
			}
		case others == 0:
			plan.Cannot = append(plan.Cannot, cannotItem{n, "the retired host is the only recipient there, so no one could read it afterwards"})
		case l.Cfg.ReadOnly:
			plan.Cannot = append(plan.Cannot, cannotItem{n, "this host is a read-only member of the repo and never commits"})
		case agecrypt.CheckKeyed(repo) != nil:
			plan.Cannot = append(plan.Cannot, cannotItem{n, "this host cannot read the repo, so it cannot re-encrypt it"})
		default:
			plan.Reencrypt = append(plan.Reencrypt, l)
		}
	}
	if hc.registryDir != "" {
		reg, err := hosts.ReadRegistry(hc.registryDir)
		if err != nil {
			return plan, err
		}
		if h, ok := reg[label]; ok {
			plan.InRegistry = true
			plan.CheckKey = h.CheckKey
			if hc.registryReadOnly {
				plan.InRegistry = false
				plan.Cannot = append(plan.Cannot, cannotItem{"registry", "this host is a read-only member of the registry; remove hosts/" + label + "/ from a writable host"})
			}
		}
	}
	return plan, nil
}

func (p retirePlan) keySummary() string {
	var parts []string
	for _, k := range p.Keys {
		m := "rw"
		if k.ReadOnly {
			m = "ro"
		}
		parts = append(parts, k.Repo+" "+m)
	}
	return strings.Join(parts, ", ")
}

func (hc *hostCtx) appURL(plan retirePlan) string {
	return "https://" + allowedHost(hc.cfg) + "/organizations/" + plan.Org + "/settings/apps/" + plan.CheckKey.App
}

func (hc *hostCtx) printPlan(w io.Writer, plan retirePlan, apply bool) {
	if apply {
		fmt.Fprintf(w, "retire %s in %s: applying\n", plan.Label, plan.Org)
	} else {
		fmt.Fprintf(w, "retire %s in %s: dry run, nothing changes (re-run with --apply to act)\n", plan.Label, plan.Org)
	}
	fmt.Fprintf(w, "deploy keys to delete: %d", len(plan.Keys))
	if len(plan.Keys) > 0 {
		fmt.Fprintf(w, " (%s)", plan.keySummary())
	}
	fmt.Fprintln(w)
	var rn []string
	for _, l := range plan.Reencrypt {
		rn = append(rn, l.Name)
	}
	fmt.Fprintf(w, "recipients to remove and re-encrypt here: %d", len(rn))
	if len(rn) > 0 {
		fmt.Fprintf(w, " (%s)", strings.Join(rn, ", "))
	}
	fmt.Fprintln(w)
	if len(plan.Cannot) > 0 {
		fmt.Fprintln(w, "cannot re-encrypt here: run retire on a member host, or archive the repo")
		for _, c := range plan.Cannot {
			fmt.Fprintf(w, "  %s: %s\n", c.Repo, c.Reason)
		}
	}
	for _, n := range plan.Notes {
		fmt.Fprintln(w, "note:", n)
	}
	if plan.InRegistry {
		fmt.Fprintf(w, "registry: hosts/%s/ will be removed (committed locally)\n", plan.Label)
	}
	if plan.CheckKey != nil {
		fmt.Fprintf(w, "manual step: delete this host's check App key %s in the GitHub UI: %s (keys can only be deleted there)\n", plan.CheckKey.Fingerprint, hc.appURL(plan))
	}
	fmt.Fprintln(w, "history stays readable to the retired key: nothing already pushed is rewritten, only new content is closed to it")
}

func hostRetire(hc *hostCtx, stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("host retire", flag.ContinueOnError)
	fs.SetOutput(stderr)
	org := fs.String("org", "", "GitHub org (default: config githubOrg, else the owner of the remote)")
	apply := fs.Bool("apply", false, "act on the plan (the default is a dry run)")
	dry := fs.Bool("dry-run", false, "only print the plan (the default)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: claude-memsync host retire <label> [--apply] [--org ORG]")
		return 2
	}
	if *apply && *dry {
		fmt.Fprintln(stderr, "host retire: --apply and --dry-run contradict each other")
		return 2
	}
	label := pos[0]
	if err := route.ValidLabel(label); err != nil {
		fmt.Fprintln(stderr, "host retire:", err)
		return 2
	}
	for _, own := range hc.ownLabels {
		if own == label {
			fmt.Fprintf(stderr, "host retire: %q is this host's own label; retire it from another host\n", label)
			return 1
		}
	}
	o, err := hc.orgName(*org)
	if err != nil {
		fmt.Fprintln(stderr, "host retire:", err)
		return 2
	}
	c, err := hc.client()
	if err != nil {
		fmt.Fprintln(stderr, "host retire:", err)
		return 1
	}
	plan, err := hc.planRetire(c, o, label)
	if err != nil {
		fmt.Fprintln(stderr, "host retire:", err)
		return 1
	}
	if plan.empty() {
		fmt.Fprintf(stderr, "host retire: no host labelled %q found (no deploy key, labelled recipient or registry entry)\n", label)
		return 1
	}
	if !*apply {
		hc.printPlan(stdout, plan, false)
		return 0
	}
	return hc.applyRetire(c, plan, stdout, stderr)
}

// applyRetire acts on the plan in an order that fails early: the daemon locks
// are checked first, the deploy keys go next (the step that needs the most
// permission), then the recipients, then the registry.
func (hc *hostCtx) applyRetire(c *ghguard.Client, plan retirePlan, stdout, stderr io.Writer) int {
	// Recipients changes refuse to run while the daemon holds the sync dir;
	// find that out before anything is deleted.
	for _, l := range plan.Reencrypt {
		lk, err := lock.Acquire(l.Dir)
		if err != nil {
			fmt.Fprintf(stderr, "host retire: %s: %v\n", l.Name, err)
			if errors.Is(err, lock.ErrHeld) {
				fmt.Fprintln(stderr, "the daemon is running on this sync dir; run `claude-memsync stop`, then this command, then `claude-memsync start`. Nothing was changed.")
			}
			return 1
		}
		lk.Release()
	}
	hc.printPlan(stdout, plan, true)
	deleted, gone := 0, 0
	for _, k := range plan.Keys {
		err := c.DeleteDeployKey(plan.Org, k.Repo, k.ID)
		switch {
		case err == nil:
			deleted++
		case errors.Is(err, ghguard.ErrNotFound):
			gone++
		case errors.Is(err, ghguard.ErrForbidden):
			fmt.Fprintf(stderr, "host retire: GitHub refused to delete a deploy key of %s: the token needs Administration: write on the mem-* repos.\n", k.Repo)
			if deleted == 0 {
				fmt.Fprintln(stderr, "Nothing was changed.")
			} else {
				fmt.Fprintf(stderr, "%d key(s) were deleted before this; fix the token and run the command again (it is safe to repeat).\n", deleted)
			}
			return 1
		default:
			fmt.Fprintf(stderr, "host retire: deleting a deploy key of %s: %v\n", k.Repo, err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "deleted %d deploy key(s)", deleted)
	if gone > 0 {
		fmt.Fprintf(stdout, " (%d already gone)", gone)
	}
	fmt.Fprintln(stdout)
	code := 0
	for _, l := range plan.Reencrypt {
		repo := gitwt.New(l.Dir)
		err := withSyncLock(l.Dir, func() error { return agecrypt.RemoveRecipientLabel(repo, plan.Label) })
		if err != nil {
			fmt.Fprintf(stderr, "host retire: %s: %v\n", l.Name, err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "%s: recipient removed and HEAD re-encrypted (committed; the daemon pushes it)\n", l.Name)
	}
	if plan.InRegistry {
		if _, err := hosts.RemoveHost(hc.registryDir, plan.Label); err != nil {
			fmt.Fprintf(stderr, "host retire: registry: %v\n", err)
			code = 1
		} else {
			fmt.Fprintf(stdout, "registry: hosts/%s/ removed (committed locally; the registry's sync pushes it)\n", plan.Label)
		}
	}
	return code
}
