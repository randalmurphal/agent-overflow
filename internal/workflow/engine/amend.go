package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/workflow/def"
)

// Changing a resting run's seeds.
//
// A run's seeds are not part of its frozen snapshot: the variable context is
// rebuilt from the run ROW at every phase entry (`variableContext`), so the
// column is live data the run re-reads rather than a definition it froze. That
// is the whole mechanism an amendment needs — and it is why the boundary rule
// here reads differently from `--refresh-def`'s (D50) even though both answer
// "when does an operator's edit reach the run".
//
// What an amendment can never do is reach work that already happened. A phase
// that completed keeps the outputs it produced, a fan-out attempt keeps the
// variables its units were expanded and launched with (`restoreFanOut` reads
// them back from the attempt's persisted input envelope, not from the row), and
// a called run keeps the seeds its caller's arguments evaluated to at
// invocation. `SeedEffect` states which of those the parked run is in, so the
// operator is told when the value they just wrote will be read instead of
// guessing from the verb they resume with.

// SeedEffect says when the run will read amended seeds.
type SeedEffect string

const (
	// SeedEffectNextAttempt: the next attempt this run starts builds its
	// variable context from the row, so any resume of this park reads the new
	// values. A continuation still counts — it continues the provider SESSION,
	// and the turn it starts is a new attempt with a freshly built context.
	SeedEffectNextAttempt SeedEffect = "next-attempt"
	// SeedEffectFreshEntry: the parked attempt is repaired in place by a bare
	// resume — a fan-out reopens its blocked units, a call phase re-links the
	// child it is waiting on — and that attempt runs on the variables it froze.
	// The new values are read when a phase is next entered fresh, which
	// `run resume --phase <id>` does deliberately and the run's next phase does
	// on its own.
	SeedEffectFreshEntry SeedEffect = "fresh-phase-entry"
)

// SeedAmendment is what an amendment did: which names changed, the whole seed
// object the run now carries, and when the run will read it.
type SeedAmendment struct {
	ItemID  string          `json:"itemId"`
	Names   []string        `json:"names"`
	Seeds   json.RawMessage `json:"seeds"`
	PhaseID string          `json:"phaseId,omitempty"`
	Effect  SeedEffect      `json:"effect"`
}

// AmendSeeds changes seed values on a run that is resting `needs-human`.
//
// It is a command like every other human action, so it cannot interleave with a
// resume, a gate resolution, or a completion: the run's state is read and its
// seeds written in one turn of the loop, and a run that starts running a
// microsecond later reads what this wrote rather than being written under.
func (e *Engine) AmendSeeds(itemID string, values map[string]any) (SeedAmendment, error) {
	result := &SeedAmendment{}
	if err := e.request(amendSeedsCommand{itemID: itemID, values: values, result: result}); err != nil {
		return SeedAmendment{}, err
	}
	return *result, nil
}

// amendSeeds is the command-loop half. Every refusal is decided before the
// write, so a rejected amendment leaves the run record byte-identical — the
// same totality `resolveDefinition` gives a refused refresh.
func (e *Engine) amendSeeds(itemID string, values map[string]any) (SeedAmendment, error) {
	if len(values) == 0 {
		return SeedAmendment{}, fmt.Errorf("amend seeds %q: name at least one seed to change", itemID)
	}
	// The row is the authority on both refusals, and it is authoritative here
	// because the transition that changes it and this read are both the command
	// loop's: a run is resident exactly while its row says `running`.
	row, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return SeedAmendment{}, fmt.Errorf("amend seeds %q: %w", itemID, err)
	}
	switch State(row.State) {
	case StateNeedsHuman:
		if Reason(row.Reason) == ReasonDisposition {
			return SeedAmendment{}, fmt.Errorf(
				"amend seeds %q: this run is done and awaiting disposition, so no phase of it will read a seed again; settle it with WorkflowMergeItem, WorkflowCreateItemPR, or WorkflowDiscardItem",
				itemID)
		}
	case StateRunning:
		return SeedAmendment{}, fmt.Errorf(
			"amend seeds %q: the run is running, and an attempt reads its seeds when it starts; changing them under one would leave a single attempt rendering two sets of inputs. Pause it, or wait for it to rest",
			itemID)
	default:
		return SeedAmendment{}, fmt.Errorf(
			"amend seeds %q: the run is %s, so there is no attempt left to read a seed", itemID, row.State)
	}
	item, err := e.loadParked(itemID)
	if err != nil {
		return SeedAmendment{}, fmt.Errorf("amend seeds %q: %w", itemID, err)
	}
	if len(item.workflow.Inputs) == 0 && len(item.workflow.Phases) == 0 {
		return SeedAmendment{}, fmt.Errorf(
			"amend seeds %q: this run never froze a workflow, so there are no declared inputs to amend", itemID)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var findings []string
	for _, name := range names {
		if _, declared := item.workflow.Inputs[name]; !declared {
			return SeedAmendment{}, fmt.Errorf(
				"amend seeds %q: %q is not an input of workflow %q; it declares %s. A definition edited since this run started is not what the run renders — `run resume --refresh-def` re-reads it at a fresh entry",
				itemID, name, item.workflow.ID, declaredInputs(item.workflow))
		}
		findings = append(findings, def.ValidateInput(item.workflow, name, values[name])...)
	}
	if len(findings) > 0 {
		return SeedAmendment{}, fmt.Errorf("amend seeds %q: %s", itemID, strings.Join(findings, "; "))
	}

	seeds := make(map[string]any)
	if len(item.item.Seeds) > 0 {
		if err := decodeJSON(item.item.Seeds, &seeds); err != nil {
			return SeedAmendment{}, fmt.Errorf("amend seeds %q: decode current seeds: %w", itemID, err)
		}
		if seeds == nil {
			return SeedAmendment{}, fmt.Errorf("amend seeds %q: the run's seeds are not an object", itemID)
		}
	}
	for _, name := range names {
		seeds[name] = values[name]
	}
	encoded, err := json.Marshal(seeds)
	if err != nil {
		return SeedAmendment{}, fmt.Errorf("amend seeds %q: encode seeds: %w", itemID, err)
	}
	if len(encoded) > MaxSeedBytes {
		return SeedAmendment{}, fmt.Errorf(
			"amend seeds %q: the amended seeds are %d bytes; maximum is %d", itemID, len(encoded), MaxSeedBytes)
	}
	if err := e.store.UpdateWorkItemSeeds(itemID, encoded); err != nil {
		return SeedAmendment{}, err
	}
	item.item.Seeds = encoded
	effect := seedEffect(item)
	e.logEvent(LogEvent{
		Event: LogEventSeedAmend, ItemID: itemID, ProjectID: item.item.ProjectID,
		PhaseID: item.phaseID, Attempt: item.attempt, Reason: Reason(item.item.Reason),
		Message: fmt.Sprintf("seeds %s amended while parked; read %s",
			strings.Join(names, ", "), effect),
	})
	return SeedAmendment{
		ItemID: itemID, Names: names, Seeds: encoded, PhaseID: item.phaseID, Effect: effect,
	}, nil
}

// seedEffect answers when the parked run will read what was just written, from
// what a bare resume of THIS park would do (`resume` → `resumeItem`).
//
// A continuable park whose attempt is repaired in place — a fan-out, or a call
// phase whose child is re-linked rather than re-invoked — runs that attempt on
// the variables it persisted. Everything else builds a new context: a
// non-continuable park re-enters the phase, and a continuable single-shape park
// starts a new attempt on the session it parked on.
func seedEffect(item *runtimeItem) SeedEffect {
	if !ContinuableReason(Reason(item.item.Reason)) {
		return SeedEffectNextAttempt
	}
	if item.phaseID == "" || item.attempt < 1 || len(item.workflow.Phases) == 0 {
		// Nothing was ever attempted, so resumeItem enters the phase fresh.
		return SeedEffectNextAttempt
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return SeedEffectNextAttempt
	}
	if phase.IsCall() || phase.EffectiveShape() == def.ShapeFanOut {
		return SeedEffectFreshEntry
	}
	return SeedEffectNextAttempt
}

// declaredInputs renders a workflow's input names the way a refusal has to name
// them: sorted, so the same mistake reads the same way twice.
func declaredInputs(workflow def.Workflow) string {
	if len(workflow.Inputs) == 0 {
		return "no inputs at all"
	}
	names := make([]string, 0, len(workflow.Inputs))
	for name := range workflow.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
