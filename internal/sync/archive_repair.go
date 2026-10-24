package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// An edit on one host and an archive of the same note on another merge without
// a conflict: git sees the archive as a 100% rename, so the rebase carries the
// edit into the archive copy. The note is then gone from live memory, and the
// copy no longer holds the bytes its hash name promises. After every pull that
// moved HEAD, each archive copy that arrived is checked against its name. For a
// mismatch the edit goes back to the note's live path (beside it, as
// <name>.conflict-<sha8>.<ext>, when that path now holds other content), the
// copy goes back to the bytes it is named for, and both are committed
// together.

const reasonRestored = "edited after archive, restored"

var hashLens = []int{8, 16, 32, 64}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return s != ""
}

// archiveNameHash splits <stem>.<hex><ext> into the note's own name and the
// hash prefix. ok is false for a name that carries no hash.
func archiveNameHash(name string) (orig, prefix string, ok bool) {
	for _, ext := range []string{path.Ext(name), ""} {
		stem := strings.TrimSuffix(name, ext)
		i := strings.LastIndexByte(stem, '.')
		if i <= 0 {
			continue
		}
		h := stem[i+1:]
		for _, n := range hashLens {
			if len(h) == n && isHex(h) {
				return stem[:i] + ext, h, true
			}
		}
	}
	return "", "", false
}

func fullHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// archiveBytesMatch reports whether b hashes to the prefix in name.
func archiveBytesMatch(name string, b []byte) bool {
	_, prefix, ok := archiveNameHash(name)
	return ok && strings.HasPrefix(fullHash(b), prefix)
}

// repairEditedArchives checks the archive copies that changed between pre and
// post and repairs the mismatched ones. It returns how many notes it restored.
// On an error nothing is committed (the caller resets to pre).
func repairEditedArchives(repo *gitwt.Repo, cfg config.Config, pre, post string) (int, error) {
	out, err := repo.Run("diff", "--name-status", "-z", "--no-renames", pre+".."+post, "--", gitwt.Literal(archive.Dir))
	if err != nil {
		return 0, err
	}
	var bad []string
	f := strings.Split(out, "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] != "A" && f[i] != "M" {
			continue
		}
		p := f[i+1]
		parts := strings.Split(p, "/")
		if len(parts) != 4 || parts[2] == archive.DistilledFolder {
			continue // INDEX.md, or not a note copy
		}
		if _, _, ok := archiveNameHash(parts[3]); !ok {
			continue
		}
		cur, ok := blobAt(repo, post, p)
		if ok && !archiveBytesMatch(parts[3], cur) {
			bad = append(bad, p)
		}
	}
	if len(bad) == 0 {
		return 0, nil
	}
	var stage, mustStage []string
	for _, p := range bad {
		parts := strings.Split(p, "/")
		date, folder, name := parts[1], parts[2], parts[3]
		orig, prefix, _ := archiveNameHash(name)
		edited, _ := blobAt(repo, post, p)

		original, ok := findOriginal(repo, post, p, folder, orig, prefix)
		if !ok {
			return 0, fmt.Errorf("an archived note was edited and its original text cannot be found")
		}
		live := path.Join("projects", folder, "memory", orig)
		if cur, exists := blobAt(repo, post, live); exists && string(cur) != string(edited) {
			ext := path.Ext(orig)
			live = path.Join("projects", folder, "memory",
				strings.TrimSuffix(orig, ext)+".conflict-"+fullHash(edited)[:8]+ext)
		}
		if err := writeRepoPath(repo, live, edited); err != nil {
			return 0, err
		}
		if err := writeRepoPath(repo, p, original); err != nil {
			return 0, err
		}
		if err := archive.AppendIndex(cfg.SyncDir, archive.Entry{
			Date: date, Folder: folder, File: orig, Reason: reasonRestored, Path: p,
		}); err != nil {
			return 0, err
		}
		stage = append(stage, gitwt.Literal(live), gitwt.Literal(p))
		mustStage = append(mustStage, live, p)
	}
	stage = append(stage, gitwt.Literal(path.Join(archive.Dir, archive.IndexName)))
	if _, err := repo.Run(append([]string{"add", "-A", "--"}, stage...)...); err != nil {
		return 0, err
	}
	// Like every commit, this one is scrubbed. The restore is all or nothing: a
	// note without its archive fix (or the reverse) must not be committed, so
	// anything held back fails the repair and the caller undoes the pull.
	if cfg.Scrub {
		ok, err := holdBack(repo, cfg)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("the scrub allows no commit now")
		}
		staged, err := repo.StagedChanges()
		if err != nil {
			return 0, err
		}
		in := map[string]bool{}
		for _, c := range staged {
			in[c.Path] = true
		}
		for _, p := range mustStage {
			if !in[p] {
				return 0, fmt.Errorf("the scrub held back a restored note")
			}
		}
	}
	if _, err := repo.Commit("auto: restore edited notes @ " + time.Now().UTC().Format(time.RFC3339)); err != nil {
		return 0, err
	}
	return len(bad), nil
}

func writeRepoPath(repo *gitwt.Repo, rel string, b []byte) error {
	full := filepath.Join(repo.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	return os.WriteFile(full, b, 0o600)
}

// findOriginal looks for the bytes an archive copy is named for: in the history
// of the copy itself (an earlier version of it, before an edit merged in) and of
// the note's live path (the archived text was its last), newest first.
func findOriginal(repo *gitwt.Repo, rev, archived, folder, orig, prefix string) ([]byte, bool) {
	for _, p := range []string{archived, path.Join("projects", folder, "memory", orig)} {
		revs, err := repo.Run("rev-list", rev, "--", gitwt.Literal(p))
		if err != nil {
			continue
		}
		for _, c := range strings.Fields(revs) {
			if b, ok := blobAt(repo, c, p); ok && strings.HasPrefix(fullHash(b), prefix) {
				return b, true
			}
		}
	}
	return nil, false
}
