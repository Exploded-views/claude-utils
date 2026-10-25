package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

const rekeyUsage = "usage: claude-memsync recipients rekey-pq (--repo <name> | --all | --config <path>) --recovery-recipient <age1pq1...> --host <label> [--fresh-root] [--apply]\n       claude-memsync recipients rekey-pq (--repo <name> | --all) --drop-backup"

// rekeyPQCmd is `recipients rekey-pq`: it migrates an existing classic repo
// whose only member is this host to the post-quantum scheme. It is a dry run
// unless --apply is given.
func rekeyPQCmd(stdout, stderr io.Writer, sel repoSel, args []string, hostLabel string) int {
	args, recovery := extractFlag(args, "--recovery-recipient")
	var all, fresh, apply, drop bool
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		case "--fresh-root":
			fresh = true
		case "--apply":
			apply = true
		case "--drop-backup":
			drop = true
		default:
			fmt.Fprintf(stderr, "recipients rekey-pq: unexpected argument %q\n%s\n", a, rekeyUsage)
			return 2
		}
	}
	recovery = strings.TrimSpace(recovery)
	if drop {
		if apply || fresh || recovery != "" || hostLabel != "" {
			fmt.Fprintln(stderr, "recipients rekey-pq: --drop-backup goes alone (with --repo or --all)")
			return 2
		}
	} else if recovery == "" || hostLabel == "" {
		fmt.Fprintln(stderr, rekeyUsage)
		return 2
	}
	var cfgs []config.Config
	var all2 []config.Config // every repo of the host, for the identity-retirement check
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
		all2 = cfgs
	} else {
		cfg, _, err := resolveRepo(sel, true)
		if err != nil {
			fmt.Fprintln(stderr, "recipients:", err)
			return 1
		}
		cfgs = append(cfgs, cfg)
		all2 = cfgs
		if path, ok, rerr := resolveRoutesPath(sel.Routes); rerr == nil && ok {
			if set, lerr := loadRouted(path); lerr == nil {
				all2 = nil
				for _, n := range set.names() {
					all2 = append(all2, set.Cfgs[n])
				}
			}
		}
	}
	code := 0
	for _, cfg := range cfgs {
		var c int
		if drop {
			c = dropBackupOne(stdout, stderr, cfg)
		} else {
			c = rekeyOne(stdout, stderr, cfg, all2, agecrypt.RekeyOpts{
				Label: hostLabel, Recovery: recovery, ConfiguredRecovery: cfg.RecoveryRecipients, FreshRoot: fresh,
			}, apply)
		}
		if c > code {
			code = c
		}
	}
	return code
}

func dropBackupOne(stdout, stderr io.Writer, cfg config.Config) int {
	repo := gitwt.New(cfg.SyncDir)
	err := withSyncLock(cfg.SyncDir, func() error { return agecrypt.DropBackup(repo) })
	if err != nil {
		return recipientsFail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s: dropped %s and purged the old objects (reflogs expired, gc --prune=now)\n", cfg.SyncDir, agecrypt.BackupRef)
	return 0
}

// newIdentityPath is the hybrid identity's file beside the classic one.
func newIdentityPath(old string) string {
	ext := filepath.Ext(old)
	return strings.TrimSuffix(old, ext) + "-pq" + ext
}

func rekeyOne(stdout, stderr io.Writer, cfg config.Config, hostCfgs []config.Config, o agecrypt.RekeyOpts, apply bool) int {
	repo := gitwt.New(cfg.SyncDir)
	if cfg.ReadOnly {
		fmt.Fprintln(stderr, "recipients: this host is read-only and never commits; run it on a writable member host")
		return 1
	}
	if cfg.HostLabel != "" && cfg.HostLabel != o.Label {
		fmt.Fprintf(stderr, "recipients: this host's hostLabel is already %q\n", cfg.HostLabel)
		return 1
	}
	if repo.IsRepo() {
		o.NewIdentityPath = newIdentityPath(agecrypt.IdentityPath(repo))
	}
	if !apply {
		p, err := agecrypt.PlanRekeyPQ(repo, o, false)
		if err != nil {
			fmt.Fprintf(stderr, "recipients: %s: %v\n", cfg.SyncDir, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: rekey-pq plan (dry run; nothing changed)\n", cfg.SyncDir)
		fmt.Fprintf(stdout, "  recipients now: %d; after: 2 (this host as host:%s, and the recovery key)\n", p.Recipients, o.Label)
		fmt.Fprintf(stdout, "  files to re-encrypt: %d; join requests dropped: %d\n", p.Blobs, p.JoinRequests)
		if o.FreshRoot {
			fmt.Fprintf(stdout, "  history (%d commits) will be REPLACED by one root commit and force-pushed (--force-with-lease on the current tip)\n", p.Commits)
		} else {
			fmt.Fprintf(stdout, "  one commit on top of the current history; classic ciphertext stays in history (use --fresh-root to replace it)\n")
		}
		fmt.Fprintf(stdout, "  a new hybrid identity is created at %s; the pre-migration tip is kept at %s\n", o.NewIdentityPath, agecrypt.BackupRef)
		fmt.Fprintln(stdout, "  add --apply to do it (the daemon must be stopped)")
		return 0
	}
	var plan agecrypt.RekeyPlan
	var pushed bool
	err := withSyncLock(cfg.SyncDir, func() error {
		p, err := agecrypt.ApplyRekeyPQ(repo, o)
		if err != nil {
			return err
		}
		plan = p
		// The config follows the repo from here on.
		cfgFile := cfg.ConfigPath
		if cfgFile == "" {
			cfgFile = cfg.Path()
		}
		raw, lerr := config.Load(cfgFile)
		if lerr != nil {
			return lerr
		}
		raw.EncryptionScheme = agecrypt.SchemePQ
		raw.HostLabel = o.Label
		raw.RecoveryRecipients = []string{o.Recovery}
		if err := config.SaveAt(cfgFile, raw); err != nil {
			return err
		}
		if err := rekeyPush(cfg, repo, p, o.FreshRoot); err != nil {
			return err
		}
		pushed = true
		return nil
	})
	if err != nil {
		if plan.NewCommit != "" && !pushed {
			fmt.Fprintf(stderr, "recipients: %s: migrated locally but the push failed (%v); the pre-migration tip is at %s\n", cfg.SyncDir, err, agecrypt.BackupRef)
			return 1
		}
		return recipientsFail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s: migrated to post-quantum; public recipient %s\n", cfg.SyncDir, plan.NewRecipient)
	if o.FreshRoot {
		c, b, _ := agecrypt.VerifyHybridRemote(repo, defaultBranch, 1)
		fmt.Fprintf(stdout, "  verified origin/main: %d commit, %d files, all hybrid ciphertext\n", c, b)
	} else {
		fmt.Fprintf(stdout, "  re-encrypted %d files and pushed\n", plan.Blobs)
	}
	fmt.Fprintf(stdout, "  rollback point kept at %s; delete it later with `recipients rekey-pq --drop-backup`\n", agecrypt.BackupRef)
	retireClassicIdentity(stdout, plan.IdentityPath, cfg, hostCfgs)
	return 0
}

// rekeyPush publishes the migrated repo: a plain push, or with a fresh root a
// force-with-lease on the old tip, then a check of what the remote now holds.
func rekeyPush(cfg config.Config, repo *gitwt.Repo, p agecrypt.RekeyPlan, fresh bool) error {
	if cfg.PrivateOnly {
		if err := validateRemoteOffline(cfg); err != nil {
			return err
		}
		if err := checkPushURL(cfg, cfg.SyncDir); err != nil {
			return err
		}
	}
	if err := agecrypt.PushGuard(repo, defaultBranch); err != nil {
		return err
	}
	if !fresh {
		if err := repo.Push(defaultBranch); err != nil {
			return err
		}
		// Classic history stays by design; the pushed tip must be hybrid only.
		return verifyTip(repo)
	}
	if _, err := repo.Run("push", "--force-with-lease=main:"+p.Tip, "origin", p.NewCommit+":refs/heads/main"); err != nil {
		return err
	}
	if _, _, err := agecrypt.VerifyHybridRemote(repo, defaultBranch, 1); err != nil {
		return fmt.Errorf("pushed, but the remote does not verify: %w", err)
	}
	return nil
}

// verifyTip checks the pushed tip holds hybrid ciphertext only.
func verifyTip(repo *gitwt.Repo) error {
	if _, _, err := agecrypt.VerifyHybridRemote(repo, defaultBranch, 0); err != nil {
		return fmt.Errorf("pushed, but the remote does not verify: %w", err)
	}
	return nil
}

// clonesUsingIdentity lists the git clones under the parent directory of each
// sync dir (siblings such as ~/.claudesync.d/<name>.incident-1 included, routed
// or not) whose repo-local memage.identity names the file old.
func clonesUsingIdentity(old string, hostCfgs []config.Config) []string {
	want := filepath.Clean(filepath.FromSlash(old))
	parents := map[string]bool{}
	for _, c := range hostCfgs {
		parents[filepath.Dir(filepath.Clean(c.SyncDir))] = true
	}
	var out []string
	seen := map[string]bool{}
	check := func(gitCfg string) {
		dir := filepath.Dir(filepath.Dir(gitCfg))
		if seen[dir] {
			return
		}
		seen[dir] = true
		if id := identityInGitConfig(gitCfg); id != "" && filepath.Clean(filepath.FromSlash(id)) == want {
			out = append(out, dir)
		}
	}
	for _, c := range hostCfgs {
		check(filepath.Join(c.SyncDir, ".git", "config"))
	}
	for p := range parents {
		matches, _ := filepath.Glob(filepath.Join(p, "*", ".git", "config"))
		for _, m := range matches {
			check(m)
		}
	}
	sort.Strings(out)
	return out
}

// identityInGitConfig reads memage.identity out of a git config file without
// running git (the file may belong to any clone).
func identityInGitConfig(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			f := strings.Fields(strings.Trim(l, "[]"))
			in = len(f) > 0 && strings.EqualFold(f[0], "memage")
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok && strings.EqualFold(strings.TrimSpace(k), "identity") {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// retireClassicIdentity renames the old classic identity to
// <name>.classic-retired once no clone on this host still points at it: every
// git clone beside the sync dirs counts, routed or not.
func retireClassicIdentity(stdout io.Writer, old string, done config.Config, hostCfgs []config.Config) {
	if old == "" {
		return
	}
	if _, err := os.Stat(old); err != nil {
		return // already retired (a rerun)
	}
	cfgs := append(append([]config.Config{}, hostCfgs...), done)
	if users := clonesUsingIdentity(old, cfgs); len(users) > 0 {
		fmt.Fprintf(stdout, "  the classic identity %s is kept: still used by %s\n", old, strings.Join(users, ", "))
		fmt.Fprintln(stdout, "  rekey those clones (or remove them), then run this command again to retire it")
		return
	}
	if err := os.Rename(old, old+".classic-retired"); err != nil {
		fmt.Fprintln(stdout, "  could not retire the classic identity:", err)
		return
	}
	fmt.Fprintf(stdout, "  the classic identity is kept as %s (needed to read the rollback point)\n", old+".classic-retired")
}
