package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
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
// so the caller still pulls and pushes what is already committed. While a
// merge or rebase is unresolved, a file with conflict markers is never staged:
// CommitMirror fails instead (see refuseConflictMarkers).
func CommitMirror(repo *gitwt.Repo, cfg config.Config, msg string) (bool, error) {
	encrypted := agecrypt.Enabled(repo)
	if encrypted {
		// Keyless: staging would store nothing useful and the work-tree holds
		// ciphertext. Commit nothing.
		if err := agecrypt.CheckKeyed(repo); err != nil {
			return false, err
		}
		if u, _ := agecrypt.Unreviewed(repo); u {
			return false, agecrypt.ErrRecipientsUnreviewed
		}
	}
	if err := refuseConflictMarkers(repo); err != nil {
		return false, err
	}
	if err := repo.AddAll(); err != nil {
		return false, fmt.Errorf("add: %w", err)
	}
	// Per-PC file: never part of a commit, whatever the scrub setting. Matched
	// case-insensitively so "Denylist.txt" from a copy or a case-folding file
	// system is caught too.
	if err := repo.UnstageFold(denylistRel); err != nil {
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
	if encrypted {
		// Names, host and time must not reach the remote in the clear.
		changes, err := repo.StagedChanges()
		if err != nil {
			return false, fmt.Errorf("list staged changes: %w", err)
		}
		msg = fmt.Sprintf("memsync: %d files", len(changes))
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

	// Deletions are not scanned, and neither are submodule entries (no blob).
	var held, toRead []string
	var readIdx []int
	byPathHits := 0
	for i, c := range changes {
		if c.Status == 'D' || c.Mode == "160000" {
			continue
		}
		if scanner.MatchesPath(c.Path) {
			held = append(held, c.Path)
			byPathHits++
			continue
		}
		toRead = append(toRead, c.Oid)
		readIdx = append(readIdx, i)
	}
	contents, err := repo.BlobContents(toRead)
	if err != nil {
		return false, fmt.Errorf("read staged files: %w", err)
	}
	// An encrypted repo stages ciphertext, which no rule can match: scan the
	// plaintext. A blob this host cannot decrypt cannot be checked, so it is
	// held back.
	plain := agecrypt.PlainReader(repo)
	for j, content := range contents {
		path := changes[readIdx[j]].Path
		pt, perr := plain(content)
		if perr != nil {
			log.Printf("scrub: holding back %s (cannot decrypt to check it)", path)
			held = append(held, path)
			continue
		}
		content = pt
		if rules := scanner.Scan(content); len(rules) > 0 {
			// Path and rule names only: never log the matched text.
			log.Printf("scrub: holding back %s (rule: %s)", path, strings.Join(rules, ", "))
			held = append(held, path)
		}
	}
	if byPathHits > 0 {
		// The path itself matches the denylist, so it is not printed.
		log.Printf("scrub: <held back: path matches denylist> (%d)", byPathHits)
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
	if err := repo.UnstageAll(); err != nil {
		return fmt.Errorf("unstage: %w", err)
	}
	return nil
}
