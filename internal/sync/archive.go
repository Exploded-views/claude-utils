package sync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
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

// archiveUnit is one deletion and the archive copy that keeps it. The unit is
// held together: the deletion, the copy, the index line and the union rule are
// committed together or not at all.
type archiveUnit struct {
	rel          string // archive path of the copy
	src          string // the deleted path
	folder, name string
}

// pureRenames are the deleted paths git pairs with an identical (100%) added
// file outside archive/: those are renames, and nothing is lost. A file in
// archive/ is never a rename target. The tool writes its copies there, and a
// copy left over from a cycle that held it back would otherwise pair with the
// next delete of the same note and let the delete commit alone.
func pureRenames(repo *gitwt.Repo) (map[string]bool, error) {
	out, err := repo.Run("diff", "--cached", "--name-status", "-z", "--find-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	pure := map[string]bool{}
	f := strings.Split(out, "\x00")
	for i := 0; i < len(f); {
		status := f[i]
		i++
		if status == "" {
			continue
		}
		n := 1
		if status[0] == 'R' || status[0] == 'C' {
			n = 2
		}
		if i+n > len(f) {
			break
		}
		if status[0] == 'R' && status[1:] == "100" && !strings.HasPrefix(f[i+1], archive.Dir+"/") {
			pure[f[i]] = true
		}
		i += n
	}
	return pure, nil
}

// stageArchiveCopies writes and stages an archive copy for every staged
// deletion of a note, except the pure renames. A rename that is not 100% (the
// note was edited as it moved) keeps the rename and archives the old content.
// It writes no index line: finishArchive does, for the copies that stay.
func stageArchiveCopies(repo *gitwt.Repo, cfg config.Config) ([]archiveUnit, error) {
	if headRev(repo) == "" {
		return nil, nil // nothing committed yet, so nothing to keep
	}
	out, err := repo.Run("diff", "--cached", "--name-status", "-z", "--no-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	pure, err := pureRenames(repo)
	if err != nil {
		return nil, err
	}
	var units []archiveUnit
	f := strings.Split(out, "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] != "D" || pure[f[i+1]] {
			continue
		}
		p := f[i+1]
		hash, name, ok := splitMirrorPath(p)
		if !ok || shouldIgnoreFile(name) {
			continue
		}
		content, ok := blobAt(repo, "HEAD", p)
		if !ok {
			continue
		}
		rel, err := archive.Place(cfg.SyncDir, hash, name, content)
		if err != nil {
			return nil, err
		}
		units = append(units, archiveUnit{rel: rel, src: p, folder: hash, name: name})
	}
	if len(units) == 0 {
		return nil, nil
	}
	args := []string{"add", "-A", "--"}
	for _, u := range units {
		args = append(args, gitwt.Literal(u.rel))
	}
	if _, err := repo.Run(args...); err != nil {
		return nil, fmt.Errorf("stage archive: %w", err)
	}
	return units, nil
}

// finishArchive runs after the scrub. A unit whose copy is not in the commit
// (held back) keeps its deletion back too, so the note never leaves the tree
// without its copy; the work-tree keeps both and the next cycle tries again.
// The index lines and the union rule are written, and staged, for the units
// that stay.
func finishArchive(repo *gitwt.Repo, cfg config.Config, units []archiveUnit) error {
	if len(units) == 0 {
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
	inHead, err := repo.HeadFiles(archive.Dir)
	if err != nil {
		return err
	}
	var held, kept []string
	var stay []archiveUnit
	for _, u := range units {
		if in[u.rel] || inHead[u.rel] { // already committed: an identical copy from a peer
			stay = append(stay, u)
		} else {
			held = append(held, u.src)
		}
	}
	if err := repo.Unstage(held...); err != nil {
		return err
	}
	if len(stay) == 0 {
		return nil
	}
	for _, u := range stay {
		if err := archive.AppendIndex(cfg.SyncDir, archive.Entry{
			Date: strings.Split(u.rel, "/")[1], Folder: u.folder, File: u.name, Reason: archive.ReasonDeleted, Path: u.rel,
		}); err != nil {
			return err
		}
	}
	kept = []string{gitwt.Literal(path.Join(archive.Dir, archive.IndexName))}
	// An encrypted repo keeps its rules in $GIT_DIR/info/attributes (the union
	// rule for the index included), and a changed .gitattributes there is a
	// reviewed event: leave the file alone.
	if !agecrypt.Enabled(repo) {
		if err := ensureUnionAttr(cfg.SyncDir); err != nil {
			return err
		}
		kept = append(kept, gitwt.Literal(".gitattributes"))
	}
	_, err = repo.Run(append([]string{"add", "-A", "--"}, kept...)...)
	return err
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
