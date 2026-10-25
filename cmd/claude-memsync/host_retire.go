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
	Prints     map[string]string // repo -> short fingerprint of the key to remove
	Cannot     []cannotItem
	Notes      []string
	InRegistry bool
	CheckKey   *hosts.CheckKey
	// Refuse is set when the label cannot be resolved to one key; nothing is done.
	Refuse error
}

// removeLabelRecipient is the recipients step; a variable so a test can make it fail.
var removeLabelRecipient = agecrypt.RemoveRecipientLabel

func (p retirePlan) empty() bool {
	return len(p.Keys) == 0 && len(p.Reencrypt) == 0 && len(p.Cannot) == 0 && !p.InRegistry
}

// planRetire reads the org and this host's repos and decides, without changing
// anything, what retiring label involves. Only mem-* repos are ever considered.
func (hc *hostCtx) planRetire(c *ghguard.Client, org, label string) (retirePlan, error) {
	plan := retirePlan{Label: label, Org: org, Prints: map[string]string{}}
	boundTo := map[string][]string{} // pinned key -> repos
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
		// The label is resolved through the pin (what a person here reviewed),
		// never through the comments in the work-tree, which upstream can change.
		keys, kerr := agecrypt.PinnedKeysForLabel(repo, label)
		inTree := false
		for _, ln := range lines {
			if agecrypt.LabelOf(ln) == label {
				inTree = true
			}
		}
		if u, _ := agecrypt.Unreviewed(repo); u {
			if inTree || len(keys) > 0 {
				plan.Cannot = append(plan.Cannot, cannotItem{n, "its recipients changed upstream and are not reviewed here (`recipients list`, then `recipients accept`)"})
			}
			continue
		}
		if errors.Is(kerr, agecrypt.ErrLabelAmbiguous) {
			plan.Refuse = fmt.Errorf("%s: label %q is bound to more than one key in the pinned recipients; fix it with `recipients remove <public-key>` on a writable host", n, label)
			return plan, nil
		}
		if len(keys) == 0 {
			if len(keysOn[n]) > 0 {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s: no recipient is labelled %s; if its line has no label, remove it with `recipients remove <public-key>`", n, label))
			}
			continue
		}
		key := keys[0]
		others := 0
		for _, ln := range lines {
			if agecrypt.CanonLine(ln) != key {
				others++
			}
		}
		boundTo[key] = append(boundTo[key], n)
		switch {
		case others == 0:
			plan.Cannot = append(plan.Cannot, cannotItem{n, "the retired host is the only recipient there, so no one could read it afterwards"})
		case l.Cfg.ReadOnly:
			plan.Cannot = append(plan.Cannot, cannotItem{n, "this host is a read-only member of the repo and never commits"})
		case agecrypt.CheckKeyed(repo) != nil:
			plan.Cannot = append(plan.Cannot, cannotItem{n, "this host cannot read the repo, so it cannot re-encrypt it"})
		default:
			plan.Reencrypt = append(plan.Reencrypt, l)
			plan.Prints[n] = agecrypt.KeyPrint(key)
		}
	}
	if len(boundTo) > 1 {
		var parts []string
		for k, repos := range boundTo {
			parts = append(parts, fmt.Sprintf("%s -> key %s", strings.Join(repos, ", "), agecrypt.KeyPrint(k)))
		}
		sort.Strings(parts)
		plan.Refuse = fmt.Errorf("label %q is bound to different keys in different repos (%s); that is not one host, so nothing is retired", label, strings.Join(parts, "; "))
		return plan, nil
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
		rn = append(rn, l.Name+" key "+plan.Prints[l.Name])
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
		fmt.Fprintf(w, "manual step: delete this host's check App key in the GitHub UI: %s\n  fingerprint %s, as recorded in the registry (verify the fingerprint in GitHub's UI before deleting); App keys can only be deleted there\n", hc.appURL(plan), plan.CheckKey.Fingerprint)
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
	if plan.Refuse != nil {
		fmt.Fprintln(stderr, "host retire: refused:", plan.Refuse)
		return 1
	}
	if plan.empty() {
		fmt.Fprintf(stdout, "nothing to retire for %q: no deploy key, labelled recipient or registry entry found (already retired, or check the spelling)\n", label)
		return 0
	}
	if !*apply {
		hc.printPlan(stdout, plan, false)
		return 0
	}
	return hc.applyRetire(c, plan, stdout, stderr)
}

// applyRetire acts on the plan. The daemon locks of every sync dir it will
// write to are taken first and held until it returns, so the daemon cannot
// start a cycle between the steps; if one is held, nothing has changed. The
// deploy keys go next (the step that needs the most permission), then the
// recipients, then the registry. What could not be done is summarised at the
// end, and a re-run picks up exactly that.
func (hc *hostCtx) applyRetire(c *ghguard.Client, plan retirePlan, stdout, stderr io.Writer) int {
	dirs := map[string]string{}
	for _, l := range plan.Reencrypt {
		dirs[l.Dir] = l.Name
	}
	if plan.InRegistry {
		for _, l := range hc.local {
			if sameDir(l.Dir, hc.registryDir) {
				dirs[l.Dir] = "registry"
			}
		}
	}
	var held []*lock.Lock
	defer func() {
		for _, lk := range held {
			lk.Release()
		}
	}()
	for dir, name := range dirs {
		lk, err := lock.Acquire(dir)
		if err != nil {
			fmt.Fprintf(stderr, "host retire: %s: %v\n", name, err)
			if errors.Is(err, lock.ErrHeld) {
				fmt.Fprintln(stderr, "the daemon is running on this sync dir; run `claude-memsync stop`, then this command, then `claude-memsync start`. Nothing was changed.")
			}
			return 1
		}
		held = append(held, lk)
	}
	hc.printPlan(stdout, plan, true)

	remainingKeys := len(plan.Keys)
	pendingRepos := map[string]bool{}
	for _, l := range plan.Reencrypt {
		pendingRepos[l.Name] = true
	}
	registryPending := plan.InRegistry
	failed := false
	summary := func() int {
		var pend []string
		if remainingKeys > 0 {
			pend = append(pend, fmt.Sprintf("%d deploy key(s) not deleted: re-run", remainingKeys))
		}
		for _, l := range plan.Reencrypt {
			if pendingRepos[l.Name] {
				pend = append(pend, fmt.Sprintf("STILL A RECIPIENT in %s: re-run", l.Name))
			}
		}
		if registryPending {
			pend = append(pend, fmt.Sprintf("registry entry hosts/%s/ not removed: re-run", plan.Label))
		}
		for _, cn := range plan.Cannot {
			pend = append(pend, fmt.Sprintf("NOT re-encrypted in %s: %s", cn.Repo, cn.Reason))
		}
		if len(pend) == 0 {
			fmt.Fprintf(stdout, "retire complete for %s\n", plan.Label)
		} else {
			fmt.Fprintln(stdout, "still pending:")
			for _, p := range pend {
				fmt.Fprintln(stdout, "  "+p)
			}
		}
		if failed {
			return 1
		}
		return 0
	}

	deleted, gone := 0, 0
	for _, k := range plan.Keys {
		err := c.DeleteDeployKey(plan.Org, k.Repo, k.ID)
		switch {
		case err == nil:
			deleted++
			remainingKeys--
		case errors.Is(err, ghguard.ErrNotFound):
			gone++
			remainingKeys--
		case errors.Is(err, ghguard.ErrForbidden):
			fmt.Fprintf(stderr, "host retire: GitHub refused to delete a deploy key of %s: the token needs Administration: write on the mem-* repos.\n", k.Repo)
			if deleted == 0 {
				fmt.Fprintln(stderr, "Nothing was changed.")
			} else {
				fmt.Fprintf(stderr, "%d key(s) were deleted before this; fix the token and run the command again (it is safe to repeat).\n", deleted)
			}
			failed = true
			return summaryOnAbort(deleted, summary)
		default:
			fmt.Fprintf(stderr, "host retire: deleting a deploy key of %s: %v\n", k.Repo, err)
			failed = true
			return summaryOnAbort(deleted, summary)
		}
	}
	fmt.Fprintf(stdout, "deleted %d deploy key(s)", deleted)
	if gone > 0 {
		fmt.Fprintf(stdout, " (%d already gone)", gone)
	}
	fmt.Fprintln(stdout)
	for _, l := range plan.Reencrypt {
		repo := gitwt.New(l.Dir)
		if err := removeLabelRecipient(repo, plan.Label); err != nil {
			fmt.Fprintf(stderr, "host retire: %s: %v\n", l.Name, err)
			failed = true
			continue
		}
		pendingRepos[l.Name] = false
		fmt.Fprintf(stdout, "%s: recipient removed and HEAD re-encrypted (committed; the daemon pushes it)\n", l.Name)
	}
	if plan.InRegistry {
		if _, err := hosts.RemoveHost(hc.registryDir, plan.Label); err != nil {
			fmt.Fprintf(stderr, "host retire: registry: %v\n", err)
			failed = true
		} else {
			registryPending = false
			fmt.Fprintf(stdout, "registry: hosts/%s/ removed (committed locally; the registry's sync pushes it)\n", plan.Label)
		}
	}
	return summary()
}

// summaryOnAbort prints the summary when the key step stopped early.
func summaryOnAbort(_ int, summary func() int) int { summary(); return 1 }
