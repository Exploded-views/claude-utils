package related

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EncryptedMarker is the file an age-encrypted repo carries.
const EncryptedMarker = ".age-recipients"

// Opener reads a clone's files. It is the hook for decryption: age lives on
// another branch, which supplies an Opener that can decrypt with this host's
// key. The default cannot, so an encrypted clone is "not readable on this
// host" and skipped.
type Opener interface {
	// Readable reports whether the clone at dir can be read on this host.
	Readable(dir string) bool
	// ReadFile returns the plaintext of the file at path.
	ReadFile(path string) ([]byte, error)
}

// PlainOpener reads plaintext clones only.
type PlainOpener struct{}

// Readable is false when the clone is encrypted.
func (PlainOpener) Readable(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, EncryptedMarker))
	return err != nil
}

// ReadFile reads the file as it is.
func (PlainOpener) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

// Index is what a session is told about a related repo: how many notes it
// has and the headings of its MEMORY.md files. Never a note body.
type Index struct {
	Notes    int
	Headings []string
}

// maxHeading caps one heading's length.
const maxHeading = 120

// BuildIndex reads the clone at dir. Archived notes and the .git dir are
// ignored; MEMORY.md and README.md are indexes, not notes.
func BuildIndex(dir string, o Opener) (Index, error) {
	var ix Index
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel == ".git" || rel == "archive" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") || !d.Type().IsRegular() {
			return nil
		}
		switch d.Name() {
		case "README.md":
		case "MEMORY.md":
			b, err := o.ReadFile(p)
			if err != nil {
				return err
			}
			ix.Headings = append(ix.Headings, headings(b)...)
		default:
			ix.Notes++
		}
		return nil
	})
	return ix, err
}

func headings(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, cleanLine(line, maxHeading))
	}
	return out
}

// cleanLine makes s safe to put in a prompt: printable runes only, one line,
// at most max runes.
func cleanLine(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if !strconv.IsPrint(r) {
			r = ' '
		}
		if n == max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// Block is one relation as the context shows it.
type Block struct {
	Repo    string
	Path    string // read-only local path
	Index   Index
	Skipped string // non-empty: why there is no index ("not readable on this host", ...)
}

// Budgets caps the context, in characters (runes).
type Budgets struct {
	PerRelation int
	Total       int
}

// DefaultBudgets are 2000 per relation and 6000 in all.
var DefaultBudgets = Budgets{PerRelation: 2000, Total: 6000}

const contextHeader = "Related memory (read-only; never write there, names and headings only; read a note when you need its text):"

// Render builds the context text for blocks within the budgets. Empty when
// there are no blocks.
func Render(blocks []Block, b Budgets) string {
	if len(blocks) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString(contextHeader + "\n")
	used := utf8.RuneCountInString(contextHeader) + 1
	for i, bl := range blocks {
		text := renderBlock(bl, b.PerRelation)
		if b.Total > 0 && used+utf8.RuneCountInString(text) > b.Total {
			room := b.Total - used
			if room > 40 {
				out.WriteString(truncate(text, room) + "\n")
			}
			fmt.Fprintf(&out, "(%d more related repo(s) left out: context budget)\n", len(blocks)-i-boolInt(room > 40))
			break
		}
		out.WriteString(text + "\n")
		used += utf8.RuneCountInString(text) + 1
	}
	return out.String()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func renderBlock(bl Block, max int) string {
	var s strings.Builder
	switch {
	case bl.Skipped != "":
		fmt.Fprintf(&s, "- %s: %s", bl.Repo, bl.Skipped)
	default:
		fmt.Fprintf(&s, "- %s, %d %s, read-only at %s", bl.Repo, bl.Index.Notes, plural(bl.Index.Notes), bl.Path)
		for _, h := range bl.Index.Headings {
			s.WriteString("\n    " + h)
		}
	}
	return truncate(s.String(), max)
}

func plural(n int) string {
	if n == 1 {
		return "note"
	}
	return "notes"
}

// truncate cuts s to max runes, marking the cut.
func truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	if max < 2 {
		return string(r[:max])
	}
	return strings.TrimRight(string(r[:max-1]), " \n") + "…"
}
