// Package archive is the "archive, never delete" store of a memory repo: a note
// that would be removed is moved to archive/<YYYY-MM-DD>/<folder>/<file> in the
// same repo, with one line in archive/INDEX.md saying why. The package only
// knows the layout (place, index, list, search); the git handling is the
// caller's.
package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// Dir is the archive's directory at the root of a memory repo.
	Dir = "archive"
	// IndexName is the append-only index inside Dir.
	IndexName = "INDEX.md"
	// ReasonDeleted is the reason recorded for a note removed on a host.
	ReasonDeleted = "deleted on a host"
	// ReasonPruned is the reason recorded for a distilled entry pruned.
	ReasonPruned = "pruned from the distilled catalog"
	// DistilledFolder is the folder name under a date for pruned catalog entries.
	DistilledFolder = "distilled"
)

// Now is the clock; tests replace it.
var Now = time.Now

// Read reads an archived file. Search goes through it, so a reader that decrypts
// can replace it where files in the repo are encrypted.
var Read = os.ReadFile

// Date is today's archive date (UTC).
func Date() string { return Now().UTC().Format("2006-01-02") }

// Entry is one archived file.
type Entry struct {
	Date, Folder, File, Reason string
	Path                       string // slash-separated, relative to the repo root
}

// Place writes content under archive/<today>/<folder>/ in root and returns the
// slash-separated path relative to root. The file name carries the first eight
// hex digits of the content's SHA-256 before the extension (note.md becomes
// note.3f2a9c1b.md). Identical copies therefore share one path, which two hosts
// archiving the same note on the same day turn into one identical add, and
// different content never shares one: nothing is overwritten, nothing collides.
// A path already holding other bytes (a hash prefix collision, or a file
// planted there) is skipped for a longer prefix.
func Place(root, folder, name string, content []byte) (string, error) {
	date := Date()
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	sum := sha256.Sum256(content)
	full := hex.EncodeToString(sum[:])
	for _, n := range []int{8, 16, 32, 64} {
		rel := path.Join(Dir, date, folder, stem+"."+full[:n]+ext)
		fp := filepath.Join(root, filepath.FromSlash(rel))
		existing, err := os.ReadFile(fp)
		switch {
		case err == nil:
			if bytes.Equal(existing, content) {
				return rel, nil // an earlier cycle wrote it (held back, or a peer)
			}
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			return "", err
		}
		// O_EXCL: never replace a file that appeared since the read above.
		f, err := os.OpenFile(fp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(content)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", werr
		}
		return rel, nil
	}
	return "", errors.New("archive: no free path for " + name)
}

func indexLine(e Entry) string {
	return fmt.Sprintf("- %s | %s | %s | %s | %s", e.Date, e.Folder, e.File, e.Reason, e.Path)
}

// AppendIndex appends one line for e to archive/INDEX.md, unless the index
// already has that exact line. The file is append-only; the repo merges it
// with the union driver, so two hosts appending do not conflict.
func AppendIndex(root string, e Entry) error {
	p := filepath.Join(root, Dir, IndexName)
	line := indexLine(e)
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l == line {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		line = "\n" + line
	}
	_, werr := f.WriteString(line + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// List returns every archived file, oldest date first. since, when not empty,
// is a YYYY-MM-DD date: earlier dates are left out.
func List(root, since string) ([]Entry, error) {
	if since != "" {
		if _, err := time.Parse("2006-01-02", since); err != nil {
			return nil, fmt.Errorf("--since %q: want a date like 2026-10-25", since)
		}
	}
	base := filepath.Join(root, Dir)
	if fi, err := os.Lstat(base); err != nil || !fi.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no archive (or a link posing as one)
		}
		return nil, err
	}
	dates, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range dates {
		if !d.IsDir() || (since != "" && d.Name() < since) {
			continue
		}
		if _, err := time.Parse("2006-01-02", d.Name()); err != nil {
			continue
		}
		err := filepath.WalkDir(filepath.Join(base, d.Name()), func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !de.Type().IsRegular() {
				return nil // a directory, or a symlink or device: never followed
			}
			rel, _ := filepath.Rel(filepath.Join(base, d.Name()), p)
			parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
			if len(parts) != 2 {
				return nil
			}
			out = append(out, Entry{Date: d.Name(), Folder: parts[0], File: parts[1],
				Path: path.Join(Dir, d.Name(), filepath.ToSlash(rel))})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Hit is one archived file that matches a search term.
type Hit struct {
	Path  string
	Lines []string // matching lines (empty when only the name matched)
}

// Search returns the archived files whose name or contents contain term, case
// insensitively. INDEX.md is not searched: it is not an archived note.
func Search(root, term string) ([]Hit, error) {
	if term == "" {
		return nil, errors.New("search term is empty")
	}
	entries, err := List(root, "")
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(term)
	var hits []Hit
	for _, e := range entries {
		h := Hit{Path: e.Path}
		nameHit := strings.Contains(strings.ToLower(e.Folder+"/"+e.File), needle)
		b, err := Read(filepath.Join(root, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Path, err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(strings.ToLower(l), needle) {
				h.Lines = append(h.Lines, l)
			}
		}
		if nameHit || len(h.Lines) > 0 {
			hits = append(hits, h)
		}
	}
	return hits, nil
}
