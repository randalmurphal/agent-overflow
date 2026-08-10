package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// The blocking read behind `agent-overflow run watch`. Transitions reach the
// ring through the SAME listener a wake does (`afterWorkflowEngineEvent`), so
// what these drive is the wiring, not a private test entry point.

type watchHarness struct{ app *App }

func newWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	app, _ := setupE2EApp(t)
	app.configDir = t.TempDir()
	return &watchHarness{app: app}
}

// scope is a phase session holding `introspect`, which is what a supervising
// agent watching its own campaign runs as.
func (h *watchHarness) scope(grants ...def.Grant) context.Context {
	names := make([]string, 0, len(grants))
	for _, grant := range grants {
		names = append(names, string(grant))
	}
	return transport.WithCallerScope(context.Background(), transport.CallerScope{
		Kind: transport.ScopeKindPhase, ThreadID: "babysitter", ProjectID: defaultTestProjectID,
		ItemID: "supervisor", PhaseID: "watch", Grants: names,
	})
}

func (h *watchHarness) run(t *testing.T, item store.WorkItem) store.WorkItem {
	t.Helper()
	item.ProjectID = defaultTestProjectID
	if item.WorkflowScope == "" {
		item.WorkflowScope = "shared"
	}
	if item.Source == "" {
		item.Source = "manual"
	}
	if item.CreatedAt == 0 {
		item.CreatedAt = 1
	}
	if err := h.app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

// move drives one engine transition through the app's real listener and then
// persists the row, in that order — which is the order the engine produces them
// in for everything except the transition it emits from inside its own write.
func (h *watchHarness) move(t *testing.T, itemID, phaseID string, attempt int, from, to engine.State, reason engine.Reason) {
	t.Helper()
	if err := h.app.store.UpdateWorkItemState(itemID, string(to), string(reason), 0); err != nil {
		t.Fatal(err)
	}
	h.app.afterWorkflowEngineEvent("workflow:item-state", engine.StateEvent{
		ItemID: itemID, ProjectID: defaultTestProjectID, From: from, To: to,
		Reason: reason, PhaseID: phaseID, Attempt: attempt,
	})
}

// A watch with no cursor answers immediately with where the run is and the
// sequence to continue from — never a backlog. A campaign's retained history
// replayed into a supervisor's context is the opposite of "tell me what happens
// next".
func TestWorkflowAgentWatchRunAnswersTheFirstCallWithoutBlocking(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	h.move(t, item.ID, "survey", 1, engine.StateRunning, engine.StateRunning, "")

	started := time.Now()
	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > maxWorkflowWatchHold/2 {
		t.Fatalf("a cursorless watch blocked for %s", elapsed)
	}
	if len(result.Transitions) != 0 {
		t.Fatalf("first call returned a backlog of %d transitions", len(result.Transitions))
	}
	if result.Cursor == 0 {
		t.Fatal("first call returned no cursor to continue from")
	}
	if result.Run.State != string(engine.StateRunning) || result.Run.Resting {
		t.Fatalf("run = %#v, want the running run", result.Run)
	}
}

// The blocking half: a call that arrives while nothing has happened parks, and
// the transition itself is what wakes it — not a timer, and not the caller
// asking again.
func TestWorkflowAgentWatchRunBlocksUntilTheRunMoves(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}

	answered := make(chan WorkflowAgentWatchResult, 1)
	go func() {
		result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
			WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 5000})
		if err != nil {
			t.Error(err)
			close(answered)
			return
		}
		answered <- result
	}()
	// The watcher is parked; the transition is what releases it.
	time.Sleep(20 * time.Millisecond)
	select {
	case result := <-answered:
		t.Fatalf("watch answered before anything happened: %#v", result)
	default:
	}
	h.move(t, item.ID, "survey", 2, engine.StateRunning, engine.StateNeedsHuman, engine.ReasonStuck)

	select {
	case result, ok := <-answered:
		if !ok {
			t.Fatal("watch failed")
		}
		if len(result.Transitions) != 1 {
			t.Fatalf("transitions = %#v, want the one that woke it", result.Transitions)
		}
		transition := result.Transitions[0]
		if transition.To != string(engine.StateNeedsHuman) || transition.Reason != string(engine.ReasonStuck) {
			t.Fatalf("transition = %#v", transition)
		}
		if transition.PhaseID != "survey" || transition.Attempt != 2 {
			t.Fatalf("transition coordinate = %s#%d, want the phase and attempt the run was in",
				transition.PhaseID, transition.Attempt)
		}
		if !transition.Resting {
			t.Fatal("a needs-human transition was not reported as resting")
		}
		if !result.Run.Resting || result.Run.Repair == "" {
			t.Fatalf("run = %#v, want the resting state and the verb that settles it", result.Run)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the transition did not wake the watch")
	}
}

// A re-park under a NEW reason is a move, even though the state did not change.
// The wake filters those out; a monitor that did would report a run as waiting
// on something it is no longer waiting on.
func TestWorkflowAgentWatchRunReportsARepark(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.move(t, item.ID, "survey", 1, engine.StateNeedsHuman, engine.StateNeedsHuman, engine.ReasonTakenOver)

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Transitions) != 1 || result.Transitions[0].Reason != string(engine.ReasonTakenOver) {
		t.Fatalf("transitions = %#v, want the re-park", result.Transitions)
	}
}

// Without --tree a watch reports only its own run; with it, the runs the watched
// run called — which for a campaign is where every transition happens.
func TestWorkflowAgentWatchRunTreeFollowsCalledRuns(t *testing.T) {
	h := newWatchHarness(t)
	root := h.run(t, store.WorkItem{ID: "root", Goal: "campaign", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	child := h.run(t, store.WorkItem{ID: "wave-3", Goal: "wave 3", WorkflowID: "port",
		State: string(engine.StateRunning), Source: "call", ParentItemID: root.ID,
		ParentPhaseID: "wave", ParentAttempt: 3, CallDepth: 1, CreatedAt: 2})

	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: root.ID, Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	h.move(t, child.ID, "build", 1, engine.StateRunning, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: root.ID, Cursor: first.Cursor, Tree: true, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Transitions) != 1 || result.Transitions[0].ItemID != child.ID {
		t.Fatalf("tree transitions = %#v, want the called run's", result.Transitions)
	}
	// The run reported is still the one being watched: a descendant parking does
	// not make the root resting, and a watcher must not read it as such.
	if result.Run.ItemID != root.ID || result.Run.Resting {
		t.Fatalf("run = %#v, want the watched root still running", result.Run)
	}
	// The same transition is invisible without the flag: the watch is scoped to
	// what the caller asked to watch.
	narrow, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: root.ID, Cursor: first.Cursor, WaitMillis: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Transitions) != 0 {
		t.Fatalf("a narrow watch saw a descendant's transition: %#v", narrow.Transitions)
	}
}

// A `--tree` watch re-resolves its membership on EVERY wake of a globally
// broadcast loop, so the read it resolves through may not carry a run's frozen
// workflow snapshot — nor make SQLite parse one to compute a phase ordinal
// nobody on this path reads: one transition anywhere in the app would otherwise
// cost a campaign supervisor that work per tree member.
//
// The three assertions are one statement. The node walk must return the whole
// tree in the same order the full-row walk does, carrying nothing but linkage,
// and the full-row walk over the same rows must carry the frozen blobs —
// otherwise the fixture proves nothing and the first assertion would pass over
// an empty column.
func TestWorkflowWatchTreeResolvesThroughSnapshotFreeRows(t *testing.T) {
	h := newWatchHarness(t)
	snapshot := json.RawMessage(`{"workflow":{"id":"campaign","phases":[{"id":"wave"}]}}`)
	root := h.run(t, store.WorkItem{ID: "root", Goal: "campaign", WorkflowID: "campaign",
		State: string(engine.StateRunning), Snapshot: snapshot,
		Seeds: json.RawMessage(`{"wave-number":3}`)})
	wanted := map[string]bool{root.ID: true}
	for index, child := range []string{"wave-1", "wave-2", "wave-3"} {
		item := h.run(t, store.WorkItem{ID: child, Goal: "wave", WorkflowID: "port",
			State: string(engine.StateRunning), Source: "call", ParentItemID: root.ID,
			ParentPhaseID: "wave", ParentAttempt: 1, CallDepth: 1, CreatedAt: int64(2 + index),
			Snapshot: snapshot, Seeds: json.RawMessage(`{"task":"x"}`)})
		wanted[item.ID] = true
	}
	// A grandchild: the walk's own recursion, not just one level of children.
	grandchild := h.run(t, store.WorkItem{ID: "wave-1-unit", Goal: "unit", WorkflowID: "port",
		State: string(engine.StateRunning), Source: "call", ParentItemID: "wave-1",
		ParentPhaseID: "fan", ParentAttempt: 1, ParentUnitID: "u1", CallDepth: 2, CreatedAt: 9,
		Snapshot: snapshot, Seeds: json.RawMessage(`{"task":"y"}`)})
	wanted[grandchild.ID] = true

	nodes, err := h.app.workflowRunTreeNodes(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != len(wanted) {
		t.Fatalf("node tree = %d members, want %d", len(nodes), len(wanted))
	}
	for _, member := range nodes {
		if !wanted[member.ID] {
			t.Fatalf("node tree carries an unrelated run %q", member.ID)
		}
	}

	full, err := h.app.workflowRunTree(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Membership AND order are identical: the two projections read one tree, so
	// a narrower read may never be a differently-shaped one.
	if len(full) != len(nodes) {
		t.Fatalf("full tree = %d members, node tree = %d", len(full), len(nodes))
	}
	for index, member := range full {
		if member.ID != nodes[index].ID {
			t.Fatalf("tree member %d: full walk = %q, node walk = %q", index, member.ID, nodes[index].ID)
		}
		if member.CallDepth != nodes[index].CallDepth {
			t.Fatalf("tree member %q: full walk depth %d, node walk depth %d",
				member.ID, member.CallDepth, nodes[index].CallDepth)
		}
		if len(member.Snapshot) == 0 {
			t.Fatalf("fixture run %q has no snapshot, so the node assertion above proves nothing", member.ID)
		}
	}

	// And the set the watch actually filters on is that same whole tree.
	watched, err := h.app.workflowWatchedRuns(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(watched) != len(wanted) {
		t.Fatalf("watched runs = %v, want %v", watched, wanted)
	}
	for id := range wanted {
		if !watched[id] {
			t.Fatalf("watched runs = %v, missing %q", watched, id)
		}
	}
}

// The id a watch AUTHORIZES is the trimmed one, so every read it then issues has
// to be keyed on that and not on the raw request field. A caller whose shell left
// a newline on the id used to pass authorization and be answered with a bare
// no-rows error from a lookup of a run that does not exist.
func TestWorkflowAgentWatchRunAnswersAnUntrimmedID(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonQuestion)})

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: " " + item.ID + "\n", Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ItemID != item.ID {
		t.Fatalf("itemId = %q, want the resolved run id", result.ItemID)
	}
	if result.Run.ItemID != item.ID || !result.Run.Resting {
		t.Fatalf("run = %#v, want the resting run the caller named", result.Run)
	}
}

// A resting transition carries the ENGINE's own diagnosis of the park, resolved
// from the exact attempt the transition recorded rather than from whatever the
// run's latest attempt is by the time the watch answers.
func TestWorkflowAgentWatchRunCarriesTheParkCauseOfThatAttempt(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	for _, phase := range []store.WorkItemPhase{
		{ItemID: item.ID, PhaseID: "survey", Attempt: 1, Status: "parked",
			ParkCause: "the worktree would not cut", StartedAt: 1},
		{ItemID: item.ID, PhaseID: "survey", Attempt: 2, Status: "parked",
			ParkCause: "the budget ran out", StartedAt: 2},
	} {
		if err := h.app.store.CreateWorkItemPhase(phase); err != nil {
			t.Fatal(err)
		}
	}
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.move(t, item.ID, "survey", 1, engine.StateRunning, engine.StateNeedsHuman, engine.ReasonSetupFailed)

	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Transitions) != 1 {
		t.Fatalf("transitions = %#v", result.Transitions)
	}
	if got := result.Transitions[0].Cause; got != "the worktree would not cut" {
		t.Fatalf("cause = %q, want the cause of attempt 1 rather than the latest attempt's", got)
	}
}

// A cursor the ring cannot honour is a resync instruction, never silence: the
// ring is a jitter buffer, and a watcher holding a sequence from a previous
// process would otherwise block on a number this one will never reach.
func TestWorkflowAgentWatchRunGapsAnImpossibleCursor(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: 9_000, WaitMillis: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Gap {
		t.Fatalf("a cursor above the head did not gap: %#v", result)
	}
	if result.Run.State != string(engine.StateRunning) {
		t.Fatalf("run = %#v, want the current state alongside the gap", result.Run)
	}
}

// A wait budget is honoured exactly: `--timeout` is only exact if the last poll
// waits the remainder and not a second more.
func TestWorkflowAgentWatchRunHonoursTheCallersWaitBudget(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 60})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < 50*time.Millisecond {
		t.Fatalf("watch returned after %s, want it to have held for the requested 60ms", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("watch held for %s, want the caller's budget to bound it", elapsed)
	}
	if len(result.Transitions) != 0 || result.Gap {
		t.Fatalf("an expired hold reported %#v", result)
	}
}

// A cancelled caller is answered rather than left holding the goroutine: a CLI
// that hung up has already stopped listening, and the app must not keep a
// blocked request alive for it.
func TestWorkflowAgentWatchRunReturnsWhenTheCallerHangsUp(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	first, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.scope(def.GrantIntrospect))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := h.app.WorkflowAgentWatchRun(ctx,
			WorkflowAgentWatchInput{ItemID: item.ID, Cursor: first.Cursor, WaitMillis: 20_000}); err != nil {
			t.Error(err)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled watch kept blocking")
	}
}

// The same row-level rule every read verb takes: a phase session with no read
// grant may watch only the runs it started, and a run it did not start is
// refused rather than watched.
func TestWorkflowAgentWatchRunRefusesARunThePhaseMayNotSee(t *testing.T) {
	h := newWatchHarness(t)
	item := h.run(t, store.WorkItem{ID: "root", Goal: "port", WorkflowID: "campaign",
		State: string(engine.StateRunning)})
	_, err := h.app.WorkflowAgentWatchRun(h.scope(), WorkflowAgentWatchInput{ItemID: item.ID})
	if err == nil {
		t.Fatal("a phase with no grant watched a run it did not start")
	}
	if _, err := h.app.WorkflowAgentWatchRun(h.scope(def.GrantIntrospect),
		WorkflowAgentWatchInput{ItemID: "nonexistent"}); err == nil {
		t.Fatal("watching a run that does not exist was accepted")
	}
}
