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
// instead of waiting out the interval. Failures reach the caller as the
// one-sentence ForgeActionError (git.ExplainForgeError); the forge's own
// text stays in its chain for "Details".
// See docs/specs/pull-requests.md.

//ao:scope git:operate
//ao:route selected
func (a *App) ApprovePR(ctx context.Context, pr gitops.PRReference, headSHA string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return err
	}
	err := a.gitCore().ApprovePR(forgeapi.WithInteractive(ctx), pr, strings.TrimSpace(headSHA))
	return a.settlePRAction(ctx, pr, gitops.ActionApprove, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) UnapprovePR(ctx context.Context, pr gitops.PRReference, approvalID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return err
	}
	err := a.gitCore().UnapprovePR(forgeapi.WithInteractive(ctx), pr, strings.TrimSpace(approvalID))
	return a.settlePRAction(ctx, pr, gitops.ActionUnapprove, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) MergePR(ctx context.Context, pr gitops.PRReference, req gitops.MergeRequest) (gitops.MergeOutcome, error) {
	if a.shuttingDown.Load() {
		return gitops.MergeOutcome{}, ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return gitops.MergeOutcome{}, err
	}
	if err := req.Validate(); err != nil {
		return gitops.MergeOutcome{}, err
	}
	action := gitops.ActionMerge
	if req.AutoMerge {
		action = gitops.ActionAutoMerge
	}
	outcome, err := a.gitCore().MergePR(forgeapi.WithInteractive(ctx), pr, req)
	return outcome, a.settlePRAction(ctx, pr, action, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) CancelPRAutoMerge(ctx context.Context, pr gitops.PRReference) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return err
	}
	err := a.gitCore().CancelPRAutoMerge(forgeapi.WithInteractive(ctx), pr)
	return a.settlePRAction(ctx, pr, gitops.ActionAutoMerge, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) UpdatePRBranch(ctx context.Context, pr gitops.PRReference, headSHA string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return err
	}
	err := a.gitCore().UpdatePRBranch(forgeapi.WithInteractive(ctx), pr, strings.TrimSpace(headSHA))
	return a.settlePRAction(ctx, pr, gitops.ActionUpdate, err)
}

//ao:scope git:operate
//ao:route selected
func (a *App) MarkPRReady(ctx context.Context, pr gitops.PRReference) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return err
	}
	err := a.gitCore().MarkPRReady(forgeapi.WithInteractive(ctx), pr)
	return a.settlePRAction(ctx, pr, gitops.ActionReady, err)
}

// settlePRAction is the tail of every merge action: a failure is explained
// for the user, a success asks the PR's pump (when one is live) to poll
// the snapshot now. The poll's own failure does not fail the action: the
// forge accepted it, and the pump reports its read failure on its own
// frame.
func (a *App) settlePRAction(ctx context.Context, pr gitops.PRReference, action gitops.ForgeAction, err error) error {
	if err != nil {
		return gitops.ExplainForgeError(pr, action, err)
	}
	a.refreshPRSnapshot(ctx, pr)
	return nil
}

// refreshPRSnapshot polls a live pump's snapshot at once, for the caller
// whose action changed the PR. No pump (no pane is watching) means nothing
// to refresh.
func (a *App) refreshPRSnapshot(ctx context.Context, pr gitops.PRReference) {
	a.prUpdates.mu.Lock()
	pump := a.livePRUpdatePumpLocked(prUpdateKey(pr))
	a.prUpdates.mu.Unlock()
	if pump == nil {
		return
	}
	if err := a.requestPRPump(ctx, pump, &prPumpRequest{snapshot: true}); err != nil && !errors.Is(err, errPRUpdatesUnknownSubscription) && !errors.Is(err, context.Canceled) {
		log.Printf("pr updates: refresh after action failed for pr=%s: %v", prUpdateKey(pr), err)
	}
}
