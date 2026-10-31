package gitwt

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// LocalOnlyPath reports whether a repo path is per-PC daemon state that the
// generated .gitignore keeps out of the repo, and that must therefore never
// arrive from a remote (a tracked config.json would replace this host's own on
// checkout): config.json, daemon.pid and denylist.txt at the root, everything
// under .state/, the derived distilled/DISTILLED.md, and the tool's own
// temporary names: *.tmp and *.tmp.* at the root, ".<name>.<id>.tmp" (the
// atomic-write temp) at any depth, and "<name>.from-remote-<hex id>" at any
// depth. A note a person named ideas.tmp.md in a project folder is not one.
//
// The match is made on FoldPath(p), so the spellings a case- and
// normalisation-insensitive file system resolves to the same file count too
// (Config.json, CONFIG.JSON, a long s for an s, an ignorable code point), as do
// the spellings Windows resolves to it: trailing dots and spaces, and 8.3
// short names (CONFIG~1.JSO). The names above are all ASCII, so the only
// non-ASCII spellings that can reach them are the case-fold and singleton
// equivalents FoldPath maps.
func LocalOnlyPath(p string) bool {
	p = FoldPath(p)
	// A per-PC name as the first component of any path, file or directory: git
	// removes an ignored file to make room for a directory of that name, and
	// replaces a directory with a file.
	parts := strings.Split(p, "/")
	switch parts[0] {
	case "config.json", "daemon.pid", "denylist.txt", ".state":
		return true
	case "distilled":
		if len(parts) > 1 && (parts[1] == "distilled.md" || shortNameRE.MatchString(parts[1])) {
			return true
		}
	}
	// Any 8.3-shaped name at the root, whatever its stem: NTFS may derive a
	// short name from a hash, so the stem need not be a prefix of the long one.
	if shortNameRE.MatchString(parts[0]) {
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

// FoldPath returns the form of a repo path in which two spellings that one
// file system resolves to the same file are equal: every component is case-
// and normalisation-folded (FoldName), loses its trailing dots and spaces, and,
// when it has the shape of an 8.3 short name of one of the per-PC names, is
// replaced by that name. It is for comparing, not for display.
func FoldPath(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		c = strings.TrimRight(FoldName(c), ". ")
		if long, ok := expandShort(c); ok {
			c = long
		}
		parts[i] = c
	}
	return strings.Join(parts, "/")
}

// FoldName folds one name for comparison: ignorable code points are dropped,
// and each rune is case-folded, with the singleton and ligature spellings that
// fold to ASCII spelled out (a normalisation-insensitive file system treats
// them as the ASCII name).
func FoldName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case ignorable(r):
			continue
		case r == 0xDF || r == 0x1E9E:
			b.WriteString("ss")
		case r >= 0xFB00 && r <= 0xFB06:
			b.WriteString([]string{"ff", "fi", "fl", "ffi", "ffl", "st", "st"}[r-0xFB00])
		case r == 0x212A:
			b.WriteByte('k')
		case r == 0x037E:
			b.WriteByte(';')
		case r == 0x1FEF:
			b.WriteByte('`')
		default:
			b.WriteRune(unicode.ToLower(unicode.ToUpper(r)))
		}
	}
	return b.String()
}

// ignorable reports whether a file system may skip r in a name. It
// over-approximates on purpose: git's own HFS check skips U+200C-U+200F,
// U+202A-U+202E, U+206A-U+206F and U+FEFF, and APFS and others skip more, so
// every default-ignorable code point and every format character (category Cf)
// counts, with the explicit ranges kept as a floor. Dropping one too many only
// widens what is refused at a handful of root names.
func ignorable(r rune) bool {
	return r == 0xFEFF || (r >= 0x200B && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2060 && r <= 0x2064) || (r >= 0x206A && r <= 0x206F) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Variation_Selector, r) ||
		unicode.Is(unicode.Cf, r)
}

// perPCNames are the per-PC names a short name can stand for.
var perPCNames = []string{"config.json", "daemon.pid", "denylist.txt", ".state", "distilled", "distilled.md"}

var shortNameRE = regexp.MustCompile(`^([^~.]{1,6})~[0-9]+(?:\.([^.]{0,3}))?$`)

// expandShort maps a name shaped like an 8.3 short name ("config~1.jso") to
// the per-PC name it can abbreviate, if any.
func expandShort(c string) (string, bool) {
	m := shortNameRE.FindStringSubmatch(c)
	if m == nil {
		return "", false
	}
	for _, long := range perPCNames {
		stem, ext, _ := strings.Cut(strings.TrimPrefix(long, "."), ".")
		if strings.HasPrefix(stem, m[1]) && strings.HasPrefix(ext, m[2]) && (m[2] != "" || ext == "") {
			return long, true
		}
	}
	return "", false
}

// TrackedLocalOnly lists the paths in rev's tree that must never arrive from a
// remote: the local-only paths, and every symlink (mode 120000) and gitlink
// (160000) entry, which a mirror copy would follow out of the tree. Only paths
// are returned, never contents.
func (r *Repo) TrackedLocalOnly(rev string) ([]string, error) {
	out, err := r.Run("-c", "core.quotePath=false", "ls-tree", "-r", "-z", rev)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, ent := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(ent, "\t")
		if !ok {
			continue
		}
		mode, _, _ := strings.Cut(meta, " ")
		if mode == "120000" || mode == "160000" || LocalOnlyPath(p) {
			bad = append(bad, p)
		}
	}
	return bad, nil
}

// LocalOnlyExcludes are the root-anchored ignore patterns for the per-PC files.
// They go in .git/info/exclude, which git never transfers, so a synced
// .gitignore cannot take them out.
var LocalOnlyExcludes = []string{"/config.json", "/daemon.pid", "/denylist.txt", "/.state/", "/distilled/DISTILLED.md"}

// EnsureExclude appends to .git/info/exclude whichever of lines it lacks.
func (r *Repo) EnsureExclude(lines ...string) error {
	// A plain repo keeps it at .git/info/exclude; only a gitfile (worktree,
	// submodule) needs git to say where, which costs a spawn.
	p := filepath.Join(r.Dir, ".git", "info", "exclude")
	if st, err := os.Stat(filepath.Join(r.Dir, ".git")); err != nil || !st.IsDir() {
		out, err := r.Run("rev-parse", "--git-path", "info/exclude")
		if err != nil {
			return err
		}
		p = strings.TrimSpace(out)
		if !filepath.IsAbs(p) {
			p = filepath.Join(r.Dir, p)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add string
	for _, l := range lines {
		if !have[l] {
			add += l + "\n"
		}
	}
	if add == "" {
		return nil
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		add = "\n" + add
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(add); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ErrPerPCStaged means a per-PC path, or a link, is staged for a commit.
var ErrPerPCStaged = errors.New("refusing to commit a per-PC file or a link")

// forbidden picks, from staged changes, the additions and changes (not
// deletions) that must never be committed: per-PC paths, symlinks and gitlinks.
func forbidden(changes []StagedChange) []string {
	var bad []string
	for _, c := range changes {
		if c.Status == 'D' {
			continue
		}
		if c.Mode == "120000" || c.Mode == "160000" || LocalOnlyPath(c.Path) {
			bad = append(bad, c.Path)
		}
	}
	return bad
}
