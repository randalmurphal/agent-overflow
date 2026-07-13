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

func loopCounts(itemID string, phases []store.WorkItemPhaseContext) (map[string]int, error) {
	counts := make(map[string]int)
	for _, phase := range phases {
		if len(phase.Intervention) > 0 {
			var takeover TakeoverIntervention
			if decodeJSON(phase.Intervention, &takeover) == nil && takeover.Kind == "taken-over" {
				continue
			}
		}
		if len(phase.GateTrace) == 0 {
			continue
		}
		var trace def.GateTrace
		if err := decodeJSON(phase.GateTrace, &trace); err != nil {
			return nil, fmt.Errorf("decode gate trace for %s/%s/%d: %w", itemID, phase.PhaseID, phase.Attempt, err)
		}
		if trace.Decision.Kind == def.DecisionLoop && trace.Decision.LoopEdge != "" {
			counts[trace.Decision.LoopEdge]++
		}
	}
	return counts, nil
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
