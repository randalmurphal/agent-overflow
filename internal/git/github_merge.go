package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitHub's merge actions (ForgeMerger). Approve is an approving REST
// review pinned to the head; Unapprove dismisses the viewer's approving
// review (found with ViewerApproval when the caller has no id); Merge is
// mergePullRequest (then the head branch's REST delete
// when asked) or enablePullRequestAutoMerge; CancelAutoMerge is
// disablePullRequestAutoMerge; UpdateBranch is updatePullRequestBranch;
// MarkReady is markPullRequestReadyForReview. The PR-scoped mutations
// first read the PR's node id (PRMergeTarget), so each costs two GraphQL
// requests. Forge errors are returned as the transport reported them, for
// ExplainForgeError. Readiness and MergeOptions are derived in
// github_readiness.go. See docs/specs/pull-requests.md.

// githubPRTargetQuery reads what a merge action addresses: the PR's node
// id, its head branch and whether that is a fork's, and whether the
// repository deletes merged branches itself.
const githubPRTargetQuery = `query PRMergeTarget($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    deleteBranchOnMerge
    pullRequest(number: $number) { id headRefName isCrossRepository }
  }
}`

const githubMergePRMutation = `mutation MergePR($pullRequestID: ID!, $method: PullRequestMergeMethod!, $expectedHeadOid: GitObjectID!) {
  mergePullRequest(input: {pullRequestId: $pullRequestID, mergeMethod: $method, expectedHeadOid: $expectedHeadOid}) { pullRequest { merged mergeCommit { oid } } }
}`

const githubEnableAutoMergeMutation = `mutation EnableAutoMerge($pullRequestID: ID!, $method: PullRequestMergeMethod!, $expectedHeadOid: GitObjectID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $pullRequestID, mergeMethod: $method, expectedHeadOid: $expectedHeadOid}) { pullRequest { autoMergeRequest { enabledAt } } }
}`

const githubDisableAutoMergeMutation = `mutation DisableAutoMerge($pullRequestID: ID!) {
  disablePullRequestAutoMerge(input: {pullRequestId: $pullRequestID}) { pullRequest { autoMergeRequest { enabledAt } } }
}`

const githubDismissReviewMutation = `mutation DismissReview($reviewID: ID!, $message: String!) {
  dismissPullRequestReview(input: {pullRequestReviewId: $reviewID, message: $message}) { pullRequestReview { state } }
}`

const githubUpdateBranchMutation = `mutation UpdatePRBranch($pullRequestID: ID!, $expectedHeadOid: GitObjectID!) {
  updatePullRequestBranch(input: {pullRequestId: $pullRequestID, expectedHeadOid: $expectedHeadOid}) { pullRequest { id } }
}`

const githubMarkReadyMutation = `mutation MarkPRReady($pullRequestID: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $pullRequestID}) { pullRequest { isDraft } }
}`

// githubViewerApprovalQuery reads the reviewers' standing verdicts, for
// the viewer's own approval: latestOpinionatedReviews keeps an approval a
// later comment would hide in latestReviews.
const githubViewerApprovalQuery = `query ViewerApproval($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { latestOpinionatedReviews(first: 100) { nodes { id state viewerDidAuthor } } }
  }
}`

// githubUnapproveMessage is the dismissal message Unapprove leaves on the
// review; GitHub requires one.
const githubUnapproveMessage = "Approval withdrawn"

// BranchKeptError is a merge GitHub completed whose requested head branch
// delete then failed. Outcome is the merge; Err is the delete's forge
// error. A caller that sees it reports the merge as done and the branch
// as still present.
type BranchKeptError struct {
	Outcome MergeOutcome
	Branch  string
	Err     error
}

func (e *BranchKeptError) Error() string {
	return fmt.Sprintf("merged, but the branch %s was not deleted: %v", e.Branch, e.Err)
}

func (e *BranchKeptError) Unwrap() error { return e.Err }

// githubPRTarget is PRMergeTarget's answer.
type githubPRTarget struct {
	ID                  string
	HeadRefName         string
	IsCrossRepository   bool
	DeleteBranchOnMerge bool
}

// prTarget reads ref's node id and head for a mutation.
func (f *githubForge) prTarget(ctx context.Context, ref PRReference) (*forgeapi.Client, githubPRTarget, error) {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return nil, githubPRTarget{}, err
	}
	vars, err := githubPullVars(ref)
	if err != nil {
		return nil, githubPRTarget{}, err
	}
	var raw struct {
		Repository *struct {
			DeleteBranchOnMerge bool `json:"deleteBranchOnMerge"`
			PullRequest         *struct {
				ID                string `json:"id"`
				HeadRefName       string `json:"headRefName"`
				IsCrossRepository bool   `json:"isCrossRepository"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "PRMergeTarget", githubPRTargetQuery, vars, &raw); err != nil {
		return nil, githubPRTarget{}, err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil || raw.Repository.PullRequest.ID == "" {
		return nil, githubPRTarget{}, fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	pull := raw.Repository.PullRequest
	return client, githubPRTarget{ID: pull.ID, HeadRefName: pull.HeadRefName, IsCrossRepository: pull.IsCrossRepository, DeleteBranchOnMerge: raw.Repository.DeleteBranchOnMerge}, nil
}

// validMergeHead checks a head sha argument.
func validMergeHead(headSHA string) (string, error) {
	headSHA = strings.TrimSpace(headSHA)
	if !mergeHeadSHAPattern.MatchString(headSHA) {
		return "", errors.New("the action requires the head commit sha the caller saw")
	}
	return headSHA, nil
}

// Approve submits an approving review pinned to headSHA (commit_id), the
// head the viewer saw. GitHub does not refuse an approval of a head that
// has moved: the review lands on that commit and shows as outdated.
func (f *githubForge) Approve(ctx context.Context, ref PRReference, headSHA string) error {
	headSHA, err := validMergeHead(headSHA)
	if err != nil {
		return err
	}
	if _, _, err := splitGitHubProject(ref.Project()); err != nil {
		return err
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	return postGitHubReview(ctx, client, ref, githubReviewIn{Event: githubReviewEvent(ReviewVerdictApprove), CommitID: headSHA})
}

// errNoApprovalToWithdraw is the refusal of an Unapprove that finds no
// standing approval of the viewer's.
var errNoApprovalToWithdraw = errors.New("no approving review of the viewer's")

// Unapprove dismisses the viewer's own standing approval, with a message
// GitHub requires, and reads the review's state back. The review is
// resolved here (ViewerApproval: the viewer's APPROVED entry of
// latestOpinionatedReviews), never named by the caller, so no other
// login's review and no review of another pull request can be dismissed
// through it. With no standing approval the answer is a *ForgeActionError
// of kind refused, "You have no approval to withdraw.", and nothing is
// dismissed.
func (f *githubForge) Unapprove(ctx context.Context, ref PRReference) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	approvalID, err := viewerApprovalID(ctx, client, ref)
	if err != nil {
		return err
	}
	var raw struct {
		Dismiss *struct {
			PullRequestReview *struct {
				State string `json:"state"`
			} `json:"pullRequestReview"`
		} `json:"dismissPullRequestReview"`
	}
	if _, err := client.GraphQL(ctx, "DismissReview", githubDismissReviewMutation, map[string]any{"reviewID": approvalID, "message": githubUnapproveMessage}, &raw); err != nil {
		return err
	}
	if raw.Dismiss == nil || raw.Dismiss.PullRequestReview == nil {
		return fmt.Errorf("GitHub reported no state for review %s after dismissing it", approvalID)
	}
	if state := raw.Dismiss.PullRequestReview.State; !strings.EqualFold(state, "DISMISSED") {
		return fmt.Errorf("GitHub reported review %s as %s after dismissing it", approvalID, state)
	}
	return nil
}

// viewerApprovalID is the node id of the viewer's standing approval of
// ref.
func viewerApprovalID(ctx context.Context, client *forgeapi.Client, ref PRReference) (string, error) {
	vars, err := githubPullVars(ref)
	if err != nil {
		return "", err
	}
	var raw struct {
		Repository *struct {
			PullRequest *struct {
				LatestOpinionatedReviews struct {
					Nodes []githubOpinionRaw `json:"nodes"`
				} `json:"latestOpinionatedReviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "ViewerApproval", githubViewerApprovalQuery, vars, &raw); err != nil {
		return "", err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil {
		return "", fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	for _, review := range raw.Repository.PullRequest.LatestOpinionatedReviews.Nodes {
		if review.ViewerDidAuthor && strings.EqualFold(review.State, "APPROVED") && review.ID != "" {
			return review.ID, nil
		}
	}
	return "", &ForgeActionError{Kind: ForgeErrorRefused, Message: "You have no approval to withdraw.", Err: errNoApprovalToWithdraw}
}

// Merge merges with req.Method only while the head is req.HeadSHA
// (expectedHeadOid), or with req.AutoMerge arms auto-merge with that
// method and head. After a merge, req.DeleteBranch deletes the head branch
// unless the repository deletes merged branches itself; a fork's branch
// is not the repository's to delete, so asking for it is refused before
// merging. A delete that fails after the merge returns the merge's
// outcome with a *BranchKeptError. A merge GitHub answers without merging
// returns neither Merged nor AutoMerge and deletes nothing; the caller
// follows the PR's next poll.
func (f *githubForge) Merge(ctx context.Context, ref PRReference, req MergeRequest) (MergeOutcome, error) {
	if err := req.Validate(); err != nil {
		return MergeOutcome{}, err
	}
	client, target, err := f.prTarget(ctx, ref)
	if err != nil {
		return MergeOutcome{}, err
	}
	vars := map[string]any{"pullRequestID": target.ID, "method": githubMergeMethodEnum(req.Method), "expectedHeadOid": strings.TrimSpace(req.HeadSHA)}
	if req.AutoMerge {
		var raw struct {
			Enable *struct {
				PullRequest *struct {
					AutoMergeRequest *struct {
						EnabledAt string `json:"enabledAt"`
					} `json:"autoMergeRequest"`
				} `json:"pullRequest"`
			} `json:"enablePullRequestAutoMerge"`
		}
		if _, err := client.GraphQL(ctx, "EnableAutoMerge", githubEnableAutoMergeMutation, vars, &raw); err != nil {
			return MergeOutcome{}, err
		}
		if raw.Enable == nil || raw.Enable.PullRequest == nil || raw.Enable.PullRequest.AutoMergeRequest == nil {
			return MergeOutcome{}, fmt.Errorf("GitHub reported no auto-merge on %s#%d after enabling it", ref.Project(), ref.Number)
		}
		return MergeOutcome{AutoMerge: true}, nil
	}
	if req.DeleteBranch && target.IsCrossRepository {
		return MergeOutcome{}, fmt.Errorf("the head branch of %s#%d is in another repository, so it cannot be deleted from here", ref.Project(), ref.Number)
	}
	var raw struct {
		Merge *struct {
			PullRequest *struct {
				Merged      bool          `json:"merged"`
				MergeCommit *githubOIDRaw `json:"mergeCommit"`
			} `json:"pullRequest"`
		} `json:"mergePullRequest"`
	}
	if _, err := client.GraphQL(ctx, "MergePR", githubMergePRMutation, vars, &raw); err != nil {
		return MergeOutcome{}, err
	}
	if raw.Merge == nil || raw.Merge.PullRequest == nil {
		return MergeOutcome{}, fmt.Errorf("GitHub reported no pull request state after merging %s#%d", ref.Project(), ref.Number)
	}
	if !raw.Merge.PullRequest.Merged {
		return MergeOutcome{}, nil
	}
	outcome := MergeOutcome{Merged: true}
	if commit := raw.Merge.PullRequest.MergeCommit; commit != nil {
		outcome.SHA = commit.OID
	}
	if req.DeleteBranch && !target.DeleteBranchOnMerge {
		if err := deleteGitHubBranch(ctx, client, ref, target.HeadRefName); err != nil {
			return outcome, &BranchKeptError{Outcome: outcome, Branch: target.HeadRefName, Err: err}
		}
	}
	return outcome, nil
}

// deleteGitHubBranch deletes refs/heads/branch of ref's repository. A
// branch already gone (422 "Reference does not exist") is deleted.
func deleteGitHubBranch(ctx context.Context, client *forgeapi.Client, ref PRReference, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("GitHub reported no head branch to delete")
	}
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	path := githubRepoPath(ref) + "/git/refs/heads/" + strings.Join(segments, "/")
	_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodDelete, Path: path}, nil)
	if status, ok := errors.AsType[*forgeapi.StatusError](err); ok && status.Status == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(forgeBodyMessage(status.Body)), "reference does not exist") {
		return nil
	}
	return err
}

// CancelAutoMerge disarms the PR's auto-merge and reads back that none is
// armed.
func (f *githubForge) CancelAutoMerge(ctx context.Context, ref PRReference) error {
	client, target, err := f.prTarget(ctx, ref)
	if err != nil {
		return err
	}
	var raw struct {
		Disable *struct {
			PullRequest *struct {
				AutoMergeRequest *struct {
					EnabledAt string `json:"enabledAt"`
				} `json:"autoMergeRequest"`
			} `json:"pullRequest"`
		} `json:"disablePullRequestAutoMerge"`
	}
	if _, err := client.GraphQL(ctx, "DisableAutoMerge", githubDisableAutoMergeMutation, map[string]any{"pullRequestID": target.ID}, &raw); err != nil {
		return err
	}
	if raw.Disable == nil || raw.Disable.PullRequest == nil {
		return fmt.Errorf("GitHub reported no pull request state after canceling auto-merge on %s#%d", ref.Project(), ref.Number)
	}
	if raw.Disable.PullRequest.AutoMergeRequest != nil {
		return fmt.Errorf("GitHub still reports auto-merge armed on %s#%d after canceling it", ref.Project(), ref.Number)
	}
	return nil
}

// UpdateBranch merges the base into the head while the head is headSHA
// (GitHub's default update method). GitHub runs it asynchronously; the
// next poll shows the new head.
func (f *githubForge) UpdateBranch(ctx context.Context, ref PRReference, headSHA string) error {
	headSHA, err := validMergeHead(headSHA)
	if err != nil {
		return err
	}
	client, target, err := f.prTarget(ctx, ref)
	if err != nil {
		return err
	}
	var raw struct {
		Update *struct {
			PullRequest *struct {
				ID string `json:"id"`
			} `json:"pullRequest"`
		} `json:"updatePullRequestBranch"`
	}
	if _, err := client.GraphQL(ctx, "UpdatePRBranch", githubUpdateBranchMutation, map[string]any{"pullRequestID": target.ID, "expectedHeadOid": headSHA}, &raw); err != nil {
		return err
	}
	if raw.Update == nil || raw.Update.PullRequest == nil {
		return fmt.Errorf("GitHub reported no pull request after updating the branch of %s#%d", ref.Project(), ref.Number)
	}
	return nil
}

// MarkReady takes the PR out of draft and reads back that it is not one.
func (f *githubForge) MarkReady(ctx context.Context, ref PRReference) error {
	client, target, err := f.prTarget(ctx, ref)
	if err != nil {
		return err
	}
	var raw struct {
		Ready *struct {
			PullRequest *struct {
				IsDraft bool `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"markPullRequestReadyForReview"`
	}
	if _, err := client.GraphQL(ctx, "MarkPRReady", githubMarkReadyMutation, map[string]any{"pullRequestID": target.ID}, &raw); err != nil {
		return err
	}
	if raw.Ready == nil || raw.Ready.PullRequest == nil {
		return fmt.Errorf("GitHub reported no pull request state after marking %s#%d ready", ref.Project(), ref.Number)
	}
	if raw.Ready.PullRequest.IsDraft {
		return fmt.Errorf("GitHub still reports %s#%d as a draft after marking it ready", ref.Project(), ref.Number)
	}
	return nil
}
