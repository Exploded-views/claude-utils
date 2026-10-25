package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// configKeysFromFile lists the sensitive keys cfg holds that did not come from
// a flag of this run.
func configKeysFromFile(cfg config.Config) []string {
	var keys []string
	add := func(set bool, key string) {
		if set && !cfg.FromFlag(key) {
			keys = append(keys, key)
		}
	}
	add(len(cfg.GitHubTokenCmd) > 0, "githubTokenCmd")
	add(cfg.GitHubTokenFile != "", "githubTokenFile")
	add(cfg.GitHubApp != "", "githubApp")
	add(cfg.GitHubAPIURL != "", "githubApiUrl")
	add(cfg.MergeDriverPath != "", "mergeDriverPath")
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
	for _, k := range keys {
		switch k {
		case "githubTokenCmd":
			cfg.GitHubTokenCmd = nil
		case "githubTokenFile":
			cfg.GitHubTokenFile = ""
		case "githubApp":
			cfg.GitHubApp = ""
		case "githubApiUrl":
			cfg.GitHubAPIURL = ""
		case "mergeDriverPath":
			cfg.MergeDriverPath = ""
		}
	}
	return cfg
}

// loadConfig is config.Load plus sanitizeCfg: every read of config.json in
// this program goes through it.
func loadConfig(path string) (config.Config, error) {
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
	dir, base := filepath.Split(real)
	repo := gitwt.New(filepath.Clean(dir))
	if _, err := repo.Run("rev-parse", "--is-inside-work-tree"); err == nil {
		if out, err := repo.Run("ls-files", "--error-unmatch", "--", base); err == nil && strings.TrimSpace(out) != "" {
			return errors.New("config.json is tracked in a git repo")
		}
		if repo.HasHead() {
			if out, err := repo.Run("ls-tree", "--name-only", "HEAD", "--", base); err == nil && strings.TrimSpace(out) != "" {
				return errors.New("config.json is tracked at HEAD in a git repo")
			}
		}
	}
	return nil
}
