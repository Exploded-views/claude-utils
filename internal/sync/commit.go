package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
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
	if cfg.ReadOnly {
		return false, ErrReadOnly
	}
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
	// .gitignore is synced, so a remote can edit the per-PC lines out of it;
	// .git/info/exclude is never transferred.
	if err := repo.EnsureExclude(gitwt.LocalOnlyExcludes...); err != nil {
		return false, fmt.Errorf("exclude per-PC files: %w", err)
	}
	if err := repo.AddAll(); err != nil {
		return false, fmt.Errorf("add: %w", err)
	}
	// The denylist is never part of a commit, whatever the scrub setting. It is
	// matched case-insensitively (a copy or a case-folding file system can spell
	// "Denylist.txt"). Any other per-PC path or link that is staged is taken
	// out, or refused, at the commit: a deletion of one an old version tracked
	// is not, so it can still be cleaned out of the repo.
	if err := repo.UnstageFold(denylistRel); err != nil {
		return false, fmt.Errorf("unstage denylist: %w", err)
	}
	// With archiveDeletes, each deleted note gets an archive copy staged beside
	// its deletion. The scrub below may hold a copy back; finishArchive then
	// holds the deletion back with it and writes the index only for the rest.
	var units []archiveUnit
	if cfg.ArchiveDeletes {
		var err error
		if units, err = stageArchiveCopies(repo, cfg); err != nil {
			return false, fmt.Errorf("archive deletes: %w", err)
		}
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
	if err := finishArchive(repo, cfg, units); err != nil {
		return false, fmt.Errorf("archive deletes: %w", err)
	}
	if encrypted {
		// Names, host and time must not reach the remote in the clear.
		changes, err := repo.StagedChanges()
		if err != nil {
			return false, fmt.Errorf("list staged changes: %w", err)
		}
		msg = fmt.Sprintf("memsync: %d files", len(changes))
	}
	return repo.CommitUnstaging(msg)
}

// holdBack unstages every staged file the scanner flags. It returns false when
// the denylist policy forbids committing anything this cycle (in which case
// the index has been emptied).
func holdBack(repo *gitwt.Repo, cfg config.Config) (bool, error) {
	deny, err := loadDenylist(cfg)
	hadDenylist := err == nil
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if cfg.RequireDenylist {
			log.Printf("scrub: requireDenylist is set but %s does not exist; committing nothing", cfg.DenylistPath())
			writeHeldRecord(cfg, heldBlocked+"\n")
			return false, unstageAll(repo)
		}
	case err != nil:
		// A denylist we cannot read is a denylist we cannot honor.
		log.Printf("scrub: cannot use %s (%v); committing nothing until it is fixed", cfg.DenylistPath(), err)
		writeHeldRecord(cfg, heldBlocked+"\n")
		return false, unstageAll(repo)
	}

	changes, err := repo.StagedChanges()
	if err != nil {
		return false, fmt.Errorf("list staged changes: %w", err)
	}
	scanner := scrub.New(deny)

	// Deletions are not scanned, and neither are submodule entries (no blob).
	var held, contentHeld, toRead []string
	var readIdx []int
	byPathHits := 0
	for i, c := range changes {
		if c.Status == 'D' || c.Mode == "160000" {
			continue
		}
		if heldByPath(scanner, c.Path) {
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
			contentHeld = append(contentHeld, path)
			continue
		}
		content = pt
		if rules := heldRules(scanner, content); len(rules) > 0 {
			// Path and rule names only: never log the matched text.
			log.Printf("scrub: holding back %s (rule: %s)", path, strings.Join(rules, ", "))
			held = append(held, path)
			contentHeld = append(contentHeld, path)
		}
	}
	if byPathHits > 0 {
		// The path itself matches the denylist, so it is not printed.
		log.Printf("scrub: <held back: path matches denylist> (%d)", byPathHits)
	}
	if err := repo.Unstage(held...); err != nil {
		return false, fmt.Errorf("unstage held-back files: %w", err)
	}
	recordHeld(cfg, contentHeld, byPathHits, hadDenylist)
	return true, nil
}

// heldRecordRel is where holdBack leaves what it held back, relative to the
// sync dir. The prompt hook reads it to tell a session which of its notes are
// not syncing, with no git process of its own. One entry per line: a
// repo-relative path held for its content, "#name-held N" (a count: a name
// that matches the denylist is the private term itself, so it is never
// written), "#denylist" (a denylist existed when it was written) or
// "#blocked" (the denylist policy stopped every commit; no paths).
const (
	heldRecordRel = ".state/held-back"
	heldBlocked   = "#blocked"
)

// recordHeld writes the paths held for their content and the count of those
// held for their name. It is best-effort: a record that cannot be written only
// costs the notice.
func recordHeld(cfg config.Config, held []string, nameHeld int, hadDenylist bool) {
	var b strings.Builder
	for _, h := range held {
		if !strings.Contains(h, "\n") {
			b.WriteString(h + "\n")
		}
	}
	if nameHeld > 0 {
		fmt.Fprintf(&b, "#name-held %d\n", nameHeld)
	}
	if b.Len() > 0 && hadDenylist {
		b.WriteString("#denylist\n")
	}
	writeHeldRecord(cfg, b.String())
}

// writeHeldRecord replaces the record with content (empty: no file).
func writeHeldRecord(cfg config.Config, content string) {
	p := filepath.Join(cfg.SyncDir, filepath.FromSlash(heldRecordRel))
	if content == "" {
		_ = os.Remove(p)
		return
	}
	if old, err := os.ReadFile(p); err == nil && string(old) == content {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(p), "held-back.*.tmp")
	if err != nil {
		return
	}
	_, werr := f.WriteString(content)
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), p) != nil {
		_ = os.Remove(f.Name())
	}
}

// heldByPath and heldRules are the scrub's two checks, used by holdBack at
// commit time and by HeldBack to report: one definition of what is held. A
// path that matches the denylist is held without its content being read.
func heldByPath(sc *scrub.Scanner, path string) bool { return sc.MatchesPath(path) }

// heldRules names the rules the content trips; none means it is not held.
func heldRules(sc *scrub.Scanner, content []byte) []string { return sc.Scan(content) }

// HeldBack lists the work-tree files, modified or untracked and not ignored,
// that the scrub would hold back from the next commit. It is read-only: it
// stages nothing and runs no filter, scanning the plaintext in the work tree.
// A missing denylist holds nothing by itself; the built-in rules still apply.
func HeldBack(repo *gitwt.Repo, cfg config.Config) ([]string, error) {
	if !cfg.Scrub {
		return nil, nil
	}
	deny, err := loadDenylist(cfg)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read denylist: %w", err)
	}
	dirty, err := repo.DirtyPaths("")
	if err != nil {
		return nil, fmt.Errorf("list changed files: %w", err)
	}
	scanner := scrub.New(deny)
	var held []string
	for _, rel := range dirty {
		// Per-PC files are never committed, so the scrub is not what stops them.
		if gitwt.LocalOnlyPath(rel) {
			continue
		}
		if heldByPath(scanner, rel) {
			held = append(held, rel)
			continue
		}
		abs := filepath.Join(repo.Dir, filepath.FromSlash(rel))
		if fi, err := os.Lstat(abs); err != nil || !fi.Mode().IsRegular() {
			continue // deleted, a directory or a link: nothing to scan
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if len(heldRules(scanner, content)) > 0 {
			held = append(held, rel)
		}
	}
	return held, nil
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
