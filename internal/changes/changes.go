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
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Options selects what Compute reports.
type Options struct {
	SyncDir   string // the sync work-tree (a git repo)
	ClaudeDir string // ~/.claude/projects, used to build each change's local path

	// Since is a git revision (SHA, abbreviated SHA, branch, HEAD~3, ...) or
	// the path of a marker file whose first line is one.
	Since string

	// Project limits the report to one project: a project directory such as
	// /home/alice/src/app, or Claude's escaped folder name for it.
	Project string

	// Limit caps how many changes are listed; the rest are only counted.
	// Zero means no cap.
	Limit int

	// UpdateMarker rewrites the marker file named by Since to the current HEAD
	// once the report is built. A marker file that does not exist yet is
	// created and nothing is reported: that run only sets the baseline.
	UpdateMarker bool
}

// Change is one memory file that differs between Since and HEAD.
type Change struct {
	Status  string `json:"status"`  // "added", "changed" or "deleted"
	Project string `json:"project"` // Claude's folder name for the project
	File    string `json:"file"`    // file name inside the project's memory dir
	Path    string `json:"path"`    // path inside the sync repo
	Local   string `json:"local"`   // where Claude Code reads (or read) the file
}

// Result is what Compute returns and what --json prints.
type Result struct {
	Head    string   `json:"head"`    // current HEAD of the sync repo
	Since   string   `json:"since"`   // the commit Since resolved to
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

	since, isMarker, err := resolveSince(repo, opts)
	if err != nil {
		return res, err
	}
	if opts.UpdateMarker && !isMarker {
		return res, fmt.Errorf("--update-marker needs --since to be a marker file, not a revision")
	}
	if since == "" {
		// Marker file did not exist: this run only records the baseline.
		res.Since = head
		return res, writeMarker(opts.Since, head)
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
	if opts.UpdateMarker {
		if err := writeMarker(opts.Since, head); err != nil {
			return res, err
		}
	}
	return res, nil
}

// resolveSince turns opts.Since into a commit SHA. isMarker reports whether it
// came from a marker file. An empty SHA with a nil error means the marker file
// does not exist yet and opts.UpdateMarker is set.
func resolveSince(repo *gitwt.Repo, opts Options) (sha string, isMarker bool, err error) {
	s := opts.Since
	if s == "" {
		return "", false, errors.New("--since is required")
	}
	if strings.HasPrefix(s, "-") {
		return "", false, fmt.Errorf("--since %q is not a revision or marker file", s)
	}
	if sha, ok := revParse(repo, s); ok {
		return sha, false, nil
	}
	b, err := os.ReadFile(s)
	switch {
	case errors.Is(err, fs.ErrNotExist) && opts.UpdateMarker:
		return "", true, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", false, fmt.Errorf("--since %q is neither a commit in %s nor an existing marker file", s, opts.SyncDir)
	case err != nil:
		return "", false, fmt.Errorf("read marker: %w", err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "-") {
		return "", false, fmt.Errorf("marker %s does not contain a commit", s)
	}
	sha, ok := revParse(repo, line)
	if !ok {
		return "", false, fmt.Errorf("marker %s names %q, which is not a commit in %s", s, line, opts.SyncDir)
	}
	return sha, true, nil
}

// revParse resolves rev to a full commit SHA.
func revParse(repo *gitwt.Repo, rev string) (string, bool) {
	out, err := repo.Run("rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func writeMarker(path, sha string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	if err := os.WriteFile(path, []byte(sha+"\n"), 0600); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	return nil
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
		fmt.Fprintf(&b, "  %-7s  %s\n", c.Status, c.Local)
	}
	if r.More > 0 {
		fmt.Fprintf(&b, "  ...%d more\n", r.More)
	}
	return b.String()
}
