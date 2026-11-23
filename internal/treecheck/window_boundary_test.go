package treecheck

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

const wailsApplicationImport = `"github.com/wailsapp/wails/v3/pkg/application"`

// windowBoundaryCheck keeps every reveal path on uiwindow.Reveal. Wails'
// Window.Restore un-maximises a maximized window (that is its documented
// job), which is how clicking an OS notification used to shrink a
// maximized app window to its normal size. No production file that imports
// the Wails application package may call `.Restore()` with no arguments;
// Reveal is the one way to bring a window forward. Creation likewise goes
// through uiwindow.New, which prevents unused window-event JavaScript from
// clearing clipboard user activation.
func windowBoundaryCheck() goFileCheck {
	var offenders []string
	return goFileCheck{
		name: "WindowCallsUseUIWindowBoundary",
		visit: func(fset *token.FileSet, path string, file *ast.File) {
			if strings.HasSuffix(path, "_test.go") || !importsPath(file, wailsApplicationImport) {
				return
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				badRestore := sel.Sel.Name == "Restore" && len(call.Args) == 0
				owner, _ := sel.X.(*ast.SelectorExpr)
				badCreation := sel.Sel.Name == "NewWithOptions" && owner != nil && owner.Sel.Name == "Window" && path != "internal/uiwindow/new.go"
				if !badRestore && !badCreation {
					return true
				}
				pos := fset.Position(call.Pos())
				offenders = append(offenders, fmt.Sprintf("%s:%d:%d", path, pos.Line, pos.Column))
				return true
			})
		},
		report: func(t *testing.T) {
			if len(offenders) > 0 {
				t.Fatalf("Shell windows must be created through uiwindow.New and revealed through uiwindow.Reveal:\n  %s", strings.Join(offenders, "\n  "))
			}
		},
	}
}

// importsPath reports whether file imports quoted, an import path as its
// source spells it.
func importsPath(file *ast.File, quoted string) bool {
	for _, spec := range file.Imports {
		if spec.Path.Value == quoted {
			return true
		}
	}
	return false
}
