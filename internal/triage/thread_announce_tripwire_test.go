package triage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	eventchanPkgPath = "agent-overflow/internal/eventchan"
	triagePkgPath    = "agent-overflow/internal/triage"
	// threadFrameEmitter is the one file that emits thread:updated frames
	// and the one that may set a frame's row.
	threadFrameEmitter = "internal/triage/thread_announce.go"
)

// threadUpdatedNamers may name the thread:updated channel without emitting
// on it: its declaration and the channel registries.
var threadUpdatedNamers = []string{
	threadFrameEmitter,
	"internal/eventchan/channels.go",
	"internal/transport/event_channels.go",
	"cmd/ao-harness/channels.go",
}

// TestThreadUpdatedFramesComeFromTheAnnouncer type-checks every production
// package and rejects a thread:updated frame that does not leave through
// triage.ThreadAnnouncer: a reference to eventchan.ThreadUpdated (or a
// constant equal to its value) outside threadUpdatedNamers, and a write to
// ThreadUpdateEvent.Thread outside threadFrameEmitter, through any alias. A
// frame emitted on its own escapes the announce lock and can leave after a
// frame describing a later state, which undoes that state on every client.
func TestThreadUpdatedFramesComeFromTheAnnouncer(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := listPackagesWithExports(root)
	if err != nil {
		t.Fatal(err)
	}
	found, err := threadFrameViolations(root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("thread:updated frames emitted outside the announcer; announce through triage.ThreadAnnouncer (announceThread, announceThreadPatch, broadcastThreadDeleted):\n%s",
			strings.Join(found, "\n"))
	}
}

type listedPackage struct {
	ImportPath      string
	Dir             string
	Export          string
	CompiledGoFiles []string
	IgnoredGoFiles  []string
	Module          *struct{ Main bool }
	Error           *struct{ Err string }
}

// listPackagesWithExports lists the module's packages and their
// dependencies with compiled export data, so production packages can be
// type-checked from source against their imports' export data.
func listPackagesWithExports(root string) ([]listedPackage, error) {
	cmd := exec.Command("go", "list", "-json", "-export", "-compiled", "-deps", ".", "./cmd/...", "./internal/...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v\n%s", err, stderr.String())
	}
	var pkgs []listedPackage
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var pkg listedPackage
		if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode go list: %w", err)
		}
		if pkg.Error != nil {
			return nil, fmt.Errorf("go list %s: %s", pkg.ImportPath, pkg.Error.Err)
		}
		pkgs = append(pkgs, pkg)
	}
}

// threadFrameViolations checks every main-module package in pkgs. Files the
// default build excludes (other platforms, build tags) get a syntactic check.
func threadFrameViolations(root string, pkgs []listedPackage) ([]string, error) {
	exports := make(map[string]string, len(pkgs))
	for _, pkg := range pkgs {
		exports[pkg.ImportPath] = pkg.Export
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	var found []string
	checked := 0
	for _, pkg := range pkgs {
		if pkg.Module == nil || !pkg.Module.Main {
			continue
		}
		var files []*ast.File
		for _, name := range pkg.CompiledGoFiles {
			if !filepath.IsAbs(name) {
				name = filepath.Join(pkg.Dir, name)
			}
			file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			files = append(files, file)
		}
		info := &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{},
			Uses:  map[*ast.Ident]types.Object{},
		}
		conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", runtime.GOARCH)}
		if _, err := conf.Check(pkg.ImportPath, fset, files, info); err != nil {
			return nil, fmt.Errorf("type-check %s: %w", pkg.ImportPath, err)
		}
		checked++
		rel := func(pos token.Pos) string {
			name, err := filepath.Rel(root, fset.Position(pos).Filename)
			if err != nil {
				return fset.Position(pos).Filename
			}
			return filepath.ToSlash(name)
		}
		for _, file := range files {
			found = append(found, typedThreadFrameViolations(fset, file, info, rel)...)
		}
		for _, name := range pkg.IgnoredGoFiles {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(pkg.Dir, name)
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			found = append(found, syntacticThreadFrameViolations(fset, file, rel(file.Package))...)
		}
	}
	if checked < 50 {
		return nil, fmt.Errorf("type-checked %d packages; the module listing is incomplete", checked)
	}
	return found, nil
}

func typedThreadFrameViolations(fset *token.FileSet, file *ast.File, info *types.Info, rel func(token.Pos) string) []string {
	path := rel(file.Package)
	var found []string
	report := func(node ast.Node, what string) {
		found = append(found, fmt.Sprintf("%s: %s", fset.Position(node.Pos()), what))
	}
	writes := threadFieldWriteSites(file)
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			obj := info.Uses[node]
			if isEventchanThreadUpdated(obj) && !slices.Contains(threadUpdatedNamers, path) {
				report(node, "names eventchan.ThreadUpdated")
			}
			if isThreadUpdateEventField(obj, "Thread") && path != threadFrameEmitter && writes[node] {
				report(node, "sets ThreadUpdateEvent.Thread")
			}
		case *ast.CompositeLit:
			if path != threadFrameEmitter && isThreadUpdateEventType(info.Types[node].Type) &&
				len(node.Elts) > 0 && !isKeyValue(node.Elts[0]) {
				report(node, "builds a ThreadUpdateEvent positionally")
			}
		case *ast.CallExpr:
			// A conversion from another struct type carries its Thread field.
			if tv, ok := info.Types[node.Fun]; ok && tv.IsType() && len(node.Args) == 1 &&
				path != threadFrameEmitter && isThreadUpdateEventType(tv.Type) &&
				!isThreadUpdateEventType(info.Types[node.Args[0]].Type) {
				report(node, "converts a value to ThreadUpdateEvent")
			}
		}
		if sel, ok := node.(*ast.SelectorExpr); ok && isEventchanThreadUpdated(info.Uses[sel.Sel]) {
			return true // the name check above judges it
		}
		if expr, ok := node.(ast.Expr); ok && path != "internal/eventchan/channels.go" &&
			!isEventchanThreadUpdated(identUse(info, expr)) {
			if tv := info.Types[expr]; tv.Value != nil && tv.Value.Kind() == constant.String &&
				constant.StringVal(tv.Value) == string(threadUpdatedChannelValue) {
				report(node, "spells the thread:updated channel")
				return false
			}
		}
		return true
	})
	return found
}

// threadUpdatedChannelValue is eventchan.ThreadUpdated's value, spelled out
// so a constant equal to it is caught wherever it is built.
const threadUpdatedChannelValue = "thread:updated"

// threadFieldWriteSites collects the identifiers naming a field in a write
// position: a composite literal key, an assignment or increment target, or
// an address taken (a pointer to the field can be written through).
func threadFieldWriteSites(file *ast.File) map[*ast.Ident]bool {
	sites := map[*ast.Ident]bool{}
	mark := func(expr ast.Expr) {
		for {
			switch e := expr.(type) {
			case *ast.ParenExpr:
				expr = e.X
				continue
			case *ast.SelectorExpr:
				sites[e.Sel] = true
			case *ast.Ident:
				sites[e] = true
			}
			return
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok {
				sites[key] = true
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				mark(lhs)
			}
		case *ast.IncDecStmt:
			mark(node.X)
		case *ast.UnaryExpr:
			if node.Op == token.AND {
				mark(node.X)
			}
		}
		return true
	})
	return sites
}

func identUse(info *types.Info, expr ast.Expr) types.Object {
	if id, ok := expr.(*ast.Ident); ok {
		return info.Uses[id]
	}
	return nil
}

func isEventchanThreadUpdated(obj types.Object) bool {
	c, ok := obj.(*types.Const)
	return ok && c.Pkg() != nil && c.Pkg().Path() == eventchanPkgPath && c.Name() == "ThreadUpdated"
}

// isThreadUpdateEventField reports whether obj is the named field of
// triage.ThreadUpdateEvent, looked up through the package that declares obj
// so imported and source-checked copies both match.
func isThreadUpdateEventField(obj types.Object, name string) bool {
	field, ok := obj.(*types.Var)
	if !ok || !field.IsField() || field.Name() != name || field.Pkg() == nil || field.Pkg().Path() != triagePkgPath {
		return false
	}
	typeName, ok := field.Pkg().Scope().Lookup("ThreadUpdateEvent").(*types.TypeName)
	if !ok {
		return false
	}
	st, ok := typeName.Type().Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if st.Field(i) == field {
			return true
		}
	}
	return false
}

func isThreadUpdateEventType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	if ptr, ok := types.Unalias(typ).(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	named, ok := types.Unalias(typ).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == triagePkgPath &&
		named.Obj().Name() == "ThreadUpdateEvent"
}

func isKeyValue(expr ast.Expr) bool {
	_, ok := expr.(*ast.KeyValueExpr)
	return ok
}

// syntacticThreadFrameViolations is the check for a file the default build
// excludes, which cannot be type-checked here: it matches names, so an alias
// escapes it, and go-build's variant compiles plus review cover the rest.
func syntacticThreadFrameViolations(fset *token.FileSet, file *ast.File, path string) []string {
	var found []string
	report := func(node ast.Node, what string) {
		found = append(found, fmt.Sprintf("%s: %s", fset.Position(node.Pos()), what))
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if node.Sel.Name == "ThreadUpdated" && !slices.Contains(threadUpdatedNamers, path) {
				report(node, "names ThreadUpdated")
			}
		case *ast.BasicLit:
			if node.Kind == token.STRING {
				if value, err := strconv.Unquote(node.Value); err == nil && value == threadUpdatedChannelValue {
					report(node, "spells the thread:updated channel")
				}
			}
		case *ast.CompositeLit:
			if !isThreadUpdateEventExpr(node.Type) || path == threadFrameEmitter {
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					report(node, "builds a ThreadUpdateEvent positionally")
					break
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Thread" {
					report(node, "sets ThreadUpdateEvent.Thread")
				}
			}
		}
		return true
	})
	return found
}

func isThreadUpdateEventExpr(expr ast.Expr) bool {
	switch typ := expr.(type) {
	case *ast.Ident:
		return typ.Name == "ThreadUpdateEvent"
	case *ast.SelectorExpr:
		return typ.Sel.Name == "ThreadUpdateEvent"
	}
	return false
}
