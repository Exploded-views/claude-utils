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
