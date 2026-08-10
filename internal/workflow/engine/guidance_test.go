package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// The pending-guidance slot: what `run guide` leaves, and the phase entry that
// consumes it.

// guidanceWorkflow is `work` → `verify`, both agent phases, so a test can guide
// a run while its first phase is in flight and assert what the SECOND phase's
// entry consumed.
func guidanceWorkflow() def.Workflow {
	return def.Workflow{ID: "guided", Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{{To: "verify"}}),
		agentPhase("verify", nil, []def.Route{{To: "done"}}),
	}}
}

func startGuidedRun(t *testing.T, workflow def.Workflow) (*testHarness, string) {
	t.Helper()
	h := newHarness(t, Config{},
		map[string]def.Workflow{workflow.ID: workflow}, []string{"project"}, nil)
	item := testItem("item", "project", workflow.ID, 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	return h, item.ID
}

func humanGuidance(text string) GuidanceDraft {
	return GuidanceDraft{Text: text, By: GuidanceByHuman}
}

// pendingGuidanceRows reads the slot straight off the row, which is what a crash
// and a restart would read.
func pendingGuidanceRows(t *testing.T, h *testHarness, itemID string) []GuidanceEntry {
	t.Helper()
	raw, err := h.store.WorkItemPendingGuidance(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		return nil
	}
	var pending []GuidanceEntry
	if err := json.Unmarshal(raw, &pending); err != nil {
		t.Fatal(err)
	}
	return pending
}

// A run that is WORKING is the target this verb exists for: the entry lands, the
// run keeps going, and nothing about the turn in flight changes.
func TestGuideAppendsToARunningRunAndStampsTheAuthor(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())

	state, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].Text != "prefer the smaller diff" {
		t.Fatalf("pending = %+v, want one entry", state.Pending)
	}
	if state.Pending[0].By != GuidanceByHuman || state.Pending[0].At == 0 {
		t.Fatalf("entry stamp = %+v, want a human author and a time", state.Pending[0])
	}
	if state.State != StateRunning || state.PhaseID != "work" {
		t.Fatalf("guidance state = %s/%s, want running/work", state.State, state.PhaseID)
	}
	if state.Quarantined != nil {
		t.Fatalf("a healthy slot reported a discard: %+v", *state.Quarantined)
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 1 {
		t.Fatalf("persisted pending = %d entries, want 1", len(rows))
	}
	// The run is untouched: no park, no new attempt, no interruption.
	requireItemState(t, h.store, itemID, StateRunning, "")
	if starts := h.runner.startedKeys(); len(starts) != 1 {
		t.Fatalf("guiding started %d runs, want none beyond the first phase", len(starts)-1)
	}
}

// A phase's entry carries the author it was given, and an agent phase's entry
// names the run it was left by — the attribution the delivered prompt prints.
func TestGuideStampsAPhaseAuthorWithItsRun(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())

	state, err := h.engine.Guide(itemID, GuidanceDraft{
		Text: "stop after this wave", By: GuidanceByPhase, ByRun: "supervisor-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending[0].By != GuidanceByPhase || state.Pending[0].ByRun != "supervisor-run" {
		t.Fatalf("entry = %+v, want a phase author naming its run", state.Pending[0])
	}
}

// Every refusal is decided before the write, so a rejected guide leaves the slot
// byte-identical — the same totality a refused amendment gives.
func TestGuideRefusesTextItCannotDeliver(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())

	for name, draft := range map[string]GuidanceDraft{
		"empty":       humanGuidance("   "),
		"oversized":   humanGuidance(strings.Repeat("x", MaxGuidanceEntryBytes+1)),
		"no author":   {Text: "steer"},
		"bad author":  {Text: "steer", By: "the-operator"},
		"forged self": {Text: "steer", By: GuidanceAuthor("human ")},
	} {
		if _, err := h.engine.Guide(itemID, draft); err == nil {
			t.Fatalf("%s guidance was accepted", name)
		}
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 0 {
		t.Fatalf("a refused guide wrote %d entries", len(rows))
	}
}

// The slot is bounded because a run that has not reached a boundary cannot read
// what is already waiting: a ninth entry would bury the eight it joins rather
// than steer anything, so it is refused instead of rotating one out.
func TestGuideRefusesMoreThanTheSlotHolds(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())

	for index := 0; index < MaxGuidanceEntries; index++ {
		if _, err := h.engine.Guide(itemID, humanGuidance("steer")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.engine.Guide(itemID, humanGuidance("one more")); err == nil {
		t.Fatal("the slot accepted an entry past its bound")
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != MaxGuidanceEntries {
		t.Fatalf("pending = %d entries, want %d", len(rows), MaxGuidanceEntries)
	}
}

// A run with no phase entry left cannot be steered, and saying so is better than
// accepting an instruction nothing will ever read.
func TestGuideRefusesARunThatWillEnterNoPhase(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	completePhases(t, h, itemID, 2)
	requireItemState(t, h.store, itemID, StateDone, "")

	if _, err := h.engine.Guide(itemID, humanGuidance("too late")); err == nil {
		t.Fatal("a finished run accepted guidance")
	}
}

// A done run resting on its disposition is the same case wearing needs-human: it
// has finished, and the verbs that settle it are the merge/PR/discard ones.
func TestGuideRefusesADispositionPark(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	completePhases(t, h, itemID, 2)
	if err := h.engine.ParkDisposition(itemID); err != nil {
		t.Fatal(err)
	}

	_, err := h.engine.Guide(itemID, humanGuidance("too late"))
	if err == nil || !strings.Contains(err.Error(), "disposition") {
		t.Fatalf("disposition park refusal = %v", err)
	}
}

// Delivery: the entries reach the attempt the run's NEXT fresh phase entry
// creates, the slot clears in the same step, and the attempt's feedback says
// where the block came from.
func TestGuidanceIsDeliveredAtTheNextFreshPhaseEntryAndCleared(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	if _, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}
	// The turn in flight is never interrupted, and never re-rendered: work's
	// attempt was built before the guidance existed.
	if first := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 1}); len(first.Guidance) != 0 {
		t.Fatalf("the in-flight attempt was handed guidance: %+v", first.Guidance)
	}
	completePhases(t, h, itemID, 1)

	entry := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "verify", Attempt: 1})
	if len(entry.Guidance) != 1 || entry.Guidance[0].Text != "prefer the smaller diff" {
		t.Fatalf("delivered guidance = %+v", entry.Guidance)
	}
	if entry.Feedback == nil || !strings.Contains(entry.Feedback.Note, "operator guidance") {
		t.Fatalf("delivered attempt has no feedback note: %+v", entry.Feedback)
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 0 {
		t.Fatalf("slot still holds %d entries after delivery", len(rows))
	}
	// The delivery is persisted on the attempt, not only handed to the runner:
	// that row is what a crash rebuild and a human both read.
	phases, err := h.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, phase := range phases {
		if phase.PhaseID != "verify" {
			continue
		}
		var input PhaseInput
		if err := json.Unmarshal(phase.InputEnvelope, &input); err != nil {
			t.Fatal(err)
		}
		if len(input.Guidance) != 1 {
			t.Fatalf("persisted input carries %d entries, want 1", len(input.Guidance))
		}
		found = true
	}
	if !found {
		t.Fatal("verify has no persisted attempt")
	}
}

// A continuation is not a delivery boundary. An answered question continues the
// round the operator was already steering, and a block arriving mid-round would
// be a second instruction to a turn that has already read the first.
func TestGuidanceIsNotDeliveredByAnAnswerContinuation(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	seedThread(t, h.store, "work-thread")
	if err := h.store.AttachWorkItemPhaseRun(itemID, "work", 1, "work-thread", "/tmp/work.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, itemID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Answer(itemID, "yes"); err != nil {
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

// A phase that renders no prompt is not a boundary at all. Clearing the slot
// into a turn that does not exist is the silent loss the whole ordering rule
// exists to prevent, so the entries wait for a phase somebody can read them in.
func TestGuidanceSkipsAPhaseThatRendersNoPrompt(t *testing.T) {
	toolPhase := def.Phase{
		ID: "check", Driver: def.DriverTool, Check: "test",
		Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}},
		Gate:    def.Gate{Routes: []def.Route{{To: "verify"}}},
	}
	workflow := def.Workflow{ID: "guided", Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{{To: "check"}}),
		toolPhase,
		agentPhase("verify", nil, []def.Route{{To: "done"}}),
	}}
	h, itemID := startGuidedRun(t, workflow)
	if _, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}

	completePhases(t, h, itemID, 1)
	check := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "check", Attempt: 1})
	if len(check.Guidance) != 0 {
		t.Fatalf("a tool phase was handed guidance: %+v", check.Guidance)
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 1 {
		t.Fatalf("the tool entry consumed the slot: %d entries left", len(rows))
	}

	completePhases(t, h, itemID, 1)
	verify := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "verify", Attempt: 1})
	if len(verify.Guidance) != 1 {
		t.Fatalf("the next agent phase delivered %d entries, want 1", len(verify.Guidance))
	}
}

// A fan-out's units and its join carry the entries their PHASE entry delivered:
// the block is part of prompt assembly, which every element of the attempt goes
// through, and a wave whose units did not hear the steer would be steered by
// nobody.
func TestGuidanceReachesFanOutUnitsAndTheirJoin(t *testing.T) {
	workflow := def.Workflow{ID: "guided", Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{{To: "wave"}}),
		staticFanOutPhase("wave", 2, nil, []def.Route{{To: "done"}}),
	}}
	h, itemID := startGuidedRun(t, workflow)
	if _, err := h.engine.Guide(itemID, humanGuidance("only blocking findings")); err != nil {
		t.Fatal(err)
	}
	completePhases(t, h, itemID, 1)

	for _, unitID := range []string{"wave-unit-0", "wave-unit-1"} {
		start := h.runner.startFor(t, unitKey(itemID, "wave", 1, unitID))
		if len(start.Guidance) != 1 {
			t.Fatalf("unit %s delivered %d entries, want 1", unitID, len(start.Guidance))
		}
	}
	for _, unitID := range []string{"wave-unit-0", "wave-unit-1"} {
		h.runner.completeRun(t, unitKey(itemID, "wave", 1, unitID),
			Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	join := h.runner.startFor(t, unitKey(itemID, "wave", 1, "wave-join"))
	if len(join.Guidance) != 1 {
		t.Fatalf("join delivered %d entries, want 1", len(join.Guidance))
	}
}

// A parked run is a legitimate target, and the entry is read by the attempt the
// resume creates rather than by the park it was left at.
func TestGuidanceLeftOnAParkedRunIsDeliveredByTheResume(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	h.runner.complete(t, itemID, Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonStuck)

	if _, err := h.engine.Guide(itemID, humanGuidance("the blocker is cleared")); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume(itemID, "", false); err != nil {
		t.Fatal(err)
	}
	entry := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "work", Attempt: 2})
	if len(entry.Guidance) != 1 {
		t.Fatalf("resumed attempt delivered %d entries, want 1", len(entry.Guidance))
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 0 {
		t.Fatalf("slot still holds %d entries", len(rows))
	}
}

// The slot is cleared against a SESSION, not against a row. Everything between
// the attempt row and that session — a global pause holding the start, a failed
// acquisition parking it, a crash — leaves the entries pending, because an
// attempt that never ran a turn rendered nothing.

// TestGuidanceSurvivesAnAttemptThatNeverStarted is the probe: the delivery lands
// on verify#1, the run is paused before that attempt starts, and the resume that
// re-enters the phase has to carry the operator's instruction into the attempt
// that actually runs.
func TestGuidanceSurvivesAnAttemptThatNeverStarted(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	if _, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}
	// Paused before the gate advances, so verify's attempt is created and then
	// held: the row carries the entries, and nothing has rendered them.
	if err := h.engine.Pause(true); err != nil {
		t.Fatal(err)
	}
	completePhases(t, h, itemID, 1)
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 1 {
		t.Fatalf("a held start cleared the slot: %d entries left, want 1 still pending", len(rows))
	}

	if err := h.engine.PauseItem(itemID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonPaused)
	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	// The parked attempt never attached a thread, so this bare resume takes the
	// fresh-entry branch — the shape that used to lose the guidance entirely.
	if err := h.engine.Resume(itemID, "", false); err != nil {
		t.Fatal(err)
	}

	entry := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "verify", Attempt: 2})
	if len(entry.Guidance) != 1 || entry.Guidance[0].Text != "prefer the smaller diff" {
		t.Fatalf("the attempt that actually ran was handed %+v, want the pending entry", entry.Guidance)
	}
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 0 {
		t.Fatalf("slot still holds %d entries after a session rendered them", len(rows))
	}
}

// A park BEFORE the session exists is the same case wearing another reason: the
// acquisition failed, so no turn ran, so the entries are still owed.
func TestGuidanceSurvivesASetupFailedParkAfterDelivery(t *testing.T) {
	// verify claims a resource the project never sized, which fails its
	// ACQUISITION — after its attempt row, and the guidance on it, is persisted.
	h, itemID := startGuidedRun(t, def.Workflow{ID: "guided", Phases: []def.Phase{
		agentPhase("work", nil, []def.Route{{To: "verify"}}),
		agentPhase("verify", []string{"db-lock"}, []def.Route{{To: "done"}}),
	}})
	if _, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}
	completePhases(t, h, itemID, 1)
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonSetupFailed)
	if rows := pendingGuidanceRows(t, h, itemID); len(rows) != 1 {
		t.Fatalf("a setup-failed park cleared the slot: %d entries left, want 1", len(rows))
	}

	h.profiles.setCapacity("project", "db-lock", 1)
	if err := h.engine.Resume(itemID, "", false); err != nil {
		t.Fatal(err)
	}
	entry := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "verify", Attempt: 2})
	if len(entry.Guidance) != 1 {
		t.Fatalf("the repaired entry delivered %d entries, want the one still pending", len(entry.Guidance))
	}
}

// The clear removes what was delivered rather than emptying the column, because
// the slot stays live in the window the delivery opened: an entry left in it is
// one no attempt has read.
func TestGuidanceLeftWhileAnAttemptIsHeldIsNotClearedByIt(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	if _, err := h.engine.Guide(itemID, humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Pause(true); err != nil {
		t.Fatal(err)
	}
	completePhases(t, h, itemID, 1)
	// The held attempt already carries the first entry; this one arrives before
	// any session renders it, so it belongs to a LATER phase entry.
	if _, err := h.engine.Guide(itemID, humanGuidance("and skip the changelog")); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	entry := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "verify", Attempt: 1})
	if len(entry.Guidance) != 1 || entry.Guidance[0].Text != "prefer the smaller diff" {
		t.Fatalf("the released attempt rendered %+v, want only what its entry delivered", entry.Guidance)
	}
	rows := pendingGuidanceRows(t, h, itemID)
	if len(rows) != 1 || rows[0].Text != "and skip the changelog" {
		t.Fatalf("pending after the clear = %+v, want the entry nothing has read", rows)
	}
}

// A CALLED run is guided directly, for the reason its seeds are amendable
// directly: the entry reaches its own remaining phases, which is the run the
// operator is watching. The caller is not consulted and is not told — naming it
// is the app's job, not this package's.
func TestGuideAcceptsACalledRun(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)

	state, err := h.engine.Guide(child.ID, humanGuidance("narrow the audit"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("child pending = %+v, want one entry", state.Pending)
	}
	// The slot is the CHILD's row; the caller is untouched.
	if rows := pendingGuidanceRows(t, h, parent); len(rows) != 0 {
		t.Fatalf("guiding a child wrote %d entries on its caller", len(rows))
	}
}

// An undecodable slot: the column is engine-written JSON, so content that will
// not decode is corruption — and its entries are unrecoverable AS GUIDANCE
// whatever the engine does next, because nothing can render bytes nothing can
// decode. What must not happen is the failure outliving the human's response to
// it: the read used to error on every fresh agent-phase entry, so the run parked
// `wiring-error`, the resume re-entered the phase, and it parked again forever.

// corruptGuidance is a truncated array — exactly what a half-written column
// would hold, and undecodable by the whole-value decode the slot goes through.
const corruptGuidance = `[{"text":"prefer the smaller diff","at":100,"by":"human"`

// The park says all three facts, the raw bytes reach the engine log VERBATIM
// (that line is the only surviving copy), and the slot is emptied — so the bare
// resume that follows enters the phase and runs it.
func TestUndecodableGuidanceParksOnceAndThenTheRunProceeds(t *testing.T) {
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink},
		map[string]def.Workflow{"guided": guidanceWorkflow()}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("item", "project", "guided", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetWorkItemPendingGuidance("item", json.RawMessage(corruptGuidance)); err != nil {
		t.Fatal(err)
	}

	// work completes and the gate advances into verify — the fresh agent-phase
	// entry that reads the slot.
	completePhases(t, h, "item", 1)
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonWiringError)
	requireParkCause(t, h.phaseAttempt(t, "item", "verify", 1),
		"would not decode", "preserved in the engine log", "slot has been cleared")

	quarantine := sink.matching(LogEventGuidanceUndecodable)
	if len(quarantine) != 1 {
		t.Fatalf("quarantine log events = %+v, want exactly one", quarantine)
	}
	if quarantine[0].ItemID != "item" || !strings.Contains(quarantine[0].Message, corruptGuidance) {
		t.Fatalf("quarantine event = %+v, want the run id and the raw bytes verbatim", quarantine[0])
	}
	raw, err := h.store.WorkItemPendingGuidance("item")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("slot still holds %q; the park would repeat on every entry", raw)
	}

	// The heal is what makes this resume ordinary: a wiring-error park re-enters
	// the phase fresh, and the slot it reads on the way in is empty.
	if err := h.engine.Resume("item", "", false); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateRunning, "")
	entry := h.runner.startFor(t, RunKey{ItemID: "item", PhaseID: "verify", Attempt: 2})
	if len(entry.Guidance) != 0 {
		t.Fatalf("the resumed attempt was handed %+v, want nothing: the slot was cleared", entry.Guidance)
	}
	if quarantine := sink.matching(LogEventGuidanceUndecodable); len(quarantine) != 1 {
		t.Fatalf("quarantine log events after the resume = %d, want the one park", len(quarantine))
	}
}

// `guide` heals and ACCEPTS. Refusing would trade an entry already lost for the
// caller's — the only one still recoverable, and the only one an operator is
// holding in their head — so the new entry lands on the healed slot and the
// discard is surfaced instead.
func TestGuideHealsAnUndecodableSlotAndKeepsTheCallersEntry(t *testing.T) {
	h, itemID := startGuidedRun(t, guidanceWorkflow())
	if err := h.store.SetWorkItemPendingGuidance(itemID, json.RawMessage(corruptGuidance)); err != nil {
		t.Fatal(err)
	}

	state, err := h.engine.Guide(itemID, humanGuidance("and skip the changelog"))
	if err != nil {
		t.Fatalf("guide over a healed slot was refused, losing the caller's entry: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].Text != "and skip the changelog" {
		t.Fatalf("pending = %+v, want only the caller's entry", state.Pending)
	}
	rows := pendingGuidanceRows(t, h, itemID)
	if len(rows) != 1 || rows[0].Text != "and skip the changelog" {
		t.Fatalf("persisted slot = %+v, want the caller's entry on a healed column", rows)
	}
	// Loud, and loud on the ANSWER: the person who can act on the discard is the
	// one who just wrote here, so the fact rides the result they are reading.
	if state.Quarantined == nil {
		t.Fatal("the result reported no quarantine; the caller was told nothing was lost")
	}
	if state.Quarantined.Bytes != len(corruptGuidance) ||
		state.Quarantined.LogEvent != LogEventGuidanceUndecodable ||
		state.Quarantined.Reason == "" {
		t.Fatalf("quarantine = %+v, want the discarded size, the decode failure, and the log event",
			*state.Quarantined)
	}
	// And NOT on the error channel: this call succeeded, and `emitError` reports a
	// fixed "workflow operation failed" whose real cause never crosses the wire.
	if events := h.emitter.errorEvents(itemID); len(events) != 0 {
		t.Fatalf("a successful guide announced a failure: %+v", events)
	}
	// And the entry is delivered like any other, which is the whole point of
	// keeping it.
	completePhases(t, h, itemID, 1)
	entry := h.runner.startFor(t, RunKey{ItemID: itemID, PhaseID: "verify", Attempt: 1})
	if len(entry.Guidance) != 1 || entry.Guidance[0].Text != "and skip the changelog" {
		t.Fatalf("delivered guidance = %+v, want the entry left on the healed slot", entry.Guidance)
	}
}

// The ack reads the slot too, and the heal discharges it rather than failing it:
// the column is gone, so the entries this attempt rendered are gone from it with
// everything else. Nothing is owed, so nothing is announced as owed — reporting a
// redelivery here would promise one the empty column can never make.
func TestAnUndecodableSlotAtAckIsDischargedRatherThanReported(t *testing.T) {
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink},
		map[string]def.Workflow{"guided": guidanceWorkflow()}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("item", "project", "guided", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Guide("item", humanGuidance("prefer the smaller diff")); err != nil {
		t.Fatal(err)
	}
	// Paused, so verify's attempt is created holding the entry and the ack is
	// still owed when the column goes bad underneath it.
	if err := h.engine.Pause(true); err != nil {
		t.Fatal(err)
	}
	completePhases(t, h, "item", 1)
	if err := h.store.SetWorkItemPendingGuidance("item", json.RawMessage(corruptGuidance)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	if events := h.emitter.errorEvents("item"); len(events) != 0 {
		t.Fatalf("the ack announced %+v; the slot it would redeliver from is empty", events)
	}
	if quarantine := sink.matching(LogEventGuidanceUndecodable); len(quarantine) != 1 {
		t.Fatalf("quarantine log events = %d, want the one the ack's read wrote", len(quarantine))
	}
	requireItemState(t, h.store, "item", StateRunning, "")
	if rows := pendingGuidanceRows(t, h, "item"); len(rows) != 0 {
		t.Fatalf("slot holds %+v after the heal", rows)
	}
}

// failingGuidanceWrite fails the one write the heal depends on.
type failingGuidanceWrite struct {
	persistence
	fail atomic.Bool
}

func (f *failingGuidanceWrite) SetWorkItemPendingGuidance(itemID string, guidance json.RawMessage) error {
	if f.fail.Load() {
		return errors.New("slot write failed")
	}
	return f.persistence.SetWorkItemPendingGuidance(itemID, guidance)
}

// A heal whose CLEAR failed is not a heal, and the park must not claim it was:
// the column still holds what it held, so this park is not the last one, and a
// cause promising an empty slot would send the operator to a resume that parks
// again for a reason nothing stated.
func TestAFailedHealParksSayingTheSlotIsStillBad(t *testing.T) {
	failing := &failingGuidanceWrite{}
	h := newHarnessWith(t, harnessOptions{
		workflows:  map[string]def.Workflow{"guided": guidanceWorkflow()},
		projectIDs: []string{"project"},
		wrapStore: func(handle persistence) persistence {
			failing.persistence = handle
			return failing
		},
	})
	if err := h.engine.StartItem(testItem("item", "project", "guided", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetWorkItemPendingGuidance("item", json.RawMessage(corruptGuidance)); err != nil {
		t.Fatal(err)
	}
	failing.fail.Store(true)

	completePhases(t, h, "item", 1)
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonWiringError)
	cause := h.phaseAttempt(t, "item", "verify", 1).ParkCause
	if !strings.Contains(cause, "could not be cleared") {
		t.Fatalf("park cause %q does not say the slot is still bad", cause)
	}
	if strings.Contains(cause, "has been cleared") {
		t.Fatalf("park cause %q claims a repair the store refused to make", cause)
	}
	failing.fail.Store(false)
	raw, err := h.store.WorkItemPendingGuidance("item")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != corruptGuidance {
		t.Fatalf("slot = %q, want the untouched column: nothing was repaired", raw)
	}
}

// failingPhaseList is the engine's persistence with one read armed to fail. The
// phase read is the only store call `guide` makes purely to DESCRIBE the run, so
// it is the one whose failure must not be able to leave a write behind.
type failingPhaseList struct {
	persistence
	fail atomic.Bool
}

func (f *failingPhaseList) ListWorkItemPhases(itemID string) ([]store.WorkItemPhase, error) {
	if f.fail.Load() {
		return nil, errors.New("phase read failed")
	}
	return f.persistence.ListWorkItemPhases(itemID)
}

// Every refusal in `guide` is decided before the write, and the phase read is
// part of that rule rather than an exception to it: it answers what the CALLER
// is told, so failing it after the slot had already grown would report a refusal
// over a run whose slot did grow — and an operator who retried the refused call
// would leave the same instruction twice, in a slot bounded at eight.
func TestARefusedGuideLeavesTheSlotUntouched(t *testing.T) {
	failing := &failingPhaseList{}
	h := newHarnessWith(t, harnessOptions{
		workflows:  map[string]def.Workflow{"guided": guidanceWorkflow()},
		projectIDs: []string{"project"},
		wrapStore: func(handle persistence) persistence {
			failing.persistence = handle
			return failing
		},
	})
	if err := h.engine.StartItem(testItem("item", "project", "guided", 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	failing.fail.Store(true)
	if _, err := h.engine.Guide("item", humanGuidance("prefer the smaller diff")); err == nil {
		t.Fatal("Guide reported success while the read behind its answer failed")
	}
	failing.fail.Store(false)

	if rows := pendingGuidanceRows(t, h, "item"); len(rows) != 0 {
		t.Fatalf("a refused guide left %+v pending; the operator was told nothing landed", rows)
	}
}
