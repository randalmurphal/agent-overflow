package app

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/testutil/reporoot"
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
//     `conn, err := transport.CallerConnection(ctx)`, so a call that came
//     over no connection is refused before anything is armed or spent.
//   - transport.CallerConnection is called only that way, as the first
//     statement of a bound method; what it calls takes the connection.
//   - Every such method has a row in callerConnectionRefusals, which shows
//     its refusal on an App with nothing behind it, and one in
//     connectionTieChecks, which TestArmedResourcesFollowTheirConnection
//     runs against the real resource (checkConnectionTie).
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
}

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
					t.Errorf("%s: %s arms a per-client resource (%q) and does not open with "+
						"`conn, err := transport.CallerConnection(ctx)`: take a leading ctx, call it first and tie "+
						"the release with conn.Bind where the resource is published, or add it to "+
						"notPerConnectionMethods with the reason it holds nothing per connection", position, name, word)
				}
			}
		}
		calls := callerConnectionCalls(fn.Body)
		if calls == 0 {
			continue
		}
		if !isAppMethod(fn) || !fn.Name.IsExported() || !opensWithCallerConnection(fn) || calls != 1 {
			t.Errorf("%s: %s calls transport.CallerConnection %d times, and other than once as a bound method's first "+
				"statement, `conn, err := transport.CallerConnection(ctx)`; a callee takes the connection instead, "+
				"so the refusal comes before anything is armed or spent", position, funcKey(fn), calls)
			continue
		}
		openers[name] = true
	}

	for _, table := range []struct {
		name string
		rows map[string]bool
	}{
		{"callerConnectionRefusals, which shows its refusal", methodNames(callerConnectionRefusals)},
		{"connectionTieChecks, which shows its tie on the real resource", methodNames(connectionTieChecks)},
	} {
		for name := range openers {
			if !table.rows[name] {
				t.Errorf("%s opens with transport.CallerConnection and has no row in %s", name, table.name)
			}
		}
		for name := range table.rows {
			if !openers[name] {
				t.Errorf("%s names %s, which does not open with transport.CallerConnection", table.name, name)
			}
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
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("an exemption names %s, which is no longer a method under the rule", name)
	}
}

func methodNames[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for key := range m {
		out[key] = true
	}
	return out
}

// appPackageDecl is one function or method of the package with where it is
// declared.
type appPackageDecl struct {
	fn       *ast.FuncDecl
	position token.Position
}

// appPackage is the non-test source of internal/app.
type appPackage struct {
	decls []appPackageDecl
}

func parseAppPackage(t *testing.T) *appPackage {
	t.Helper()
	fset := token.NewFileSet()
	pkg := &appPackage{}
	for _, path := range reporoot.Files(t, "internal/app", "*.go") {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := reporoot.ParseGoFile(t, fset, path, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				pkg.decls = append(pkg.decls, appPackageDecl{fn: fn, position: fset.Position(fn.Pos())})
			}
		}
	}
	return pkg
}

// isCallerConnection is a call of transport.CallerConnection.
func isCallerConnection(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "transport" && sel.Sel.Name == "CallerConnection"
}

// callerConnectionCalls counts body's calls of transport.CallerConnection,
// in closures too.
func callerConnectionCalls(body *ast.BlockStmt) int {
	calls := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isCallerConnection(call) {
			calls++
		}
		return true
	})
	return calls
}

// opensWithCallerConnection reports whether fn's first statement is
// `conn, err := transport.CallerConnection(ctx)` on its own leading
// parameter, with the error kept.
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
	if !ok || len(call.Args) != 1 || !isCallerConnection(call) {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == fn.Type.Params.List[0].Names[0].Name
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

// connectionTie is one arming method driven against the real resource it
// arms, for checkConnectionTie.
type connectionTie struct {
	// open arms one resource on ctx's connection and answers the id its
	// release names ("" when it has none).
	open func(ctx context.Context) (string, error)
	// held counts the resources of this kind that exist: the resource
	// itself (a pump handle, a mount, a recording, a claim, a window) and
	// what it alone keeps alive (a pump's lifetime context), never a
	// record of its tie.
	held func() int
	// unit is how many of held one open makes; zero means one.
	unit int
	// release gives the resource back by id from ctx's connection; nil
	// when the client has no release by id.
	release func(ctx context.Context, id string) error
	// anyCaller is whether the release lands from any connection; when
	// false, only the opening connection's release does.
	anyCaller bool
	// key is the key the resource's release is bound under; nil for a
	// release a connection binds once for its life.
	key func(id string) string
	// principal is the connection every open runs on; zero for most.
	principal transport.ConnPrincipal
}

// connectionTieChecks drive every method that opens with
// transport.CallerConnection against its real resource
// (TestArmingMethodsAreTiedToTheirConnection keeps the lists equal). Each
// builds its own App.
var connectionTieChecks = map[string]func(*testing.T) connectionTie{
	"SubscribePRUpdates":         prUpdatesConnectionTie,
	"SubscribePRList":            prListConnectionTie,
	"SubscribePipeline":          pipelineConnectionTie,
	"GitStatusSubscribe":         gitStatusConnectionTie,
	"BrowserCompanionPaneAttach": companionPaneConnectionTie,
	"BeginDictation":             dictationConnectionTie,
	"BeginLiveDictation":         liveDictationConnectionTie,
	"ProviderTerminalAttach":     providerTerminalConnectionTie,
	"WatchBackgroundTaskOutput":  backgroundTaskOutputConnectionTie,
	"GetNativeNetworkConfig":     nativeNetworkConnectionTie,
	"SubscribeTrackerBoard":      trackerBoardConnectionTie,
	"SubscribeTrackerTicket":     trackerTicketConnectionTie,
	"OpenComputerPairing": func(t *testing.T) connectionTie {
		return computerPairingConnectionTie(t, nil, func(a *App, ctx context.Context) (ComputerPairingWindow, error) {
			return a.OpenComputerPairing(ctx, "lan", "full")
		})
	},
	"OpenOwnComputerPairing": func(t *testing.T) connectionTie {
		// Own devices keep their pairings in the attached-backends store.
		attach := func(t *testing.T, a *App) {
			manager, err := newTestBackends(t, t.TempDir(), "Host", "test")
			if err != nil {
				t.Fatal(err)
			}
			SetAttachedBackends(a, manager)
		}
		return computerPairingConnectionTie(t, attach, func(a *App, ctx context.Context) (ComputerPairingWindow, error) {
			return a.OpenOwnComputerPairing(ctx, "lan")
		})
	},
	"OpenPRDiff": func(t *testing.T) connectionTie {
		return gitReviewDiffConnectionTie(t, func(a *App, ctx context.Context, f reviewDiffTieFixture) (ReviewDiffOpened, error) {
			return a.OpenPRDiff(ctx, f.ref, prRef(), f.rev, false, false)
		})
	},
	"OpenPRCommitDiff": func(t *testing.T) connectionTie {
		return gitReviewDiffConnectionTie(t, func(a *App, ctx context.Context, f reviewDiffTieFixture) (ReviewDiffOpened, error) {
			return a.OpenPRCommitDiff(ctx, f.ref, prRef(), f.rev, f.rev.HeadSHA, f.rev.HeadSHA, false, false)
		})
	},
	"OpenCommitFileDiff": func(t *testing.T) connectionTie {
		return gitReviewDiffConnectionTie(t, func(a *App, ctx context.Context, f reviewDiffTieFixture) (ReviewDiffOpened, error) {
			return a.OpenCommitFileDiff(ctx, f.ref, []string{reviewDiffTieUncommitted})
		})
	},
	"OpenWorkspaceDiff": func(t *testing.T) connectionTie {
		return gitReviewDiffConnectionTie(t, func(a *App, ctx context.Context, f reviewDiffTieFixture) (ReviewDiffOpened, error) {
			return a.OpenWorkspaceDiff(ctx, f.ref, false)
		})
	},
	"OpenBranchBaseDiff": func(t *testing.T) connectionTie {
		return gitReviewDiffConnectionTie(t, func(a *App, ctx context.Context, f reviewDiffTieFixture) (ReviewDiffOpened, error) {
			return a.OpenBranchBaseDiff(ctx, f.ref, "main", false)
		})
	},
	"OpenCommitDiff": func(t *testing.T) connectionTie {
		return gitReviewDiffConnectionTie(t, func(a *App, ctx context.Context, f reviewDiffTieFixture) (ReviewDiffOpened, error) {
			return a.OpenCommitDiff(ctx, f.ref, f.rev.HeadSHA, false)
		})
	},
	"OpenEditDiff": func(t *testing.T) connectionTie {
		return editsReviewDiffConnectionTie(t, func(a *App, ctx context.Context, threadID string) (ReviewDiffOpened, error) {
			return a.OpenEditDiff(ctx, threadID, "edit-0")
		})
	},
	"OpenTurnEditsDiff": func(t *testing.T) connectionTie {
		return editsReviewDiffConnectionTie(t, func(a *App, ctx context.Context, threadID string) (ReviewDiffOpened, error) {
			return a.OpenTurnEditsDiff(ctx, threadID, 1)
		})
	},
	"ThreadOwnerChanges": ownerChangesConnectionTie,
}

// TestArmedResourcesFollowTheirConnection runs checkConnectionTie over
// every arming method's real resource.
func TestArmedResourcesFollowTheirConnection(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, len(connectionTieChecks))
	for name := range connectionTieChecks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkConnectionTie(t, connectionTieChecks[name](t))
		})
	}
}

// checkConnectionTie proves, on the real resource, the three things a tie
// owes its connection:
//
//   - the connection's end releases what it armed;
//   - a release by id unties the connection that opened it, so a
//     long-lived connection keeps no release per resource it ever held,
//     and, unless any caller may release, another connection's release
//     lands on nothing; a release bound once for the connection's life
//     (key nil) stays one registration however often it opens and
//     releases;
//   - an open on a connection that is already closing fails with
//     transport.ErrConnectionClosing and changes nothing by the time it
//     returns: it arms nothing of its own, and another connection's live
//     resource, which an open that replaces something (a pairing window,
//     a device's activity log, the native network claim) would retire,
//     survives it.
//
// What it does not prove: each connection is an in-process ConnState
// whose end is a RunCleanups call, never a socket, so the transport's own
// teardown is the transport's tests' to prove; and a release bound for the
// connection's life (the provider terminal claims, a pairing window, the
// native network claim, a device's activity log) is proven to run at the
// connection's end and not to grow, never to be untied by a release by id,
// which by design it is not.
func checkConnectionTie(t *testing.T, tie connectionTie) {
	t.Helper()
	base, unit := tie.held(), max(tie.unit, 1)
	open := func(ctx context.Context) string {
		t.Helper()
		id, err := tie.open(ctx)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if got := tie.held(); got != base+unit {
			t.Fatalf("an open holds %d resources, want %d", got, base+unit)
		}
		return id
	}
	refuse := func(when string) {
		t.Helper()
		ctx, state := newTestConnection(t, tie.principal)
		state.RunCleanups()
		if _, err := tie.open(ctx); !errors.Is(err, transport.ErrConnectionClosing) {
			t.Fatalf("an open on a closing connection %s = %v, want transport.ErrConnectionClosing", when, err)
		}
	}

	ctx, state := newTestConnection(t, tie.principal)
	open(ctx)
	state.RunCleanups()
	awaitHeld(t, tie, base, "after the connection that opened it ended")

	if tie.release != nil {
		ctx, state = newTestConnection(t, tie.principal)
		id := open(ctx)
		bound := len(state.BoundKeys())
		other, _ := newTestConnection(t, tie.principal)
		releaser := ctx
		if tie.anyCaller {
			releaser = other
		} else {
			// Its answer is the method's own (unknown, or nothing); what
			// matters is that it released nothing.
			_ = tie.release(other, id)
			if got := tie.held(); got != base+unit || len(state.BoundKeys()) != bound {
				t.Fatalf("another connection's release landed: %d held, want %d", got, base+unit)
			}
		}
		if err := tie.release(releaser, id); err != nil {
			t.Fatalf("release: %v", err)
		}
		awaitHeld(t, tie, base, "after its release")
		if tie.key != nil {
			if state.Bound(tie.key(id)) || len(state.BoundKeys()) != bound-1 {
				t.Fatalf("the connection that opened a released resource still holds its release: %v", state.BoundKeys())
			}
		} else {
			// The connection's one registration stays one: a second open
			// and release on it bind nothing more.
			id = open(ctx)
			if err := tie.release(releaser, id); err != nil {
				t.Fatalf("release: %v", err)
			}
			awaitHeld(t, tie, base, "after its second release")
			if got := len(state.BoundKeys()); got != bound {
				t.Fatalf("a connection that opened and released twice holds %d releases, want %d: %v", got, bound, state.BoundKeys())
			}
		}
		state.RunCleanups()
	}

	// With nothing live, where an open makes everything it needs anew.
	refuse("with nothing open")
	if got := tie.held(); got != base {
		t.Fatalf("an open refused by a closing connection left %d held, want %d at once", got, base)
	}

	// Beside another connection's live resource, which a refused open must
	// leave as it was.
	liveCtx, live := newTestConnection(t, tie.principal)
	open(liveCtx)
	liveKeys := live.BoundKeys()
	refuse("beside another connection's resource")
	if got := tie.held(); got != base+unit {
		t.Fatalf("an open refused by a closing connection left %d held, want %d at once: the other connection's resource and nothing more", got, base+unit)
	}
	if keys := live.BoundKeys(); !slices.Equal(keys, liveKeys) {
		t.Fatalf("an open refused by a closing connection changed another connection's releases from %v to %v", liveKeys, keys)
	}
	live.RunCleanups()
	awaitHeld(t, tie, base, "after the other connection ended")
}

// awaitHeld waits for held to come to want: a release that removes files
// runs in the background (holdReviewDiff).
func awaitHeld(t *testing.T, tie connectionTie, want int, when string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for tie.held() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d held %s, want %d", tie.held(), when, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countedLifetime is an app lifetime (App.appCtx) that counts the contexts
// still derived from it, so a row's held can count what a pump keeps
// alive. context.WithCancel on a parent that is no cancelCtx of its own
// registers the child through the parent's AfterFunc and stops that
// registration when the child is cancelled; count is registrations not yet
// stopped. The lifetime itself never ends.
type countedLifetime struct {
	done chan struct{}
	mu   sync.Mutex
	live int
}

func newCountedLifetime() *countedLifetime { return &countedLifetime{done: make(chan struct{})} }

func (l *countedLifetime) Deadline() (time.Time, bool) { return time.Time{}, false }
func (l *countedLifetime) Done() <-chan struct{}       { return l.done }
func (l *countedLifetime) Err() error                  { return nil }
func (l *countedLifetime) Value(any) any               { return nil }

func (l *countedLifetime) AfterFunc(func()) func() bool {
	l.mu.Lock()
	l.live++
	l.mu.Unlock()
	var once sync.Once
	return func() bool {
		stopped := false
		once.Do(func() {
			l.mu.Lock()
			l.live--
			l.mu.Unlock()
			stopped = true
		})
		return stopped
	}
}

func (l *countedLifetime) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.live
}
