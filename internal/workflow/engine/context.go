package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

type controlEnvelope struct {
	Status  string         `json:"status"`
	Outputs map[string]any `json:"outputs"`
}

func (e *Engine) variableContext(item *runtimeItem, current json.RawMessage) (map[string]any, []store.WorkItemPhaseContext, error) {
	vars := make(map[string]any)
	if len(item.item.Seeds) > 0 {
		if len(item.item.Seeds) > MaxSeedBytes {
			return nil, nil, fmt.Errorf("item %q seeds are %d bytes; maximum is %d", item.item.ID, len(item.item.Seeds), MaxSeedBytes)
		}
		if err := decodeJSON(item.item.Seeds, &vars); err != nil {
			return nil, nil, fmt.Errorf("decode item %q seeds: %w", item.item.ID, err)
		}
		if vars == nil {
			return nil, nil, fmt.Errorf("decode item %q seeds: expected an object", item.item.ID)
		}
	}
	phases, err := e.store.ListWorkItemPhaseContexts(item.item.ID)
	if err != nil {
		return nil, nil, err
	}
	latest := make(map[string]store.WorkItemPhaseContext)
	for _, phase := range phases {
		if phase.Status != "completed" || len(phase.OutputEnvelope) == 0 {
			continue
		}
		if prior, ok := latest[phase.PhaseID]; !ok || phase.Attempt > prior.Attempt {
			latest[phase.PhaseID] = phase
		}
	}
	for phaseID, phase := range latest {
		if err := addOutputs(vars, phaseID, phase.OutputEnvelope); err != nil {
			return nil, nil, fmt.Errorf("decode completed phase %s/%s/%d: %w", item.item.ID, phaseID, phase.Attempt, err)
		}
	}
	if len(current) > 0 {
		if err := addOutputs(vars, item.phaseID, current); err != nil {
			return nil, nil, fmt.Errorf("decode current phase %s/%s/%d: %w", item.item.ID, item.phaseID, item.attempt, err)
		}
	}
	return vars, phases, nil
}

func addOutputs(vars map[string]any, phaseID string, payload json.RawMessage) error {
	var envelope controlEnvelope
	if err := decodeJSON(payload, &envelope); err != nil {
		return err
	}
	if envelope.Status != "done" {
		return fmt.Errorf("status is %q, want done", envelope.Status)
	}
	for name, value := range envelope.Outputs {
		if value != nil {
			vars[phaseID+"."+name] = value
		}
	}
	return nil
}

func decodeJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

// loopWalkStep is how the previous counted attempt left the run, which is all
// the walk needs to classify the entry into the attempt that follows it.
type loopWalkStep struct {
	phaseID  string
	decision def.RouteDecision
	// failed marks that the run ended `failed` at this attempt. The only edge
	// out of `failed` is RerunFailed, which re-stamps the run start and enters
	// this same phase again — a new run epoch, not a continued attempt.
	failed bool
}

// freshLoopEntry reports whether phaseID was entered from outside its cycle,
// which is the only thing that refills the budget of the loop edges targeting
// it (spec §4, decision D21).
//
// A loop traversal is never a fresh entry — not even for a sibling edge that
// shares the target. If it were, two loop edges aimed at one phase would clear
// each other every lap and the pair could iterate forever with both counters
// below their bound.
//
// Anything the walk cannot attribute to a routing decision is treated as the
// same entry continuing (an Answer continuation, a resume in place, a takeover
// finalize turn), so an unfamiliar history can only under-refill a budget,
// never unbind one.
func freshLoopEntry(previous *loopWalkStep, phaseID string) bool {
	switch {
	case previous == nil:
		return true // The run's first attempt: nothing has been counted yet.
	case previous.decision.Kind == def.DecisionLoop && previous.decision.Target == phaseID:
		return false
	case previous.failed:
		return true // RerunFailed re-entered the phase whose gate failed the run.
	case previous.decision.Kind == def.DecisionAdvance && previous.decision.Target == phaseID:
		return true
	default:
		// A phase change no persisted decision explains is a human Resume aimed
		// at another phase, which enters that phase from outside.
		return previous.phaseID != phaseID
	}
}

// loopCounts derives how much of each loop edge's bound the item has already
// spent, from its persisted phase attempts alone.
//
// A loop edge's counter counts its traversals since the edge's target phase was
// last entered from outside the cycle (spec §4, decision D21) — not the item's
// whole lifetime, which starved an inner loop of retry budget on every lap of
// an outer one. Nothing extra is persisted for this: the gate traces the run
// already writes are the record.
//
// The walk depends on rows arriving in run order.
// store.ListWorkItemPhaseContexts orders by (started_at, phase_id, attempt) and
// every attempt's started_at comes from Engine.timestamp(), which is strictly
// increasing across the engine's lifetime and is re-seeded from persisted
// timestamps whenever an item is rebuilt or rerun — so attempt order is
// insertion order regardless of what the wall clock does.
//
// The soundness of the reset rule also rests on def's graph validation: a cycle
// closed by forward routes is rejected (`gate.unbounded-cycle`) and a loop
// target must strictly dominate its source (`gate.loop-ancestor`), so the only
// way back to an earlier phase is a bounded loop route.
func loopCounts(itemID string, phases []store.WorkItemPhaseContext) (map[string]int, error) {
	counts := make(map[string]int)
	targets := make(map[string]string) // Loop edge -> the phase it re-enters.
	var previous *loopWalkStep
	for _, phase := range phases {
		abandoned, err := abandonedByTakeover(phase.Intervention)
		if err != nil {
			return nil, fmt.Errorf("decode intervention for %s/%s/%d: %w", itemID, phase.PhaseID, phase.Attempt, err)
		}
		if abandoned {
			// A taken-over attempt spends no loop budget and is not an entry of
			// its own: the finalize turn continues the entry that preceded it.
			continue
		}
		if freshLoopEntry(previous, phase.PhaseID) {
			for edge, target := range targets {
				if target == phase.PhaseID {
					delete(counts, edge)
				}
			}
		}
		step := loopWalkStep{phaseID: phase.PhaseID, failed: phase.Status == "failed"}
		if len(phase.GateTrace) > 0 {
			var trace def.GateTrace
			if err := decodeJSON(phase.GateTrace, &trace); err != nil {
				return nil, fmt.Errorf("decode gate trace for %s/%s/%d: %w", itemID, phase.PhaseID, phase.Attempt, err)
			}
			step.decision = trace.Decision
			step.failed = step.failed || trace.Decision.Kind == def.DecisionFailed
			if trace.Decision.Kind == def.DecisionLoop && trace.Decision.LoopEdge != "" {
				counts[trace.Decision.LoopEdge]++
				targets[trace.Decision.LoopEdge] = trace.Decision.Target
			}
		}
		previous = &step
	}
	return counts, nil
}

// abandonedByTakeover reports whether an attempt was detached for human
// steering. A malformed intervention is an error rather than a false: the
// column is CHECK-constrained JSON, so undecodable content is corruption the
// run should park on, not a row to silently count.
func abandonedByTakeover(intervention json.RawMessage) (bool, error) {
	if len(intervention) == 0 {
		return false, nil
	}
	var takeover TakeoverIntervention
	if err := decodeJSON(intervention, &takeover); err != nil {
		return false, err
	}
	return takeover.Kind == TakeoverInterventionKind, nil
}

func nextAttempt(phases []store.WorkItemPhaseContext, phaseID string) int {
	next := 1
	for _, phase := range phases {
		if phase.PhaseID == phaseID && phase.Attempt >= next {
			next = phase.Attempt + 1
		}
	}
	return next
}

func (e *Engine) timestamp() int64 {
	now := e.now().UnixMilli()
	if now <= e.lastTimestamp {
		now = e.lastTimestamp + 1
	}
	e.lastTimestamp = now
	return now
}
