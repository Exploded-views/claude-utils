package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/scrub"
)

// denylistRel is the denylist's path relative to the work-tree root. It lives
// beside config.json and is per-PC: it must never be staged, even by users
// whose .gitignore predates the denylist.
const denylistRel = "denylist.txt"

// CommitMirror stages everything in the sync work-tree and commits it, minus
// any file the scrub rules hold back. It reports whether a commit was made.
//
// With cfg.Scrub on, each added or modified file is checked against the
// built-in secret rules and the user's denylist. A file that trips one is
// unstaged (it stays on disk, so the next cycle checks it again) and logged by
// path and rule name only. Deletions are never scanned. If cfg.RequireDenylist
// is set and the denylist is missing, or the denylist cannot be parsed,
// nothing at all is committed. Those cases are logged and return (false, nil)
// so the caller still pulls and pushes what is already committed.
func CommitMirror(repo *gitwt.Repo, cfg config.Config, msg string) (bool, error) {
	if err := repo.AddAll(); err != nil {
		return false, fmt.Errorf("add: %w", err)
	}
	// Per-PC file: never part of a commit, whatever the scrub setting.
	if err := repo.Unstage(denylistRel); err != nil {
		return false, fmt.Errorf("unstage denylist: %w", err)
	}
	if cfg.Scrub {
		ok, err := holdBack(repo, cfg)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return repo.Commit(msg)
}

// holdBack unstages every staged file the scanner flags. It returns false when
// the denylist policy forbids committing anything this cycle (in which case
// the index has been emptied).
func holdBack(repo *gitwt.Repo, cfg config.Config) (bool, error) {
	deny, err := loadDenylist(cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if cfg.RequireDenylist {
			log.Printf("scrub: requireDenylist is set but %s does not exist; committing nothing", cfg.DenylistPath())
			return false, unstageAll(repo)
		}
	case err != nil:
		// A denylist we cannot read is a denylist we cannot honor.
		log.Printf("scrub: cannot use %s (%v); committing nothing until it is fixed", cfg.DenylistPath(), err)
		return false, unstageAll(repo)
	}

	changes, err := repo.StagedChanges()
	if err != nil {
		return false, fmt.Errorf("list staged changes: %w", err)
	}
	scanner := scrub.New(deny)
	var held []string
	for _, c := range changes {
		if c.Status == 'D' {
			continue
		}
		content, err := repo.StagedContent(c.Path)
		if err != nil {
			return false, fmt.Errorf("read staged %s: %w", c.Path, err)
		}
		if rules := scanner.Scan(content); len(rules) > 0 {
			// Path and rule names only: never log the matched text.
			log.Printf("scrub: holding back %s (rule: %s)", c.Path, strings.Join(rules, ", "))
			held = append(held, c.Path)
		}
	}
	if err := repo.Unstage(held...); err != nil {
		return false, fmt.Errorf("unstage held-back files: %w", err)
	}
	return true, nil
}

// loadDenylist reads the user's denylist. A missing file is fs.ErrNotExist.
func loadDenylist(cfg config.Config) ([]scrub.DenylistPattern, error) {
	f, err := os.Open(cfg.DenylistPath())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scrub.ParseDenylist(f)
}

// unstageAll empties the index (the work-tree is untouched).
func unstageAll(repo *gitwt.Repo) error {
	if err := repo.Unstage("."); err != nil {
		return fmt.Errorf("unstage: %w", err)
	}
	return nil
}
