package app

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

func TestDeriveCallerScopeByThreadKind(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	project := store.Project{ID: "scope-project", Path: t.TempDir(), Name: "Scope", CreatedAt: 1, UpdatedAt: 1}
	if _, err := app.store.CreateProject(project); err != nil {
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
	// The threads of an assistant live outside every project and are
	// interactive with no project.
	for name, thread := range map[string]store.Thread{
		"current": {ID: "current", Mode: threadmode.ModeChat, AgentID: "a1"},
		"past":    {ID: "past", Mode: threadmode.ModeChat, PastAgentID: "a1"},
		"helper":  {ID: "helper", Mode: threadmode.ModeChat, HelperOfAgentID: "a1"},
	} {
		scope, ok, err := app.deriveCallerScope(thread)
		if err != nil || !ok || scope.Kind != transport.ScopeKindInteractive || scope.ProjectID != "" || scope.ThreadID != thread.ID {
			t.Fatalf("%s thread of an assistant: scope %#v ok=%t err=%v", name, scope, ok, err)
		}
	}

	// A workflow thread carries the phase's FROZEN grants.
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		ID: "wf", Phases: []def.Phase{
			{ID: "build", Driver: def.DriverAgent, Access: def.AccessWrite, Grants: []string{string(def.GrantStartRun), "retired-grant"}},
			{ID: "verify", Driver: def.DriverAgent, Shape: def.ShapeFanOut, FanOut: []def.Unit{{ID: "alpha", Access: def.AccessWrite}}},
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
	if scope.Access != string(def.AccessWrite) {
		t.Fatalf("phase access = %q, want the frozen phase's write", scope.Access)
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
	// A unit declares its own access: this one writes in a read-only phase.
	if scope.Access != string(def.AccessWrite) {
		t.Fatalf("unit access = %q, want the unit's own write", scope.Access)
	}

	// A workflow thread with no attempt row yet gets no credential rather than
	// an unscoped one.
	if _, ok, err := app.deriveCallerScope(store.Thread{
		ID: "unattached", ProjectID: project.ID, Mode: threadmode.ModeWorkflow,
	}); err != nil || ok {
		t.Fatalf("unattached workflow thread: ok=%t err=%v", ok, err)
	}
}

func TestFrozenPhaseAuthorityRefusesWorkTheSnapshotDoesNotName(t *testing.T) {
	t.Parallel()
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		ID: "wf", Phases: []def.Phase{{ID: "build", Driver: def.DriverAgent}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frozenPhaseAuthority(snapshot, phaseAttachment{phaseID: "ghost"}); err == nil {
		t.Fatal("a phase absent from the snapshot resolved to no grants instead of erroring")
	}
	if _, err := frozenPhaseAuthority(snapshot, phaseAttachment{phaseID: "build", unitID: "ghost"}); err == nil {
		t.Fatal("a unit absent from the snapshot resolved to the phase's access instead of erroring")
	}
	// An undeclared access is read-only.
	if authority, err := frozenPhaseAuthority(snapshot, phaseAttachment{phaseID: "build"}); err != nil || authority.access != def.AccessReadOnly {
		t.Fatalf("frozenPhaseAuthority(build) = %#v, %v", authority, err)
	}
	// An empty snapshot is a legitimate pre-freeze state, not an error.
	if authority, err := frozenPhaseAuthority(nil, phaseAttachment{phaseID: "build"}); err != nil || authority.grants != nil || authority.access != def.DefaultAccess {
		t.Fatalf("frozenPhaseAuthority(nil) = %#v, %v", authority, err)
	}
}

// The boot-mode values and the in-session marker are pins, and the marker
// is AO's own: a boot value for the same name does not replace it.
func TestSessionProcessEnvPrecedence(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.providerExtraEnv = map[string]string{control.EnvAddr: "extra", aocli.EnvInSession: "override"}
	env := app.sessionProcessEnv(string(provider.Claude))
	if got := env.Pins[provider.AOEnv(control.EnvAddr)]; got != "extra" {
		t.Fatalf("%s = %q, want the boot-mode value pinned", control.EnvAddr, got)
	}
	if got := env.Pins[provider.AOEnv(aocli.EnvInSession)]; got != "1" {
		t.Fatalf("%s = %q, want static in-session marker \"1\"", aocli.EnvInSession, got)
	}
	if len(env.Pins) != 2 {
		t.Fatalf("pins = %v, want the boot value and the marker", env.Pins)
	}
}

// D30: every session, of every provider, gets the directory holding the
// canonical-name link at the front of its PATH, which is how
// `agent-overflow` resolves inside an agent's shell.
func TestSessionProcessEnvPublishesTheCLIOnPath(t *testing.T) {
	t.Parallel()
	published := (&App{cliBinDir: "/cfg/bin"}).sessionProcessEnv(string(provider.Claude))
	if published.PathPrepend != "/cfg/bin" {
		t.Fatalf("PathPrepend = %q, want /cfg/bin", published.PathPrepend)
	}
	if published.Pins[provider.AOEnv(aocli.EnvInSession)] != "1" {
		t.Fatalf("the in-session marker did not survive: %v", published.Pins)
	}
	// A boot that could not publish the command must not invent a PATH entry
	// pointing at nothing.
	if unpublished := (&App{}).sessionProcessEnv(string(provider.Claude)); unpublished.PathPrepend != "" {
		t.Fatalf("PathPrepend = %q with no published command, want none", unpublished.PathPrepend)
	}
}

// The provider env helper is shared, so pin that it merges rather than replaces.
func TestBuildEnvironmentMergesOverrides(t *testing.T) {
	t.Parallel()
	env := provider.BuildEnvironment(provider.EnvInput{Custom: map[string]string{"AO_TEST_ONLY": "1"}})
	var found bool
	for _, entry := range env {
		if entry == "AO_TEST_ONLY=1" {
			found = true
		}
	}
	if !found {
		t.Fatal("BuildEnvironment() dropped the override")
	}
	if len(env) <= 1 {
		t.Fatal("BuildEnvironment() replaced the process environment instead of extending it")
	}
}
