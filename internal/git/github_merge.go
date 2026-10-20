package git

import "context"

// GitHub's merge actions (ForgeMerger). Approve is an approving review,
// Unapprove dismisses the viewer's approving review, Merge is
// mergePullRequest or enablePullRequestAutoMerge, CancelAutoMerge is
// disablePullRequestAutoMerge, UpdateBranch is updatePullRequestBranch
// and MarkReady is markPullRequestReadyForReview, all GraphQL. Readiness
// and MergeOptions are derived in github_read.go from mergeStateStatus,
// reviewDecision, the rollup, autoMergeRequest, the viewer flags and the
// repository's merge settings. See docs/specs/pull-requests.md.

func (f *githubForge) Approve(ctx context.Context, ref PRReference, headSHA string) error {
	return errForgeMergeNotImplemented
}

func (f *githubForge) Unapprove(ctx context.Context, ref PRReference, approvalID string) error {
	return errForgeMergeNotImplemented
}

func (f *githubForge) Merge(ctx context.Context, ref PRReference, req MergeRequest) (MergeOutcome, error) {
	return MergeOutcome{}, errForgeMergeNotImplemented
}

func (f *githubForge) CancelAutoMerge(ctx context.Context, ref PRReference) error {
	return errForgeMergeNotImplemented
}

func (f *githubForge) UpdateBranch(ctx context.Context, ref PRReference, headSHA string) error {
	return errForgeMergeNotImplemented
}

func (f *githubForge) MarkReady(ctx context.Context, ref PRReference) error {
	return errForgeMergeNotImplemented
}
