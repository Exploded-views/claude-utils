package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Archive, never delete. Every way a note leaves a Claude memory folder (the
// watcher, Reconcile's manifest check, a vanished file) ends the same way: the
// mirror copy is gone from the work-tree. So the archiving is done once, at
// commit time, on the staged deletions: git has already paired a rename with
// its new name (a deleted file with a matching added one is a R, not a D), and
// what is left is a real deletion. Each of those has its HEAD content written
// to archive/<date>/<folder>/<file> and staged, so the commit holds a move.
//
// Nothing below archive/ is ever a Claude folder: the mirror is
// <sync>/projects, and propagate, catch-up, Reconcile, the manifest and
// `changes` only look at projects/<folder>/memory/<file>.

// unionAttr makes the append-only index merge as a union, so two hosts that
// append in the same cycle do not conflict.
const unionAttr = "archive/INDEX.md merge=union"

// archiveDeletes archives the staged deletions of notes in folders owns
// accepts. It returns archive path -> deleted path, for the ones it archived.
func archiveDeletes(repo *gitwt.Repo, cfg config.Config, owns func(string) bool) (map[string]string, error) {
	if headRev(repo) == "" {
		return nil, nil // nothing committed yet, so nothing to keep
	}
	out, err := repo.Run("diff", "--cached", "--name-status", "-z", "--find-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	var dels []string
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); {
		status := fields[i]
		i++
		if status == "" {
			continue
		}
		n := 1
		if status[0] == 'R' || status[0] == 'C' {
			n = 2
		}
		if i+n > len(fields) {
			break
		}
		if status[0] == 'D' {
			dels = append(dels, fields[i])
		}
		i += n
	}
	inHead, err := repo.HeadFiles(archive.Dir)
	if err != nil {
		return nil, err
	}
	archived := map[string]string{}
	for _, p := range dels {
		hash, name, ok := splitMirrorPath(p)
		if !ok || shouldIgnoreFile(name) || (owns != nil && !owns(hash)) {
			continue
		}
		content, ok := blobAt(repo, "HEAD", p)
		if !ok {
			continue
		}
		// A retry may land on the file an unfinished cycle wrote (held back,
		// not yet committed); one that is committed is never reused.
		rel, err := archive.PlaceReuse(cfg.SyncDir, hash, name, content, func(rel string) bool { return !inHead[rel] })
		if err != nil {
			return nil, err
		}
		if err := archive.AppendIndex(cfg.SyncDir, archive.Entry{
			Date: strings.Split(rel, "/")[1], Folder: hash, File: name, Reason: archive.ReasonDeleted, Path: rel,
		}); err != nil {
			return nil, err
		}
		archived[rel] = p
	}
	if len(archived) == 0 {
		return nil, nil
	}
	if err := ensureUnionAttr(cfg.SyncDir); err != nil {
		return nil, err
	}
	if _, err := repo.Run("add", "-A", "--", gitwt.Literal(archive.Dir), gitwt.Literal(".gitattributes")); err != nil {
		return nil, fmt.Errorf("stage archive: %w", err)
	}
	return archived, nil
}

// keepDeletesWithArchive unstages the deletion of every note whose archive copy
// the scrub held back: the note must not leave the tree without its copy. The
// work-tree keeps both, so the next cycle tries again.
func keepDeletesWithArchive(repo *gitwt.Repo, archived map[string]string) error {
	if len(archived) == 0 {
		return nil
	}
	staged, err := repo.StagedChanges()
	if err != nil {
		return err
	}
	in := map[string]bool{}
	for _, c := range staged {
		if c.Status == 'A' {
			in[c.Path] = true
		}
	}
	var keep []string
	for arch, src := range archived {
		if !in[arch] {
			keep = append(keep, src)
		}
	}
	return repo.Unstage(keep...)
}

// ensureUnionAttr adds the union rule for the index to .gitattributes.
func ensureUnionAttr(syncDir string) error {
	p := filepath.Join(syncDir, ".gitattributes")
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == unionAttr {
			return nil
		}
	}
	s := string(b)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return os.WriteFile(p, []byte(s+unionAttr+"\n"), 0o600)
}
