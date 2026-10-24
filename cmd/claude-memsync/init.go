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

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
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
	force := flags.Bool("force", false, "re-init even if sync-dir already exists")
	denylist := flags.String("denylist", "", "UTF-8 file of regexes (one per line) to install as the denylist before the first commit")
	encrypt := flags.Bool("encrypt", false, "encrypt every file in the repo with age (new or already-encrypted remotes only)")
	identity := flags.String("identity", "", "with --encrypt: age identity file (default: per-host key under the user config dir; generated if missing)")
	var extraRecipients stringList
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

	cfg := config.Defaults()
	if *syncDir != "" {
		cfg.SyncDir = *syncDir
	}
	if *claudeDir != "" {
		cfg.ClaudeProjectsDir = *claudeDir
	}
	cfg.RemoteURL = *remote

	driver, err := config.DiscoverMergeDriver()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init: cannot locate claude-memmerge:", err)
		return 1
	}
	cfg.MergeDriverPath = driver

	var enc *encryptSpec
	if *encrypt {
		enc = &encryptSpec{IdentityPath: *identity, Recipients: extraRecipients}
		if enc.IdentityPath == "" {
			enc.IdentityPath = defaultIdentityPath()
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "init: cannot locate claude-memsync:", err)
			return 1
		}
		enc.MemsyncExe = exe
	} else if *identity != "" || len(extraRecipients) > 0 {
		fmt.Fprintln(os.Stderr, "init: --identity and --recipient need --encrypt")
		return 2
	}

	if err := bootstrapWith(cfg, *force, denylistData, enc); err != nil {
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
	return bootstrapWith(cfg, force, denylist, nil)
}

// bootstrapWith is bootstrap with optional age encryption. With enc set, the
// clone is made with -n and the filter configured before the first checkout,
// so no plaintext is ever committed or fetched without it.
func bootstrapWith(cfg config.Config, force bool, denylist []byte, enc *encryptSpec) error {
	// Step 1: ensure SyncDir exists / is empty.
	info, err := os.Stat(cfg.SyncDir)
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
	if enc != nil {
		if cloneErr = repo.CloneNoCheckout(cfg.RemoteURL); cloneErr == nil {
			cloned = true
		}
	} else {
		cloned, cloneErr = tryClone(repo, cfg.RemoteURL)
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

	// Step 3: configure merge driver and gitattributes.
	keyless := false
	if enc != nil {
		if err := setupEncryption(repo, cfg, enc, ownRecipient); err != nil {
			return err
		}
		if err := agecrypt.CheckKeyed(repo); err != nil {
			keyless = true
			fmt.Fprintln(os.Stderr, "this host cannot read the repo yet:", err)
			fmt.Fprintln(os.Stderr, "send this public key to a host that can, to run `claude-memsync recipients add` with it, then pull:")
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
	driverCmd := fmt.Sprintf("%s %%O %%A %%B %%L %%P", quoteIfSpaces(filepath.ToSlash(cfg.MergeDriverPath)))
	if err := repo.ConfigSet(fmt.Sprintf("merge.%s.driver", mergeDriverName), driverCmd); err != nil {
		return err
	}
	if enc == nil {
		if err := writeGitattributes(cfg.SyncDir); err != nil {
			return fmt.Errorf("write .gitattributes: %w", err)
		}
	}
	if err := writeGitignore(cfg.SyncDir); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}

	// Step 4: write config.json.
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
	roots := syncpkg.Roots{Claude: cfg.ClaudeProjectsDir, Mirror: mirrorProjects}
	// First-time init: no manifest yet. Pass nil so Reconcile doesn't try
	// to propagate deletes (with no prior state, "missing in Claude" can't
	// be distinguished from "this PC never had it").
	report, err := syncpkg.Reconcile(roots, nil)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	printReport(report)

	// Step 6: stage + commit + push.
	committed, err := syncpkg.CommitMirror(repo, cfg, "init: bootstrap from "+hostname())
	if err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	if committed {
		fmt.Fprintln(os.Stderr, "committed bootstrap snapshot")
	} else {
		fmt.Fprintln(os.Stderr, "no local changes to commit")
	}
	if cloned || committed {
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
	if m, err := syncpkg.BuildFromClaudeTree(cfg.ClaudeProjectsDir); err == nil {
		if err := m.Save(cfg.SyncDir); err != nil {
			fmt.Fprintln(os.Stderr, "save manifest:", err)
		}
	}

	fmt.Fprintln(os.Stderr, "init OK")
	fmt.Fprintln(os.Stderr, "  sync dir:        ", cfg.SyncDir)
	fmt.Fprintln(os.Stderr, "  claude projects: ", cfg.ClaudeProjectsDir)
	fmt.Fprintln(os.Stderr, "  distilled:       ", cfg.DistilledPath())
	fmt.Fprintln(os.Stderr, "  remote:          ", cfg.RemoteURL)
	fmt.Fprintln(os.Stderr, "  merge driver:    ", cfg.MergeDriverPath)
	printDistillPermissionHint(cfg)
	return nil
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
	if err := agecrypt.Configure(repo, agecrypt.Opts{
		MemsyncExe:   enc.MemsyncExe,
		MemmergeExe:  cfg.MergeDriverPath,
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
		keys = append([]string{ownRecipient}, keys...)
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

// tryClone attempts `git clone <url> <dir>`. Returns (true, nil) on success,
// (false, err) on failure. An empty-repo error is one common failure.
func tryClone(repo *gitwt.Repo, url string) (bool, error) {
	if err := repo.Clone(url); err != nil {
		return false, err
	}
	return true, nil
}

func writeGitattributes(syncDir string) error {
	path := filepath.Join(syncDir, gitattributesRel)
	content := fmt.Sprintf("# Managed by claude-memsync — semantic merge for memory index files.\nMEMORY.md merge=%s\n", mergeDriverName)
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
