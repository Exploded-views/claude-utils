// Package changes reports which memory files changed in the sync repo since a
// given point, so a Claude Code hook can tell a long-running session that the
// notes it read earlier are out of date.
//
// It only reads the sync repo's history. It does not fetch: the daemon keeps
// the repo current, and "changed" means "changed in what this PC has pulled or
// committed".
package changes

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Options selects what Compute reports.
type Options struct {
	SyncDir   string // the sync work-tree (a git repo)
	ClaudeDir string // ~/.claude/projects, used to build each change's local path

	// Since is a git revision (SHA, abbreviated SHA, branch, HEAD~3, ...).
	// Marker is the path of a file whose first line is one. Exactly one of
	// the two must be set.
	Since  string
	Marker string

	// Project limits the report to one project: a project directory such as
	// /home/alice/src/app, or Claude's escaped folder name for it.
	Project string

	// Limit caps how many changes are listed; the rest are only counted.
	// Zero means no cap.
	Limit int

	// UpdateMarker rewrites Marker to the current HEAD once the report is
	// built; it is only valid with Marker. A marker file that does not exist
	// yet is created and nothing is reported: that run only sets the baseline.
	// A marker whose commit no longer exists is reset the same way and the
	// result has Reset set.
	UpdateMarker bool

	// DeferMarker leaves the final marker rewrite to the caller (WriteMarker),
	// for callers that must not advance it until the report was delivered.
	// A marker that has to be created or reset is still written by Compute,
	// since nothing is reported then.
	DeferMarker bool
}

// Change is one memory file that differs between Since and HEAD.
type Change struct {
	Status  string `json:"status"`  // "added", "changed" or "deleted"
	Project string `json:"project"` // Claude's folder name for the project
	File    string `json:"file"`    // file name inside the project's memory dir
	Path    string `json:"path"`    // path inside the sync repo
	Local   string `json:"local"`   // where Claude Code reads (or read) the file
}

// Display returns Local for printing. Names come from the sync repo, so one
// with a newline or other unprintable character could forge extra lines in a
// prompt; such a path is quoted instead.
func (c Change) Display() string {
	for _, name := range []string{c.Project, c.File, c.Local} {
		for _, r := range name {
			if !strconv.IsPrint(r) {
				return strconv.Quote(c.Local)
			}
		}
	}
	return c.Local
}

// Result is what Compute returns and what --json prints.
type Result struct {
	Head    string   `json:"head"`    // current HEAD of the sync repo
	Since   string   `json:"since"`   // the commit Since or Marker resolved to
	Busy    bool     `json:"busy"`    // the repo is mid-rebase/merge; nothing was reported
	Reset   bool     `json:"reset"`   // the marker named a lost commit and was reset to Head
	Total   int      `json:"total"`   // number of changed files
	More    int      `json:"more"`    // changes not listed because of Limit
	Changes []Change `json:"changes"` // never nil
}

// Compute lists the memory files that changed between opts.Since and HEAD.
func Compute(opts Options) (Result, error) {
	repo := gitwt.New(opts.SyncDir)
	res := Result{Changes: []Change{}}

	head, ok := revParse(repo, "HEAD")
	if !ok {
		return res, fmt.Errorf("%s has no commits yet", opts.SyncDir)
	}
	res.Head = head

	if opts.Since == "" && opts.Marker == "" {
		return res, errors.New("one of --since and --marker is required")
	}
	if opts.Since != "" && opts.Marker != "" {
		return res, errors.New("--since and --marker cannot be used together")
	}
	if opts.UpdateMarker && opts.Marker == "" {
		return res, errors.New("--update-marker needs --marker")
	}

	if busy(repo) {
		res.Busy = true
		return res, nil
	}

	since, err := resolveSince(repo, opts)
	if err != nil {
		return res, err
	}
	if since == "" {
		// Marker file missing or its commit lost: this run only records the
		// baseline.
		res.Since = head
		res.Reset = opts.Marker != "" && markerExists(opts.Marker)
		return res, WriteMarker(opts.Marker, head)
	}
	res.Since = since

	spec := ":(literal)projects/"
	if opts.Project != "" {
		spec = ":(literal)projects/" + projectFolder(opts.Project) + "/memory/"
	}
	out, err := repo.Run("diff", "--name-status", "--no-renames", "-z", since, head, "--", spec)
	if err != nil {
		return res, err
	}
	all := parse(out, opts.ClaudeDir)
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })

	res.Total = len(all)
	res.Changes = all
	if opts.Limit > 0 && len(all) > opts.Limit {
		res.Changes = all[:opts.Limit]
		res.More = len(all) - opts.Limit
	}
	if res.Changes == nil {
		res.Changes = []Change{}
	}
	if opts.UpdateMarker && !opts.DeferMarker {
		if err := WriteMarker(opts.Marker, head); err != nil {
			return res, err
		}
	}
	return res, nil
}

// resolveSince turns opts.Since or opts.Marker into a commit SHA. An empty SHA
// with a nil error means the marker has to be (re)created: the file does not
// exist yet, or names a commit that is gone, and opts.UpdateMarker is set.
func resolveSince(repo *gitwt.Repo, opts Options) (string, error) {
	if s := opts.Since; s != "" {
		if strings.HasPrefix(s, "-") {
			return "", fmt.Errorf("--since %q is not a revision", s)
		}
		sha, ok := revParse(repo, s)
		if !ok {
			return "", fmt.Errorf("--since %q is not a commit in %s", s, opts.SyncDir)
		}
		return sha, nil
	}
	m := opts.Marker
	b, err := os.ReadFile(m)
	switch {
	case errors.Is(err, fs.ErrNotExist) && opts.UpdateMarker:
		return "", nil
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("marker %s does not exist", m)
	case err != nil:
		return "", fmt.Errorf("read marker: %w", err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "-") {
		return "", fmt.Errorf("marker %s does not contain a commit", m)
	}
	sha, ok := revParse(repo, line)
	switch {
	case ok:
		return sha, nil
	case opts.UpdateMarker:
		return "", nil
	}
	return "", fmt.Errorf("marker %s names %q, which is not a commit in %s", m, line, opts.SyncDir)
}

func markerExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// busy reports whether the daemon's pull --rebase (or a merge or cherry-pick)
// is stopped half-way, when HEAD is not a meaningful point to diff against.
func busy(repo *gitwt.Repo) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD"} {
		out, err := repo.Run("rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		p := strings.TrimSpace(out)
		if !filepath.IsAbs(p) {
			p = filepath.Join(repo.Dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// revParse resolves rev to a full commit SHA.
func revParse(repo *gitwt.Repo, rev string) (string, bool) {
	out, err := repo.Run("rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// WriteMarker replaces the marker atomically: a half-written marker would
// make the next run fail, so write a temp file beside it and rename.
func WriteMarker(path, sha string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("write marker: %w", err)
		}
	}()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".marker-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(sha + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

var driveName = regexp.MustCompile(`^[A-Za-z]--`)

// projectFolder maps a --project value to Claude's folder name: a value that
// already looks like one is used as is, anything else is treated as a
// directory and escaped the way Claude Code does it.
func projectFolder(p string) string {
	if !strings.ContainsAny(p, `/\`) && (strings.HasPrefix(p, "-") || driveName.MatchString(p)) {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return escapePath(p)
}

// escapePath replaces every non-alphanumeric character with '-', which is how
// Claude Code derives a project's folder name from its path.
func escapePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

var statusNames = map[byte]string{'A': "added", 'M': "changed", 'T': "changed", 'D': "deleted"}

// parse reads `git diff --name-status -z` output, keeping only
// projects/<folder>/memory/<file> entries.
func parse(out, claudeDir string) []Change {
	var changes []Change
	f := strings.Split(out, "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] == "" {
			break
		}
		status, ok := statusNames[f[i][0]]
		path := f[i+1]
		parts := strings.Split(path, "/")
		if !ok || len(parts) != 4 || parts[0] != "projects" || parts[2] != "memory" || strings.HasPrefix(parts[3], ".") {
			continue
		}
		changes = append(changes, Change{
			Status:  status,
			Project: parts[1],
			File:    parts[3],
			Path:    path,
			Local:   filepath.Join(claudeDir, parts[1], "memory", parts[3]),
		})
	}
	return changes
}

// Text renders the result for a person or for pasting into a prompt.
func (r Result) Text() string {
	short := func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	}
	var b strings.Builder
	if r.Busy {
		b.WriteString("sync repo is busy (rebase or merge in progress); nothing reported\n")
		return b.String()
	}
	if r.Total == 0 {
		fmt.Fprintf(&b, "no memory changes since %s (head %s)\n", short(r.Since), short(r.Head))
		return b.String()
	}
	noun := "files"
	if r.Total == 1 {
		noun = "file"
	}
	fmt.Fprintf(&b, "%d memory %s changed since %s (head %s)\n", r.Total, noun, short(r.Since), short(r.Head))
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "  %-7s  %s\n", c.Status, c.Display())
	}
	if r.More > 0 {
		fmt.Fprintf(&b, "  ...%d more\n", r.More)
	}
	return b.String()
}
