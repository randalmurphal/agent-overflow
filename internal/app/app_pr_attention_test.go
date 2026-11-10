package app

import (
	"testing"

	gitops "agent-overflow/internal/git"
)

func TestPRListAttentionStates(t *testing.T) {
	t.Parallel()
	gh := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "o", Repo: "r", Number: 1}
	gl := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "o", Repo: "r", Number: 1}
	const (
		before = "2026-01-01T00:00:00Z"
		review = "2026-01-02T00:00:00Z"
		after  = "2026-01-03T00:00:00Z"
	)
	// reviewed is a row the viewer reviewed at review, its state as the
	// forge reads it.
	reviewed := func(ref gitops.PRReference, state string, mutate func(*gitops.PRListRow)) gitops.PRListRow {
		row := gitops.PRListRow{Ref: ref, ViewerIsReviewer: true, ViewerReviewState: state, ViewerActivityAt: review, LastCommitAt: before, UpdatedAt: review}
		if ref.Forge == "github" {
			row.ViewerReview = gitops.PRListReview{SubmittedAt: review}
		}
		if mutate != nil {
			mutate(&row)
		}
		return row
	}
	cases := []struct {
		name   string
		row    gitops.PRListRow
		state  string
		reason *PRReason
	}{
		// Someone else's PR: the viewer's turn.
		{name: "github review requested", row: gitops.PRListRow{Ref: gh, ReviewRequested: true, ViewerIsReviewer: true, ViewerReviewState: "unreviewed"},
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReviewRequested}},
		{name: "gitlab reviewer unreviewed", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ViewerReviewState: "unreviewed"},
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReviewRequested}},
		{name: "gitlab reviewer review started", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ViewerReviewState: "review_started"},
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReviewRequested}},
		{name: "gitlab reviewer whose review read failed", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true},
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReviewRequested}},
		{name: "review request outranks a draft", row: gitops.PRListRow{Ref: gh, Draft: true, ReviewRequested: true, ViewerReviewState: "unreviewed"},
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReviewRequested}},
		{name: "github pushed after my approval", row: reviewed(gh, "approved", func(r *gitops.PRListRow) { r.LastCommitAt = after }),
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonPushed}},
		{name: "gitlab pushed after my change request", row: reviewed(gl, "requested_changes", func(r *gitops.PRListRow) { r.LastCommitAt, r.UpdatedAt = after, after }),
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonPushed}},
		{name: "a push outranks a reply", row: reviewed(gh, "requested_changes", func(r *gitops.PRListRow) { r.LastCommitAt, r.AuthorActivityAt, r.AuthorReplies = after, after, 2 }),
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonPushed}},
		{name: "github author replied after my review", row: reviewed(gh, "requested_changes", func(r *gitops.PRListRow) { r.AuthorActivityAt, r.AuthorReplies = after, 2 }),
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReplied, Replies: 2}},
		{name: "github author replied before my review", row: reviewed(gh, "requested_changes", func(r *gitops.PRListRow) { r.AuthorActivityAt = before }),
			state: PRStateWaitingOnAuthor},
		{name: "github the update time alone is not a reply", row: reviewed(gh, "reviewed", func(r *gitops.PRListRow) { r.UpdatedAt = after }),
			state: PRStateApproved, reason: &PRReason{Kind: PRReasonReviewed}},
		{name: "gitlab updated after my activity", row: reviewed(gl, "reviewed", func(r *gitops.PRListRow) { r.UpdatedAt = "2026-01-02T00:00:11Z" }),
			state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReplied}},
		// GitLab touches the merge request just after the viewer's own note.
		{name: "gitlab my own comment is the last activity", row: reviewed(gl, "reviewed", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.UpdatedAt = "2026-01-02T00:00:00.120Z", "2026-01-02T00:00:00.934Z"
		}), state: PRStateApproved, reason: &PRReason{Kind: PRReasonReviewed}},
		{name: "gitlab a touch within the slack is no reply", row: reviewed(gl, "approved", func(r *gitops.PRListRow) { r.UpdatedAt = "2026-01-02T00:00:10Z" }),
			state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		// Waiting on others, the viewer's own review standing.
		{name: "github my change request stands", row: reviewed(gh, "requested_changes", func(r *gitops.PRListRow) { r.ReviewDecision = "CHANGES_REQUESTED" }),
			state: PRStateWaitingOnAuthor},
		{name: "gitlab my change request stands", row: reviewed(gl, "requested_changes", nil), state: PRStateWaitingOnAuthor},
		{name: "github my approval stands", row: reviewed(gh, "approved", nil), state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		{name: "gitlab my approval stands", row: reviewed(gl, "approved", nil), state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		{name: "github reviewed without approving", row: reviewed(gh, "reviewed", nil), state: PRStateApproved, reason: &PRReason{Kind: PRReasonReviewed}},
		{name: "gitlab reviewed without approving", row: reviewed(gl, "reviewed", nil), state: PRStateApproved, reason: &PRReason{Kind: PRReasonReviewed}},
		{name: "gitlab withdrew an approval", row: reviewed(gl, "unapproved", nil), state: PRStateApproved, reason: &PRReason{Kind: PRReasonReviewed}},
		{name: "another reviewer's change request is not mine", row: reviewed(gl, "approved", func(r *gitops.PRListRow) { r.ReviewDecision = "CHANGES_REQUESTED" }),
			state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		{name: "gitlab reviewed, the events unread", row: reviewed(gl, "reviewed", func(r *gitops.PRListRow) { r.ViewerActivityAt, r.LastCommitAt, r.UpdatedAt = "", after, after }),
			state: PRStateApproved, reason: &PRReason{Kind: PRReasonReviewed}},
		// The events are a lower bound: an approval with nothing in the
		// window read is older than the window's floor (review here).
		{name: "gitlab approved before the window, pushed after its floor", row: reviewed(gl, "approved", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.ViewerActivityFloor, r.LastCommitAt, r.UpdatedAt = "", review, after, after
		}), state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonPushed}},
		{name: "gitlab approved before the window, updated after its floor", row: reviewed(gl, "approved", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.ViewerActivityFloor, r.UpdatedAt = "", review, "2026-01-02T00:00:11Z"
		}), state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonReplied}},
		{name: "gitlab approved before the window, nothing since its floor", row: reviewed(gl, "approved", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.ViewerActivityFloor, r.LastCommitAt, r.UpdatedAt = "", review, before, before
		}), state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		{name: "gitlab changes requested before the window, pushed after its floor", row: reviewed(gl, "requested_changes", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.ViewerActivityFloor, r.LastCommitAt = "", review, after
		}), state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonPushed}},
		// A feed cut above the viewer's later comment hides it; the older
		// approval the other feed found yields to the floor.
		{name: "gitlab activity older than the floor, pushed before it", row: reviewed(gl, "approved", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.ViewerActivityFloor, r.LastCommitAt, r.UpdatedAt = before, after, review, review
		}), state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		{name: "gitlab activity older than the floor, pushed after it", row: reviewed(gl, "approved", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.ViewerActivityFloor, r.LastCommitAt = before, review, after
		}), state: PRStateYourTurn, reason: &PRReason{Kind: PRReasonPushed}},
		{name: "activity in the window outranks the floor", row: reviewed(gl, "approved", func(r *gitops.PRListRow) {
			r.ViewerActivityAt, r.ViewerActivityFloor, r.LastCommitAt = after, review, after
		}), state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		{name: "unreadable times are not a move", row: reviewed(gh, "approved", func(r *gitops.PRListRow) { r.LastCommitAt = "soon" }),
			state: PRStateApproved, reason: &PRReason{Kind: PRReasonApproved}},
		// Nobody asked the viewer.
		{name: "github author activity without my review", row: gitops.PRListRow{Ref: gh, AuthorActivityAt: after}, state: PRStateOpen},
		{name: "gitlab not a reviewer", row: gitops.PRListRow{Ref: gl, UpdatedAt: after}, state: PRStateOpen},
		{name: "someone's draft", row: gitops.PRListRow{Ref: gh, Draft: true}, state: PRStateDraft},
		{name: "someone else's failing pipeline", row: gitops.PRListRow{Ref: gh, ChecksState: "failure", PipelineStatus: gitops.CIStatusFailed}, state: PRStateOpen},
		// The viewer's own PR.
		{name: "github mine, checks failed", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, PipelineStatus: gitops.CIStatusFailed, MergeStatus: "clean"}, state: PRStatePipelineFailed},
		{name: "gitlab mine, pipeline failed", row: gitops.PRListRow{Ref: gl, ViewerIsAuthor: true, PipelineStatus: gitops.CIStatusFailed, MergeStatus: "ci_must_pass"}, state: PRStatePipelineFailed},
		{name: "gitlab mine, pipeline unknown", row: gitops.PRListRow{Ref: gl, ViewerIsAuthor: true, MergeStatus: "ci_still_running"}, state: PRStateOpen},
		{name: "github mine, clean", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, MergeStatus: "clean"}, state: PRStateReady},
		{name: "github mine, has hooks", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, MergeStatus: "has_hooks"}, state: PRStateReady},
		{name: "gitlab mine, mergeable", row: gitops.PRListRow{Ref: gl, ViewerIsAuthor: true, MergeStatus: "mergeable"}, state: PRStateReady},
		{name: "mine with changes requested", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, ReviewDecision: "CHANGES_REQUESTED", MergeStatus: "blocked"}, state: PRStateChangesRequested},
		{name: "gitlab mine with changes requested", row: gitops.PRListRow{Ref: gl, ViewerIsAuthor: true, ReviewDecision: "CHANGES_REQUESTED", MergeStatus: "requested_changes"}, state: PRStateChangesRequested},
		{name: "my draft", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, Draft: true, MergeStatus: "draft"}, state: PRStateDraft},
		{name: "mine, blocked", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, MergeStatus: "blocked"}, state: PRStateOpen},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			state, group, reason := prAttention(tt.row)
			if state != tt.state {
				t.Fatalf("state = %q, want %q", state, tt.state)
			}
			if (reason == nil) != (tt.reason == nil) || (reason != nil && *reason != *tt.reason) {
				t.Fatalf("reason = %+v, want %+v", reason, tt.reason)
			}
			wantGroup := PRGroupWaiting
			if tt.state == PRStateYourTurn || tt.state == PRStatePipelineFailed || tt.state == PRStateReady {
				wantGroup = PRGroupNeedsYou
			}
			if group != wantGroup {
				t.Fatalf("group = %q, want %q", group, wantGroup)
			}
		})
	}
}

// A row's detail fields reach the wire as the page shows them: approvals
// only when both counts are known, the unresolved count and the CI status.
func TestPRListItemCarriesTheDetailFields(t *testing.T) {
	t.Parallel()
	one, two, three := 1, 2, 3
	item := prListItemOf(gitops.PRListRow{ApprovalsGiven: &one, ApprovalsRequired: &two, Unresolved: &three, PipelineStatus: gitops.CIStatusRunning})
	if item.Approvals == nil || *item.Approvals != (PRApprovals{Given: 1, Required: 2}) || item.Unresolved == nil || *item.Unresolved != 3 || item.CI != gitops.CIStatusRunning {
		t.Fatalf("item = %+v approvals %+v", item, item.Approvals)
	}
	if partial := prListItemOf(gitops.PRListRow{ApprovalsRequired: &two}); partial.Approvals != nil || partial.Unresolved != nil || partial.CI != "" {
		t.Fatalf("unknown detail = %+v", partial)
	}
}

func TestPRListSortsByGroupStateThenUpdated(t *testing.T) {
	t.Parallel()
	item := func(number int, state, updated string) PRListItem {
		group := PRGroupWaiting
		if state == PRStateYourTurn || state == PRStatePipelineFailed || state == PRStateReady {
			group = PRGroupNeedsYou
		}
		return PRListItem{PRListRow: gitops.PRListRow{Ref: gitops.PRReference{Number: number}, UpdatedAt: updated}, State: state, Group: group}
	}
	items := []PRListItem{
		item(1, PRStateOpen, "2026-01-09T00:00:00Z"),
		item(2, PRStateReady, "2026-01-01T00:00:00Z"),
		item(3, PRStateYourTurn, "2026-01-01T00:00:00Z"),
		item(4, PRStateYourTurn, "2026-01-03T00:00:00Z"),
		item(5, PRStateDraft, "2026-01-02T00:00:00Z"),
		item(6, PRStateWaitingOnAuthor, "2026-01-02T00:00:00Z"),
		// Offsets compare as times, not strings.
		item(7, PRStateYourTurn, "2026-01-03T01:00:00+02:00"),
		item(8, PRStateOpen, "2026-01-09T00:00:00Z"),
		item(9, PRStateApproved, "2026-01-01T00:00:00Z"),
	}
	sortPRListItems(items)
	var got []int
	for _, it := range items {
		got = append(got, it.Ref.Number)
	}
	want := []int{4, 7, 3, 2, 6, 9, 5, 8, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
