package gitwt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// Every git process memsync starts, in the hook and in the daemon, goes through
// Command in safe.go, which pins each repo-local setting that can run a program
// or redirect traffic. This walks the syntax of every non-test Go file and
// flags a call of exec.Command, exec.CommandContext, proc.Command,
// proc.CommandContext or proc.GroupedCommandContext whose program is git:
//   - the literal "git", or any string literal "git" among its arguments;
//   - a program that is a name or a field spelled like git (gitBin, GitExe).
//
// Only the helper file may spawn git. A program held in an unrelated variable
// is beyond a syntax walk; the review of a new spawn site covers that.
func TestNoGitExecOutsideTheHelper(t *testing.T) {
	root, err := filepath.Abs(filepath.Join(testenv.SrcDir(), "../.."))
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "internal", "gitwt", "safe.go")
	spawners := map[string]map[string]bool{
		"exec": {"Command": true, "CommandContext": true},
		"proc": {"Command": true, "CommandContext": true, "GroupedCommandContext": true},
	}
	sites := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			se, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := se.X.(*ast.Ident)
			if !ok || !spawners[pkg.Name][se.Sel.Name] || !namesGit(ce) {
				return true
			}
			sites++
			if p != helper {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s:%d: %s.%s starts git outside internal/gitwt/safe.go; use gitwt.Command, which pins the repo-local settings that run programs", rel, fset.Position(ce.Pos()).Line, pkg.Name, se.Sel.Name)
			}
			return true
		})
		return nil
	})
	if sites == 0 {
		t.Error("no git spawn found anywhere: the walk is broken, or the helper does not spawn git")
	}
}

// namesGit reports whether a spawn call's program (its first argument, or the
// second for a Context variant) is git, by literal or by name, or a string
// literal "git" appears among its arguments.
func namesGit(ce *ast.CallExpr) bool {
	for _, a := range ce.Args {
		if bl, ok := a.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			if v, err := strconv.Unquote(bl.Value); err == nil && v == "git" {
				return true
			}
		}
	}
	prog := 0
	if se, ok := ce.Fun.(*ast.SelectorExpr); ok && strings.HasSuffix(se.Sel.Name, "Context") && len(ce.Args) > 1 {
		prog = 1
	}
	if prog >= len(ce.Args) {
		return false
	}
	var name string
	switch a := ce.Args[prog].(type) {
	case *ast.Ident:
		name = a.Name
	case *ast.SelectorExpr:
		name = a.Sel.Name
	}
	return strings.HasPrefix(strings.ToLower(name), "git")
}
