package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
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

// runRecipients handles `claude-memsync recipients add|remove|list`.
func runRecipients(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-memsync recipients add <public-key> | remove <public-key> | list")
		return 2
	}
	cfg, err := loadDistillConfig("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "recipients:", err)
		return 1
	}
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintln(os.Stderr, "recipients: the sync repo is not encrypted (init it with --encrypt)")
		return 1
	}
	switch args[0] {
	case "list":
		keys, err := agecrypt.ListRecipients(cfg.SyncDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "recipients:", err)
			return 1
		}
		for _, k := range keys {
			fmt.Println(k)
		}
		if u, _ := agecrypt.Unreviewed(repo); u {
			printRecipientsDiff(repo)
		}
	case "accept":
		if u, _ := agecrypt.Unreviewed(repo); !u {
			fmt.Fprintln(os.Stderr, "recipients: nothing to accept; the work-tree matches the pinned set")
			return 0
		}
		printRecipientsDiff(repo)
		if len(args) != 2 || args[1] != "--yes" {
			fmt.Fprintln(os.Stderr, "recipients: review the change above; if you trust every added key, run `claude-memsync recipients accept --yes`")
			return 1
		}
		if err := agecrypt.AcceptRecipients(repo); err != nil {
			fmt.Fprintln(os.Stderr, "recipients:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "recipients accepted; syncing resumes on the next cycle")
	case "add", "remove":
		if len(args) != 2 {
			fmt.Fprintf(os.Stderr, "usage: claude-memsync recipients %s <public-key>\n", args[0])
			return 2
		}
		if args[0] == "add" {
			err = agecrypt.AddRecipient(repo, args[1])
		} else {
			err = agecrypt.RemoveRecipient(repo, args[1])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "recipients:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "recipients updated and committed; the daemon pushes it on its next cycle")
		if args[0] == "remove" {
			fmt.Fprintln(os.Stderr, "note: the removed key still reads everything it already fetched; only new content is closed to it")
		}
	default:
		fmt.Fprintf(os.Stderr, "recipients: unknown action %q\n", args[0])
		return 2
	}
	return 0
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
func printRecipientsDiff(repo *gitwt.Repo) {
	added, removed, err := agecrypt.RecipientsDiff(repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "recipients:", err)
		return
	}
	for _, f := range added {
		fmt.Fprintln(os.Stderr, "  + key", f, "(not pinned)")
	}
	for _, f := range removed {
		fmt.Fprintln(os.Stderr, "  - key", f, "(pinned, gone upstream)")
	}
}
