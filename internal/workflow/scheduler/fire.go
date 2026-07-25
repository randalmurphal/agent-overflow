package scheduler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// Source is the run source an automation-started run records; the automation's
// id is its source ref. The overlap probe reads exactly this pair back.
const Source = "automation"

// attempt is the one path from "a trigger fired" to "a run started, or a skip
// was recorded". Every gate below refuses the fire the same way: a scheduled
// fire records a skip with the reason on the automation's row, a manual fire
// (Run now) returns the reason as an error because a human is present to read
// it. Nothing is ever dropped quietly.
//
// The order is cheapest-and-most-specific first: a self-chain and an overlapping
// run both make the condition irrelevant, and the reason a human needs is the
// one that actually blocked the fire.
//
// It takes an id rather than a row so every fire path — cron, internal event,
// Run now — reads the automation at the moment it fires. The loop's snapshot is
// only ever used to decide *whether* something is due; job notes, seeds, and the
// run-if condition can all be edited between arming an occurrence and its coming
// due, and continuity notes exist precisely so the next run reads what the last
// one left.
func (s *Scheduler) attempt(automationID string, fire Fire, manual bool) (string, error) {
	automation, err := s.store.GetAutomation(automationID)
	if err != nil {
		if manual {
			return "", fmt.Errorf("run automation %s: %w", automationID, err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			// Deleted while its occurrence was in flight. There is no row left to
			// record a skip on, and losing a fire to a delete is what the delete
			// asked for.
			return "", nil
		}
		return "", fmt.Errorf("workflow scheduler: load automation %s to fire: %w", automationID, err)
	}
	// Disabled while its occurrence was in flight. Not a skip either: a disabled
	// automation has no schedule to have skipped. Run now works on a disabled row
	// by design — a human pressing the button is not the schedule.
	if !manual && !automation.Enabled {
		return "", nil
	}

	refuse := func(reason string) (string, error) {
		if manual {
			return "", fmt.Errorf("run automation %q now: %s", automation.Name, reason)
		}
		if err := s.store.RecordAutomationSkip(automation.ID, fire.At.UnixMilli(), reason); err != nil {
			return "", fmt.Errorf("workflow scheduler: record skip for automation %s (%s): %w",
				automation.ID, reason, err)
		}
		return "", nil
	}

	// A run this same automation started, whose completion re-triggers it, is
	// always an authoring accident: it would chain forever. Cycles across two
	// automations stay legal — those are a deliberate design.
	if fire.Event != nil && fire.Event.Source == Source && fire.Event.SourceRef == automation.ID {
		return refuse(fmt.Sprintf("self-chain: run %s was started by this automation", fire.Event.ItemID))
	}

	active, found, err := s.store.ActiveAutomationRun(automation.ID)
	if err != nil {
		// Not knowing whether a run is active is not permission to start one.
		return refuse(fmt.Sprintf("overlap check failed: %v", err))
	}
	if found {
		return refuse(fmt.Sprintf("run %s is still %s", active.ItemID, describeRun(active)))
	}

	seeds, encoded, err := runSeeds(automation, fire)
	if err != nil {
		return refuse(fmt.Sprintf("seeds are unusable: %v", err))
	}

	if !manual {
		predicate, present, err := ParseCondition(automation.Condition)
		if err != nil {
			return refuse(fmt.Sprintf("condition error: %v", err))
		}
		if present {
			matched, err := def.EvaluatePredicate(predicate, seeds)
			if err != nil {
				return refuse(fmt.Sprintf("condition error: %v", err))
			}
			if !matched {
				return refuse("condition false")
			}
		}
	}

	itemID, err := s.start(automation, automationGoal(automation, fire), encoded)
	if err != nil {
		if manual {
			return "", fmt.Errorf("run automation %q now: %w", automation.Name, err)
		}
		return refuse(fmt.Sprintf("start failed: %v", err))
	}
	if err := s.store.RecordAutomationFire(automation.ID, fire.At.UnixMilli(), itemID); err != nil {
		// The run exists and is the caller's answer; only its receipt is missing.
		return itemID, fmt.Errorf("workflow scheduler: record fire for automation %s: %w", automation.ID, err)
	}
	return itemID, nil
}

func describeRun(run store.AutomationRun) string {
	if run.Reason != "" {
		return fmt.Sprintf("%s(%s)", run.State, run.Reason)
	}
	return run.State
}

// runSeeds builds the variable context a fired run starts with: the
// automation's stored seeds, then the two reserved names bound last so a stored
// seed can never shadow them. It returns both the decoded map (what a run-if
// condition is evaluated against) and its encoding (what the run is seeded
// with), so the condition can never be judged against a different context than
// the one the run gets.
func runSeeds(automation store.Automation, fire Fire) (map[string]any, json.RawMessage, error) {
	values := make(map[string]any)
	if trimmed := bytes.TrimSpace(automation.Seeds); len(trimmed) > 0 && string(trimmed) != "null" {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&values); err != nil {
			return nil, nil, fmt.Errorf("seeds must be an object: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, nil, fmt.Errorf("seeds must contain one JSON object")
		}
		if values == nil {
			return nil, nil, fmt.Errorf("seeds must be an object")
		}
	}
	values[TriggerVariable] = fire.Context()
	values[JobNotesVariable] = automation.Notes
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, nil, fmt.Errorf("encode seeds: %w", err)
	}
	return values, encoded, nil
}

// automationGoal names the run for every surface that lists runs: which
// automation started it and what fired.
func automationGoal(automation store.Automation, fire Fire) string {
	name := strings.TrimSpace(automation.Name)
	if name == "" {
		name = "Automation " + automation.ID
	}
	return fmt.Sprintf("%s (%s)", name, fire.Summary())
}
