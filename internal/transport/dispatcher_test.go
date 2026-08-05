package transport

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeApp gives the dispatcher a representative method surface — at
// least one of every signature shape we care about (no-args / pointer
// receiver / context.Context / multi-return / variadic / error
// surface). The call recorder is mutex-protected because the
// concurrent-RPC test invokes methods from many goroutines at once.
type fakeApp struct {
	mu    sync.Mutex
	calls []string
}

func (a *fakeApp) record(s string) {
	a.mu.Lock()
	a.calls = append(a.calls, s)
	a.mu.Unlock()
}

// Greet has the simplest "input -> output" shape.
func (a *fakeApp) Greet(name string) string {
	a.record("Greet:" + name)
	return "hello, " + name
}

// Add returns a single non-error result. Exercises arity > 1.
func (a *fakeApp) Add(x, y int) int {
	a.record("Add")
	return x + y
}

// Maybe returns (T, error) — most common production shape.
func (a *fakeApp) Maybe(want bool) (string, error) {
	a.record("Maybe")
	if !want {
		return "", errors.New("intentionally unhappy")
	}
	return "ok", nil
}

// Save returns just an error.
func (a *fakeApp) Save(payload string) error {
	a.record("Save:" + payload)
	if payload == "fail" {
		return errors.New("save refused")
	}
	return nil
}

// WithCtx exercises context injection.
func (a *fakeApp) WithCtx(ctx context.Context, label string) string {
	a.record("WithCtx:" + label)
	if _, ok := ctx.Value(testKey{}).(string); !ok {
		return label // ctx not threaded
	}
	return label + "+ctx"
}

// Lines exercises a slice param (NOT variadic — Wails-style: methods
// accept `[]string`, the wire passes a single JSON array). This is the
// most common production shape, so it gets explicit coverage.
func (a *fakeApp) Lines(lines []string) int {
	a.record("Lines")
	return len(lines)
}

// Variadic confirms the dispatcher's variadic branch wires correctly
// even though no production App method uses it today.
func (a *fakeApp) Variadic(prefix string, parts ...string) string {
	a.record("Variadic")
	return prefix + ":" + strings.Join(parts, ",")
}

// MultiReturn returns (a, b) without a trailing error. Exercises the
// JSON-array fallback path.
func (a *fakeApp) MultiReturn(s string) (string, int) {
	a.record("MultiReturn")
	return s, len(s)
}

// Boom panics — must NOT crash the dispatcher.
func (a *fakeApp) Boom() string {
	a.record("Boom")
	panic("simulated panic")
}

// privateMethod is unexported and must not be registered.
func (a *fakeApp) privateMethod() {} //nolint:unused

type testKey struct{}

func newTestDispatcher(t *testing.T, opts RegisterOptions) (*Dispatcher, *fakeApp) {
	t.Helper()
	d := NewDispatcher()
	app := &fakeApp{}
	if _, err := d.Register(app, opts); err != nil {
		t.Fatalf("register: %v", err)
	}
	return d, app
}

func TestDispatcher_Register_PointerOnly(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(fakeApp{}, RegisterOptions{}); err == nil {
		t.Fatalf("expected non-pointer receiver to error")
	}
}

func TestDispatcher_Register_NilReceiver(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(nil, RegisterOptions{}); err == nil {
		t.Fatalf("expected nil receiver to error")
	}
}

func TestDispatcher_Register_SkipsUnexported(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	if _, ok := d.LookupName("privateMethod"); ok {
		t.Fatalf("unexported method must not be registered")
	}
	// Sanity: Greet is reachable via the same path.
	if _, ok := d.LookupName("Greet"); !ok {
		t.Fatalf("Greet should be registered")
	}
}

func TestDispatcher_Register_SkipList(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{
		Package:  "main",
		TypeName: "App",
		Skip:     map[string]bool{"Boom": true},
	})
	if _, ok := d.LookupName("Boom"); ok {
		t.Fatalf("Boom should not be registered when in Skip")
	}
	if _, ok := d.LookupName("Greet"); !ok {
		t.Fatalf("Greet should be registered")
	}
}

func TestDispatcher_Register_AllowList(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{
		Package:   "main",
		TypeName:  "App",
		AllowList: map[string]bool{"Greet": true},
	})
	if _, ok := d.LookupName("Greet"); !ok {
		t.Fatalf("Greet should be registered")
	}
	if _, ok := d.LookupName("Add"); ok {
		t.Fatalf("Add should NOT be registered (AllowList excludes it)")
	}
}

func TestDispatcher_LookupID_Resolves(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	id := fnvHash("main.App.Greet")
	m, ok := d.LookupID(id)
	if !ok {
		t.Fatalf("expected to resolve Greet by ID %d", id)
	}
	if m.FQN != "main.App.Greet" {
		t.Fatalf("unexpected FQN: %s", m.FQN)
	}
}

func TestDispatcher_LookupName_Resolves(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, ok := d.LookupName("Greet")
	if !ok {
		t.Fatalf("expected to resolve Greet by name")
	}
	if m.FQN != "main.App.Greet" {
		t.Fatalf("unexpected FQN: %s", m.FQN)
	}
}

func TestDispatcher_Resolve_FallbackToName(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	// Zero ID, valid name should still resolve.
	m, fe := d.Resolve(0, "Greet")
	if fe != nil {
		t.Fatalf("expected fallback to name, got error: %v", fe)
	}
	if m.Name != "Greet" {
		t.Fatalf("wrong method: %s", m.Name)
	}
}

func TestDispatcher_Resolve_NeitherIDNorName(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	_, fe := d.Resolve(0, "")
	if fe == nil {
		t.Fatalf("expected method-not-found")
	}
	if fe.Code != ErrCodeMethodNotFound {
		t.Fatalf("wrong code: %s", fe.Code)
	}
}

func TestDispatcher_Resolve_BothMissing(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	_, fe := d.Resolve(0xDEADBEEF, "DoesNotExist")
	if fe == nil || fe.Code != ErrCodeMethodNotFound {
		t.Fatalf("expected method-not-found, got %v", fe)
	}
	// Wire message must NOT contain the supplied id/name (info-leak guard).
	if strings.Contains(fe.Message, "DoesNotExist") || strings.Contains(fe.Message, "DEADBEEF") {
		t.Fatalf("error message leaked probe input: %q", fe.Message)
	}
}

func TestDispatcher_Invoke_SimpleCall(t *testing.T) {
	d, app := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Greet")
	result, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"world"`)})
	if fe != nil {
		t.Fatalf("invoke: %v", fe)
	}
	if string(result) != `"hello, world"` {
		t.Fatalf("unexpected result: %s", string(result))
	}
	app.mu.Lock()
	calls := append([]string(nil), app.calls...)
	app.mu.Unlock()
	if len(calls) != 1 || calls[0] != "Greet:world" {
		t.Fatalf("call did not reach receiver: %v", calls)
	}
}

func TestDispatcher_Invoke_SliceParam(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Lines")
	result, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`["a","b","c"]`)})
	if fe != nil {
		t.Fatalf("invoke: %v", fe)
	}
	if string(result) != `3` {
		t.Fatalf("expected 3, got %s", string(result))
	}
}

func TestDispatcher_Invoke_BadParams(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Greet")
	// Wrong arity.
	_, fe := d.Invoke(context.Background(), m, nil)
	if fe == nil || fe.Code != ErrCodeBadParams {
		t.Fatalf("expected bad_params for missing arg, got %v", fe)
	}
	// Wrong type.
	_, fe = d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`123`)})
	if fe == nil || fe.Code != ErrCodeBadParams {
		t.Fatalf("expected bad_params for type mismatch, got %v", fe)
	}
}

func TestDispatcher_Invoke_BadParams_DoesNotLeakInternals(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Greet")
	_, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`123`)})
	if fe == nil {
		t.Fatalf("expected bad_params")
	}
	// Wire message must NOT include json.Unmarshal's verbose internals.
	if strings.Contains(fe.Message, "json:") || strings.Contains(fe.Message, "main.App") {
		t.Fatalf("error message leaked internals: %q", fe.Message)
	}
}

func TestDispatcher_Invoke_TooManyParams(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Greet")
	huge := make([]json.RawMessage, MaxRPCParams+1)
	for i := range huge {
		huge[i] = json.RawMessage(`"x"`)
	}
	_, fe := d.Invoke(context.Background(), m, huge)
	if fe == nil || fe.Code != ErrCodeBadParams {
		t.Fatalf("expected bad_params for oversized params, got %v", fe)
	}
}

func TestDispatcher_Invoke_MethodReturnsError(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Save")
	_, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"fail"`)})
	if fe == nil {
		t.Fatalf("expected error frame")
	}
	if fe.Code != ErrCodeMethodError {
		t.Fatalf("expected method_error code (distinguishes method-returned err from panic), got %s", fe.Code)
	}
	// Method-returned errors are redacted on the wire and correlated
	// to a server-side log entry via a short ID. The original prose
	// stays out of the wire payload so a method that wraps a path /
	// secret error doesn't leak it to a LAN-attached attacker.
	if strings.Contains(fe.Message, "save refused") {
		t.Fatalf("method-returned error text leaked to wire: %q", fe.Message)
	}
	if !strings.HasPrefix(fe.Message, "method failed (id: ") {
		t.Fatalf("expected redacted message, got %q", fe.Message)
	}
}

func TestDispatcher_Invoke_MethodNoErrorReturn(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Save")
	result, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"ok"`)})
	if fe != nil {
		t.Fatalf("invoke: %v", fe)
	}
	if string(result) != `null` {
		t.Fatalf("expected null result for void return, got %s", string(result))
	}
}

func TestDispatcher_Invoke_TwoReturnsWithError(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Maybe")

	result, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`true`)})
	if fe != nil {
		t.Fatal(fe.Message)
	}
	if string(result) != `"ok"` {
		t.Fatalf("unexpected: %s", string(result))
	}

	_, fe = d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`false`)})
	if fe == nil {
		t.Fatalf("expected error path")
	}
	if fe.Code != ErrCodeMethodError {
		t.Fatalf("expected method_error code, got %s", fe.Code)
	}
	// Same redaction guarantee as TestDispatcher_Invoke_MethodReturnsError:
	// method prose stays out of the wire frame; only a correlation id
	// surfaces.
	if strings.Contains(fe.Message, "intentionally unhappy") {
		t.Fatalf("method-returned error text leaked to wire: %q", fe.Message)
	}
	if !strings.HasPrefix(fe.Message, "method failed (id: ") {
		t.Fatalf("expected redacted message, got %q", fe.Message)
	}
}

// TestDispatcher_Invoke_MethodErrorDoesNotLeakInternals pins the
// info-disclosure guard from a different angle: an error string that
// would obviously embarrass us on the wire (filesystem path) MUST be
// redacted before the FrameError leaves Invoke.
func TestDispatcher_Invoke_MethodErrorDoesNotLeakInternals(t *testing.T) {
	d := NewDispatcher()
	app := &leakyApp{}
	if _, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, _ := d.Resolve(0, "LeakPath")
	_, fe := d.Invoke(context.Background(), m, nil)
	if fe == nil {
		t.Fatalf("expected error frame")
	}
	if fe.Code != ErrCodeMethodError {
		t.Fatalf("expected method_error, got %s", fe.Code)
	}
	if strings.Contains(fe.Message, "/Users/randy/secret") {
		t.Fatalf("filesystem path leaked to wire: %q", fe.Message)
	}
	if strings.Contains(fe.Message, "file not found") {
		t.Fatalf("internal error string leaked to wire: %q", fe.Message)
	}
}

// TestDispatcher_InvokeForOrigin_LoopbackExposesError pins the dual
// of the redaction guarantee: a loopback peer (i.e. the same machine,
// the embedded webview or the user's own dev tab) gets the full
// methodErr.Error() text on the wire so the frontend can show a
// useful toast / dev-console message without users having to grep
// `make dev` output for the cid. The cost-benefit: loopback already
// has access to everything in the process; exposing the wire error
// adds no leak.
func TestDispatcher_InvokeForOrigin_LoopbackExposesError(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Save")
	_, fe := d.InvokeForOrigin(context.Background(), m, []json.RawMessage{json.RawMessage(`"fail"`)}, true)
	if fe == nil {
		t.Fatalf("expected error frame")
	}
	if fe.Code != ErrCodeMethodError {
		t.Fatalf("expected method_error, got %s", fe.Code)
	}
	// fakeApp.Save("fail") returns errors.New("save refused"). Loopback
	// callers see that text directly — no redaction.
	if fe.Message != "save refused" {
		t.Fatalf("loopback caller should see method error text, got %q", fe.Message)
	}
}

// TestDispatcher_Invoke_MethodErrorIncludesCorrelationID pins the
// "users can grep logs" half of the redaction contract: every method-
// error frame surfaces an opaque ID the operator can correlate against
// the full server-side log entry.
func TestDispatcher_Invoke_MethodErrorIncludesCorrelationID(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Save")
	_, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"fail"`)})
	if fe == nil {
		t.Fatalf("expected error frame")
	}
	// Format: "method failed (id: <11 base64url chars>)". Match
	// loosely on the suffix shape so future log-format tweaks don't
	// have to touch this test.
	pat := regexp.MustCompile(`^method failed \(id: [A-Za-z0-9_-]{4,}\)$`)
	if !pat.MatchString(fe.Message) {
		t.Fatalf("error message %q did not match expected redacted shape", fe.Message)
	}
}

// leakyApp is a focused stub for the redaction guard. Defined here
// rather than on fakeApp so the assertion is unambiguous about which
// receiver method is being exercised, and so tests for fakeApp's
// existing methods don't have to grow new branches.
type leakyApp struct{}

// LeakPath returns an error whose .Error() string contains a real-
// shaped filesystem path. The redaction layer must keep the path out
// of the wire frame.
func (l *leakyApp) LeakPath() error {
	return errors.New("/Users/randy/secret/path: file not found")
}

func TestDispatcher_Invoke_ContextInjection(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "WithCtx")
	if !m.NeedsContext {
		t.Fatalf("WithCtx should be flagged NeedsContext")
	}
	ctx := context.WithValue(context.Background(), testKey{}, "yes")
	result, fe := d.Invoke(ctx, m, []json.RawMessage{json.RawMessage(`"label"`)})
	if fe != nil {
		t.Fatal(fe.Message)
	}
	if string(result) != `"label+ctx"` {
		t.Fatalf("ctx not threaded through: %s", string(result))
	}
}

func TestDispatcher_Invoke_VariadicCollects(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Variadic")
	if !m.IsVariadic {
		t.Fatalf("Variadic should be flagged IsVariadic")
	}
	result, fe := d.Invoke(context.Background(), m, []json.RawMessage{
		json.RawMessage(`"head"`),
		json.RawMessage(`"a"`),
		json.RawMessage(`"b"`),
		json.RawMessage(`"c"`),
	})
	if fe != nil {
		t.Fatal(fe.Message)
	}
	if string(result) != `"head:a,b,c"` {
		t.Fatalf("variadic params lost: %s", string(result))
	}

	// Variadic with zero trailing params is also valid.
	result, fe = d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"head"`)})
	if fe != nil {
		t.Fatal(fe.Message)
	}
	if string(result) != `"head:"` {
		t.Fatalf("zero variadic should still produce result: %s", string(result))
	}
}

// Variadic with a wrong-type element must surface ErrCodeBadParams,
// not crash the dispatcher. Regression guard for the variadic path
// since CallSlice was introduced after the initial cut.
func TestDispatcher_Invoke_VariadicWrongElementType(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Variadic")
	_, fe := d.Invoke(context.Background(), m, []json.RawMessage{
		json.RawMessage(`"head"`),
		json.RawMessage(`123`), // expected string
	})
	if fe == nil || fe.Code != ErrCodeBadParams {
		t.Fatalf("expected bad_params for variadic type mismatch, got %v", fe)
	}
}

func TestDispatcher_Invoke_MultiReturnNoError(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "MultiReturn")
	result, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"hello"`)})
	if fe != nil {
		t.Fatal(fe.Message)
	}
	// Multi-return without trailing error -> JSON array.
	if string(result) != `["hello",5]` {
		t.Fatalf("multi-return shape wrong: %s", string(result))
	}
}

func TestDispatcher_Invoke_PanicRecover(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := d.Resolve(0, "Boom")
	_, fe := d.Invoke(context.Background(), m, nil)
	if fe == nil {
		t.Fatalf("expected error frame on panic")
	}
	if fe.Code != ErrCodeInternal {
		t.Fatalf("expected internal code, got %s", fe.Code)
	}
	// Wire message must NOT include the panic value (which could
	// contain credentials, paths, or stack info). Generic only.
	if strings.Contains(fe.Message, "simulated panic") || strings.Contains(fe.Message, "Boom") {
		t.Fatalf("panic value leaked to wire: %q", fe.Message)
	}
}

func TestDispatcher_Register_HashCollision(t *testing.T) {
	// Construct the collision artificially: two different FQN paths
	// that hash to the same value would be detected. The fakeApp
	// surface alone won't collide, but Register reports collisions
	// across the whole receiver. This test checks the error path by
	// re-registering the same receiver twice (every method collides
	// with itself).
	d := NewDispatcher()
	app := &fakeApp{}
	if _, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatal(err)
	}
	_, err := d.Register(app, RegisterOptions{Package: "main", TypeName: "App"})
	if err == nil {
		t.Fatalf("expected collision error on duplicate Register")
	}
}

func TestDispatcher_Methods_StableOrder(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	first := d.Methods()
	second := d.Methods()
	if len(first) != len(second) {
		t.Fatalf("Methods() returned different lengths across calls")
	}
	for i := range first {
		if first[i].FQN != second[i].FQN {
			t.Fatalf("Methods() not stable: idx %d differs (%s vs %s)", i, first[i].FQN, second[i].FQN)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].FQN >= first[i].FQN {
			t.Fatalf("Methods() not sorted: %s >= %s", first[i-1].FQN, first[i].FQN)
		}
	}
}

// FNV-1a verification — these IDs come straight from the generated
// frontend bindings file, so this guards the cross-language hash
// agreement at the unit level even before the integrity test runs.
func TestFnvHash_MatchesWails(t *testing.T) {
	cases := map[string]uint32{
		"main.App.AppendUIRenderTraceBatch": 2157691816,
		"main.App.ArchiveProject":           1352159878,
	}
	for fqn, want := range cases {
		got := fnvHash(fqn)
		if got != want {
			t.Errorf("fnvHash(%q) = %d, want %d (Wails compatibility broken)", fqn, got, want)
		}
	}
}

// privilegedApp is a fixture covering every entry in LocalOnlyMethods
// plus one non-privileged method (PublicEcho) so the LAN-bind tests can
// assert on the origin-aware Resolve path without wiring the real
// main.App. Each method's body is irrelevant — ResolveForOrigin refuses
// before dispatch.
//
// MAINTENANCE: when LocalOnlyMethods gains a new entry, add a matching
// stub here so TestDispatcher_LocalOnlyEnforcement_AllMethods stays
// exhaustive. The companion TestDispatcher_PrivilegedAppCoversLocalOnly
// is the catch — it fails immediately if a name is missing here.
type privilegedApp struct{}

// PublicEcho is intentionally NOT in LocalOnlyMethods so the
// loopback-allowed path stays exercised on the same dispatcher.
func (p *privilegedApp) PublicEcho() string { return "ok" }

// 1. RCE-equivalent.
func (p *privilegedApp) OpenTerminal() string                   { return "ok" }
func (p *privilegedApp) ListTerminals() string                  { return "ok" }
func (p *privilegedApp) GetTerminalReplay() string              { return "ok" }
func (p *privilegedApp) WriteTerminal() string                  { return "ok" }
func (p *privilegedApp) RestartTerminal() string                { return "ok" }
func (p *privilegedApp) CloseTerminal() string                  { return "ok" }
func (p *privilegedApp) CloseThreadTerminals() string           { return "ok" }
func (p *privilegedApp) ResizeTerminal() string                 { return "ok" }
func (p *privilegedApp) RefreshTerminal() string                { return "ok" }
func (p *privilegedApp) MoveThreadTerminals() string            { return "ok" }
func (p *privilegedApp) RetryThreadWorktreeSetup() string       { return "ok" }
func (p *privilegedApp) GetThreadWorktreeSetup() string         { return "ok" }
func (p *privilegedApp) ProviderTerminalAttach() string         { return "ok" }
func (p *privilegedApp) ProviderTerminalDetach() string         { return "ok" }
func (p *privilegedApp) ProviderTerminalReplay() string         { return "ok" }
func (p *privilegedApp) ProviderTerminalInput() string          { return "ok" }
func (p *privilegedApp) ProviderTerminalResize() string         { return "ok" }
func (p *privilegedApp) ProviderTerminalRefresh() string        { return "ok" }
func (p *privilegedApp) ProviderTerminalSetControl() string     { return "ok" }
func (p *privilegedApp) OpenInEditor() string                   { return "ok" }
func (p *privilegedApp) OpenExternalURL() string                { return "ok" }
func (p *privilegedApp) BrowseDirectory() string                { return "ok" }
func (p *privilegedApp) SavePayloadToFile() string              { return "ok" }
func (p *privilegedApp) WriteThreadWorkspaceFile() string       { return "ok" }
func (p *privilegedApp) GitPush() string                        { return "ok" }
func (p *privilegedApp) GitStatusSubscribe() string             { return "ok" }
func (p *privilegedApp) GitStatusUnsubscribe() string           { return "ok" }
func (p *privilegedApp) GetGitStatus() string                   { return "ok" }
func (p *privilegedApp) GetGitStatusFast() string               { return "ok" }
func (p *privilegedApp) GetGitStatusFastForProject() string     { return "ok" }
func (p *privilegedApp) GitCheckout() string                    { return "ok" }
func (p *privilegedApp) GitCheckoutForProject() string          { return "ok" }
func (p *privilegedApp) GitCreateBranch() string                { return "ok" }
func (p *privilegedApp) GitCreateBranchFrom() string            { return "ok" }
func (p *privilegedApp) GitCreateWorktree() string              { return "ok" }
func (p *privilegedApp) GitRemoveWorktree() string              { return "ok" }
func (p *privilegedApp) GitWorktreeStatus() string              { return "ok" }
func (p *privilegedApp) GitWorktreeStatusForProject() string    { return "ok" }
func (p *privilegedApp) GitListBranches() string                { return "ok" }
func (p *privilegedApp) GitListBranchesForProject() string      { return "ok" }
func (p *privilegedApp) GitListWorktrees() string               { return "ok" }
func (p *privilegedApp) GitListWorktreesForProject() string     { return "ok" }
func (p *privilegedApp) GitMaybeFetchRemotes() string           { return "ok" }
func (p *privilegedApp) GitMaybeFetchRemotesForProject() string { return "ok" }
func (p *privilegedApp) GitListBranchPruneCandidates() string   { return "ok" }
func (p *privilegedApp) GitPruneBranches() string               { return "ok" }
func (p *privilegedApp) GitSyncBranch() string                  { return "ok" }
func (p *privilegedApp) GitSyncBranchForProject() string        { return "ok" }
func (p *privilegedApp) RemoveOtherWorktree() string            { return "ok" }
func (p *privilegedApp) RemoveOtherWorktreeForProject() string  { return "ok" }
func (p *privilegedApp) GitCommit() string                      { return "ok" }
func (p *privilegedApp) GitPull() string                        { return "ok" }
func (p *privilegedApp) GitStageAll() string                    { return "ok" }
func (p *privilegedApp) GitCreatePR() string                    { return "ok" }
func (p *privilegedApp) GetPRDetail() string                    { return "ok" }
func (p *privilegedApp) GetPRDiff() string                      { return "ok" }
func (p *privilegedApp) GetPRMergeConflicts() string            { return "ok" }
func (p *privilegedApp) GetMergeConflictFile() string           { return "ok" }
func (p *privilegedApp) GetPRCIJobs() string                    { return "ok" }
func (p *privilegedApp) GetPRCIJobLog() string                  { return "ok" }
func (p *privilegedApp) SavePRCIJobLog() string                 { return "ok" }
func (p *privilegedApp) ListPRReviewThreads() string            { return "ok" }
func (p *privilegedApp) SubmitPRReview() string                 { return "ok" }
func (p *privilegedApp) ReplyToPRThread() string                { return "ok" }
func (p *privilegedApp) SubscribePRUpdates() string             { return "ok" }
func (p *privilegedApp) UnsubscribePRUpdates() string           { return "ok" }
func (p *privilegedApp) SetPRUpdatesActive() string             { return "ok" }
func (p *privilegedApp) PrepareThreadWorktree() string          { return "ok" }
func (p *privilegedApp) AttachThreadWorktree() string           { return "ok" }
func (p *privilegedApp) GetBranchBaseDiff() string              { return "ok" }
func (p *privilegedApp) GetWorkingTreeDiff() string             { return "ok" }
func (p *privilegedApp) GetWorkspaceCurrentDiff() string        { return "ok" }
func (p *privilegedApp) ListBranchCommits() string              { return "ok" }
func (p *privilegedApp) ListRecentCommits() string              { return "ok" }
func (p *privilegedApp) GetCommitDiff() string                  { return "ok" }
func (p *privilegedApp) ListPRCommits() string                  { return "ok" }
func (p *privilegedApp) GetPRCommitDiff() string                { return "ok" }
func (p *privilegedApp) GetDiffContextLines() string            { return "ok" }
func (p *privilegedApp) VerifyEditDiffs() string                { return "ok" }
func (p *privilegedApp) HighlightPatchWithContext() string      { return "ok" }
func (p *privilegedApp) ListDiffReviewComments() string         { return "ok" }
func (p *privilegedApp) CreateDiffReviewComment() string        { return "ok" }
func (p *privilegedApp) UpdateDiffReviewComment() string        { return "ok" }
func (p *privilegedApp) DeleteDiffReviewComment() string        { return "ok" }
func (p *privilegedApp) MarkDiffReviewCommentsSent() string     { return "ok" }
func (p *privilegedApp) SendDiffReviewComments() string         { return "ok" }
func (p *privilegedApp) GetModelsForProvider() string           { return "ok" }
func (p *privilegedApp) GetCodexSkills() string                 { return "ok" }
func (p *privilegedApp) GetClaudeSkills() string                { return "ok" }
func (p *privilegedApp) CreateProject() string                  { return "ok" }
func (p *privilegedApp) ListAvailableEditors() string           { return "ok" }
func (p *privilegedApp) GenerateCommitMessage() string          { return "ok" }
func (p *privilegedApp) SearchWorkspaceFiles() string           { return "ok" }
func (p *privilegedApp) GetPayloadPreview() string              { return "ok" }
func (p *privilegedApp) GetPayloadChunk() string                { return "ok" }
func (p *privilegedApp) GetPayloadData() string                 { return "ok" }
func (p *privilegedApp) ListPayloadMetas() string               { return "ok" }

// 2. Session control.
func (p *privilegedApp) StartSession() string                  { return "ok" }
func (p *privilegedApp) AutoResumeThread() string              { return "ok" }
func (p *privilegedApp) StopSession() string                   { return "ok" }
func (p *privilegedApp) ReconnectSession() string              { return "ok" }
func (p *privilegedApp) SendMessage() string                   { return "ok" }
func (p *privilegedApp) SendMessageWithOptions() string        { return "ok" }
func (p *privilegedApp) SteerMessageWithOptions() string       { return "ok" }
func (p *privilegedApp) SendPlanRevisionComments() string      { return "ok" }
func (p *privilegedApp) NotificationActivated() string         { return "ok" }
func (p *privilegedApp) RegisterQueueItem() string             { return "ok" }
func (p *privilegedApp) GetQueueState() string                 { return "ok" }
func (p *privilegedApp) GetThreadLiveState() string            { return "ok" }
func (p *privilegedApp) SaveDraft() string                     { return "ok" }
func (p *privilegedApp) GetDraft() string                      { return "ok" }
func (p *privilegedApp) ClearDraft() string                    { return "ok" }
func (p *privilegedApp) DeleteEmptyDraftThread() string        { return "ok" }
func (p *privilegedApp) StartDiscussion() string               { return "ok" }
func (p *privilegedApp) StartDiscussionByID() string           { return "ok" }
func (p *privilegedApp) PostChannelMessage() string            { return "ok" }
func (p *privilegedApp) ConcludeDiscussion() string            { return "ok" }
func (p *privilegedApp) WorkflowStartRun() string              { return "ok" }
func (p *privilegedApp) WorkflowCancelItem() string            { return "ok" }
func (p *privilegedApp) WorkflowResumeItem() string            { return "ok" }
func (p *privilegedApp) WorkflowAnswerQuestion() string        { return "ok" }
func (p *privilegedApp) WorkflowResolveGate() string           { return "ok" }
func (p *privilegedApp) WorkflowSetGlobalPause() string        { return "ok" }
func (p *privilegedApp) WorkflowListItems() string             { return "ok" }
func (p *privilegedApp) WorkflowListItemCosts() string         { return "ok" }
func (p *privilegedApp) WorkflowGetItem() string               { return "ok" }
func (p *privilegedApp) WorkflowCompleteTakeover() string      { return "ok" }
func (p *privilegedApp) WorkflowMergeItem() string             { return "ok" }
func (p *privilegedApp) WorkflowCreateItemPR() string          { return "ok" }
func (p *privilegedApp) WorkflowDiscardItem() string           { return "ok" }
func (p *privilegedApp) WorkflowFetchPRReviewComments() string { return "ok" }
func (p *privilegedApp) WorkflowSendPRReviewCommentsToThread() string {
	return "ok"
}
func (p *privilegedApp) WorkflowDiscussPR() string              { return "ok" }
func (p *privilegedApp) WorkflowGetJobNotes() string            { return "ok" }
func (p *privilegedApp) WorkflowGetEngineState() string         { return "ok" }
func (p *privilegedApp) WorkflowSetJobNotes() string            { return "ok" }
func (p *privilegedApp) WorkflowListDefinitions() string        { return "ok" }
func (p *privilegedApp) WorkflowRerunItem() string              { return "ok" }
func (p *privilegedApp) WorkflowRetryUnit() string              { return "ok" }
func (p *privilegedApp) WorkflowRetryFailedUnits() string       { return "ok" }
func (p *privilegedApp) WorkflowDropUnit() string               { return "ok" }
func (p *privilegedApp) WorkflowTakeOverUnit() string           { return "ok" }
func (p *privilegedApp) WorkflowAgentStartRun() string          { return "ok" }
func (p *privilegedApp) WorkflowAgentRunStatus() string         { return "ok" }
func (p *privilegedApp) WorkflowAgentRunOutput() string         { return "ok" }
func (p *privilegedApp) WorkflowAgentListRuns() string          { return "ok" }
func (p *privilegedApp) WorkflowAgentSchedule() string          { return "ok" }
func (p *privilegedApp) WorkflowAgentGetNotes() string          { return "ok" }
func (p *privilegedApp) WorkflowAgentSetNotes() string          { return "ok" }
func (p *privilegedApp) WorkflowPauseItem() string              { return "ok" }
func (p *privilegedApp) WorkflowRequestSoftStop() string        { return "ok" }
func (p *privilegedApp) WorkflowBindThread() string             { return "ok" }
func (p *privilegedApp) WorkflowUnbindThread() string           { return "ok" }
func (p *privilegedApp) WorkflowDiscardPreview() string         { return "ok" }
func (p *privilegedApp) ProjectDeletionPreview() string         { return "ok" }
func (p *privilegedApp) WorkflowCreateAutomation() string       { return "ok" }
func (p *privilegedApp) WorkflowUpdateAutomation() string       { return "ok" }
func (p *privilegedApp) WorkflowDeleteAutomation() string       { return "ok" }
func (p *privilegedApp) WorkflowSetAutomationEnabled() string   { return "ok" }
func (p *privilegedApp) WorkflowRunAutomationNow() string       { return "ok" }
func (p *privilegedApp) UpdateThreadMode() string               { return "ok" }
func (p *privilegedApp) UpdateThreadProvider() string           { return "ok" }
func (p *privilegedApp) UpdateThreadModel() string              { return "ok" }
func (p *privilegedApp) UpdateThreadModelSelection() string     { return "ok" }
func (p *privilegedApp) UpdateThreadReasoningEffort() string    { return "ok" }
func (p *privilegedApp) UpdateThreadFastMode() string           { return "ok" }
func (p *privilegedApp) UpdateThreadContextWindow() string      { return "ok" }
func (p *privilegedApp) UpdateThreadContextSettings() string    { return "ok" }
func (p *privilegedApp) UpdateThreadRuntimeMode() string        { return "ok" }
func (p *privilegedApp) UpdateThreadBranch() string             { return "ok" }
func (p *privilegedApp) UpdateThreadWorkspace() string          { return "ok" }
func (p *privilegedApp) InterruptTurn() string                  { return "ok" }
func (p *privilegedApp) InterruptAndRevertIfClean() string      { return "ok" }
func (p *privilegedApp) ListPendingInteractiveRequests() string { return "ok" }
func (p *privilegedApp) RespondToApproval() string              { return "ok" }
func (p *privilegedApp) RespondToUserInput() string             { return "ok" }
func (p *privilegedApp) CreateThread() string                   { return "ok" }
func (p *privilegedApp) CreateThreadFromPR() string             { return "ok" }
func (p *privilegedApp) GetThreadDefaults() string              { return "ok" }
func (p *privilegedApp) UpdateNewThreadDefaults() string        { return "ok" }
func (p *privilegedApp) StartTerminal() string                  { return "ok" }
func (p *privilegedApp) ForkThread() string                     { return "ok" }
func (p *privilegedApp) ForkThreadFromMessage() string          { return "ok" }
func (p *privilegedApp) RevertConversationToMessage() string    { return "ok" }
func (p *privilegedApp) StopClaudeTask() string                 { return "ok" }
func (p *privilegedApp) CleanCodexBackgroundTerminals() string  { return "ok" }

func (p *privilegedApp) TerminateCodexBackgroundTerminal() string { return "ok" }
func (p *privilegedApp) StartCodexReview() string                 { return "ok" }
func (p *privilegedApp) CompactCodexThread() string               { return "ok" }
func (p *privilegedApp) GetThreadContextUsage() string            { return "ok" }
func (p *privilegedApp) GetProviderStatuses() string              { return "ok" }
func (p *privilegedApp) ProbeClaudeAccount() string               { return "ok" }
func (p *privilegedApp) ProbeCodexAccount() string                { return "ok" }
func (p *privilegedApp) RecheckClaudeAccount() string             { return "ok" }
func (p *privilegedApp) RecheckCodexAccount() string              { return "ok" }

// 3. Settings mutation.
func (p *privilegedApp) UpdateSettings() string               { return "ok" }
func (p *privilegedApp) UpdateContextSettingsProfile() string { return "ok" }
func (p *privilegedApp) SetNetworkSettings() string           { return "ok" }
func (p *privilegedApp) AddRemoteEndpoint() string            { return "ok" }
func (p *privilegedApp) UpdateRemoteEndpoint() string         { return "ok" }
func (p *privilegedApp) DeleteRemoteEndpoint() string         { return "ok" }
func (p *privilegedApp) TouchRemoteEndpoint() string          { return "ok" }
func (p *privilegedApp) SetEditorSettings() string            { return "ok" }
func (p *privilegedApp) UpdateKeybindings() string            { return "ok" }
func (p *privilegedApp) ResetKeybindings() string             { return "ok" }
func (p *privilegedApp) SetChatBarFavorite() string           { return "ok" }
func (p *privilegedApp) SetProviderCustomEnvVar() string      { return "ok" }
func (p *privilegedApp) DeleteProviderCustomEnvVar() string   { return "ok" }
func (p *privilegedApp) GetProjectWorktreeSetup() string      { return "ok" }
func (p *privilegedApp) SetProjectWorktreeSetup() string      { return "ok" }
func (p *privilegedApp) SetWSLDistroPreference() string       { return "ok" }
func (p *privilegedApp) ReconfigureObservability() string     { return "ok" }

// 4. Attachment / payload local-FS surface.
func (p *privilegedApp) UploadAttachment() string       { return "ok" }
func (p *privilegedApp) DeleteAttachment() string       { return "ok" }
func (p *privilegedApp) GetAttachmentData() string      { return "ok" }
func (p *privilegedApp) GetAttachmentThumbnail() string { return "ok" }
func (p *privilegedApp) IngestDiagnosticBatch() string  { return "ok" }
func (p *privilegedApp) EnsureDesignWorkdir() string    { return "ok" }
func (p *privilegedApp) DismissDesignOptionSet() string { return "ok" }
func (p *privilegedApp) LatestDesignOptionSet() string  { return "ok" }
func (p *privilegedApp) GetDesignWorkdirInfo() string   { return "ok" }

// 5. Local-FS bookkeeping.
func (p *privilegedApp) AppendUIRenderTraceBatch() string { return "ok" }
func (p *privilegedApp) BookmarkUIRenderTrace() string    { return "ok" }
func (p *privilegedApp) GetUIRenderTracePath() string     { return "ok" }
func (p *privilegedApp) ReportFrontendErrorBatch() string { return "ok" }

// 6. Credential retrieval / endpoint enumeration.
func (p *privilegedApp) GetRemoteEndpointToken() string { return "ok" }
func (p *privilegedApp) ListRemoteEndpoints() string    { return "ok" }
func (p *privilegedApp) GetNetworkSettings() string     { return "ok" }

// 7. WSL inventory / preference.
func (p *privilegedApp) ListWSLDistros() string         { return "ok" }
func (p *privilegedApp) GetWSLDistroPreference() string { return "ok" }

// 8. MCP per-thread state and status.
func (p *privilegedApp) ListThreadMcpServers() string      { return "ok" }
func (p *privilegedApp) ListWorkspaceMcpServers() string   { return "ok" }
func (p *privilegedApp) SetThreadMcpServerEnabled() string { return "ok" }
func (p *privilegedApp) SetWorkspaceMcpServerEnabled() string {
	return "ok"
}
func (p *privilegedApp) ReconnectMcpServer() string     { return "ok" }
func (p *privilegedApp) GetMcpServerStatus() string     { return "ok" }
func (p *privilegedApp) ListMcpServerStatuses() string  { return "ok" }
func (p *privilegedApp) RefreshMcpServerStatus() string { return "ok" }
func (p *privilegedApp) TriggerMcpAuth() string         { return "ok" }

// 8b. Provider-reported account usage (spawns the provider CLI under the
// user's credentials and returns account-scoped data).
func (p *privilegedApp) GetCodexAccountUsage() string { return "ok" }

// 9. Native provider account credentials and login processes.
func (p *privilegedApp) ListProviderAccounts() string        { return "ok" }
func (p *privilegedApp) LoginProviderAccount() string        { return "ok" }
func (p *privilegedApp) SwitchProviderAccount() string       { return "ok" }
func (p *privilegedApp) RemoveProviderAccount() string       { return "ok" }
func (p *privilegedApp) RefreshProviderAccountUsage() string { return "ok" }

// 10. In-app self-update (network + local-FS + host-process control).
func (p *privilegedApp) CheckForUpdate() string  { return "ok" }
func (p *privilegedApp) ListReleases() string    { return "ok" }
func (p *privilegedApp) DownloadUpdate() string  { return "ok" }
func (p *privilegedApp) RestartToUpdate() string { return "ok" }

// TestDispatcher_LocalOnlyRefusedFromNonLoopback pins the LAN-bind
// safety contract on the dispatcher itself. ResolveForOrigin must
// refuse a LocalOnlyMethods entry when isLoopback is false, and the
// refusal must look like ErrCodeMethodNotFound — same shape as a probe
// of an unregistered method, so a LAN scanner can't fingerprint which
// methods are privileged.
func TestDispatcher_LocalOnlyRefusedFromNonLoopback(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Sanity: the test fixture lines up with the production
	// LocalOnlyMethods. A future rename would invalidate this test.
	if !LocalOnlyMethods["OpenTerminal"] {
		t.Fatalf("test fixture mismatch: OpenTerminal should be in LocalOnlyMethods")
	}

	_, fe := d.ResolveForOrigin(0, "OpenTerminal", false)
	if fe == nil {
		t.Fatalf("expected refusal for non-loopback caller")
	}
	if fe.Code != ErrCodeMethodNotFound {
		t.Fatalf("refusal code = %s, want %s (must be indistinguishable from missing method)", fe.Code, ErrCodeMethodNotFound)
	}
	// Wire message must be the generic "not registered" string —
	// "forbidden" or "local-only" would let an attacker fingerprint
	// privileged methods.
	if !strings.Contains(fe.Message, "not registered") {
		t.Fatalf("refusal message %q should match the generic shape", fe.Message)
	}
}

// TestDispatcher_LocalOnlyAllowedFromLoopback pins the loopback path:
// the same method that gets refused from a non-loopback peer must
// resolve cleanly when the peer is loopback. Without this guarantee a
// regression that flipped the polarity would silently break the
// embedded webview path.
func TestDispatcher_LocalOnlyAllowedFromLoopback(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	method, fe := d.ResolveForOrigin(0, "OpenTerminal", true)
	if fe != nil {
		t.Fatalf("loopback caller refused: %v", fe)
	}
	if method == nil || method.Name != "OpenTerminal" {
		t.Fatalf("loopback resolve missed method, got %+v", method)
	}
}

// TestDispatcher_NonLocalOnlyAlwaysAllowed ensures the LAN refusal is
// targeted: methods that aren't in LocalOnlyMethods stay reachable
// from non-loopback peers (the whole point of the LAN-bind feature).
func TestDispatcher_NonLocalOnlyAlwaysAllowed(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	method, fe := d.ResolveForOrigin(0, "PublicEcho", false)
	if fe != nil {
		t.Fatalf("non-loopback caller refused on non-privileged method: %v", fe)
	}
	if method == nil || method.Name != "PublicEcho" {
		t.Fatalf("resolve missed method, got %+v", method)
	}
}

// TestDispatcher_Resolve_DefaultsToLoopback documents the back-compat
// shim: the legacy single-argument Resolve treats every caller as
// loopback. The connection handler always uses ResolveForOrigin; tests
// and tooling that haven't been retrofitted keep working.
func TestDispatcher_Resolve_DefaultsToLoopback(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	method, fe := d.Resolve(0, "OpenTerminal")
	if fe != nil {
		t.Fatalf("legacy Resolve refused privileged method: %v", fe)
	}
	if method == nil || method.Name != "OpenTerminal" {
		t.Fatalf("legacy Resolve missed method, got %+v", method)
	}
}

// TestDispatcher_PrivilegedAppCoversLocalOnly is the gate that keeps
// the table-driven enforcement test honest. Every name in
// LocalOnlyMethods MUST have a matching method on the privilegedApp
// fixture; without that, ResolveForOrigin would never find the method
// to refuse, and TestDispatcher_LocalOnlyEnforcement_AllMethods would
// silently skip coverage for the missing entry.
//
// Failure here means a new LocalOnlyMethods entry was added without a
// matching stub method on privilegedApp — fix by adding the stub.
func TestDispatcher_PrivilegedAppCoversLocalOnly(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for name := range LocalOnlyMethods {
		if _, ok := d.LookupName(name); !ok {
			t.Errorf("privilegedApp is missing a stub for LocalOnlyMethods[%q]; add it so the LAN-bind enforcement test can exercise the refusal path", name)
		}
	}
}

// TestDispatcher_LocalOnlyEnforcement_AllMethods exhaustively asserts
// the LAN-bind safety contract for every entry in LocalOnlyMethods.
// Each name must:
//
//  1. Refuse a non-loopback caller with ErrCodeMethodNotFound (wire
//     shape indistinguishable from an unregistered method, so a LAN
//     scanner can't fingerprint the privileged surface).
//  2. Resolve cleanly when the caller is loopback (the embedded webview
//     path must keep working).
//
// Iterating over the production set rather than a hand-picked subset
// closes the gap where a future entry could land in LocalOnlyMethods
// but accidentally not get covered by the enforcement test.
func TestDispatcher_LocalOnlyEnforcement_AllMethods(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	for name := range LocalOnlyMethods {
		t.Run(name, func(t *testing.T) {
			// Non-loopback caller: refusal indistinguishable from an
			// unregistered method.
			_, fe := d.ResolveForOrigin(0, name, false)
			if fe == nil {
				t.Fatalf("non-loopback caller was NOT refused for %s", name)
			}
			if fe.Code != ErrCodeMethodNotFound {
				t.Fatalf("%s refusal code = %s, want %s", name, fe.Code, ErrCodeMethodNotFound)
			}
			if !strings.Contains(fe.Message, "not registered") {
				t.Fatalf("%s refusal message %q should match the generic shape", name, fe.Message)
			}

			// Loopback caller: must still resolve cleanly.
			method, fe := d.ResolveForOrigin(0, name, true)
			if fe != nil {
				t.Fatalf("loopback caller refused on %s: %v", name, fe)
			}
			if method == nil || method.Name != name {
				t.Fatalf("loopback resolve missed %s, got %+v", name, method)
			}
		})
	}
}

// TestDispatcher_ResolveForOrigin_ConcurrentLoopbackAndNonLoopback
// hammers the origin-aware Resolve from two goroutines simultaneously.
// One always passes isLoopback=true (must always succeed for
// OpenTerminal); the other always passes false (must always be
// refused). The test exists so the race detector can prove the
// LocalOnlyMethods read path is not racy under contended access — the
// production server fields requests from both classes of peer
// concurrently, so a torn read here would translate directly to a
// security regression in the field.
func TestDispatcher_ResolveForOrigin_ConcurrentLoopbackAndNonLoopback(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&privilegedApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	const iterations = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	loopbackErr := make(chan error, 1)
	nonLoopbackErr := make(chan error, 1)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			method, fe := d.ResolveForOrigin(0, "OpenTerminal", true)
			if fe != nil || method == nil || method.Name != "OpenTerminal" {
				select {
				case loopbackErr <- errors.New("loopback caller was refused / mis-resolved"):
				default:
				}
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, fe := d.ResolveForOrigin(0, "OpenTerminal", false)
			if fe == nil || fe.Code != ErrCodeMethodNotFound {
				select {
				case nonLoopbackErr <- errors.New("non-loopback caller was NOT refused with method-not-found"):
				default:
				}
				return
			}
		}
	}()

	wg.Wait()
	select {
	case err := <-loopbackErr:
		t.Fatalf("loopback path: %v", err)
	default:
	}
	select {
	case err := <-nonLoopbackErr:
		t.Fatalf("non-loopback path: %v", err)
	default:
	}
}
