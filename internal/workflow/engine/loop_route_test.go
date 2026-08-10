package engine

import (
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// The session half of a loop route — which session the re-entered attempt runs
// on — plus the harness both halves share. The `prompt:` override's own tests
// are in loop_prompt_test.go.

// loopKnobWorkflow is `work` → `review`, with review's first route looping back
// to work exactly once. The loop route is the one under test; the second route
// is what the second pass through the gate takes, which is what makes the run
// finish instead of iterating until a bound stops it.
func loopKnobWorkflow(route def.Route) def.Workflow {
	work := agentPhase("work", nil, []def.Route{{To: "review"}})
	work.Prompt = "the phase's own body"
	review := agentPhase("review", nil, []def.Route{route, {To: "done"}})
	return def.Workflow{ID: "loops", Phases: []def.Phase{work, review}}
}

// startLoopRun runs the first lap: work's attempt 1 completes, review's attempt
// 1 completes, and the gate takes the loop route. `threadID` is the session
// work's first attempt ran on — seeded as a real thread row unless
// `deadThread`, which is a session the run remembers and the app no longer has.
func startLoopRun(t *testing.T, route def.Route, threadID string, deadThread bool) (*testHarness, string) {
	t.Helper()
	h := newHarness(t, Config{},
		map[string]def.Workflow{"loops": loopKnobWorkflow(route)}, []string{"project"}, nil)
	item := testItem("item", "project", "loops", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if threadID != "" {
		if !deadThread {
			seedThread(t, h.store, threadID)
		}
		if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, threadID, "/tmp/work.md"); err != nil {
			t.Fatal(err)
		}
	}
	completePhases(t, h, item.ID, 2)
	return h, item.ID
}

// completePhases settles `count` consecutive phase attempts, syncing between
// each: the engine's next start is a later turn of the command loop, so a second
// completion submitted before that turn would report the attempt that already
// finished.
func completePhases(t *testing.T, h *testHarness, itemID string, count int) {
	t.Helper()
	for step := 0; step < count; step++ {
		h.runner.complete(t, itemID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
}

// loopEntry is the request that started the attempt the loop created: work's
// SECOND attempt.
func loopEntry(t *testing.T, h *testHarness, itemID string) RunRequest {
	t.Helper()
	return h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 2})
}

// `session: continue` re-enters the target phase on the session that phase's own
// previous attempt ran, through the same PriorThreadID an `Answer` continuation
// and a resume-in-place use — there is one same-session mechanism, not two.
func TestLoopRouteSessionContinueRunsOnTheTargetPhasesOwnThread(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Session: def.SessionContinue}
	h, itemID := startLoopRun(t, route, "work-thread", false)

	entry := loopEntry(t, h, itemID)
	if entry.PriorThreadID != "work-thread" {
		t.Fatalf("loop re-entry prior thread = %q, want work-thread", entry.PriorThreadID)
	}
	if entry.Feedback != nil && strings.Contains(entry.Feedback.Note, "no longer available") {
		t.Fatalf("a successful continuation reported a degradation: %q", entry.Feedback.Note)
	}
	// The two attempts of the phase share a thread id, which is the durable
	// record that the round ran warm — no column stores the mode.
	phases, err := h.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	threads := make(map[int]string)
	for _, phase := range phases {
		if phase.PhaseID == "work" {
			threads[phase.Attempt] = phase.ThreadID
		}
	}
	if threads[1] != "work-thread" {
		t.Fatalf("work attempt 1 thread = %q, want work-thread", threads[1])
	}
}

// The default is unchanged, and deliberately: a loop that says nothing about its
// session re-enters cold, which is what every loop did before the knob existed.
func TestLoopRouteWithoutSessionReEntersCold(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1)}
	h, itemID := startLoopRun(t, route, "work-thread", false)

	if entry := loopEntry(t, h, itemID); entry.PriorThreadID != "" {
		t.Fatalf("default loop re-entry prior thread = %q, want none", entry.PriorThreadID)
	}
}

// A session that no longer exists degrades to a cold round WITH the fact stated
// in the attempt's feedback. It is never a park: the round the loop wanted still
// happens, and an unavailable optimisation must not become an outage.
func TestLoopRouteSessionContinueFallsBackToFreshWithANote(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Session: def.SessionContinue}
	h, itemID := startLoopRun(t, route, "deleted-thread", true)

	entry := loopEntry(t, h, itemID)
	if entry.PriorThreadID != "" {
		t.Fatalf("prior thread = %q, want none for a deleted session", entry.PriorThreadID)
	}
	if entry.Feedback == nil || !strings.Contains(entry.Feedback.Note, "no longer available") {
		t.Fatalf("degraded continuation left no note: %+v", entry.Feedback)
	}
	requireItemState(t, h.store, itemID, StateRunning, "")
}

// A phase that has never run holds no session, which is the same degradation as
// a deleted one and takes the same note rather than a different failure.
func TestLoopRouteSessionContinueWithNoPriorSessionRunsCold(t *testing.T) {
	route := def.Route{Loop: "work", Max: def.LiteralBound(1), Session: def.SessionContinue}
	h, itemID := startLoopRun(t, route, "", false)

	entry := loopEntry(t, h, itemID)
	if entry.PriorThreadID != "" {
		t.Fatalf("prior thread = %q, want none", entry.PriorThreadID)
	}
	if entry.Feedback == nil || !strings.Contains(entry.Feedback.Note, "starts cold") {
		t.Fatalf("missing degradation note: %+v", entry.Feedback)
	}
}

// A HUMAN gate's reject synthesizes a loop decision whose route index points at
// the `human:` route it came from. Neither knob is authorable there, so reading
// one off that route would apply a declaration validation refuses — the re-entry
// stays cold and unoverridden.
func TestHumanGateRejectIgnoresLoopKnobsOnItsOwnRoute(t *testing.T) {
	work := agentPhase("work", nil, []def.Route{{To: "review"}})
	work.Prompt = "the phase's own body"
	// The knobs are set on the human route directly, which is the shape a frozen
	// snapshot could carry even though validation refuses it today.
	review := agentPhase("review", nil, []def.Route{{
		Human:   &def.HumanRoute{Reject: &def.LoopTarget{Loop: "work", Max: def.LiteralBound(1)}},
		Session: def.SessionContinue, Prompt: "the narrower body",
	}})
	h := newHarness(t, Config{},
		map[string]def.Workflow{"loops": {ID: "loops", Phases: []def.Phase{work, review}}},
		[]string{"project"}, nil)
	item := testItem("item", "project", "loops", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "work-thread")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "work-thread", "/tmp/work.md"); err != nil {
		t.Fatal(err)
	}
	completePhases(t, h, item.ID, 2)
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonGate)
	if err := h.engine.ResolveHumanGate(item.ID, HumanReject, ""); err != nil {
		t.Fatal(err)
	}
	entry := loopEntry(t, h, item.ID)
	if entry.PriorThreadID != "" {
		t.Fatalf("reject re-entry prior thread = %q, want none", entry.PriorThreadID)
	}
	if entry.Phase.Prompt != "the phase's own body" {
		t.Fatalf("reject re-entry prompt = %q, want the phase's own body", entry.Phase.Prompt)
	}
}

// loopIntoCallWorkflow is `prepare` → `audit` (a call phase) → `report`, whose
// gate loops once back to `audit` asking to continue its session. `audit` runs
// no turn, so there is nothing there for the knob to continue — which is the
// shape the id has to be closed against.
func loopIntoCallWorkflow() def.Workflow {
	return def.Workflow{ID: "caller", Phases: []def.Phase{
		agentPhase("prepare", nil, []def.Route{{To: "audit"}}),
		callPhaseDef("audit", "child", map[string]string{"seed": "prepare.ok"}, 0,
			[]def.Route{{To: "report"}}),
		agentPhase("report", nil, []def.Route{
			{Loop: "audit", Max: def.LiteralBound(1), Session: def.SessionContinue},
			{To: "done"},
		}),
	}}
}

// A `session: continue` loop aimed at a CALL phase has no session to continue,
// and the id it would otherwise carry is consumed by nobody: a call phase starts
// a child and rests, so an id left set at its entry would be handed to whichever
// phase starts next — a turn continuing a session belonging to a phase it never
// ran. Both halves are closed here: the arming is refused with the fact stated,
// and the entry clears the field regardless of how it came to be set.
func TestLoopRouteSessionContinueIntoACallPhaseLeavesNoIdBehind(t *testing.T) {
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink}, map[string]def.Workflow{
		"caller": loopIntoCallWorkflow(), "child": childWorkflow("child"),
	}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("parent", "project", "caller", 0)); err != nil {
		t.Fatal(err)
	}
	completePhases(t, h, "parent", 1) // prepare → audit invokes the child

	// A call phase's attempt holding a thread is what an edit re-frozen by
	// `run resume --refresh-def` leaves behind when it turns an agent phase into
	// a call: the earlier rows keep the sessions they ran on, and the loop route
	// resolves its continuation out of exactly those rows.
	seedThread(t, h.store, "audit-thread")
	if err := h.store.AttachWorkItemPhaseRun("parent", "audit", 1, "audit-thread", ""); err != nil {
		t.Fatal(err)
	}

	for round, phase := range []string{"audit", "audit"} {
		child := h.callChild(t, "parent", phase, round+1)
		h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			completePhases(t, h, "parent", 1) // report's first pass takes the loop
		}
	}

	entry := h.runner.startFor(t, RunKey{ItemID: "parent", PhaseID: "report", Attempt: 2})
	if entry.PriorThreadID != "" {
		t.Fatalf("phase after the call continued session %q, which belongs to a phase it never ran",
			entry.PriorThreadID)
	}
	refusals := sink.matching(LogEventLoopSession)
	if len(refusals) != 1 || !strings.Contains(refusals[0].Message, "starts no session of its own") {
		t.Fatalf("loop-session log = %+v, want the refused continuation stated once", refusals)
	}
}
