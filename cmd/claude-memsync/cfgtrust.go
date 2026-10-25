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

// githubTokenCmd in config.json is code that runs on every token use, and
// config.json sits in the sync work-tree where a remote could try to plant one.
// So it is honoured only when the file it came from is this user's own, mode
// 0600-like (no group or other bits), and, if it lies in the work-tree, not
// tracked by git (neither at HEAD nor in the index). A command typed as
// --token-cmd is the user's own and needs none of this.

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

func warnTokenCmdIgnored(err error) {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if warned[err.Error()] {
		return
	}
	warned[err.Error()] = true
	warnf("githubTokenCmd ignored (%v); no GitHub token is used from it. Fix with chmod 600 and make sure config.json is not tracked in the sync repo, or pass --token-cmd", err)
}

func tokenCmdConfigTrusted(cfg config.Config) error {
	path := cfg.ConfigPath
	if path == "" {
		path = cfg.Path()
	}
	st, err := os.Lstat(path)
	if err != nil {
		return errors.New("config.json cannot be read")
	}
	if !st.Mode().IsRegular() {
		return errors.New("config.json is not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config.json mode %04o lets others read or write it; it must be 0600", st.Mode().Perm())
	}
	if !ownedByCurrentUser(st) {
		return errors.New("config.json is not owned by the current user")
	}
	// Inside the sync work-tree: the file must not be tracked.
	if rel, err := filepath.Rel(cfg.SyncDir, path); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
		repo := gitwt.New(cfg.SyncDir)
		if repo.IsRepo() {
			rel = filepath.ToSlash(rel)
			if out, err := repo.Run("ls-files", "--error-unmatch", "--", rel); err == nil && strings.TrimSpace(out) != "" {
				return errors.New("config.json is tracked in the sync repo")
			}
			if repo.HasHead() {
				if _, err := repo.Run("cat-file", "-e", "HEAD:"+rel); err == nil {
					return errors.New("config.json is tracked at HEAD in the sync repo")
				}
			}
		}
	}
	return nil
}
