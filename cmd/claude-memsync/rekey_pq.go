package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

const rekeyUsage = "usage: claude-memsync recipients rekey-pq (--repo <name> | --all | --config <path>) --recovery-recipient <age1pq1...> --host <label> [--fresh-root] [--apply]\n       claude-memsync recipients rekey-pq (--repo <name> | --all) --rollback\n       claude-memsync recipients rekey-pq (--repo <name> | --all) --drop-backup"

// rekeyTarget is one repo of a rekey-pq run and the name it is reported under.
type rekeyTarget struct {
	label string
	cfg   config.Config
}

// rekeyPQCmd is `recipients rekey-pq`: it migrates an existing classic repo
// whose only member is this host to the post-quantum scheme. It is a dry run
// unless --apply is given. With --all an --apply run stops at the first repo
// that fails and says which repos are done, which failed and which were not
// started; the same command, rerun, resumes. A dry run with --all goes past a
// failing repo, ends with "dry run: N ok, M would fail" and exits nonzero if any
// would fail. --rollback undoes a migration
// that was not pushed.
func rekeyPQCmd(stdout, stderr io.Writer, sel repoSel, args []string, hostLabel string) int {
	args, recovery := extractFlag(args, "--recovery-recipient")
	var all, fresh, apply, drop, rollback bool
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
		case "--rollback":
			rollback = true
		default:
			fmt.Fprintf(stderr, "recipients rekey-pq: unexpected argument %q\n%s\n", a, rekeyUsage)
			return 2
		}
	}
	recovery = strings.TrimSpace(recovery)
	if drop || rollback {
		if drop && rollback {
			fmt.Fprintln(stderr, "recipients rekey-pq: --drop-backup and --rollback are separate runs")
			return 2
		}
		if apply || fresh || recovery != "" || hostLabel != "" {
			fmt.Fprintln(stderr, "recipients rekey-pq: --drop-backup and --rollback go alone (with --repo or --all)")
			return 2
		}
	} else if recovery == "" || hostLabel == "" {
		fmt.Fprintln(stderr, rekeyUsage)
		return 2
	}
	var targets []rekeyTarget
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
			targets = append(targets, rekeyTarget{n, set.Cfgs[n]})
			all2 = append(all2, set.Cfgs[n])
		}
	} else {
		cfg, name, err := resolveRepo(sel, true)
		if err != nil {
			fmt.Fprintln(stderr, "recipients:", err)
			return 1
		}
		if name == "" {
			name = cfg.SyncDir
		}
		targets = append(targets, rekeyTarget{name, cfg})
		all2 = []config.Config{cfg}
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
	var done, failed, notStarted []string
	for i, t := range targets {
		var c int
		switch {
		case drop:
			c = dropBackupOne(stdout, stderr, t.cfg)
		case rollback:
			c = rollbackOne(stdout, stderr, t.cfg, all)
		default:
			c = rekeyOne(stdout, stderr, t.cfg, all2, agecrypt.RekeyOpts{
				Label: hostLabel, Recovery: recovery, ConfiguredRecovery: t.cfg.RecoveryRecipients, FreshRoot: fresh,
			}, apply)
		}
		if c > code {
			code = c
		}
		if c == 0 {
			done = append(done, t.label)
			continue
		}
		failed = append(failed, t.label)
		// A failed apply stops the run: a later repo should not be migrated
		// on top of a host in an unknown state. Dry runs and drops go on.
		if apply && !rollback && !drop {
			for _, r := range targets[i+1:] {
				notStarted = append(notStarted, r.label)
			}
			break
		}
	}
	if all && len(targets) > 1 && (len(failed) > 0 || len(notStarted) > 0) {
		verb := "run"
		if apply {
			verb = "apply run stopped at the first failure"
		}
		fmt.Fprintf(stderr, "recipients rekey-pq --all: %s\n  done: %s\n  failed: %s\n  not started: %s\n", verb, joinOrNone(done), joinOrNone(failed), joinOrNone(notStarted))
		if apply && len(failed) > 0 && !rollback && !drop {
			fmt.Fprintln(stderr, "  fix the cause and rerun the same command to resume (finished repos are skipped), or undo an unpublished repo with `recipients rekey-pq --repo <name> --rollback`")
		}
	}
	if all && !apply && !drop && !rollback {
		// A window script reads this line (and the exit code) to decide
		// whether it is safe to go on to --apply.
		fmt.Fprintf(stderr, "dry run: %d ok, %d would fail\n", len(done), len(failed))
	}
	return code
}

func joinOrNone(l []string) string {
	if len(l) == 0 {
		return "(none)"
	}
	return strings.Join(l, ", ")
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

// cfgFileOf is the config.json this repo's settings were loaded from.
func cfgFileOf(cfg config.Config) string {
	if cfg.ConfigPath != "" {
		return cfg.ConfigPath
	}
	return cfg.Path()
}

// rollbackOne undoes an unpublished migration of one repo. With quietNone a
// repo that has nothing to roll back is only mentioned (for --all).
func rollbackOne(stdout, stderr io.Writer, cfg config.Config, quietNone bool) int {
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() {
		fmt.Fprintf(stderr, "recipients: %s: not a git repo\n", cfg.SyncDir)
		return 1
	}
	if _, err := agecrypt.ReadRekeyState(repo); errors.Is(err, os.ErrNotExist) && quietNone {
		fmt.Fprintf(stdout, "%s: nothing to roll back\n", cfg.SyncDir)
		return 0
	}
	cfgFile := cfgFileOf(cfg)
	head, _ := repo.Run("rev-parse", "HEAD")
	var st agecrypt.PreRekeyState
	err := withSyncLock(cfg.SyncDir, func() error {
		var rerr error
		st, rerr = agecrypt.RollbackRekey(repo, func(pc agecrypt.PriorConfig) error {
			raw, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			raw.EncryptionScheme, raw.HostLabel, raw.RecoveryRecipients = pc.EncryptionScheme, pc.HostLabel, pc.RecoveryRecipients
			return config.SaveAt(cfgFile, raw)
		})
		if rerr != nil {
			return rerr
		}
		return syncpkg.MoveSyncedHead(repo, strings.TrimSpace(head), st.Tip)
	})
	if err != nil {
		return recipientsFail(stderr, fmt.Errorf("%s: %w", cfg.SyncDir, err))
	}
	fmt.Fprintf(stdout, "%s: rolled back to the classic repo at %s (identity, scheme, pin and config.json restored)\n", cfg.SyncDir, shortRev(st.Tip))
	fmt.Fprintf(stdout, "  the hybrid identity %s is kept; a later rekey-pq reuses it\n", st.NewIdentity)
	return 0
}

func shortRev(r string) string {
	if len(r) > 10 {
		return r[:10]
	}
	return r
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
		return rekeyPlanOne(stdout, stderr, cfg, repo, o)
	}
	// Everything that can refuse the push runs before the first local change.
	if cfg.PrivateOnly {
		if err := validateRemoteOffline(cfg); err != nil {
			return recipientsFail(stderr, fmt.Errorf("%s: %w", cfg.SyncDir, err))
		}
		if err := checkPushURL(cfg, cfg.SyncDir); err != nil {
			return recipientsFail(stderr, fmt.Errorf("%s: %w", cfg.SyncDir, err))
		}
	}
	if !repo.IsRepo() || !agecrypt.Enabled(repo) {
		fmt.Fprintf(stderr, "recipients: %s: the sync repo is not encrypted\n", cfg.SyncDir)
		return 1
	}
	cfgFile := cfgFileOf(cfg)
	raw, err := config.Load(cfgFile)
	if err != nil {
		return recipientsFail(stderr, err)
	}
	o.Prior = agecrypt.PriorConfig{EncryptionScheme: raw.EncryptionScheme, HostLabel: raw.HostLabel, RecoveryRecipients: raw.RecoveryRecipients}

	var (
		plan   agecrypt.RekeyPlan
		stage  agecrypt.RekeyStage
		pushed bool
		pushEr error
		verr   error
		vc, vb int
	)
	err = withSyncLock(cfg.SyncDir, func() error {
		var st agecrypt.PreRekeyState
		var derr error
		if stage, st, derr = agecrypt.DetectRekey(repo, o); derr != nil {
			return derr
		}
		switch stage {
		case agecrypt.StageDone:
			return nil
		case agecrypt.StageFresh:
			p, aerr := agecrypt.ApplyRekeyPQ(repo, o)
			if aerr != nil {
				return aerr
			}
			plan = p
		case agecrypt.StageFinishLocal:
			p, ferr := agecrypt.FinishRekeyLocal(repo, st)
			if ferr != nil {
				return ferr
			}
			plan = p
		default:
			plan = agecrypt.PlanFromState(repo, st)
		}
		if stage != agecrypt.StagePublished {
			if err := syncpkg.MoveSyncedHead(repo, plan.Tip, plan.NewCommit); err != nil {
				return err
			}
			if pushEr = rekeyPush(repo, plan, o.FreshRoot); pushEr != nil {
				return pushEr
			}
		}
		pushed = true
		// The remote now holds the migrated repo: the config follows it,
		// whether or not the verification below passes.
		raw.EncryptionScheme = agecrypt.SchemePQ
		raw.HostLabel = o.Label
		raw.RecoveryRecipients = []string{o.Recovery}
		if err := config.SaveAt(cfgFile, raw); err != nil {
			return fmt.Errorf("pushed, but config.json could not be saved: %w (set encryptionScheme to pq, hostLabel and recoveryRecipients by hand)", err)
		}
		want := 0
		if o.FreshRoot {
			want = 1
		}
		vc, vb, verr = agecrypt.VerifyHybridRemote(repo, defaultBranch, want)
		return nil
	})
	if err != nil {
		if pushEr != nil && plan.NewCommit != "" {
			fmt.Fprintf(stderr, "recipients: %s: migrated locally but the push failed (%v); nothing was published.\n  rerun the same command to retry, or undo with `recipients rekey-pq --repo <name> --rollback`; the pre-migration tip is at %s\n", cfg.SyncDir, pushEr, agecrypt.BackupRef)
			return 1
		}
		return recipientsFail(stderr, fmt.Errorf("%s: %w", cfg.SyncDir, err))
	}
	if stage == agecrypt.StageDone {
		fmt.Fprintf(stdout, "%s: already post-quantum; nothing to do\n", cfg.SyncDir)
		return 0
	}
	if pushed && verr != nil {
		fmt.Fprintf(stderr, "recipients: %s: pushed; verification failed: %v\n  the classic identity is kept. Inspect origin/main before relying on it.\n", cfg.SyncDir, verr)
		return 1
	}
	fmt.Fprintf(stdout, "%s: migrated to post-quantum; public recipient %s\n", cfg.SyncDir, plan.NewRecipient)
	if o.FreshRoot {
		fmt.Fprintf(stdout, "  verified origin/main: %d commit, %d files, all hybrid ciphertext\n", vc, vb)
	} else {
		fmt.Fprintf(stdout, "  re-encrypted and pushed; origin/main verified: %d files, all hybrid ciphertext\n", vb)
	}
	fmt.Fprintf(stdout, "  rollback point kept at %s; delete it later with `recipients rekey-pq --drop-backup`\n", agecrypt.BackupRef)
	retireClassicIdentity(stdout, plan.IdentityPath, cfg, hostCfgs)
	return 0
}

func rekeyPlanOne(stdout, stderr io.Writer, cfg config.Config, repo *gitwt.Repo, o agecrypt.RekeyOpts) int {
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

// rekeyPush publishes the migrated repo: a plain push, or with a fresh root a
// force-with-lease on the old tip. The private-only and push-URL checks have
// already run, before anything changed locally.
func rekeyPush(repo *gitwt.Repo, p agecrypt.RekeyPlan, fresh bool) error {
	if err := agecrypt.PushGuard(repo, defaultBranch); err != nil {
		return err
	}
	if !fresh {
		return repo.Push(defaultBranch)
	}
	_, err := repo.Run("push", "--force-with-lease=main:"+p.Tip, "origin", p.NewCommit+":refs/heads/main")
	return err
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
