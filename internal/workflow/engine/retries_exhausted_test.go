package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// A `retries-exhausted` park is a turn that DIED — the runner's transient layer
// gave up on a provider API failure — and the session it died in is still there.
// These tests pin that a bare resume continues on it, that the two things a
// continuation cannot do (refill a loop bound, deliver pending guidance) stay
// undone, and that `--phase` is still the start-over.

// parkRetriesExhausted drives a single-shape run into the park the runner
// produces when transient retries run out, with its attempt attached to a live
// provider session.
func parkRetriesExhausted(t *testing.T, h *testHarness, itemID, threadID string) string {
	t.Helper()
	item := testItem(itemID, "project", "pausable", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if threadID != "" {
		seedThread(t, h.store, threadID)
		if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, threadID, "/tmp/narrative.md"); err != nil {
			t.Fatal(err)
		}
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeTransientExhausted})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonRetriesExhausted)
	return item.ID
}

// The change itself: `run resume` with no phase takes the next turn on the
// session the dead one ran in, instead of throwing away a turn's context that a
// phase running for many minutes had built up.
func TestResumeAfterRetriesExhaustedContinuesTheParkedSession(t *testing.T) {
	h := newPauseHarness(t)
	item := parkRetriesExhausted(t, h, "item", "thread-one")

	if err := h.engine.Resume(item, "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Key.Attempt != 2 || starts[1].PriorThreadID != "thread-one" ||
		starts[1].PromptMode != PromptContinue {
		t.Fatalf("resume starts = %+v, want a second attempt on the session that died", starts)
	}
	if starts[1].Feedback == nil ||
		!strings.Contains(starts[1].Feedback.Note, "resumed after the phase ran out of retries") ||
		!strings.Contains(starts[1].Feedback.Note, "continue from where the previous turn stopped") {
		t.Fatalf("resume feedback = %+v, want the note naming the park", starts[1].Feedback)
	}
	// The continuation is an ordinary attempt from there on.
	h.runner.complete(t, item, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
}

// The session can genuinely be gone — a thread deleted while the run was parked
// — and that falls back to the phase's inputs with the loss stated, rather than
// handing the runner a dead id and parking `agent-error` from a failed start.
func TestResumeAfterRetriesExhaustedFallsBackWhenTheSessionIsGone(t *testing.T) {
	h := newPauseHarness(t)
	item := parkRetriesExhausted(t, h, "item", "thread-one")
	if err := h.store.DeleteThread("thread-one"); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.Resume(item, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].PriorThreadID != "" || starts[1].PromptMode != PromptFull {
		t.Fatalf("resume after session loss = %+v, want a fresh attempt", starts)
	}
	if starts[1].Feedback == nil ||
		!strings.Contains(starts[1].Feedback.Note, "provider session is unavailable") {
		t.Fatalf("session-loss feedback = %+v, want the loss recorded", starts[1].Feedback)
	}
}

// Naming a phase is still the deliberate start-over, and `--refresh-def` is
// still offered only there: a bare resume now continues an attempt launched
// under the frozen definition, so the refusal names the flag's one entry point.
func TestResumeWithAPhaseAfterRetriesExhaustedIsStillTheFreshEntry(t *testing.T) {
	h := newPauseHarness(t)
	item := parkRetriesExhausted(t, h, "item", "thread-one")

	err := h.engine.Resume(item, "", true)
	if err == nil || !strings.Contains(err.Error(), "--phase work") {
		t.Fatalf("bare --refresh-def resume = %v, want a refusal naming the fresh entry", err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonRetriesExhausted)

	if err := h.engine.Resume(item, "work", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].PriorThreadID != "" || starts[1].PromptMode != PromptFull {
		t.Fatalf("targeted resume = %+v, want a cold attempt", starts)
	}
}

func TestFreshEntryDropsThePreviousContinuationsControlNote(t *testing.T) {
	h := newPauseHarness(t)
	item := parkRetriesExhausted(t, h, "item", "thread-one")
	if err := h.engine.Resume(item, "", false); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item, Outcome{Kind: OutcomeExecutionFailure})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonAgentError)

	if err := h.engine.Resume(item, "work", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	fresh := starts[len(starts)-1]
	if fresh.PromptMode != PromptFull || fresh.PriorThreadID != "" || fresh.Feedback != nil {
		t.Fatalf("fresh entry inherited continuation state: %+v", fresh)
	}
}

// loopExhaustionWorkflow spends a loop bound of one: build → review, review
// loops back to build once and then has no route left, which is the OTHER thing
// `retries-exhausted` means.
func loopExhaustionWorkflow() def.Workflow {
	return def.Workflow{ID: "loop", Phases: []def.Phase{
		agentPhase("build", nil, []def.Route{{To: "review"}}),
		agentPhase("review", nil, []def.Route{{Loop: "build", Max: def.LiteralBound(1)}}),
	}}
}

// A continuation refills nothing. It is the same answer the fresh entry gave —
// re-entering the phase that parked is not an entry from outside its cycle — so
// a resumed run whose LOOP bound ran out parks on it again, and only an earlier
// phase gives the bound back. A continuation that quietly refilled would let a
// bounded loop iterate forever.
func TestRetriesExhaustedContinuationRefillsNoLoopBudget(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"loop": loopExhaustionWorkflow()},
		[]string{"project"}, nil)
	item := testItem("item", "project", "loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// build → review → (loop) build → review, and the bound of one is spent.
	for round := 0; round < 3; round++ {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonRetriesExhausted)

	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	// The continued round is a second attempt of `review`, not a third lap of the
	// loop: nothing gave the edge its bound back.
	resumed := h.runner.started()
	last := resumed[len(resumed)-1]
	if last.Key.PhaseID != "review" || last.Key.Attempt != 3 {
		t.Fatalf("continued attempt = %s/%d, want a further attempt of review", last.Key.PhaseID, last.Key.Attempt)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonRetriesExhausted)

	// The loop's TARGET, entered from outside the cycle, is what refills it.
	if err := h.engine.Resume(item.ID, "build", false); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	after := h.runner.started()
	if last := after[len(after)-1]; last.Key.PhaseID != "build" {
		t.Fatalf("after a refilled bound the run is at %s, want the loop taken again", last.Key.PhaseID)
	}
}

// The phase-level park of a fan-out is its JOIN's: the join's envelope is the
// phase's, so its transient exhaustion parks the phase. Resuming continues the
// join on the session it died in, over the results the wave already produced —
// re-expanding the wave would re-run finished work, which for a campaign is
// whole child runs.
func TestResumeAfterAJoinsRetriesExhaustedContinuesTheJoin(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	for _, unitID := range []string{"work-unit-0", "work-unit-1"} {
		h.runner.completeRun(t, unitKey(item, "work", 1, unitID),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(unitID)})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, "join-thread")
	if err := h.store.AttachWorkItemPhaseRun(item, "work", 1, "join-thread", ""); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{Kind: OutcomeTransientExhausted})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonRetriesExhausted)

	if err := h.engine.Resume(item, "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	// The attempt is repaired, not replaced: the units keep their results and only
	// the join runs again.
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitDone,
		"work-unit-1": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitRunning,
	})
	rerun := h.runner.startFor(t, unitKey(item, "work", 1, "work-join"))
	if rerun.PriorThreadID != "join-thread" {
		t.Fatalf("join prior thread = %q, want the session the attempt parked on", rerun.PriorThreadID)
	}
	if rerun.Feedback == nil || !strings.Contains(rerun.Feedback.Note, "ran out of retries") {
		t.Fatalf("join feedback = %+v, want the resume note naming why it parked", rerun.Feedback)
	}
}

// A continuation is not a delivery boundary, and this park is no exception: the
// round it continues has already read whatever prompt it is going to read, so a
// block arriving now would be a second instruction to a turn in flight.
func TestGuidanceIsNotDeliveredByARetriesExhaustedContinuation(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	seedThread(t, h.store, "work-thread")
	if err := h.store.AttachWorkItemPhaseRun(itemID, "work", 1, "work-thread", "/tmp/work.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, itemID, Outcome{Kind: OutcomeTransientExhausted})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonRetriesExhausted)
	if _, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.Resume(itemID, "", false); err != nil {
		t.Fatal(err)
	}
	entry := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 2})
	if len(entry.Guidance) != 0 {
		t.Fatalf("a continuation consumed guidance: %+v", entry.Guidance)
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 1 {
		t.Fatalf("pending after a continuation = %d entries, want 1 still waiting", len(rows))
	}
}

// `run amend` says WHEN the run reads a new seed, and for this park the honest
// answer is the next attempt: a single-shape continuation continues the SESSION,
// and the turn it starts builds its variable context from the run row like any
// other. The second half is what makes the label more than a string.
func TestAmendSeedsOnARetriesExhaustedParkIsReadByTheContinuation(t *testing.T) {
	h := newSeededHarness(t)
	item := startSeeded(t, h, `{"fix-budget":2}`)
	seedThread(t, h.store, "work-thread")
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 1, "work-thread", "/tmp/work.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeTransientExhausted})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonRetriesExhausted)

	amendment, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": float64(4)})
	if err != nil {
		t.Fatal(err)
	}
	if amendment.Effect != SeedEffectNextAttempt {
		t.Fatalf("effect = %q, want %q", amendment.Effect, SeedEffectNextAttempt)
	}
	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if last := starts[len(starts)-1]; last.PriorThreadID != "work-thread" ||
		last.PromptMode != PromptContinue || last.Feedback == nil || len(last.Feedback.Values) != 1 ||
		fmt.Sprint(last.Feedback.Values["fix-budget"]) != "4" {
		t.Fatalf("resumed attempt = %+v, want a continuation carrying only the amended input", last)
	}
	if got := startedVar(t, h, len(starts)-1, "fix-budget"); got != "4" {
		t.Fatalf("continued attempt fix-budget = %s, want the amended 4", got)
	}
}

// A fan-out park is the other answer, and it is not a wording difference: the
// join a bare resume reopens runs on the variables the attempt froze
// (`restoreFanOut` reads them back from the persisted input envelope), so the
// amendment names the fresh entry that would actually read it.
func TestAmendSeedsOnARetriesExhaustedFanOutParkReportsAFreshEntry(t *testing.T) {
	workflow := fanOutWorkflow("flow", 1)
	workflow.Inputs = map[string]def.Variable{
		"fix-budget": {Schema: def.JSONSchema{Type: "number"}},
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"flow": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	item.Seeds = json.RawMessage(`{"fix-budget":2}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("a")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-join"), Outcome{Kind: OutcomeTransientExhausted})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonRetriesExhausted)

	amendment, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": float64(4)})
	if err != nil {
		t.Fatal(err)
	}
	if amendment.Effect != SeedEffectFreshEntry {
		t.Fatalf("effect = %q, want %q for an attempt a bare resume repairs in place",
			amendment.Effect, SeedEffectFreshEntry)
	}
}

// The refusal for every park that is NOT continuable has to name the set, and
// naming it from the same list the predicate reads is what keeps the two from
// drifting apart.
func TestResumeRefusalNamesEveryContinuableReason(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"question": onePhaseWorkflow("question", nil, []def.Route{{To: "done"}}),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "question", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	err := h.engine.ResumeItem(item.ID)
	if err == nil {
		t.Fatal("resume of a question park was accepted")
	}
	for _, reason := range continuableReasons {
		if !strings.Contains(err.Error(), string(reason)) {
			t.Fatalf("refusal %q does not name %q", err, reason)
		}
	}
}
