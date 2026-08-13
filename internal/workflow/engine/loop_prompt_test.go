package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// The prompt half of a loop route: which body the round it creates renders, and
// the one entry that arming belongs to. The session half, and the harness both
// halves share, are in loop_route_test.go.

// A `prompt:` override renders instead of the phase's own body, for the one
// attempt the route created — and the phase itself is otherwise the snapshot's.
func TestLoopRoutePromptOverrideRendersOnTheAttemptItCreated(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Prompt: "the narrower body"}
	h, itemID := startLoopRun(t, route, "", false)

	entry := loopEntry(t, h, itemID)
	if entry.Phase.Prompt != "the narrower body" {
		t.Fatalf("loop re-entry prompt = %q, want the route override", entry.Phase.Prompt)
	}
	first := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 1})
	if first.Phase.Prompt != "the phase's own body" {
		t.Fatalf("first attempt prompt = %q, want the phase's own body", first.Phase.Prompt)
	}
}

// The override is route-scoped and never sticky: the NEXT attempt of the same
// phase, created by anything other than that route, renders the phase's body
// again. A resume aimed at the phase is the cheapest fresh entry to prove it on.
func TestLoopRoutePromptOverrideIsNotSticky(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Prompt: "the narrower body"}
	h, itemID := startLoopRun(t, route, "", false)

	// Park the run so a targeted resume can enter the phase again.
	if err := h.engine.PauseItem(itemID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume(itemID, "work", false); err != nil {
		t.Fatal(err)
	}
	third := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 3})
	if third.Phase.Prompt != "the phase's own body" {
		t.Fatalf("attempt 3 prompt = %q, want the phase's own body", third.Phase.Prompt)
	}
}

// Both knobs on one route compose: the override becomes the authored body in
// the warm round's full prompt; provider context reuse does not make it a
// recovery continuation.
func TestLoopRouteSessionContinueComposesWithThePromptOverride(t *testing.T) {
	route := def.Route{
		Loop: "work", Max: def.LiteralBound(1),
		Session: def.SessionContinue, Prompt: "the narrower body",
	}
	h, itemID := startLoopRun(t, route, "work-thread", false)

	entry := loopEntry(t, h, itemID)
	if entry.Launch.ThreadID() != "work-thread" || entry.Phase.Prompt != "the narrower body" ||
		entry.Launch.ContinuesThread() {
		t.Fatalf("composed loop re-entry = thread %q prompt %q", entry.Launch.ThreadID(), entry.Phase.Prompt)
	}
}

func TestPhaseTurnLaunchRejectsContradictoryState(t *testing.T) {
	for _, test := range []struct {
		name     string
		entry    phaseEntry
		threadID string
		finalize bool
	}{
		{name: "continuation without thread", entry: entryContinuation},
		{name: "restart with old thread", entry: entryRestart, threadID: "old-thread"},
		{name: "finalize on fresh entry", entry: entryFresh, threadID: "old-thread", finalize: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := phaseTurnLaunch(test.entry, test.threadID, test.finalize); err == nil {
				t.Fatal("contradictory launch state was accepted")
			}
		})
	}
	warm, err := phaseTurnLaunch(entryFresh, "old-thread", false)
	if err != nil || !warm.ReusesThread() || warm.ContinuesThread() {
		t.Fatalf("warm fresh entry = %+v, %v", warm, err)
	}
}

func TestUnavailableContinuationReconstructsTheSameRound(t *testing.T) {
	route := def.Route{
		Loop: "work", Max: def.LiteralBound(1),
		Session: def.SessionContinue, Prompt: "the narrower body",
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"loops": loopKnobWorkflow(route),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "loops", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "work-thread")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "work-thread", "/tmp/work-1.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Guide(item.ID, humanGuidance("keep the compatibility layer")); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 2, "work-thread", "/tmp/work-2.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: json.RawMessage(`{"status":"question"}`)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	h.runner.startErrs["item/work/3"] = errors.Join(ErrProviderContextUnavailable, errors.New("provider thread missing"))
	if err := h.engine.Answer(item.ID, "use the safe option"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	failedContinuation := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 3})
	if !failedContinuation.Launch.ContinuesThread() {
		t.Fatalf("attempt 3 launch = %+v, want the attempted continuation", failedContinuation.Launch)
	}
	restarted := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 4})
	if restarted.Launch.ReusesThread() || restarted.Phase.Prompt != "the narrower body" {
		t.Fatalf("restart = launch %+v prompt %q, want a fresh session reconstructing the route prompt",
			restarted.Launch, restarted.Phase.Prompt)
	}
	if len(restarted.Guidance) != 1 || restarted.Guidance[0].Text != "keep the compatibility layer" {
		t.Fatalf("restart guidance = %+v, want the parked round's guidance", restarted.Guidance)
	}
	if restarted.Feedback == nil || !strings.Contains(restarted.Feedback.Note, "use the safe option") ||
		!strings.Contains(restarted.Feedback.Note, continuationUnavailableNote) {
		t.Fatalf("restart feedback = %+v, want the answer and context-loss note", restarted.Feedback)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if phase, ok := phaseAttempt(phases, "work", 3); !ok || phase.Status != "superseded" {
		t.Fatalf("failed continuation row = %+v, found %v; want superseded", phase, ok)
	}
}

func TestUnavailableWarmReuseReconstructsTheNewRound(t *testing.T) {
	route := def.Route{
		Loop: "work", Max: def.LiteralBound(1),
		Session: def.SessionContinue, Prompt: "the narrower body",
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"loops": loopKnobWorkflow(route),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "loops", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "work-thread")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "work-thread", "/tmp/work-1.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Guide(item.ID, humanGuidance("keep the compatibility layer")); err != nil {
		t.Fatal(err)
	}
	h.runner.startErrs["item/work/2"] = errors.Join(ErrProviderContextUnavailable, errors.New("provider thread missing"))
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	warm := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 2})
	if !warm.Launch.ReusesThread() || warm.Launch.ContinuesThread() {
		t.Fatalf("attempt 2 launch = %+v, want the unavailable warm context", warm.Launch)
	}
	restarted := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 3})
	if restarted.Launch.ReusesThread() || restarted.Phase.Prompt != "the narrower body" {
		t.Fatalf("restart = launch %+v prompt %q, want a cold reconstruction of the warm round",
			restarted.Launch, restarted.Phase.Prompt)
	}
	if len(restarted.Guidance) != 1 || restarted.Guidance[0].Text != "keep the compatibility layer" {
		t.Fatalf("restart guidance = %+v, want the round's original guidance", restarted.Guidance)
	}
	if restarted.Feedback == nil || !strings.Contains(restarted.Feedback.Note, loopContinuationNote) {
		t.Fatalf("restart feedback = %+v, want the warm-context degradation", restarted.Feedback)
	}
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if phase, ok := phaseAttempt(phases, "work", 2); !ok || phase.Status != "superseded" {
		t.Fatalf("unavailable warm attempt = %+v, found %v; want superseded", phase, ok)
	}
}

// The override belongs to ONE entry of ONE phase — the loop route's own target —
// and the tests below are the boundary cases where it used to escape that.

// gatedLoopWorkflow is the converge-on-review shape: `work` loops from
// `review`'s gate under a narrower body, and `work`'s own gate hands the run to
// a human once the work reports ok. The human's approve then advances to a THIRD
// phase, which is the entry the override must not reach: the attempt the run
// parks on is the one that rendered it.
func gatedLoopWorkflow() def.Workflow {
	work := agentPhase("work", nil, []def.Route{
		{
			When:  &def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: true}},
			Human: &def.HumanRoute{Approve: "report", Reject: &def.LoopTarget{Loop: "review", Max: def.LiteralBound(1)}},
		},
		{To: "review"},
	})
	work.Prompt = "the phase's own body"
	review := agentPhase("review", nil, []def.Route{
		{Loop: "work", Max: def.LiteralBound(1), Prompt: "the narrower body"},
		{To: "done"},
	})
	report := agentPhase("report", nil, []def.Route{{To: "done"}})
	report.Prompt = "the report's own body"
	return def.Workflow{ID: "loops", Phases: []def.Phase{work, review, report}}
}

// startGatedLoopRun runs work#1 (not ok, so it advances), review#1 (which loops
// with the override), and work#2 (which renders it and reports ok, so its gate
// parks on the human route).
func startGatedLoopRun(t *testing.T) (*testHarness, string) {
	t.Helper()
	h := newHarness(t, Config{},
		map[string]def.Workflow{"loops": gatedLoopWorkflow()}, []string{"project"}, nil)
	item := testItem("item", "project", "loops", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []bool{false, true, true} {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(ok)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if entry := loopEntry(t, h, item.ID); entry.Phase.Prompt != "the narrower body" {
		t.Fatalf("the loop round rendered %q, want the route override", entry.Phase.Prompt)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	return h, item.ID
}

// The gate that parks is on the phase the loop re-entered, so the parked attempt
// is the one that ran under the override. Approving it advances to a DIFFERENT
// phase, and that phase renders its own body: the override was consumed by the
// entry the loop route made, and nothing re-arms it.
func TestPromptOverrideDoesNotFollowAGateResolveIntoTheNextPhase(t *testing.T) {
	h, itemID := startGatedLoopRun(t)

	if err := h.engine.ResolveHumanGate(itemID, HumanApprove, ""); err != nil {
		t.Fatal(err)
	}
	report := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "report", Attempt: 1})
	if report.Phase.Prompt != "the report's own body" {
		t.Fatalf("report rendered %q, want its own body", report.Phase.Prompt)
	}
	// The record agrees with what ran: the attempt claims no route coordinate.
	if route := persistedPromptRoute(t, h, itemID, "report", 1); route != nil {
		t.Fatalf("report's input claims prompt route %+v", route)
	}
}

// A bare resume of the parked round IS that round continuing, so it renders the
// same narrower body — restored from the attempt's own persisted input, which is
// the only thing `loadParked` carries forward.
func TestPromptOverrideSurvivesAContinuationOfTheRoundItCreated(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Prompt: "the narrower body"}
	h, itemID := startLoopRun(t, route, "", false)
	seedThread(t, h.store, "loop-thread")
	if err := h.store.AttachWorkItemPhaseRun(itemID, "work", 2, "loop-thread", "/tmp/work.md"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem(itemID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume(itemID, "", false); err != nil {
		t.Fatal(err)
	}

	continued := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 3})
	if continued.Phase.Prompt != "the narrower body" {
		t.Fatalf("the continuation rendered %q, want the override the round was running", continued.Phase.Prompt)
	}
	if continued.Launch.ThreadID() != "loop-thread" || !continued.Launch.ContinuesThread() {
		t.Fatalf("the continuation = %+v, want the parked session with a continuation prompt", continued)
	}
}

// A resume whose parked attempt has no session left reconstructs that same
// round in a new provider context. The context must receive the full prompt the
// round originally ran, including the loop route's narrower body.
func TestPromptOverrideSurvivesASessionLostRestart(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Prompt: "the narrower body"}
	h, itemID := startLoopRun(t, route, "", false)
	if err := h.engine.PauseItem(itemID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume(itemID, "", false); err != nil {
		t.Fatal(err)
	}

	restarted := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 3})
	if restarted.Phase.Prompt != "the narrower body" {
		t.Fatalf("the session-lost restart rendered %q, want the parked round's route body", restarted.Phase.Prompt)
	}
}

// A park that lands ON the loop decision — step mode, and the crash rebuild that
// replays the same persisted decision — still makes the round the route asked
// for. The arming comes from the DECISION, which is why nothing has to survive
// the park on the run's restored state.
func TestPromptOverrideIsReArmedByAStepModeApproval(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Prompt: "the narrower body"}
	h := newHarness(t, Config{},
		map[string]def.Workflow{"loops": loopKnobWorkflow(route)}, []string{"project"}, nil)
	item := testItem("item", "project", "loops", 0)
	item.StepMode = true
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// work#1's advance parks first; review#1's loop decision parks second.
	for step := 0; step < 2; step++ {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
		if err := h.engine.ResolveHumanGate(item.ID, HumanApprove, ""); err != nil {
			t.Fatal(err)
		}
	}

	entry := loopEntry(t, h, item.ID)
	if entry.Phase.Prompt != "the narrower body" {
		t.Fatalf("the approved loop decision rendered %q, want the route override", entry.Phase.Prompt)
	}
	if route := persistedPromptRoute(t, h, item.ID, "work", 2); route == nil {
		t.Fatal("the attempt that rendered the override recorded no route coordinate")
	}
}

// persistedPromptRoute reads back what an attempt's input envelope says it
// rendered, which is the durable half of the same claim the request carries.
func persistedPromptRoute(t *testing.T, h *testHarness, itemID, phaseID string, attempt int) *PromptRoute {
	t.Helper()
	phases, err := h.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := phaseAttempt(phases, phaseID, attempt)
	if !ok {
		t.Fatalf("attempt %s/%d has no row", phaseID, attempt)
	}
	var input PhaseInput
	if len(row.InputEnvelope) > 0 {
		if err := decodeJSON(row.InputEnvelope, &input); err != nil {
			t.Fatal(err)
		}
	}
	return input.PromptRoute
}

// The crash-rebuild path is the third re-arm, and the one with no live state at
// all: the process died between the gate that decided the loop and the attempt
// row that decision would have created. The rebuild replays the persisted
// decision through the same `applyLoopRoute`, so the round comes back narrowed.
//
// This is the whole reason `loadParked` restores no arming: nothing has to
// survive a crash on the run's restored state, because the DECISION is what the
// arming is derived from and the decision is on the row.
func TestPromptOverrideIsReArmedByACrashRebuild(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Prompt: "the narrower body"}
	workflow := loopKnobWorkflow(route)
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	trace, err := json.Marshal(def.GateTrace{Decision: def.RouteDecision{
		Kind: def.DecisionLoop, RouteIndex: 0, Target: "work",
		LoopEdge: def.GateEdgeKey("review", 0), Max: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The run is persisted mid-flight: work#1 and review#1 completed, review#1's
	// gate decided the loop, and no row for the attempt it decided on exists.
	h := newHarness(t, Config{}, map[string]def.Workflow{"loops": workflow}, []string{"project"},
		func(database *store.Store) {
			item := testItem("item", "project", "loops", 0)
			if err := database.CreateWorkItem(item); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateWorkItemRunStart(item.ID, snapshot, "", "", "", 20); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []store.WorkItemPhase{
				{
					ItemID: item.ID, PhaseID: "work", Attempt: 1,
					InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
					Status: "completed", StartedAt: 21, EndedAt: 22,
				},
				{
					ItemID: item.ID, PhaseID: "review", Attempt: 1,
					InputEnvelope: json.RawMessage(`{"vars":{}}`), OutputEnvelope: doneEnvelope(true),
					GateTrace: trace, Status: "completed", StartedAt: 23, EndedAt: 24,
				},
			} {
				if err := database.CreateWorkItemPhase(phase); err != nil {
					t.Fatal(err)
				}
			}
		})

	entry := loopEntry(t, h, "item")
	if entry.Phase.Prompt != "the narrower body" {
		t.Fatalf("the rebuilt loop round rendered %q, want the route override", entry.Phase.Prompt)
	}
	if route := persistedPromptRoute(t, h, "item", "work", 2); route == nil {
		t.Fatal("the rebuilt attempt rendered the override without recording its coordinate")
	}
	requireItemState(t, h.store, "item", StateRunning, "")
}
