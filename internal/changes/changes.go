// Package changes reports which memory files changed in the sync repo since a
// given point, so a Claude Code hook can tell a long-running session that the
// notes it read earlier are out of date.
//
// It only reads the sync repo's history. It does not fetch: the daemon keeps
// the repo current, and "changed" means "changed in what this PC has pulled or
// committed".
package changes

import (
	"bytes"
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

	// Upper, if it resolves to a commit, replaces HEAD as the newest commit
	// the report looks at (Result.Head is then Upper). The caller passes the
	// commit whose files were copied into ~/.claude, so a session is not told
	// about a file that is not there yet. Empty, or naming no commit: HEAD.
	Upper string

	// Hide drops every change it returns true for before the Limit applies:
	// the change is counted in Result.Hidden and never listed. HideAll hides
	// every change (the caller could not read the rules Hide would apply).
	Hide    func(Change) bool
	HideAll bool

	// Keep, if set, drops every change it returns false for before Hide and
	// Limit, without counting it: the change is not this caller's to report
	// (it belongs to a folder another repo owns).
	Keep func(Change) bool

	// CompareLocal sets Change.LocalKept on each listed change whose local
	// copy differs from the committed bytes. It costs one git process for all.
	CompareLocal bool

	// Limit caps how many changes are listed; the rest are only counted.
	// Zero means no cap.
	Limit int

	// UpdateMarker rewrites Marker to the current HEAD once the report is
	// built; it is only valid with Marker. A marker file that does not exist
	// yet is created and nothing is reported: that run only sets the baseline.
	// An empty marker, or one holding a commit id that no longer exists, is
	// reset the same way and the result has Reset set. A file with anything
	// else in it is not a marker and is never overwritten.
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

	// LocalKept is set when the local copy exists and is not the committed
	// version: the daemon skipped the file (held back, or edited since), and
	// the incoming version was saved as .from-remote-* beside the mirror copy.
	LocalKept bool `json:"localKept,omitempty"`
}

// localKeptNote is appended to the line of a change whose local copy was kept.
const localKeptNote = "(the copy on this PC differs from the synced version: a local edit not yet synced, or an incoming change that was held back)"

// Describe is Display plus the note for a kept local copy.
func (c Change) Describe() string {
	if c.LocalKept {
		return c.Display() + " " + localKeptNote
	}
	return c.Display()
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
	Head    string   `json:"head"`    // the newest commit looked at: HEAD, or Options.Upper
	Since   string   `json:"since"`   // the commit Since or Marker resolved to
	Busy    bool     `json:"busy"`    // the repo is mid-rebase/merge; nothing was reported
	Reset   bool     `json:"reset"`   // the marker named a lost commit and was reset to Head
	Total   int      `json:"total"`   // number of changed files, not counting the hidden ones
	Hidden  int      `json:"hidden"`  // changes left out because Options.Hide or HideAll dropped them
	More    int      `json:"more"`    // changes not listed because of Limit
	Changes []Change `json:"changes"` // never nil

	// MovedBack: Head is an ancestor of Since (history moved back). The diff
	// is still reported once, and the marker must then follow Head back
	// (WriteMarker, not AdvanceMarker), or the same diff repeats on every run.
	MovedBack bool `json:"movedBack"`

	hideAll bool // Hidden counts everything because the rules were unusable
}

// SaveMarker moves the marker to res.Head after a report: back as well as
// forward when history moved back, otherwise never backwards (see
// AdvanceMarker), so two hooks racing cannot rewind each other.
func SaveMarker(repo *gitwt.Repo, path string, res Result) error {
	if res.MovedBack {
		return WriteMarker(path, res.Head)
	}
	return AdvanceMarker(repo, path, res.Head)
}

// HiddenLine is the one line that stands in for the hidden changes, or "".
func (r Result) HiddenLine() string {
	switch {
	case r.Hidden == 0:
		return ""
	case r.hideAll:
		return fmt.Sprintf("%d changed file(s) are not listed (the denylist could not be read)", r.Hidden)
	}
	return fmt.Sprintf("%d changed file(s) whose name matches your denylist are not listed", r.Hidden)
}

// Compute lists the memory files that changed between opts.Since and HEAD (or
// opts.Upper).
func Compute(opts Options) (Result, error) {
	repo := gitwt.New(opts.SyncDir)
	res := Result{Changes: []Change{}}

	head, ok := revParse(repo, "HEAD")
	if !ok {
		return res, fmt.Errorf("%s has no commits yet", opts.SyncDir)
	}
	if opts.Upper != "" {
		if up, ok := revParse(repo, opts.Upper); ok {
			head = up
		}
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
	if since != head {
		// The upper bound is behind where the report last got to: history
		// moved back (a reset, a force-push). The diff is what changed on disk
		// (a revert, say), so it is reported, once: the marker follows head
		// back below.
		if _, err := repo.Run("merge-base", "--is-ancestor", head, since); err == nil {
			res.MovedBack = true
		}
	}

	spec := ":(literal)projects/"
	if opts.Project != "" {
		spec = ":(literal)projects/" + projectFolder(opts.Project) + "/memory/"
	}
	out, err := repo.Run("diff", "--name-status", "--no-renames", "-z", since, head, "--", spec)
	if err != nil {
		return res, err
	}
	all := parse(out, opts.ClaudeDir)
	if opts.Keep != nil {
		kept := all[:0]
		for _, c := range all {
			if opts.Keep(c) {
				kept = append(kept, c)
			}
		}
		all = kept
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })

	res.hideAll = opts.HideAll
	if opts.HideAll || opts.Hide != nil {
		shown := all[:0]
		for _, c := range all {
			if opts.HideAll || opts.Hide(c) {
				res.Hidden++
			} else {
				shown = append(shown, c)
			}
		}
		all = shown
	}

	res.Total = len(all)
	res.Changes = all
	if opts.Limit > 0 && len(all) > opts.Limit {
		res.Changes = all[:opts.Limit]
		res.More = len(all) - opts.Limit
	}
	if res.Changes == nil {
		res.Changes = []Change{}
	}
	if opts.CompareLocal {
		markLocalKept(repo, head, res.Changes)
	}
	if opts.UpdateMarker && !opts.DeferMarker {
		if err := SaveMarker(repo, opts.Marker, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// markLocalKept sets LocalKept on the non-deleted changes whose local file
// differs from the blob at commit. All blobs come from one `git cat-file
// --batch`. It is best effort: on an error nothing is flagged.
func markLocalKept(repo *gitwt.Repo, commit string, list []Change) {
	var idx []int
	var specs []string
	var local [][]byte
	for i, c := range list {
		// A newline in a name would split the batch request.
		if c.Status == "deleted" || strings.ContainsAny(c.Path, "\n") {
			continue
		}
		b, err := os.ReadFile(c.Local)
		if err != nil {
			continue
		}
		idx = append(idx, i)
		specs = append(specs, commit+":"+c.Path)
		local = append(local, b)
	}
	blobs, err := repo.BlobContents(specs)
	if err != nil {
		return
	}
	for k, i := range idx {
		list[i].LocalKept = !bytes.Equal(blobs[k], local[k])
	}
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
	line := markerLine(b)
	if line == "" {
		if opts.UpdateMarker {
			return "", nil // empty, e.g. made with touch
		}
		return "", fmt.Errorf("marker %s does not contain a commit id", m)
	}
	// Anything that is not a commit id is not ours to overwrite, and is not
	// echoed: it may be a note the user pointed --marker at by mistake.
	if !commitID.MatchString(line) {
		return "", fmt.Errorf("marker %s does not contain a commit id", m)
	}
	sha, ok := revParse(repo, line)
	switch {
	case ok:
		return sha, nil
	case opts.UpdateMarker:
		return "", nil
	}
	return "", fmt.Errorf("marker %s names a commit that is not in %s", m, opts.SyncDir)
}

var commitID = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// markerLine returns the first line of a marker file, trimmed.
func markerLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

func markerExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// busy reports whether the daemon's pull --rebase (or a merge or cherry-pick)
// is stopped half-way, when HEAD is not a meaningful point to diff against.
func busy(repo *gitwt.Repo) bool {
	names := []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD"}
	args := []string{"rev-parse"}
	for _, name := range names {
		args = append(args, "--git-path", name)
	}
	out, err := repo.Run(args...)
	if err != nil {
		return false
	}
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		if p == "" {
			continue
		}
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

// AdvanceMarker writes head to the marker unless the marker already names a
// commit that head is an ancestor of, i.e. unless that would move it
// backwards (the repo was reset to an older commit): the changes in between
// were reported already.
func AdvanceMarker(repo *gitwt.Repo, path, head string) error {
	if b, err := os.ReadFile(path); err == nil {
		if line := markerLine(b); commitID.MatchString(line) {
			if cur, ok := revParse(repo, line); ok && cur != head {
				if _, err := repo.Run("merge-base", "--is-ancestor", head, cur); err == nil {
					return nil
				}
			}
		}
	}
	return WriteMarker(path, head)
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
		if r.Hidden > 0 {
			fmt.Fprintf(&b, "%s (since %s, head %s)\n", r.HiddenLine(), short(r.Since), short(r.Head))
			return b.String()
		}
		fmt.Fprintf(&b, "no memory changes since %s (head %s)\n", short(r.Since), short(r.Head))
		return b.String()
	}
	noun := "files"
	if r.Total == 1 {
		noun = "file"
	}
	fmt.Fprintf(&b, "%d memory %s changed since %s (head %s)\n", r.Total, noun, short(r.Since), short(r.Head))
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "  %-7s  %s\n", c.Status, c.Describe())
	}
	if r.More > 0 {
		fmt.Fprintf(&b, "  ...%d more\n", r.More)
	}
	if r.Hidden > 0 {
		fmt.Fprintf(&b, "  %s\n", r.HiddenLine())
	}
	return b.String()
}
