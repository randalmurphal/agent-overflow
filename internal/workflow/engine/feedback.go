package engine

import (
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/workflow/def"
)

// Feedback that no turn ever read, and the attempt that reads it instead.
//
// Feedback is the channel every human verb and every gate uses to say something
// to the next round: an answered question's answer, a reject's note, a gate's
// declared values, the engine's own "your definition was re-read" sentences. It
// is persisted on the attempt row's `input_envelope` the moment the attempt is
// created — and until v64 that was the ONLY thing that ever happened to it.
// Nothing re-read a superseded or parked attempt's feedback, so an operator who
// answered a question whose continuation then failed to start had their answer
// durably recorded and effectively destroyed: the recovery they reached for (a
// fresh phase entry) built an attempt that carried none of it, and `run guide`
// was the only channel left to say the same thing a second time by hand.
//
// This is `guidance.go`'s ordering rule applied to the other prompt block, and
// it is deliberately the same rule rather than a second one:
//
//   - The attempt row carrying the feedback is written FIRST. Its
//     `feedback_delivered_at` stays 0 — the note is still owed.
//   - The stamp lands when the note has actually been SENT to a provider session,
//     reported by the app runner's send door through `AckFeedbackRendered`. Not
//     when the row lands: everything between the two is a window in which a
//     pause, a failed acquisition, a wedged start, or a crash can end the attempt
//     with no turn, and a stamp written at the row would convert every one of
//     those into a silent loss. Not at the runner start's success either — a
//     start returns nil for a send that was dropped, which is the same loss with
//     a narrower window.
//   - Anything still owed when the NEXT attempt of that phase is created is
//     redelivered into it, with provenance saying where it came from.
//
// Between telling a round something twice and never telling it at all, twice is
// the answer. A redelivered note says which attempt left it and that nothing
// rendered it, so a second delivery reads as what it is.
//
// The one thing that differs from guidance is WHO owes: guidance is owed by the
// run (one pending slot on the run row), feedback is owed by the ATTEMPT that
// persisted it. That is why the stamp is a column on `work_item_phases` and not
// a slot to be cleared — an attempt is the smallest thing that has feedback of
// its own, and a phase's next attempt is the only place it can still be read.

// MaxRedeliveredFeedbackBytes bounds the redelivered block one attempt carries.
//
// It is twice `MaxHumanNoteBytes` so a maximal operator answer survives
// redelivery whole, with its provenance sentence and one further lap of nesting
// (see `redeliveredFeedbackNote`) still inside the bound. Past it the block is
// TRUNCATED with the fact stated rather than dropped, the same choice
// `MaxParkCauseBytes` makes: a shortened instruction still beats none, and a
// reader who cannot tell a short note from a cut-off one will trust the wrong
// half.
const MaxRedeliveredFeedbackBytes = 32 * 1024

// redeliveredFeedbackTruncated is appended to a block the bound cut.
const redeliveredFeedbackTruncated = " …(redelivered feedback truncated)"

// deliversFeedback reports whether a turn started by entering this phase renders
// the attempt's phase-level feedback. It IS `deliversGuidance`, deliberately:
// the two blocks travel together in prompt assembly — a single-shape agent
// phase sends `Feedback: cloneFeedback(item.feedback)` from `startRunner`, and
// every agent element of a fan-out renders the phase note through
// `unitRequestFeedback` exactly as it renders the guidance block — so a phase
// that can read one can read the other, and two predicates would be the
// drift-prone copy of one fact.
//
// It used to answer narrower: a fan-out's phase-level feedback reached no
// element (a unit's `RunRequest.Feedback` was only its own `unit.feedback`), so
// a gate reject looping back into a fan-out phase recorded the operator's
// reasoning on the attempt row and silently rendered it to nobody. The
// composition in `unitRequestFeedback` is what closed that; this predicate
// widening is its other half — a fan-out attempt carrying a note is now born
// OWING it (`phaseOwesFeedback`), settled by the first element send
// (`ackFeedbackRendered`), and redelivered on the next entry if nothing sent.
//
// What still never owes: a `driver: tool` phase (argv interpolation reads no
// feedback), a `shape: call` phase (no prompt at all), and a fan-out whose
// every element is a command. Those attempts are stamped delivered at creation
// instead of accumulating a debt nothing could ever settle — the same reason
// `deliversGuidance` refuses to clear the pending slot at a boundary nobody can
// read it in.
func deliversFeedback(phase def.Phase) bool {
	return deliversGuidance(phase)
}

// unitRequestFeedback composes what one fan-out element renders: the attempt's
// phase-level note (a gate reject's reasoning, a definition-refresh sentence, a
// redelivered answer) prepended to the unit's own note (a repair verb's
// instruction, a join continuation's answer), phase note first because it
// belongs to an earlier decision than the unit's round.
//
// Only the phase NOTE is carried, never `item.feedback.Values`: values are
// variable bindings the entry rebuilt from the live run record, and every
// element already receives them through its variable context (`unitVars` copies
// `item.fan.vars`) — carrying them here would hand the element two answers to
// one name. The unit's own feedback is cloned before composition, so the
// persisted unit row and the repair verbs keep seeing exactly the note they
// wrote.
//
// A CONTINUATION is not a boundary, one level down: the only element launched
// while `item.entry` is entryContinuation is the join being continued on the
// session that parked (`continueFanOutJoin` refuses while any work unit still
// blocks), and that session rendered the phase note in the try that parked —
// re-prepending it would hand the same instruction to a turn that has already
// read it, exactly what `promptGuidanceForEntry` already refuses for the other
// block. The one exception is a note still OWED: a debt the durable stamp says
// no session ever proved rendered is carried rather than suppressed, because
// the continuation's send will ack it — and an ack discharging a note this
// prompt did not carry would be the silent loss the ordering rule exists to
// prevent. A reconstructed join (entryRestart) always keeps the note: its
// fresh session has read nothing.
func unitRequestFeedback(item *runtimeItem, unit *unitRun) *Feedback {
	feedback := cloneFeedback(unit.feedback)
	if item.feedback == nil {
		return feedback
	}
	if item.entry == entryContinuation && !item.feedbackOwed {
		return feedback
	}
	note := strings.TrimSpace(item.feedback.Note)
	if note == "" {
		return feedback
	}
	return prependFeedbackNote(feedback, note)
}

// owedFeedback is one prior attempt's unrendered note, resolved from its
// persisted input envelope.
type owedFeedback struct {
	attempt int
	note    string
}

// collectOwedFeedback reads the prior attempts of this phase whose feedback no
// turn ever rendered, oldest first.
//
// THE WINDOW IS EVERY UNDELIVERED PRIOR ATTEMPT, and it is kept accurate by the
// marking rather than by arithmetic: the sources are stamped the moment the
// attempt that carries them is persisted (`dischargeCarriedFeedback`), so the
// window a later entry sees holds only what is genuinely still owed. In steady
// state that is zero rows or one. It can be more than one only when a marking
// write failed, and collecting all of them is then the right answer — the
// failure left the notes owed, and redelivering both is the redelivery direction
// this whole contract errs in.
//
// A row whose envelope will not decode is an ERROR rather than a skipped entry,
// the same rule `history.go` applies to the same column: it is CHECK-constrained
// JSON this engine wrote, so content that will not decode is corruption, and
// reading it as "nothing owed" would drop an operator's answer and report
// success.
//
// `exclude` names the one attempt a caller is already carrying VERBATIM (the
// provider-context restart, below). That row is still owed — it is settled only
// once the entry carrying it has a row of its own — so redelivering it here
// would prepend the note to itself.
func (e *Engine) collectOwedFeedback(itemID, phaseID string, belowAttempt, exclude int) ([]owedFeedback, error) {
	rows, err := e.store.ListUndeliveredWorkItemPhaseFeedback(itemID, phaseID, belowAttempt)
	if err != nil {
		return nil, err
	}
	owed := make([]owedFeedback, 0, len(rows))
	for _, row := range rows {
		if row.Attempt == exclude {
			continue
		}
		var input PhaseInput
		if err := decodeJSON(row.InputEnvelope, &input); err != nil {
			return nil, fmt.Errorf(
				"decode phase attempt %s/%s/%d input for feedback redelivery: %w",
				itemID, phaseID, row.Attempt, err)
		}
		if input.Feedback == nil {
			continue
		}
		note := strings.TrimSpace(input.Feedback.Note)
		if note == "" {
			// Only the NOTE is redeliverable. `Feedback.Values` are variable
			// bindings resolved for the round that attempt ran, and the entry being
			// composed has just rebuilt every one of them from the live run record —
			// carrying stale copies forward would hand the next round two answers to
			// the same name. An attempt that carried values and no note therefore
			// owes nothing, and is left alone rather than stamped: it is not a debt,
			// so there is nothing to settle.
			continue
		}
		owed = append(owed, owedFeedback{attempt: row.Attempt, note: note})
	}
	return owed, nil
}

// redeliveredFeedbackNote renders the block a redelivering attempt prepends.
//
// The provenance is not decoration: an element reading a note has to be able to
// tell "your gate said this about your last round" from "a person said this to a
// round that never ran", and the second one is the whole reason this block
// exists. It names the attempt and states that nothing rendered it, so an
// element can see that the instruction is older than the attempt it arrived in.
//
// Successive redeliveries NEST rather than flatten: an attempt that redelivered
// and then wedged itself is redelivered in turn, provenance and all. That is
// honest — the note really did pass through both attempts unread — and it is
// what makes the bound load-bearing rather than theoretical, since the alternative
// (a chain that grows one provenance sentence per wedged start forever) is
// exactly what `MaxRedeliveredFeedbackBytes` cuts.
func redeliveredFeedbackNote(owed []owedFeedback) string {
	if len(owed) == 0 {
		return ""
	}
	var block strings.Builder
	for index, entry := range owed {
		if index > 0 {
			block.WriteString("\n")
		}
		fmt.Fprintf(&block,
			"undelivered feedback from attempt %d (never rendered by a provider session), redelivered: %s",
			entry.attempt, entry.note)
	}
	return truncateRedeliveredFeedback(block.String())
}

// truncateRedeliveredFeedback bounds the block and says so when it cut. It is
// `parkCauseText`'s rule with this bound and this marker — one helper, because
// the two strings are bounded for identical reasons (`truncate.go`).
func truncateRedeliveredFeedback(note string) string {
	return truncateWithNote(note, MaxRedeliveredFeedbackBytes, redeliveredFeedbackTruncated)
}

// prependFeedbackNote puts one engine sentence BEFORE an attempt's own feedback,
// allocating the feedback when the entry composed none.
//
// It is `appendFeedbackNote`'s mirror and the order is the point: redelivered
// feedback belongs to an EARLIER round than whatever this entry is saying, and
// the note reads as a chronology. A resume's "continue from where the previous
// turn stopped" printed above the answer that turn never received would put the
// two in the wrong order.
func prependFeedbackNote(feedback *Feedback, note string) *Feedback {
	if note == "" {
		return feedback
	}
	if feedback == nil {
		return &Feedback{Note: note}
	}
	if feedback.Note == "" {
		feedback.Note = note
		return feedback
	}
	feedback.Note = note + "\n" + feedback.Note
	return feedback
}

// redeliverFeedback folds every note this phase still owes into the feedback of
// the attempt about to be created, and reports which attempts it took them from
// so the caller can settle them once that attempt's row exists.
//
// It runs for every entry kind. An answer's content is relevant however the
// phase is re-entered: a continuation, a fresh entry after a park, and a
// reconstruction of a round whose provider context died all produce a turn that
// should read what the last one never did. The one carry-forward that does NOT
// come through here is `restartPhaseWithoutProviderContext`, which moves the
// feedback itself into the reconstruction — `carriedFrom` names the source it
// moved, which this read skips so the note lands exactly once.
func (e *Engine) redeliverFeedback(item *runtimeItem, phase def.Phase, attempt, carriedFrom int) ([]owedFeedback, error) {
	if !deliversFeedback(phase) {
		// Nothing entering this phase will render feedback, so nothing here can
		// settle a debt either. The attempt is stamped delivered at creation
		// instead (`enterPhase`), which is what keeps an unreadable phase from
		// accumulating owed notes forever.
		return nil, nil
	}
	owed, err := e.collectOwedFeedback(item.item.ID, phase.ID, attempt, carriedFrom)
	if err != nil {
		return nil, err
	}
	if len(owed) == 0 {
		return nil, nil
	}
	item.feedback = prependFeedbackNote(item.feedback, redeliveredFeedbackNote(owed))
	return owed, nil
}

// dischargeCarriedFeedback settles the attempts a newly persisted attempt took
// feedback from. It runs AFTER that row exists, which is the same ordering
// `ackGuidance` keeps: a source stamped before its carrier landed would be a
// note nothing holds, and the create it depends on can still fail.
func (e *Engine) dischargeCarriedFeedback(item *runtimeItem, phaseID string, owed []owedFeedback, carrier int) {
	if len(owed) == 0 {
		return
	}
	labels := make([]string, len(owed))
	for index, entry := range owed {
		labels[index] = strconv.Itoa(entry.attempt)
	}
	e.settleOwedFeedback(item, phaseID, owed, carrier, fmt.Sprintf(
		"feedback no provider session rendered on attempt %s was redelivered into this attempt",
		strings.Join(labels, ", ")))
}

// dischargeRestartedFeedback settles the attempt a provider-context restart
// superseded. Its note is not redelivered — it is carried by the reconstruction
// directly, in `restartPhaseWithoutProviderContext` — so this settles the source
// so the redelivery window stays empty and the note lands exactly once.
//
// Like `dischargeCarriedFeedback` it runs AFTER the reconstruction's row exists.
// Settling before that create would leave a window in which a crash destroys the
// note outright: the source would read as delivered while the only surviving
// copy was still in memory. Settling after leaves a window in which the note is
// redelivered instead, which is the direction this whole contract errs in.
func (e *Engine) dischargeRestartedFeedback(item *runtimeItem, phaseID string, superseded int) {
	e.settleOwedFeedback(item, phaseID, []owedFeedback{{attempt: superseded}}, superseded,
		"this attempt's unrendered feedback is carried into the round reconstructed after its provider context became unavailable")
}

// settleOwedFeedback stamps each attempt's feedback as no longer owed and says
// so once.
//
// A failed stamp leaves the sources owed, which is a redelivery on the next
// entry rather than a loss — the safe direction — and it is reported rather than
// swallowed, exactly as `reportGuidanceAck` reports its own.
func (e *Engine) settleOwedFeedback(item *runtimeItem, phaseID string, owed []owedFeedback, logAttempt int, message string) {
	if len(owed) == 0 {
		return
	}
	now := e.timestamp()
	for _, entry := range owed {
		if err := e.store.MarkWorkItemPhaseFeedbackDelivered(item.item.ID, phaseID, entry.attempt, now); err != nil {
			e.reportFeedbackDischarge(item, phaseID, entry.attempt, err)
			return
		}
	}
	e.logEvent(LogEvent{
		Event: LogEventFeedbackRedeliver, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
		PhaseID: phaseID, Attempt: logAttempt, Message: message,
	})
}

// dischargeRenderedFeedback settles the CURRENT attempt's own feedback, because
// a turn that renders it has actually been dispatched. This is the ack half of
// the contract, and its trigger is `AckFeedbackRendered` — the app runner's
// SEND door, reporting a prompt it handed to a live provider session.
//
// It is deliberately not the runner start's success result any more. A start can
// return nil having DROPPED its opening send (a latched session death, a stale
// epoch), and a stamp written there recorded a note no turn ever rendered — the
// exact silent loss the ordering rule exists to prevent, one step further along.
// The send is the earliest event that proves the block reached a model.
//
// It is idempotent: the flag is the guard, so a ladder resend acking a second
// time writes nothing. The flag clears only on a stamp that LANDED — the same
// rule `ackGuidance` keeps for its half of this ack — so a refused write leaves
// the next send of this attempt retrying it, and only an attempt that ends
// without another send falls back to the next entry's redelivery.
func (e *Engine) dischargeRenderedFeedback(item *runtimeItem) {
	if !item.feedbackOwed {
		return
	}
	if err := e.store.MarkWorkItemPhaseFeedbackDelivered(
		item.item.ID, item.phaseID, item.attempt, e.timestamp(),
	); err != nil {
		e.reportFeedbackDischarge(item, item.phaseID, item.attempt, err)
		return
	}
	item.feedbackOwed = false
}

// AckFeedbackRendered reports that the prompt for one running piece of work has
// been handed to a live provider session. It is the app runner's send door
// calling — the opening send of an attempt, a unit's or the join's opening send,
// and every ladder resend into the same session alike — and it is what settles
// the attempt's owed feedback AND clears the guidance entries the attempt
// rendered. The two blocks share one delivery proof because they share one
// prompt: every agent element renders the phase entry's guidance and its
// phase-level feedback, so the first send of any of them is the earliest event
// that proves either block reached a model.
//
// It is idempotent and silent about everything it does not recognise: a key
// naming an attempt that is no longer the run's live one, a run that has since
// parked, an attempt owing nothing, and a unit key naming a unit outside the
// current attempt's fan all no-op.
//
// It never fails the send: a stamp the store refuses is reported through
// `reportFeedbackDischarge` and leaves the note owed, which is a redelivery on
// the phase's next entry rather than a loss; a guidance clear that fails leaves
// the entries pending the same way (`reportGuidanceAck`).
func (e *Engine) AckFeedbackRendered(key RunKey) error {
	return e.request(ackFeedbackCommand{key: key})
}

// ackFeedbackRendered is the command-loop half. The key must name the run's LIVE
// phase attempt: a send reported for a superseded or torn-down attempt proves
// nothing about the blocks the current one is carrying.
func (e *Engine) ackFeedbackRendered(key RunKey) {
	item, ok := e.items[key.ItemID]
	if !ok || item.phaseID != key.PhaseID || item.attempt != key.Attempt ||
		State(item.item.State) != StateRunning {
		return
	}
	if key.UnitID != "" {
		// A unit send proves delivery only if the unit is an element of the
		// CURRENT attempt's fan. The item/phase/attempt match above already rules
		// out a stale attempt; this refuses the two shapes that can still reach
		// here: a legitimately LATE ack — the send door dispatches the ack on its
		// own goroutine, so one can land after a teardown or a fresh entry cleared
		// the fan — and a key naming a unit this fan never expanded, which no
		// correct sender produces.
		if item.fan == nil || item.fan.find(key.UnitID) == nil {
			return
		}
	}
	e.dischargeRenderedFeedback(item)
	e.ackGuidance(item)
}

// reportFeedbackDischarge surfaces a stamp that did not land. The run is fine —
// it is already running the feedback — so this never parks it; what it must not
// do is stay quiet about a redelivery an operator will otherwise read as the
// engine repeating itself for no reason.
func (e *Engine) reportFeedbackDischarge(item *runtimeItem, phaseID string, attempt int, err error) {
	wrapped := fmt.Errorf(
		"settle delivered feedback for run %q attempt %s/%d: %w", item.item.ID, phaseID, attempt, err)
	e.emitError(item.item.ID, wrapped)
	e.logEvent(LogEvent{
		Event: LogEventFeedbackRedeliver, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
		PhaseID: phaseID, Attempt: attempt,
		Message: fmt.Sprintf(
			"this attempt's feedback was delivered but could not be marked so; "+
				"the attempt's next send retries the stamp, and failing that a later "+
				"attempt of this phase will deliver it again: %v", err),
	})
}

// phaseOwesFeedback reports whether the attempt about to be created carries a
// note a provider session still has to render. It is ONE predicate behind both
// halves of the debt — the durable `feedback_delivered_at` stamp
// (`phaseFeedbackCreateStamp`) and the in-memory `feedbackOwed` flag — because a
// row born owing what no flag tracks is a debt nothing can ever settle.
//
// It is narrower than `deliversFeedback` on purpose. A phase that renders
// feedback but was handed NONE owes nothing: `dischargeRenderedFeedback` only
// ever settles an attempt whose flag is set, so a note-less row born at 0 would
// stay owed forever — and every later entry of that phase would then list and
// JSON-decode every one of those rows, which is quadratic over a looping phase
// and finds nothing each time.
func phaseOwesFeedback(phase def.Phase, feedback *Feedback) bool {
	return deliversFeedback(phase) && feedback != nil && strings.TrimSpace(feedback.Note) != ""
}

// phaseFeedbackCreateStamp is the `feedback_delivered_at` a new attempt row is
// born with. An attempt that owes a note is born at 0 — settled by the send that
// proves a turn rendered it — and every other attempt is born settled, whether
// because its phase renders no feedback at all or because it carries none.
func phaseFeedbackCreateStamp(phase def.Phase, feedback *Feedback, startedAt int64) int64 {
	if phaseOwesFeedback(phase, feedback) {
		return 0
	}
	return startedAt
}
