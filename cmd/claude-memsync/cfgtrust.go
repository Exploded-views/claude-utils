package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// config.json sits in the sync work-tree, where a remote could try to plant
// one, so some of its keys are code or credentials the user must really have
// chosen: githubTokenCmd runs a command on every token use, githubTokenFile
// names a file whose content is sent to the API, githubApp picks an app key
// that signs for GitHub, githubApiUrl decides where tokens are sent, and
// mergeDriverPath names a program git will run. They are honoured only when
// the file they came from is this user's own, mode 0600-like (no group or
// other bits), and, if it lies in the work-tree, not tracked by git (neither
// at HEAD nor in the index). Otherwise they are ignored with one warning. A
// value typed on the command line (--token-cmd, --token-file, --github-app) is
// the user's own and needs none of this.

var (
	warnf = log.Printf

	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

func resetTokenCmdWarned() {
	warnedMu.Lock()
	warned = map[string]bool{}
	warnedMu.Unlock()
}

func warnIgnored(keys []string, err error) {
	msg := strings.Join(keys, ", ") + ": " + err.Error()
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if warned[msg] {
		return
	}
	warned[msg] = true
	warnf("%s", fmt.Sprintf("config.json is not trusted (%v); ignoring %s. Fix with chmod 600 and make sure config.json is not tracked in the sync repo, or pass the value as a flag", err, strings.Join(keys, ", ")))
}

// sensitiveKeys is the one table of the keys the trust check guards: what
// configKeysFromFile reports, what sanitizeCfg drops from an untrusted file and
// what stripSensitive always drops. Adding a key here covers all three.
var sensitiveKeys = []struct {
	name  string
	set   func(config.Config) bool
	clear func(*config.Config)
}{
	{"githubTokenCmd", func(c config.Config) bool { return len(c.GitHubTokenCmd) > 0 }, func(c *config.Config) { c.GitHubTokenCmd = nil }},
	{"githubTokenFile", func(c config.Config) bool { return c.GitHubTokenFile != "" }, func(c *config.Config) { c.GitHubTokenFile = "" }},
	{"githubApp", func(c config.Config) bool { return c.GitHubApp != "" }, func(c *config.Config) { c.GitHubApp = "" }},
	{"githubApiUrl", func(c config.Config) bool { return c.GitHubAPIURL != "" }, func(c *config.Config) { c.GitHubAPIURL = "" }},
	{"mergeDriverPath", func(c config.Config) bool { return c.MergeDriverPath != "" }, func(c *config.Config) { c.MergeDriverPath = "" }},
}

// configKeysFromFile lists the sensitive keys cfg holds that did not come from
// a flag of this run.
func configKeysFromFile(cfg config.Config) []string {
	var keys []string
	for _, k := range sensitiveKeys {
		if k.set(cfg) && !cfg.FromFlag(k.name) {
			keys = append(keys, k.name)
		}
	}
	return keys
}

// sanitizeCfg drops the sensitive keys that came from an untrusted config.json
// (warning once). It is idempotent and a no-op for a config that was not
// loaded from a file or holds none of those keys.
func sanitizeCfg(cfg config.Config) config.Config {
	if cfg.ConfigPath == "" {
		return cfg
	}
	keys := configKeysFromFile(cfg)
	if len(keys) == 0 {
		return cfg
	}
	err := configTrusted(cfg)
	if err == nil {
		return cfg
	}
	warnIgnored(keys, err)
	for _, k := range sensitiveKeys {
		if slices.Contains(keys, k.name) {
			k.clear(&cfg)
		}
	}
	return cfg
}

// stripSensitive drops every sensitive key without judging the file: for the
// read-only callers (the changes hook's report path) that never use them. It
// starts no git process and warns of nothing. A caller that needs a key must
// use loadConfig, which keeps it only from a trusted file.
func stripSensitive(c config.Config) config.Config {
	for _, k := range sensitiveKeys {
		k.clear(&c)
	}
	return c
}

// loadConfigHook is config.Load plus stripSensitive: no trust check, so no git.
func loadConfigHook(path string) (config.Config, error) {
	if cfgLoadHook != nil {
		cfgLoadHook(path)
	}
	c, err := config.Load(path)
	if err != nil {
		return c, err
	}
	return stripSensitive(c), nil
}

// cfgLoadHook, if set, is told the path of every config loadConfig or
// loadConfigHook reads; tests count loads with it.
var cfgLoadHook func(path string)

// loadConfig is config.Load plus sanitizeCfg: every read of config.json in
// this program goes through it.
func loadConfig(path string) (config.Config, error) {
	if cfgLoadHook != nil {
		cfgLoadHook(path)
	}
	c, err := config.Load(path)
	if err != nil {
		return c, err
	}
	return sanitizeCfg(c), nil
}

func configTrusted(cfg config.Config) error {
	path := cfg.ConfigPath
	if path == "" {
		path = cfg.Path()
	}
	// Judge the real file: symlinks in the path (a symlinked sync dir, an
	// aliased --config) must not hide which repo it lies in.
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return errors.New("config.json path was not expanded (a literal ~ is not judged)")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errors.New("config.json cannot be read")
	}
	st, err := os.Lstat(real)
	if err != nil {
		return errors.New("config.json cannot be read")
	}
	if !st.Mode().IsRegular() {
		return errors.New("config.json is not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config.json mode %04o lets others read or write it; it must be 0600", st.Mode().Perm())
	}
	if err := ownerAndLinksOK(st); err != nil {
		return err
	}
	// If the file's own directory is in a git work-tree (any, not only the
	// configured sync dir), it must not be tracked there. git is asked from the
	// file's directory, so it applies its own path and case rules.
	dir, _ := filepath.Split(real)
	dir = filepath.Clean(dir)
	repo := gitwt.New(dir)
	if _, err := repo.Run("rev-parse", "--is-inside-work-tree"); err == nil {
		// Every tracked name in the file's directory is compared in folded
		// form, and one that is the file itself (same device and inode) counts:
		// that covers the exact name and any other spelling of it (Config.json
		// on a case-insensitive file system). One listing of the index and one
		// of HEAD, and HEAD is probed once (each git start-up costs a spawn).
		if tracked(repo, dir, real, "ls-files", "-z", "--cached", "--", ":(glob)*") {
			return errors.New("config.json is tracked in a git repo")
		}
		if repo.HasHead() && tracked(repo, dir, real, "ls-tree", "-z", "--name-only", "HEAD") {
			return errors.New("config.json is tracked at HEAD in a git repo")
		}
	}
	return nil
}

// tracked runs a git listing of names relative to dir and reports whether one
// of them, spelled differently or not, is the file real.
func tracked(repo *gitwt.Repo, dir, real string, args ...string) bool {
	// Anything that cannot be established counts as tracked: the answer is
	// "untrusted", never "nothing found".
	out, err := repo.Run(append([]string{"-c", "core.quotePath=false"}, args...)...)
	if err != nil {
		return true
	}
	want := gitwt.FoldPath(filepath.Base(real))
	self, err := os.Stat(real)
	if err != nil {
		return true
	}
	for _, name := range strings.Split(out, "\x00") {
		if name == "" || strings.Contains(name, "/") || gitwt.FoldPath(name) != want {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && os.SameFile(self, st) {
			return true
		}
	}
	return false
}
