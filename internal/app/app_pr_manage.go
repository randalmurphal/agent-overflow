package app

import (
	"context"
	"sync"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The management actions of the review pane: edit the title and
// description, change reviewers, assignees and labels, close, reopen or
// convert to draft, re-request a review, dismiss (GitHub) or bypass
// (GitLab) a review that requested changes. Each follows the merge
// actions' contract (app_forge_merge.go): the user's own action
// (interactive), validated in git.Core, one at a time per PR
// (runPRManage), and on success the PR's pump
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
	return a.runPRManage(ctx, pr, gitops.ActionEdit, func(ctx context.Context) error {
		return a.gitCore().EditPR(ctx, pr, edit)
	})
}

// SetPRReviewers removes and adds reviewers in one change.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRReviewers(ctx context.Context, pr gitops.PRReference, change gitops.PRPeopleChange) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionReviewers, func(ctx context.Context) error {
		return a.gitCore().SetPRPeople(ctx, pr, true, change)
	})
}

// SetPRAssignees removes and adds assignees in one change.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRAssignees(ctx context.Context, pr gitops.PRReference, change gitops.PRPeopleChange) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionAssignees, func(ctx context.Context) error {
		return a.gitCore().SetPRPeople(ctx, pr, false, change)
	})
}

// SetPRLabels removes and adds labels in one change.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRLabels(ctx context.Context, pr gitops.PRReference, change gitops.PRLabelChange) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionLabels, func(ctx context.Context) error {
		return a.gitCore().SetPRLabels(ctx, pr, change)
	})
}

// SetPRState closes ("close"), reopens ("reopen") or converts the PR to a
// draft ("draft").
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRState(ctx context.Context, pr gitops.PRReference, state string) (PRActionResult, error) {
	if err := gitops.ValidatePRStateChange(state); err != nil {
		return PRActionResult{}, err
	}
	action := map[string]gitops.ForgeAction{gitops.PRStateClose: gitops.ActionClose, gitops.PRStateReopen: gitops.ActionReopen, gitops.PRStateToDraft: gitops.ActionToDraft}[state]
	return a.runPRManage(ctx, pr, action, func(ctx context.Context) error {
		return a.gitCore().SetPRState(ctx, pr, state)
	})
}

// RerequestPRReview asks one reviewer for a new review.
//
//ao:scope git:operate
//ao:route selected
func (a *App) RerequestPRReview(ctx context.Context, pr gitops.PRReference, reviewer gitops.PRPerson) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionRerequest, func(ctx context.Context) error {
		return a.gitCore().RerequestPRReview(ctx, pr, reviewer)
	})
}

// DismissPRReview dismisses a GitHub review that requested changes
// (a ChangeRequest's ReviewID) with the reason GitHub requires.
//
//ao:scope git:operate
//ao:route selected
func (a *App) DismissPRReview(ctx context.Context, pr gitops.PRReference, reviewID, message string) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionDismiss, func(ctx context.Context) error {
		return a.gitCore().DismissPRReview(ctx, pr, reviewID, message)
	})
}

// BypassPRRequestedChanges overrides the requested changes blocking a
// GitLab merge request.
//
//ao:scope git:operate
//ao:route selected
func (a *App) BypassPRRequestedChanges(ctx context.Context, pr gitops.PRReference) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionBypass, func(ctx context.Context) error {
		return a.gitCore().BypassPRRequestedChanges(ctx, pr)
	})
}

// runPRManage is every management write: admitted as the merge actions
// are, run as the user's own (interactive) while holding the PR's
// management lock, so a second write of the same PR waits and then reads
// what the first left (a description edit's precondition sees it), and
// settled as the merge actions are. The lock is released before the
// settle's refresh.
func (a *App) runPRManage(ctx context.Context, pr gitops.PRReference, action gitops.ForgeAction, write func(context.Context) error) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	release, err := a.prManage.acquire(ctx, pr.Identity())
	if err != nil {
		return PRActionResult{}, err
	}
	err = write(forgeapi.WithInteractive(ctx))
	release()
	return a.settlePRAction(ctx, pr, action, err)
}

// prManageLocks is one lock per PR for its management writes. A caller
// waits for the holder, bounded by its context; an entry lives while it
// is held or awaited.
type prManageLocks struct {
	mu      sync.Mutex
	entries map[string]*prManageLock
	// woke, when set, runs once a caller's wait ended, before its context
	// is checked again: a test's way to end the context at that moment.
	woke func()
}

type prManageLock struct {
	slot  chan struct{}
	users int
}

// acquire takes key's lock, or fails with ctx's error; release gives it
// back and forgets the entry once nobody holds or awaits it. A context
// that ended takes no lock, also when it ended as the slot came free:
// the wait can wake on either, so the context is checked after it too.
func (l *prManageLocks) acquire(ctx context.Context, key string) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.entries == nil {
		l.entries = map[string]*prManageLock{}
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &prManageLock{slot: make(chan struct{}, 1)}
		l.entries[key] = entry
	}
	entry.users++
	l.mu.Unlock()
	leave := func() {
		l.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
	select {
	case entry.slot <- struct{}{}:
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
	if l.woke != nil {
		l.woke()
	}
	if err := ctx.Err(); err != nil {
		<-entry.slot
		leave()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-entry.slot
			leave()
		})
	}, nil
}

// users is how many callers hold or await key's lock.
func (l *prManageLocks) users(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.entries[key]; entry != nil {
		return entry.users
	}
	return 0
}

// size is the number of PRs with a held or awaited lock.
func (l *prManageLocks) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
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
