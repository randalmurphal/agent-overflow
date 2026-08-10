package engine

import (
	"fmt"
	"strings"
)

// ParkDisposition moves a completed item into the typed human-attention state
// after a disposition refusal. The engine remains the lifecycle owner even
// though done items are no longer held in its in-memory scheduler map.
func (e *Engine) ParkDisposition(itemID string) error {
	return e.request(parkDispositionCommand{itemID: itemID})
}

// RerunFailed rebuilds a failed item from its frozen snapshot and starts the
// failed phase again immediately, carrying its diagnosis as guidance. Optional
// guidance from whoever asked for the rerun is appended to that diagnosis, the
// same way RetryUnit's note rides a unit retry — a human or an agent re-running
// a failure usually knows something the diagnosis does not.
//
// refreshDefinition re-reads the workflow from disk for the new attempt instead
// of rendering the one the run froze at start. A rerun is a fresh phase entry,
// which is the only place a refresh is offered (refresh.go).
func (e *Engine) RerunFailed(itemID, guidance string, refreshDefinition bool) error {
	return e.request(rerunFailedCommand{itemID: itemID, guidance: guidance, refreshDefinition: refreshDefinition})
}

// ResolveDisposition returns a disposition-parked item to done after its
// external disposition and receipt persistence succeed.
func (e *Engine) ResolveDisposition(itemID string) error {
	return e.request(resolveDispositionCommand{itemID: itemID})
}

func (e *Engine) parkDisposition(itemID string) error {
	item, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("park disposition item %q: %w", itemID, err)
	}
	if State(item.State) != StateDone {
		return fmt.Errorf("park disposition item %q: invalid state %s, want %s", itemID, item.State, StateDone)
	}
	endedAt := e.timestamp()
	if err := e.store.UpdateWorkItemState(itemID, string(StateNeedsHuman), string(ReasonDisposition), endedAt); err != nil {
		return fmt.Errorf("park disposition item %q: %w", itemID, err)
	}
	e.emitItemState(itemID, item.ProjectID, StateDone, StateNeedsHuman, ReasonDisposition)
	return nil
}

// rerunAction labels the refusals a rerun's definition re-read can raise, so
// they read as the verb the human used.
const rerunAction = "rerun failed item"

func (e *Engine) rerunFailed(itemID, guidance string, refreshDefinition bool) error {
	stored, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("rerun failed item %q: %w", itemID, err)
	}
	if State(stored.State) != StateFailed {
		return fmt.Errorf("rerun failed item %q: invalid state %s, want %s", itemID, stored.State, StateFailed)
	}
	if len(stored.Disposition) > 0 {
		return fmt.Errorf("rerun failed item %q: item was discarded — record kept; start a new run instead", itemID)
	}
	if _, tracked := e.items[itemID]; tracked {
		return fmt.Errorf("rerun failed item %q: already tracked", itemID)
	}
	runtime := &runtimeItem{item: stored}
	snapshot, err := decodeSnapshot(stored.Snapshot)
	if err != nil {
		return fmt.Errorf("rerun failed item %q snapshot: %w", itemID, err)
	}
	runtime.adoptSnapshot(snapshot)
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return fmt.Errorf("rerun failed item %q phases: %w", itemID, err)
	}
	if len(phases) == 0 {
		return fmt.Errorf("rerun failed item %q: no phase attempts", itemID)
	}
	latest := phases[len(phases)-1]
	frozen := stored.Snapshot
	// Everything a re-read can refuse is decided before the first write, so a
	// refused rerun leaves the failed run exactly as it was.
	if refreshDefinition {
		refreshed, encoded, err := e.resolveDefinition(runtime, latest.PhaseID, rerunAction)
		if err != nil {
			return err
		}
		// Adopted in memory here so every check below reads the definition this
		// attempt will render; the run-start write below is what persists it.
		runtime.adoptSnapshot(refreshed)
		snapshot, frozen = refreshed, encoded
	}
	if _, ok := findPhase(runtime.workflow, latest.PhaseID); !ok {
		return fmt.Errorf("rerun failed item %q: phase %q is not in the frozen workflow", itemID, latest.PhaseID)
	}
	runtime.phaseID = latest.PhaseID
	runtime.attempt = 0
	// The guidance seed is process-local; the diagnosis it distils stays
	// durable in the phase history, so a crash before the attempt lands loses
	// only the convenience copy.
	runtime.feedback, err = failedAttemptFeedback(Reason(stored.Reason), latest.PhaseID, latest.OutputEnvelope, guidance)
	if err != nil {
		return fmt.Errorf("rerun failed item %q output envelope: %w", itemID, err)
	}
	// A failed item is not resident after a restart, so the engine clock has
	// never seen its timestamps. Seed them before stamping anything new: the
	// rerun's attempts must sort after the ones they follow even if the wall
	// clock moved backwards, because attempt order is how phase history is
	// read back (loop counts, current attempt).
	e.observeItemTimestamps(stored)
	// Re-stamp the run start before the transition. If the transition then
	// fails the row stays `failed` with only a bumped started_at, which the
	// next rerun overwrites; the reverse order could leave a running item the
	// engine is not tracking.
	startedAt := e.timestamp()
	if err := e.freezeSnapshot(runtime, snapshot, frozen, startedAt); err != nil {
		return fmt.Errorf("rerun failed item %q run start: %w", itemID, err)
	}
	if refreshDefinition {
		e.noteDefinitionRefresh(runtime, latest.PhaseID)
	}
	if err := e.transition(runtime, StateRunning, ""); err != nil {
		return err
	}
	e.items[itemID] = runtime
	return e.enterPhase(runtime, entryFresh)
}

func (e *Engine) resolveDisposition(itemID string) error {
	item, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("resolve disposition item %q: %w", itemID, err)
	}
	if State(item.State) != StateNeedsHuman || Reason(item.Reason) != ReasonDisposition {
		return fmt.Errorf(
			"resolve disposition item %q: invalid state %s(%s), want %s(%s)",
			itemID, item.State, item.Reason, StateNeedsHuman, ReasonDisposition,
		)
	}
	if err := e.store.UpdateWorkItemState(itemID, string(StateDone), "", item.EndedAt); err != nil {
		return fmt.Errorf("resolve disposition item %q: %w", itemID, err)
	}
	e.emitItemState(itemID, item.ProjectID, StateNeedsHuman, StateDone, "")
	return nil
}

// failedAttemptFeedback distils the failed attempt's diagnosis into the note the
// next attempt reads, then appends the guidance whoever asked for the rerun
// supplied. Guidance comes last so it reads as a correction to the diagnosis
// above it rather than as part of it.
func failedAttemptFeedback(reason Reason, phaseID string, payload []byte, guidance string) (*Feedback, error) {
	note := string(reason)
	var envelope struct {
		Outputs map[string]any `json:"outputs"`
		Reason  *string        `json:"reason"`
	}
	if len(payload) > 0 {
		if err := decodeJSON(payload, &envelope); err != nil {
			return nil, err
		}
	}
	feedback := &Feedback{Values: make(map[string]any)}
	for _, name := range []string{"diagnosis", "summary"} {
		value, ok := envelope.Outputs[name]
		if !ok {
			continue
		}
		feedback.Values[phaseID+"."+name] = value
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" && note == string(reason) {
			note += ": " + strings.TrimSpace(text)
		}
	}
	if envelope.Reason != nil && note == string(reason) {
		if diagnosis := strings.TrimSpace(*envelope.Reason); diagnosis != "" {
			note += ": " + diagnosis
		}
	}
	if trimmed := strings.TrimSpace(guidance); trimmed != "" {
		note += "\nGuidance for this rerun: " + trimmed
	}
	feedback.Note = note
	if len(feedback.Values) == 0 {
		feedback.Values = nil
	}
	return feedback, nil
}
