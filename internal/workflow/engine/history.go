package engine

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// historyEnvelope is what a prior attempt's envelope is read for: a completed
// attempt's ratified outputs, and — for one that rested instead — the account
// it wrote about why. It is separate from controlEnvelope because that shape is
// also WRITTEN (a synthesized call-phase envelope, a park cause) and must not
// grow fields the engine does not author.
type historyEnvelope struct {
	Status   string         `json:"status"`
	Outputs  map[string]any `json:"outputs"`
	Reason   string         `json:"reason"`
	Question string         `json:"question"`
}

// bindPhaseHistory binds every `history.<phase>` input the phase being run
// declares. Only that phase's declarations are read: the binding is expensive
// enough — one decode per prior attempt — that materializing every phase's
// history on every gate evaluation would be paid by runs that reference none of
// it. Fan-out units and the join inherit the attempt's context, so a unit
// prompt reads the same binding its phase declared.
//
// A runtimeItem with no resident phase (a completed child read for its declared
// outputs) declares nothing and binds nothing.
func bindPhaseHistory(vars map[string]any, item *runtimeItem, current attemptRef, phases []store.WorkItemPhaseContext) error {
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return nil
	}
	for name, declaration := range phase.Inputs {
		target, reserved := def.HistoryBinding(name)
		if !reserved {
			continue
		}
		entries, err := historyEntries(item.item.ID, target, phases, current, def.EffectiveHistoryWindow(declaration))
		if err != nil {
			return err
		}
		vars[name] = entries
	}
	return nil
}

// historyEntries renders one phase's prior attempts, oldest last-in-the-slice
// first, EXCLUDING the attempt the context is being built for.
//
// Non-completed attempts appear, which no other variable path admits: a round
// that parked with a question or failed outright is part of why a loop is on
// its fourth lap, and a series that silently skipped them would read as a
// shorter, cleaner history than the one that actually happened. They carry no
// outputs — nothing ratified them — only the account the attempt wrote.
func historyEntries(
	itemID, target string, phases []store.WorkItemPhaseContext, current attemptRef, window int,
) ([]any, error) {
	rows := make([]store.WorkItemPhaseContext, 0, window)
	for _, phase := range phases {
		if phase.PhaseID != target || current.matches(phase) {
			continue
		}
		rows = append(rows, phase)
	}
	// The window trims from the OLD end: a loop's most recent rounds are what
	// the next one is reacting to.
	if len(rows) > window {
		rows = rows[len(rows)-window:]
	}
	entries := make([]any, len(rows))
	spent := 0
	elide := false
	// Newest first, so the byte budget falls on the oldest rounds rather than on
	// the ones nearest the attempt that will read them. `spent` counts carried
	// content only; the skeletons that replace the rest are bounded by the
	// window, which is the whole reason the window has a ceiling of its own.
	for index := len(rows) - 1; index >= 0; index-- {
		if elide {
			entries[index] = elidedHistoryEntry(rows[index])
			continue
		}
		entry, err := historyEntry(itemID, rows[index])
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("encode phase history %s/%s/%d: %w", itemID, rows[index].PhaseID, rows[index].Attempt, err)
		}
		// The newest prior attempt is always carried whole. One envelope can fill
		// the whole budget by itself, and a binding whose only substantive entry
		// was elided would be worse than no binding at all.
		if index != len(rows)-1 && spent+len(encoded) > def.MaxHistoryBytes {
			elide = true
			entries[index] = elidedHistoryEntry(rows[index])
			continue
		}
		spent += len(encoded)
		entries[index] = entry
	}
	return entries, nil
}

// historyEntry renders one prior attempt. An envelope is decoded only where its
// content is carried, and a malformed one is then an error rather than a skipped
// entry, for the same reason addOutputs treats one that way: the column is
// CHECK-constrained JSON this engine wrote, so content it cannot read back is
// corruption the run should park on rather than a round it quietly drops from
// its own history.
func historyEntry(itemID string, row store.WorkItemPhaseContext) (map[string]any, error) {
	entry := map[string]any{"attempt": row.Attempt, "status": row.Status}
	if len(row.OutputEnvelope) == 0 {
		return entry, nil
	}
	var envelope historyEnvelope
	if err := decodeJSON(row.OutputEnvelope, &envelope); err != nil {
		return nil, fmt.Errorf("decode phase history %s/%s/%d: %w", itemID, row.PhaseID, row.Attempt, err)
	}
	if row.Status == "completed" {
		if len(envelope.Outputs) > 0 {
			entry["outputs"] = envelope.Outputs
		}
		return entry, nil
	}
	// A parked, failed, or superseded attempt produced no ratified outputs — the
	// gate never accepted any — so the entry carries what the attempt itself said
	// and invents nothing else.
	for name, value := range map[string]string{
		"envelopeStatus": envelope.Status, "reason": envelope.Reason, "question": envelope.Question,
	} {
		if value != "" {
			entry[name] = value
		}
	}
	return entry, nil
}

// elidedHistoryEntry is the skeleton an entry past the byte budget becomes. The
// attempt still appears — the count of rounds is itself a fact the reader needs
// — and the entry states that its content was dropped, so a shortened window
// can never be read as a phase that ran fewer times.
func elidedHistoryEntry(row store.WorkItemPhaseContext) map[string]any {
	return map[string]any{
		"attempt": row.Attempt,
		"status":  row.Status,
		"elided": fmt.Sprintf(
			"content omitted: this binding's %d-byte budget was reached; read the attempt from the run record",
			def.MaxHistoryBytes,
		),
	}
}
