package app

import (
	"slices"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
)

// The attention state of one row of the Pull requests page's list
// (docs/specs/pull-requests.md, "The page"): what the PR needs from the
// signed-in user, from its list row and, once the focus read landed, its
// detail. A pure function of the two, so the table tests pin every rule.

// The attention states, in sort order.
const (
	PRStateYourTurn         = "your_turn"
	PRStatePipelineFailed   = "pipeline_failed"
	PRStateReady            = "ready"
	PRStateChangesRequested = "changes_requested"
	PRStateWaitingOnAuthor  = "waiting_on_author"
	PRStateDraft            = "draft"
	PRStateOpen             = "open"
)

// The groups a state belongs to: the first three need the user, and their
// count is the sidebar badge.
const (
	PRGroupNeedsYou = "needs_you"
	PRGroupWaiting  = "waiting"
)

var prStateOrder = []string{
	PRStateYourTurn, PRStatePipelineFailed, PRStateReady, PRStateChangesRequested,
	PRStateWaitingOnAuthor, PRStateDraft, PRStateOpen,
}

// PRListDetail is what the focus read (git.Core.ReadPRSummary) adds to a
// row: readiness and its blocker kinds, approvals, unresolved threads, the
// head checks (ChecksState in the list's success/failure/pending
// vocabulary, PipelineStatus the normalized CI status), whether the
// viewer's approval stands and, on GitLab, the viewer's own review state
// (git.PRDetail.ViewerReviewState).
type PRListDetail struct {
	ReadinessState    string   `json:"readinessState"`
	Blockers          []string `json:"blockers"`
	ApprovalsGiven    int      `json:"approvalsGiven"`
	ApprovalsRequired int      `json:"approvalsRequired"`
	Unresolved        int      `json:"unresolved"`
	ChecksState       string   `json:"checksState"`
	PipelineStatus    string   `json:"pipelineStatus"`
	ViewerApproved    bool     `json:"viewerApproved"`
	ViewerReviewState string   `json:"viewerReviewState,omitempty"`
}

// prListDetailOf summarizes a PR's detail for its row.
func prListDetailOf(detail gitops.PRDetail) *PRListDetail {
	out := &PRListDetail{
		ReadinessState:    detail.Readiness.State,
		Blockers:          make([]string, 0, len(detail.Readiness.Blockers)),
		ApprovalsGiven:    detail.Readiness.Approvals.Given,
		ApprovalsRequired: detail.Readiness.Approvals.Required,
		ViewerApproved:    detail.Merge.ViewerApproved,
		ViewerReviewState: detail.ViewerReviewState,
	}
	for _, blocker := range detail.Readiness.Blockers {
		out.Blockers = append(out.Blockers, blocker.Kind)
	}
	if detail.Readiness.Unresolved != nil {
		out.Unresolved = *detail.Readiness.Unresolved
	}
	checks := detail.Checks
	switch {
	case checks.Failure > 0:
		out.ChecksState = "failure"
	case checks.Pending > 0:
		out.ChecksState = "pending"
	case checks.Total > 0:
		out.ChecksState = "success"
	}
	if len(checks.Checks) > 0 {
		statuses := make([]string, 0, len(checks.Checks))
		for _, check := range checks.Checks {
			statuses = append(statuses, gitops.NormalizeCIStatus(check.Status, check.Conclusion))
		}
		out.PipelineStatus = gitops.AggregateCIStatus(statuses)
	}
	return out
}

// prAttention is a row's state and group. detail is nil until the focus
// read landed. In order:
//
//   - your_turn: the user did not open it, and the forge requests their
//     review, or they reviewed it and the author pushed or replied after
//     that review (GitHub: the head commit, or the author's latest
//     comment or review, is newer than the review). On GitLab, whose
//     list cannot say whether a reviewer reviewed, a reviewer whose own
//     review, as the detail reads it, has not approved, requested changes
//     or reviewed (before the detail lands, any reviewer).
//   - pipeline_failed: theirs, and the checks or the pipeline failed. A
//     GitLab list row carries no pipeline, so there it waits for the
//     detail.
//   - ready: theirs, and the readiness is ready, or before the detail the
//     forge's merge status is clean (GitHub clean or has_hooks, GitLab
//     mergeable).
//   - changes_requested: theirs, and a reviewer requested changes.
//   - waiting_on_author: the user's own review requested changes and
//     nothing moved since (GitHub: their latest review; GitLab: their
//     reviewer state, which the detail reads). Another reviewer's change
//     request is never the user's.
//   - draft, then open.
func prAttention(row gitops.PRListRow, detail *PRListDetail) (state, group string) {
	state = prAttentionState(row, detail)
	group = PRGroupWaiting
	switch state {
	case PRStateYourTurn, PRStatePipelineFailed, PRStateReady:
		group = PRGroupNeedsYou
	}
	return state, group
}

func prAttentionState(row gitops.PRListRow, detail *PRListDetail) string {
	changesRequested := row.ReviewDecision == "CHANGES_REQUESTED"
	if !row.ViewerIsAuthor {
		review := row.ViewerReview
		reviewed := review.SubmittedAt != ""
		headMoved := reviewed && rfc3339After(row.LastCommitAt, review.SubmittedAt)
		authorReplied := reviewed && rfc3339After(row.AuthorActivityAt, review.SubmittedAt)
		switch {
		case row.Ref.Forge == "gitlab":
			if row.ViewerIsReviewer && !gitlabViewerReviewed(detail) {
				return PRStateYourTurn
			}
		case row.ReviewRequested || headMoved || authorReplied:
			return PRStateYourTurn
		}
	} else {
		failed := row.ChecksState == "failure"
		if detail != nil {
			failed = detail.ChecksState == "failure" || detail.PipelineStatus == gitops.CIStatusFailed
		}
		if failed {
			return PRStatePipelineFailed
		}
		ready := slices.Contains([]string{"clean", "has_hooks", "mergeable"}, row.MergeStatus)
		if detail != nil {
			ready = detail.ReadinessState == gitops.MergeStateReady
		}
		if ready {
			return PRStateReady
		}
		if changesRequested {
			return PRStateChangesRequested
		}
	}
	if !row.ViewerIsAuthor {
		// The user's own change request: GitHub's list names their latest
		// review, GitLab's detail their reviewer state. The aggregate
		// decision may be another reviewer's.
		mine := strings.EqualFold(row.ViewerReview.State, "CHANGES_REQUESTED")
		if row.Ref.Forge == "gitlab" {
			mine = row.ViewerIsReviewer && detail != nil && detail.ViewerReviewState == gitlabReviewRequestedChanges
		}
		if mine {
			return PRStateWaitingOnAuthor
		}
	}
	if row.Draft {
		return PRStateDraft
	}
	return PRStateOpen
}

// gitlabReviewRequestedChanges is GitLab's reviewer state for a review
// that requested changes.
const gitlabReviewRequestedChanges = "requested_changes"

// gitlabViewerReviewed reports that the detail shows the viewer's own
// review done: their approval stands, or their reviewer state is
// approved, requested_changes or reviewed. Unknown before the detail.
func gitlabViewerReviewed(detail *PRListDetail) bool {
	if detail == nil {
		return false
	}
	switch detail.ViewerReviewState {
	case "approved", gitlabReviewRequestedChanges, "reviewed":
		return true
	}
	return detail.ViewerApproved
}

// rfc3339After reports whether a is a time after b; an unreadable time is
// not after anything.
func rfc3339After(a, b string) bool {
	at, errA := time.Parse(time.RFC3339, a)
	bt, errB := time.Parse(time.RFC3339, b)
	return errA == nil && errB == nil && at.After(bt)
}

// sortPRListItems orders a project's rows: needs-you first, then by state
// order, then most recently updated first, then by number for a stable
// order.
func sortPRListItems(items []PRListItem) {
	rank := func(state string) int { return slices.Index(prStateOrder, state) }
	slices.SortStableFunc(items, func(x, y PRListItem) int {
		if gx, gy := x.Group == PRGroupNeedsYou, y.Group == PRGroupNeedsYou; gx != gy {
			if gx {
				return -1
			}
			return 1
		}
		if d := rank(x.State) - rank(y.State); d != 0 {
			return d
		}
		if c := strings.Compare(y.UpdatedAt, x.UpdatedAt); c != 0 {
			if rfc3339After(x.UpdatedAt, y.UpdatedAt) {
				return -1
			}
			if rfc3339After(y.UpdatedAt, x.UpdatedAt) {
				return 1
			}
			return c
		}
		return y.Ref.Number - x.Ref.Number
	})
}
