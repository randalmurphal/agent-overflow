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
		if fe := AuthorizeSessionMethod(granted, method, CallerProof{}); fe != nil {
			t.Errorf("%s with its scope granted = %#v, want admitted", method, fe)
		}
	}
}

func TestAuthorizeSessionMethodRefusesAnUngrantedScopeAndNamesIt(t *testing.T) {
	granted := []string{string(ScopeThreadsRead)}
	fe := AuthorizeSessionMethod(granted, executeMethod, CallerProof{})
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
	if fe := AuthorizeSessionMethod(nil, observeMethod, CallerProof{HostPresent: true}); fe == nil {
		t.Fatal("a session granted nothing was admitted to an observe-tier method")
	}
}

// `host` is a method property, never a grant: a session that somehow
// carried the name must still be refused when it is not on this machine,
// and admitted when it is (the embedded webview's own session names one).
func TestHostScopedMethodFollowsHostPresenceNotGrants(t *testing.T) {
	claiming := []string{string(ScopeHost)}
	fe := AuthorizeSessionMethod(claiming, hostMethod, CallerProof{})
	if fe == nil {
		t.Fatalf("%s from a remote session = admitted, want refused", hostMethod)
	}
	if fe.Code != ErrCodeScopeRequired || fe.Scope != string(ScopeHost) {
		t.Errorf("refusal = %#v, want scope_required naming host", fe)
	}
	if fe := AuthorizeSessionMethod(nil, hostMethod, CallerProof{HostPresent: true}); fe != nil {
		t.Errorf("%s from the host itself = %#v, want admitted", hostMethod, fe)
	}
}

func TestStepUpMethodNeedsAProofNoGrantCanSupply(t *testing.T) {
	everything := make([]string, 0, len(Scopes))
	for _, scope := range Scopes {
		everything = append(everything, string(scope))
	}
	fe := AuthorizeSessionMethod(everything, stepUpMethod, CallerProof{})
	if fe == nil {
		t.Fatalf("%s from a remote session holding every scope = admitted", stepUpMethod)
	}
	if fe.Code != ErrCodeStepUpRequired {
		t.Errorf("code = %q, want %q", fe.Code, ErrCodeStepUpRequired)
	}
	// Two proofs satisfy it (stepUpProven), and each one alone is enough:
	// standing at the machine, or a passkey assertion this backend just
	// verified. The second is the only one a remote owner can produce.
	if fe := AuthorizeSessionMethod(everything, stepUpMethod, CallerProof{HostPresent: true}); fe != nil {
		t.Errorf("%s with host presence = %#v, want admitted", stepUpMethod, fe)
	}
	if fe := AuthorizeSessionMethod(everything, stepUpMethod, CallerProof{StepUp: true}); fe != nil {
		t.Errorf("%s with a spent passkey token = %#v, want admitted", stepUpMethod, fe)
	}
}

// A passkey proof satisfies STEP-UP and nothing else. Host scope is a
// statement about where the caller is, and no signature can make a remote
// caller be on this machine — so a stepped-up remote session still cannot
// reach a method with no remote form.
func TestAPasskeyProofNeverStandsInForHostPresence(t *testing.T) {
	fe := AuthorizeSessionMethod(nil, hostMethod, CallerProof{StepUp: true})
	if fe == nil {
		t.Fatalf("%s reached a remote session that had stepped up", hostMethod)
	}
	if fe.Code != ErrCodeScopeRequired || fe.Scope != string(ScopeHost) {
		t.Fatalf("refusal = %#v, want the host scope refusal", fe)
	}
}

// The token is resolved ONCE per call and spent by the asking. Two
// properties ride on that: the argument-dependent recheck inside a method
// reads the answer rather than re-asking (a second ask would find the
// token gone), and a token presented on a call that did not need one is
// still consumed.
func TestAStepUpTokenIsSpentOncePerCall(t *testing.T) {
	spent := 0
	h := &connHandler{
		profile: connProfile{sessionID: "s1", isLoopback: false},
		stepUpProof: func(sessionID, token string) bool {
			spent++
			return sessionID == "s1" && token == "good"
		},
	}

	proof := h.callerProof(ClientFrame{StepUpToken: "good"})
	if !proof.StepUp {
		t.Fatal("a valid token did not prove step-up")
	}
	if spent != 1 {
		t.Fatalf("the hook was asked %d times for one call, want 1", spent)
	}

	if proof := h.callerProof(ClientFrame{StepUpToken: "wrong"}); proof.StepUp {
		t.Fatal("a token the hook refused proved step-up")
	}
	if spent != 2 {
		t.Fatalf("the hook was asked %d times, want 2 — a refused token must still be consumed", spent)
	}
}

// The session comes from the CONNECTION and never from the frame, which is
// what makes "bound to the session that asked" enforceable: a token minted
// elsewhere is presented on this socket against this socket's session.
func TestAStepUpTokenIsJudgedAgainstTheConnectionsSession(t *testing.T) {
	var asked string
	h := &connHandler{
		profile: connProfile{sessionID: "the-connections-session", isLoopback: false},
		stepUpProof: func(sessionID, token string) bool {
			asked = sessionID
			return true
		},
	}
	h.callerProof(ClientFrame{StepUpToken: "t"})
	if asked != "the-connections-session" {
		t.Fatalf("the hook was asked about %q, want the connection's session", asked)
	}
}

// A host-present caller is already proven, so presenting a token there
// would burn it for nothing — and a connection that names no session has
// nothing a token could be bound to.
func TestAStepUpTokenIsNotSpentWhenItCouldNotChangeTheAnswer(t *testing.T) {
	cases := map[string]connProfile{
		"host present":     {sessionID: "s1", isLoopback: true},
		"names no session": {isLoopback: false},
	}
	for name, profile := range cases {
		t.Run(name, func(t *testing.T) {
			asked := false
			h := &connHandler{
				profile:     profile,
				stepUpProof: func(string, string) bool { asked = true; return true },
			}
			h.callerProof(ClientFrame{StepUpToken: "t"})
			if asked {
				t.Fatal("the token was spent on a call whose answer it could not change")
			}
		})
	}
}

// A backend with no step-up hook keeps exactly the behavior it had before
// passkeys: host presence is the only proof, and a token proves nothing.
func TestNoStepUpHookLeavesHostPresenceAsTheOnlyProof(t *testing.T) {
	h := &connHandler{profile: connProfile{sessionID: "s1", isLoopback: false}}
	if h.callerProof(ClientFrame{StepUpToken: "t"}).StepUp {
		t.Fatal("a token proved step-up against a backend that cannot verify one")
	}
}

// A method no generated row covers — a receiver methodgen does not scan —
// enforces as `host`: fail-closed for a remote caller, unchanged for the
// loopback tooling those receivers exist for.
func TestUnclassifiedMethodEnforcesAsHost(t *testing.T) {
	if got := classify(unclassified).Scope; got != ScopeHost {
		t.Fatalf("classify(%s) = %q, want host", unclassified, got)
	}
	if fe := AuthorizeSessionMethod(nil, unclassified, CallerProof{}); fe == nil {
		t.Error("an unclassified method was admitted to a remote session")
	}
	if fe := AuthorizeSessionMethod(nil, unclassified, CallerProof{HostPresent: true}); fe != nil {
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
	if fe := h.authorizeSession(floorMethod, CallerProof{}); fe != nil {
		t.Fatalf("a session holding nothing was refused %s: %#v", floorMethod, fe)
	}
	if !asked {
		t.Error("the grant hook was skipped for a floor method; a revoked session would still be admitted")
	}
	// The floor moves no other answer: the same connection is still
	// refused everything its (empty) grant set does not cover.
	if fe := h.authorizeSession(executeMethod, CallerProof{}); fe == nil {
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
	if fe := h.authorizeSession(hostMethod, CallerProof{}); fe != nil {
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
	if fe := h.authorizeSession(executeMethod, CallerProof{}); fe != nil {
		t.Fatalf("connection with no grant hook refused: %#v", fe)
	}
}

func TestSessionScopedConnectionIsGatedPerCall(t *testing.T) {
	granted := []string{string(ScopeThreadsRead)}
	h := &connHandler{
		profile:       connProfile{sessionID: "s1", isLoopback: false},
		sessionScopes: func(string) ([]string, string) { return granted, "" },
	}
	if fe := h.authorizeSession(observeMethod, CallerProof{}); fe != nil {
		t.Fatalf("granted method refused: %#v", fe)
	}
	if fe := h.authorizeSession(executeMethod, CallerProof{}); fe == nil || fe.Code != ErrCodeScopeRequired {
		t.Fatalf("ungranted method = %#v, want scope_required", fe)
	}
	// The grants are re-read per call, so narrowing them takes effect on
	// the very next RPC rather than at the next watchdog tick.
	granted = nil
	if fe := h.authorizeSession(observeMethod, CallerProof{}); fe == nil {
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
	fe := h.authorizeSession(observeMethod, CallerProof{})
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

// The whole path, over a real socket from a peer that is not on this
// machine: a client attaches a step-up token to one frame, the transport
// spends it against that connection's session, and the METHOD BODY sees
// the answer. The body is the half that matters — internal/app's
// argument-dependent rechecks run there, and a proof that stopped at the
// gate would leave every one of them refusing a caller the gate admitted.
func TestAStepUpTokenReachesTheMethodBodyFromARemotePeer(t *testing.T) {
	spent := 0
	f := newAdmissionFixtureWith(t, func(cfg *Config) {
		cfg.StepUpProof = func(sessionID, token string) bool {
			spent++
			return sessionID == admissionSessionID && token == "fresh-proof"
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+f.remote+"/ws?token=admission-token",
		&websocket.DialOptions{HTTPHeader: sessionHeader()})
	if err != nil {
		t.Fatalf("dial the off-host leg: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	readFrameOfType(t, conn, frameTypeHello)

	ask := func(t *testing.T, id, token string) bool {
		t.Helper()
		frame := ClientFrame{Type: frameTypeRPC, ID: id, Method: "ReportStepUp", StepUpToken: token}
		buf, err := json.Marshal(frame)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
			t.Fatalf("write frame: %v", err)
		}
		answer := readFrameOfType(t, conn, frameTypeRPC)
		if answer.Error != nil {
			t.Fatalf("ReportStepUp refused: %#v", answer.Error)
		}
		var proven bool
		if err := json.Unmarshal(answer.Result, &proven); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		return proven
	}

	if ask(t, "1", "") {
		t.Fatal("a remote call carrying no token reported a step-up proof")
	}
	if spent != 0 {
		t.Fatalf("the hook was asked %d times for a call with no token", spent)
	}
	if !ask(t, "2", "fresh-proof") {
		t.Fatal("a valid token did not reach the method body")
	}
	if spent != 1 {
		t.Fatalf("the hook was asked %d times, want exactly one spend per call", spent)
	}
	if ask(t, "3", "some-other-token") {
		t.Fatal("a token this backend refused reported a step-up proof")
	}
}

// readFrameOfType reads until a frame of the wanted type arrives, so a
// keepalive landing mid-test is not mistaken for the answer.
func readFrameOfType(t *testing.T, conn *websocket.Conn, want string) ServerFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read %s frame: %v", want, err)
		}
		var frame ServerFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if frame.Type == want {
			return frame
		}
	}
}
