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
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type rule struct {
	name string
	re   *regexp.Regexp
	// accept, if set, vets each match's submatches; the rule fires only if
	// one passes. It lets a rule exclude placeholders RE2 cannot express.
	accept func(sub [][]byte) bool
}

// tokenStart is what may precede a vendor token prefix. \b is no use: "_" is a
// word character, so it would miss "GITHUB_TOKEN_ghp_...".
const tokenStart = `(?:^|[^A-Za-z0-9])`

// builtin is the always-on rule set. The patterns favor few false positives on
// ordinary prose over catching every conceivable credential format: a memory
// file is mostly English, and a scrub that holds back harmless notes trains
// users to turn it off.
var builtin = []rule{
	{name: "private-key", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "github-token", re: regexp.MustCompile(tokenStart + `(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{name: "anthropic-api-key", re: regexp.MustCompile(tokenStart + `sk-ant-[A-Za-z0-9_-]{20,}`)},
	// Hyphens are only allowed after the sk-proj-, sk-svcacct- and sk-admin-
	// prefixes, so kebab-case prose that happens to start with "sk-" is not
	// flagged.
	{name: "openai-api-key", re: regexp.MustCompile(tokenStart + `sk-(?:(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}|[A-Za-z0-9]{20,})`)},
	{name: "stripe-key", re: regexp.MustCompile(tokenStart + `[sr]k_live_[A-Za-z0-9]{16,}`)},
	{name: "google-api-key", re: regexp.MustCompile(tokenStart + `AIza[0-9A-Za-z_-]{35}`)},
	{name: "gitlab-token", re: regexp.MustCompile(tokenStart + `glpat-[A-Za-z0-9_-]{20,}`)},
	{name: "slack-token", re: regexp.MustCompile(tokenStart + `xox[abdoprs]-[A-Za-z0-9-]{10,}`)},
	{name: "aws-access-key", re: regexp.MustCompile(tokenStart + `(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "jwt", re: regexp.MustCompile(tokenStart + `eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	// A 40-character value after aws_secret_access_key. A value starting with
	// "<" or "$" is a placeholder.
	{name: "aws-secret-key", re: regexp.MustCompile(`(?i)aws_secret_access_key["']?\s*[:=]\s*["']?[^\s<$"']\S{39}`)},
	// scheme://[user]:password@host. The username may be empty (redis://:pw@host).
	// The password class admits the bracket and shell characters placeholders
	// are made of, so accept can see the whole value and reject a placeholder
	// (${...}, <...>, {{...}}, ***, or a dummy word).
	{name: "url-credentials", re: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://([^\s:/@<>{}$*"']*):([^\s:/@"]{4,})@`), accept: realURLPassword},
	// password/token/secret/api_key assigned a quoted value of 16+ characters
	// with no whitespace. The name may carry only a few suffixes (_key,
	// _value, ...): "tokenizer", "secretName" and "token_type" hold ordinary
	// config words that are long enough to look like secrets. Unquoted values
	// are ignored: prose like "the token is rotated weekly" is far more common
	// than an unquoted secret. A value starting with "<", "$" or "{" is a
	// placeholder (<token>, ${TOKEN}, {{ token }}).
	{name: "credential-assignment", re: regexp.MustCompile(`(?i)(?:password|passwd|token|secret|api[_-]?key)(?:[_-]?(?:access[_-]?key|key|value|string|str|data))?["']?\s*[:=]\s*["']([^"'\s]{16,})["']`), accept: realCredentialValue},
}

// placeholderPasswords are the dummy passwords documentation uses.
var placeholderPasswords = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true, "secret": true,
	"changeme": true, "example": true, "redacted": true, "yourpassword": true,
	"your_password": true, "your-password": true, "mypassword": true,
	"xxxx": true, "xxxxx": true, "xxxxxx": true, "xxxxxxxx": true,
}

// placeholderValue matches a value that is wholly a template placeholder:
// ${NAME}, <name>, {{name}} or a run of asterisks.
var placeholderValue = regexp.MustCompile(`^(?:\$\{[^}]*\}|<[^>]*>|\{\{.*\}\}|\*+)$`)

// realURLPassword reports whether sub[2], the password in a URL's userinfo,
// is not an obvious placeholder.
func realURLPassword(sub [][]byte) bool {
	pw := sub[2]
	return !placeholderValue.Match(pw) && !placeholderPasswords[strings.ToLower(string(pw))]
}

// realCredentialValue reports whether sub[1], a quoted credential value, does
// not start like a placeholder.
func realCredentialValue(sub [][]byte) bool {
	return len(sub[1]) > 0 && strings.IndexByte("<${", sub[1][0]) < 0
}

// DenylistPattern is one user-supplied regex and the line it came from.
type DenylistPattern struct {
	Line int
	re   *regexp.Regexp
}

// ParseDenylist reads one case-insensitive regex per line. Blank lines and
// lines starting with "#" are skipped. A leading UTF-8 BOM is ignored (Windows
// editors add one). UTF-16 input, which editors on Windows also produce, would
// parse as garbage patterns that match nothing, so it is an error. A bad
// pattern is an error naming the line (not the pattern: it may itself be
// sensitive); callers should treat any error as "cannot vouch for anything"
// rather than skipping the line.
func ParseDenylist(r io.Reader) ([]DenylistPattern, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("denylist is not UTF-8 (looks like UTF-16 or binary); re-save the file as UTF-8")
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})

	var out []DenylistPattern
	sc := bufio.NewScanner(bytes.NewReader(data))
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
		if r.matches(content) {
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

func (r rule) matches(content []byte) bool {
	if r.accept == nil {
		return r.re.Match(content)
	}
	for _, sub := range r.re.FindAllSubmatch(content, -1) {
		if r.accept(sub) {
			return true
		}
	}
	return false
}

// MatchesPath reports whether any denylist pattern matches the file path
// itself. The built-in rules are about contents and are not applied to names.
// Callers must not log a path for which this is true: the name may be exactly
// what the user listed as sensitive.
func (s *Scanner) MatchesPath(path string) bool {
	for _, d := range s.deny {
		if d.re.MatchString(path) {
			return true
		}
	}
	return false
}
