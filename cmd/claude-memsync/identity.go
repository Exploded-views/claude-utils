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
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
)

const identityUsage = "usage: claude-memsync identity backup --store-cmd \"<argv>\" [--verify-cmd \"<argv>\"] [--identity <path> | --all | --repo <name> | --config <path>] [--routes <path>] [--dry-run]"

// hardenFn is hardenProcess and readIdentityFn is readIdentityFile; tests
// replace them.
var (
	hardenFn       = hardenProcess
	readIdentityFn = readIdentityFile
)

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

// idTarget is one identity file to back up and why it is included.
type idTarget struct {
	Path  string
	users []string // clones whose memage.identity names it
	extra []string // other reasons
	fi    os.FileInfo
}

func (t *idTarget) why() string {
	var parts []string
	if len(t.users) > 0 {
		parts = append(parts, "used by: "+strings.Join(t.users, ", "))
	}
	parts = append(parts, t.extra...)
	return strings.Join(parts, "; ")
}

type idTargets []*idTarget

// add records path with a reason, merging into an earlier entry for the same
// file (the same spelling, or the same file under another name).
func (ts *idTargets) add(path, user, extra string) {
	fi, _ := os.Lstat(path)
	var t *idTarget
	for _, o := range *ts {
		if o.Path == path || (fi != nil && o.fi != nil && os.SameFile(fi, o.fi)) {
			t = o
			break
		}
	}
	if t == nil {
		t = &idTarget{Path: path, fi: fi}
		*ts = append(*ts, t)
	}
	if user != "" {
		for _, u := range t.users {
			if u == user {
				return
			}
		}
		t.users = append(t.users, user)
	}
	if extra != "" {
		for _, e := range t.extra {
			if e == extra {
				return
			}
		}
		t.extra = append(t.extra, extra)
	}
}

const (
	whyRelated = "related clones"
	whyClassic = "classic history and rollback"
)

// expandIdentityPath makes a configured path absolute (~ expanded) without
// resolving symlinks: a symlinked identity must stay visible to the checks.
func expandIdentityPath(p string) string {
	p = filepath.FromSlash(strings.TrimSpace(p))
	if p == "~" || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// classicOf is the classic file beside a hybrid identity (identity-pq.txt ->
// identity.txt); any other path is its own classic.
func classicOf(p string) string {
	ext := filepath.Ext(p)
	if base := strings.TrimSuffix(p, ext); strings.HasSuffix(base, "-pq") {
		return strings.TrimSuffix(base, "-pq") + ext
	}
	return p
}

// identityPaths resolves the files to back up. --identity names one. Otherwise
// the memage.identity of the clone --repo or --config names, or of every clone
// on this host; for the whole host also the identities related read clones use
// (the default ones). A classic identity and its *.classic-retired that
// rekey-pq keeps beside any of them come too. Clones that contribute nothing
// are returned in skipped, with the reason.
func identityPaths(sel repoSel, all bool, explicit string) (targets idTargets, skipped []string, err error) {
	if explicit != "" {
		targets.add(explicit, "", "named with --identity")
		return targets, nil, nil
	}
	type clone struct{ label, dir string }
	var clones []clone
	wholeHost := sel.Repo == "" && sel.Config == ""
	if !wholeHost {
		cfg, name, err := resolveRepo(sel, true)
		if err != nil {
			return nil, nil, err
		}
		if name == "" {
			name = cfg.SyncDir
		}
		clones = []clone{{name, cfg.SyncDir}}
	} else if path, ok, err := resolveRoutesPath(sel.Routes); err != nil {
		return nil, nil, err
	} else if ok {
		set, err := loadRouted(path)
		if err != nil {
			return nil, nil, err
		}
		for _, n := range set.names() {
			clones = append(clones, clone{n, set.Cfgs[n].SyncDir})
		}
	} else {
		cfg, err := loadDistillConfig("")
		if err != nil {
			return nil, nil, err
		}
		clones = []clone{{cfg.SyncDir, cfg.SyncDir}}
	}
	for _, c := range clones {
		repo := gitwt.New(c.dir)
		if !repo.IsRepo() {
			skipped = append(skipped, "skipped: "+c.dir+" (not a git repo)")
			continue
		}
		p := agecrypt.IdentityPath(repo)
		if p == "" {
			skipped = append(skipped, "skipped: "+c.dir+" (no memage.identity: the repo is not encrypted)")
			continue
		}
		targets.add(expandIdentityPath(p), c.label, "")
	}
	if wholeHost {
		// What related read clones decrypt with (related_age.go).
		pq := defaultIdentityFor(agecrypt.SchemePQ)
		for _, p := range []string{pq, defaultIdentityPath()} {
			if p != "" && lexists(p) {
				targets.add(expandIdentityPath(p), "", whyRelated)
			}
		}
	}
	// The classic identity and its retired copy that rekey-pq keeps.
	for _, t := range append(idTargets(nil), targets...) {
		c := classicOf(t.Path)
		if c != t.Path && lexists(c) {
			targets.add(c, "", whyClassic)
		}
		if lexists(c + ".classic-retired") {
			targets.add(c+".classic-retired", "", whyClassic)
		}
	}
	if len(targets) == 0 {
		return nil, skipped, fmt.Errorf("no clone on this host names an identity (memage.identity); name the file with --identity <path> (the default location is %s)", defaultIdentityFor(agecrypt.SchemePQ))
	}
	return targets, skipped, nil
}

// identityBackupCmd is `identity backup`: it stores each identity file through
// the store command (the bytes on its stdin), optionally reads it back through
// the verify command, and prints the path, fingerprint and result. The key is
// never printed, logged or passed in an argument or the environment.
func identityBackupCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("identity backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeLine := fs.String("store-cmd", "", "command that stores the identity read from its stdin (argv, no shell; {fingerprint} and {name} are replaced per file, and needed when several files are found)")
	verifyLine := fs.String("verify-cmd", "", "command that prints the stored bytes on stdout, to compare with the file")
	explicit := fs.String("identity", "", "back up this identity file instead of the clones' memage.identity")
	all := fs.Bool("all", false, "every distinct identity the routed clones of this host name (the default)")
	dry := fs.Bool("dry-run", false, "list what would be stored and where; run nothing")
	var sel repoSel
	fs.StringVar(&sel.Repo, "repo", "", "back up the identity of this routed repo")
	fs.StringVar(&sel.Config, "config", "", "back up the identity of the repo of this config.json")
	fs.StringVar(&sel.Routes, "routes", "", "routes file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
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
	hardenFn()
	paths, skipped, err := identityPaths(sel, *all, *explicit)
	for _, l := range skipped {
		fmt.Fprintln(stdout, l)
	}
	if err != nil {
		fmt.Fprintln(stderr, "identity backup:", err)
		return 1
	}

	// Read and check every file first, so a problem with the item names is
	// found before anything is stored.
	type loaded struct {
		path  string
		why   string
		info  *idFileInfo
		err   error
		dupOf string // the first path holding the same bytes
		sum   [sha256.Size]byte
	}
	files := make([]*loaded, 0, len(paths))
	defer func() {
		for _, f := range files {
			if f.info != nil {
				wipe(f.info.data)
			}
		}
	}()
	var uniq []*loaded // one per distinct content
	for _, t := range paths {
		info, err := readIdentityFn(t.Path)
		f := &loaded{path: t.Path, why: t.why(), info: info, err: err}
		files = append(files, f)
		if err != nil {
			continue
		}
		f.sum = sha256.Sum256(info.data)
		for _, u := range uniq {
			if u.sum == f.sum {
				f.dupOf = u.path
				break
			}
		}
		if f.dupOf == "" {
			uniq = append(uniq, f)
		}
	}
	if len(uniq) > 1 && !hasPlaceholder(storeArgv) {
		fmt.Fprintf(stderr, "identity backup: %d identities would go through one fixed --store-cmd, and a manager that replaces an item by name would keep only the last key. Nothing was run.\n  files:\n", len(uniq))
		for _, u := range uniq {
			fmt.Fprintf(stderr, "    %s\n", u.path)
		}
		fmt.Fprintln(stderr, "  Add {fingerprint} (or {name}) to the item's title in --store-cmd and --verify-cmd, or back the files up one at a time with --identity <path>.")
		return 2
	}
	storeKeys, verifyKeys := map[string]string{}, map[string]string{}
	for _, u := range uniq {
		k := strings.Join(substitute(storeArgv, u.info.Fingerprint, u.path), "\x00")
		if other, dup := storeKeys[k]; dup {
			fmt.Fprintf(stderr, "identity backup: %s and %s hold different keys but would be stored under the same item (--store-cmd expands to the same arguments for both). Nothing was run; add {fingerprint} to the title.\n", other, u.path)
			return 2
		}
		storeKeys[k] = u.path
		if len(uniq) > 1 && len(verifyArgv) > 0 {
			k = strings.Join(substitute(verifyArgv, u.info.Fingerprint, u.path), "\x00")
			if other, dup := verifyKeys[k]; dup {
				fmt.Fprintf(stderr, "identity backup: --verify-cmd expands to the same arguments for %s and %s, so it would read the same item back for both and could not notice an overwrite. Nothing was run; put {fingerprint} where --verify-cmd names the item.\n", other, u.path)
				return 2
			}
			verifyKeys[k] = u.path
		}
	}

	failed, verified, unverified := 0, 0, 0
	if *dry {
		fmt.Fprintln(stdout, "dry run: nothing is run or stored")
	} else if len(verifyArgv) == 0 {
		fmt.Fprintln(stderr, "warning: no --verify-cmd, so the backup is UNVERIFIED: nothing reads it back to check it was stored intact; add --verify-cmd, or restore it once to be sure")
	}
	var stored []*loaded // items to read back again at the end
	for _, f := range files {
		p, info := f.path, f.info
		why := "\t(" + f.why + ")"
		if f.err != nil {
			fmt.Fprintf(stdout, "%s\tREFUSED%s\n", p, why)
			fmt.Fprintf(stderr, "identity backup: %s: %v\n", p, f.err)
			failed++
			continue
		}
		if f.dupOf != "" {
			fmt.Fprintf(stdout, "%s\tfingerprint %s\tsame key as %s (stored once)%s\n", p, info.Fingerprint, f.dupOf, why)
			continue
		}
		sArgv := substitute(storeArgv, info.Fingerprint, p)
		var vArgv []string
		if len(verifyArgv) > 0 {
			vArgv = substitute(verifyArgv, info.Fingerprint, p)
		}
		if *dry {
			v := ""
			if len(vArgv) > 0 {
				v = ", then read back with " + vArgv[0]
			}
			fmt.Fprintf(stdout, "%s\tfingerprint %s\twould be stored with %s%s%s\n", p, info.Fingerprint, sArgv[0], v, why)
			continue
		}
		res, err := storeAndVerify(stderr, info, sArgv, vArgv)
		fmt.Fprintf(stdout, "%s\tfingerprint %s\t%s%s\n", p, info.Fingerprint, res, why)
		if err != nil {
			fmt.Fprintf(stderr, "identity backup: %s: %v\n", p, err)
			failed++
			continue
		}
		if len(vArgv) > 0 {
			verified++
			stored = append(stored, f)
		} else {
			unverified++
		}
	}
	// A later store may have replaced an earlier item whatever the argument
	// layout: read every item back once more, after the last store.
	if !*dry && len(stored) > 1 {
		for _, f := range stored {
			vArgv := substitute(verifyArgv, f.info.Fingerprint, f.path)
			got, err := secretcmd.Run(context.Background(), vArgv, secretcmd.KeyCmdTimeout())
			want := sha256.Sum256(f.info.data)
			have := sha256.Sum256(got)
			wipe(got)
			if err == nil && subtle.ConstantTimeCompare(want[:], have[:]) == 1 {
				continue
			}
			msg := "OVERWRITTEN by a later store: " + f.path
			if err != nil {
				msg = "NOT READABLE at the end (" + err.Error() + "): " + f.path
			}
			fmt.Fprintf(stdout, "%s\tfingerprint %s\t%s\n", f.path, f.info.Fingerprint, msg)
			fmt.Fprintf(stderr, "identity backup: %s\n", msg)
			verified--
			failed++
		}
	}
	if !*dry {
		fmt.Fprintf(stdout, "%d stored (%d verified, %d UNVERIFIED), %d failed\n", verified+unverified, verified, unverified, failed)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// Placeholders, replaced in every element of --store-cmd and --verify-cmd for
// each file. They carry only public values, never key material.
const (
	phFingerprint = "{fingerprint}"
	phName        = "{name}"
)

func hasPlaceholder(argv []string) bool {
	for _, a := range argv {
		if strings.Contains(a, phFingerprint) || strings.Contains(a, phName) {
			return true
		}
	}
	return false
}

// substitute returns argv with {fingerprint} (the grouped fingerprint with
// dashes for spaces, safe in a title) and {name} (the file's base name)
// replaced. argv itself is left alone.
func substitute(argv []string, fingerprint, path string) []string {
	fp := strings.ReplaceAll(fingerprint, " ", "-")
	r := strings.NewReplacer(phFingerprint, fp, phName, filepath.Base(path))
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = r.Replace(a)
	}
	return out
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
