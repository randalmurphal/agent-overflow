package app

import (
	"slices"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
)

// The attention state of one row of the Pull requests page's list
// (docs/specs/pull-requests.md, "The page"): what the PR needs from the
// signed-in user, from the forge's own record of the user's activity on
// it. A pure function of the row, so the table tests pin every rule.

// The attention states, in sort order.
const (
	PRStateYourTurn         = "your_turn"
	PRStatePipelineFailed   = "pipeline_failed"
	PRStateReady            = "ready"
	PRStateChangesRequested = "changes_requested"
	PRStateWaitingOnAuthor  = "waiting_on_author"
	PRStateApproved         = "approved"
	PRStateDraft            = "draft"
	PRStateOpen             = "open"
)

// The groups a state belongs to: the first three need the user, and their
// count is the sidebar badge.
const (
	PRGroupNeedsYou = "needs_you"
	PRGroupWaiting  = "waiting"
)

// The reasons a row carries: why it is the user's turn, or how the user
// stands on a PR it waits on others for.
const (
	PRReasonReviewRequested = "review_requested"
	PRReasonPushed          = "pushed"
	PRReasonReplied         = "replied"
	PRReasonApproved        = "approved"
	PRReasonReviewed        = "reviewed"
)

var prStateOrder = []string{
	PRStateYourTurn, PRStatePipelineFailed, PRStateReady, PRStateChangesRequested,
	PRStateWaitingOnAuthor, PRStateApproved, PRStateDraft, PRStateOpen,
}

// PRReason is why a row is in its state: for your_turn a review requested
// from the user, the author's push after the user's review (Commits, the
// commits since when the forge says, else 0) or the author's reply after
// it (Replies, the author's comments and reviews since on GitHub, else
// 0); for approved whether the user's approval stands (approved) or the
// user reviewed without approving (reviewed).
type PRReason struct {
	Kind    string `json:"kind"`
	Commits int    `json:"commits"`
	Replies int    `json:"replies"`
}

// gitlabTouchSlack is how much later than the user's own note or
// approval GitLab's updated_at may read for the same action: GitLab
// touches the merge request after it writes the note, so the merge
// request's update time trails the event's created_at by a moment.
// Without the slack the user's own comment reads as the author's reply.
const gitlabTouchSlack = 10 * time.Second

// prAttention is a row's state, group and reason. In order:
//
//   - your_turn, not the user's PR: review_requested while the user's
//     review is asked for and not given (GitLab reviewer state unreviewed
//     or review_started, or unknown for a reviewer when the review read
//     failed; GitHub a pending review request); otherwise, once the user
//     reviewed, pushed when the head commit is newer than the user's
//     latest activity, else replied when the PR moved after it (GitLab
//     its update time, past gitlabTouchSlack; GitHub the author's latest
//     comment or review).
//   - pipeline_failed and ready, the user's PR: its head pipeline failed;
//     the forge would merge it (GitLab detailed_merge_status mergeable,
//     GitHub clean or has_hooks).
//   - changes_requested, the user's PR: a reviewer requested changes.
//   - waiting_on_author: the user's review requested changes and nothing
//     moved since.
//   - approved: the user approved (reason approved) or reviewed without
//     approving (reason reviewed), and nothing moved since.
//   - draft, then open.
func prAttention(row gitops.PRListRow) (state, group string, reason *PRReason) {
	state, reason = prAttentionState(row)
	group = PRGroupWaiting
	switch state {
	case PRStateYourTurn, PRStatePipelineFailed, PRStateReady:
		group = PRGroupNeedsYou
	}
	return state, group, reason
}

func prAttentionState(row gitops.PRListRow) (string, *PRReason) {
	if row.ViewerIsAuthor {
		switch {
		case row.PipelineStatus == gitops.CIStatusFailed:
			return PRStatePipelineFailed, nil
		case slices.Contains([]string{"clean", "has_hooks", "mergeable"}, row.MergeStatus):
			return PRStateReady, nil
		case row.ReviewDecision == "CHANGES_REQUESTED":
			return PRStateChangesRequested, nil
		}
		return prQuietState(row), nil
	}
	switch review := row.ViewerReviewState; {
	case review == "unreviewed" || review == "review_started":
		return PRStateYourTurn, &PRReason{Kind: PRReasonReviewRequested}
	case review == "" && row.Ref.Forge == "gitlab" && row.ViewerIsReviewer:
		// The review read failed: a reviewer whose review is unknown is
		// asked for it until the read says otherwise.
		return PRStateYourTurn, &PRReason{Kind: PRReasonReviewRequested}
	case review == "":
		return prQuietState(row), nil
	}
	if activity := row.ViewerActivityAt; activity != "" {
		if rfc3339After(row.LastCommitAt, activity) {
			return PRStateYourTurn, &PRReason{Kind: PRReasonPushed}
		}
		replied := rfc3339After(row.AuthorActivityAt, activity)
		if row.Ref.Forge == "gitlab" {
			replied = rfc3339AfterBy(row.UpdatedAt, activity, gitlabTouchSlack)
		}
		if replied {
			return PRStateYourTurn, &PRReason{Kind: PRReasonReplied, Replies: row.AuthorReplies}
		}
	}
	switch row.ViewerReviewState {
	case "requested_changes":
		return PRStateWaitingOnAuthor, nil
	case "approved":
		return PRStateApproved, &PRReason{Kind: PRReasonApproved}
	}
	return PRStateApproved, &PRReason{Kind: PRReasonReviewed}
}

// prQuietState is a row's state when nothing asks anything of the user.
func prQuietState(row gitops.PRListRow) string {
	if row.Draft {
		return PRStateDraft
	}
	return PRStateOpen
}

// rfc3339After reports whether a is a time after b; an unreadable time is
// not after anything.
func rfc3339After(a, b string) bool {
	return rfc3339AfterBy(a, b, 0)
}

// rfc3339AfterBy reports whether a is more than slack after b.
func rfc3339AfterBy(a, b string, slack time.Duration) bool {
	at, errA := time.Parse(time.RFC3339, a)
	bt, errB := time.Parse(time.RFC3339, b)
	return errA == nil && errB == nil && at.After(bt.Add(slack))
}

// prListItemOf is a row as the list publishes it: its attention and the
// detail fields in the wire's shape.
func prListItemOf(row gitops.PRListRow) PRListItem {
	item := PRListItem{PRListRow: row, Unresolved: row.Unresolved, CI: row.PipelineStatus}
	item.State, item.Group, item.Reason = prAttention(row)
	if row.ApprovalsGiven != nil && row.ApprovalsRequired != nil {
		item.Approvals = &PRApprovals{Given: *row.ApprovalsGiven, Required: *row.ApprovalsRequired}
	}
	return item
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
