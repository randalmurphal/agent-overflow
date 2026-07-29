package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/scheduler"
)

// The server side of the `ao` surface: which runs a scope may touch, and the
// effect ledger that makes a re-entered phase's side effects fire once.

const cliToolPhase = `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - to: done`

// cliWorkflowInputs are the seeds the surface-and-skip tests vary. They have to
// be declared: agent-supplied seeds are validated against the workflow's inputs
// before anything starts.
const cliWorkflowInputs = `
inputs:
  depth:
    optional: true
    schema:
      type: number
  label:
    optional: true
    schema:
      type: string`

// newCLIFixture boots the tool-driver workflow fixture with a command that
// always succeeds, so a run started through the agent surface reaches `done`
// without any provider process.
func newCLIFixture(t *testing.T) *toolWorkflowFixture {
	t.Helper()
	fixture := newToolWorkflowFixture(t, cliToolPhase+cliWorkflowInputs)
	fixture.writeProfile(t, map[string][]string{"verify": {"/bin/true"}}, nil, "")
	return fixture
}

func phaseScope(fixture *toolWorkflowFixture, itemID string, grants ...def.Grant) transport.CallerScope {
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		names = append(names, string(grant))
	}
	return transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: "phase-thread", ProjectID: fixture.project.ID,
		ItemID: itemID, PhaseID: "orchestrate", Grants: names,
	}
}

func interactiveScope(fixture *toolWorkflowFixture, threadID string) transport.CallerScope {
	return transport.CallerScope{
		Kind: transport.ScopeKindInteractive, ThreadID: threadID, ProjectID: fixture.project.ID,
	}
}

func TestWorkflowAgentStartRunBindsAnInteractiveThread(t *testing.T) {
	fixture := newCLIFixture(t)
	thread, err := fixture.app.CreateThread(CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, thread.ID))
	result, err := fixture.app.WorkflowAgentStartRun(ctx, WorkflowAgentStartInput{
		WorkflowID: "tool-flow", Goal: "start from a conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BindingWarning != "" || result.BoundThreadID != thread.ID {
		t.Fatalf("start result = %#v, want a clean binding to %s", result, thread.ID)
	}
	item, err := fixture.app.store.GetWorkItem(result.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.OriginThreadID != thread.ID {
		t.Fatalf("origin thread = %q, want %q", item.OriginThreadID, thread.ID)
	}
	// An interactive start carries no source ref: provenance lives in the
	// binding, and the unique agent-source-ref index would collide on a second.
	if item.Source != workflowSourceAgent || item.SourceRef != "" {
		t.Fatalf("source = %q/%q", item.Source, item.SourceRef)
	}
	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateDone, "")

	// A second identical start from an interactive scope is a second run: an
	// interactive invocation is a human-approved bash call, and replaying one is
	// the human's decision.
	second, err := fixture.app.WorkflowAgentStartRun(ctx, WorkflowAgentStartInput{
		WorkflowID: "tool-flow", Goal: "start from a conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Skipped || second.ItemID == result.ItemID {
		t.Fatalf("interactive re-start = %#v, want a fresh run", second)
	}
	waitForWorkflowItem(t, fixture.app, second.ItemID, engine.StateDone, "")
}

func TestWorkflowAgentStartRunReportsAnUnbindableThread(t *testing.T) {
	fixture := newCLIFixture(t)
	// A workflow-mode thread cannot legally hold a binding. The run must still
	// start — it is already the caller's intent — and say why it is unbound.
	thread := store.Thread{
		ID: "workflow-thread", ProjectID: fixture.project.ID, Mode: threadmode.ModeWorkflow,
		Provider: "claude", Model: "claude-opus-4-7", CreatedAt: 1, UpdatedAt: 1,
	}
	if err := fixture.app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, thread.ID))
	result, err := fixture.app.WorkflowAgentStartRun(ctx, WorkflowAgentStartInput{
		WorkflowID: "tool-flow", Goal: "unbindable",
	})
	if err != nil {
		t.Fatalf("an unbindable thread failed the start instead of reporting: %v", err)
	}
	if result.ItemID == "" {
		t.Fatal("no run was started")
	}
	if !strings.Contains(result.BindingWarning, "unbound") || result.BoundThreadID != "" {
		t.Fatalf("binding warning = %q", result.BindingWarning)
	}
	item, err := fixture.app.store.GetWorkItem(result.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.OriginThreadID != "" {
		t.Fatalf("an invalid thread was bound anyway: %q", item.OriginThreadID)
	}
	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateDone, "")
}

func TestWorkflowAgentStartRunSurfacesAndSkipsForAPhase(t *testing.T) {
	fixture := newCLIFixture(t)
	scope := phaseScope(fixture, "caller-item", def.GrantStartRun)
	ctx := transport.WithCallerScope(context.Background(), scope)
	input := WorkflowAgentStartInput{
		WorkflowID: "tool-flow", Goal: "decompose",
		Seeds: json.RawMessage(`{"depth":1,"label":"a"}`),
	}
	first, err := fixture.app.WorkflowAgentStartRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Skipped {
		t.Fatalf("the first start reported a skip: %#v", first)
	}
	item, err := fixture.app.store.GetWorkItem(first.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Source != workflowSourceAgent || !strings.HasPrefix(item.SourceRef, "caller-item/orchestrate/") {
		t.Fatalf("phase-started run provenance = %q/%q", item.Source, item.SourceRef)
	}
	if item.OriginThreadID != "" {
		t.Fatalf("a phase-started run was bound to a thread: %q", item.OriginThreadID)
	}
	waitForWorkflowItem(t, fixture.app, first.ItemID, engine.StateDone, "")

	// Re-entering the phase with the same arguments must NOT fire again; it must
	// answer with the run the earlier attempt started.
	replay, err := fixture.app.WorkflowAgentStartRun(ctx, WorkflowAgentStartInput{
		WorkflowID: "tool-flow", Goal: "decompose",
		// Reordered seeds hash the same: the payload hash is canonical JSON.
		Seeds: json.RawMessage(`{"label":"a","depth":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Skipped || replay.ItemID != first.ItemID {
		t.Fatalf("replay = %#v, want skipped with item %s", replay, first.ItemID)
	}

	// A different payload is a different effect and does fire.
	different, err := fixture.app.WorkflowAgentStartRun(ctx, WorkflowAgentStartInput{
		WorkflowID: "tool-flow", Goal: "decompose", Seeds: json.RawMessage(`{"depth":2,"label":"a"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if different.Skipped || different.ItemID == first.ItemID {
		t.Fatalf("a different payload = %#v, want a fresh run", different)
	}
	waitForWorkflowItem(t, fixture.app, different.ItemID, engine.StateDone, "")

	// The same payload from a DIFFERENT phase of the same run is also a
	// different effect: the ledger key is (item, phase, tool, hash).
	otherPhase := scope
	otherPhase.PhaseID = "verify"
	sibling, err := fixture.app.WorkflowAgentStartRun(
		transport.WithCallerScope(context.Background(), otherPhase), input)
	if err != nil {
		t.Fatal(err)
	}
	if sibling.Skipped || sibling.ItemID == first.ItemID {
		t.Fatalf("a sibling phase = %#v, want a fresh run", sibling)
	}
	waitForWorkflowItem(t, fixture.app, sibling.ItemID, engine.StateDone, "")
}

func TestScopedRunActionsAreConfinedToWhatAPhaseStarted(t *testing.T) {
	fixture := newCLIFixture(t)
	scope := phaseScope(fixture, "caller-item", def.GrantStartRun)
	ctx := transport.WithCallerScope(context.Background(), scope)
	started, err := fixture.app.WorkflowAgentStartRun(ctx, WorkflowAgentStartInput{
		WorkflowID: "tool-flow", Goal: "mine",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, fixture.app, started.ItemID, engine.StateDone, "")

	// A run this phase did not start is invisible to it without introspect...
	foreign, err := fixture.app.WorkflowStartRun(
		fixture.project.ID, "tool-flow", "shared", "someone else's", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, fixture.app, foreign.ID, engine.StateDone, "")
	if _, err := fixture.app.WorkflowAgentRunStatus(ctx, foreign.ID); err == nil {
		t.Fatal("a start-run-only phase read a run it did not start")
	}
	// ... and readable with it.
	introspecting := transport.WithCallerScope(context.Background(),
		phaseScope(fixture, "caller-item", def.GrantStartRun, def.GrantIntrospect))
	if _, err := fixture.app.WorkflowAgentRunStatus(introspecting, foreign.ID); err != nil {
		t.Fatalf("introspect could not read a project run: %v", err)
	}
	// Acting on it stays refused even with introspect: reading the project is
	// not permission to stop its work.
	if err := fixture.app.WorkflowCancelItem(introspecting, foreign.ID); err == nil {
		t.Fatal("introspect allowed a cancel of a run this phase did not start")
	}
	// The phase's own run stays actionable. Authorization is asserted rather
	// than a real cancel, because the run already finished and the engine would
	// refuse it for a reason that has nothing to do with scope.
	if err := fixture.app.authorizeScopedRunAction(ctx, started.ItemID, "cancel"); err != nil {
		t.Fatalf("a phase was refused action on the run it started: %v", err)
	}
	if err := fixture.app.authorizeScopedRunAction(introspecting, foreign.ID, "cancel"); err == nil {
		t.Fatal("introspect allowed action on a run this phase did not start")
	}

	// The listing needs introspect outright — an empty list would read as "no
	// runs", which is a different and wrong answer.
	if _, err := fixture.app.WorkflowAgentListRuns(ctx, false); err == nil {
		t.Fatal("a start-run-only phase listed the project's runs")
	}
	views, err := fixture.app.WorkflowAgentListRuns(introspecting, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) < 2 {
		t.Fatalf("introspecting list returned %d runs, want every run in the project", len(views))
	}

	// The UI path carries no caller scope and is never narrowed.
	if err := fixture.app.authorizeScopedRunAction(context.Background(), foreign.ID, "cancel"); err != nil {
		t.Fatalf("an unscoped (UI) call was narrowed: %v", err)
	}
}

// `run status` is the only CLI surface that can name a run's failed units, and
// those ids are the second argument of `run retry-unit`. Reporting
// needs-human(unit-failed) without them tells an agent a fan-out needs repair
// and leaves the ids only in the app — the one place a scoped credential cannot
// look. The parent rides along for the same reason: a campaign's runs are a
// tree, and a view that never names the caller renders it flat.
func TestAgentRunStatusNamesTheParentAndTheFailedUnits(t *testing.T) {
	fixture := newCLIFixture(t)
	root, err := fixture.app.WorkflowStartRun(
		fixture.project.ID, "tool-flow", "shared", "wave 1", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, fixture.app, root.ID, engine.StateDone, "")
	child := store.WorkItem{
		ID: "wave-2", ProjectID: fixture.project.ID, Goal: "wave 2", WorkflowID: "tool-flow",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonUnitFailed), Source: engine.WorkItemSourceCall,
		ParentItemID: root.ID, ParentPhaseID: "advance", ParentAttempt: 1, CallDepth: 1,
		CreatedAt: 2,
	}
	if err := fixture.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: child.ID, PhaseID: "fan", Attempt: 1, UnitID: "lane-1", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitFailed, UnitAttempt: 2},
		{ItemID: child.ID, PhaseID: "fan", Attempt: 1, UnitID: "lane-2", UnitIndex: 1,
			Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitDone, UnitAttempt: 1},
		// A failed join is the phase's own closing step, not a unit a human
		// retries — the same exclusion `run retry-failed-units` applies.
		{ItemID: child.ID, PhaseID: "fan", Attempt: 1, UnitID: "merge", UnitIndex: 2,
			Kind: store.WorkItemUnitKindJoin, Status: store.WorkItemUnitFailed, UnitAttempt: 1},
	}); err != nil {
		t.Fatal(err)
	}

	thread, err := fixture.app.CreateThread(CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	interactive := transport.WithCallerScope(context.Background(), interactiveScope(fixture, thread.ID))
	view, err := fixture.app.WorkflowAgentRunStatus(interactive, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ParentItemID != root.ID {
		t.Fatalf("status parent = %q, want %q", view.ParentItemID, root.ID)
	}
	if len(view.FailedUnits) != 1 || view.FailedUnits[0].UnitID != "lane-1" || view.FailedUnits[0].UnitAttempt != 2 {
		t.Fatalf("status failed units = %#v, want only lane-1 on its second try", view.FailedUnits)
	}

	// A run that is not parked on a failed fan-out carries none, and neither
	// does the list: one unit query per listed run is a fan-out of its own, and
	// a list is for locating a run rather than repairing one.
	rootView, err := fixture.app.WorkflowAgentRunStatus(interactive, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootView.FailedUnits) != 0 {
		t.Fatalf("a done run reported failed units: %#v", rootView.FailedUnits)
	}
	views, err := fixture.app.WorkflowAgentListRuns(interactive, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range views {
		if len(listed.FailedUnits) != 0 {
			t.Fatalf("run %s carried failed units in a list: %#v", listed.ItemID, listed.FailedUnits)
		}
	}
}

// Deliverable of the campaign shape: a run started from a conversation calls
// itself for the next wave, so the run that needs repairing is almost never the
// one the human started. The interactive credential the bound thread's session
// holds is PROJECT-scoped, so it reaches the whole tree; a phase credential
// stays limited to what that phase itself started, descendants included.
func TestAnInteractiveScopeActsOnDescendantRunsAndAPhaseScopeDoesNot(t *testing.T) {
	fixture := newCLIFixture(t)
	root, err := fixture.app.WorkflowStartRun(
		fixture.project.ID, "tool-flow", "shared", "wave 1", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, fixture.app, root.ID, engine.StateDone, "")

	// The engine writes these rows when a `shape: call` phase invokes the next
	// wave; the test writes them directly so the authorization question is asked
	// without a second provider run in the way.
	child := store.WorkItem{
		ID: "wave-2", ProjectID: fixture.project.ID, Goal: "wave 2", WorkflowID: "tool-flow",
		WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonUnitFailed), Source: engine.WorkItemSourceCall,
		ParentItemID: root.ID, ParentPhaseID: "advance", ParentAttempt: 1, CallDepth: 1,
		CreatedAt: 2,
	}
	if err := fixture.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	grandchild := child
	grandchild.ID = "wave-3"
	grandchild.Goal = "wave 3"
	grandchild.ParentItemID = child.ID
	grandchild.CallDepth = 2
	grandchild.CreatedAt = 3
	if err := fixture.app.store.CreateWorkItem(grandchild); err != nil {
		t.Fatal(err)
	}

	thread, err := fixture.app.CreateThread(CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	interactive := transport.WithCallerScope(context.Background(), interactiveScope(fixture, thread.ID))
	for _, itemID := range []string{root.ID, child.ID, grandchild.ID} {
		for _, action := range []string{"retry every failed unit", "resume workflow run", "pause workflow run"} {
			if err := fixture.app.authorizeScopedRunAction(interactive, itemID, action); err != nil {
				t.Fatalf("an interactive session was refused %q on %s: %v", action, itemID, err)
			}
		}
		if _, err := fixture.app.WorkflowAgentRunStatus(interactive, itemID); err != nil {
			t.Fatalf("an interactive session could not read %s: %v", itemID, err)
		}
	}

	// Project confinement is the boundary that remains: another project's tree is
	// refused whatever its shape.
	elsewhere := transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: "other-project",
	})
	if err := fixture.app.authorizeScopedRunAction(elsewhere, grandchild.ID, "resume workflow run"); err == nil {
		t.Fatal("an interactive session acted on a run outside its project")
	}

	// A phase credential that started the ROOT still may not touch what the root
	// called: its grant is frozen to the runs it started itself.
	phase := transport.WithCallerScope(context.Background(), phaseScope(fixture, root.ID, def.GrantStartRun))
	if err := fixture.app.authorizeScopedRunAction(phase, grandchild.ID, "resume workflow run"); err == nil {
		t.Fatal("a phase credential acted on a descendant it did not start")
	}
}

func TestWorkflowAgentNotesAndScheduleRecordOneEffect(t *testing.T) {
	fixture := newCLIFixture(t)
	scope := phaseScope(fixture, "caller-item", def.GrantSchedule, def.GrantUpdateNotes)
	ctx := transport.WithCallerScope(context.Background(), scope)

	schedule := WorkflowAgentScheduleInput{WorkflowID: "tool-flow", Cron: "0 3 * * *", Name: "Nightly"}
	created, err := fixture.app.WorkflowAgentSchedule(ctx, schedule)
	if err != nil {
		t.Fatal(err)
	}
	if created.Skipped || created.AutomationID == "" {
		t.Fatalf("schedule = %#v", created)
	}
	replay, err := fixture.app.WorkflowAgentSchedule(ctx, schedule)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Skipped || replay.AutomationID != created.AutomationID {
		t.Fatalf("schedule replay = %#v, want a skip naming %s", replay, created.AutomationID)
	}
	automations, err := fixture.app.store.ListAutomations(fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(automations) != 1 {
		t.Fatalf("re-entering the phase created %d automations", len(automations))
	}
	var trigger scheduler.Trigger
	if err := json.Unmarshal(automations[0].Trigger, &trigger); err != nil {
		t.Fatal(err)
	}
	if trigger.Kind != scheduler.KindCron || trigger.Expr != "0 3 * * *" {
		t.Fatalf("trigger = %#v", trigger)
	}

	notesResult, err := fixture.app.WorkflowAgentSetNotes(ctx, created.AutomationID, "watch the flaky suite")
	if err != nil {
		t.Fatal(err)
	}
	if notesResult.Skipped {
		t.Fatal("the first notes write reported a skip")
	}
	notes, err := fixture.app.WorkflowAgentGetNotes(ctx, created.AutomationID)
	if err != nil {
		t.Fatal(err)
	}
	if notes != "watch the flaky suite" {
		t.Fatalf("notes = %q", notes)
	}
	again, err := fixture.app.WorkflowAgentSetNotes(ctx, created.AutomationID, "watch the flaky suite")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Skipped {
		t.Fatal("rewriting identical notes fired a second write")
	}
	// Different notes are a different effect.
	changed, err := fixture.app.WorkflowAgentSetNotes(ctx, created.AutomationID, "the suite is fixed")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Skipped {
		t.Fatal("a changed notes write was skipped")
	}
}

func TestEffectPayloadHashIsCanonical(t *testing.T) {
	first, err := effectPayloadHash(map[string]any{"a": 1, "b": []any{"x", "y"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := effectPayloadHash(map[string]any{"b": []any{"x", "y"}, "a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("key order changed the hash: %s vs %s", first, second)
	}
	different, err := effectPayloadHash(map[string]any{"a": 1, "b": []any{"y", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if different == first {
		t.Fatal("array order did not change the hash; element order is meaningful")
	}
}

func TestWorkflowAgentMethodsRequireAScope(t *testing.T) {
	fixture := newCLIFixture(t)
	if _, err := fixture.app.WorkflowAgentStartRun(context.Background(), WorkflowAgentStartInput{
		WorkflowID: "tool-flow",
	}); err == nil {
		t.Fatal("an unscoped caller started a run through the agent surface")
	}
	// A scope with no project is equally refused: every method resolves its
	// project from the credential, not from an argument.
	ctx := transport.WithCallerScope(context.Background(),
		transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: "t"})
	if _, err := fixture.app.WorkflowAgentListRuns(ctx, false); err == nil {
		t.Fatal("a projectless scope listed runs")
	}
}

func TestWorkflowComposerBlockRendersTheProjectSurface(t *testing.T) {
	fixture := newCLIFixture(t)
	thread, err := fixture.app.CreateThread(CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := fixture.app.WorkflowStartRun(
		fixture.project.ID, "tool-flow", "shared", "composer", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateDone, "")

	block, err := fixture.app.workflowComposerBlock(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"agent-overflow run start", "agent-overflow workflow list",
		aocli.EnvEndpoint, "tool-flow (shared)", fixture.configRoot,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("composer block is missing %q:\n%s", want, block)
		}
	}
	// This fixture's App never ran Start, so it published no command; the block
	// has to say so instead of instructing an agent to run something it cannot
	// resolve. The reachable case is covered in app_cli_path_test.go and by the
	// e2e spec that resolves the name through the published directory alone.
	if !strings.Contains(block, "could not publish `agent-overflow`") {
		t.Fatalf("composer block promised a command the app never published:\n%s", block)
	}
	fixture.app.cliBinDir = t.TempDir()
	published, err := fixture.app.workflowComposerBlock(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(published, "could not publish") {
		t.Fatalf("a published command was reported as missing:\n%s", published)
	}
	// The run above finished, so it is not active and must not be listed.
	if strings.Contains(block, item.ID) {
		t.Fatalf("a finished run appeared in the active-runs list:\n%s", block)
	}
	// The thread has no live session, so the block must say the environment is
	// not there yet rather than promise credentials that do not exist.
	if !strings.Contains(block, "no live session yet") {
		t.Fatalf("composer block claimed a session that does not exist:\n%s", block)
	}
	if _, err := fixture.app.workflowComposerBlock(""); err == nil {
		t.Fatal("an empty thread id produced a block")
	}
}
