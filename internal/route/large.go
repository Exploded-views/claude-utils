package route

import (
	"os"
	"path/filepath"
	"time"
)

// liveWindow is how recently a folder's notes must have changed for the folder
// to count as a live project (not one of the many folders for deleted dirs).
const liveWindow = 14 * 24 * time.Hour

// looksLive reports whether the folder's own directory under ProjectsDir, or a
// file in it, changed within liveWindow. It reads at most a few hundred names.
func (r *Router) looksLive(folder string) bool {
	if r.ProjectsDir == "" {
		return false
	}
	cutoff := r.Now().Add(-liveWindow)
	budget := 300
	var recent func(dir string, depth int) bool
	recent = func(dir string, depth int) bool {
		info, err := os.Stat(dir)
		if err != nil {
			return false
		}
		if info.ModTime().After(cutoff) {
			return true
		}
		if depth == 0 || !info.IsDir() {
			return false
		}
		es, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		for _, e := range es {
			if budget--; budget < 0 {
				return false
			}
			if recent(filepath.Join(dir, e.Name()), depth-1) {
				return true
			}
		}
		return false
	}
	return recent(filepath.Join(r.ProjectsDir, folder), 3)
}

// tooLargeLive reports whether d left the folder unresolved because a directory
// on its path was too big to list, and the folder looks live.
func (r *Router) tooLargeLive(folder string, d Decision) bool {
	return d.Repo == "" && d.Reason == ReasonUnresolved && r.largeFn != nil && r.largeFn(folder) && r.looksLive(folder)
}

// noteLargeLocked logs, once per folder, a live folder left unresolved because
// a directory on its path is too large to list. r.mu is held.
func (r *Router) noteLargeLocked(folder string, d Decision) {
	if !r.tooLargeLive(folder, d) {
		return
	}
	k := folder + "\x00toolarge"
	if !r.logged[k] {
		r.logged[k] = true
		r.Logf("route: folder %s is not routed: a directory on its path has too many entries to list, so a name with an unusual character in it cannot be found; it stays denied", hash8(folder))
	}
}
