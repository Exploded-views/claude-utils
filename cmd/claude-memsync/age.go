package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// runAgeFilter is the git long-running filter process (`filter.memage.process`).
// Git starts it in the work-tree root; stdout carries only the protocol.
func runAgeFilter() int {
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

const recipientsUsage = "usage: claude-memsync recipients add <public-key> | remove <public-key> | list | accept --yes  [--repo <name> | --config <path>] [--routes <path>]"

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
	cfg, _, err := resolveRepo(sel, true)
	if err != nil {
		fmt.Fprintln(stderr, "recipients:", err)
		return 1
	}
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintln(stderr, "recipients: the sync repo is not encrypted (init it with --encrypt)")
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
		if err := withSyncLock(cfg.SyncDir, func() error { return agecrypt.AcceptRecipients(repo) }); err != nil {
			return recipientsFail(stderr, err)
		}
		fmt.Fprintln(stderr, "recipients accepted; syncing resumes on the next cycle")
	case "add", "remove":
		if len(args) != 2 {
			fmt.Fprintf(stderr, "usage: claude-memsync recipients %s <public-key>\n", args[0])
			return 2
		}
		err = withSyncLock(cfg.SyncDir, func() error {
			if args[0] == "add" {
				return agecrypt.AddRecipient(repo, args[1])
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
	if u, _ := agecrypt.Unreviewed(repo); u {
		out = append(out, "recipients UNREVIEWED: .age-recipients differs from the set pinned on this host; nothing is committed or pushed. Review with `claude-memsync recipients list`, then `recipients accept --yes`")
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
