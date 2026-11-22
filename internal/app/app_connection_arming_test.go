package app

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// A bound method that ARMS something for one client (a subscription, a
// recording, a mount, a watch, a held diff, a claim on a shared channel)
// owes that client's connection its release. The client's own release call
// is the ordinary path; the connection's end is the one that leaks when the
// tie is missing. TestArmingMethodsAreTiedToTheirConnection holds these
// rules over every non-test file of the package:
//
//   - An exported *App method whose name says it arms (armWords) opens with
//     `conn, err := callerConnection(ctx)`, so a call that came over no
//     connection is refused before anything is armed or spent.
//   - callerConnection is called only that way, as the first statement of a
//     bound method. Every such method reaches callerConn.bindRelease or
//     callerConn.registerRelease, where its release is tied, and has a row
//     in callerConnectionRefusals, which shows its refusal.
//   - Nothing else ties a release to a connection: BindCleanup and
//     RegisterCleanup are called only by those two, or by a function in
//     directTieFunctions with its reason.
//   - A method whose name says it releases (releaseWords) reads its own
//     call's connection, so a release by id answers for whose it is.
//
// Reaching the connection needs a leading ctx context.Context, which the
// Wails generator strips from the TS bindings, so following the rules
// changes no wire signature and no method ID.

// armWords put a method under the arming rule.
var armWords = []string{
	"Attach",
	"Subscribe",
	"Begin",
	"Watch",
	"Open",
	"Acquire",
	"Hold",
	"TakeControl",
}

// releaseWords put a method under the release rule. A release by id is
// where "from anywhere" is most often written by accident, so each one
// answers for whose resource it gives back.
var releaseWords = []string{
	"Detach",
	"Unsubscribe",
	"Unwatch",
	"Release",
	"ReleaseControl",
	"SetControl",
}

// notPerConnectionMethods are the methods a word names but no rule applies
// to, one true reason each. Every entry claims that the method holds
// nothing per connection; a wrong one is the leak this guard exists to
// catch.
var notPerConnectionMethods = map[string]string{
	"AttachThreadWorktree":      "attaches a git worktree to a thread row; the directory outlives every connection, and detaching it is a deliberate act, not a teardown",
	"BeginThreadTransfer":       "starts a transfer in the durable transfer journal, which runs to its end whoever is connected",
	"BeginDraftProjectTransfer": "starts a draft copy in the durable transfer journal, which runs to its end whoever is connected",
	"BeginPasskeyRegistration":  "starts a ceremony the session core holds in memory, single-use and expiring with identity.PasskeyCeremonyTTL",
	"BeginPasskeyStepUp":        "starts a ceremony the session core holds in memory, single-use and expiring with identity.PasskeyCeremonyTTL",
	"OpenAIKeyFile":             "\"OpenAI\" is the vendor; it reads the key file's state",
	"EditOpenAIKeyFile":         "creates the key file and opens it in the user's editor, a process of its own",
	"OpenExternalURL":           "hands an address to the system browser",
	"OpenInEditor":              "starts the user's editor, a process of its own",
	"OpenTrackerTokenPage":      "hands an address to the system browser",
	"OpenPRThread":              "creates a thread row, which outlives every connection",
	"OpenTerminal":              "starts a terminal of the thread; every client shows it, and CloseTerminal or the thread's end closes it",
	"ThreadToolWatch":           "answers one forwarded thread_status wait and holds nothing once it returns",
	"ReleaseReadAloudTicket":    "gives back a read-aloud section, which MintReadAloudTicket binds to no connection: an unspent one expires with its 30 s ticket, and its synthesis ends with the fetch request that runs it",
}

// directTieFunctions tie a release to a connection without callerConn,
// keyed "Receiver.method" or "function", with the reason the helper's
// release-at-once does not fit.
var directTieFunctions = map[string]string{
	"ownerActivity.watchLocked": "re-points one device's activity log at the latest connection under the revocation lock; the release takes that lock, and a connection already closing is tied to nothing, so there is nothing to release at once",
}

// tieMethods are the callerConn methods that tie a release.
var tieMethods = map[string]bool{"bindRelease": true, "registerRelease": true}

func TestArmingMethodsAreTiedToTheirConnection(t *testing.T) {
	t.Parallel()
	pkg := parseAppPackage(t)
	seen := map[string]bool{}
	openers := map[string]bool{}
	for _, decl := range pkg.decls {
		fn, position := decl.fn, decl.position
		name := fn.Name.Name
		if isAppMethod(fn) && fn.Name.IsExported() {
			if word, ok := nameWord(name, armWords); ok {
				seen[name] = true
				if _, exempt := notPerConnectionMethods[name]; !exempt && !opensWithCallerConnection(fn) {
					t.Errorf("%s: %s arms a per-client resource (%q) and does not open with `conn, err := callerConnection(ctx)`: "+
						"take a leading ctx, call it first and tie the release with conn.bindRelease or conn.registerRelease, "+
						"or add it to notPerConnectionMethods with the reason it holds nothing per connection", position, name, word)
				}
			} else if word, ok := nameWord(name, releaseWords); ok {
				seen[name] = true
				if _, exempt := notPerConnectionMethods[name]; !exempt && !pkg.reaches(fn, isConnStateRead) {
					t.Errorf("%s: %s releases a per-client resource (%q) without reading its call's connection "+
						"(transport.ConnStateFromContext): unbind the release on that connection, or add it to "+
						"notPerConnectionMethods with the reason it is not per connection", position, name, word)
				}
			}
		}
		if callsIdent(fn.Body, "callerConnection") && name != "callerConnection" {
			if !isAppMethod(fn) || !fn.Name.IsExported() || !opensWithCallerConnection(fn) {
				t.Errorf("%s: %s calls callerConnection other than as a bound method's first statement, "+
					"`conn, err := callerConnection(ctx)`; a callee takes the callerConn instead, "+
					"so the refusal comes before anything is armed or spent", position, funcKey(fn))
				continue
			}
			openers[name] = true
			if !pkg.reaches(fn, isTie) {
				t.Errorf("%s: %s takes the caller's connection and never ties a release to it "+
					"(conn.bindRelease or conn.registerRelease)", position, name)
			}
		}
		if _, direct := directTieFunctions[funcKey(fn)]; direct || isTieMethod(fn) {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "BindCleanup" || sel.Sel.Name == "RegisterCleanup") {
					t.Errorf("%s: %s calls %s directly; tie the release through callerConn.bindRelease or "+
						"registerRelease, which release at once when the connection is closing, or add it to "+
						"directTieFunctions with the reason that does not fit", pkg.fset.Position(call.Pos()), funcKey(fn), sel.Sel.Name)
				}
			}
			return true
		})
	}

	for name := range openers {
		if _, ok := callerConnectionRefusals[name]; !ok {
			t.Errorf("%s calls callerConnection and has no row in callerConnectionRefusals, the test that shows its refusal", name)
		}
	}
	for name := range callerConnectionRefusals {
		if !openers[name] {
			t.Errorf("callerConnectionRefusals names %s, which does not open with callerConnection", name)
		}
	}
	// An exemption for a name that is gone, or no longer carries a word, is
	// dead prose that would quietly cover something else the day the name
	// is reused.
	var stale []string
	for name := range notPerConnectionMethods {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	for key := range directTieFunctions {
		if !pkg.declared(key) {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("an exemption names %s, which is no longer a method under the rule", name)
	}
}

// appPackageDecl is one function or method of the package with where it is
// declared.
type appPackageDecl struct {
	fn       *ast.FuncDecl
	position token.Position
}

// appPackage is the non-test source of internal/app, indexed by the name a
// call site spells each function with.
type appPackage struct {
	fset   *token.FileSet
	decls  []appPackageDecl
	byName map[string][]*ast.FuncDecl
}

func parseAppPackage(t *testing.T) *appPackage {
	t.Helper()
	entries, err := os.ReadDir("internal/app")
	if err != nil {
		t.Fatalf("read internal/app: %v", err)
	}
	pkg := &appPackage{fset: token.NewFileSet(), byName: map[string][]*ast.FuncDecl{}}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join("internal/app", name)
		parsed, err := parser.ParseFile(pkg.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				pkg.decls = append(pkg.decls, appPackageDecl{fn: fn, position: pkg.fset.Position(fn.Pos())})
				pkg.byName[fn.Name.Name] = append(pkg.byName[fn.Name.Name], fn)
			}
		}
	}
	return pkg
}

// declared reports whether a function or method has key's funcKey.
func (p *appPackage) declared(key string) bool {
	for _, decl := range p.decls {
		if funcKey(decl.fn) == key {
			return true
		}
	}
	return false
}

// reaches reports whether fn, or a package function it calls, makes a call
// that found accepts. Calls resolve by name, so a method name two types
// share reaches both: the walk over-reports, never misses.
func (p *appPackage) reaches(fn *ast.FuncDecl, found func(*ast.CallExpr) bool) bool {
	visited := map[*ast.FuncDecl]bool{}
	queue := []*ast.FuncDecl{fn}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current] {
			continue
		}
		visited[current] = true
		hit := false
		ast.Inspect(current.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if hit || !ok {
				return !hit
			}
			if found(call) {
				hit = true
				return false
			}
			var name string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			case *ast.Ident:
				name = fun.Name
			}
			queue = append(queue, p.byName[name]...)
			return true
		})
		if hit {
			return true
		}
	}
	return false
}

// isConnStateRead is a call of transport.ConnStateFromContext.
func isConnStateRead(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "transport" && sel.Sel.Name == "ConnStateFromContext"
}

// isTie is a call of a callerConn tie method, or of a function that ties
// directly.
func isTie(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if tieMethods[sel.Sel.Name] {
		return true
	}
	for key := range directTieFunctions {
		if strings.HasSuffix(key, "."+sel.Sel.Name) {
			return true
		}
	}
	return false
}

// isTieMethod reports whether fn is one of callerConn's tie methods, the
// only callers of BindCleanup and RegisterCleanup the rule admits.
func isTieMethod(fn *ast.FuncDecl) bool {
	return tieMethods[fn.Name.Name] && receiverName(fn) == "callerConn"
}

// opensWithCallerConnection reports whether fn's first statement is
// `conn, err := callerConnection(ctx)` on its own leading parameter, with
// the error kept.
func opensWithCallerConnection(fn *ast.FuncDecl) bool {
	if len(fn.Body.List) == 0 || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 || len(fn.Type.Params.List[0].Names) == 0 {
		return false
	}
	assign, ok := fn.Body.List[0].(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
		return false
	}
	if errName, ok := assign.Lhs[1].(*ast.Ident); !ok || errName.Name == "_" {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	callee, ok := call.Fun.(*ast.Ident)
	if !ok || callee.Name != "callerConnection" {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == fn.Type.Params.List[0].Names[0].Name
}

// callsIdent reports whether body calls the package function name.
func callsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// funcKey names fn as "Receiver.method", or by its name for a function.
func funcKey(fn *ast.FuncDecl) string {
	if receiver := receiverName(fn); receiver != "" {
		return receiver + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// receiverName is the type name of fn's receiver, or "" for a function.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if ident, ok := typ.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// nameWord reports the word of words a method name carries, matched at a
// CamelCase word boundary so DeleteAttachment and ListReleases, which are
// about attachments and releases rather than arming one, do not match.
func nameWord(name string, words []string) (string, bool) {
	for _, word := range words {
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

// byIDTie is a subscription the client can also end by id.
type byIDTie struct {
	subscribe   func(context.Context) (id string, err error)
	unsubscribe func(ctx context.Context, id string) error
	key         func(id string) string
	live        func(id string) bool
	// principal is the connection the subscribe runs on; zero for most.
	principal transport.ConnPrincipal
}

// checkByIDTie proves that the subscribe binds its release to the connection
// under the subscription's key and that the unsubscribe removes what is bound
// there, so a long-lived connection keeps no cleanup per subscription it ever
// made.
func checkByIDTie(t *testing.T, tie byIDTie) {
	t.Helper()
	ctx, state := transport.WithConnState(context.Background(), tie.principal)
	id, err := tie.subscribe(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	state.UnbindCleanup(tie.key(id))
	state.RunCleanups()
	if !tie.live(id) {
		t.Fatal("the connection's end released a subscription whose key was unbound: its tie is bound elsewhere")
	}
	if err := tie.unsubscribe(ctx, id); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}

	ctx, state = transport.WithConnState(context.Background(), tie.principal)
	if id, err = tie.subscribe(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if !state.BindCleanup(tie.key(id), func() { t.Error("the unsubscribe left a cleanup bound under its key") }) {
		t.Fatal("bind refused on an open connection")
	}
	if err := tie.unsubscribe(ctx, id); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if tie.live(id) {
		t.Fatal("the unsubscribe did not release the subscription")
	}
	state.RunCleanups()
}
