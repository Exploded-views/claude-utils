package stoprec

import (
	"regexp"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/scrub"
)

const pathRedacted = "<path redacted>"

// pathLike is a token that looks like a path, for when no denylist can be
// trusted.
var pathLike = regexp.MustCompile(`\S*[/\\]\S*`)

// ScrubTail makes journal lines safe to keep. A line that trips a built-in
// scrub rule is replaced whole by "[line withheld: <rule>]" (masking the match
// alone would leave the rest of a token line to identify it), and counted. In
// the other lines, every span the scanner's denylist matches becomes
// "<path redacted>". A nil scanner means the denylist exists but could not be
// read: then no path can be vouched for, and every path-like token is
// redacted. The result never contains matched text, and neither do the rule
// names.
func ScrubTail(lines []string, sc *scrub.Scanner) (kept []string, withheld int) {
	closed := sc == nil
	if closed {
		sc = scrub.New(nil)
	}
	kept = make([]string, 0, len(lines))
	for _, l := range lines {
		if rule := firstBuiltin(sc.Scan([]byte(l))); rule != "" {
			kept = append(kept, "[line withheld: "+rule+"]")
			withheld++
			continue
		}
		if closed {
			l = pathLike.ReplaceAllString(l, pathRedacted)
		} else {
			l = redactSpans(l, sc.PathSpans(l))
		}
		kept = append(kept, l)
	}
	return kept, withheld
}

// firstBuiltin is the first rule that is not a denylist line (those are
// handled as paths).
func firstBuiltin(hits []string) string {
	for _, h := range hits {
		if !strings.HasPrefix(h, "denylist:") {
			return h
		}
	}
	return ""
}

func redactSpans(s string, spans [][2]int) string {
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp[1] <= pos {
			continue
		}
		if sp[0] > pos {
			b.WriteString(s[pos:sp[0]])
			pos = sp[0]
		}
		b.WriteString(pathRedacted)
		pos = sp[1]
	}
	b.WriteString(s[pos:])
	return b.String()
}
