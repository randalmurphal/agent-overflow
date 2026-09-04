package uiwindow

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goSourceRoots mirrors GO_PACKAGE_ROOTS in the Makefile, relative to the
// repository root (this package sits two levels below it).
var goSourceRoots = []string{".", "cmd", "internal"}

const wailsApplicationImport = `"github.com/wailsapp/wails/v3/pkg/application"`

// TestNoWindowRestoreOutsideReveal keeps every reveal path on Reveal. Wails'
// Window.Restore un-maximises a maximized window (that is its documented job),
// which is how clicking an OS notification used to shrink a maximized app
// window to its normal size. No file that imports the Wails application
// package may call `.Restore()` with no arguments; Reveal is the one way to
// bring a window forward.
func TestNoWindowRestoreOutsideReveal(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	for _, sub := range goSourceRoots {
		dir := filepath.Join(root, sub)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != dir && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "frontend" || (sub == "." && (name == "cmd" || name == "internal"))) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.Contains(string(src), wailsApplicationImport) {
				return nil
			}
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Restore" {
					return true
				}
				rel, _ := filepath.Rel(root, path)
				pos := fset.Position(call.Pos())
				offenders = append(offenders, fmt.Sprintf("%s:%d:%d", rel, pos.Line, pos.Column))
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("Window.Restore() un-maximises a maximized window; reveal through uiwindow.Reveal instead:\n  %s", strings.Join(offenders, "\n  "))
	}
}
