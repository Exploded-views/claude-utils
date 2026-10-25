package related

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/route"
)

// Ref is one pointer from a checkout at another repository: the remote key
// (host/owner/repo, lower case) and the file that names it.
type Ref struct {
	Key  string
	File string
}

// maxRefFile caps how much of one file is read.
const maxRefFile = 1 << 20

var (
	// host/owner/repo of a hosting URL or go module path. github.com is the
	// common case; other hosts need an ssh/https URL (.gitmodules) or a
	// go.mod path under a dotted host.
	githubRe = regexp.MustCompile(`(?i)github\.com[/:]([A-Za-z0-9][A-Za-z0-9_.-]*)/([A-Za-z0-9_.-]+)`)
	modRe    = regexp.MustCompile(`(?i)\b((?:[a-z0-9-]+\.)+[a-z]{2,})/([A-Za-z0-9][A-Za-z0-9_.-]*)/([A-Za-z0-9][A-Za-z0-9_.-]*)`)
	npmShort = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.-]*)/([A-Za-z0-9_.-]+)(#.*)?$`)
)

func cleanRepo(s string) string {
	s = strings.TrimSuffix(strings.TrimRight(s, "."), ".git")
	return strings.TrimRight(s, ".")
}

func keyOf(host, owner, repo string) string {
	return strings.ToLower(host + "/" + owner + "/" + cleanRepo(repo))
}

func readCapped(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, maxRefFile))
	return string(b)
}

// CheckoutRefs looks at the top of a checkout for pointers at other
// repositories: .gitmodules URLs, go.mod require and replace paths,
// package.json dependencies that are git or GitHub references, and
// github.com links in the README. Only the matched keys are returned, never
// file text.
func CheckoutRefs(dir string) []Ref {
	seen := map[Ref]bool{}
	var out []Ref
	add := func(file, key string) {
		r := Ref{Key: key, File: file}
		if key != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}

	for _, line := range strings.Split(readCapped(filepath.Join(dir, ".gitmodules")), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "url") {
			continue
		}
		if _, v, ok := strings.Cut(line, "="); ok {
			if k, ok := route.NormalizeRemote(strings.TrimSpace(v)); ok {
				add(".gitmodules", strings.ToLower(k))
			}
		}
	}

	for _, line := range strings.Split(readCapped(filepath.Join(dir, "go.mod")), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "module\t") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, ".") || strings.HasPrefix(f, "/") {
				continue // a local replace path
			}
			if m := modRe.FindStringSubmatch(f); m != nil && strings.HasPrefix(f, m[0]) {
				add("go.mod", keyOf(m[1], m[2], m[3]))
			}
		}
	}

	var pkg struct {
		Deps map[string]string `json:"dependencies"`
		Dev  map[string]string `json:"devDependencies"`
		Peer map[string]string `json:"peerDependencies"`
		Opt  map[string]string `json:"optionalDependencies"`
	}
	if json.Unmarshal([]byte(readCapped(filepath.Join(dir, "package.json"))), &pkg) == nil {
		var specs []string
		for _, m := range []map[string]string{pkg.Deps, pkg.Dev, pkg.Peer, pkg.Opt} {
			for _, v := range m {
				specs = append(specs, v)
			}
		}
		sort.Strings(specs)
		for _, v := range specs {
			add("package.json", npmSpecKey(v))
		}
	}

	readme := ""
	for _, n := range []string{"README.md", "README", "README.rst", "README.txt", "readme.md"} {
		if s := readCapped(filepath.Join(dir, n)); s != "" {
			readme = s
			break
		}
	}
	for _, m := range githubRe.FindAllStringSubmatch(readme, -1) {
		add("README.md", keyOf("github.com", m[1], m[2]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// npmSpecKey is the remote key a package.json dependency value points at, if
// it is a git or GitHub reference (and not a version range or a tag).
func npmSpecKey(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case strings.HasPrefix(v, "github:"):
		if m := npmShort.FindStringSubmatch(strings.TrimPrefix(v, "github:")); m != nil {
			return keyOf("github.com", m[1], m[2])
		}
	case strings.Contains(v, "://") || strings.HasPrefix(v, "git@"):
		v = strings.TrimPrefix(v, "git+")
		if k, ok := route.NormalizeRemote(strings.SplitN(v, "#", 2)[0]); ok {
			return strings.ToLower(k)
		}
	default:
		if m := npmShort.FindStringSubmatch(v); m != nil && !strings.HasPrefix(v, "@") {
			return keyOf("github.com", m[1], m[2])
		}
	}
	return ""
}
