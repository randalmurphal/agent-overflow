// methodgen produces internal/transport/methods_gen.go: a static map
// of every exported method the wire-level dispatcher should expose,
// keyed by name. The scan targets are the receiverSpecs list below —
// one entry today (the internal/app App promoted by the root wrapper),
// an explicit list because
// docs/architecture/root-decomposition.md promotes services into
// internal packages that keep registering under main.App. Used by:
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
	"go/format"
	"go/parser"
	"go/token"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const defaultOut = "internal/transport/methods_gen.go"

// receiverSpec names one scan target: a directory of Go source, the
// receiver type declared in it, and the package/type labels its
// methods register under on the wire.
//
// Package and TypeName are the FQN parts, NOT facts about where the
// code lives: the dispatcher takes both as plain strings from
// RegisterOptions (see docs/architecture/root-decomposition.md § Wire
// compatibility), so a service promoted into internal/<pkg> keeps
// registering as "main"/"App" and its method IDs never move. That is
// what lets this list grow a second directory without a wire
// migration.
type receiverSpec struct {
	// Dir is the directory to scan, relative to the repo root.
	Dir string
	// Receiver is the receiver type name as spelled in SOURCE:
	// func (x *Receiver) Method(...). Pointer receivers only, same as
	// Wails' own generator.
	Receiver string
	// Package is the registered package label used in the FQN.
	Package string
	// TypeName is the registered type label used in the FQN. Empty
	// means "same as Receiver" — set it only when a receiver registers
	// under a different name than it is declared with.
	TypeName string
}

// fqnType is the type label this spec's methods hash under.
func (s receiverSpec) fqnType() string {
	if s.TypeName != "" {
		return s.TypeName
	}
	return s.Receiver
}

// receiverSpecs is the full set of receivers whose methods belong in
// the generated table. One entry today: the App implementation in
// internal/app, promoted by the root wrapper and registered as
// main.App.<Method>.
//
// Harness (also a repo-root receiver registered as main.Harness) is
// deliberately absent. The generated table is the App allow-list —
// bootTransport passes NewMethodAllowList() only on the App
// registration — while Harness registers unfiltered, receiver-level
// LocalOnly, and only under the --harness boot path. Listing it here
// would put boot-mode-only methods into the production allow-list and
// into the LAN-safety classification gate that partners it.
var receiverSpecs = []receiverSpec{
	{Dir: "internal/app", Receiver: "App", Package: "main"},
}

// loadInternalSkipList parses internal/transport/internalmethods.go
// for the var InternalServiceMethods literal and returns its keys as a
// skip set, so the runtime dispatcher and the codegen tool stay in
// sync. Failure to parse is fatal — methodgen would otherwise silently
// expose framework lifecycle methods.
func loadInternalSkipList(root string) (map[string]bool, error) {
	internalServiceMethods := map[string]bool{}
	target := filepath.Join(root, "internal/transport/internalmethods.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", target, err)
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
		return nil, fmt.Errorf("InternalServiceMethods set is empty in %s", target)
	}
	return internalServiceMethods, nil
}

// MethodEntry is one row in the generated map.
type MethodEntry struct {
	Name string
	ID   uint32
	FQN  string
}

func main() {
	out := flag.String("out", defaultOut, "output file path (relative to repo root)")
	rootFlag := flag.String("root", ".", "repository root every receiverSpec.Dir resolves against")
	flag.Parse()

	root := *rootFlag

	skip, err := loadInternalSkipList(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: %v\n", err)
		os.Exit(1)
	}

	entries, err := scanReceivers(root, receiverSpecs, skip)
	if err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: %v\n", err)
		os.Exit(1)
	}

	body, err := renderFile(entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: render: %v\n", err)
		os.Exit(1)
	}

	// gofmt the generated source so the committed file stays format-clean
	// and the in-sync CI gate (methods_gen_test.go) keeps matching even
	// after a repo-wide gofmt pass.
	body, err = format.Source(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "methodgen: format: %v\n", err)
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

// scanReceivers walks every spec's directory and extracts each
// exported method on that spec's receiver type, honouring
// //wails:ignore and the internal skip set. The merged result is
// sorted by method name, so the emitted table does not depend on spec
// order or on directory iteration order.
//
// Method names share ONE namespace across receivers — the dispatcher
// falls back to name lookup when a frame carries no ID, and refuses a
// duplicate at Register time (transport.Dispatcher.byName). Refuse it
// here too, so a shadowing method fails codegen rather than boot.
func scanReceivers(root string, specs []receiverSpec, skip map[string]bool) ([]MethodEntry, error) {
	fset := token.NewFileSet()
	var entries []MethodEntry
	// method name -> the FQN that claimed it, for the collision report.
	claimed := map[string]string{}

	for _, spec := range specs {
		dir := filepath.Join(root, spec.Dir)
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read receiver dir %s: %w", dir, err)
		}

		for _, de := range dirEntries {
			if de.IsDir() || filepath.Ext(de.Name()) != ".go" {
				continue
			}
			// Skip *_test.go — bindings only consider production code.
			if strings.HasSuffix(de.Name(), "_test.go") {
				continue
			}

			path := filepath.Join(dir, de.Name())
			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			// A directory holds exactly one non-test package, so the
			// receiver-name match below already pins the package; the
			// only extra filter worth keeping is the external test
			// package that can legally share the directory.
			if strings.HasSuffix(file.Name.Name, "_test") {
				continue
			}

			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
					continue
				}
				if !isPointerReceiver(fn.Recv.List[0].Type, spec.Receiver) {
					continue
				}
				name := fn.Name.Name
				if !ast.IsExported(name) {
					continue
				}
				if skip[name] {
					continue
				}
				if hasWailsIgnore(fn.Doc) {
					continue
				}

				fqn := fmt.Sprintf("%s.%s.%s", spec.Package, spec.fqnType(), name)
				if prev, ok := claimed[name]; ok {
					return nil, fmt.Errorf("name collision between %s and %s on name %q (%s)",
						prev, fqn, name, path)
				}
				claimed[name] = fqn

				entries = append(entries, MethodEntry{
					Name: name,
					ID:   fnvHash(fqn),
					FQN:  fqn,
				})
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// isPointerReceiver returns true when expr is "*<typeName>" — the
// receiver form for production-bound methods. Pointer-only because
// Wails' generator also requires *T receivers.
func isPointerReceiver(expr ast.Expr, typeName string) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == typeName
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
