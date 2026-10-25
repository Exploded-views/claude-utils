package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/route"
	"github.com/MarimerLLC/claude-utils/internal/scrub"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

const (
	defaultBranch    = "main"
	mergeDriverName  = "claude-memory-index"
	gitattributesRel = ".gitattributes"
)

// encryptSpec turns on age encryption at rest for a new sync repo.
type encryptSpec struct {
	IdentityPath string   // this host's identity file (generated if missing)
	Recipients   []string // extra public keys, e.g. an offline recovery key
	MemsyncExe   string   // path of claude-memsync, which runs the filter
	MemmergeExe  string   // path of claude-memmerge for the merge drivers; empty: cfg.MergeDriverPath
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// defaultIdentityPath is where init keeps this host's age identity: outside
// the sync repo, which must never hold it.
func defaultIdentityPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "claude-memsync", "identity.txt")
}

// runInit handles `claude-memsync init [flags]`.
func runInit(args []string) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	remote := flags.String("remote", "", "git remote URL to sync against (e.g. git@github.com:user/claude-memories.git)")
	syncDir := flags.String("sync-dir", "", "override sync work-tree (default: ~/.claudesync)")
	claudeDir := flags.String("claude-dir", "", "override Claude projects directory (default: ~/.claude/projects)")
	privateOnly := flags.Bool("private-only", false, "enforce private mem-* repos only (name allowlist + GitHub visibility checks); saved to config")
	apiURL := flags.String("github-api-url", "", "GitHub API base URL (GitHub Enterprise); saved to config")
	ghHost := flags.String("github-host", "", "git host remotes must use (default github.com); saved to config")
	tokenFile := flags.String("token-file", "", "file holding a GitHub token for the private-only check (or set MEMSYNC_GITHUB_TOKEN)")
	tokenCmd := flags.String("token-cmd", "", "command (shell-style quotes, no shell) printing a GitHub token, e.g. \"gh auth token\"; saved as githubTokenCmd (the command, never the token)")
	ghApp := flags.String("github-app", "", "apps.json entry to mint the GitHub token from (see app-token); saved to config")
	hostLabel := flags.String("host-label", "", "a name you choose for this host (such as laptop-2): titles its deploy keys, labels its age recipient, names it in the registry; saved to config")
	readOnly := flags.Bool("read-only", false, "read-only member: pull and apply notes into the Claude tree, never commit or push (the remote must already hold the repo); saved to config")
	expires := flags.String("expires", "", "last day this host syncs (YYYY-MM-DD); the day after, the daemon holds and status says to retire it; saved to config and published to the registry")
	force := flags.Bool("force", false, "re-init even if sync-dir already exists")
	archiveDeletes := flags.Bool("archive-deletes", false, "archive instead of delete: a note removed from a Claude folder moves to archive/ in the repo (sets archiveDeletes in config.json)")
	denylist := flags.String("denylist", "", "UTF-8 file of regexes (one per line) to install as the denylist before the first commit")
	encrypt := flags.Bool("encrypt", false, "encrypt every file in the repo with age (new or already-encrypted remotes only)")
	identity := flags.String("identity", "", "with --encrypt: age identity file (default: per-host key under the user config dir; generated if missing)")
	var extraRecipients stringList
	var gopts guardOpts
	flags.BoolVar(&gopts.IgnoreForeign, "ignore-foreign-writers", false, "go on although a foreignWriters entry is active or cannot be checked (logged)")
	flags.BoolVar(&gopts.SkipQuiet, "skip-quiet-window", false, "skip the start-up quiet window")
	var recoveryRecipients stringList
	flags.Var(&recoveryRecipients, "recovery-recipient", "with --encrypt: the owner's offline recovery key (age/ssh public key, repeatable): a recipient like --recipient, marked \"# recovery\", saved as recoveryRecipients, and the only kind of recipient `join accept` lets go unlabelled")
	flags.Var(&extraRecipients, "recipient", "with --encrypt: extra age/ssh public key, e.g. an offline recovery key (repeatable)")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *remote == "" {
		fmt.Fprintln(os.Stderr, "init: --remote is required")
		flags.Usage()
		return 2
	}

	// Read and validate up front: a bad denylist must stop init before it
	// touches the sync dir or the remote.
	var denylistData []byte
	if *denylist != "" {
		var err error
		if denylistData, err = readDenylistFile(*denylist); err != nil {
			fmt.Fprintln(os.Stderr, "init:", err)
			return 1
		}
	}

	if *hostLabel != "" {
		if err := route.ValidLabel(*hostLabel); err != nil {
			fmt.Fprintln(os.Stderr, "init: --host-label:", err)
			return 2
		}
	}
	if *expires != "" {
		d, err := time.Parse("2006-01-02", *expires)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "init: --expires must be a date like 2026-12-31")
			return 2
		case time.Now().UTC().Truncate(24 * time.Hour).After(d):
			fmt.Fprintln(os.Stderr, "init: --expires is in the past")
			return 2
		}
	}
	cfg := config.Defaults()
	cfg.ExpiresAt = *expires
	cfg.HostLabel = *hostLabel
	cfg.ReadOnly = *readOnly
	if *syncDir != "" {
		cfg.SyncDir = *syncDir
	}
	if *claudeDir != "" {
		cfg.ClaudeProjectsDir = *claudeDir
	}
	cfg.RemoteURL = *remote
	cfg.GitHubTokenFile = *tokenFile
	cfg.GitHubApp = *ghApp
	applyTokenCmdFlag(&cfg, *tokenCmd)
	cfg.PrivateOnly = *privateOnly
	cfg.GitHubAPIURL = *apiURL
	cfg.GitHubHost = *ghHost

	// Private-only guarantee: prove the remote is a private mem-* repo
	// before anything is cloned, committed or pushed. Fails closed.
	if err := guardRemote(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "init refused:", err)
		return 1
	}
	cfg.ArchiveDeletes = *archiveDeletes

	driver, err := config.DiscoverMergeDriver()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init: cannot locate claude-memmerge:", err)
		return 1
	}
	cfg.MergeDriverPath = driver

	owns, err := initOwner(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		return 1
	}
	var enc *encryptSpec
	if *encrypt {
		recs := append([]string{}, extraRecipients...)
		for _, r := range recoveryRecipients {
			recs = append(recs, strings.TrimSpace(r)+" # recovery")
		}
		enc = &encryptSpec{IdentityPath: *identity, Recipients: recs}
		cfg.RecoveryRecipients = append([]string{}, recoveryRecipients...)
		if enc.IdentityPath == "" {
			enc.IdentityPath = defaultIdentityPath()
		}
		exe, err := executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "init: cannot locate claude-memsync:", err)
			return 1
		}
		enc.MemsyncExe = exe
		if b, ok := runningBinaries(cfg); ok {
			// The stable link if there is one, else the resolved build: never a
			// path git would keep running after the next upgrade.
			enc.MemsyncExe = b.ms
			enc.MemmergeExe = b.mm
		}
	} else if *identity != "" || len(extraRecipients) > 0 || len(recoveryRecipients) > 0 {
		fmt.Fprintln(os.Stderr, "init: --identity, --recipient and --recovery-recipient need --encrypt")
		return 2
	}

	// Before the clone, the reconcile and the push: nothing may be mirrored
	// while another tool is still writing into the Claude folders.
	if err := initGuard(cfg, owns, gopts); err != nil {
		fmt.Fprintln(os.Stderr, "init refused:", err)
		return 1
	}
	if err := bootstrapFull(cfg, *force, denylistData, enc, owns); err != nil {
		fmt.Fprintln(os.Stderr, "init failed:", err)
		return 1
	}
	return 0
}

// readDenylistFile reads a user-supplied denylist and checks that it parses,
// so init never installs one the daemon would refuse to use.
func readDenylistFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--denylist: %w", err)
	}
	if _, err := scrub.ParseDenylist(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("--denylist %s: %w", path, err)
	}
	return data, nil
}

// bootstrap sets up the sync repo. A non-nil denylist is written to
// cfg.DenylistPath() before the first CommitMirror, so the bootstrap commit is
// already scrubbed against it.
func bootstrap(cfg config.Config, force bool, denylist []byte) error {
	return bootstrapFull(cfg, force, denylist, nil, nil)
}

// initOwner is the Owns gate for init: with a routes file, the folders routed
// to the repo whose sync dir this is. nil without one. A routes file that does
// not list this sync dir is a refusal: init would otherwise copy every folder
// into a repo no rule names.
func initOwner(cfg config.Config) (func(string) bool, error) {
	path, ok, err := resolveRoutesPath("")
	if err != nil || !ok {
		return nil, err
	}
	set, err := loadRouted(path)
	if err != nil {
		return nil, err
	}
	name := set.repoForSyncDir(cfg.SyncDir)
	if name == "" {
		return nil, fmt.Errorf("routes file %s does not list the sync dir %s; add it under repos, or point %s elsewhere", path, cfg.SyncDir, routesEnv)
	}
	if _, have := set.Cfgs[name]; !have {
		set.Cfgs[name] = cfg // not initialised yet, but this host will have it
	}
	return set.Router.Owns(name), nil
}

// bootstrapOwned is bootstrap with the folders it may touch limited by owns
// (nil: all of them).
func bootstrapOwned(cfg config.Config, force bool, denylist []byte, owns func(string) bool) error {
	return bootstrapFull(cfg, force, denylist, nil, owns)
}

// bootstrapWith is bootstrap with optional age encryption. With enc set, the
// clone is made with -n and the filter configured before the first checkout,
// so no plaintext is ever committed or fetched without it.
func bootstrapWith(cfg config.Config, force bool, denylist []byte, enc *encryptSpec) error {
	return bootstrapFull(cfg, force, denylist, enc, nil)
}

// bootstrapFull is bootstrap with both: optional encryption and the folders it
// may touch limited by owns.
func bootstrapFull(cfg config.Config, force bool, denylist []byte, enc *encryptSpec, owns func(string) bool) error {
	// Step 1: ensure SyncDir exists / is empty.
	info, err := os.Stat(cfg.SyncDir)
	existed := err == nil
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// fine, will be created by clone
	case err != nil:
		return fmt.Errorf("stat %s: %w", cfg.SyncDir, err)
	default:
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", cfg.SyncDir)
		}
		if !force {
			entries, _ := os.ReadDir(cfg.SyncDir)
			if len(entries) > 0 {
				return fmt.Errorf("%s already exists and is non-empty (pass --force to re-use)", cfg.SyncDir)
			}
		}
	}

	repo := gitwt.New(cfg.SyncDir)

	var ownRecipient string
	if enc != nil {
		var err error
		if ownRecipient, err = prepareIdentity(enc); err != nil {
			return err
		}
	}

	// Step 2: clone, falling back to init+remote-add for empty repos.
	if err := os.MkdirAll(filepath.Dir(cfg.SyncDir), 0700); err != nil {
		return err
	}
	var cloned bool
	var cloneErr error
	// Always clone without a checkout: what the remote tree holds is looked at
	// before any of it reaches the work-tree.
	if cloneErr = repo.CloneNoCheckout(cfg.RemoteURL); cloneErr == nil {
		cloned = true
		if repo.HasHead() {
			bad, err := repo.TrackedLocalOnly("HEAD")
			if err == nil && len(bad) > 0 {
				if !existed {
					_ = os.RemoveAll(cfg.SyncDir)
				}
				return fmt.Errorf("the remote tracks per-PC files (%s) that must never be synced; nothing was checked out. Remove them from the remote first", strings.Join(bad, ", "))
			}
			if err != nil {
				return err
			}
			if enc == nil {
				if _, err := repo.Run("reset", "--hard", "-q", "HEAD"); err != nil {
					return fmt.Errorf("check out: %w", err)
				}
			}
		}
	}
	if !cloned {
		// Clone failed (likely empty repo) — init local and add remote.
		fmt.Fprintln(os.Stderr, "clone returned empty/failed, initializing locally:", cloneErr)
		if err := repo.Init(defaultBranch); err != nil {
			return fmt.Errorf("init local repo: %w", err)
		}
		if err := repo.SetRemote(cfg.RemoteURL); err != nil {
			return fmt.Errorf("add remote: %w", err)
		}
	}

	if cfg.ReadOnly && !repo.HasHead() {
		return fmt.Errorf("the remote holds no commits and this host is read-only: a read-only host cannot create the repo's first commit; init it from a writable host first")
	}

	// Private-only: git must push where we verified, not where an
	// insteadOf rule sends it. Nothing has been committed or pushed yet.
	if cfg.PrivateOnly {
		if err := checkPushURL(cfg, cfg.SyncDir); err != nil {
			return err
		}
	}

	// Step 3: configure merge driver and gitattributes.
	keyless := false
	if enc != nil {
		if err := setupEncryption(repo, cfg, enc, ownRecipient); err != nil {
			return err
		}
		if err := agecrypt.CheckKeyed(repo); err != nil {
			keyless = true
			fmt.Fprintln(os.Stderr, "this host cannot read the repo yet:", err)
			add := "`claude-memsync recipients add`"
			if cfg.HostLabel != "" {
				add = "`claude-memsync recipients add --host " + cfg.HostLabel + "`"
			}
			fmt.Fprintln(os.Stderr, "send this public key to a host that can, to run "+add+" with it, then pull:")
			fmt.Fprintln(os.Stderr, "  ", ownRecipient)
		}
	}
	if err := repo.ConfigSet(fmt.Sprintf("merge.%s.name", mergeDriverName), "claude memory index merge"); err != nil {
		return err
	}
	// Use forward slashes in the driver path: git invokes the driver command
	// through its bundled sh, which treats backslashes as escapes — a Windows
	// path like S:\src\...\claude-memmerge.exe would be mangled to
	// S:srcclaude-memmerge.exe ("command not found"), silently disabling the
	// MEMORY.md union merge. Forward slashes work for Windows exe invocation and
	// survive sh unquoted or quoted.
	driverCmd := agecrypt.MergeCommand(cfg.MergeDriverPath)
	if err := repo.ConfigSet(fmt.Sprintf("merge.%s.driver", mergeDriverName), driverCmd); err != nil {
		return err
	}
	if enc == nil {
		if err := writeGitattributes(cfg.SyncDir, cfg.ArchiveDeletes); err != nil {
			return fmt.Errorf("write .gitattributes: %w", err)
		}
	}
	if err := writeGitignore(cfg.SyncDir); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}

	// Step 4: write config.json. A re-init keeps the guard settings of the
	// config it replaces: the list is not an init flag, and dropping it would
	// switch the foreign-writer guard off.
	// Only a config this host already had: one that came with a clone is never
	// read (the clone is refused above if the remote tracks one).
	if existed {
		cfg = mergeReinit(cfg, cfg.Path())
	}
	if old, err := config.Load(cfg.Path()); existed && err == nil {
		if len(cfg.ForeignWriters) == 0 {
			cfg.ForeignWriters = old.ForeignWriters
		}
		if cfg.StartQuietSec == nil {
			cfg.StartQuietSec = old.StartQuietSec
		}

	}
	// init is the migration moment: the next start gets the quiet window again.
	_ = os.Remove(quietMarker(cfg.SyncDir))
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	if denylist != nil {
		if err := os.WriteFile(cfg.DenylistPath(), denylist, 0600); err != nil {
			return fmt.Errorf("install denylist: %w", err)
		}
	}
	if keyless {
		// The mirror holds ciphertext: reconciling it into the Claude tree or
		// committing would do harm. Stop here; `status` reports the state.
		fmt.Fprintln(os.Stderr, "init OK (keyless: nothing is synced until this host's key is added)")
		return nil
	}

	// Step 5: bidirectional reconcile of Claude tree ↔ mirror.
	mirrorProjects := filepath.Join(cfg.SyncDir, "projects")
	if err := os.MkdirAll(mirrorProjects, 0700); err != nil {
		return err
	}
	roots := syncpkg.Roots{Claude: cfg.ClaudeProjectsDir, Mirror: mirrorProjects, Owns: owns}
	// First-time init: no manifest yet. Pass nil so Reconcile doesn't try
	// to propagate deletes (with no prior state, "missing in Claude" can't
	// be distinguished from "this PC never had it").
	if cfg.ReadOnly {
		n, err := syncpkg.PopulateClaude(roots)
		if err != nil {
			return fmt.Errorf("copy notes into the Claude tree: %w", err)
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr, "copied %d files mirror → Claude\n", n)
		}
	} else {
		report, err := syncpkg.Reconcile(roots, nil)
		if err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		printReport(report)
	}

	// Step 6: stage + commit + push.
	committed := false
	if cfg.ReadOnly {
		fmt.Fprintln(os.Stderr, "read-only: nothing committed or pushed")
	} else {
		var err error
		committed, err = syncpkg.CommitMirror(repo, cfg, "init: bootstrap from "+hostname())
		if err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
		if committed {
			fmt.Fprintln(os.Stderr, "committed bootstrap snapshot")
		} else {
			fmt.Fprintln(os.Stderr, "no local changes to commit")
		}
	}
	if !cfg.ReadOnly && (cloned || committed) {
		if enc != nil {
			if err := agecrypt.PushGuard(repo, defaultBranch); err != nil {
				return err
			}
		}
		if err := repo.Push(defaultBranch); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
		fmt.Fprintln(os.Stderr, "pushed to", cfg.RemoteURL)
	}

	// Capture the post-init Claude state so the first Reconcile after the
	// daemon starts can correctly detect deletions.
	if m, err := syncpkg.BuildFromClaudeTreeOwned(cfg.ClaudeProjectsDir, owns); err == nil {
		if err := m.Save(cfg.SyncDir); err != nil {
			fmt.Fprintln(os.Stderr, "save manifest:", err)
		}
	}

	fmt.Fprintln(os.Stderr, "init OK")
	fmt.Fprintln(os.Stderr, "  sync dir:        ", cfg.SyncDir)
	fmt.Fprintln(os.Stderr, "  claude projects: ", cfg.ClaudeProjectsDir)
	personal := isPersonalRepo(cfg) // only that repo has a catalog
	if personal {
		fmt.Fprintln(os.Stderr, "  distilled:       ", cfg.DistilledPath())
	}
	fmt.Fprintln(os.Stderr, "  remote:          ", cfg.RemoteURL)
	fmt.Fprintln(os.Stderr, "  merge driver:    ", cfg.MergeDriverPath)
	if personal {
		printDistillPermissionHint(cfg)
	}
	return nil
}

// mergeReinit keeps, across a re-init, the settings init has no flag to clear:
// a read-only host stays read-only (a re-init must never make it writable),
// and the host label and check-key threshold survive.
func mergeReinit(cfg config.Config, oldPath string) config.Config {
	old, err := config.Load(oldPath)
	if err != nil {
		return cfg
	}
	if old.ReadOnly && !cfg.ReadOnly {
		cfg.ReadOnly = true
		fmt.Fprintln(os.Stderr, "init: this host was read-only and stays so; edit readOnly in config.json to change that")
	}
	if cfg.HostLabel == "" {
		cfg.HostLabel = old.HostLabel
	}
	if len(cfg.RecoveryRecipients) == 0 {
		cfg.RecoveryRecipients = old.RecoveryRecipients
	}
	if cfg.CheckKeyWarn == 0 {
		cfg.CheckKeyWarn = old.CheckKeyWarn
	}
	return cfg
}

// prepareIdentity makes sure the identity file exists (generating an X25519 key
// at mode 0600 if not) and returns its public recipient. Only the public key is
// ever printed.
func prepareIdentity(enc *encryptSpec) (string, error) {
	if _, err := os.Stat(enc.IdentityPath); err == nil {
		rcpt, err := agecrypt.IdentityRecipient(enc.IdentityPath)
		if err != nil {
			if len(enc.Recipients) == 0 {
				return "", fmt.Errorf("identity %s: %w", enc.IdentityPath, err)
			}
			if _, lerr := agecrypt.LoadIdentities(enc.IdentityPath); lerr != nil {
				return "", lerr
			}
			return "", nil
		}
		return rcpt, nil
	}
	rcpt, err := agecrypt.GenerateIdentity(enc.IdentityPath)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(os.Stderr, "generated a new age identity (mode 0600):", enc.IdentityPath)
	fmt.Fprintln(os.Stderr, "back it up somewhere safe and offline; without it (or the recovery key) the data is unreadable")
	fmt.Fprintln(os.Stderr, "public recipient:", rcpt)
	return rcpt, nil
}

// setupEncryption runs after the clone (or git init) and before the first
// checkout: it sets the filter config, refuses a remote that is not encrypted,
// writes the recipients and attributes of a brand-new repo, and checks the
// existing one out through the filter.
func setupEncryption(repo *gitwt.Repo, cfg config.Config, enc *encryptSpec, ownRecipient string) error {
	mm := cfg.MergeDriverPath
	if enc.MemmergeExe != "" {
		mm = enc.MemmergeExe
	}
	if err := agecrypt.Configure(repo, agecrypt.Opts{
		MemsyncExe:   enc.MemsyncExe,
		MemmergeExe:  mm,
		IdentityPath: enc.IdentityPath,
	}); err != nil {
		return fmt.Errorf("configure encryption: %w", err)
	}
	if repo.HasHead() {
		if _, err := repo.Run("cat-file", "-e", "HEAD:"+agecrypt.RecipientsFile); err != nil {
			return fmt.Errorf("the remote already holds an unencrypted repo; --encrypt only supports a new or an already-encrypted remote")
		}
		// Pin before the checkout: the recipients of the remote as first seen.
		if _, err := repo.Run("checkout", "-q", "HEAD", "--", agecrypt.RecipientsFile); err != nil {
			return err
		}
		if err := agecrypt.PinCurrent(repo); err != nil {
			return err
		}
		if _, err := repo.Run("reset", "--hard", "-q", "HEAD"); err != nil {
			return fmt.Errorf("check out through the filter: %w", err)
		}
		return nil
	}
	keys := append([]string{}, enc.Recipients...)
	if ownRecipient != "" {
		line := ownRecipient
		if cfg.HostLabel != "" {
			line += " # host:" + cfg.HostLabel // lets `host retire` find this line by name
		}
		keys = append([]string{line}, keys...)
	}
	if err := agecrypt.WriteRecipients(cfg.SyncDir, keys); err != nil {
		return fmt.Errorf("write %s: %w", agecrypt.RecipientsFile, err)
	}
	if err := agecrypt.WriteAttributes(cfg.SyncDir); err != nil {
		return fmt.Errorf("write .gitattributes: %w", err)
	}
	if err := agecrypt.PinCurrent(repo); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "encryption on: add an offline recovery recipient with `claude-memsync recipients add <public-key>` if you did not pass --recipient")
	return nil
}

// printDistillPermissionHint suggests the one-time Claude Code permission rule
// that lets the /distill and /distill-apply skills read and write the shared
// catalog without prompting under default (non-bypass) permissions. The write
// side must be spelled Edit(path), not Write(path): Claude Code matches file
// permission checks against Edit rules only (they cover every file-editing
// tool), and warns at startup about any Write(path) allow rule. We only
// advise — editing the user's global settings.json is left to them (or the
// update-config skill), since it is theirs to own.
func printDistillPermissionHint(cfg config.Config) {
	dir := filepath.ToSlash(cfg.DistilledPath())
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To let the /distill skills use the catalog without permission prompts,")
	fmt.Fprintln(os.Stderr, "add this to ~/.claude/settings.json (permissions.allow):")
	fmt.Fprintf(os.Stderr, "  \"Read(%s/**)\",\n", dir)
	fmt.Fprintf(os.Stderr, "  \"Edit(%s/**)\"\n", dir)
}

// writeGitattributes makes sure .gitattributes has the lines the daemon needs,
// keeping whatever the clone already holds (a repo cloned from a peer has the
// union rule for the archive index already, and a user may have added lines).
// The index rule is added when archiveDeletes is on: the index is append-only
// and two hosts appending in the same cycle must merge, not conflict.
func writeGitattributes(syncDir string, archiveDeletes bool) error {
	path := filepath.Join(syncDir, gitattributesRel)
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	want := []string{fmt.Sprintf("MEMORY.md merge=%s", mergeDriverName)}
	if archiveDeletes {
		want = append(want, "archive/INDEX.md merge=union")
	}
	content := string(existing)
	if content == "" {
		content = "# Managed by claude-memsync — semantic merge for memory index files.\n"
	} else if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	have := map[string]bool{}
	for _, l := range strings.Split(content, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	for _, w := range want {
		if !have[w] {
			content += w + "\n"
		}
	}
	return os.WriteFile(path, []byte(content), 0600)
}

// writeGitignore excludes per-PC daemon state from the sync repo. config.json
// embeds machine-specific paths (e.g. MergeDriverPath), so it must NOT be
// shared across workstations; each PC writes its own at init time. Likewise,
// daemon.pid is runtime state and *.from-remote-* are local conflict backups
// surfaced for the user — none of these should propagate.
func writeGitignore(syncDir string) error {
	path := filepath.Join(syncDir, ".gitignore")
	content := `# Managed by claude-memsync — local-only daemon state, must not sync.
config.json
daemon.pid
.state/
denylist.txt
*.tmp
*.tmp.*
*.from-remote-*
# Distilled catalog index is a derived artifact; each PC regenerates it locally
# from the synced entry files (avoids merge conflicts on the generated table).
distilled/DISTILLED.md
`
	return os.WriteFile(path, []byte(content), 0600)
}

func quoteIfSpaces(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func printReport(r syncpkg.SyncReport) {
	if len(r.CopiedToMirror) > 0 {
		fmt.Fprintf(os.Stderr, "copied %d files Claude → mirror\n", len(r.CopiedToMirror))
	}
	if len(r.CopiedToClaude) > 0 {
		fmt.Fprintf(os.Stderr, "copied %d files mirror → Claude\n", len(r.CopiedToClaude))
	}
	if len(r.Merged) > 0 {
		fmt.Fprintf(os.Stderr, "semantically merged %d MEMORY.md files\n", len(r.Merged))
	}
	if len(r.BackedUp) > 0 {
		fmt.Fprintf(os.Stderr, "%d collisions saved as .from-remote-* in the mirror\n", len(r.BackedUp))
	}
}
