package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
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
		syncpkg.ClearRecipientsNotice(cfg.SyncDir)
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
		syncpkg.ClearRecipientsNotice(cfg.SyncDir)
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
	if syncpkg.RecipientsNotice(cfg.SyncDir) {
		out = append(out, "warning: .age-recipients changed upstream; review with `claude-memsync recipients list`")
	}
	return out
}
