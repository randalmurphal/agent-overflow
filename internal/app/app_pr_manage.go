package app

import (
	"context"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The management actions of the review pane: edit the title and
// description, change reviewers, assignees and labels, close, reopen or
// convert to draft, re-request a review, dismiss (GitHub) or bypass
// (GitLab) a review that requested changes. Each follows the merge
// actions' contract (app_forge_merge.go): the user's own action
// (interactive), validated in git.Core, and on success the PR's pump
// polls at once and the answer carries its sequence after that poll
// (PRActionResult.Seq). A change the forge may have applied is not sent
// again: the pump polls all the same and the failure says so. The two
// reads outside the tick, a picker's search and the default merge commit
// message, are their own methods: their failure is theirs alone and
// never touches the PR's read. See docs/architecture/forge-transport.md,
// "Managing a PR".

// EditPR changes the title and/or the description; a description edit
// names the description it replaces (git.PREdit).
//
//ao:scope git:operate
//ao:route selected
func (a *App) EditPR(ctx context.Context, pr gitops.PRReference, edit gitops.PREdit) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().EditPR(forgeapi.WithInteractive(ctx), pr, edit)
	return a.settlePRAction(ctx, pr, gitops.ActionEdit, err)
}

// SetPRReviewers adds and removes reviewers in one change.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRReviewers(ctx context.Context, pr gitops.PRReference, change gitops.PRPeopleChange) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().SetPRPeople(forgeapi.WithInteractive(ctx), pr, true, change)
	return a.settlePRAction(ctx, pr, gitops.ActionReviewers, err)
}

// SetPRAssignees adds and removes assignees in one change.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRAssignees(ctx context.Context, pr gitops.PRReference, change gitops.PRPeopleChange) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().SetPRPeople(forgeapi.WithInteractive(ctx), pr, false, change)
	return a.settlePRAction(ctx, pr, gitops.ActionAssignees, err)
}

// SetPRLabels adds and removes labels in one change.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRLabels(ctx context.Context, pr gitops.PRReference, change gitops.PRLabelChange) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().SetPRLabels(forgeapi.WithInteractive(ctx), pr, change)
	return a.settlePRAction(ctx, pr, gitops.ActionLabels, err)
}

// SetPRState closes ("close"), reopens ("reopen") or converts the PR to a
// draft ("draft").
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRState(ctx context.Context, pr gitops.PRReference, state string) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	if err := gitops.ValidatePRStateChange(state); err != nil {
		return PRActionResult{}, err
	}
	action := map[string]gitops.ForgeAction{gitops.PRStateClose: gitops.ActionClose, gitops.PRStateReopen: gitops.ActionReopen, gitops.PRStateToDraft: gitops.ActionToDraft}[state]
	err := a.gitCore().SetPRState(forgeapi.WithInteractive(ctx), pr, state)
	return a.settlePRAction(ctx, pr, action, err)
}

// RerequestPRReview asks one reviewer for a new review.
//
//ao:scope git:operate
//ao:route selected
func (a *App) RerequestPRReview(ctx context.Context, pr gitops.PRReference, reviewer gitops.PRPerson) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().RerequestPRReview(forgeapi.WithInteractive(ctx), pr, reviewer)
	return a.settlePRAction(ctx, pr, gitops.ActionRerequest, err)
}

// DismissPRReview dismisses a GitHub review that requested changes
// (a ChangeRequest's ReviewID) with the reason GitHub requires.
//
//ao:scope git:operate
//ao:route selected
func (a *App) DismissPRReview(ctx context.Context, pr gitops.PRReference, reviewID, message string) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().DismissPRReview(forgeapi.WithInteractive(ctx), pr, reviewID, message)
	return a.settlePRAction(ctx, pr, gitops.ActionDismiss, err)
}

// BypassPRRequestedChanges overrides the requested changes blocking a
// GitLab merge request.
//
//ao:scope git:operate
//ao:route selected
func (a *App) BypassPRRequestedChanges(ctx context.Context, pr gitops.PRReference) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().BypassPRRequestedChanges(forgeapi.WithInteractive(ctx), pr)
	return a.settlePRAction(ctx, pr, gitops.ActionBypass, err)
}

// SearchPRCandidates is one picker search: kind "reviewers",
// "assignees" or "labels", the first page matching query.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SearchPRCandidates(ctx context.Context, pr gitops.PRReference, kind, query string) (gitops.PRCandidates, error) {
	if err := a.admitPRAction(pr); err != nil {
		return gitops.PRCandidates{}, err
	}
	out, err := a.gitCore().SearchPRCandidates(forgeapi.WithInteractive(ctx), pr, kind, query)
	if err != nil {
		return gitops.PRCandidates{}, gitops.ExplainForgeError(pr, gitops.ActionSearch, err)
	}
	return out, nil
}

// GetPRMergeMessage reads the forge's default commit message of a merge
// with method ("merge" or "squash").
//
//ao:scope git:operate
//ao:route selected
func (a *App) GetPRMergeMessage(ctx context.Context, pr gitops.PRReference, method string) (gitops.MergeMessage, error) {
	if err := a.admitPRAction(pr); err != nil {
		return gitops.MergeMessage{}, err
	}
	out, err := a.gitCore().PRMergeMessage(forgeapi.WithInteractive(ctx), pr, method)
	if err != nil {
		return gitops.MergeMessage{}, gitops.ExplainForgeError(pr, gitops.ActionMergeText, err)
	}
	return out, nil
}
