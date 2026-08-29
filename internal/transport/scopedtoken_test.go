package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// scopedApp is the receiver the scoped route dispatches into. It exposes one
// method from ScopedTokenMethods, one method outside it, and records the scope
// each call arrived with.
type scopedApp struct {
	mu     sync.Mutex
	scopes []CallerScope
}

func (a *scopedApp) WorkflowAgentRunStatus(ctx context.Context, itemID string) (string, error) {
	scope, ok := CallerScopeFrom(ctx)
	if !ok {
		return "", fmt.Errorf("no caller scope on the context")
	}
	a.mu.Lock()
	a.scopes = append(a.scopes, scope)
	a.mu.Unlock()
	return "status:" + itemID, nil
}

func (a *scopedApp) WorkflowAgentListRuns(ctx context.Context, active bool) ([]string, error) {
	return []string{"run"}, nil
}

// ListThreads stands in for the whole non-workflow surface: a real method, on
// the same receiver, that a scoped token must never reach.
func (a *scopedApp) ListThreads() ([]string, error) { return []string{"thread"}, nil }

func (a *scopedApp) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.scopes)
}

// tokenRegistry is a settable stand-in for the app's registry.
type tokenRegistry struct {
	mu     sync.Mutex
	tokens map[string]CallerScope
}

func newTokenRegistry() *tokenRegistry {
	return &tokenRegistry{tokens: map[string]CallerScope{}}
}

func (r *tokenRegistry) ResolveScopedToken(token string) (CallerScope, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	scope, ok := r.tokens[token]
	return scope, ok
}

func (r *tokenRegistry) put(token string, scope CallerScope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[token] = scope
}

func (r *tokenRegistry) revoke(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tokens, token)
}

func TestScopedTokenMethodsNameOnlyKnownGrants(t *testing.T) {
	// The table is written with string literals so this package stays free of
	// workflow imports; this test is what keeps those literals honest.
	for method, grants := range ScopedTokenMethods {
		if len(grants) == 0 {
			t.Errorf("%s lists no grants; a method no grant admits is unreachable from a phase", method)
		}
		for _, grant := range grants {
			if grant == GrantNotRequired {
				continue
			}
			if !def.KnownGrant(grant) {
				t.Errorf("%s requires grant %q, which is not in def's closed set %v", method, grant, def.GrantNames())
			}
		}
	}
	// GrantNotRequired must stay outside the grant vocabulary, or a workflow
	// could declare it and every method in the table would be admitted.
	if def.KnownGrant(GrantNotRequired) {
		t.Fatalf("GrantNotRequired %q is a declarable grant; a workflow could name it and admit every method",
			GrantNotRequired)
	}
}

// TestGrantNotRequiredAdmitsEveryPhaseWhateverItsGrants pins the one thing the
// marker is for: a phase whose workflow declared NO grants at all still reaches
// the methods that are part of doing the work.
func TestGrantNotRequiredAdmitsEveryPhaseWhateverItsGrants(t *testing.T) {
	ungranted := CallerScope{Kind: ScopeKindPhase, ItemID: "run", PhaseID: "implement"}
	for method, grants := range ScopedTokenMethods {
		admitted := false
		for _, grant := range grants {
			admitted = admitted || grant == GrantNotRequired
		}
		err := AuthorizeScopedMethod(ungranted, method)
		if admitted && err != nil {
			t.Errorf("%s is marked GrantNotRequired but a grantless phase was refused: %v", method, err)
		}
		if !admitted && err == nil {
			t.Errorf("%s admits a grantless phase without being marked GrantNotRequired", method)
		}
	}
}

// TestScopedTokenMethodsAreLocalOnly pins the pairing rule this package's
// AGENTS.md states in prose: a method the `ao` CLI may call is by definition a
// method that drives autonomous provider sessions, so it must also be refused
// for non-loopback WebSocket peers. The scoped route is loopback-only on its
// own, but the same method is reachable over `/ws`; without this check a new
// entry could widen the LAN surface while looking like a CLI-only change.
//
// Transitively this also proves every scoped method exists: LocalOnlyMethods is
// checked against GeneratedMethods by TestLocalOnlyMethods_AllExist.
func TestScopedTokenMethodsAreLocalOnly(t *testing.T) {
	for method := range ScopedTokenMethods {
		if !LocalOnlyMethods[method] {
			t.Errorf("ScopedTokenMethods[%q] is not in LocalOnlyMethods: a method an unattended agent session may call must not be reachable from a LAN peer either — add it to internalmethods.go", method)
		}
	}
}

func TestAuthorizeScopedMethodByKindAndGrant(t *testing.T) {
	interactive := CallerScope{Kind: ScopeKindInteractive, ThreadID: "t", ProjectID: "p"}
	granted := CallerScope{
		Kind: ScopeKindPhase, ThreadID: "t", ProjectID: "p", ItemID: "i", PhaseID: "build",
		Grants: []string{string(def.GrantStartRun)},
	}
	ungranted := CallerScope{Kind: ScopeKindPhase, ThreadID: "t", ProjectID: "p", ItemID: "i", PhaseID: "build"}

	if frameErr := AuthorizeScopedMethod(interactive, "WorkflowAgentStartRun"); frameErr != nil {
		t.Fatalf("interactive scope refused a listed method: %#v", frameErr)
	}
	if frameErr := AuthorizeScopedMethod(granted, "WorkflowAgentStartRun"); frameErr != nil {
		t.Fatalf("granted phase refused: %#v", frameErr)
	}
	// start-run also admits the per-run reads, so a phase that starts work can
	// follow it without holding introspect.
	if frameErr := AuthorizeScopedMethod(granted, "WorkflowAgentRunStatus"); frameErr != nil {
		t.Fatalf("granted phase refused run status: %#v", frameErr)
	}
	// ... but not the project-wide listing.
	frameErr := AuthorizeScopedMethod(granted, "WorkflowAgentListRuns")
	if frameErr == nil || frameErr.Code != ErrCodeGrantRequired {
		t.Fatalf("list runs for a start-run-only phase = %#v, want %s", frameErr, ErrCodeGrantRequired)
	}
	if !strings.Contains(frameErr.Message, `"introspect"`) {
		t.Fatalf("refusal does not name the missing grant: %q", frameErr.Message)
	}
	frameErr = AuthorizeScopedMethod(ungranted, "WorkflowAgentStartRun")
	if frameErr == nil || frameErr.Code != ErrCodeGrantRequired {
		t.Fatalf("ungranted phase = %#v, want %s", frameErr, ErrCodeGrantRequired)
	}
	if !strings.Contains(frameErr.Message, `"start-run"`) {
		t.Fatalf("refusal does not name the missing grant: %q", frameErr.Message)
	}

	// Both kinds are refused every method outside the table, and both are told
	// the same thing an unregistered method would say.
	for _, scope := range []CallerScope{interactive, granted} {
		for _, method := range []string{"ListThreads", "GetSettings", "OpenTerminal"} {
			frameErr := AuthorizeScopedMethod(scope, method)
			if frameErr == nil || frameErr.Code != ErrCodeMethodNotFound {
				t.Fatalf("%s scope on %s = %#v, want %s", scope.Kind, method, frameErr, ErrCodeMethodNotFound)
			}
		}
	}
}

func TestAuthorizeScopedMethodRefusesUnknownScopeKind(t *testing.T) {
	frameErr := AuthorizeScopedMethod(CallerScope{Kind: "future"}, "WorkflowAgentRunStatus")
	if frameErr == nil || frameErr.Code != ErrCodeInvalidScope {
		t.Fatalf("unknown scope kind = %#v, want %s", frameErr, ErrCodeInvalidScope)
	}
	if !strings.Contains(frameErr.Message, `"future"`) {
		t.Fatalf("unknown scope refusal does not name kind: %q", frameErr.Message)
	}
}

// TestResolveGrantAdmitsTheHumanDecisionMethods pins the separation `resolve`
// exists for: a phase that may start work and stop it is not thereby allowed to
// answer the decision its author routed to a human. The row-level half — which
// runs a resolve-granted phase may decide — is enforced by the bound methods
// (TestResolvingAParkIsConfinedToWhatAPhaseStarted, repo root).
func TestResolveGrantAdmitsTheHumanDecisionMethods(t *testing.T) {
	starter := CallerScope{
		Kind: ScopeKindPhase, ThreadID: "t", ProjectID: "p", ItemID: "i", PhaseID: "build",
		Grants: []string{string(def.GrantStartRun)},
	}
	resolver := starter
	resolver.Grants = []string{string(def.GrantStartRun), string(def.GrantResolve)}
	interactive := CallerScope{Kind: ScopeKindInteractive, ThreadID: "t", ProjectID: "p"}

	for _, method := range []string{"WorkflowResolveGate", "WorkflowAnswerQuestion"} {
		frameErr := AuthorizeScopedMethod(starter, method)
		if frameErr == nil || frameErr.Code != ErrCodeGrantRequired {
			t.Fatalf("%s for a start-run-only phase = %#v, want %s", method, frameErr, ErrCodeGrantRequired)
		}
		if !strings.Contains(frameErr.Message, `"resolve"`) {
			t.Fatalf("%s refusal does not name the missing grant: %q", method, frameErr.Message)
		}
		if frameErr := AuthorizeScopedMethod(resolver, method); frameErr != nil {
			t.Fatalf("%s for a resolve-granted phase = %#v", method, frameErr)
		}
		if frameErr := AuthorizeScopedMethod(interactive, method); frameErr != nil {
			t.Fatalf("%s for an interactive scope = %#v", method, frameErr)
		}
	}
}

// newScopedRPCServer boots a server whose only receiver is scopedApp and returns
// it with its registry.
func newScopedRPCServer(t *testing.T) (*Server, *scopedApp, *tokenRegistry) {
	t.Helper()
	app := &scopedApp{}
	registry := newTokenRegistry()
	dispatcher := NewDispatcher()
	if _, err := dispatcher.Register(app, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	server, err := New(Config{
		Dispatcher: dispatcher, EventBus: NewEventBus(DefaultRingCapacity), ScopedTokens: registry,
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return server, app, registry
}

// postScoped issues one scoped RPC and returns the HTTP status plus the frame.
func postScoped(t *testing.T, server *Server, token, method string, params ...any) (int, ServerFrame) {
	t.Helper()
	encoded := make([]json.RawMessage, 0, len(params))
	for _, param := range params {
		raw, err := json.Marshal(param)
		if err != nil {
			t.Fatalf("encode param: %v", err)
		}
		encoded = append(encoded, raw)
	}
	body, err := json.Marshal(ClientFrame{Type: "rpc", ID: "1", Method: method, Params: encoded})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+server.Addr()+ScopedRPCPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, ServerFrame{}
	}
	var frame ServerFrame
	if err := json.NewDecoder(response.Body).Decode(&frame); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.StatusCode, frame
}

func TestScopedRPCRouteAuthorizesAndRevokes(t *testing.T) {
	server, app, registry := newScopedRPCServer(t)
	phase := CallerScope{
		Kind: ScopeKindPhase, ThreadID: "thread", ProjectID: "project", ItemID: "item",
		PhaseID: "build", Grants: []string{string(def.GrantStartRun)},
	}
	registry.put("phase-token", phase)
	registry.put("chat-token", CallerScope{Kind: ScopeKindInteractive, ThreadID: "chat", ProjectID: "project"})

	// A granted method dispatches, and the scope reaches the method body.
	status, frame := postScoped(t, server, "phase-token", "WorkflowAgentRunStatus", "item")
	if status != http.StatusOK || frame.Error != nil {
		t.Fatalf("granted call = %d %#v", status, frame.Error)
	}
	if string(frame.Result) != `"status:item"` {
		t.Fatalf("granted call result = %s", frame.Result)
	}
	if app.callCount() != 1 || app.scopes[0].PhaseID != "build" {
		t.Fatalf("method did not observe the caller scope: %#v", app.scopes)
	}

	// ListThreads is a real registered method that no scoped token may reach —
	// for either kind.
	for _, token := range []string{"phase-token", "chat-token"} {
		status, frame := postScoped(t, server, token, "ListThreads")
		if status != http.StatusOK {
			t.Fatalf("%s ListThreads status = %d", token, status)
		}
		if frame.Error == nil || frame.Error.Code != ErrCodeMethodNotFound {
			t.Fatalf("%s reached ListThreads: %#v", token, frame)
		}
	}

	// A method the phase's grants do not admit is refused with the typed code.
	status, frame = postScoped(t, server, "phase-token", "WorkflowAgentListRuns", true)
	if status != http.StatusOK {
		t.Fatalf("ungranted status = %d", status)
	}
	if frame.Error == nil || frame.Error.Code != ErrCodeGrantRequired {
		t.Fatalf("ungranted call = %#v, want %s", frame, ErrCodeGrantRequired)
	}
	// The interactive token reaches the same method: the human approves it.
	status, frame = postScoped(t, server, "chat-token", "WorkflowAgentListRuns", true)
	if status != http.StatusOK || frame.Error != nil {
		t.Fatalf("interactive list runs = %d %#v", status, frame.Error)
	}

	// Revocation is what a session ending looks like. The same token that
	// worked a moment ago is now indistinguishable from a forged one.
	registry.revoke("phase-token")
	status, _ = postScoped(t, server, "phase-token", "WorkflowAgentRunStatus", "item")
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", status)
	}
	status, _ = postScoped(t, server, "", "WorkflowAgentRunStatus", "item")
	if status != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", status)
	}
	status, _ = postScoped(t, server, "never-minted", "WorkflowAgentRunStatus", "item")
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown token status = %d, want 401", status)
	}
}

func TestScopedRPCRouteRefusesUnknownScopeKind(t *testing.T) {
	server, _, registry := newScopedRPCServer(t)
	registry.put("future-token", CallerScope{Kind: "future", ThreadID: "t", ProjectID: "p"})
	status, frame := postScoped(t, server, "future-token", "WorkflowAgentRunStatus", "item")
	if status != http.StatusOK {
		t.Fatalf("unknown scope status = %d, want 200", status)
	}
	if frame.Error == nil || frame.Error.Code != ErrCodeInvalidScope {
		t.Fatalf("unknown scope frame = %#v, want %s", frame, ErrCodeInvalidScope)
	}
}

func TestScopedRPCRouteRefusesMethodIDsAndBadVerbs(t *testing.T) {
	server, _, registry := newScopedRPCServer(t)
	registry.put("chat-token", CallerScope{Kind: ScopeKindInteractive, ThreadID: "chat", ProjectID: "project"})

	// A numeric method id is not an accepted way to name a method here: the
	// allow-list is keyed by name, and honouring ids would mean keying it twice.
	body, err := json.Marshal(ClientFrame{Type: "rpc", ID: "1", MethodID: 1234})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+server.Addr()+ScopedRPCPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer chat-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	var frame ServerFrame
	if err := json.NewDecoder(response.Body).Decode(&frame); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if frame.Error == nil || frame.Error.Code != ErrCodeBadParams {
		t.Fatalf("method id call = %#v, want %s", frame, ErrCodeBadParams)
	}

	getRequest, err := http.NewRequest(http.MethodGet, "http://"+server.Addr()+ScopedRPCPath, nil)
	if err != nil {
		t.Fatalf("build GET: %v", err)
	}
	getRequest.Header.Set("Authorization", "Bearer chat-token")
	getResponse, err := http.DefaultClient.Do(getRequest)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", getResponse.StatusCode)
	}
}

// TestScopedRPCRouteAbsentWithoutRegistry pins that a server built without a
// scoped-token registry has no scoped route at all — the surface exists only
// where credentials can be minted for it.
func TestScopedRPCRouteAbsentWithoutRegistry(t *testing.T) {
	dispatcher := NewDispatcher()
	if _, err := dispatcher.Register(&scopedApp{}, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	server, err := New(Config{
		Dispatcher: dispatcher, EventBus: NewEventBus(DefaultRingCapacity),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	request, err := http.NewRequest(http.MethodPost, "http://"+server.Addr()+ScopedRPCPath, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("scoped route answered on a server with no scoped-token registry")
	}
}

// TestWebviewTokenIsNotAScopedToken pins the separation in the other direction:
// the server's own session token authenticates the WebSocket, never this route.
func TestWebviewTokenIsNotAScopedToken(t *testing.T) {
	server, _, _ := newScopedRPCServer(t)
	status, _ := postScoped(t, server, server.Token(), "WorkflowAgentRunStatus", "item")
	if status != http.StatusUnauthorized {
		t.Fatalf("session token on the scoped route = %d, want 401", status)
	}
}
