package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The backend:set-changed frame is declared once, in
// internal/attachedbackends (SetChange), emitted by two desktops
// (internal/app and internal/frontendclient) and mirrored once in the
// frontend (systems.svelte.ts BackendSetChangeEvent). Nothing type-checks
// across that boundary, so the two tests here do: the action vocabulary is
// compared against the TS mirror in both directions, and every emit site is
// required to carry the typed frame rather than a map or a hand-spelled
// action — which is exactly how the frontend-only desktop came to emit a
// second shape of the same channel.

const (
	setChangePackageDir = "internal/attachedbackends"
	setChangeMirrorPath = "frontend/src/lib/stores/systems.svelte.ts"
)

// setChangeEmitters are the packages whose production sources may emit
// backend:set-changed. Each must have at least one site, so the scan cannot
// pass vacuously because an emitter moved.
var setChangeEmitters = []string{"internal/app", "internal/frontendclient"}

// setChangeTypeSpellings are the ways the frame's type is written at an
// emit site: the declaring package's name, the app alias, and the
// unqualified name inside internal/attachedbackends itself.
var setChangeTypeSpellings = map[string]bool{
	"attachedbackends.SetChange": true,
	"BackendSetChange":           true,
	"SetChange":                  true,
}

// TestBackendSetChangeVocabularyMatchesTheFrontend pins the action and
// reason sets between the Go declaration and the TS mirror, both ways: a
// Go action the frontend does not know is a frame it drops on the floor,
// and a TS action Go never emits is a branch nothing reaches.
func TestBackendSetChangeVocabularyMatchesTheFrontend(t *testing.T) {
	for _, pair := range []struct{ goType, tsField string }{
		{"SetAction", "action"},
		{"RemovalReason", "reason"},
	} {
		declared := setChangeConstants(t, pair.goType)
		// The empty constant is the omitted optional field, which the TS
		// side spells as `?:` rather than as a member of the union.
		delete(declared, "")
		mirrored := setChangeMirrorUnion(t, pair.tsField)
		if len(declared) == 0 || len(mirrored) == 0 {
			t.Fatalf("%s/%s: declared=%v mirrored=%v; one side is empty", pair.goType, pair.tsField, declared, mirrored)
		}
		for _, name := range sortedKeys(declared) {
			if !mirrored[name] {
				t.Errorf("%s declares %q and %s's %s union does not", pair.goType, name, setChangeMirrorPath, pair.tsField)
			}
		}
		for _, name := range sortedKeys(mirrored) {
			if !declared[name] {
				t.Errorf("%s's %s union names %q and %s.%s does not", setChangeMirrorPath, pair.tsField, name, setChangePackageDir, pair.goType)
			}
		}
	}
}

// TestBackendSetChangeEmitsCarryTheTypedFrame walks the production sources
// of both emitting packages and fails on an emit of
// eventchan.BackendSetChanged whose payload is not the typed frame: a map
// literal, a struct of another type, or a SetChange whose Action is a bare
// string. Inside internal/attachedbackends the same rule holds for every
// SetChange literal, since that is where the frames are built.
func TestBackendSetChangeEmitsCarryTheTypedFrame(t *testing.T) {
	fset := token.NewFileSet()
	for _, dir := range append([]string{setChangePackageDir}, setChangeEmitters...) {
		sites := 0
		for _, path := range productionGoFiles(t, dir) {
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			funcs := functionParams(file)
			ast.Inspect(file, func(n ast.Node) bool {
				if lit, ok := n.(*ast.CompositeLit); ok && setChangeTypeSpellings[types.ExprString(lit.Type)] {
					if reason := literalActionProblem(lit); reason != "" {
						t.Errorf("%s: %s", fset.Position(lit.Pos()), reason)
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || !eventEmitFuncNames[emitCallName(call.Fun)] || len(call.Args) < 2 {
					return true
				}
				if types.ExprString(call.Args[0]) != "eventchan.BackendSetChanged" {
					return true
				}
				sites++
				switch arg := call.Args[1].(type) {
				case *ast.CompositeLit:
					if !setChangeTypeSpellings[types.ExprString(arg.Type)] {
						t.Errorf("%s: backend:set-changed emitted as %s, not attachedbackends.SetChange", fset.Position(call.Pos()), types.ExprString(arg.Type))
					}
				case *ast.Ident:
					if typ := funcs.paramType(call.Pos(), arg.Name); !setChangeTypeSpellings[typ] {
						t.Errorf("%s: backend:set-changed emitted as %q, which is not a parameter of type attachedbackends.SetChange (got %q)", fset.Position(call.Pos()), arg.Name, typ)
					}
				default:
					t.Errorf("%s: backend:set-changed emitted with an untyped payload", fset.Position(call.Pos()))
				}
				return true
			})
		}
		if dir != setChangePackageDir && sites == 0 {
			t.Errorf("%s: no emit of eventchan.BackendSetChanged found; the scan would pass vacuously", dir)
		}
	}
}

// literalActionProblem names what is wrong with a SetChange literal whose
// Action (or Reason) is spelled by hand, or "" when every field names a
// constant.
func literalActionProblem(lit *ast.CompositeLit) string {
	for _, element := range lit.Elts {
		kv, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return "SetChange literal uses positional fields; name them so the action is visibly a constant"
		}
		key := types.ExprString(kv.Key)
		if key != "Action" && key != "Reason" {
			continue
		}
		if _, isLiteral := kv.Value.(*ast.BasicLit); isLiteral {
			return fmt.Sprintf("SetChange.%s is a bare string; use the attachedbackends constant", key)
		}
	}
	return ""
}

// paramScopes is every function in one file with its parameter types, so
// an identifier at an emit site can be traced to the type it was declared
// with. Innermost function wins, which is what a closure's parameter is.
type paramScopes []struct {
	pos, end token.Pos
	params   map[string]string
}

func functionParams(file *ast.File) paramScopes {
	var scopes paramScopes
	record := func(n ast.Node, fields *ast.FieldList) {
		if fields == nil {
			return
		}
		params := map[string]string{}
		for _, field := range fields.List {
			for _, name := range field.Names {
				params[name.Name] = types.ExprString(field.Type)
			}
		}
		scopes = append(scopes, struct {
			pos, end token.Pos
			params   map[string]string
		}{n.Pos(), n.End(), params})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			record(fn, fn.Type.Params)
		case *ast.FuncLit:
			record(fn, fn.Type.Params)
		}
		return true
	})
	return scopes
}

func (s paramScopes) paramType(at token.Pos, name string) string {
	best := ""
	var bestPos token.Pos
	for _, scope := range s {
		if at < scope.pos || at > scope.end {
			continue
		}
		if typ, ok := scope.params[name]; ok && scope.pos >= bestPos {
			best, bestPos = typ, scope.pos
		}
	}
	return best
}

// setChangeConstants reads the string constants of one named type out of
// internal/attachedbackends, by parsing rather than importing, so the test
// sees what is DECLARED and not what happens to be referenced.
func setChangeConstants(t *testing.T, typeName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, path := range productionGoFiles(t, setChangePackageDir) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if value.Type == nil || types.ExprString(value.Type) != typeName {
					continue
				}
				for _, expr := range value.Values {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: %s constant is not a string literal", fset.Position(expr.Pos()), typeName)
					}
					text, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[text] = true
				}
			}
		}
	}
	return out
}

// setChangeMirrorUnion reads one field's string-literal union out of the
// BackendSetChangeEvent interface in the frontend store.
func setChangeMirrorUnion(t *testing.T, field string) map[string]bool {
	t.Helper()
	source, err := os.ReadFile(setChangeMirrorPath)
	if err != nil {
		t.Fatalf("read %s: %v", setChangeMirrorPath, err)
	}
	start := strings.Index(string(source), "export interface BackendSetChangeEvent {")
	if start < 0 {
		t.Fatalf("%s no longer declares BackendSetChangeEvent", setChangeMirrorPath)
	}
	body := string(source[start:])
	body = body[:strings.Index(body, "\n}")]
	line := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `\??:\s*(.+);`).FindStringSubmatch(body)
	if line == nil {
		t.Fatalf("%s: BackendSetChangeEvent has no %q field", setChangeMirrorPath, field)
	}
	out := map[string]bool{}
	for _, match := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(line[1], -1) {
		out[match[1]] = true
	}
	return out
}

// productionGoFiles lists one package directory's non-test Go sources.
func productionGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files
}
