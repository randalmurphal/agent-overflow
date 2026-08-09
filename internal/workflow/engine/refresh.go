package engine

import (
	"encoding/json"
	"fmt"
	"log"

	"agent-overflow/internal/workflow/def"
)

// Re-reading a run's definition from disk.
//
// A run freezes its whole resolved definition — prompt file contents inlined —
// at start, and every later attempt renders from that snapshot. The freeze is
// deliberate (§3a): the phases a run has left keep the definition it started
// under, and the designed channel for picking up an edit is the call edge, which
// resolves its target fresh on every invocation — so a campaign's next wave runs
// the edited workflow while the wave in flight finishes under the one it began
// with.
//
// A run parked for OPERATOR REPAIR has no such channel. An operator who edits
// the prompt of a phase that parked `stuck` and resumes gets the frozen prompt
// rendered again, and the same park a second time. A refresh is that channel,
// and it is deliberately available only where a phase is entered FRESH: a
// continuation continues an attempt whose units — whole called runs among them —
// were launched under the frozen definition, and swapping the definition under
// it would render one half of an attempt from each.

// definitionRefreshNote rides the feedback of the attempt a refresh produces. It
// is the only place the next turn learns that its instructions are not the ones
// its predecessor read; the log line below is for the human reading diagnostics.
const definitionRefreshNote = "the workflow definition and its prompt files were re-read from disk for this attempt, so this phase's instructions may differ from the previous attempt's"

// resolveDefinition resolves the run's workflow from disk and encodes the
// snapshot it would freeze, WITHOUT writing anything. Every refusal a refresh
// can raise lands here, so a rejected refresh leaves the run record exactly as
// it was: an unresolvable definition, one with no phases, one that does not
// declare the phase this entry is aimed at, and one whose workspace the run
// cannot satisfy.
//
// `entryPhase` is the phase the caller is about to enter, or empty when the
// caller has none yet (a run that never froze a definition enters the new
// workflow's first phase, whatever it is now called).
func (e *Engine) resolveDefinition(item *runtimeItem, entryPhase, action string) (Snapshot, json.RawMessage, error) {
	itemID := item.item.ID
	resolved, err := e.definitions.Resolve(e.ctx, item.item)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("%s %q: %w", action, itemID, err)
	}
	if len(resolved.Workflow.Phases) == 0 {
		return Snapshot{}, nil, fmt.Errorf("%s %q: workflow has no phases", action, itemID)
	}
	if entryPhase != "" {
		if _, ok := findPhase(resolved.Workflow, entryPhase); !ok {
			return Snapshot{}, nil, fmt.Errorf(
				"%s %q: phase %q is not in the workflow on disk; the definition the run froze still declares it, so this action works without re-reading the definition, and a phase the edit renamed has to be entered under its new id",
				action, itemID, entryPhase,
			)
		}
	}
	if err := e.checkRefreshedWorkspace(item, resolved.WorkspaceNeed, action); err != nil {
		return Snapshot{}, nil, err
	}
	snapshot := Snapshot{Workflow: resolved.Workflow, WorkspaceNeed: resolved.WorkspaceNeed}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("%s %q snapshot: %w", action, itemID, err)
	}
	if len(encoded) > MaxSnapshotBytes {
		return Snapshot{}, nil, fmt.Errorf("%s %q snapshot is %d bytes; maximum is %d", action, itemID, len(encoded), MaxSnapshotBytes)
	}
	return snapshot, encoded, nil
}

// checkRefreshedWorkspace refuses a re-read definition whose workspace need the
// run cannot satisfy (§9).
//
// Provisioning is lazy and idempotent — the first phase to run under a
// `worktree` need cuts one — so a ROOT run that has none can acquire the
// worktree a newly-writing definition needs, exactly as its own first phase
// would have, and nothing is lost doing so: a run under `project-root-read-only`
// has no writing phase, so it has written nothing to abandon.
//
// What a run cannot do is give a worktree back. Its work lives in that checkout,
// and a definition that no longer needs one would run every remaining phase in
// the project root instead. A CALLED run cannot acquire one either: it executes
// in the workspace its tree's ROOT froze, and nothing here re-freezes the root's
// decision — a writing phase would run in whatever the root provisioned.
func (e *Engine) checkRefreshedWorkspace(item *runtimeItem, need def.WorkspaceNeed, action string) error {
	if item.workspaceNeed == "" {
		// The run never froze a need, so there is nothing for this one to be
		// incompatible with: it is about to start for the first time.
		return nil
	}
	if need == item.workspaceNeed {
		return nil
	}
	itemID := item.item.ID
	if need == def.WorkspaceProjectRoot {
		if item.item.WorktreePath == "" {
			return nil
		}
		return fmt.Errorf(
			"%s %q: the workflow on disk needs %s, but this run's work lives in the worktree it provisioned at %q; re-reading the definition would run the rest of the run in the project root",
			action, itemID, need, item.item.WorktreePath,
		)
	}
	if item.item.ParentItemID != "" {
		return fmt.Errorf(
			"%s %q: the workflow on disk needs %s, but this run was called by %s and executes in that tree's workspace, which was provisioned for %s",
			action, itemID, need, item.item.ParentItemID, item.workspaceNeed,
		)
	}
	return nil
}

// freezeSnapshot persists a resolved definition onto the run record and adopts
// it in memory. `startedAt` is the caller's decision: a run that never really
// started stamps its start here, while a re-read of a mid-flight run keeps the
// one its budget is measured against.
func (e *Engine) freezeSnapshot(item *runtimeItem, snapshot Snapshot, encoded json.RawMessage, startedAt int64) error {
	if err := e.store.UpdateWorkItemRunStart(
		item.item.ID, encoded, item.item.WorktreePath, item.item.Branch,
		item.item.BaseBranch, startedAt,
	); err != nil {
		return err
	}
	item.adoptSnapshot(snapshot)
	item.item.Snapshot = encoded
	item.item.StartedAt = startedAt
	return nil
}

// noteDefinitionRefresh records the re-read where both audiences can see it: the
// attempt's own feedback, which is what the next turn reads, and the engine log,
// which is where a human reconstructing "why did this attempt run something
// else" looks. The run record needs no column for it — the snapshot it now
// carries IS the durable evidence.
func (e *Engine) noteDefinitionRefresh(item *runtimeItem, entryPhase string) {
	if item.feedback == nil {
		item.feedback = &Feedback{}
	}
	if item.feedback.Note != "" {
		item.feedback.Note += "\n"
	}
	item.feedback.Note += definitionRefreshNote
	log.Printf(
		"workflow definition refresh %s: workflow %s re-read from disk for a fresh entry into phase %s (workspace need %s)",
		item.item.ID, item.workflow.ID, entryPhase, item.workspaceNeed,
	)
}
