package gitwt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// Every commit on a sync, related or registry repo goes through CommitGuarded
// or CommitPath, which refuse a per-PC path or a link. This walks the syntax of
// every non-test Go file outside this package and flags any string literal that
// names a git subcommand which makes commits (commit, commit-tree, cherry-pick,
// revert, am, merge), wherever it appears: a Run argument, an argument slice, a
// multi-line call, exec.Command, RunEnv, a leading -c. A call site that is safe
// (a merge that only fast-forwards, a commit built from HEAD's own tree) carries
// a `lint:allow-commit: <reason>` comment on its line or the line above. The
// marker is matched as a whole word and must carry a reason; the (file, word)
// pairs it excuses are pinned in allowedCommitWords, so a new excuse is a
// reviewed change to this test.
func TestNoPlainCommitOutsideGitwt(t *testing.T) {
	root, err := filepath.Abs(filepath.Join(testenv.SrcDir(), "../.."))
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	words := map[string]bool{"commit": true, "commit-tree": true, "cherry-pick": true, "revert": true, "am": true, "merge": true}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor" || p == filepath.Join(root, "internal", "gitwt")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		allowed := map[int]bool{}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				l := fset.Position(c.Pos()).Line
				switch m := allowMarker.FindStringSubmatch(c.Text); {
				case m == nil && strings.Contains(c.Text, "lint:allow-commit"):
					rel, _ := filepath.Rel(root, p)
					t.Errorf("%s:%d: lint:allow-commit needs the form `lint:allow-commit: <reason>`", rel, l)
				case m != nil && len(strings.Fields(m[1])) < 2:
					rel, _ := filepath.Rel(root, p)
					t.Errorf("%s:%d: lint:allow-commit needs a reason of at least two words", rel, l)
				case m != nil:
					allowed[l], allowed[l+1] = true, true
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if ce, ok := n.(*ast.CallExpr); ok && isCommitCall(ce) {
				se := ce.Fun.(*ast.SelectorExpr)
				rel, _ := filepath.Rel(root, p)
				if line := fset.Position(se.Sel.Pos()).Line; !allowed[line] {
					t.Errorf("%s:%d: a plain .Commit; use CommitGuarded or CommitPath", rel, line)
				} else {
					used[filepath.ToSlash(rel)+" Commit"] = true
				}
				return true
			}
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(bl.Value)
			if err != nil || !words[v] {
				return true
			}
			rel, _ := filepath.Rel(root, p)
			if line := fset.Position(bl.Pos()).Line; !allowed[line] {
				t.Errorf("%s:%d: %q can make a commit outside gitwt; use CommitGuarded or CommitPath, or justify with a lint:allow-commit comment", rel, line, v)
			} else {
				used[filepath.ToSlash(rel)+" "+v] = true
			}
			return true
		})
		return nil
	})
	var got []string
	for k := range used {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(allowedCommitWords, "\n") {
		t.Errorf("the lint:allow-commit excuses changed.\n got: %q\nwant: %q\n(a new excuse needs a review: update allowedCommitWords)", got, allowedCommitWords)
	}
}

// allowMarker is the whole-word marker with its reason: `lint:allow-commit:`
// and the text after it. `lint:allow-commit-x` or a bare marker do not match.
var allowMarker = regexp.MustCompile(`(?:^|[^\w-])lint:allow-commit:[ \t]*(.*)$`)

// allowedCommitWords are the (file, word) pairs the lint:allow-commit comments
// excuse, sorted.
var allowedCommitWords = []string{
	"internal/agecrypt/join.go merge",
	"internal/agecrypt/recipients.go commit-tree",
	"internal/agecrypt/rekey_state.go commit",
	"internal/version/version.go commit",
}

// isCommitCall reports a call of the form x.Commit(...).
func isCommitCall(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	return ok && se.Sel.Name == "Commit"
}
