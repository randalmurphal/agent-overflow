package treecheck

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// jiraDevDataNeedles name Jira's undocumented development-data endpoints
// and the project permission that gates them. The app reads pull requests
// and their reviews from the forge instead, so no Go string, the fake
// tracker's included, may name either.
var jiraDevDataNeedles = []string{"rest/dev-status", "VIEW_DEV_TOOLS"}

// jiraDevDataCheck fails on any string literal outside this package that
// names a needle.
func jiraDevDataCheck() goFileCheck {
	var offenders []string
	return goFileCheck{
		name: "NothingReadsJiraDevelopmentData",
		visit: func(fset *token.FileSet, path string, file *ast.File) {
			if strings.HasPrefix(path, "internal/treecheck/") {
				return
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, needle := range jiraDevDataNeedles {
					if strings.Contains(lit.Value, needle) {
						pos := fset.Position(lit.Pos())
						offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, pos.Line, needle))
					}
				}
				return true
			})
		},
		report: func(t *testing.T) {
			if len(offenders) > 0 {
				t.Fatalf("Jira's development data is not read; pull requests come from the forge:\n  %s", strings.Join(offenders, "\n  "))
			}
		},
	}
}
