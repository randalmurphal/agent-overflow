package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// The registry is maintained by the session map's mutators, so these tests
// drive it through those mutators — every transition, not just every state. An
// entry that outlived its session would be a credential nothing could revoke.

func aoTestSession(token string, scope transport.CallerScope) session {
	return session{token: "session-" + token, aoToken: token, aoScope: scope}
}

func TestAOTokenRegistryFollowsTheSessionMap(t *testing.T) {
	app := newTestAppWithStore(t)
	app.aoTokens = map[string]transport.CallerScope{}
	manager := app.sessionManager()
	scope := transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: "thread", ProjectID: "project"}

	assertResolves := func(label, token string, want bool) {
		t.Helper()
		if _, ok := app.ResolveScopedToken(token); ok != want {
			t.Fatalf("%s: ResolveScopedToken(%q) resolved = %t, want %t", label, token, ok, want)
		}
	}

	assertResolves("before any session", "tok-1", false)
	manager.put("thread", aoTestSession("tok-1", scope))
	assertResolves("after put", "tok-1", true)
	resolved, _ := app.ResolveScopedToken("tok-1")
	if resolved.ThreadID != "thread" || resolved.Kind != transport.ScopeKindInteractive {
		t.Fatalf("resolved scope = %#v", resolved)
	}

	// take: the ordinary stop path.
	if _, ok := manager.take("thread"); !ok {
		t.Fatal("take() found no session")
	}
	assertResolves("after take", "tok-1", false)

	// put -> put (a session replacing another in place) must not leave the
	// displaced credential live.
	manager.put("thread", aoTestSession("tok-2", scope))
	manager.put("thread", aoTestSession("tok-3", scope))
	assertResolves("displaced by a second put", "tok-2", false)
	assertResolves("after the second put", "tok-3", true)

	// unregister with a stale session token must change nothing...
	if _, ok := manager.unregister("thread", "session-tok-2"); ok {
		t.Fatal("unregister() matched a stale session token")
	}
	assertResolves("after a stale unregister", "tok-3", true)
	// ... and with the live one must revoke.
	if _, ok := manager.unregister("thread", "session-tok-3"); !ok {
		t.Fatal("unregister() did not match the live session token")
	}
	assertResolves("after unregister", "tok-3", false)

	// takeIdle: the reaper path.
	manager.put("thread", aoTestSession("tok-4", scope))
	if _, ok := manager.takeIdle("thread", 1<<62); !ok {
		t.Fatal("takeIdle() found no session")
	}
	assertResolves("after takeIdle", "tok-4", false)

	// snapshotAndClear: the shutdown path, over more than one session.
	manager.put("a", aoTestSession("tok-5", scope))
	manager.put("b", aoTestSession("tok-6", scope))
	if cleared := manager.snapshotAndClear(); len(cleared) != 2 {
		t.Fatalf("snapshotAndClear() returned %d sessions, want 2", len(cleared))
	}
	assertResolves("after snapshotAndClear", "tok-5", false)
	assertResolves("after snapshotAndClear", "tok-6", false)
	if len(app.aoTokens) != 0 {
		t.Fatalf("registry retained %d entries after every session ended: %#v", len(app.aoTokens), app.aoTokens)
	}

	// A session with no credential (no transport server at mint time) must not
	// register an empty-string key that would then resolve for a blank token.
	manager.put("thread", session{token: "session-bare"})
	if _, ok := app.ResolveScopedToken(""); ok {
		t.Fatal("the empty token resolved")
	}
	if len(app.aoTokens) != 0 {
		t.Fatalf("a credential-less session registered: %#v", app.aoTokens)
	}
}

func TestSessionAOEnvIsLiveOnly(t *testing.T) {
	app := newTestAppWithStore(t)
	app.aoTokens = map[string]transport.CallerScope{}
	manager := app.sessionManager()
	if env := app.sessionAOEnv("thread"); env != nil {
		t.Fatalf("sessionAOEnv() with no session = %#v", env)
	}
	sess := aoTestSession("tok", transport.CallerScope{Kind: transport.ScopeKindInteractive})
	sess.aoEnv = map[string]string{aocli.EnvEndpoint: "http://127.0.0.1:1", aocli.EnvToken: "tok"}
	manager.put("thread", sess)
	env := app.sessionAOEnv("thread")
	if env[aocli.EnvToken] != "tok" {
		t.Fatalf("sessionAOEnv() = %#v", env)
	}
	// The returned map is a copy: mutating it must not reach the session.
	env[aocli.EnvToken] = "tampered"
	if again := app.sessionAOEnv("thread"); again[aocli.EnvToken] != "tok" {
		t.Fatalf("sessionAOEnv() handed out its own map: %#v", again)
	}
	manager.take("thread")
	if env := app.sessionAOEnv("thread"); env != nil {
		t.Fatalf("sessionAOEnv() after the session ended = %#v", env)
	}
}

func TestAOEndpointFromAppURLStripsTheWebviewToken(t *testing.T) {
	endpoint, err := aoEndpointFromAppURL("http://127.0.0.1:54321/?t=super-secret")
	if err != nil {
		t.Fatalf("aoEndpointFromAppURL() error = %v", err)
	}
	if endpoint != "http://127.0.0.1:54321" {
		t.Fatalf("aoEndpointFromAppURL() = %q", endpoint)
	}
	if strings.Contains(endpoint, "super-secret") {
		t.Fatal("the webview token survived into the CLI endpoint")
	}
	if _, err := aoEndpointFromAppURL(""); err == nil {
		t.Fatal("an empty app URL produced an endpoint")
	}
}

func TestDeriveCallerScopeByThreadKind(t *testing.T) {
	app := newTestAppWithStore(t)
	project := store.Project{ID: "scope-project", Path: t.TempDir(), Name: "Scope", CreatedAt: 1, UpdatedAt: 1}
	if err := app.store.CreateProject(project); err != nil {
		t.Fatal(err)
	}

	// A chat thread in a project is interactive.
	scope, ok, err := app.deriveCallerScope(store.Thread{ID: "chat", ProjectID: project.ID, Mode: threadmode.ModeChat})
	if err != nil || !ok {
		t.Fatalf("chat thread: ok=%t err=%v", ok, err)
	}
	if scope.Kind != transport.ScopeKindInteractive || scope.ProjectID != project.ID || len(scope.Grants) != 0 {
		t.Fatalf("chat scope = %#v", scope)
	}

	// A thread with no project gets no credential: there is nothing to scope.
	if _, ok, err := app.deriveCallerScope(store.Thread{ID: "loose", Mode: threadmode.ModeChat}); err != nil || ok {
		t.Fatalf("projectless thread: ok=%t err=%v", ok, err)
	}

	// A workflow thread carries the phase's FROZEN grants.
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		ID: "wf", Phases: []def.Phase{
			{ID: "build", Driver: def.DriverAgent, Grants: []string{string(def.GrantStartRun), "retired-grant"}},
			{ID: "verify", Driver: def.DriverAgent},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: "scope-item", ProjectID: project.ID, Goal: "g", WorkflowID: "wf", WorkflowScope: "project",
		Snapshot: snapshot, State: string(engine.StateRunning), Source: "manual", CreatedAt: 1,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: "build", Attempt: 1, ThreadID: "phase-thread", Status: "running", StartedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	scope, ok, err = app.deriveCallerScope(store.Thread{
		ID: "phase-thread", ProjectID: project.ID, Mode: threadmode.ModeWorkflow,
	})
	if err != nil || !ok {
		t.Fatalf("phase thread: ok=%t err=%v", ok, err)
	}
	if scope.Kind != transport.ScopeKindPhase || scope.ItemID != item.ID || scope.PhaseID != "build" {
		t.Fatalf("phase scope = %#v", scope)
	}
	// The unknown grant is dropped, not passed through: this build cannot
	// enforce a name it does not know.
	if len(scope.Grants) != 1 || scope.Grants[0] != string(def.GrantStartRun) {
		t.Fatalf("frozen grants = %#v", scope.Grants)
	}

	// A unit thread inherits its phase's grants through the unit row, which is
	// consulted BEFORE the phase table — a unit thread has no phase row of its
	// own, and the phase lookup would attribute it to the wrong attempt.
	if err := app.store.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: item.ID, PhaseID: "verify", Attempt: 1, UnitID: "alpha", Kind: "unit",
		ThreadID: "unit-thread", Status: "running", StartedAt: 20,
	}}); err != nil {
		t.Fatal(err)
	}
	scope, ok, err = app.deriveCallerScope(store.Thread{
		ID: "unit-thread", ProjectID: project.ID, Mode: threadmode.ModeWorkflow,
	})
	if err != nil || !ok {
		t.Fatalf("unit thread: ok=%t err=%v", ok, err)
	}
	if scope.PhaseID != "verify" || len(scope.Grants) != 0 {
		t.Fatalf("unit scope = %#v (want the verify phase's own grants)", scope)
	}

	// A workflow thread with no attempt row yet gets no credential rather than
	// an unscoped one.
	if _, ok, err := app.deriveCallerScope(store.Thread{
		ID: "unattached", ProjectID: project.ID, Mode: threadmode.ModeWorkflow,
	}); err != nil || ok {
		t.Fatalf("unattached workflow thread: ok=%t err=%v", ok, err)
	}
}

func TestFrozenPhaseGrantsRefusesAPhaseTheSnapshotDoesNotName(t *testing.T) {
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		ID: "wf", Phases: []def.Phase{{ID: "build", Driver: def.DriverAgent}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frozenPhaseGrants(snapshot, "ghost"); err == nil {
		t.Fatal("a phase absent from the snapshot resolved to no grants instead of erroring")
	}
	// An empty snapshot is a legitimate pre-freeze state, not an error.
	if grants, err := frozenPhaseGrants(nil, "build"); err != nil || grants != nil {
		t.Fatalf("frozenPhaseGrants(nil) = %#v, %v", grants, err)
	}
}

func TestSessionProcessEnvPrecedence(t *testing.T) {
	app := newTestAppWithStore(t)
	app.providerExtraEnv = map[string]string{"SHARED": "extra", "EXTRA_ONLY": "yes"}
	credential := aoSessionCredential{env: map[string]string{
		aocli.EnvToken: "tok", "SHARED": "ao",
	}}
	env := app.sessionProcessEnv(map[string]string{"SHARED": "provider", "PROVIDER_ONLY": "yes"}, credential)
	// The AO credential wins: nothing a provider config or an extra-env setting
	// says may replace the token a session was minted with.
	if env["SHARED"] != "ao" {
		t.Fatalf("SHARED = %q, want the ao credential to win", env["SHARED"])
	}
	if env[aocli.EnvToken] != "tok" || env["PROVIDER_ONLY"] != "yes" || env["EXTRA_ONLY"] != "yes" {
		t.Fatalf("merged env = %#v", env)
	}

	// An empty credential contributes nothing at all — no blank AO_* variables
	// that would make `ao` believe it is inside a session it cannot reach.
	env = app.sessionProcessEnv(nil, aoSessionCredential{})
	if _, present := env[aocli.EnvToken]; present {
		t.Fatalf("a credential-less session got %s: %#v", aocli.EnvToken, env)
	}
}

// The provider env helper is shared, so pin that it merges rather than replaces.
func TestEnvironWithMergesOverrides(t *testing.T) {
	env := provider.EnvironWith(map[string]string{"AO_TEST_ONLY": "1"})
	var found bool
	for _, entry := range env {
		if entry == "AO_TEST_ONLY=1" {
			found = true
		}
	}
	if !found {
		t.Fatal("EnvironWith() dropped the override")
	}
	if len(env) <= 1 {
		t.Fatal("EnvironWith() replaced the process environment instead of extending it")
	}
}
