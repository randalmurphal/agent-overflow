package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Every case below names a REAL method out of the generated table, so a
// re-annotation that changes what one of them requires fails here rather
// than silently re-scoping the assertion.
const (
	observeMethod  = "ListThreads"                // threads:read
	executeMethod  = "RenameThread"               // threads:operate
	hostMethod     = "BrowseDirectory"            // host
	stepUpMethod   = "SetNetworkSettings"         // settings:write + //ao:stepup
	settingsGetter = "GetSettings"                // settings:read
	floorMethod    = "SetUIState"                 // session: the floor
	unclassified   = "HarnessSomethingUnbindable" // no row: enforces as host
)

func TestAuthorizeSessionMethodAdmitsAGrantedScope(t *testing.T) {
	granted := []string{string(ScopeThreadsRead), string(ScopeSettingsRead)}
	for _, method := range []string{observeMethod, settingsGetter} {
		if fe := AuthorizeSessionMethod(granted, method, false); fe != nil {
			t.Errorf("%s with its scope granted = %#v, want admitted", method, fe)
		}
	}
}

func TestAuthorizeSessionMethodRefusesAnUngrantedScopeAndNamesIt(t *testing.T) {
	granted := []string{string(ScopeThreadsRead)}
	fe := AuthorizeSessionMethod(granted, executeMethod, false)
	if fe == nil {
		t.Fatalf("%s with only threads:read = admitted, want refused", executeMethod)
	}
	if fe.Code != ErrCodeScopeRequired {
		t.Errorf("code = %q, want %q", fe.Code, ErrCodeScopeRequired)
	}
	// The NAME is the actionable half and rides a field, because a method
	// error's prose does not survive the wire for a non-loopback caller.
	if fe.Scope != string(ScopeThreadsOperate) {
		t.Errorf("scope = %q, want %q", fe.Scope, ScopeThreadsOperate)
	}
}

// An empty grant set is a real answer — a session narrowed to nothing —
// and must refuse rather than read as "no restrictions".
func TestAuthorizeSessionMethodRefusesAnEmptyGrantSet(t *testing.T) {
	if fe := AuthorizeSessionMethod(nil, observeMethod, true); fe == nil {
		t.Fatal("a session granted nothing was admitted to an observe-tier method")
	}
}

// `host` is a method property, never a grant: a session that somehow
// carried the name must still be refused when it is not on this machine,
// and admitted when it is (the embedded webview's own session names one).
func TestHostScopedMethodFollowsHostPresenceNotGrants(t *testing.T) {
	claiming := []string{string(ScopeHost)}
	fe := AuthorizeSessionMethod(claiming, hostMethod, false)
	if fe == nil {
		t.Fatalf("%s from a remote session = admitted, want refused", hostMethod)
	}
	if fe.Code != ErrCodeScopeRequired || fe.Scope != string(ScopeHost) {
		t.Errorf("refusal = %#v, want scope_required naming host", fe)
	}
	if fe := AuthorizeSessionMethod(nil, hostMethod, true); fe != nil {
		t.Errorf("%s from the host itself = %#v, want admitted", hostMethod, fe)
	}
}

func TestStepUpMethodNeedsAProofNoGrantCanSupply(t *testing.T) {
	everything := make([]string, 0, len(Scopes))
	for _, scope := range Scopes {
		everything = append(everything, string(scope))
	}
	fe := AuthorizeSessionMethod(everything, stepUpMethod, false)
	if fe == nil {
		t.Fatalf("%s from a remote session holding every scope = admitted", stepUpMethod)
	}
	if fe.Code != ErrCodeStepUpRequired {
		t.Errorf("code = %q, want %q", fe.Code, ErrCodeStepUpRequired)
	}
	// Host presence IS the proof this phase (stepUpProven), so the same
	// call from this machine goes through.
	if fe := AuthorizeSessionMethod(everything, stepUpMethod, true); fe != nil {
		t.Errorf("%s with host presence = %#v, want admitted", stepUpMethod, fe)
	}
}

// A method no generated row covers — a receiver methodgen does not scan —
// enforces as `host`: fail-closed for a remote caller, unchanged for the
// loopback tooling those receivers exist for.
func TestUnclassifiedMethodEnforcesAsHost(t *testing.T) {
	if got := classify(unclassified).Scope; got != ScopeHost {
		t.Fatalf("classify(%s) = %q, want host", unclassified, got)
	}
	if fe := AuthorizeSessionMethod(nil, unclassified, false); fe == nil {
		t.Error("an unclassified method was admitted to a remote session")
	}
	if fe := AuthorizeSessionMethod(nil, unclassified, true); fe != nil {
		t.Errorf("an unclassified method from the host = %#v, want admitted", fe)
	}
}

// The floor over the wire: a session that was granted NOTHING still
// writes its own ui_state bucket and its own device-tier settings, which
// is the case the `session` scope exists for. The grant hook is still
// consulted — a revoked session must refuse here like anywhere else — and
// its empty answer admits.
func TestSessionFloorAdmitsAConnectionWithNoGrants(t *testing.T) {
	asked := false
	h := &connHandler{
		profile: connProfile{sessionID: "s1", isLoopback: false},
		sessionScopes: func(string) ([]string, string) {
			asked = true
			return nil, ""
		},
	}
	if fe := h.authorizeSession(floorMethod); fe != nil {
		t.Fatalf("a session holding nothing was refused %s: %#v", floorMethod, fe)
	}
	if !asked {
		t.Error("the grant hook was skipped for a floor method; a revoked session would still be admitted")
	}
	// The floor moves no other answer: the same connection is still
	// refused everything its (empty) grant set does not cover.
	if fe := h.authorizeSession(executeMethod); fe == nil {
		t.Errorf("a session holding nothing reached %s", executeMethod)
	}
}

// The migration window's other half: a connection carrying only the launch
// credential names no session, so the scope gate has nothing to compare
// and must not invent an answer.
func TestConnectionNamingNoSessionSkipsTheScopeGate(t *testing.T) {
	asked := false
	h := &connHandler{
		profile: connProfile{isLoopback: false},
		sessionScopes: func(string) ([]string, string) {
			asked = true
			return nil, ""
		},
	}
	if fe := h.authorizeSession(hostMethod); fe != nil {
		t.Fatalf("launch-credential connection refused %s: %#v", hostMethod, fe)
	}
	if asked {
		t.Error("the grant hook was consulted for a connection that named no session")
	}
}

// Same for a process that cannot resolve grants at all: the origin gate
// stays the only judge, which is what it was before enforcement.
func TestConnectionWithNoGrantHookSkipsTheScopeGate(t *testing.T) {
	h := &connHandler{profile: connProfile{sessionID: "s1", isLoopback: false}}
	if fe := h.authorizeSession(executeMethod); fe != nil {
		t.Fatalf("connection with no grant hook refused: %#v", fe)
	}
}

func TestSessionScopedConnectionIsGatedPerCall(t *testing.T) {
	granted := []string{string(ScopeThreadsRead)}
	h := &connHandler{
		profile:       connProfile{sessionID: "s1", isLoopback: false},
		sessionScopes: func(string) ([]string, string) { return granted, "" },
	}
	if fe := h.authorizeSession(observeMethod); fe != nil {
		t.Fatalf("granted method refused: %#v", fe)
	}
	if fe := h.authorizeSession(executeMethod); fe == nil || fe.Code != ErrCodeScopeRequired {
		t.Fatalf("ungranted method = %#v, want scope_required", fe)
	}
	// The grants are re-read per call, so narrowing them takes effect on
	// the very next RPC rather than at the next watchdog tick.
	granted = nil
	if fe := h.authorizeSession(observeMethod); fe == nil {
		t.Fatal("a call after the grants were withdrawn was still admitted")
	}
}

// A session that stopped admitting work keeps the CREDENTIAL channel's
// refusal shape, reason and all — it is not a scope problem, and telling a
// client to acquire a scope would send it to the wrong remedy.
func TestRevokedSessionRefusesWithTheCredentialShape(t *testing.T) {
	h := &connHandler{
		profile:       connProfile{sessionID: "s1", isLoopback: true},
		sessionScopes: func(string) ([]string, string) { return nil, "revoked_session" },
	}
	fe := h.authorizeSession(observeMethod)
	if fe == nil {
		t.Fatal("a revoked session was admitted")
	}
	if fe.Code != ErrCodeAuthFailed || fe.Reason != "revoked_session" {
		t.Fatalf("refusal = %#v, want auth_failed/revoked_session", fe)
	}
}

// scopeGateStub answers to a real generated method name so the wire test
// exercises a real classification rather than the unclassified default.
type scopeGateStub struct{}

func (s *scopeGateStub) ListThreads() ([]string, error) { return []string{"t1"}, nil }

func (s *scopeGateStub) RenameThread(id, title string) error { return nil }

// A method that refuses on its ARGUMENTS, which is the shape the autonomy
// and settings-tier rechecks use.
func (s *scopeGateStub) UpdateThreadRuntimeMode(id, mode string) error {
	if mode == "full-access" {
		return ScopeRequired(ScopeThreadsAutonomy, "runtime mode \"full-access\"")
	}
	return nil
}

// scopeGateFixture is the integration fixture with the session hooks the
// enforcement wave added: a connection that names a session, and grants
// the test controls between calls.
func scopeGateFixture(t *testing.T, granted *[]string) (string, string) {
	t.Helper()
	d := NewDispatcher()
	if _, err := d.Register(&scopeGateStub{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv, err := New(Config{
		Dispatcher: d,
		EventBus:   NewEventBus(16),
		Token:      "scope-gate-token",
		SessionForRequest: func(*http.Request) (string, bool) {
			return "session-under-test", true
		},
		SessionLive: func(string) bool { return true },
		SessionScopes: func(string) ([]string, string) {
			return *granted, ""
		},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv.Addr(), "scope-gate-token"
}

func TestScopeGateRefusesOverTheWire(t *testing.T) {
	granted := []string{string(ScopeThreadsRead)}
	addr, token := scopeGateFixture(t, &granted)
	conn, _, err := websocket.Dial(context.Background(), "ws://"+addr+"/ws?token="+token, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	if got := callRPC(t, conn, "ListThreads"); got.Error != nil {
		t.Fatalf("granted call = %#v, want a result", got.Error)
	}
	refused := callRPC(t, conn, "RenameThread", "t1", "new title")
	if refused.Error == nil || refused.Error.Code != ErrCodeScopeRequired {
		t.Fatalf("ungranted call = %#v, want scope_required", refused.Error)
	}
	if refused.Error.Scope != string(ScopeThreadsOperate) {
		t.Fatalf("refusal named scope %q, want threads:operate", refused.Error.Scope)
	}

	// The method's OWN refusal — the argument recheck — reaches the wire
	// with the same code and its message intact.
	granted = []string{string(ScopeThreadsRead), string(ScopeThreadsOperate)}
	argRefused := callRPC(t, conn, "UpdateThreadRuntimeMode", "t1", "full-access")
	if argRefused.Error == nil || argRefused.Error.Code != ErrCodeScopeRequired {
		t.Fatalf("argument recheck = %#v, want scope_required", argRefused.Error)
	}
	if argRefused.Error.Scope != string(ScopeThreadsAutonomy) {
		t.Fatalf("argument recheck named scope %q, want threads:autonomy", argRefused.Error.Scope)
	}
	if !strings.Contains(argRefused.Error.Message, "full-access") {
		t.Fatalf("argument recheck message %q lost the argument it refused", argRefused.Error.Message)
	}
}

// An in-method refusal must NOT go through the correlation-id redaction a
// non-loopback caller gets for ordinary errors: the message is the answer.
func TestArgumentRefusalSurvivesRedaction(t *testing.T) {
	d := NewDispatcher()
	if _, err := d.Register(&scopeGateStub{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	method, fe := d.ResolveForOrigin(0, "UpdateThreadRuntimeMode", true)
	if fe != nil {
		t.Fatalf("resolve: %#v", fe)
	}
	params := []json.RawMessage{json.RawMessage(`"t1"`), json.RawMessage(`"full-access"`)}
	_, frameErr := d.InvokeForOrigin(context.Background(), method, params, false)
	if frameErr == nil {
		t.Fatal("the argument recheck did not refuse")
	}
	if frameErr.Code != ErrCodeScopeRequired || frameErr.Scope != string(ScopeThreadsAutonomy) {
		t.Fatalf("refusal = %#v, want scope_required naming threads:autonomy", frameErr)
	}
	if strings.Contains(frameErr.Message, "method failed") {
		t.Fatalf("the refusal was redacted to %q", frameErr.Message)
	}
}

func TestAuthzErrorsAreRecognizedThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("apply the patch: %w", StepUpRequired(`settings key "network"`))
	frame, ok := AuthzFrame(wrapped)
	if !ok {
		t.Fatal("a wrapped step-up refusal was not recognized")
	}
	if frame.Code != ErrCodeStepUpRequired {
		t.Fatalf("code = %q, want %q", frame.Code, ErrCodeStepUpRequired)
	}
	if _, ok := AuthzFrame(errors.New("ordinary failure")); ok {
		t.Fatal("an ordinary error was read as an authorization refusal")
	}
}
