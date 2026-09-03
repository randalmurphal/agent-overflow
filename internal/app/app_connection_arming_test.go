package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A bound method that ARMS something on behalf of one client owes that client's
// connection a way to give it back. The client's own un-arm call is the happy
// path; the connection's death is the one that used to leak. claude-tui
// take-control is the incident this guard was written from: ProviderTerminal
// Attach / SetControl took no ctx, so a socket that died mid-take-control left
// the input lease held and every Send on that thread refused until the session
// restarted, and a second client's attach silently displaced the first's.
//
// The rule: an exported *App method whose NAME says it arms or un-arms a
// per-client resource must read transport.ConnStateFromContext — directly, or
// through a helper in the same file — or say in one line here why it does not.
//
// Reaching the connection needs a leading ctx context.Context, which the Wails
// generator strips from the TS bindings, so satisfying this guard changes no
// wire signature and no method ID (internal/app/AGENTS.md § Argument-dependent
// authorization).

// armingVocabulary is the naming that puts a method under the rule. Both halves
// are here on purpose: an un-arm method is where a per-connection release is
// most often written as "by id, from anywhere", and each one should have had to
// answer for that rather than slip past a guard that only watched the arm.
var armingVocabulary = []string{
	"Attach",
	"Detach",
	"Subscribe",
	"Unsubscribe",
	"SetControl",
	"TakeControl",
	"ReleaseControl",
	"Hold",
	"Release",
	"Acquire",
}

// connStateExemptMethods are the methods the rule names but does not apply to,
// one true reason each. Keep it short: every entry is a claim that the resource
// is not per connection, and a wrong one is exactly the leak this guard exists
// to catch.
var connStateExemptMethods = map[string]string{
	"AttachThreadWorktree":       "attaches a git worktree to a thread row; the resource is a directory on disk that outlives every connection, and detaching it is a deliberate act, not a teardown",
	"GitStatusUnsubscribe":       "releases a subscription BY ID; GitStatusSubscribe owns the connection tie and this is the client's own idempotent release of a handle it was given",
	"UnsubscribePRUpdates":       "releases a PR-update reference BY ID, on the same terms as GitStatusUnsubscribe; SubscribePRUpdates owns the connection tie",
	"BrowserCompanionPaneDetach": "releases a companion pane BY ID, on the same terms; BrowserCompanionPaneAttach owns the connection tie",
}

func TestArmingMethodsAreTiedToTheirConnection(t *testing.T) {
	entries, err := os.ReadDir("internal/app")
	if err != nil {
		t.Fatalf("read internal/app: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join("internal/app", name)
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		bodies := functionBodiesByName(parsed)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isAppMethod(fn) || !fn.Name.IsExported() || fn.Body == nil {
				continue
			}
			word, armed := armingWord(fn.Name.Name)
			if !armed {
				continue
			}
			seen[fn.Name.Name] = true
			if _, exempt := connStateExemptMethods[fn.Name.Name]; exempt {
				continue
			}
			if readsConnState(fn, bodies, map[string]bool{}) {
				continue
			}
			t.Errorf(
				"%s:%d %s arms or releases a per-client resource (%q) without reading "+
					"transport.ConnStateFromContext — take a leading ctx and register the "+
					"release with state.RegisterCleanup (see ProviderTerminalAttach), or add "+
					"it to connStateExemptMethods with the reason it is not per connection",
				path, fileSet.Position(fn.Pos()).Line, fn.Name.Name, word,
			)
		}
	}
	// An exemption for a method that is gone, or that no longer reads as an
	// arming call, is dead prose that would quietly cover something else the
	// day the name is reused.
	var stale []string
	for name := range connStateExemptMethods {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("connStateExemptMethods names %s, which is no longer an exported arming method on *App", name)
	}
}

// armingWord reports the vocabulary word a method name carries, matched at a
// CamelCase word boundary so DeleteAttachment and ListReleases — which are
// about attachments and releases, not about arming one — do not trip.
func armingWord(name string) (string, bool) {
	for _, word := range armingVocabulary {
		for start := 0; ; {
			at := strings.Index(name[start:], word)
			if at < 0 {
				break
			}
			at += start
			end := at + len(word)
			if end == len(name) || !isLowerASCII(name[end]) {
				return word, true
			}
			start = at + 1
		}
	}
	return "", false
}

func isLowerASCII(b byte) bool { return b >= 'a' && b <= 'z' }

// isAppMethod reports whether fn is a method on *App.
func isAppMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "App"
}

// functionBodiesByName indexes every function and method declared in one file
// by the name a call site would spell it with, so a method that delegates its
// connection bookkeeping to a helper next door still satisfies the rule.
func functionBodiesByName(file *ast.File) map[string]*ast.FuncDecl {
	bodies := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			bodies[fn.Name.Name] = fn
		}
	}
	return bodies
}

// readsConnState reports whether fn reaches transport.ConnStateFromContext,
// directly or through a function declared in the same file. Same-file only:
// this is a naming guard, not a call graph, and a release that travels further
// than one file is one whose author should say so in the exemption list.
func readsConnState(fn *ast.FuncDecl, bodies map[string]*ast.FuncDecl, visited map[string]bool) bool {
	if fn == nil || fn.Body == nil || visited[fn.Name.Name] {
		return false
	}
	visited[fn.Name.Name] = true
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "transport" && fun.Sel.Name == "ConnStateFromContext" {
				found = true
				return false
			}
			if readsConnState(bodies[fun.Sel.Name], bodies, visited) {
				found = true
				return false
			}
		case *ast.Ident:
			if readsConnState(bodies[fun.Name], bodies, visited) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
