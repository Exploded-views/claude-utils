package gitwt

import (
	"path"
	"strings"
)

// LocalOnlyPath reports whether a repo path is per-PC daemon state that the
// generated .gitignore keeps out of the repo: config.json, daemon.pid and
// denylist.txt at the root, everything under .state/, the derived
// distilled/DISTILLED.md, and the temporary names *.tmp, *.tmp.* and
// *.from-remote-*. None of them may arrive from a remote: a tracked
// config.json would replace this host's own on checkout.
func LocalOnlyPath(p string) bool {
	switch p {
	case "config.json", "daemon.pid", "denylist.txt", "distilled/DISTILLED.md":
		return true
	}
	if strings.HasPrefix(p, ".state/") {
		return true
	}
	base := path.Base(p)
	for _, pat := range []string{"*.tmp", "*.tmp.*", "*.from-remote-*"} {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// TrackedLocalOnly lists the local-only paths that rev's tree tracks.
func (r *Repo) TrackedLocalOnly(rev string) ([]string, error) {
	out, err := r.Run("-c", "core.quotePath=false", "ls-tree", "-r", "-z", "--name-only", rev)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" && LocalOnlyPath(p) {
			bad = append(bad, p)
		}
	}
	return bad, nil
}
