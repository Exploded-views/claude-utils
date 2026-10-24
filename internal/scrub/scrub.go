// Package scrub decides whether a memory file looks like it holds a secret.
//
// The sync repo is a private git remote, but a private remote is still not a
// place for credentials: history is forever, and a memory file is easy to
// paste something into by accident. Before the daemon commits mirrored files
// it asks a Scanner about each one. A file that trips a built-in rule, or a
// pattern from the user's denylist, is left out of the commit.
//
// Scan reports rule names, never the matched text, so callers can log the
// result without writing a secret into a log file.
package scrub

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type rule struct {
	name string
	re   *regexp.Regexp
}

// builtin is the always-on rule set. The patterns favor few false positives on
// ordinary prose over catching every conceivable credential format: a memory
// file is mostly English, and a scrub that holds back harmless notes trains
// users to turn it off.
var builtin = []rule{
	{"private-key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{"github-token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{"anthropic-api-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	// Hyphens are only allowed after the sk-proj- prefix, so kebab-case prose
	// that happens to start with "sk-" is not flagged.
	{"openai-api-key", regexp.MustCompile(`\bsk-(?:proj-[A-Za-z0-9_-]{20,}|[A-Za-z0-9]{20,})`)},
	{"slack-token", regexp.MustCompile(`\bxox[abdoprs]-[A-Za-z0-9-]{10,}`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	// password/token/secret/api_key assigned a quoted value of 16+ characters
	// with no whitespace. Unquoted values are ignored: prose like "the token
	// is rotated weekly" is far more common than an unquoted secret.
	{"credential-assignment", regexp.MustCompile(`(?i)(?:password|passwd|token|secret|api[_-]?key)\w*["']?\s*[:=]\s*["'][^"'\s]{16,}["']`)},
}

// DenylistPattern is one user-supplied regex and the line it came from.
type DenylistPattern struct {
	Line int
	re   *regexp.Regexp
}

// ParseDenylist reads one case-insensitive regex per line. Blank lines and
// lines starting with "#" are skipped. A bad pattern is an error naming the
// line (not the pattern: it may itself be sensitive); callers should treat
// that as "cannot vouch for anything" rather than skipping the line.
func ParseDenylist(r io.Reader) ([]DenylistPattern, error) {
	var out []DenylistPattern
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile("(?i)" + line)
		if err != nil {
			return nil, fmt.Errorf("denylist line %d: invalid regular expression", n)
		}
		out = append(out, DenylistPattern{Line: n, re: re})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Scanner applies the built-in rules plus an optional denylist.
type Scanner struct {
	deny []DenylistPattern
}

// New returns a Scanner. deny may be nil.
func New(deny []DenylistPattern) *Scanner { return &Scanner{deny: deny} }

// Scan returns the name of every rule that matches content, each at most once,
// in a stable order (built-in rules first, then denylist lines). Denylist
// rules are named "denylist:<line>". The result never contains matched text.
func (s *Scanner) Scan(content []byte) []string {
	var hits []string
	for _, r := range builtin {
		if r.re.Match(content) {
			hits = append(hits, r.name)
		}
	}
	for _, d := range s.deny {
		if d.re.Match(content) {
			hits = append(hits, fmt.Sprintf("denylist:%d", d.Line))
		}
	}
	return hits
}
