package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The language an agent is answered in is a contract, so it is decided once: every
// string that reaches the model (tool descriptions, envelope values, notes) is
// English. The Chinese in this repo lives in prose -- docs/ and code comments --
// which the model never sees. Notes were the only place that used to answer in
// Chinese, and prose is exactly where wording drifts back, so this pins the rule at
// the source instead of relying on review.
//
// It scans string literals in every non-test file of the package (comments are free
// to be Chinese), so a new file is covered without maintaining a list. Test fixtures
// stay out: a message body of "漢漢漢" is data about rune boundaries, not our wording.
func TestLLMFacingLiteralsAreEnglish(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	scanned, seen := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				v = lit.Value // raw string: scan the token as written
			}
			seen++
			for _, r := range v {
				if unicode.Is(unicode.Han, r) {
					t.Errorf("%s:%d: string literal %q is not English -- text the model reads stays English (a comment is the right home for Chinese prose)",
						name, fset.Position(lit.Pos()).Line, v)
					return true
				}
			}
			return true
		})
	}
	if scanned < 4 || seen == 0 {
		t.Fatalf("the scan found almost nothing (%d files, %d literals): it is broken, not passing", scanned, seen)
	}
}
