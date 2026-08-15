package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/workflow/def"
)

// Steering a run without parking it.
//
// `notify:` carries a run's progress OUT to the thread watching it (D54). This
// is the same channel in the other direction: an operator who has read a wake,
// or an agent babysitting a campaign, leaves an instruction and the run picks it
// up at its next natural boundary. Before it existed the only way to steer a
// free-running run was to park it — pause, edit, resume — which costs the turn
// in flight and, for a campaign, a whole wave's worth of coordination.
//
// The boundary is a FRESH PHASE ENTRY and nothing else. There is deliberately no
// mid-turn injection: correcting a turn already in flight is an explicitly
// deferred non-feature (root CLAUDE.md), it maps to no provider wire event on
// either CLI, and a prompt that arrived halfway through a turn would reach the
// model as a second instruction with no contract saying which one wins. A run
// that is mid-turn simply keeps the guidance pending until its next entry.
//
// The slot is engine-owned in both directions. `Guide` decides who may leave an
// entry and stamps its author and time; `deliverGuidance` decides which entries
// a phase entry consumes and clears them. The author is never taken from the
// caller's text, because the entry is quoted into a prompt as data and "a human
// said this" is the one claim in it worth forging.

// Guide appends one operator instruction to a run's pending-guidance slot.
//
// It is a command like every other human action, so it cannot interleave with a
// phase entry: the slot is read and written in one turn of the loop, and an
// entry that lands a microsecond before a boundary is delivered by it rather
// than being written under it.
func (e *Engine) Guide(itemID string, draft GuidanceDraft) (GuidanceState, error) {
	result := &GuidanceState{}
	if err := e.request(guideCommand{itemID: itemID, draft: draft, result: result}); err != nil {
		return GuidanceState{}, err
	}
	return *result, nil
}

// guide is the command-loop half. Every refusal is decided before the write, so
// a rejected guide leaves the run record byte-identical — the same totality a
// refused amendment and a refused refresh give.
func (e *Engine) guide(itemID string, draft GuidanceDraft) (GuidanceState, error) {
	text := strings.TrimSpace(draft.Text)
	if text == "" {
		return GuidanceState{}, fmt.Errorf("guide run %q: guidance text cannot be empty", itemID)
	}
	if len(text) > MaxGuidanceEntryBytes {
		return GuidanceState{}, fmt.Errorf(
			"guide run %q: this guidance is %d bytes; one entry may be at most %d. Say the steer in a sentence — a specification belongs in the phase's prompt file, which `run resume --refresh-def` re-reads",
			itemID, len(text), MaxGuidanceEntryBytes)
	}
	if draft.By != GuidanceByHuman && draft.By != GuidanceByPhase {
		return GuidanceState{}, fmt.Errorf("guide run %q: guidance author must be %q or %q", itemID, GuidanceByHuman, GuidanceByPhase)
	}
	// The row is the authority on every refusal, and it is authoritative here
	// because this read and the transitions that change it are both the command
	// loop's. Unlike an amendment, a RUNNING run is a legitimate target: that is
	// the whole point — the run keeps working and hears about it at the boundary.
	row, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return GuidanceState{}, fmt.Errorf("guide run %q: %w", itemID, err)
	}
	switch State(row.State) {
	case StateRunning:
	case StateNeedsHuman:
		if Reason(row.Reason) == ReasonDisposition {
			return GuidanceState{}, fmt.Errorf(
				"guide run %q: this run is done and awaiting disposition, so no phase of it will be entered again; settle it with WorkflowMergeItem, WorkflowCreateItemPR, or WorkflowDiscardItem",
				itemID)
		}
	default:
		return GuidanceState{}, fmt.Errorf(
			"guide run %q: the run is %s, so there is no phase entry left to deliver guidance at", itemID, row.State)
	}

	pending, err := e.pendingGuidance(itemID)
	var quarantined *GuidanceQuarantine
	if err != nil {
		discarded, healed := healedGuidance(err)
		if !healed {
			return GuidanceState{}, err
		}
		// The slot was undecodable and has just been quarantined and cleared. The
		// entries it held are unrecoverable AS GUIDANCE either way — nothing can
		// render bytes nothing can decode — so refusing here would trade an entry
		// already lost for the caller's, which is the only one still recoverable
		// and the only one an operator is holding in their head right now. The
		// entry is kept and the discard is reported on the ANSWER: this call
		// succeeded, so an error channel would say the wrong thing, and the person
		// who needs the fact is the one reading this result.
		quarantined, pending = discarded, nil
	}
	if len(pending) >= MaxGuidanceEntries {
		return GuidanceState{}, fmt.Errorf(
			"guide run %q: %d entries are already waiting for this run's next phase entry, which is the maximum; the run has not reached a boundary since they were left, so adding another would bury them rather than steer anything",
			itemID, len(pending))
	}
	entry := GuidanceEntry{Text: text, At: e.timestamp(), By: draft.By, ByRun: draft.ByRun}
	pending = append(pending, entry)
	encoded, err := json.Marshal(pending)
	if err != nil {
		return GuidanceState{}, fmt.Errorf("guide run %q: encode guidance: %w", itemID, err)
	}
	// Read before the write, like every other refusal here: this read answers what
	// the caller is TOLD, and a failure after the append would report a refusal
	// over a run whose slot had already grown — an operator who retried would then
	// leave the same instruction twice.
	phaseID, err := e.guidedPhase(itemID)
	if err != nil {
		return GuidanceState{}, err
	}
	if err := e.store.SetWorkItemPendingGuidance(itemID, encoded); err != nil {
		return GuidanceState{}, err
	}
	e.logEvent(LogEvent{
		Event: LogEventGuide, ItemID: itemID, ProjectID: row.ProjectID,
		State: State(row.State), Reason: Reason(row.Reason),
		Message: fmt.Sprintf("guidance left by %s (%d now pending)", guidanceAuthorText(entry), len(pending)),
	})
	return GuidanceState{
		ItemID: itemID, Pending: pending,
		State: State(row.State), Reason: Reason(row.Reason), PhaseID: phaseID,
		Quarantined: quarantined,
	}, nil
}

// guidedPhase names the phase the run is in, so the caller can say what the
// guidance is waiting behind. It reads the persisted attempts rather than the
// resident item, because a parked run has no resident item at all and a caller
// must not get a different answer depending on whether the run happens to be
// running. A run that has entered no phase yet has none, which drops the field.
func (e *Engine) guidedPhase(itemID string) (string, error) {
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return "", fmt.Errorf("guide run %q phases: %w", itemID, err)
	}
	current, ok := currentPhaseAttempt(phases)
	if !ok {
		return "", nil
	}
	return current.PhaseID, nil
}

// pendingGuidance decodes the slot. A slot that will not decode is an ERROR
// rather than an empty one: the column is engine-written JSON, so undecodable
// content is corruption, and silently reading it as "nothing pending" would drop
// an operator's instruction and report success. It is a HEALED error, though —
// the bytes are quarantined and the column emptied on the way out, so the fault
// is stated once rather than re-raised forever (healGuidanceSlot).
func (e *Engine) pendingGuidance(itemID string) ([]GuidanceEntry, error) {
	raw, err := e.store.WorkItemPendingGuidance(itemID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var pending []GuidanceEntry
	if err := decodeJSON(raw, &pending); err != nil {
		return nil, e.healGuidanceSlot(itemID, raw, err)
	}
	return pending, nil
}

// healedGuidanceError is what every read of a healed slot returns. It is a type
// rather than a sentinel because the heal produces two things that must not
// drift apart: the prose a delivery parks with, and the typed facts `guide`
// hands back to the caller whose entry just landed on an emptied slot. Callers
// reach the facts through `errors.As` and each decides what an empty slot means
// to it — the delivery parks ONCE and says so, `guide` keeps the new entry and
// reports the discard, and an ack has nothing left to owe.
type healedGuidanceError struct {
	quarantine GuidanceQuarantine
	message    string
}

func (e *healedGuidanceError) Error() string { return e.message }

// healedGuidance answers the quarantine an error carries, if it is one.
func healedGuidance(err error) (*GuidanceQuarantine, bool) {
	var healed *healedGuidanceError
	if !errors.As(err, &healed) {
		return nil, false
	}
	return &healed.quarantine, true
}

// healGuidanceSlot is what an undecodable slot costs: one loud park, and then
// the run is well again.
//
// The column is engine-written JSON, so content that will not decode is
// corruption — but its entries are already unrecoverable AS GUIDANCE, because
// nothing can render bytes nothing can decode. Leaving them in place made the
// failure immortal instead of merely bad: every fresh agent-phase entry read the
// slot, parked `wiring-error`, and every resume re-entered that phase and parked
// again, with no verb anywhere that could clear the column. So the heal preserves
// what can still be preserved and removes what cannot: the raw bytes go to the
// engine log VERBATIM — that line is the only surviving copy, which is why it is
// never truncated — and the column is emptied.
//
// The error it returns says all three things, because it is what the operator
// reads as the attempt's park cause: the guidance would not decode, the raw
// content is in the log, and the slot is empty so their steer must be re-issued.
// A clear that FAILS returns an unhealed error instead: the slot is still bad,
// this park is not the last one, and saying otherwise would be a promise the
// store just refused to keep.
func (e *Engine) healGuidanceSlot(itemID string, raw json.RawMessage, decodeErr error) error {
	e.logEvent(LogEvent{
		Event: LogEventGuidanceUndecodable, ItemID: itemID,
		Message: fmt.Sprintf(
			"pending guidance would not decode (%v); its raw column content is preserved here verbatim and nowhere else: %s",
			decodeErr, raw),
	})
	if err := e.store.SetWorkItemPendingGuidance(itemID, nil); err != nil {
		return fmt.Errorf(
			"run %q pending guidance would not decode (%w) and the slot could not be cleared: %w",
			itemID, decodeErr, err)
	}
	return &healedGuidanceError{
		quarantine: GuidanceQuarantine{
			Bytes: len(raw), Reason: decodeErr.Error(), LogEvent: LogEventGuidanceUndecodable,
		},
		message: fmt.Sprintf(
			"run %q had pending operator guidance that would not decode (%v); its raw content (%d bytes) is preserved in the engine log under event %q, and the slot has been cleared, so nothing will park on it again — re-issue any steer with `run guide`",
			itemID, decodeErr, len(raw), LogEventGuidanceUndecodable),
	}
}

// deliverGuidance consumes the slot for one phase entry, returning the entries
// the attempt will render.
//
// ORDER IS THE WHOLE CONTRACT HERE, and it is deliberately not atomic. The
// attempt row carrying the guidance is persisted FIRST (by the caller) and the
// slot cleared afterwards — not when the row lands, but when the send door
// reports a prompt that renders the block dispatched to a live provider
// session (`AckFeedbackRendered` → `ackGuidance`). Everything between those
// two points is a window in which the entries are still pending, so every way an
// attempt can end without a turn — a pause taking the held start down, a failed
// acquisition parking it, a crash — redelivers rather than loses. The reverse
// order, a single transaction, and a clear at the row write all convert that
// window into a LOST instruction: a row would exist, the slot would be empty,
// and nothing would ever render what the operator wrote. Between telling a run
// something twice and never telling it at all, twice is the answer — the entry
// says when it was left, so a second delivery reads as what it is.
//
// A phase that renders no prompt is not a boundary at all (deliversGuidance), so
// the entries stay pending for one that does.
//
// A slot that will not DECODE parks this entry `wiring-error` with the cause
// healGuidanceSlot composed — once. The heal has already emptied the column by
// the time the error arrives, so the resume that follows reads an empty slot and
// runs the phase: the run is told loudly, and exactly as often as a human can act
// on it.
func (e *Engine) deliverGuidance(item *runtimeItem, phase def.Phase, entry phaseEntry) ([]GuidanceEntry, error) {
	if entry != entryFresh || !deliversGuidance(phase) {
		return nil, nil
	}
	return e.pendingGuidance(item.item.ID)
}

// entryGuidance resolves both the guidance belonging to the parked round and
// what is still owed an acknowledgement.
//
// A continuation keeps the round guidance resident for a possible context-loss
// restart, but its short prompt renders no guidance block. A restart renders
// the original block into the replacement context. It acknowledges only those
// original entries that are still in the pending slot: that is the held-start
// case, where the parked attempt recorded the block but no provider turn ever
// rendered it. Entries added after the park remain pending for the next fresh
// phase entry.
func (e *Engine) entryGuidance(item *runtimeItem, phase def.Phase, entry phaseEntry) ([]GuidanceEntry, []GuidanceEntry, error) {
	switch entry {
	case entryContinuation:
		return nil, nil, nil
	case entryRestart:
		pending, err := e.pendingGuidance(item.item.ID)
		if err != nil {
			return nil, nil, err
		}
		round := append([]GuidanceEntry(nil), item.guidance...)
		return round, matchingGuidance(pending, round), nil
	}
	delivered, err := e.deliverGuidance(item, phase, entry)
	if err != nil {
		return nil, nil, err
	}
	return delivered, delivered, nil
}

// matchingGuidance returns the entries recorded on the round that still exist
// in the pending slot. Equality covers the engine-stamped identity (text,
// author, run, and timestamp), so guidance added after the park cannot be
// mistaken for an unacknowledged original entry.
func matchingGuidance(pending, round []GuidanceEntry) []GuidanceEntry {
	matched := make([]GuidanceEntry, 0, len(round))
	for _, entry := range round {
		for _, candidate := range pending {
			if candidate == entry {
				matched = append(matched, entry)
				break
			}
		}
	}
	return matched
}

// ackGuidance clears the entries a dispatched prompt has now rendered into a
// live provider session. It is the second half of deliverGuidance's ordering
// rule, settled from the send door's `AckFeedbackRendered` alongside the
// attempt's owed feedback; see deliverGuidance for why the clear waits this
// long, and `ackFeedbackRendered` for why the send — not the runner start's
// success — is the proof.
//
// It removes the delivered entries rather than emptying the slot, because the
// slot is live between the delivery and this call: an operator who guided the
// run again in that window left an entry no attempt has read, and clearing the
// column wholesale would drop exactly the instruction this ordering exists to
// protect. The removal is idempotent — a wave's second agent unit finds nothing
// left to remove and writes nothing — and a failure leaves the entries pending,
// which is a redelivery and therefore the safe direction. It is stated loudly
// all the same: nothing else would say the run is about to hear itself twice.
func (e *Engine) ackGuidance(item *runtimeItem) {
	if len(item.guidanceUnacked) == 0 {
		return
	}
	pending, err := e.pendingGuidance(item.item.ID)
	if err != nil {
		if _, healed := healedGuidance(err); healed {
			// The heal emptied the column, which includes the entries this attempt
			// rendered: there is nothing left to remove and nothing left to owe, so
			// the acknowledgement is discharged rather than failed. The quarantine
			// line already says what was discarded.
			item.guidanceUnacked = nil
			return
		}
		e.reportGuidanceAck(item, err)
		return
	}
	remaining := withoutGuidance(pending, item.guidanceUnacked)
	if len(remaining) == len(pending) {
		item.guidanceUnacked = nil // Already cleared: this attempt was a redelivery.
		return
	}
	var encoded json.RawMessage
	if len(remaining) > 0 {
		encoded, err = json.Marshal(remaining)
		if err != nil {
			e.reportGuidanceAck(item, fmt.Errorf("encode remaining guidance: %w", err))
			return
		}
	}
	if err := e.store.SetWorkItemPendingGuidance(item.item.ID, encoded); err != nil {
		e.reportGuidanceAck(item, err)
		return
	}
	delivered := len(item.guidanceUnacked)
	item.guidanceUnacked = nil
	e.logEvent(LogEvent{
		Event: LogEventGuidanceDeliver, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
		PhaseID: item.phaseID, Attempt: item.attempt,
		Message: fmt.Sprintf("%d guidance %s rendered by this attempt's session and cleared from the slot",
			delivered, pluralEntries(delivered)),
	})
}

// reportGuidanceAck surfaces a slot write that did not happen. The run is fine —
// it is already running the guidance — so this never parks it; what it must not
// do is stay silent about a redelivery the operator will otherwise read as the
// engine ignoring their retraction. The entries stay owed, so the next element
// send of this attempt (`AckFeedbackRendered`) tries again.
func (e *Engine) reportGuidanceAck(item *runtimeItem, err error) {
	wrapped := fmt.Errorf("clear delivered guidance for run %q: %w", item.item.ID, err)
	e.emitError(item.item.ID, wrapped)
	e.logEvent(LogEvent{
		Event: LogEventGuidanceDeliver, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
		PhaseID: item.phaseID, Attempt: item.attempt,
		Message: fmt.Sprintf(
			"%d guidance %s were rendered but could not be cleared, so a later phase entry will deliver them again: %v",
			len(item.guidanceUnacked), pluralEntries(len(item.guidanceUnacked)), err),
	})
}

// withoutGuidance removes every delivered entry from the pending slice. Entries
// are compared by value — text, time, author — because that is the whole of what
// one is, and the slot has no ids of its own.
func withoutGuidance(pending, delivered []GuidanceEntry) []GuidanceEntry {
	if len(pending) == 0 || len(delivered) == 0 {
		return pending
	}
	remaining := make([]GuidanceEntry, 0, len(pending))
	for _, entry := range pending {
		consumed := false
		for _, done := range delivered {
			if entry == done {
				consumed = true
				break
			}
		}
		if !consumed {
			remaining = append(remaining, entry)
		}
	}
	return remaining
}

// deliversGuidance reports whether entering this phase renders a prompt somebody
// could read the guidance in.
//
// A `driver: tool` phase runs a command, a `shape: call` phase starts a child
// run, and a fan-out whose every element is a command likewise renders nothing:
// delivering there would clear the slot into a turn that does not exist, which
// is the silent loss the whole ordering rule above exists to prevent. Those
// phases are simply not boundaries, and the entries wait for one that is.
func deliversGuidance(phase def.Phase) bool {
	if phase.IsCall() {
		return false
	}
	if phase.EffectiveShape() == def.ShapeFanOut {
		for _, unit := range phase.UnitDefinitions() {
			if driver, ok := unit.EffectiveDriver(); ok && driver == def.DriverAgent {
				return true
			}
		}
		if phase.Join != nil {
			driver, ok := phase.Join.EffectiveDriver()
			return ok && driver == def.DriverAgent
		}
		return false
	}
	return phase.Driver == def.DriverAgent
}

// guidanceNote is the line a delivered attempt carries in its feedback, so the
// turn reading the guidance block is told where it came from — the same job
// definitionRefreshNote does for a re-read definition.
func guidanceNote(delivered []GuidanceEntry) string {
	return fmt.Sprintf(
		"%d operator guidance %s was left for this run while it was working and is delivered at this phase entry; it appears in the operator-guidance block below and is not part of your phase's authored instructions",
		len(delivered), pluralEntries(len(delivered)))
}

func pluralEntries(count int) string {
	if count == 1 {
		return "entry"
	}
	return "entries"
}

// guidanceAuthorText renders one entry's provenance for a log line and for the
// prompt block's attribution.
func guidanceAuthorText(entry GuidanceEntry) string {
	if entry.By == GuidanceByPhase {
		if entry.ByRun != "" {
			return fmt.Sprintf("an agent phase of run %s", entry.ByRun)
		}
		return "an agent phase"
	}
	return "a human operator"
}
