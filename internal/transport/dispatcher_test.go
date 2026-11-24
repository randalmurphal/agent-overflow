package transport

import (
	"agent-overflow/internal/errorsx"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// resolveLoopback is pure method lookup for tests that do not exercise the
// host-tooling refusal (ResolveForOrigin with a remote peer).
func resolveLoopback(d *Dispatcher, id uint32, name string) (*Method, *FrameError) {
	return d.ResolveForOrigin(id, name, true)
}

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

// Transient returns a retryable method error so the dispatcher can pin the
// stable code beside its message.
func (a *fakeApp) Transient() error {
	a.record("Transient")
	return fmt.Errorf("%w: read deadline exceeded", ErrTemporarilyUnavailable)
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

// TestDispatcher_Register_PointerOnly refuses a value receiver and a nil
// one.
func TestDispatcher_Register_PointerOnly(t *testing.T) {
	for name, receiver := range map[string]any{"value receiver": fakeApp{}, "nil receiver": nil} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDispatcher().Register(receiver, RegisterOptions{}); err == nil {
				t.Fatalf("expected a %s to error", name)
			}
		})
	}
}

// TestDispatcher_Register_FiltersMethods: an unexported method, a Skip
// entry and a method outside the AllowList are each left unregistered,
// while Greet stays reachable.
func TestDispatcher_Register_FiltersMethods(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   RegisterOptions
		absent string
	}{
		{"unexported", RegisterOptions{Package: "main", TypeName: "App"}, "privateMethod"},
		{"skip list", RegisterOptions{Package: "main", TypeName: "App", Skip: map[string]bool{"Boom": true}}, "Boom"},
		{"allow list", RegisterOptions{Package: "main", TypeName: "App", AllowList: map[string]bool{"Greet": true}}, "Add"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, tc.opts)
			if _, ok := d.LookupName(tc.absent); ok {
				t.Fatalf("%s must not be registered", tc.absent)
			}
			if _, ok := d.LookupName("Greet"); !ok {
				t.Fatalf("Greet should be registered")
			}
		})
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
	m, fe := resolveLoopback(d, 0, "Greet")
	if fe != nil {
		t.Fatalf("expected fallback to name, got error: %v", fe)
	}
	if m.Name != "Greet" {
		t.Fatalf("wrong method: %s", m.Name)
	}
}

func TestDispatcher_Resolve_NeitherIDNorName(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	_, fe := resolveLoopback(d, 0, "")
	if fe == nil {
		t.Fatalf("expected method-not-found")
	}
	if fe.Code != ErrCodeMethodNotFound {
		t.Fatalf("wrong code: %s", fe.Code)
	}
}

func TestDispatcher_Resolve_BothMissing(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	_, fe := resolveLoopback(d, 0xDEADBEEF, "DoesNotExist")
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
	m, _ := resolveLoopback(d, 0, "Greet")
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
	m, _ := resolveLoopback(d, 0, "Lines")
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
	m, _ := resolveLoopback(d, 0, "Greet")
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
	// Wire message must NOT include json.Unmarshal's verbose internals.
	if strings.Contains(fe.Message, "json:") || strings.Contains(fe.Message, "main.App") {
		t.Fatalf("error message leaked internals: %q", fe.Message)
	}
}

func TestDispatcher_Invoke_TooManyParams(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := resolveLoopback(d, 0, "Greet")
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
	m, _ := resolveLoopback(d, 0, "Save")
	_, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"fail"`)})
	if fe == nil {
		t.Fatalf("expected error frame")
	}
	if fe.Code != ErrCodeMethodError {
		t.Fatalf("expected method_error code (distinguishes method-returned err from panic), got %s", fe.Code)
	}
	if fe.Message != "save refused" {
		t.Fatalf("method error text = %q, want the method's own error", fe.Message)
	}
	// The frame carries the reference for its server log entry.
	if fe.Detail == nil || !regexp.MustCompile(`^[A-Za-z0-9_-]{4,}$`).MatchString(fe.Detail.Ref) {
		t.Fatalf("detail = %+v; want a correlation id as its ref", fe.Detail)
	}
}

// A harness boot standing in for an older computer answers a method as
// that release did: its successful answer reshaped, the method's own error
// on failure, a reshape that fails as an internal error, and every other
// method as it is.
func TestDispatcher_Invoke_ReshapeRewritesASuccess(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{
		Package:  "main",
		TypeName: "App",
		Reshape: map[string]func(json.RawMessage) (json.RawMessage, error){
			"Maybe": func(answer json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"was":` + string(answer) + `}`), nil
			},
			"Add": func(json.RawMessage) (json.RawMessage, error) { return nil, errors.New("cannot reshape") },
		},
	})
	maybe, _ := resolveLoopback(d, 0, "Maybe")
	result, fe := d.Invoke(context.Background(), maybe, []json.RawMessage{json.RawMessage(`true`)})
	if fe != nil || string(result) != `{"was":"ok"}` {
		t.Fatalf("reshaped success = %s, %v; want the reshaped answer", result, fe)
	}
	if _, fe := d.Invoke(context.Background(), maybe, []json.RawMessage{json.RawMessage(`false`)}); fe == nil || fe.Message != "intentionally unhappy" {
		t.Fatalf("reshaped failure = %v, want the method's error", fe)
	}
	add, _ := resolveLoopback(d, 0, "Add")
	if _, fe := d.Invoke(context.Background(), add, []json.RawMessage{json.RawMessage(`1`), json.RawMessage(`2`)}); fe == nil || fe.Code != ErrCodeInternal {
		t.Fatalf("failed reshape = %v, want an internal error", fe)
	}
	greet, _ := resolveLoopback(d, 0, "Greet")
	if result, fe := d.Invoke(context.Background(), greet, []json.RawMessage{json.RawMessage(`"you"`)}); fe != nil || string(result) != `"hello, you"` {
		t.Fatalf("other method = %s, %v; want its result", result, fe)
	}
}

func TestDispatcher_Invoke_MethodNoErrorReturn(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := resolveLoopback(d, 0, "Save")
	result, fe := d.Invoke(context.Background(), m, []json.RawMessage{json.RawMessage(`"ok"`)})
	if fe != nil {
		t.Fatalf("invoke: %v", fe)
	}
	if string(result) != `null` {
		t.Fatalf("expected null result for void return, got %s", string(result))
	}
}

func TestDispatcher_Invoke_TemporarilyUnavailablePreservesCodeAndProse(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := resolveLoopback(d, 0, "Transient")

	_, fe := d.Invoke(context.Background(), m, nil)
	if fe == nil || fe.Code != ErrCodeTemporarilyUnavailable {
		t.Fatalf("transient error = %+v, want code %q", fe, ErrCodeTemporarilyUnavailable)
	}
	if !strings.Contains(fe.Message, "read deadline exceeded") {
		t.Fatalf("transient error hid actionable prose: %q", fe.Message)
	}
}

func TestDispatcher_Invoke_TwoReturnsWithError(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := resolveLoopback(d, 0, "Maybe")

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
	if !strings.Contains(fe.Message, "intentionally unhappy") {
		t.Fatalf("method error text = %q, want the method's own error", fe.Message)
	}
}

func TestDispatcher_Invoke_ContextInjection(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	m, _ := resolveLoopback(d, 0, "WithCtx")
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
	m, _ := resolveLoopback(d, 0, "Variadic")
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
	m, _ := resolveLoopback(d, 0, "Variadic")
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
	m, _ := resolveLoopback(d, 0, "MultiReturn")
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
	m, _ := resolveLoopback(d, 0, "Boom")
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

// shadowApp is a second receiver that redeclares fakeApp's Greet. Its
// FQN differs (different TypeName), so the ID index would accept it —
// only the name index can catch the shadowing.
type shadowApp struct{}

func (a *shadowApp) Greet(name string) string { return "shadowed, " + name }

func (a *shadowApp) ShadowOnly() string { return "shadow" }

// prefixedApp is the shape AGENTS.md prescribes for a second receiver:
// a distinctive prefix, so nothing shares a name with App.
type prefixedApp struct{}

func (a *prefixedApp) PrefixedGreet(name string) string { return "prefixed, " + name }

func TestDispatcher_Register_NameCollision(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	_, err := d.Register(&shadowApp{}, RegisterOptions{Package: "main", TypeName: "Shadow"})
	if err == nil {
		t.Fatalf("expected name collision error when a second receiver redeclares Greet")
	}
	if !strings.Contains(err.Error(), "Greet") {
		t.Fatalf("collision error should name the colliding method, got: %v", err)
	}
	// The earlier receiver must still own the name — no silent shadowing.
	m, ok := d.LookupName("Greet")
	if !ok {
		t.Fatalf("Greet should still be registered after a refused collision")
	}
	if m.FQN != "main.App.Greet" {
		t.Fatalf("Greet was shadowed: FQN is %s", m.FQN)
	}
}

func TestDispatcher_Register_DistinctNamesCoexist(t *testing.T) {
	d, _ := newTestDispatcher(t, RegisterOptions{Package: "main", TypeName: "App"})
	if _, err := d.Register(&prefixedApp{}, RegisterOptions{Package: "main", TypeName: "Prefixed"}); err != nil {
		t.Fatalf("distinct method names must register cleanly: %v", err)
	}
	m, ok := d.LookupName("PrefixedGreet")
	if !ok {
		t.Fatalf("PrefixedGreet should be registered")
	}
	if m.FQN != "main.Prefixed.PrefixedGreet" {
		t.Fatalf("unexpected FQN: %s", m.FQN)
	}
	if _, ok := d.LookupName("Greet"); !ok {
		t.Fatalf("the first receiver's Greet should still be registered")
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

// TestDispatcher_ResolveForOrigin_ConcurrentLoopbackAndNonLoopback
// hammers the origin-aware Resolve from two goroutines at once so the
// race detector can prove the read path is not racy under contended
// access. The production server fields both classes of peer
// concurrently.
func TestDispatcher_ResolveForOrigin_ConcurrentLoopbackAndNonLoopback(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&localOnlyReceiver{}, RegisterOptions{Package: "main", TypeName: "Harness", LocalOnly: true}); err != nil {
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
			method, fe := d.ResolveForOrigin(0, "Ping", true)
			if fe != nil || method == nil || method.Name != "Ping" {
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
			_, fe := d.ResolveForOrigin(0, "Ping", false)
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

type publicFailureApp struct{ err error }

func (a *publicFailureApp) Fail() error { return fmt.Errorf("private wrapper: %w", a.err) }

func TestDispatcherPublicErrorKeepsCodeAndHidesCause(t *testing.T) {
	cause := errors.New("secret /private/path?ticket=private-token")
	d := NewDispatcher()
	if _, err := d.Register(&publicFailureApp{errorsx.Public("remote_capacity", "All command slots are busy. Wait and retry.", cause)}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatal(err)
	}
	method, _ := resolveLoopback(d, 0, "Fail")
	_, frame := d.Invoke(context.Background(), method, nil)
	if frame == nil || frame.Code != "remote_capacity" || frame.Message != "All command slots are busy. Wait and retry." {
		t.Fatalf("frame = %+v", frame)
	}
}

type rateLimitRefusal struct {
	public  error
	reserve bool
	until   time.Time
}

func (e *rateLimitRefusal) Error() string                       { return e.public.Error() }
func (e *rateLimitRefusal) Unwrap() error                       { return e.public }
func (e *rateLimitRefusal) RateLimitRelease() (bool, time.Time) { return e.reserve, e.until }

// A forge rate limit refusal says whether it kept the reserve and when it
// lifts, and only under forge_rate_limited.
func TestDispatcherForgeRateLimitCarriesItsRelease(t *testing.T) {
	until := time.Date(2026, 10, 21, 9, 30, 0, 0, time.FixedZone("x", 3600))
	cases := []struct {
		code     string
		reserve  bool
		resumeAt string
	}{
		{ErrCodeForgeRateLimited, true, "2026-10-21T08:30:00Z"},
		{"forge_transient", false, ""},
	}
	for _, tc := range cases {
		d := NewDispatcher()
		refusal := &rateLimitRefusal{public: errorsx.Public(tc.code, "Paused.", nil), reserve: true, until: until}
		if _, err := d.Register(&publicFailureApp{refusal}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
			t.Fatal(err)
		}
		method, _ := resolveLoopback(d, 0, "Fail")
		_, frame := d.Invoke(context.Background(), method, nil)
		if frame == nil || frame.Code != tc.code || frame.Reserve != tc.reserve || frame.ResumeAt != tc.resumeAt {
			t.Fatalf("%s: %+v", tc.code, frame)
		}
	}
}

type agentRefusal struct {
	public error
	agents json.RawMessage
}

func (e *agentRefusal) Error() string                            { return e.public.Error() }
func (e *agentRefusal) Unwrap() error                            { return e.public }
func (e *agentRefusal) RefusedBackgroundAgents() json.RawMessage { return e.agents }

// A refused stop's agents reach the caller as backgroundAgents, and only
// under background_agents_running: another public code never carries them.
func TestDispatcherBackgroundAgentRefusalCarriesTheAgents(t *testing.T) {
	agents := json.RawMessage(`[{"launchItemId":"a1","description":"Scan","runState":"parked","transcriptRootId":"a1"}]`)
	cases := []struct {
		code string
		want string
	}{
		{ErrCodeBackgroundAgentsRunning, string(agents)},
		{"remote_capacity", ""},
	}
	for _, tc := range cases {
		d := NewDispatcher()
		refusal := &agentRefusal{public: errorsx.Public(tc.code, "Confirm to stop it.", nil), agents: agents}
		if _, err := d.Register(&publicFailureApp{refusal}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
			t.Fatal(err)
		}
		method, _ := resolveLoopback(d, 0, "Fail")
		_, frame := d.Invoke(context.Background(), method, nil)
		if frame == nil || frame.Code != tc.code || frame.Message != "Confirm to stop it." {
			t.Fatalf("%s: %+v", tc.code, frame)
		}
		if string(frame.BackgroundAgents) != tc.want {
			t.Fatalf("%s: backgroundAgents = %s, want %s", tc.code, frame.BackgroundAgents, tc.want)
		}
		wire, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if carried := strings.Contains(string(wire), `"backgroundAgents":`); carried != (tc.want != "") {
			t.Fatalf("%s: wire %s", tc.code, wire)
		}
	}
}
