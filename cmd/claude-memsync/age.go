package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// runAgeFilter is the git long-running filter process (`filter.memage.process`).
// Git starts it in the work-tree root; stdout carries only the protocol.
func runAgeFilter(args []string) int {
	if done, code := noFlagArgs("age-filter", "git's long-running age filter process (started by git, not by hand)", args); done {
		return code
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "age-filter:", err)
		return 1
	}
	f, err := agecrypt.NewFilter(wd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "age-filter:", err)
		return 1
	}
	if err := f.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "age-filter:", err)
		return 1
	}
	return 0
}

// runAgeTextconv prints a file's plaintext for `git diff` and `git log -p`.
func runAgeTextconv(args []string) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Println("usage: claude-memsync age-textconv <file>\n\nPrint a file's plaintext for git diff and git log -p (started by git)")
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-memsync age-textconv <file>")
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "age-textconv:", err)
		return 1
	}
	wd, _ := os.Getwd()
	f, err := agecrypt.NewFilter(wd)
	if err != nil {
		os.Stdout.Write(data)
		return 0
	}
	os.Stdout.Write(f.Textconv(data))
	return 0
}

// runRecipients handles `claude-memsync recipients add|remove|list|accept`.
func runRecipients(args []string) int { return recipientsCmd(os.Stdout, os.Stderr, args) }

const recipientsUsage = "usage: claude-memsync recipients add <public-key> [--host <label>] | remove <public-key> | list | accept --yes | label self --host <label> | label recovery <public-key> [--all] | rekey-pq --recovery-recipient <age1pq1...> --host <label> [--all] [--fresh-root] [--apply] | rekey-pq (--repo <name> | --all) --rollback [--discard-local-commits] | rekey-pq --drop-backup | retire-classic [--all] [--apply]  [--repo <name> | --config <path>] [--routes <path>]"

// recipientsCmd acts on one repo. With a routes file the repo must be named
// (--repo or --config): the personal repo is never assumed, since a key added
// to the wrong repo would be a key admitted to the wrong notes.
func recipientsCmd(stdout, stderr io.Writer, args []string) int {
	sel, args, err := extractRepoFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, "recipients:", err)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, recipientsUsage)
		return 2
	}
	args, hostLabel := extractFlag(args, "--host")
	if len(args) > 0 && args[0] == "label" {
		return labelCmd(stdout, stderr, sel, args[1:], hostLabel)
	}
	if len(args) > 0 && args[0] == "retire-classic" {
		if hostLabel != "" {
			fmt.Fprintln(stderr, "recipients: retire-classic takes no --host")
			return 2
		}
		return retireClassicCmd(stdout, stderr, sel, args[1:])
	}
	if len(args) > 0 && args[0] == "rekey-pq" {
		return rekeyPQCmd(stdout, stderr, sel, args[1:], hostLabel)
	}
	cfg, _, err := resolveRepo(sel, true)
	if err != nil {
		fmt.Fprintln(stderr, "recipients:", err)
		return 1
	}
	if hostLabel != "" && (len(args) == 0 || args[0] != "add") {
		fmt.Fprintln(stderr, "recipients: --host only goes with add")
		return 2
	}
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintln(stderr, "recipients: the sync repo is not encrypted (init it with --encrypt)")
		return 1
	}
	if cfg.ReadOnly && (args[0] == "add" || args[0] == "remove") {
		fmt.Fprintln(stderr, "recipients: this host is read-only and never commits; run it on a writable member host")
		return 1
	}
	switch args[0] {
	case "list":
		keys, err := agecrypt.ListRecipients(cfg.SyncDir)
		if err != nil {
			fmt.Fprintln(stderr, "recipients:", err)
			return 1
		}
		for _, k := range keys {
			fmt.Fprintln(stdout, k)
		}
		if u, _ := agecrypt.Unreviewed(repo); u {
			printRecipientsDiff(stderr, repo)
		}
	case "accept":
		if u, _ := agecrypt.Unreviewed(repo); !u {
			fmt.Fprintln(stderr, "recipients: nothing to accept; the work-tree matches the pinned set")
			return 0
		}
		printRecipientsDiff(stderr, repo)
		if len(args) != 2 || args[1] != "--yes" {
			fmt.Fprintln(stderr, "recipients: review the change above; if you trust every added key, run `claude-memsync recipients accept --yes`")
			return 1
		}
		accept := agecrypt.AcceptRecipients
		if cfg.ReadOnly {
			accept = agecrypt.PinCurrent // pin, but never re-encrypt and commit
		}
		if err := withSyncLockStopping(stderr, cfg.SyncDir, func() error { return accept(repo) }); err != nil {
			return recipientsFail(stderr, err)
		}
		fmt.Fprintln(stderr, "recipients accepted; syncing resumes on the next cycle")
	case "add", "remove":
		if len(args) != 2 {
			fmt.Fprintf(stderr, "usage: claude-memsync recipients %s <public-key>\n", args[0])
			return 2
		}
		err = withSyncLockStopping(stderr, cfg.SyncDir, func() error {
			if args[0] == "add" {
				return agecrypt.AddRecipientLabelled(repo, args[1], hostLabel)
			}
			return agecrypt.RemoveRecipient(repo, args[1])
		})
		if err != nil {
			return recipientsFail(stderr, err)
		}
		fmt.Fprintln(stderr, "recipients updated and committed; the daemon pushes it on its next cycle")
		if args[0] == "remove" {
			fmt.Fprintln(stderr, "note: the removed key still reads everything it already fetched; only new content is closed to it")
		}
	default:
		fmt.Fprintf(stderr, "recipients: unknown action %q\n", args[0])
		return 2
	}
	return 0
}

// withSyncLock runs fn holding the sync dir's lock, the one the daemon holds
// for its whole life. It refuses (never waits) when the daemon has it: the
// actions that use it rewrite refs and the index, which must not race a cycle.
func withSyncLock(syncDir string, fn func() error) error {
	l, err := lock.Acquire(syncDir)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}

func recipientsFail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "recipients:", err)
	if errors.Is(err, lock.ErrHeld) {
		fmt.Fprintln(stderr, "the daemon is running on this sync dir; run `claude-memsync stop`, then this command, then `claude-memsync start`")
	}
	return 1
}

// encryptionLines is the encryption part of `status`: nothing for an
// unencrypted repo; otherwise whether this host can read the repo, and any
// unreviewed upstream change to the recipients.
// readOnlyLine is the status line of a read-only member, "" for a writable host.
func readOnlyLine(cfg config.Config) string {
	if !cfg.ReadOnly {
		return ""
	}
	return "read-only: this host pulls notes into the Claude tree but never commits or pushes"
}

func encryptionLines(cfg config.Config) []string {
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		return nil
	}
	var out []string
	switch err := agecrypt.CheckKeyed(repo); {
	case errors.Is(err, agecrypt.ErrKeyless):
		out = append(out, "encryption: age, KEYLESS: nothing is committed or synced. "+err.Error())
	case err != nil:
		out = append(out, "encryption: age, key check failed: "+err.Error())
	default:
		out = append(out, "encryption: age, this host can decrypt the repo")
	}
	if why, blocked := agecrypt.RekeyBlocksSync(repo); blocked {
		out = append(out, "rekey-pq: not syncing: "+why)
	}
	if u, _ := agecrypt.Unreviewed(repo); u {
		out = append(out, "recipients UNREVIEWED: .age-recipients differs from the set pinned on this host; nothing is committed or pushed. Review with `claude-memsync recipients list`, then `recipients accept --yes`")
	}
	if l := joinStatusLine(cfg); l != "" {
		out = append(out, l)
	}
	if l := filterVersionNote(cfg, repo); l != "" {
		out = append(out, l)
	}
	return out
}

// printRecipientsDiff shows the unreviewed recipient change as short
// fingerprints of the keys, never the keys themselves.
func printRecipientsDiff(w io.Writer, repo *gitwt.Repo) {
	added, removed, err := agecrypt.RecipientsDiff(repo)
	if err != nil {
		fmt.Fprintln(w, "recipients:", err)
		return
	}
	for _, f := range added {
		fmt.Fprintln(w, "  + key", f, "(not pinned)")
	}
	for _, f := range removed {
		fmt.Fprintln(w, "  - key", f, "(pinned, gone upstream)")
	}
}

// labelCmd is `recipients label self --host <label> [--repo <name> | --all]`:
// it labels this host's own recipient line in an existing repo (or in every
// repo of the routes file with --all), commits and pushes it, re-pins, and
// records the label as the config's hostLabel.
func labelCmd(stdout, stderr io.Writer, sel repoSel, args []string, label string) int {
	all := false
	var rest []string
	for _, a := range args {
		if a == "--all" {
			all = true
		} else {
			rest = append(rest, a)
		}
	}
	recovery := ""
	switch {
	case len(rest) == 1 && rest[0] == "self" && label != "":
		if !agecrypt.ValidJoinLabel(label) {
			fmt.Fprintln(stderr, "recipients: label must match ^[a-z0-9][a-z0-9-]{0,31}$")
			return 2
		}
	case len(rest) == 2 && rest[0] == "recovery" && label == "":
		recovery = strings.TrimSpace(rest[1])
		if _, err := agecrypt.ParseRecipients([]byte(recovery + "\n")); err != nil || recovery == "" {
			fmt.Fprintln(stderr, "recipients: not a valid age or ssh public key")
			return 2
		}
	default:
		fmt.Fprintln(stderr, "usage: claude-memsync recipients label self --host <label> | label recovery <public-key>  [--repo <name> | --all]")
		return 2
	}
	var cfgs []config.Config
	if all {
		if sel.Repo != "" || sel.Config != "" {
			fmt.Fprintln(stderr, "recipients: --all does not go with --repo or --config")
			return 2
		}
		path, ok, err := resolveRoutesPath(sel.Routes)
		if err != nil || !ok {
			fmt.Fprintln(stderr, "recipients: --all needs a routes file")
			return 1
		}
		set, err := loadRouted(path)
		if err != nil {
			fmt.Fprintln(stderr, "recipients:", err)
			return 1
		}
		for _, n := range set.names() {
			cfgs = append(cfgs, set.Cfgs[n])
		}
	} else {
		cfg, _, err := resolveRepo(sel, true)
		if err != nil {
			fmt.Fprintln(stderr, "recipients:", err)
			return 1
		}
		cfgs = append(cfgs, cfg)
	}
	code := 0
	for _, cfg := range cfgs {
		var c int
		if recovery != "" {
			c = labelRecoveryOne(stdout, stderr, cfg, recovery)
		} else {
			c = labelSelfOne(stdout, stderr, cfg, label)
		}
		if c > code {
			code = c
		}
	}
	return code
}

func labelSelfOne(stdout, stderr io.Writer, cfg config.Config, label string) int {
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintf(stderr, "recipients: %s: the sync repo is not encrypted\n", cfg.SyncDir)
		return 1
	}
	if cfg.ReadOnly {
		fmt.Fprintln(stderr, "recipients: this host is read-only and never commits; label it from a writable host")
		return 1
	}
	if cfg.HostLabel != "" && cfg.HostLabel != label {
		fmt.Fprintf(stderr, "recipients: this host's hostLabel is already %q\n", cfg.HostLabel)
		return 1
	}
	committed := false
	err := withSyncLockStopping(stderr, cfg.SyncDir, func() error {
		if err := agecrypt.SyncFromOrigin(repo); err != nil {
			fmt.Fprintln(stderr, "recipients: could not bring in the latest remote state; working from this clone:", err)
		}
		if err := agecrypt.LabelSelf(repo, label); err != nil {
			return err
		}
		committed = true
		return joinPush(cfg, repo, false)
	})
	if err != nil {
		if committed {
			fmt.Fprintln(stderr, "recipients: labelled locally but the push failed; the daemon pushes it on its next cycle")
		} else {
			return recipientsFail(stderr, err)
		}
	}
	// Record the label in the host's own config.json as it is on disk (not the
	// trust-filtered copy), so no other key is dropped.
	if raw, lerr := config.Load(cfg.Path()); lerr == nil {
		raw.HostLabel = label
		if serr := config.Save(raw); serr != nil {
			fmt.Fprintln(stderr, "recipients: could not save hostLabel:", serr)
			return 1
		}
	}
	fmt.Fprintf(stdout, "labelled this host %s in %s\n", label, cfg.SyncDir)
	if err != nil {
		return 1
	}
	return 0
}

// labelRecoveryOne marks the recipient line for key as the recovery key in one
// repo and records the key in that repo's config as a recoveryRecipient.
func labelRecoveryOne(stdout, stderr io.Writer, cfg config.Config, key string) int {
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintf(stderr, "recipients: %s: the sync repo is not encrypted\n", cfg.SyncDir)
		return 1
	}
	if cfg.ReadOnly {
		fmt.Fprintln(stderr, "recipients: this host is read-only and never commits; mark the key from a writable host")
		return 1
	}
	committed := false
	err := withSyncLockStopping(stderr, cfg.SyncDir, func() error {
		if err := agecrypt.SyncFromOrigin(repo); err != nil {
			fmt.Fprintln(stderr, "recipients: could not bring in the latest remote state; working from this clone:", err)
		}
		if err := agecrypt.LabelRecovery(repo, key); err != nil {
			return err
		}
		committed = true
		return joinPush(cfg, repo, false)
	})
	if err != nil {
		if !committed {
			return recipientsFail(stderr, err)
		}
		fmt.Fprintln(stderr, "recipients: marked locally but the push failed; the daemon pushes it on its next cycle")
	}
	if raw, lerr := config.Load(cfg.Path()); lerr == nil {
		have := false
		for _, r := range raw.RecoveryRecipients {
			if r == key {
				have = true
			}
		}
		if !have {
			raw.RecoveryRecipients = append(raw.RecoveryRecipients, key)
		}
		if serr := config.Save(raw); serr != nil {
			fmt.Fprintln(stderr, "recipients: could not save recoveryRecipients:", serr)
			return 1
		}
	}
	fmt.Fprintf(stdout, "marked the recovery key in %s\n", cfg.SyncDir)
	if err != nil {
		return 1
	}
	return 0
}
