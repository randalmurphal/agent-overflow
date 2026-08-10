package main

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-overflow/internal/workflow/engine"
)

// `agent-overflow run amend --seed k=v` (D53): changing a resting run's inputs
// without throwing the run away.
//
// It exists because the alternative was priced: one seed value wrong on a live
// campaign cost a cancel, a respawn, and $14 of tokens, because no verb could
// change a seed and the repair pattern that works for a prompt (edit the file,
// `run resume --refresh-def`, D50) had no equivalent for an input.
//
// The engine owns every rule — which states may be amended, which names are
// amendable, and when the run will read the new value — because all three are
// facts about the run's own frozen definition and its place in the FSM. This
// file adds what the engine has no business knowing: who is allowed to ask, and
// what a caller has to be told about a run that is not the root of its tree.

// WorkflowAgentAmendSeedsInput is `run amend`. Seeds is a partial object: only
// the named keys change, every other seed the run froze is left alone. Clearing
// a seed is deliberately not expressible — an absent optional input and one
// explicitly set to null are different runs, and "remove this key" is a verb
// nobody has asked for.
type WorkflowAgentAmendSeedsInput struct {
	ItemID string          `json:"itemId"`
	Seeds  json.RawMessage `json:"seeds"`
}

// WorkflowAgentAmendSeedsResult is what the amendment did. Effect is the
// engine's own answer to "when is this read", and AppliesNote states it in the
// words the operator needs; CallerNote is present only for a called run.
type WorkflowAgentAmendSeedsResult struct {
	ItemID  string          `json:"itemId"`
	Names   []string        `json:"names"`
	Seeds   json.RawMessage `json:"seeds"`
	PhaseID string          `json:"phaseId,omitempty"`
	Effect  string          `json:"effect"`
	// AppliesNote is one sentence saying when the run reads the new values and,
	// where it matters, which verb makes it read them sooner. It is composed here
	// rather than by the CLI because the answer depends on the run record, and a
	// caller that had to derive it would be deriving the engine's dispatch.
	AppliesNote string `json:"appliesNote"`
	// CallerNote is set when the amended run was CALLED by another. Its seeds are
	// its own — its remaining phases read this row — but the next run its caller
	// invokes re-evaluates the caller's `args:` and will not carry this change.
	CallerNote string `json:"callerNote,omitempty"`
}

// WorkflowAgentAmendSeeds changes seed values on a run that is resting.
//
// LocalOnly: it mutates what autonomous provider sessions will be told to do,
// which is the same authority as resuming the run.
func (a *App) WorkflowAgentAmendSeeds(ctx context.Context, input WorkflowAgentAmendSeedsInput) (WorkflowAgentAmendSeedsResult, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	if err := a.authorizeScopedRunAction(ctx, input.ItemID, "amend workflow run seeds"); err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	values, _, err := decodeWorkflowSeeds(input.Seeds)
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, fmt.Errorf("amend workflow run seeds: %w", err)
	}
	amendment, err := workflowEngine.AmendSeeds(input.ItemID, values)
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	result := WorkflowAgentAmendSeedsResult{
		ItemID: amendment.ItemID, Names: amendment.Names, Seeds: amendment.Seeds,
		PhaseID: amendment.PhaseID, Effect: string(amendment.Effect),
		AppliesNote: workflowAmendmentNote(amendment),
	}
	// The row is re-read rather than trusted from before the write: the caller
	// note is about linkage, which the amendment does not carry, and a run that
	// was called is the one case where "amend the root instead" is the more
	// useful half of the answer.
	item, err := a.store.GetWorkItem(amendment.ItemID)
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	if item.ParentItemID != "" {
		chain, err := a.workflowAncestry(item)
		if err != nil {
			return WorkflowAgentAmendSeedsResult{}, err
		}
		result.CallerNote = fmt.Sprintf(
			"this run was called by %s (root %s), so the change reaches its own remaining phases only; the next run %s starts re-evaluates its call arguments and will not carry it — amend %s to change what later waves are given",
			item.ParentItemID, chain[0].ID, item.ParentItemID, chain[0].ID)
	}
	return result, nil
}

// workflowAmendmentNote states when the run reads what was just written, in the
// terms the operator's next command is expressed in.
//
// Both answers are true statements about the same mechanism: seeds live on the
// run row and the variable context is rebuilt from it whenever a phase attempt
// starts. What differs is whether a bare resume of THIS park starts one — a
// fan-out or a call phase is repaired in place and runs on the variables its
// attempt persisted, so its operator needs to know that re-entering the phase is
// what makes the new value take.
func workflowAmendmentNote(amendment engine.SeedAmendment) string {
	if amendment.Effect == engine.SeedEffectFreshEntry {
		return fmt.Sprintf(
			"the parked attempt of phase %q is repaired in place by a bare resume and keeps the values it froze; the new values are read at the next FRESH phase entry — `agent-overflow run resume %s --phase %s` enters one now, and the run's next phase does so on its own",
			amendment.PhaseID, amendment.ItemID, amendment.PhaseID)
	}
	return "the next attempt this run starts renders the new values; if it continues a provider session that already ran with the old ones, say so in the resume — that session's earlier turns are unchanged"
}
