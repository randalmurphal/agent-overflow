package app

import (
	"context"
	"errors"
	"log"
	"strings"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The merge actions of the review pane's Merge section: approve,
// unapprove, merge now or arm auto-merge, cancel auto-merge, update the
// branch, mark ready. Each is the user's own action (interactive), is
// validated here, runs through git.Core, and on success asks the PR's
// pump to poll at once so every pane on the PR follows the forge's answer
// instead of waiting out the interval. The answer carries the pump's
// sequence after that poll (PRActionResult.Seq), so a pane knows when the
// forge's answer is on its screen. Failures reach the caller as the
// one-sentence ForgeActionError (git.ExplainForgeError); the forge's own
// text stays in its chain for "Details".
// See docs/specs/pull-requests.md.

// PRActionResult is a merge action's answer. Seq is the PR pump's sequence
// once the poll the action asked for ran: the frame that carried the
// forge's answer has that sequence, or a smaller one when the poll found
// the snapshot unchanged and emitted nothing, so a pane whose applied
// sequence has reached Seq shows the forge's answer. Zero when no pane
// watches the PR. Outcome is set by MergePR.
type PRActionResult struct {
	Seq     uint64              `json:"seq"`
	Outcome gitops.MergeOutcome `json:"outcome"`
}

//ao:scope git:operate
//ao:route selected
func (a *App) ApprovePR(ctx context.Context, pr gitops.PRReference, headSHA string) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().ApprovePR(forgeapi.WithInteractive(ctx), pr, strings.TrimSpace(headSHA))
	return a.settlePRAction(ctx, pr, gitops.ActionApprove, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) UnapprovePR(ctx context.Context, pr gitops.PRReference) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().UnapprovePR(forgeapi.WithInteractive(ctx), pr)
	return a.settlePRAction(ctx, pr, gitops.ActionUnapprove, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) MergePR(ctx context.Context, pr gitops.PRReference, req gitops.MergeRequest) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	if err := req.Validate(); err != nil {
		return PRActionResult{}, err
	}
	action := gitops.ActionMerge
	if req.AutoMerge {
		action = gitops.ActionAutoMerge
	}
	outcome, err := a.gitCore().MergePR(forgeapi.WithInteractive(ctx), pr, req)
	result, err := a.settlePRAction(ctx, pr, action, err)
	result.Outcome = outcome
	return result, err
}

//ao:scope git:operate
//ao:route selected
func (a *App) CancelPRAutoMerge(ctx context.Context, pr gitops.PRReference) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().CancelPRAutoMerge(forgeapi.WithInteractive(ctx), pr)
	return a.settlePRAction(ctx, pr, gitops.ActionAutoMerge, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) UpdatePRBranch(ctx context.Context, pr gitops.PRReference, headSHA string) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().UpdatePRBranch(forgeapi.WithInteractive(ctx), pr, strings.TrimSpace(headSHA))
	return a.settlePRAction(ctx, pr, gitops.ActionUpdate, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) MarkPRReady(ctx context.Context, pr gitops.PRReference) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().MarkPRReady(forgeapi.WithInteractive(ctx), pr)
	return a.settlePRAction(ctx, pr, gitops.ActionReady, err)
}

// admitPRAction is the gate every merge action passes before touching a
// forge: the app is running and the reference is well formed.
func (a *App) admitPRAction(pr gitops.PRReference) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return pr.Validate()
}

// settlePRAction is the tail of every merge action: a failure is explained
// for the user, a success asks the PR's pump (when one is live) to poll
// the snapshot now and answers with the pump's sequence after that poll.
// A merge whose branch delete failed changed the PR, so it refreshes too.
// The poll's own failure does not fail the action: the forge accepted it,
// and the pump reports its read failure on its own channel, where the
// pane shows it.
func (a *App) settlePRAction(ctx context.Context, pr gitops.PRReference, action gitops.ForgeAction, err error) (PRActionResult, error) {
	if err != nil {
		if _, merged := errors.AsType[*gitops.BranchKeptError](err); merged {
			a.refreshPRSnapshot(ctx, pr)
		}
		return PRActionResult{}, gitops.ExplainForgeError(pr, action, err)
	}
	return PRActionResult{Seq: a.refreshPRSnapshot(ctx, pr)}, nil
}

// refreshPRSnapshot polls a live pump's snapshot at once, for the caller
// whose action changed the PR, and reports the pump's sequence once the
// poll ran. No pump (no pane is watching) means nothing to refresh and
// sequence zero.
func (a *App) refreshPRSnapshot(ctx context.Context, pr gitops.PRReference) uint64 {
	a.prUpdates.mu.Lock()
	pump := a.livePRUpdatePumpLocked(prUpdateKey(pr))
	a.prUpdates.mu.Unlock()
	if pump == nil {
		return 0
	}
	if err := a.requestPump(ctx, &pump.pumpCore, &prPumpRequest{snapshot: true}); err != nil && !errors.Is(err, errPRUpdatesUnknownSubscription) && !errors.Is(err, context.Canceled) {
		log.Printf("pr updates: refresh after action failed for pr=%s: %v", prUpdateKey(pr), err)
	}
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	return pump.seq
}
