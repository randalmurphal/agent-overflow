package surfaces

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The gate. surfaces.go is an authored list, and an authored list of
// what the code does is worth exactly as much as its last update. These
// tests read the tree and fail when the two disagree, in both
// directions: a bind or a route with no row, and a row naming a file
// that no longer binds or serves anything.
//
// The matchers below are deliberately narrow, and where narrowness would
// create a silent hole the test fails loudly instead of skipping. A gate
// that quietly matches nothing is worse than no gate, because it reads
// as coverage.
//
// Scope mirrors the Makefile's GO_PACKAGE_ROOTS, so `spike/` is outside
// it by the same rule that keeps it out of `make go-build` — no
// exclusion list required. Test files are excluded: a test binds a
// throwaway port constantly and none of those are surfaces anybody
// ships.

// goSourceRoots mirrors GO_PACKAGE_ROOTS in the Makefile: the root
// package, ./cmd/... and ./internal/....
var goSourceRoots = []string{".", "cmd", "internal"}

// listenerPackageCalls are package-qualified calls that bind a port,
// keyed by import path. Matched only when the qualifier resolves to that
// import in the file being scanned, so `client.Listen(...)` on a
// harnessclient value — a different thing entirely, and present in
// cmd/ao-harness today — cannot trip them.
var listenerPackageCalls = map[string]map[string]bool{
	"net": {
		"Listen":             true,
		"ListenTCP":          true,
		"ListenUDP":          true,
		"ListenIP":           true,
		"ListenUnix":         true,
		"ListenUnixgram":     true,
		"ListenPacket":       true,
		"ListenMulticastUDP": true,
		"FileListener":       true,
	},
	"crypto/tls": {
		"Listen":      true,
		"NewListener": true,
	},
	"net/http": {
		"ListenAndServe":    true,
		"ListenAndServeTLS": true,
	},
}

// listenerMethodCalls are bind calls reached through a value rather than
// a package, matched on the method name alone. Both names belong to
// http.Server and to nothing else in this tree or its dependencies'
// surface: whatever the receiver is, a call named ListenAndServe binds.
var listenerMethodCalls = map[string]bool{
	"ListenAndServe":    true,
	"ListenAndServeTLS": true,
}

// listenerCompositeTypes are types whose construction is the only way to
// reach a bind method this scan cannot match by name. net.ListenConfig's
// Listen is called on a value, and `Listen` on a value is too common a
// name to match blind — so the gate fires on building the config
// instead, which is equally unavoidable and completely unambiguous.
var listenerCompositeTypes = map[string]map[string]bool{
	"net": {"ListenConfig": true},
}

// routeRegistrationNames are the calls that add a pattern to a mux.
// Matched on the method name plus an argument count of two, which is
// what separates `mux.Handle(pattern, handler)` from `triage.Handle(evt)`
// and from the `windows.Handle(fd)` conversion. Deliberate exclusions:
// none. Nothing in the tree today has this shape without being a route.
var routeRegistrationNames = map[string]bool{
	"Handle":     true,
	"HandleFunc": true,
}

// repoRoot resolves the repository root from this file's own path rather
// than from the working directory, so the gate does not need a TestMain
// that chdirs the whole package.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file; the scan has no root to walk")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// scanned is one parsed production source, with the two things the
// matchers need to resolve names: which local identifiers are imported
// packages, and which package-level constants hold string literals.
type scanned struct {
	// Rel is the repository-relative path, slash-separated, which is
	// the spelling Listener.Sites uses.
	Rel string

	File *ast.File

	// ImportPathFor maps a local package identifier ("net", "nethttp")
	// to its import path.
	ImportPathFor map[string]string
}

// scanSources parses every non-test Go file under the Makefile's package
// roots.
func scanSources(t *testing.T, fset *token.FileSet) []scanned {
	t.Helper()
	root := repoRoot(t)

	var paths []string
	for _, sourceRoot := range goSourceRoots {
		dir := filepath.Join(root, sourceRoot)
		// The root package is the root DIRECTORY's own files and
		// nothing below it — cmd/ and internal/ are walked on their own
		// passes, and everything else down there (frontend/, docs/,
		// build/, .claude/worktrees/…) holds no Go this build compiles.
		descend := sourceRoot != "."
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				switch {
				case path == dir:
					return nil
				case !descend, strings.HasPrefix(entry.Name(), "."):
					return fs.SkipDir
				}
				return nil
			}
			name := entry.Name()
			if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(paths) == 0 {
		t.Fatal("no Go sources found; every gate below would pass vacuously")
	}

	sources := make([]scanned, 0, len(paths))
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relativize %s: %v", path, err)
		}
		imports := make(map[string]string, len(file.Imports))
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquotable import path %s", rel, spec.Path.Value)
			}
			local := importPath
			if i := strings.LastIndex(local, "/"); i >= 0 {
				local = local[i+1:]
			}
			if spec.Name != nil {
				local = spec.Name.Name
			}
			imports[local] = importPath
		}
		sources = append(sources, scanned{
			Rel:           filepath.ToSlash(rel),
			File:          file,
			ImportPathFor: imports,
		})
	}
	return sources
}

// bindSites returns the repository-relative files holding at least one
// listener-creating call, each mapped to the call sites found there so a
// failure can name a position rather than only a file.
func bindSites(t *testing.T, fset *token.FileSet, sources []scanned) map[string][]string {
	t.Helper()
	sites := map[string][]string{}
	for _, source := range sources {
		ast.Inspect(source.File, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CallExpr:
				name, matched := listenerCallName(source, typed.Fun)
				if !matched {
					return true
				}
				sites[source.Rel] = append(sites[source.Rel],
					fmt.Sprintf("%s: %s", fset.Position(typed.Pos()), name))
			case *ast.CompositeLit:
				selector, ok := typed.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				if !listenerCompositeTypes[source.ImportPathFor[ident.Name]][selector.Sel.Name] {
					return true
				}
				sites[source.Rel] = append(sites[source.Rel],
					fmt.Sprintf("%s: %s.%s{…}", fset.Position(typed.Pos()), ident.Name, selector.Sel.Name))
			}
			return true
		})
	}
	return sites
}

// listenerCallName reports whether a call binds a port, and what to call
// it in a failure message.
func listenerCallName(source scanned, fun ast.Expr) (string, bool) {
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if ident, ok := selector.X.(*ast.Ident); ok {
		if importPath, isImport := source.ImportPathFor[ident.Name]; isImport {
			if listenerPackageCalls[importPath][selector.Sel.Name] {
				return ident.Name + "." + selector.Sel.Name, true
			}
			// A package-qualified call whose package we track but whose
			// name we do not is not a bind; fall through rather than
			// letting the method matcher below see it.
			return "", false
		}
	}
	if listenerMethodCalls[selector.Sel.Name] {
		return selector.Sel.Name, true
	}
	return "", false
}

// TestEveryListenerHasAnInventoryRow is the forward direction: a bind
// the inventory does not claim fails the build, naming the file.
func TestEveryListenerHasAnInventoryRow(t *testing.T) {
	fset := token.NewFileSet()
	sources := scanSources(t, fset)
	sites := bindSites(t, fset, sources)

	if len(sites) == 0 {
		t.Fatal("the listener matcher found no binds at all; it has stopped matching and the gate is vacuous")
	}

	claimedBy := map[string]string{}
	for _, listener := range Listeners {
		for _, site := range listener.Sites {
			if existing, dup := claimedBy[site]; dup {
				t.Errorf("%s is claimed by both %q and %q; one file is one listener surface, so split the file", site, existing, listener.Name)
				continue
			}
			claimedBy[site] = listener.Name
		}
	}

	var unclaimed []string
	for file, calls := range sites {
		if _, ok := claimedBy[file]; ok {
			continue
		}
		sort.Strings(calls)
		unclaimed = append(unclaimed, fmt.Sprintf("%s\n      %s", file, strings.Join(calls, "\n      ")))
	}
	if len(unclaimed) > 0 {
		sort.Strings(unclaimed)
		t.Errorf("%d file(s) bind a port with no row in internal/surfaces.Listeners:\n  %s\n\n"+
			"Add a Listener row naming the binding class, the credential a caller must "+
			"present, what bytes leave it, and a Why that says what capability sits "+
			"behind that credential. A listener that serves nothing still gets a row "+
			"(see \"dev supervisor port probe\") — the row is where the reader learns why.",
			len(unclaimed), strings.Join(unclaimed, "\n  "))
	}
}

// TestEveryInventoryRowStillBinds is the reverse direction: a row whose
// file no longer binds is a description of code that is gone, and it
// would go on reassuring readers indefinitely.
func TestEveryInventoryRowStillBinds(t *testing.T) {
	fset := token.NewFileSet()
	sources := scanSources(t, fset)
	sites := bindSites(t, fset, sources)

	for _, listener := range Listeners {
		for _, site := range listener.Sites {
			if len(sites[site]) == 0 {
				t.Errorf("Listeners[%q] names %s, but no listener-creating call is there any more; "+
					"move the row to the file that binds now, or delete it", listener.Name, site)
			}
		}
	}
}

// routePatterns returns every mux registration in the tree as
// file → patterns.
func routePatterns(t *testing.T, fset *token.FileSet, sources []scanned) map[string][]string {
	t.Helper()
	patterns := map[string][]string{}

	// Constants are collected per DIRECTORY, not per file: one
	// directory is one package here, and transport's ScopedRPCPath is
	// declared in httprpc.go but registered in server.go.
	byPackage := map[string]map[string]string{}
	for _, source := range sources {
		dir := packageDir(source.Rel)
		if byPackage[dir] == nil {
			byPackage[dir] = map[string]string{}
		}
		for name, value := range stringConstants(source.File) {
			byPackage[dir][name] = value
		}
	}

	for _, source := range sources {
		consts := byPackage[packageDir(source.Rel)]
		ast.Inspect(source.File, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !routeRegistrationNames[selector.Sel.Name] {
				return true
			}
			pattern, ok := routePattern(call.Args[0], consts)
			if !ok {
				// Failing here rather than skipping is the whole point.
				// A pattern this scan cannot read is a route the
				// inventory cannot be checked against, which is exactly
				// the state the gate exists to prevent.
				t.Errorf("%s: %s(…) registers a route whose pattern is not a string literal or a "+
					"package-level string constant, so the inventory cannot be checked against it. "+
					"Spell the pattern as a constant in the same package.",
					fset.Position(call.Pos()), selector.Sel.Name)
				return true
			}
			patterns[source.Rel] = append(patterns[source.Rel], pattern)
			return true
		})
	}
	return patterns
}

// routePattern reads a registration's first argument as a pattern
// string, following one level of package-level constant.
func routePattern(arg ast.Expr, consts map[string]string) (string, bool) {
	switch typed := arg.(type) {
	case *ast.BasicLit:
		if typed.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(typed.Value)
		if err != nil {
			return "", false
		}
		return value, true
	case *ast.Ident:
		value, ok := consts[typed.Name]
		return value, ok
	}
	return "", false
}

// packageDir is the per-package grouping key for the constant scan: one
// directory is one package in this repo.
func packageDir(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return "."
}

// stringConstants collects a file's package-level string constants,
// which is how PageURLPath and ScopedRPCPath resolve.
func stringConstants(file *ast.File) map[string]string {
	consts := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				consts[name.Name] = unquoted
			}
		}
	}
	return consts
}

// TestEveryRouteHasAnInventoryRow checks both directions at once: a
// registered pattern with no row, and a row for a pattern nothing
// registers. Routes are keyed by (listener, pattern) because "/" and
// "/ws" are each registered on two different muxes and mean different
// things on each.
func TestEveryRouteHasAnInventoryRow(t *testing.T) {
	fset := token.NewFileSet()
	sources := scanSources(t, fset)
	registered := routePatterns(t, fset, sources)

	if len(registered) == 0 {
		t.Fatal("the route matcher found no registrations at all; it has stopped matching and the gate is vacuous")
	}

	// Which listener a file's routes belong to comes from that
	// listener's own Sites, so the two halves of the inventory cannot
	// drift apart: a route row can only name a listener the file
	// actually binds.
	listenerForFile := map[string]string{}
	for _, listener := range Listeners {
		for _, site := range listener.Sites {
			listenerForFile[site] = listener.Name
		}
	}

	type key struct{ listener, pattern string }
	inventory := map[key]bool{}
	for _, route := range Routes {
		inventory[key{route.Listener, route.Pattern}] = true
	}

	seen := map[key]bool{}
	var missing []string
	for file, patterns := range registered {
		listener, ok := listenerForFile[file]
		if !ok {
			// The listener gate reports this file already; adding a
			// second failure for the same cause would only obscure it.
			continue
		}
		for _, pattern := range patterns {
			entry := key{listener, pattern}
			seen[entry] = true
			if !inventory[entry] {
				missing = append(missing, fmt.Sprintf("%q on %q (%s)", pattern, listener, file))
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d route(s) are registered with no row in internal/surfaces.Routes:\n  %s\n\n"+
			"Add a Route row naming the credential the route demands, the posture of what "+
			"it answers with, and a Why.", len(missing), strings.Join(missing, "\n  "))
	}

	var stale []string
	for entry := range inventory {
		if !seen[entry] {
			stale = append(stale, fmt.Sprintf("%q on %q", entry.pattern, entry.listener))
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%d row(s) in internal/surfaces.Routes describe a route nothing registers:\n  %s\n\n"+
			"Delete the row, or fix the pattern to match the registration verbatim.",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// TestEveryRegistryStillMatchesItsTable is the gate for the two
// reference rows. Listeners and routes are checked by scanning the whole
// tree for binds and registrations; a registry cannot be found that way,
// because its 360 (or 72) entries are the thing this package
// deliberately does not copy. What it CAN check is that the row still
// describes the table it points at:
//
//   - the file exists and declares the named symbol,
//   - the symbol is a composite literal with entries in it, so a row
//     naming an emptied-out table fails rather than reading as coverage,
//   - every entry sets every field the row names as required, which is
//     what catches an entry somebody added without classifying it, and
//   - every named gate is still a function in the file that held it.
//
// The last one is the drift a reference row exists to notice. This
// tree's per-method origin partition was deleted in one wave; a row
// naming the function that used to enforce it would have kept claiming a
// control that no longer existed.
func TestEveryRegistryStillMatchesItsTable(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	for _, registry := range Registries {
		t.Run(registry.Name, func(t *testing.T) {
			file, err := parser.ParseFile(fset, filepath.Join(root, registry.Source), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", registry.Source, err)
			}

			literal := packageLevelComposite(file, registry.Symbol)
			if literal == nil {
				t.Fatalf("%s declares no package-level %s bound to a composite literal; the row is pointing at something that moved or was renamed",
					registry.Source, registry.Symbol)
			}
			if len(literal.Elts) == 0 {
				t.Fatalf("%s.%s is empty; a registry row over an empty table reads as coverage and is none",
					registry.Source, registry.Symbol)
			}
			for i, element := range literal.Elts {
				entry, ok := element.(*ast.CompositeLit)
				if !ok {
					t.Errorf("%s.%s[%d] is not a composite literal, so its fields cannot be checked", registry.Source, registry.Symbol, i)
					continue
				}
				set := keyedFieldNames(entry)
				for _, required := range registry.RowFields {
					if !set[required] {
						t.Errorf("%s.%s[%d] sets no %s; every entry must carry the decisions surfaces.Registries names",
							registry.Source, registry.Symbol, i, required)
					}
				}
			}

			for _, gate := range registry.Gates {
				source, name, ok := strings.Cut(gate, ":")
				if !ok {
					t.Errorf("Registries[%q] gate %q is not \"file:function\"", registry.Name, gate)
					continue
				}
				gateFile, err := parser.ParseFile(fset, filepath.Join(root, source), nil, 0)
				if err != nil {
					t.Errorf("parse %s: %v", source, err)
					continue
				}
				if !declaresFunc(gateFile, name) {
					t.Errorf("%s declares no %s; the row names a gate that moved or was deleted", source, name)
				}
			}
		})
	}
}

// packageLevelComposite finds `var name = <composite literal>` or
// `var name = func() T { … }()`. The second form is how a table built
// once at init is spelled, and the literal inside it is still the
// authored rows.
func packageLevelComposite(file *ast.File, name string) *ast.CompositeLit {
	var found *ast.CompositeLit
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.VAR {
			continue
		}
		for _, spec := range genDecl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range value.Names {
				if ident.Name != name || i >= len(value.Values) {
					continue
				}
				ast.Inspect(value.Values[i], func(node ast.Node) bool {
					if found != nil {
						return false
					}
					if literal, ok := node.(*ast.CompositeLit); ok {
						found = literal
						return false
					}
					return true
				})
			}
		}
	}
	return found
}

// keyedFieldNames is the set of field names an entry sets explicitly.
// Keyed literals are what every table here uses, and they are what makes
// an omitted field visible: a positional literal would set every field
// and say nothing about which ones somebody thought about.
func keyedFieldNames(entry *ast.CompositeLit) map[string]bool {
	names := map[string]bool{}
	for _, element := range entry.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := pair.Key.(*ast.Ident); ok {
			names[key.Name] = true
		}
	}
	return names
}

// declaresFunc reports whether the file declares name as a function or
// as a method on any receiver. Either spelling satisfies a gate row:
// what the row asserts is that the decision still has somewhere to
// happen, not how it is reached.
func declaresFunc(file *ast.File, name string) bool {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return true
		}
	}
	return false
}
