package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/transport"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/engine"
)

// `agent-overflow run guide <run-id> "<text>"` — steering a run without parking
// it.
//
// It is the thread→run direction of `notify:` (D54): a `notify:` route tells the
// watching thread what a run just decided, and this tells the run what the
// watcher wants next. Before it existed, the only way to redirect a free-running
// campaign was `run pause` → edit → `run resume`, which costs the turn in flight
// and, for a wave, the coordination of every unit under it.
//
// The engine owns every rule — which states may be guided, how many entries fit,
// which phase entry consumes them, and the author stamp. This file adds what the
// engine has no business knowing: who is allowed to ask, whose name goes on the
// entry, and what a caller has to be told about a run that is not the root of
// its tree.

// WorkflowAgentGuideRunInput is `run guide`. One entry per call: the slot is a
// list of instructions, and merging two calls into one entry would lose the
// order and the times they were left at.
type WorkflowAgentGuideRunInput struct {
	ItemID string `json:"itemId"`
	Text   string `json:"text"`
}

// WorkflowAgentGuideRunResult is what is now waiting for the run, and when the
// run will read it. Entries are not echoed back: the caller wrote the newest one
// and the older ones are the run's business, while `run inspect` is the read
// surface for the whole slot.
type WorkflowAgentGuideRunResult struct {
	ItemID string `json:"itemId"`
	// Pending is the slot depth after this entry, against MaxGuidanceEntries.
	Pending    int `json:"pending"`
	MaxPending int `json:"maxPending"`
	// By is the author the ENGINE stamped, echoed so a caller can see that the
	// attribution is not the one it typed — it is the one its credential earned.
	By      string `json:"by"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	PhaseID string `json:"phaseId,omitempty"`
	// DeliversNote is one sentence saying when the run reads this, in the terms
	// the operator's next command is expressed in. It is composed here rather
	// than by the CLI because the answer depends on where the run is resting.
	DeliversNote string `json:"deliversNote"`
	// CallerNote is set when the guided run was CALLED by another. The entry
	// reaches this run's own remaining phases and nothing else.
	CallerNote string `json:"callerNote,omitempty"`
	// QuarantineNote is set when this append landed on a slot the engine had to
	// heal: whatever was pending would not decode, so it was written to the
	// engine log and discarded. The call SUCCEEDED — the caller's entry is
	// pending — which is exactly why the fact has to travel on the result rather
	// than as an error nobody would see.
	QuarantineNote string `json:"quarantineNote,omitempty"`
}

// WorkflowAgentGuideRun leaves one instruction for a run's next phase entry.
//
// LocalOnly: it changes what an autonomous provider session will be told to do,
// which is the same authority as amending its seeds or resuming it.
func (a *App) WorkflowAgentGuideRun(ctx context.Context, input WorkflowAgentGuideRunInput) (WorkflowAgentGuideRunResult, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	if err := a.authorizeScopedRunAction(ctx, input.ItemID, "guide workflow run"); err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	state, err := workflowEngine.Guide(input.ItemID, guidanceDraftFor(ctx, input.Text))
	if err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	result := WorkflowAgentGuideRunResult{
		ItemID: state.ItemID, Pending: len(state.Pending), MaxPending: engine.MaxGuidanceEntries,
		State: string(state.State), Reason: string(state.Reason), PhaseID: state.PhaseID,
		DeliversNote:   workflowGuidanceNote(state),
		QuarantineNote: workflowGuidanceQuarantineNote(state.Quarantined),
	}
	if len(state.Pending) > 0 {
		result.By = string(state.Pending[len(state.Pending)-1].By)
	}
	// The row is re-read rather than trusted from before the write, for the same
	// reason `run amend` re-reads it: the caller note is about linkage, which the
	// guidance state does not carry.
	item, err := a.store.GetWorkItem(state.ItemID)
	if err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	if item.ParentItemID != "" {
		chain, err := a.workflowAncestry(item)
		if err != nil {
			return WorkflowAgentGuideRunResult{}, err
		}
		result.CallerNote = fmt.Sprintf(
			"this run was called by %s (root %s), so the guidance reaches its own remaining phases only; the next run %s starts is a different run and will not see it — guide that run when it exists, or %s to steer the caller's own phases",
			item.ParentItemID, chain[0].ID, item.ParentItemID, chain[0].ID)
	}
	return result, nil
}

// guidanceDraftFor stamps the author from the AUTHENTICATED caller, never from
// the request. A scoped phase session is an agent, an interactive session is the
// person driving it, and the distinction is the whole value of the label: an
// entry that could claim "a human said this" would make the attribution in the
// delivered prompt worth nothing.
func guidanceDraftFor(ctx context.Context, text string) engine.GuidanceDraft {
	draft := engine.GuidanceDraft{Text: text, By: engine.GuidanceByHuman}
	if scope, ok := transport.CallerScopeFrom(ctx); ok && scope.IsPhase() {
		draft.By, draft.ByRun = engine.GuidanceByPhase, scope.ItemID
	}
	return draft
}

// workflowGuidanceNote states when the run reads what was just left, which is a
// different sentence for each place a run can be resting.
//
// The trap it exists to close is the continuable park: a bare `run resume` of a
// `paused`, `interrupted`, `checkpoint`, `unit-failed`,
// `provider-retries-exhausted`, or legacy `retries-exhausted`
// park CONTINUES the attempt that parked instead of entering a phase, and a
// continuation is not a delivery boundary — the guidance would sit pending
// through a resume the operator reasonably expected to consume it. The verb that
// does enter a phase is named instead of left to be inferred. The sentence reads
// `engine.ContinuableReason` rather than restating the set, so a new member
// changes what this says without an edit here.
func workflowGuidanceNote(state engine.GuidanceState) string {
	switch {
	case state.State == engine.StateRunning:
		return fmt.Sprintf(
			"the run is working; this is delivered at its next FRESH phase entry, which is the next phase it advances or loops into — the turn in flight%s is never interrupted",
			workflowGuidancePhaseClause(state.PhaseID))
	case engine.ContinuableReason(state.Reason):
		return fmt.Sprintf(
			"the run is parked %s, which `agent-overflow run resume %s` CONTINUES rather than re-enters — a continuation is not a delivery boundary, so this is read at the next fresh phase entry after that, or immediately by `agent-overflow run resume %s --phase <id>`, which starts a phase over",
			state.Reason, state.ItemID, state.ItemID)
	default:
		// Which verb settles this park is the repair map's answer, not this
		// method's — a `gate` park takes `run resolve`, a `stuck` one takes `run
		// resume`, and naming the wrong one here would be worse than naming none.
		return fmt.Sprintf(
			"the run is parked %s%s; this is delivered at the fresh phase entry the verb that settles that park produces",
			state.Reason, workflowGuidancePhaseClause(state.PhaseID))
	}
}

// workflowGuidanceQuarantineNote says what this append cost, when it cost
// anything. The engine hands back facts (how big the discarded column was, why
// it would not decode, which log event holds it); the sentence is composed here
// for the same reason `workflowGuidanceNote` is — the caller reads prose, not a
// struct, and the CLI's job is to print what the app says rather than to know
// what a quarantine means.
//
// It names what the operator has to DO, because the discard is not repairable
// from the record: whatever was pending is in the log and not in the run, so any
// earlier steer that has not been delivered has to be left again.
func workflowGuidanceQuarantineNote(quarantine *engine.GuidanceQuarantine) string {
	if quarantine == nil {
		return ""
	}
	return fmt.Sprintf(
		"the guidance already pending on this run (%d bytes) could not be decoded (%s), so it was written to the engine log as %q and the slot was cleared before your entry was added; your entry is safe and is the only one pending, but any earlier steer that had not been delivered is gone — re-issue it",
		quarantine.Bytes, quarantine.Reason, quarantine.LogEvent)
}

func workflowGuidancePhaseClause(phaseID string) string {
	if phaseID == "" {
		return ""
	}
	return fmt.Sprintf(" (phase %q)", phaseID)
}

// WorkflowAgentGuidanceEntry is one pending entry as `run inspect` reports it.
// The text is bounded — the slot holds several kilobytes per entry and an
// inspection is read by an agent paying per byte — and the age is computed here
// rather than left as a timestamp for the reader to subtract, because "left four
// hours ago and still not delivered" is the fact the field exists for.
type WorkflowAgentGuidanceEntry struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
	// AgeSeconds is how long this entry has been waiting for a phase entry. It
	// can legitimately be zero (an entry left this second) and is never negative:
	// a clock that moved backwards clamps rather than reporting the future.
	AgeSeconds int64 `json:"ageSeconds"`
	// By is the author the engine stamped, and ByRun the run whose phase left it.
	By    string `json:"by"`
	ByRun string `json:"byRun,omitempty"`
}

// maxGuidanceEntryRunes bounds one entry's text in an inspection. It is wider
// than an output digest's cap because a steer is prose the reader has to act on,
// and narrower than the entry's own limit because eight of them at full length
// would be a read nobody asked for.
const maxGuidanceEntryRunes = 400

// workflowPendingGuidance decodes a run's pending-guidance slot. A slot that
// will not decode fails the read: the column is engine-written JSON, so
// unreadable content is corruption, and reporting it as "nothing pending" would
// tell an operator their instruction is gone when it is merely unreadable — the
// same rule workflowAgentPhaseAttempts applies to a gate trace.
func (a *App) workflowPendingGuidance(itemID string) ([]engine.GuidanceEntry, error) {
	raw, err := a.store.WorkItemPendingGuidance(itemID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var pending []engine.GuidanceEntry
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, fmt.Errorf("workflow run %s: pending guidance is unreadable: %w", itemID, err)
	}
	return pending, nil
}

// workflowGuidanceEntries projects the slot for an inspection, oldest first —
// the order they were left in and the order they will be delivered in.
func workflowGuidanceEntries(pending []engine.GuidanceEntry, now int64) []WorkflowAgentGuidanceEntry {
	entries := make([]WorkflowAgentGuidanceEntry, 0, len(pending))
	for _, entry := range pending {
		age := (now - entry.At) / 1000
		if age < 0 {
			age = 0
		}
		entries = append(entries, WorkflowAgentGuidanceEntry{
			Text:       untrustedtext.Truncate(entry.Text, maxGuidanceEntryRunes),
			At:         entry.At,
			AgeSeconds: age,
			By:         string(entry.By),
			ByRun:      entry.ByRun,
		})
	}
	return entries
}

// workflowInspectGuidance is the inspection's half: read the slot and project it
// against the wall clock.
func (a *App) workflowInspectGuidance(itemID string) ([]WorkflowAgentGuidanceEntry, error) {
	pending, err := a.workflowPendingGuidance(itemID)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	return workflowGuidanceEntries(pending, time.Now().UnixMilli()), nil
}
