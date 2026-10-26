package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
)

const identityUsage = "usage: claude-memsync identity backup --store-cmd \"<argv>\" [--verify-cmd \"<argv>\"] [--identity <path> | --all | --repo <name> | --config <path>] [--routes <path>] [--dry-run]"

// maxIdentityBytes bounds an identity file: a real one is a few hundred bytes,
// and the verify command's output is capped at secretcmd.MaxOutput anyway.
const maxIdentityBytes = secretcmd.MaxOutput

// runIdentity handles `claude-memsync identity <subcommand>`.
func runIdentity(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, identityUsage)
		return 2
	}
	switch args[0] {
	case "backup":
		return identityBackupCmd(os.Stdout, os.Stderr, args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, identityUsage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "identity: unknown subcommand %q\n%s\n", args[0], identityUsage)
	return 2
}

// idFileInfo is one identity file that passed every check.
type idFileInfo struct {
	Path        string
	Fingerprint string // as `join` shows it
	data        []byte // the file's bytes; zeroed after use
}

// readIdentityFile checks the identity file at path and reads it. It refuses
// anything but a regular file owned by the user, with no other hard link and
// no group or other permissions, that parses as an age (classic or hybrid)
// identity. The contents never appear in an error.
func readIdentityFile(path string) (*idFileInfo, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("cannot read the identity file: " + errReason(err))
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("the identity file is a symlink; name the real file")
	}
	if !lst.Mode().IsRegular() {
		return nil, errors.New("the identity file is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read the identity file: " + errReason(err))
	}
	defer f.Close()
	st, err := f.Stat() // the opened file, not the name: it cannot have been swapped since the Lstat
	if err != nil {
		return nil, errors.New("cannot read the identity file: " + errReason(err))
	}
	if !os.SameFile(lst, st) || !st.Mode().IsRegular() {
		return nil, errors.New("the identity file changed while it was checked")
	}
	if err := identityModeOK(st); err != nil {
		return nil, err
	}
	if err := identityOwnerLinksOK(st); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxIdentityBytes+1))
	if err != nil {
		wipe(data)
		return nil, errors.New("cannot read the identity file: " + errReason(err))
	}
	if len(data) > maxIdentityBytes {
		wipe(data)
		return nil, errors.New("the identity file is too large to be an age identity")
	}
	rcpt, err := identityRecipientOf(data)
	if err != nil {
		wipe(data)
		return nil, err
	}
	return &idFileInfo{Path: path, Fingerprint: agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(rcpt)), data: data}, nil
}

// identityRecipientOf parses data as an age identity file and returns the
// public recipient of its first key.
func identityRecipientOf(data []byte) (string, error) {
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil || len(ids) == 0 {
		return "", errors.New("the file does not parse as an age identity")
	}
	switch x := ids[0].(type) {
	case *age.X25519Identity:
		return x.Recipient().String(), nil
	case *age.HybridIdentity:
		return x.Recipient().String(), nil
	}
	return "", errors.New("the file is not an age X25519 or hybrid identity")
}

// errReason is an OS error without the path, which the caller already prints.
func errReason(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// wipe zeroes b, best effort (Go may have copied it, and the OS may have
// paged it out).
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// identityPaths resolves the files to back up: the one named by --identity, or
// the distinct memage.identity of the clones --repo, --config or (otherwise)
// every routed clone of this host name.
func identityPaths(sel repoSel, all bool, explicit string) ([]string, error) {
	if explicit != "" {
		return []string{explicit}, nil
	}
	var syncDirs []string
	if sel.Repo != "" || sel.Config != "" {
		cfg, _, err := resolveRepo(sel, true)
		if err != nil {
			return nil, err
		}
		syncDirs = []string{cfg.SyncDir}
	} else if path, ok, err := resolveRoutesPath(sel.Routes); err != nil {
		return nil, err
	} else if ok {
		set, err := loadRouted(path)
		if err != nil {
			return nil, err
		}
		for _, n := range set.names() {
			syncDirs = append(syncDirs, set.Cfgs[n].SyncDir)
		}
	} else {
		cfg, err := loadDistillConfig("")
		if err != nil {
			return nil, err
		}
		syncDirs = []string{cfg.SyncDir}
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range syncDirs {
		repo := gitwt.New(d)
		if !repo.IsRepo() {
			continue
		}
		p := agecrypt.IdentityPath(repo)
		if p == "" {
			continue
		}
		p = normIdentityPath(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no clone on this host names an identity (memage.identity); name the file with --identity <path> (the default location is %s)", defaultIdentityFor(agecrypt.SchemePQ))
	}
	return out, nil
}

// identityBackupCmd is `identity backup`: it stores each identity file through
// the store command (the bytes on its stdin), optionally reads it back through
// the verify command, and prints the path, fingerprint and result. The key is
// never printed, logged or passed in an argument or the environment.
func identityBackupCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("identity backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeLine := fs.String("store-cmd", "", "command that stores the identity read from its stdin (argv, no shell)")
	verifyLine := fs.String("verify-cmd", "", "command that prints the stored bytes on stdout, to compare with the file")
	explicit := fs.String("identity", "", "back up this identity file instead of the clones' memage.identity")
	all := fs.Bool("all", false, "every distinct identity the routed clones of this host name (the default)")
	dry := fs.Bool("dry-run", false, "list what would be stored and where; run nothing")
	var sel repoSel
	fs.StringVar(&sel.Repo, "repo", "", "back up the identity of this routed repo")
	fs.StringVar(&sel.Config, "config", "", "back up the identity of the repo of this config.json")
	fs.StringVar(&sel.Routes, "routes", "", "routes file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "identity backup: %s\n%s\n", msg, identityUsage)
		return 2
	}
	if fs.NArg() > 0 {
		return usage(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if *storeLine == "" {
		return usage("--store-cmd is required")
	}
	storeArgv, err := secretcmd.SplitArgs(*storeLine)
	if err != nil || len(storeArgv) == 0 {
		return usage("--store-cmd is not a usable command")
	}
	var verifyArgv []string
	if *verifyLine != "" {
		if verifyArgv, err = secretcmd.SplitArgs(*verifyLine); err != nil || len(verifyArgv) == 0 {
			return usage("--verify-cmd is not a usable command")
		}
	}
	if *explicit != "" && (*all || sel.Repo != "" || sel.Config != "") {
		return usage("--identity does not go with --all, --repo or --config")
	}
	if *all && (sel.Repo != "" || sel.Config != "") {
		return usage("--all does not go with --repo or --config")
	}
	paths, err := identityPaths(sel, *all, *explicit)
	if err != nil {
		fmt.Fprintln(stderr, "identity backup:", err)
		return 1
	}

	failed := 0
	if *dry {
		fmt.Fprintln(stdout, "dry run: nothing is run or stored")
	} else if len(verifyArgv) == 0 {
		fmt.Fprintln(stderr, "warning: no --verify-cmd, so the backup is UNVERIFIED: nothing reads it back to check it was stored intact; add --verify-cmd, or restore it once to be sure")
	}
	for _, p := range paths {
		info, err := readIdentityFile(p)
		if err != nil {
			fmt.Fprintf(stdout, "%s\tREFUSED\n", p)
			fmt.Fprintf(stderr, "identity backup: %s: %v\n", p, err)
			failed++
			continue
		}
		if *dry {
			v := ""
			if len(verifyArgv) > 0 {
				v = ", then read back with " + verifyArgv[0]
			}
			fmt.Fprintf(stdout, "%s\tfingerprint %s\twould be stored with %s%s\n", p, info.Fingerprint, storeArgv[0], v)
			wipe(info.data)
			continue
		}
		res, err := storeAndVerify(stderr, info, storeArgv, verifyArgv)
		wipe(info.data)
		fmt.Fprintf(stdout, "%s\tfingerprint %s\t%s\n", p, info.Fingerprint, res)
		if err != nil {
			fmt.Fprintf(stderr, "identity backup: %s: %v\n", p, err)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// storeAndVerify stores info's bytes and, with a verify command, reads them
// back and compares SHA-256 digests. res is what to print; err is non-nil
// for a failure.
func storeAndVerify(stderr io.Writer, info *idFileInfo, storeArgv, verifyArgv []string) (res string, err error) {
	fmt.Fprintln(stderr, secretcmd.WaitMessage)
	ctx := context.Background()
	timeout := secretcmd.KeyCmdTimeout()
	if err := secretcmd.Feed(ctx, storeArgv, info.data, timeout); err != nil {
		return "NOT STORED", fmt.Errorf("the store command did not succeed: %w", err)
	}
	if len(verifyArgv) == 0 {
		return "stored (unverified)", nil
	}
	got, err := secretcmd.Run(ctx, verifyArgv, timeout)
	if err != nil {
		wipe(got)
		return "stored, NOT VERIFIED", fmt.Errorf("the verify command did not succeed: %w", err)
	}
	want := sha256.Sum256(info.data)
	have := sha256.Sum256(got)
	wipe(got)
	if subtle.ConstantTimeCompare(want[:], have[:]) != 1 {
		return "MISMATCH", errors.New("what the verify command returned differs from the identity file; the stored copy is not a usable backup")
	}
	return "verified (sha256 matches)", nil
}
