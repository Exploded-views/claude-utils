package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

const joinUsage = `usage:
  claude-memsync join request --repo <name> --label <host-label>   (on the new host)
  claude-memsync join status [--repo <name>]                       (on the new host)
  claude-memsync join list [--repo <name>]                         (on a member host)
  claude-memsync join accept <label> --repo <name> --fingerprint <fp>
  claude-memsync join reject <label> [--repo <name>]
  (repo selection as for recipients: --repo <name> | --config <path>, --routes <path>)`

// runJoin handles `claude-memsync join request|status|list|accept|reject`.
func runJoin(args []string) int { return joinCmd(os.Stdout, os.Stderr, args) }

func joinCmd(stdout, stderr io.Writer, args []string) int {
	sel, args, err := extractRepoFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, "join:", err)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, joinUsage)
		return 2
	}
	action, rest := args[0], args[1:]
	var label string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		label, rest = rest[0], rest[1:]
	}
	flags := flag.NewFlagSet("join "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flabel := flags.String("label", "", "host label (^[a-z0-9][a-z0-9-]{0,31}$)")
	fp := flags.String("fingerprint", "", "accept: the fingerprint the requesting host printed")
	if err := flags.Parse(rest); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, "join: unexpected argument", flags.Arg(0))
		return 2
	}
	if *flabel != "" {
		if label != "" && label != *flabel {
			fmt.Fprintln(stderr, "join: two different labels given")
			return 2
		}
		label = *flabel
	}
	switch action {
	case "request", "accept", "reject":
		if label == "" {
			fmt.Fprintf(stderr, "join %s: a label is required\n%s\n", action, joinUsage)
			return 2
		}
		if !agecrypt.ValidJoinLabel(label) {
			fmt.Fprintln(stderr, "join: label must match ^[a-z0-9][a-z0-9-]{0,31}$")
			return 2
		}
	case "list", "status":
	default:
		fmt.Fprintf(stderr, "join: unknown action %q\n%s\n", action, joinUsage)
		return 2
	}
	if action == "list" && sel.Repo == "" && sel.Config == "" {
		if path, ok, err := resolveRoutesPath(sel.Routes); err == nil && ok {
			if set, err := loadRouted(path); err == nil {
				code := 0
				for _, n := range set.names() {
					fmt.Fprintf(stdout, "repo %s:\n", n)
					if c := joinList(stdout, stderr, set.Cfgs[n]); c > code {
						code = c
					}
				}
				return code
			}
		}
	}
	cfg, _, err := resolveRepo(sel, true)
	if err != nil {
		fmt.Fprintln(stderr, "join:", err)
		return 1
	}
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintln(stderr, "join: the sync repo is not encrypted")
		return 1
	}
	switch action {
	case "list":
		return joinList(stdout, stderr, cfg)
	case "status":
		return joinStatus(stdout, stderr, cfg, repo)
	case "request":
		return joinRequest(stdout, stderr, cfg, repo, label)
	case "accept":
		return joinChange(stdout, stderr, cfg, repo, label, func() error { return agecrypt.AcceptJoin(repo, label, *fp) }, true)
	default:
		return joinChange(stdout, stderr, cfg, repo, label, func() error { return agecrypt.RejectJoin(repo, label) }, false)
	}
}

// joinFetch brings in the remote's state without touching the work-tree.
func joinFetch(repo *gitwt.Repo) {
	br, err := repo.CurrentBranch()
	if err != nil || br == "" || br == "HEAD" {
		br = defaultBranch
	}
	_, _ = repo.Run("fetch", "-q", "origin", br)
}

// joinGuardHook lets tests see which push guard a join command used.
var joinGuardHook func(kind string)

func joinPush(cfg config.Config, repo *gitwt.Repo, request bool) error {
	if cfg.PrivateOnly {
		if err := validateRemoteOffline(cfg); err != nil {
			return err
		}
		if err := checkPushURL(cfg, cfg.SyncDir); err != nil {
			return err
		}
	}
	br, err := repo.CurrentBranch()
	if err != nil || br == "" || br == "HEAD" {
		br = defaultBranch
	}
	guard := agecrypt.PushGuard
	kind := "normal"
	if request {
		guard = agecrypt.PushGuardJoinRequest
		kind = "request"
	}
	if joinGuardHook != nil {
		joinGuardHook(kind)
	}
	if err := guard(repo, br); err != nil {
		return err
	}
	return repo.Push(br)
}

func joinRequest(stdout, stderr io.Writer, cfg config.Config, repo *gitwt.Repo, label string) int {
	var j agecrypt.Join
	var res agecrypt.JoinResult
	err := withSyncLock(cfg.SyncDir, func() error {
		// A request is built on origin's tip and nothing else: if the clone
		// cannot be brought up to date, or differs from origin in any other
		// way, nothing is committed or pushed.
		if err := agecrypt.SyncFromOrigin(repo); err != nil {
			return fmt.Errorf("could not bring this clone up to date with origin: %w", err)
		}
		var err error
		j, res, err = agecrypt.RequestJoin(repo, label, time.Now())
		if err != nil {
			return err
		}
		if res == agecrypt.JoinAlreadyMember {
			return nil
		}
		need, err := agecrypt.VerifyJoinPush(repo, j.Label)
		if err != nil {
			return err
		}
		if !need {
			return nil
		}
		return joinPush(cfg, repo, true)
	})
	if err != nil {
		return recipientsFail(stderr, fmt.Errorf("join request: %w", err))
	}
	switch res {
	case agecrypt.JoinAlreadyMember:
		fmt.Fprintln(stdout, "this host is already a recipient of this repo; nothing to request")
		return 0
	case agecrypt.JoinExists:
		fmt.Fprintf(stdout, "join request already pending: %s\n", j.Label)
	default:
		fmt.Fprintf(stdout, "join request sent: %s\n", j.Label)
	}
	fmt.Fprintf(stdout, "fingerprint: %s\n", agecrypt.GroupFingerprint(j.Fingerprint))
	fmt.Fprintln(stderr, "Give this fingerprint to the person who runs `claude-memsync join accept` on a member host; they must type it back.")
	fmt.Fprintln(stderr, "After they accept, pull (the daemon does, or `git -C <sync dir> pull`), then run `claude-memsync join status` here to see the pending recipient set and compare it with `join list` and `recipients list` on a member host. Only if every key is expected, run `claude-memsync recipients accept --yes`.")
	return 0
}

func joinStatus(stdout, stderr io.Writer, cfg config.Config, repo *gitwt.Repo) int {
	joinFetch(repo)
	st, err := agecrypt.JoinStatus(repo)
	if err != nil {
		fmt.Fprintln(stderr, "join status:", err)
		return 1
	}
	switch st.State {
	case agecrypt.JoinStatePending:
		fmt.Fprintf(stdout, "pending: label %s, fingerprint %s\n", st.Join.Label, agecrypt.GroupFingerprint(st.Join.Fingerprint))
	case agecrypt.JoinStateAccepted:
		if u, _ := agecrypt.Unreviewed(repo); u {
			fmt.Fprintln(stdout, "accepted: your key is in .age-recipients. Do NOT run `recipients accept --yes` blindly: compare the set below with `claude-memsync join list` and `recipients list` on a member host first.")
			printPendingRecipients(stdout, repo)
		} else if agecrypt.CheckKeyed(repo) == nil {
			fmt.Fprintln(stdout, "accepted: this host can decrypt the repo")
		} else {
			fmt.Fprintln(stdout, "accepted: your key is a recipient; pull to receive re-encrypted content")
		}
	default:
		fmt.Fprintf(stdout, "none: no join request for this host's key (fingerprint %s)\n", agecrypt.GroupFingerprint(st.Join.Fingerprint))
	}
	return 0
}

func joinList(stdout, stderr io.Writer, cfg config.Config) int {
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintln(stdout, "  (not encrypted)")
		return 0
	}
	joinFetch(repo)
	es, err := agecrypt.ListJoins(repo)
	if err != nil {
		fmt.Fprintln(stderr, "join list:", err)
		return 1
	}
	sort.Slice(es, func(i, k int) bool { return es[i].Path < es[k].Path })
	if len(es) == 0 {
		fmt.Fprintln(stdout, "no join requests pending")
		return 0
	}
	for _, e := range es {
		if e.Valid {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", e.Join.Label, agecrypt.GroupFingerprint(e.Join.Fingerprint), e.Join.Requested.Format(time.RFC3339))
		} else {
			fmt.Fprintf(stdout, "%s\tINVALID (will not be accepted; reject it)\t-\n", e.Join.Label)
		}
	}
	return 0
}

// joinChange runs an accept or a reject under the sync lock, then pushes.
func joinChange(stdout, stderr io.Writer, cfg config.Config, repo *gitwt.Repo, label string, do func() error, accept bool) int {
	committed := false
	err := withSyncLock(cfg.SyncDir, func() error {
		if err := agecrypt.SyncFromOrigin(repo); err != nil {
			fmt.Fprintln(stderr, "join: could not bring in the latest remote state; working from this clone:", err)
		}
		if err := agecrypt.WriteInfoAttributes(repo); err != nil {
			return err
		}
		if err := do(); err != nil {
			return err
		}
		committed = true
		return joinPush(cfg, repo, false)
	})
	if err != nil {
		if committed {
			fmt.Fprintln(stderr, "join: committed locally but the push failed; the daemon pushes it on its next cycle")
		}
		return recipientsFail(stderr, fmt.Errorf("join: %w", err))
	}
	if accept {
		fmt.Fprintf(stdout, "accepted %s and pushed; the new host pulls, then runs `claude-memsync recipients accept --yes`\n", label)
	} else {
		fmt.Fprintf(stdout, "rejected %s and pushed\n", label)
	}
	return 0
}

// joinStatusLine is the `status` line for requests waiting on a member host.
func joinStatusLine(cfg config.Config) string {
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) || agecrypt.CheckKeyed(repo) != nil {
		return ""
	}
	if n := agecrypt.PendingJoins(repo); n > 0 {
		return fmt.Sprintf("%d join request(s) pending (see `claude-memsync join list`)", n)
	}
	return ""
}

// printPendingRecipients shows the recipient set waiting for review: every key
// with its fingerprint (the form `join list` uses), new ones and this host's
// own marked.
func printPendingRecipients(w io.Writer, repo *gitwt.Repo) {
	list, removed, err := agecrypt.PendingRecipients(repo)
	if err != nil {
		fmt.Fprintln(w, "pending recipient set: unavailable:", err)
		return
	}
	fmt.Fprintf(w, "pending recipient set (%d key(s)):\n", len(list))
	for _, r := range list {
		var tags []string
		if r.New {
			tags = append(tags, "new")
		}
		if r.Own {
			tags = append(tags, "this host")
		}
		note := ""
		if len(tags) > 0 {
			note = "  (" + strings.Join(tags, ", ") + ")"
		}
		fmt.Fprintf(w, "recipient: %s  %s%s\n", agecrypt.GroupFingerprint(r.Fingerprint), r.Key, note)
	}
	if removed > 0 {
		fmt.Fprintf(w, "%d pinned key(s) are gone from the new set\n", removed)
	}
	fmt.Fprintln(w, "then, if every key is expected: claude-memsync recipients accept --yes")
}
