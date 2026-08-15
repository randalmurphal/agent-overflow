package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// Feedback that no turn ever read, and the attempt that reads it instead.
//
// The incident these tests pin: an operator answered a parked question, the
// engine persisted the answer as the new attempt's `input_envelope.feedback`
// and dispatched the continuation, and the runner start wedged. The answer was
// durably recorded and effectively destroyed — nothing ever re-read a parked
// attempt's feedback, so the fresh entry the operator recovered with carried
// none of it and `run guide` was the only way left to say it again by hand.

const answerText = "use the safe option"

// feedbackWorkflow is the single-shape agent phase that owes feedback: the one
// shape whose `startRunner` actually sends `RunRequest.Feedback`.
func feedbackWorkflow() def.Workflow {
	return onePhaseWorkflow("feedback", nil, []def.Route{{To: "done"}})
}

// startAskingRun admits a run, gives its first attempt a provider session, and
// parks it on a question — the state `Answer` is the only valid verb for.
func startAskingRun(t *testing.T, h *testHarness, itemID, threadID string) store.WorkItem {
	t.Helper()
	item := testItem(itemID, "project", "feedback", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	seedThread(t, h.store, threadID)
	if err := h.store.AttachWorkItemPhaseRun(itemID, "work", 1, threadID, "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, itemID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonQuestion)
	return item
}

func newFeedbackHarness(t *testing.T) *testHarness {
	t.Helper()
	return newHarness(t, Config{},
		map[string]def.Workflow{"feedback": feedbackWorkflow()}, []string{"project"}, nil)
}

// attemptRow reads one persisted attempt, which is where both halves of the
// contract live: the note in the input envelope, and whether it is still owed.
func attemptRow(t *testing.T, h *testHarness, itemID, phaseID string, attempt int) store.WorkItemPhase {
	t.Helper()
	phases, err := h.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := phaseAttempt(phases, phaseID, attempt)
	if !ok {
		t.Fatalf("attempt %s/%d does not exist (phases: %+v)", phaseID, attempt, phases)
	}
	return row
}

// requireWedgedStart asserts a verb whose runner start failed reports it. A
// start that fails inside the reply budget still fails the verb, so a test that
// swallowed the error would be asserting nothing about the state it then reads.
func requireWedgedStart(t *testing.T, err error, verb string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s reported success even though its runner start never began a turn", verb)
	}
	if !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("%s failed with %v, want the setup failure the start reported", verb, err)
	}
}

func attemptFeedbackNote(t *testing.T, row store.WorkItemPhase) string {
	t.Helper()
	if len(row.InputEnvelope) == 0 {
		return ""
	}
	var input PhaseInput
	if err := json.Unmarshal(row.InputEnvelope, &input); err != nil {
		t.Fatal(err)
	}
	if input.Feedback == nil {
		return ""
	}
	return input.Feedback.Note
}

// THE INCIDENT, end to end. The answer reaches a turn even though the
// continuation it was written for never started one.
func TestUnrenderedAnswerIsRedeliveredIntoTheNextEntryOfThatPhase(t *testing.T) {
	h := newFeedbackHarness(t)
	item := startAskingRun(t, h, "item", "thread-one")

	// The continuation wedges: the runner never starts, so no turn ever renders
	// the answer this attempt persisted.
	h.runner.startErrs["item/work/2"] = errors.Join(ErrSetupFailed, errors.New("workspace would not provision"))
	requireWedgedStart(t, h.engine.Answer(item.ID, answerText), "answer")
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)

	parked := attemptRow(t, h, item.ID, "work", 2)
	if note := attemptFeedbackNote(t, parked); note != answerText {
		t.Fatalf("attempt 2 feedback = %q, want the answer", note)
	}
	if parked.FeedbackDeliveredAt != 0 {
		t.Fatalf("attempt 2 feedback_delivered_at = %d, want 0: no session ever rendered it",
			parked.FeedbackDeliveredAt)
	}

	// The recovery the operator actually reaches for: a fresh entry into the
	// phase. It used to carry none of the answer.
	delete(h.runner.startErrs, "item/work/2")
	if err := h.engine.Resume(item.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	recovered := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 3})
	if recovered.Feedback == nil {
		t.Fatal("the recovered attempt carried no feedback at all")
	}
	for _, want := range []string{
		answerText,
		"undelivered feedback from attempt 2",
		"never rendered by a provider session",
	} {
		if !strings.Contains(recovered.Feedback.Note, want) {
			t.Fatalf("recovered feedback %q does not state %q", recovered.Feedback.Note, want)
		}
	}
	// The record and the prompt agree: the row the turn ran under carries it too.
	if note := attemptFeedbackNote(t, attemptRow(t, h, item.ID, "work", 3)); !strings.Contains(note, answerText) {
		t.Fatalf("attempt 3 persisted feedback = %q, want the redelivered answer", note)
	}
	// And the source is settled the moment its carrier's row exists, so the entry
	// after this one does not deliver it a third time.
	if attemptRow(t, h, item.ID, "work", 2).FeedbackDeliveredAt == 0 {
		t.Fatal("attempt 2 still owes feedback after a later attempt carried it")
	}
}

// The ack: a prompt handed to a live provider session is the proof a turn
// renders the note, and it is the only thing that settles it. The next attempt
// then carries only what it was given.
func TestRenderedFeedbackIsSettledByTheSendAndNotRedelivered(t *testing.T) {
	h := newFeedbackHarness(t)
	item := startAskingRun(t, h, "item", "thread-one")

	if err := h.engine.Answer(item.ID, answerText); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	answered := attemptRow(t, h, item.ID, "work", 2)
	if answered.FeedbackDeliveredAt == 0 {
		t.Fatal("a dispatched attempt still owes its feedback; the send is what settles it")
	}

	// Ask again, answer again. Attempt 3 must carry the SECOND answer and nothing
	// else: the first was rendered, so redelivering it would tell the round
	// something it already heard.
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "work", 2, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Answer(item.ID, "and commit it"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	second := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 3})
	if second.Feedback == nil || second.Feedback.Note != "and commit it" {
		t.Fatalf("attempt 3 feedback = %+v, want only the second answer", second.Feedback)
	}
	if strings.Contains(second.Feedback.Note, "undelivered feedback") {
		t.Fatalf("a rendered answer was redelivered: %q", second.Feedback.Note)
	}
}

// A note that passes through two attempts unread is delivered ONCE per entry,
// not once per attempt that ever held it. The chain nests its provenance rather
// than duplicating the instruction.
func TestFeedbackIsNotRedeliveredTwiceAcrossSuccessiveEntries(t *testing.T) {
	h := newFeedbackHarness(t)
	item := startAskingRun(t, h, "item", "thread-one")

	h.runner.startErrs["item/work/2"] = errors.Join(ErrSetupFailed, errors.New("first wedge"))
	h.runner.startErrs["item/work/3"] = errors.Join(ErrSetupFailed, errors.New("second wedge"))
	requireWedgedStart(t, h.engine.Answer(item.ID, answerText), "answer")
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	// First recovery: attempt 3 carries the answer, and wedges too.
	requireWedgedStart(t, h.engine.Resume(item.ID, "work", false), "resume")
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonSetupFailed)

	// Second recovery: attempt 4 is the first one that actually runs.
	delete(h.runner.startErrs, "item/work/3")
	if err := h.engine.Resume(item.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	recovered := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 4})
	if recovered.Feedback == nil {
		t.Fatal("the second recovery carried no feedback")
	}
	if count := strings.Count(recovered.Feedback.Note, answerText); count != 1 {
		t.Fatalf("the answer appears %d times in %q, want exactly once", count, recovered.Feedback.Note)
	}
	// The provenance names attempt 3 — the row it was actually taken from — and
	// carries attempt 2's own provenance inside it, because the note really did
	// pass through both unread.
	for _, want := range []string{"undelivered feedback from attempt 3", "undelivered feedback from attempt 2"} {
		if !strings.Contains(recovered.Feedback.Note, want) {
			t.Fatalf("chained provenance %q does not state %q", recovered.Feedback.Note, want)
		}
	}
	// Every source is settled: nothing is owed once a turn has it.
	for _, attempt := range []int{2, 3} {
		if row := attemptRow(t, h, item.ID, "work", attempt); row.FeedbackDeliveredAt == 0 {
			t.Fatalf("attempt %d still owes feedback after attempt 4 carried it", attempt)
		}
	}
}

// A provider-context restart carries the parked round's feedback into the
// reconstruction itself. That is the ONE carry-forward that does not go through
// the redelivery read, so the two mechanisms must not both deliver it.
func TestProviderContextRestartCarriesFeedbackExactlyOnce(t *testing.T) {
	h := newFeedbackHarness(t)
	item := startAskingRun(t, h, "item", "thread-one")

	h.runner.startErrs["item/work/2"] = errors.Join(ErrProviderContextUnavailable, errors.New("provider thread missing"))
	if err := h.engine.Answer(item.ID, answerText); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	restarted := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 3})
	if restarted.Feedback == nil {
		t.Fatal("the reconstruction carried no feedback")
	}
	if count := strings.Count(restarted.Feedback.Note, answerText); count != 1 {
		t.Fatalf("the answer appears %d times in %q, want exactly once", count, restarted.Feedback.Note)
	}
	if strings.Contains(restarted.Feedback.Note, "undelivered feedback from attempt") {
		t.Fatalf("the restart's carry-forward was redelivered on top of itself: %q", restarted.Feedback.Note)
	}
	if !strings.Contains(restarted.Feedback.Note, continuationUnavailableNote) {
		t.Fatalf("reconstruction feedback %q does not state the context loss", restarted.Feedback.Note)
	}
	// The superseded row is settled by the restart, and the reconstruction's own
	// row owes nothing either, because its start succeeded.
	if row := attemptRow(t, h, item.ID, "work", 2); row.FeedbackDeliveredAt == 0 {
		t.Fatal("the superseded attempt still owes feedback the reconstruction is carrying")
	}
	if row := attemptRow(t, h, item.ID, "work", 3); row.FeedbackDeliveredAt == 0 {
		t.Fatal("the reconstruction's own feedback was never settled by its start")
	}
}

// failingPhaseCreate is the engine's persistence with ONE named attempt's
// `CreateWorkItemPhase` armed to fail. One rather than all: the park that
// follows the failure creates an attempt row of its own, and a store that
// refused every create would leave the run with no record at all — a different
// failure from the one under test.
type failingPhaseCreate struct {
	persistence
	failAttempt atomic.Int64
}

func (f *failingPhaseCreate) CreateWorkItemPhase(phase store.WorkItemPhase) error {
	if want := f.failAttempt.Load(); want != 0 && int64(phase.Attempt) == want {
		f.failAttempt.Store(0)
		return errors.New("attempt row would not persist")
	}
	return f.persistence.CreateWorkItemPhase(phase)
}

// THE C1 CRASH WINDOW. A provider-context restart carries the superseded
// attempt's note into the reconstruction in memory. If the source were settled
// before the reconstruction's row existed, anything between the two — a crash, a
// store that refuses the create — would leave the note recorded as delivered and
// the only surviving copy in a process that is gone.
func TestARestartThatCannotPersistItsReconstructionLeavesTheNoteOwed(t *testing.T) {
	failing := &failingPhaseCreate{}
	h := newHarnessWith(t, harnessOptions{
		workflows:  map[string]def.Workflow{"feedback": feedbackWorkflow()},
		projectIDs: []string{"project"},
		wrapStore: func(handle persistence) persistence {
			failing.persistence = handle
			return failing
		},
	})
	item := startAskingRun(t, h, "item", "thread-one")

	h.runner.startErrs["item/work/2"] = errors.Join(ErrProviderContextUnavailable, errors.New("provider thread missing"))
	// Attempt 2 is the answered continuation; 3 is the reconstruction the restart
	// carries the answer into, and it is that create that must not land.
	failing.failAttempt.Store(3)
	// The failed create reaches the verb: a reconstruction that could not be
	// persisted is a failure of the answer, not a silent one.
	if err := h.engine.Answer(item.ID, answerText); err == nil {
		t.Fatal("Answer reported success though its reconstruction's row never persisted")
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	// The reconstruction never landed, so the note is still owed by the attempt
	// that actually holds it — the redelivery direction, not the loss.
	superseded := attemptRow(t, h, item.ID, "work", 2)
	if superseded.FeedbackDeliveredAt != 0 {
		t.Fatalf("the superseded attempt was settled by a reconstruction that never persisted (stamp %d); the answer is now unrecoverable",
			superseded.FeedbackDeliveredAt)
	}
	if note := attemptFeedbackNote(t, superseded); note != answerText {
		t.Fatalf("superseded attempt feedback = %q, want the answer still on the row", note)
	}

	// And the recovery reaches it: the ordinary redelivery read finds the source
	// still owing and hands the answer to the entry that finally runs.
	delete(h.runner.startErrs, "item/work/2")
	if err := h.engine.Resume(item.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	recovered := h.runner.lastStartFor(t, item.ID)
	if recovered.Feedback == nil || !strings.Contains(recovered.Feedback.Note, answerText) {
		t.Fatalf("the recovered attempt carried %+v, want the answer the restart could not deliver", recovered.Feedback)
	}
}

// THE C2 DROP. A runner start returns nil for an opening send that never
// reached a model (a latched session death, a stale epoch). Nothing acks, so the
// note stays owed and the phase's next entry delivers it — which is the whole
// point of moving the stamp off the start's success result.
func TestAStartWhoseSendWasDroppedLeavesTheFeedbackOwed(t *testing.T) {
	h := newFeedbackHarness(t)
	item := startAskingRun(t, h, "item", "thread-one")

	h.runner.dropSend["item/work/2"] = true
	if err := h.engine.Answer(item.ID, answerText); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if row := attemptRow(t, h, item.ID, "work", 2); row.FeedbackDeliveredAt != 0 {
		t.Fatalf("attempt 2 was stamped delivered (%d) even though its send was dropped", row.FeedbackDeliveredAt)
	}

	// The attempt is live and the run eventually parks on it again; the entry
	// after it is what redelivers the answer no turn ever read.
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Resume(item.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	recovered := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 3})
	if recovered.Feedback == nil || !strings.Contains(recovered.Feedback.Note, answerText) {
		t.Fatalf("attempt 3 carried %+v, want the answer the dropped send never rendered", recovered.Feedback)
	}
}

// The ack is idempotent and recognises nothing else. A ladder resend acks the
// same attempt again, and a key for an attempt the run has moved past must never
// settle the one it is on.
func TestAckFeedbackRenderedIsIdempotentAndIgnoresStaleKeys(t *testing.T) {
	h := newFeedbackHarness(t)
	item := startAskingRun(t, h, "item", "thread-one")

	h.runner.dropSend["item/work/2"] = true
	if err := h.engine.Answer(item.ID, answerText); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	for name, key := range map[string]RunKey{
		"another run":    {ItemID: "nobody", PhaseID: "work", Attempt: 2},
		"another phase":  {ItemID: item.ID, PhaseID: "elsewhere", Attempt: 2},
		"a past attempt": {ItemID: item.ID, PhaseID: "work", Attempt: 1},
		// A unit key against a single-shape phase: no fan exists, so no unit
		// send can prove anything about this attempt's blocks.
		"a unit of a fanless phase": {ItemID: item.ID, PhaseID: "work", Attempt: 2, UnitID: "work-unit-0"},
	} {
		if err := h.engine.AckFeedbackRendered(key); err != nil {
			t.Fatalf("ack for %s: %v", name, err)
		}
		if row := attemptRow(t, h, item.ID, "work", 2); row.FeedbackDeliveredAt != 0 {
			t.Fatalf("an ack for %s settled the live attempt's feedback", name)
		}
	}

	live := RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 2}
	if err := h.engine.AckFeedbackRendered(live); err != nil {
		t.Fatal(err)
	}
	settled := attemptRow(t, h, item.ID, "work", 2).FeedbackDeliveredAt
	if settled == 0 {
		t.Fatal("the live attempt's own send did not settle its feedback")
	}
	if err := h.engine.AckFeedbackRendered(live); err != nil {
		t.Fatal(err)
	}
	if again := attemptRow(t, h, item.ID, "work", 2).FeedbackDeliveredAt; again != settled {
		t.Fatalf("a second ack re-stamped the row (%d -> %d); a ladder resend must write nothing", settled, again)
	}
}

// corruptOwedFeedback answers the redelivery read with a row whose input
// envelope will not decode. The column is CHECK-constrained JSON this engine
// wrote, so content that will not decode is corruption — and reading it as
// "nothing owed" would drop an operator's answer and report success.
type corruptOwedFeedback struct {
	persistence
	fail atomic.Bool
}

func (c *corruptOwedFeedback) ListUndeliveredWorkItemPhaseFeedback(itemID, phaseID string, belowAttempt int) ([]store.WorkItemPhaseFeedback, error) {
	if c.fail.Load() {
		return []store.WorkItemPhaseFeedback{{
			Attempt: 1, InputEnvelope: json.RawMessage(`{"feedback":7}`),
		}}, nil
	}
	return c.persistence.ListUndeliveredWorkItemPhaseFeedback(itemID, phaseID, belowAttempt)
}

func TestAnUndecodableOwedEnvelopeParksTheEntryRatherThanDroppingTheNote(t *testing.T) {
	corrupt := &corruptOwedFeedback{}
	h := newHarnessWith(t, harnessOptions{
		workflows:  map[string]def.Workflow{"feedback": feedbackWorkflow()},
		projectIDs: []string{"project"},
		wrapStore: func(handle persistence) persistence {
			corrupt.persistence = handle
			return corrupt
		},
	})
	item := startAskingRun(t, h, "item", "thread-one")

	corrupt.fail.Store(true)
	if err := h.engine.Answer(item.ID, answerText); err == nil {
		t.Fatal("Answer reported success over an owed-feedback row nothing could decode")
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonWiringError)
	requireParkCause(t, attemptRow(t, h, item.ID, "work", 2), "input for feedback redelivery")
}

// failingFeedbackMark refuses the stamp that settles a delivered note.
type failingFeedbackMark struct {
	persistence
	fail atomic.Bool
}

func (f *failingFeedbackMark) MarkWorkItemPhaseFeedbackDelivered(itemID, phaseID string, attempt int, deliveredAt int64) error {
	if f.fail.Load() {
		return errors.New("stamp would not write")
	}
	return f.persistence.MarkWorkItemPhaseFeedbackDelivered(itemID, phaseID, attempt, deliveredAt)
}

// A stamp that does not land leaves the note OWED — a redelivery on the phase's
// next entry, which is the safe direction — and says so loudly. Nothing else
// would tell an operator the run is about to hear itself twice.
func TestAFailedFeedbackStampIsReportedAndLeavesTheNoteOwed(t *testing.T) {
	failing := &failingFeedbackMark{}
	sink := &recordingLog{}
	h := newHarnessWith(t, harnessOptions{
		config:     Config{Log: sink},
		workflows:  map[string]def.Workflow{"feedback": feedbackWorkflow()},
		projectIDs: []string{"project"},
		wrapStore: func(handle persistence) persistence {
			failing.persistence = handle
			return failing
		},
	})
	item := startAskingRun(t, h, "item", "thread-one")

	failing.fail.Store(true)
	if err := h.engine.Answer(item.ID, answerText); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	failing.fail.Store(false)

	if row := attemptRow(t, h, item.ID, "work", 2); row.FeedbackDeliveredAt != 0 {
		t.Fatalf("attempt 2 reads as settled (%d) though the stamp failed", row.FeedbackDeliveredAt)
	}
	if events := h.emitter.errorEvents(item.ID); len(events) == 0 {
		t.Fatal("a stamp that did not land was swallowed; nothing reached workflow:error")
	}
	lines := sink.matchingItem(LogEventFeedbackRedeliver, item.ID)
	if len(lines) == 0 {
		t.Fatal("a stamp that did not land wrote no engine-log line")
	}
	if !strings.Contains(lines[len(lines)-1].Message, "deliver it again") {
		t.Fatalf("log line %q does not say a later attempt will deliver the note again", lines[len(lines)-1].Message)
	}
}

// A phase nothing renders feedback in is born settled rather than accumulating
// a debt no entry could ever discharge — the same rule `deliversGuidance` uses
// to refuse clearing the pending slot at a boundary nobody can read it in.
func TestPhasesThatRenderNoFeedbackAreBornSettled(t *testing.T) {
	toolPhase := agentPhase("work", nil, []def.Route{{To: "done"}})
	toolPhase.Driver = def.DriverTool
	toolPhase.Check = "lint"
	toolPhase.Provider, toolPhase.Model = "", ""

	// A fan-out whose every element is a command renders no prompt anywhere, so
	// nothing in it can read a phase-level note.
	commandFanOut := staticFanOutPhase("work", 1, nil, []def.Route{{To: "done"}})
	for index := range commandFanOut.FanOut {
		commandFanOut.FanOut[index].Provider, commandFanOut.FanOut[index].Model = "", ""
		commandFanOut.FanOut[index].Prompt = ""
		commandFanOut.FanOut[index].Command = "true"
	}
	commandFanOut.Join.Provider, commandFanOut.Join.Model = "", ""
	commandFanOut.Join.Prompt = ""
	commandFanOut.Join.Command = "true"

	for name, phase := range map[string]def.Phase{
		"tool":            toolPhase,
		"command fan-out": commandFanOut,
	} {
		t.Run(name, func(t *testing.T) {
			if deliversFeedback(phase) {
				t.Fatalf("%s phase claims to render phase-level feedback", name)
			}
			if stamp := phaseFeedbackCreateStamp(phase, &Feedback{Note: "steer"}, 42); stamp != 42 {
				t.Fatalf("%s phase create stamp = %d, want the attempt's start time", name, stamp)
			}
			if phaseOwesFeedback(phase, &Feedback{Note: "steer"}) {
				t.Fatalf("%s phase owes feedback no element of it will read", name)
			}
		})
	}

	agent := agentPhase("work", nil, []def.Route{{To: "done"}})
	if !deliversFeedback(agent) {
		t.Fatal("a single-shape agent phase does not render feedback")
	}
	// An agent fan-out owes: every agent element renders the phase note through
	// unitRequestFeedback, so a note here has readers — before the composition, a
	// gate reject looping into a fan-out phase was recorded and rendered to nobody.
	agentFanOut := staticFanOutPhase("work", 1, nil, []def.Route{{To: "done"}})
	if !deliversFeedback(agentFanOut) {
		t.Fatal("an agent fan-out phase does not render phase-level feedback")
	}
	if stamp := phaseFeedbackCreateStamp(agentFanOut, &Feedback{Note: "steer"}, 42); stamp != 0 {
		t.Fatalf("agent fan-out create stamp = %d, want 0 (owed)", stamp)
	}
	if stamp := phaseFeedbackCreateStamp(agent, &Feedback{Note: "steer"}, 42); stamp != 0 {
		t.Fatalf("agent phase create stamp = %d, want 0 (owed)", stamp)
	}
	if phaseOwesFeedback(agent, nil) || phaseOwesFeedback(agent, &Feedback{Note: "  "}) {
		t.Fatal("an attempt with no note owes nothing, so its start must not pay a write to settle it")
	}
	if !phaseOwesFeedback(agent, &Feedback{Note: "steer"}) {
		t.Fatal("an agent attempt carrying a note does not owe it")
	}
	// The two halves are ONE predicate. A row born at 0 that no flag tracks is a
	// debt nothing can settle: `dischargeRenderedFeedback` only ever settles an
	// attempt whose flag is set, so the row would be redelivered from forever.
	for name, feedback := range map[string]*Feedback{
		"no feedback at all": nil,
		"values, no note":    {Values: map[string]any{"review.ok": false}},
		"blank note":         {Note: "  \n "},
	} {
		if stamp := phaseFeedbackCreateStamp(agent, feedback, 42); stamp != 42 {
			t.Fatalf("an agent attempt with %s was born owing (stamp %d); nothing would ever settle it",
				name, stamp)
		}
	}
}

// The B4 incident in the engine rather than in the predicate: an agent phase
// entered N times with no feedback at all must leave the undelivered window
// EMPTY. Every entry lists and JSON-decodes that window, so a phase whose rows
// are all born owing costs one decode per prior attempt per lap and finds
// nothing every time.
func TestFeedbacklessAttemptsAreBornSettledAndLeaveTheWindowEmpty(t *testing.T) {
	h := newFeedbackHarness(t)
	item := testItem("item", "project", "feedback", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}

	const laps = 4
	for lap := 1; lap <= laps; lap++ {
		row := attemptRow(t, h, item.ID, "work", lap)
		if row.FeedbackDeliveredAt == 0 {
			t.Fatalf("attempt %d was born owing feedback it never carried", lap)
		}
		owed, err := h.store.ListUndeliveredWorkItemPhaseFeedback(item.ID, "work", lap+1)
		if err != nil {
			t.Fatal(err)
		}
		if len(owed) != 0 {
			t.Fatalf("entry %d saw %d undelivered prior attempts, want none", lap+1, len(owed))
		}
		if lap == laps {
			break
		}
		// Re-enter the same phase, which is what a loop lap costs.
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Resume(item.ID, "work", false); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
}

// Which phases render the feedback and guidance blocks at all is ONE answer,
// on purpose: the two blocks travel together in prompt assembly, so a phase
// that can read one can read the other (`deliversFeedback` delegates to
// `deliversGuidance`). This pins the answer per shape × driver — against BOTH
// predicates, not against each other — so a future narrowing of either fails
// here and has to consciously fork the shared fact rather than drift it. The
// stakes are asymmetric and both real: a phase that owed feedback while
// rendering no prompt would owe a debt nothing could ever discharge, and a
// prompt-rendering phase born settled would drop an operator's note silently.
func TestFeedbackAndGuidanceDeliveryShareOneAnswerAcrossTheShapeDriverMatrix(t *testing.T) {
	routes := []def.Route{{To: "done"}}
	toolPhase := func(id string) def.Phase {
		phase := agentPhase(id, nil, routes)
		phase.Driver, phase.Check, phase.Provider, phase.Model = def.DriverTool, "lint", "", ""
		return phase
	}
	commandUnit := func(unit *def.Unit, command string) {
		unit.Provider, unit.Model = "", ""
		unit.Prompt, unit.Command = "", command
	}
	commandFanOut := func(id string) def.Phase {
		phase := staticFanOutPhase(id, 1, nil, routes)
		for index := range phase.FanOut {
			commandUnit(&phase.FanOut[index], "make check")
		}
		commandUnit(phase.Join, "make report")
		return phase
	}
	commandDynamicFanOut := func(id string) def.Phase {
		phase := dynamicFanOutPhase(id, "items", "item", routes)
		commandUnit(phase.Unit, "make check")
		commandUnit(phase.Join, "make report")
		return phase
	}

	for name, testCase := range map[string]struct {
		phase   def.Phase
		renders bool
	}{
		"single/agent":            {agentPhase("work", nil, routes), true},
		"single/tool":             {toolPhase("work"), false},
		"fan-out/agent":           {staticFanOutPhase("work", 2, nil, routes), true},
		"fan-out/command":         {commandFanOut("work"), false},
		"fan-out/dynamic agent":   {dynamicFanOutPhase("work", "items", "item", routes), true},
		"fan-out/dynamic command": {commandDynamicFanOut("work"), false},
		"call":                    {callPhaseDef("work", "child", nil, 0, routes), false},
		// Width is a runtime fact and zero is legal: the join still runs, and
		// an agent join is a prompt somebody reads the blocks in.
		"fan-out/no units": {staticFanOutPhase("work", 0, nil, routes), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := deliversGuidance(testCase.phase); got != testCase.renders {
				t.Fatalf("deliversGuidance = %v, want %v", got, testCase.renders)
			}
			if got := deliversFeedback(testCase.phase); got != testCase.renders {
				t.Fatalf("deliversFeedback = %v, want %v", got, testCase.renders)
			}
		})
	}
}

// The block is bounded, and a bound that cut says so: a reader who cannot tell
// a short instruction from a cut-off one will trust the wrong half.
func TestRedeliveredFeedbackIsBoundedAndStatesTheTruncation(t *testing.T) {
	if note := redeliveredFeedbackNote(nil); note != "" {
		t.Fatalf("an empty window rendered %q, want nothing", note)
	}

	within := redeliveredFeedbackNote([]owedFeedback{{attempt: 2, note: "short"}})
	if !strings.Contains(within, "attempt 2") || !strings.HasSuffix(within, "short") {
		t.Fatalf("in-bound block = %q, want the provenance and the note whole", within)
	}
	if strings.Contains(within, redeliveredFeedbackTruncated) {
		t.Fatalf("an in-bound block claimed truncation: %q", within)
	}

	oversized := redeliveredFeedbackNote([]owedFeedback{
		{attempt: 2, note: strings.Repeat("x", MaxRedeliveredFeedbackBytes+1)},
	})
	if len(oversized) > MaxRedeliveredFeedbackBytes+len(redeliveredFeedbackTruncated) {
		t.Fatalf("block is %d bytes, want at most %d plus the truncation marker",
			len(oversized), MaxRedeliveredFeedbackBytes)
	}
	if !strings.HasSuffix(oversized, redeliveredFeedbackTruncated) {
		t.Fatalf("a truncated block does not say so: %q", oversized[len(oversized)-80:])
	}
	// The provenance survives the cut: it leads the block, so an element still
	// knows the instruction is older than the attempt it arrived in.
	if !strings.HasPrefix(oversized, "undelivered feedback from attempt 2") {
		t.Fatalf("truncation ate the provenance: %q", oversized[:80])
	}

	// The cut lands on a rune boundary — the note is a person's own words, and
	// half a rune would be invalid UTF-8 in the attempt's persisted input.
	multibyte := redeliveredFeedbackNote([]owedFeedback{
		{attempt: 2, note: strings.Repeat("é", MaxRedeliveredFeedbackBytes)},
	})
	body := strings.TrimSuffix(multibyte, redeliveredFeedbackTruncated)
	if !utf8.ValidString(body) {
		t.Fatalf("truncation split a rune: %q", body[len(body)-8:])
	}
}

// The redelivery is prepended, never appended: it belongs to an EARLIER round
// than whatever the entry itself is saying, and the note reads as a chronology.
func TestRedeliveredFeedbackLeadsTheEntrysOwnNote(t *testing.T) {
	feedback := prependFeedbackNote(&Feedback{Note: "continue from where the previous turn stopped"}, "older")
	if feedback.Note != "older\ncontinue from where the previous turn stopped" {
		t.Fatalf("composed note = %q, want the redelivery first", feedback.Note)
	}
	if fresh := prependFeedbackNote(nil, "older"); fresh == nil || fresh.Note != "older" {
		t.Fatalf("prepend onto no feedback = %+v, want an allocated note", fresh)
	}
	if empty := prependFeedbackNote(&Feedback{Note: "kept"}, ""); empty.Note != "kept" {
		t.Fatalf("prepending nothing changed the note: %q", empty.Note)
	}
}

// A gate reject looping into a FAN-OUT phase used to record the operator's
// reasoning on the attempt row and render it to nobody: a unit's
// `RunRequest.Feedback` was only its own `unit.feedback`. Every agent element
// now renders the phase note (`unitRequestFeedback`), and the first element
// send is what settles the debt — through the same unit-key ack this test
// exercises end to end, since the harness runner acks from inside each start.
func TestFanOutElementsRenderThePhaseFeedbackNote(t *testing.T) {
	workflow := def.Workflow{ID: "campaign", Phases: []def.Phase{
		staticFanOutPhase("build", 2, nil, []def.Route{{To: "review"}}),
		agentPhase("review", nil, []def.Route{{Human: &def.HumanRoute{
			Approve: "done",
			Reject:  &def.LoopTarget{Loop: "build", Max: def.LiteralBound(2)},
		}}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"campaign": workflow}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("item", "project", "campaign", 0)); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"build-unit-0", "build-unit-1"} {
		h.runner.completeRun(t, unitKey("item", "build", 1, unit), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey("item", "build", 1, "build-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "item", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonGate)

	const rejectNote = "make each lane smaller"
	if err := h.engine.ResolveHumanGate("item", HumanReject, rejectNote); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	// Every work unit of the re-entered wave renders the reject's reasoning.
	rendered := 0
	for _, start := range h.runner.started() {
		if start.Key.Attempt != 2 || start.Key.UnitID == "" {
			continue
		}
		rendered++
		if start.Feedback == nil || !strings.Contains(start.Feedback.Note, rejectNote) {
			t.Fatalf("unit %s feedback = %+v, want the reject note", start.Key.UnitID, start.Feedback)
		}
	}
	if rendered != 2 {
		t.Fatalf("attempt 2 unit starts carrying feedback = %d, want both work units", rendered)
	}
	// The unit-key ack settled the phase-level debt: the attempt was born owing
	// (`phaseFeedbackCreateStamp` answers 0 for an agent fan-out with a note) and
	// the first element send discharged it.
	if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt == 0 {
		t.Fatal("a dispatched element send did not settle the fan-out attempt's feedback")
	}

	// The join renders it too: an instruction about the wave steers its
	// consolidation as much as its lanes.
	for _, unit := range []string{"build-unit-0", "build-unit-1"} {
		h.runner.completeRun(t, unitKey("item", "build", 2, unit), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	join := h.runner.startFor(t, unitKey("item", "build", 2, "build-join"))
	if join.Feedback == nil || !strings.Contains(join.Feedback.Note, rejectNote) {
		t.Fatalf("join feedback = %+v, want the reject note", join.Feedback)
	}
}

// The drop case one level down: a fan-out wave whose every element send was
// dropped leaves the phase note OWED, and the phase's next fresh entry
// redelivers it into the new wave with provenance.
func TestAFanOutWaveWhoseSendsWereDroppedLeavesThePhaseFeedbackOwed(t *testing.T) {
	workflow := def.Workflow{ID: "campaign", Phases: []def.Phase{
		staticFanOutPhase("build", 1, nil, []def.Route{{To: "review"}}),
		agentPhase("review", nil, []def.Route{{Human: &def.HumanRoute{
			Approve: "done",
			Reject:  &def.LoopTarget{Loop: "build", Max: def.LiteralBound(2)},
		}}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"campaign": workflow}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("item", "project", "campaign", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey("item", "build", 1, "build-unit-0"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey("item", "build", 1, "build-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "item", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonGate)

	const rejectNote = "make the lane smaller"
	h.runner.dropSend["item/build/2/build-unit-0"] = true
	if err := h.engine.ResolveHumanGate("item", HumanReject, rejectNote); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt != 0 {
		t.Fatalf("attempt 2 was stamped delivered (%d) even though every element send was dropped", row.FeedbackDeliveredAt)
	}

	// A unit key naming a unit this fan never expanded must not settle the
	// still-owed debt. Item, phase, and attempt all name the live attempt, so
	// the fan-membership check is the only guard standing.
	if err := h.engine.AckFeedbackRendered(unitKey("item", "build", 2, "imposter")); err != nil {
		t.Fatal(err)
	}
	if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt != 0 {
		t.Fatal("an ack naming a unit outside the fan settled the attempt's feedback")
	}

	// The wave dies without a rendered note; the fresh entry that repairs the
	// phase must say it again, saying where it came from.
	h.runner.completeRun(t, unitKey("item", "build", 2, "build-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonUnitFailed)
	if err := h.engine.Resume("item", "build", false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	recovered := h.runner.startFor(t, unitKey("item", "build", 3, "build-unit-0"))
	if recovered.Feedback == nil {
		t.Fatal("the re-entered wave carried no feedback at all")
	}
	for _, want := range []string{rejectNote, "undelivered feedback from attempt 2"} {
		if !strings.Contains(recovered.Feedback.Note, want) {
			t.Fatalf("re-entered wave feedback %q does not state %q", recovered.Feedback.Note, want)
		}
	}
}

// rejectedFanOutWorkflow is the shape every repair-path feedback test runs: a
// one-unit wave whose human gate can reject back into it twice, so attempt 2
// exists to carry the reject note and attempt 3 exists to prove what attempt 2
// still owed.
func rejectedFanOutWorkflow() def.Workflow {
	return def.Workflow{ID: "campaign", Phases: []def.Phase{
		staticFanOutPhase("build", 1, nil, []def.Route{{To: "review"}}),
		agentPhase("review", nil, []def.Route{{Human: &def.HumanRoute{
			Approve: "done",
			Reject:  &def.LoopTarget{Loop: "build", Max: def.LiteralBound(2)},
		}}}),
	}}
}

// runRejectedFanOutLap drives one full clean lap — wave, join, review — and
// leaves the run resting at the human gate.
func runRejectedFanOutLap(t *testing.T, h *testHarness, attempt int) {
	t.Helper()
	h.runner.completeRun(t, unitKey("item", "build", attempt, "build-unit-0"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey("item", "build", attempt, "build-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "item", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonGate)
}

// The repair-in-place transition. A bare resume of a `unit-failed` park
// relaunches the failed unit WITHOUT a fresh phase entry, so nothing runs
// `redeliverFeedback` for it — the parked attempt's own still-owed note is the
// only copy in play. `loadParked` restoring `feedbackOwed` from the row's stamp
// is what makes the relaunched element render the note and its send settle the
// row; before the restore, the flag was reborn false, the note was suppressed
// as if already rendered, and the debt survived the repair to be redelivered
// into a round that never needed it.
func TestABareResumeOfAFailedWaveRendersAndSettlesTheOwedNote(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"campaign": rejectedFanOutWorkflow()}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("item", "project", "campaign", 0)); err != nil {
		t.Fatal(err)
	}
	runRejectedFanOutLap(t, h, 1)

	const rejectNote = "make the lane smaller"
	h.runner.dropSend["item/build/2/build-unit-0"] = true
	if err := h.engine.ResolveHumanGate("item", HumanReject, rejectNote); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey("item", "build", 2, "build-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonUnitFailed)
	if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt != 0 {
		t.Fatalf("attempt 2 was stamped delivered (%d) even though its one element send was dropped", row.FeedbackDeliveredAt)
	}

	// The repaired try's send goes through this time.
	delete(h.runner.dropSend, "item/build/2/build-unit-0")
	if err := h.engine.Resume("item", "", false); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	repaired := h.runner.startFor(t, unitKey("item", "build", 2, "build-unit-0"))
	if repaired.UnitAttempt != 2 {
		t.Fatalf("repaired unit try = %d, want the reopened unit, not a re-expansion", repaired.UnitAttempt)
	}
	if repaired.Feedback == nil || !strings.Contains(repaired.Feedback.Note, rejectNote) {
		t.Fatalf("repaired unit feedback = %+v, want the still-owed phase note rendered", repaired.Feedback)
	}
	if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt == 0 {
		t.Fatal("the repaired element's send did not settle the attempt's feedback")
	}

	// The debt is settled, so the phase's NEXT fresh entry redelivers nothing:
	// the second reject's own note arrives alone.
	h.runner.completeRun(t, unitKey("item", "build", 2, "build-unit-0"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey("item", "build", 2, "build-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "item", Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonGate)
	const secondNote = "now split the lane differently"
	if err := h.engine.ResolveHumanGate("item", HumanReject, secondNote); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	fresh := h.runner.startFor(t, unitKey("item", "build", 3, "build-unit-0"))
	if fresh.Feedback == nil || !strings.Contains(fresh.Feedback.Note, secondNote) {
		t.Fatalf("attempt 3 feedback = %+v, want the second reject's note", fresh.Feedback)
	}
	if strings.Contains(fresh.Feedback.Note, "undelivered feedback from attempt 2") {
		t.Fatalf("attempt 3 redelivered a note attempt 2's repair already rendered: %q", fresh.Feedback.Note)
	}
}

// A join continuation is not a boundary, one level down: the answered join has
// already read the phase note in its first try, so its continuation carries the
// answer alone — UNLESS the note is still owed, where suppressing it would let
// the continuation's ack discharge a note nothing ever rendered.
func TestAJoinContinuationCarriesOnlyTheAnswerUnlessTheNoteIsStillOwed(t *testing.T) {
	const rejectNote = "make the lane smaller"
	const joinAnswer = "keep the first result"

	// Drives the shared shape to a parked join question on attempt 2, with the
	// reject note either rendered (sends flow) or owed (every send dropped).
	parkOnJoinQuestion := func(t *testing.T, dropSends bool) *testHarness {
		t.Helper()
		h := newHarness(t, Config{}, map[string]def.Workflow{"campaign": rejectedFanOutWorkflow()}, []string{"project"}, nil)
		if err := h.engine.StartItem(testItem("item", "project", "campaign", 0)); err != nil {
			t.Fatal(err)
		}
		runRejectedFanOutLap(t, h, 1)
		if dropSends {
			h.runner.dropSend["item/build/2/build-unit-0"] = true
			h.runner.dropSend["item/build/2/build-join"] = true
		}
		if err := h.engine.ResolveHumanGate("item", HumanReject, rejectNote); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		h.runner.completeRun(t, unitKey("item", "build", 2, "build-unit-0"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		// The app runner stamps the join's thread onto the phase attempt row the
		// moment it exists; Answer refuses a fan-out park without it.
		seedThread(t, h.store, "join-thread")
		if err := h.store.AttachWorkItemPhaseRun("item", "build", 2, "join-thread", ""); err != nil {
			t.Fatal(err)
		}
		h.runner.completeRun(t, unitKey("item", "build", 2, "build-join"),
			Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		requireItemState(t, h.store, "item", StateNeedsHuman, ReasonQuestion)
		return h
	}

	t.Run("a rendered note is not re-prepended", func(t *testing.T) {
		h := parkOnJoinQuestion(t, false)
		if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt == 0 {
			t.Fatal("the wave's element sends did not settle the attempt's feedback")
		}
		if err := h.engine.Answer("item", joinAnswer); err != nil {
			t.Fatal(err)
		}
		continued := h.runner.startFor(t, unitKey("item", "build", 2, "build-join"))
		if continued.UnitAttempt != 2 {
			t.Fatalf("join try = %d, want the continuation, not the first run", continued.UnitAttempt)
		}
		if continued.Feedback == nil || !strings.Contains(continued.Feedback.Note, joinAnswer) {
			t.Fatalf("join continuation feedback = %+v, want the human's answer", continued.Feedback)
		}
		if strings.Contains(continued.Feedback.Note, rejectNote) {
			t.Fatalf("the continuation re-prepended a phase note the join already read: %q", continued.Feedback.Note)
		}
	})

	t.Run("an owed note rides the continuation and its send settles it", func(t *testing.T) {
		h := parkOnJoinQuestion(t, true)
		if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt != 0 {
			t.Fatalf("attempt 2 was stamped delivered (%d) even though every element send was dropped", row.FeedbackDeliveredAt)
		}
		delete(h.runner.dropSend, "item/build/2/build-join")
		if err := h.engine.Answer("item", joinAnswer); err != nil {
			t.Fatal(err)
		}
		continued := h.runner.startFor(t, unitKey("item", "build", 2, "build-join"))
		if continued.Feedback == nil {
			t.Fatal("the join continuation carried no feedback at all")
		}
		for _, want := range []string{joinAnswer, rejectNote} {
			if !strings.Contains(continued.Feedback.Note, want) {
				t.Fatalf("join continuation feedback %q does not state %q", continued.Feedback.Note, want)
			}
		}
		if row := attemptRow(t, h, "item", "build", 2); row.FeedbackDeliveredAt == 0 {
			t.Fatal("the continuation's send did not settle the attempt's feedback")
		}
	})
}
