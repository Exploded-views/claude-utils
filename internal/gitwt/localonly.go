package gitwt

import (
	"path"
	"regexp"
	"strings"
)

// LocalOnlyPath reports whether a repo path is per-PC daemon state that the
// generated .gitignore keeps out of the repo, and that must therefore never
// arrive from a remote (a tracked config.json would replace this host's own on
// checkout): config.json, daemon.pid and denylist.txt at the root, everything
// under .state/, the derived distilled/DISTILLED.md, and the tool's own
// temporary names: *.tmp and *.tmp.* at the root, ".<name>.<id>.tmp" (the
// atomic-write temp) at any depth, and "<name>.from-remote-<hex id>" at any
// depth. A note a person named ideas.tmp.md in a project folder is not one.
func LocalOnlyPath(p string) bool {
	switch p {
	case "config.json", "daemon.pid", "denylist.txt", "distilled/DISTILLED.md":
		return true
	}
	if strings.HasPrefix(p, ".state/") {
		return true
	}
	base := path.Base(p)
	if !strings.Contains(p, "/") {
		for _, pat := range []string{"*.tmp", "*.tmp.*"} {
			if ok, _ := path.Match(pat, base); ok {
				return true
			}
		}
	}
	if strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".tmp") && len(base) > 5 {
		return true
	}
	return fromRemoteRE.MatchString(base)
}

var fromRemoteRE = regexp.MustCompile(`^.+\.from-remote-[0-9a-f]{6,}$`)

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
