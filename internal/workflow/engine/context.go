package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// controlEnvelope is the flat control shape every phase envelope carries. The
// engine reads Status and Outputs; Reason is written when the engine has to
// synthesize an envelope for work that ran no turn of its own (a call phase's
// child outcome), and is decoded so those envelopes round-trip.
type controlEnvelope struct {
	Status  string         `json:"status"`
	Outputs map[string]any `json:"outputs"`
	Reason  string         `json:"reason,omitempty"`
}

// attemptRef identifies the phase attempt a variable context is being built
// FOR, and carries that attempt's envelope when it has produced one.
//
// It is passed rather than read off the runtimeItem because the two disagree at
// a fresh phase entry: `item.attempt` there still holds the attempt the run is
// leaving, and the attempt being entered has no row yet. Reading the item would
// therefore exclude a real prior attempt from that phase's history binding —
// which is exactly the round a re-entered phase most needs to see. The zero
// value means "no current attempt" and matches no row, since attempts start
// at 1.
type attemptRef struct {
	phaseID  string
	attempt  int
	envelope json.RawMessage
}

// currentAttempt is the attemptRef of the attempt an item is resident in, for
// the paths that build a context for a live or parked attempt rather than for
// one being entered.
func (i *runtimeItem) currentAttempt(envelope json.RawMessage) attemptRef {
	return attemptRef{phaseID: i.phaseID, attempt: i.attempt, envelope: envelope}
}

// matches reports whether a persisted attempt row IS the current attempt.
func (a attemptRef) matches(phase store.WorkItemPhaseContext) bool {
	return a.attempt > 0 && a.phaseID == phase.PhaseID && a.attempt == phase.Attempt
}

func (e *Engine) variableContext(item *runtimeItem, current attemptRef) (map[string]any, []store.WorkItemPhaseContext, error) {
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
	if len(current.envelope) > 0 {
		if err := addOutputs(vars, current.phaseID, current.envelope); err != nil {
			return nil, nil, fmt.Errorf("decode current phase %s/%s/%d: %w", item.item.ID, current.phaseID, current.attempt, err)
		}
	}
	// Bound last, and under the names def declares to the prompt validator, so an
	// authored variable can never shadow a phase's own attempt history.
	if err := bindPhaseHistory(vars, item, current, phases); err != nil {
		return nil, nil, err
	}
	// The reserved call-depth read, bound last for the same reason: a seed of the
	// same name must not be able to tell a wave it is a different wave. The value
	// is the run row's own depth (0 for a directly started run), which is the one
	// place the ordinal exists and the same number campaign memory stamps a note's
	// `wave` provenance from. Declaring it is a validation finding, so nothing an
	// author wrote is being replaced here.
	vars[def.CallDepthVariable] = item.item.CallDepth
	e.bindBudget(vars, item)
	return vars, phases, nil
}

// bindBudget binds the reserved `budget` read: the ceiling in force for this
// run's TREE and where the tree stands against it, from the same
// `ResolveBudget` the enforcement calls — so a prompt can never quote a number
// that would not park the run.
//
// Absence is a real state and is bound as one: a run under no ceiling leaves
// the name unset, and `{{budget}}` renders the "(not provided)" every absent
// optional input renders. The alternative — a zero ceiling, or a `present:
// false` object — asks every prompt that reads it to special-case a shape.
//
// A read that cannot be answered leaves the name unbound too, and says so
// through the log. This binding is PROMPT SURFACE — `def` refuses it in a gate
// predicate — so a ledger that would not answer (a locked database, a profile
// that could not be read) must degrade exactly as an absent ceiling does, never
// fail the context it is one field of. It used to: a variable context is built
// at gate evaluation as well as at phase entry, so a transient spend-source
// error there marked an attempt that had COMPLETED as parked and threw away the
// advance its gate had already decided, with no continuation that could repair
// it. Enforcement is the loud half and stays loud: `checkBudget` refuses a
// ceiling it cannot judge, and refuses it before the phase starts.
//
// Cost: a run WITH a ceiling pays one tree-spend aggregate per context build,
// on top of the one its phase-entry budget check already runs. A run without
// one pays a root lookup and the profile read the engine does at every
// acquisition anyway, and never touches the ledger.
func (e *Engine) bindBudget(vars map[string]any, item *runtimeItem) {
	view, err := e.budgetView(item)
	if err != nil {
		e.logEvent(LogEvent{
			Event: LogEventBudgetUnread, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
			PhaseID: item.phaseID, Attempt: item.attempt,
			Message: fmt.Sprintf(
				"the {{budget}} binding is unbound for this context: the ceiling could not be read (%v)", err),
		})
		return
	}
	if view == nil {
		return
	}
	vars[def.BudgetVariable] = renderBudgetBinding(*view)
}

// renderBudgetBinding is what an element sees. It carries the four numbers a
// prompt can act on and nothing else: which ceiling is in force, what the tree
// has spent, what is left, and whether the spend figure was partly priced from
// a rate table rather than reported by a provider (Codex reports tokens only).
//
// Tokens and dollars render as numbers; a wall clock renders as a duration
// string, because "1h24m48s" is what a model can reason about and a millisecond
// count is not.
func renderBudgetBinding(view BudgetView) map[string]any {
	binding := map[string]any{"kind": view.Kind}
	switch view.Kind {
	case BudgetKindTokens:
		binding["ceiling"] = view.CeilingTokens
		binding["spent"] = view.Spend.Tokens
		binding["remaining"] = max(view.CeilingTokens-view.Spend.Tokens, 0)
		// Token counts are exact whatever the rate table knows, so a token
		// ceiling is never estimated — saying otherwise would put a caveat on the
		// one budget kind that never needs one.
		binding["estimated"] = false
	case BudgetKindUSD:
		binding["ceiling"] = view.CeilingUSD
		binding["spent"] = view.Spend.USD
		binding["remaining"] = max(view.CeilingUSD-view.Spend.USD, 0)
		binding["estimated"] = view.Spend.Estimated
	case BudgetKindWallClock:
		ceiling := time.Duration(view.CeilingMillis) * time.Millisecond
		elapsed := view.Elapsed()
		binding["ceiling"] = ceiling.String()
		binding["spent"] = elapsed.Round(time.Second).String()
		binding["remaining"] = max(ceiling-elapsed, 0).Round(time.Second).String()
		binding["estimated"] = false
	}
	return binding
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
