package agecrypt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// IdentityConfigKey is the repo-local git config key holding the identity path.
const IdentityConfigKey = "memage.identity"

// FilterName is the git filter/diff driver name used in .gitattributes.
const FilterName = "memage"

// Opts says which binaries and key a clone uses.
type Opts struct {
	MemsyncExe   string // claude-memsync, runs the filter and textconv
	MemmergeExe  string // claude-memmerge, runs the merge drivers
	IdentityPath string // this host's age identity
}

// ShellQuote quotes a path for the POSIX sh that git runs configured commands
// with (git for Windows bundles one). Backslashes become slashes first, which
// Windows executables accept, and the path is single-quoted with embedded
// single quotes escaped, so spaces, $, & and the rest are literal.
func ShellQuote(p string) string {
	p = filepath.ToSlash(p)
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// MergeCommand is the value of merge.<name>.driver. Git expands % sequences in
// it before the shell sees it, so a literal % in the program path is doubled;
// the placeholders that follow are left for git.
func MergeCommand(exe string, args ...string) string {
	q := strings.ReplaceAll(ShellQuote(exe), "%", "%%")
	if len(args) > 0 {
		q += " " + strings.Join(args, " ")
	}
	return q + " %O %A %B %L %P"
}

func quote(p string) string { return ShellQuote(p) }

// Configure sets the repo-local git config encryption needs: the filter
// (required, so a missing binary fails closed), textconv, the identity path and
// both merge drivers. Call it before the first checkout.
func Configure(repo *gitwt.Repo, o Opts) error {
	kv := [][2]string{
		{"filter." + FilterName + ".process", quote(o.MemsyncExe) + " age-filter"},
		{"filter." + FilterName + ".required", "true"},
		{"diff." + FilterName + ".textconv", quote(o.MemsyncExe) + " age-textconv"},
		{IdentityConfigKey, filepath.ToSlash(o.IdentityPath)},
		{"merge.claude-memory-index.name", "claude memory index merge"},
		{"merge.claude-memory-index.driver", MergeCommand(o.MemmergeExe)},
		{"merge.memtext.name", "decrypting text merge"},
		{"merge.memtext.driver", MergeCommand(o.MemmergeExe, "--text")},
	}
	for _, e := range kv {
		if err := repo.ConfigSet(e[0], e[1]); err != nil {
			return err
		}
	}
	// The rules live in $GIT_DIR/info/attributes, which outranks every
	// .gitattributes: an upstream or nested one cannot switch the filter off.
	if err := writeInfoAttributes(repo); err != nil {
		return err
	}
	// A neutral author, so commits do not carry the host's name. Only filled in
	// when the user has not configured one.
	if out, _ := repo.Run("config", "--get", "user.email"); strings.TrimSpace(out) == "" {
		if err := repo.ConfigSet("user.email", "memsync@localhost"); err != nil {
			return err
		}
		if err := repo.ConfigSet("user.name", "memsync"); err != nil {
			return err
		}
	}
	return nil
}

// Configured reports whether the clone has the age filter configured.
func Configured(repo *gitwt.Repo) bool {
	out, err := repo.Run("config", "--get", "filter."+FilterName+".process")
	return err == nil && strings.TrimSpace(out) != ""
}

// Enabled reports whether the repo is meant to be encrypted: the filter is
// configured, or the repo carries .age-recipients. The second test keeps a
// clone whose config went missing from being treated as a plain repo: it is
// held to the encrypted rules (and fails closed) instead.
func Enabled(repo *gitwt.Repo) bool {
	if Configured(repo) {
		return true
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, RecipientsFile)); err == nil {
		return true
	}
	_, err := repo.Run("cat-file", "-e", "HEAD:"+RecipientsFile)
	return err == nil
}

// IdentityPath is the clone's configured identity file ("" if unset).
func IdentityPath(repo *gitwt.Repo) string {
	out, _ := repo.Run("config", "--get", IdentityConfigKey)
	return strings.TrimSpace(out)
}

// exempt lists the files that stay plaintext: git's own metadata and the
// public recipients.
var exempt = map[string]bool{".gitattributes": true, ".gitignore": true, RecipientsFile: true}

// Exempt reports whether a repo-root path is stored in the clear.
func Exempt(path string) bool { return exempt[path] }

// Attributes is the .gitattributes content of an encrypted repo.
func Attributes() string {
	return `# Managed by claude-memsync: every file is age-encrypted in the repo.
# Exempt (stored in the clear): this file, .gitignore and the public recipients.
* filter=memage diff=memage merge=memtext
MEMORY.md filter=memage diff=memage merge=claude-memory-index
/.gitattributes !filter !diff !merge
/.gitignore !filter !diff !merge
/.age-recipients !filter !diff merge=union
`
}

// writeInfoAttributes writes Attributes() to $GIT_DIR/info/attributes.
func writeInfoAttributes(repo *gitwt.Repo) error {
	out, err := repo.Run("rev-parse", "--git-path", "info/attributes")
	if err != nil {
		return err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo.Dir, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(Attributes()), 0o600)
}

// WriteAttributes writes Attributes() into the work-tree at dir.
func WriteAttributes(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(Attributes()), 0o600)
}

// WriteRecipients writes a new recipients file at dir.
func WriteRecipients(dir string, keys []string) error {
	var b strings.Builder
	b.WriteString("# Public age recipients of this memory repo. Public keys only.\n")
	for _, k := range keys {
		fmt.Fprintln(&b, strings.TrimSpace(k))
	}
	if _, err := ParseRecipients([]byte(b.String())); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, RecipientsFile), []byte(b.String()), 0o600)
}
