package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// threadRowFrameBuilder is the one file allowed to put a row on a
// thread:updated frame (app_thread_broadcast.go).
const threadRowFrameBuilder = "internal/triage/thread_announce.go"

// TestThreadRowFramesComeFromTheAnnouncer rejects a ThreadUpdateEvent with a
// Thread built anywhere but the announcer. A frame built from a row the
// writer holds can carry a row older than another writer's frame and undo
// that write on every client; the announcer reads the row as the frame
// leaves. These suites run from the repository root (see TestMain).
func TestThreadRowFramesComeFromTheAnnouncer(t *testing.T) {
	fset := token.NewFileSet()
	var found []string
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if filepath.ToSlash(path) == threadRowFrameBuilder {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				lit, ok := node.(*ast.CompositeLit)
				if !ok || !isThreadUpdateEventType(lit.Type) {
					return true
				}
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Thread" {
							found = append(found, fset.Position(lit.Pos()).String())
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(found) > 0 {
		t.Fatalf("thread:updated row frames built outside %s; announce the thread by id (announceThread, ThreadAnnouncer.Announce):\n%s",
			threadRowFrameBuilder, strings.Join(found, "\n"))
	}
}

func isThreadUpdateEventType(expr ast.Expr) bool {
	switch typ := expr.(type) {
	case *ast.Ident:
		return typ.Name == "ThreadUpdateEvent"
	case *ast.SelectorExpr:
		return typ.Sel.Name == "ThreadUpdateEvent"
	}
	return false
}
