// methodgen produces internal/transport/methods_gen.go: a static map
// of every exported App method that the wire-level dispatcher should
// expose, keyed by name. Used by:
//
//   - production: NewMethodRegistry() returns the map; the runtime
//     dispatcher's allow-list is set to its keys, so methods marked
//     //wails:ignore in source stay unreachable on the wire.
//
//   - CI gate: a transport_methodgen_test.go runs `go run ./internal/
//     transport/methodgen` into a tempdir and diffs the result against
//     the committed file. Drift fails CI so a developer who adds an
//     App method without regenerating gets a fast signal.
//
// Method-name -> FNV-1a-32 ID map matches Wails' internal/hash.Fnv
// (verified at v3.0.0-alpha.76 against the generated frontend bindings:
// fnvHash("main.App.ArchiveProject") == 1352159878).
//
// Run via:
//
//	go run ./internal/transport/methodgen [-out internal/transport/methods_gen.go]
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultOut = "internal/transport/methods_gen.go"

	// receiverTypeName is the App struct name. Hard-coded because there
	// is exactly one bound service in this app and the FQN format
	// (main.App.<Method>) is what Wails uses internally.
	receiverTypeName = "App"

	// pkgPath is the Go package path used in the FQN. The App lives in
	// the main module's root package, so its import path is "main"
	// from the runtime reflection perspective.
	pkgPath = "main"
)

// internalServiceMethods is loaded from internal/transport/
// internalmethods.go via AST parse so the runtime dispatcher and the
// codegen tool stay in sync. The +loaded set augments the framework
// lifecycle list with App-level skips (today: nothing — //wails:ignore
// directives carry that information in source).
var internalServiceMethods = map[string]bool{}

// loadInternalSkipList parses internal/transport/internalmethods.go
// for the var InternalServiceMethods literal and copies its keys into
// internalServiceMethods. Failure to parse is fatal — methodgen would
// otherwise silently expose framework lifecycle methods.
func loadInternalSkipList(root string) error {
	target := filepath.Join(root, "internal/transport/internalmethods.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", target, err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "InternalServiceMethods" {
				continue
			}
			cl, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, e := range cl.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				lit, ok := kv.Key.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				key, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				internalServiceMethods[key] = true
			}
		}
	}
	if len(internalServiceMethods) == 0 {
		return fmt.Errorf("InternalServiceMethods set is empty in %s", target)
	}
	return nil
}

// MethodEntry is one row in the generated map.
type MethodEntry struct {
	Name string
	ID   uint32
	FQN  string
}

func main() {
	out := flag.String("out", defaultOut, "output file path (relative to repo root)")
	rootFlag := flag.String("root", ".", "repository root containing app.go and app_*.go")
	flag.Parse()

	root := *rootFlag

	if err := loadInternalSkipList(root); err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: %v\n", err)
		os.Exit(1)
	}

	entries, err := scanRepo(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: %v\n", err)
		os.Exit(1)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	body, err := renderFile(entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: render: %v\n", err)
		os.Exit(1)
	}

	// Absolute -out path: write directly. Relative: resolve under root
	// so `go run` from any cwd lands the file in the canonical spot.
	target := *out
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	if err := os.WriteFile(target, body, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: write %s: %v\n", target, err)
		os.Exit(1)
	}
	fmt.Printf("methodgen: wrote %d methods to %s\n", len(entries), target)
}

// scanRepo walks the project root for *.go files in package main and
// extracts every exported method on *App, honouring //wails:ignore.
func scanRepo(root string) ([]MethodEntry, error) {
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read root %s: %w", root, err)
	}

	fset := token.NewFileSet()
	var entries []MethodEntry
	seen := map[string]bool{}

	for _, de := range dirEntries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".go" {
			continue
		}
		// Skip *_test.go — bindings only consider production code.
		if strings.HasSuffix(de.Name(), "_test.go") {
			continue
		}

		path := filepath.Join(root, de.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if file.Name.Name != "main" {
			continue
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if !isAppReceiver(fn.Recv.List[0].Type) {
				continue
			}
			name := fn.Name.Name
			if !ast.IsExported(name) {
				continue
			}
			if internalServiceMethods[name] {
				continue
			}
			if hasWailsIgnore(fn.Doc) {
				continue
			}
			if seen[name] {
				return nil, fmt.Errorf("duplicate method %s.%s.%s in %s",
					pkgPath, receiverTypeName, name, path)
			}
			seen[name] = true

			fqn := fmt.Sprintf("%s.%s.%s", pkgPath, receiverTypeName, name)
			entries = append(entries, MethodEntry{
				Name: name,
				ID:   fnvHash(fqn),
				FQN:  fqn,
			})
		}
	}
	return entries, nil
}

// isAppReceiver returns true when expr is "*App" — the receiver type
// for production-bound methods. Pointer-only because Wails' generator
// also requires *T receivers.
func isAppReceiver(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == receiverTypeName
}

// hasWailsIgnore returns true if any line in the doc comment is
// exactly the directive "//wails:ignore" (gofmt strips trailing space).
// The directive lives WITHIN the doc-comment group on the function so
// it travels with the source on grep / refactor.
func hasWailsIgnore(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		if c.Text == "//wails:ignore" {
			return true
		}
	}
	return false
}

// fnvHash matches Wails' internal/hash.Fnv (FNV-1a 32-bit). Same impl
// as transport.fnvHash; duplicated here so the codegen tool has no
// transitive deps on the package it generates into.
func fnvHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// renderFile emits the generated Go source.
func renderFile(entries []MethodEntry) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`// Code generated by internal/transport/methodgen. DO NOT EDIT.
//
// Run "go run ./internal/transport/methodgen" to regenerate.
//
// This file lists every exported (App).Method that the runtime
// dispatcher exposes on the WebSocket transport. Methods marked
// //wails:ignore in the App receiver source are intentionally
// excluded so they remain unreachable from the wire — same set the
// auto-generated TS bindings expose, just enforced at runtime.

package transport

// GeneratedMethod pairs a method name with its FNV-1a 32-bit hash.
// The hash matches Wails' internal/hash.Fnv("main.App.<Name>") so the
// frontend's $Call.ByID(<num>, ...args) routes correctly.
type GeneratedMethod struct {
	Name string
	ID   uint32
}

// GeneratedMethods is the static, sorted-by-name list of every method
// the dispatcher should expose. Use NewMethodAllowList to build a
// dispatcher allow-list from this set.
var GeneratedMethods = []GeneratedMethod{
`)
	for _, e := range entries {
		fmt.Fprintf(&buf, "\t{Name: %q, ID: %d}, // %s\n", e.Name, e.ID, e.FQN)
	}
	buf.WriteString(`}

// NewMethodAllowList returns a name set suitable for
// RegisterOptions.AllowList, locking the dispatcher to exactly the
// methods this generated file lists.
func NewMethodAllowList() map[string]bool {
	out := make(map[string]bool, len(GeneratedMethods))
	for _, m := range GeneratedMethods {
		out[m.Name] = true
	}
	return out
}
`)
	return buf.Bytes(), nil
}
