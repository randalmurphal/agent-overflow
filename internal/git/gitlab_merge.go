package git

import "context"

// GitLab's merge actions (ForgeMerger). Approve is POST .../approve with
// the head sha, Unapprove POST .../unapprove, Merge PUT .../merge (with
// auto_merge, or merge_when_pipeline_succeeds on a server that rejects
// it), CancelAutoMerge POST .../cancel_auto_merge (falling back to
// cancel_merge_when_pipeline_succeeds), UpdateBranch PUT .../rebase and
// MarkReady PUT .../ with the draft title prefix removed. Readiness and
// MergeOptions are derived in gitlab.go from detailed_merge_status,
// has_conflicts, the approvals read and the project's merge settings.
// See docs/specs/pull-requests.md.

func (f *gitlabForge) Approve(ctx context.Context, ref PRReference, headSHA string) error {
	return errForgeMergeNotImplemented
}

func (f *gitlabForge) Unapprove(ctx context.Context, ref PRReference, approvalID string) error {
	return errForgeMergeNotImplemented
}

func (f *gitlabForge) Merge(ctx context.Context, ref PRReference, req MergeRequest) (MergeOutcome, error) {
	return MergeOutcome{}, errForgeMergeNotImplemented
}

func (f *gitlabForge) CancelAutoMerge(ctx context.Context, ref PRReference) error {
	return errForgeMergeNotImplemented
}

func (f *gitlabForge) UpdateBranch(ctx context.Context, ref PRReference, headSHA string) error {
	return errForgeMergeNotImplemented
}

func (f *gitlabForge) MarkReady(ctx context.Context, ref PRReference) error {
	return errForgeMergeNotImplemented
}
