package app

import (
	"testing"

	gitops "agent-overflow/internal/git"
)

func TestPRListAttentionStates(t *testing.T) {
	t.Parallel()
	gh := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "o", Repo: "r", Number: 1}
	gl := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "o", Repo: "r", Number: 1}
	cases := []struct {
		name   string
		row    gitops.PRListRow
		detail *PRListDetail
		state  string
	}{
		{name: "github review requested", row: gitops.PRListRow{Ref: gh, ReviewRequested: true, ViewerIsReviewer: true}, state: PRStateYourTurn},
		{name: "github head moved after my review", row: gitops.PRListRow{Ref: gh, LastCommitAt: "2026-01-02T00:00:00Z", ViewerReview: gitops.PRListReview{State: "APPROVED", SubmittedAt: "2026-01-01T00:00:00Z"}}, state: PRStateYourTurn},
		{name: "github head moved after my change request", row: gitops.PRListRow{Ref: gh, ReviewDecision: "CHANGES_REQUESTED", LastCommitAt: "2026-01-02T00:00:00Z", ViewerReview: gitops.PRListReview{State: "CHANGES_REQUESTED", SubmittedAt: "2026-01-01T00:00:00Z"}}, state: PRStateYourTurn},
		{name: "github my change request stands", row: gitops.PRListRow{Ref: gh, ReviewDecision: "CHANGES_REQUESTED", LastCommitAt: "2026-01-01T00:00:00Z", ViewerReview: gitops.PRListReview{State: "CHANGES_REQUESTED", SubmittedAt: "2026-01-02T00:00:00Z"}}, state: PRStateWaitingOnAuthor},
		{name: "github reviewed, nothing moved", row: gitops.PRListRow{Ref: gh, LastCommitAt: "2026-01-01T00:00:00Z", ViewerReview: gitops.PRListReview{State: "APPROVED", SubmittedAt: "2026-01-02T00:00:00Z"}}, state: PRStateOpen},
		{name: "github unreadable times are not a move", row: gitops.PRListRow{Ref: gh, LastCommitAt: "soon", ViewerReview: gitops.PRListReview{State: "APPROVED", SubmittedAt: "2026-01-02T00:00:00Z"}}, state: PRStateOpen},
		{name: "gitlab reviewer not approved", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ReviewRequested: true}, state: PRStateYourTurn},
		{name: "gitlab reviewer approved", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ReviewRequested: true}, detail: &PRListDetail{ViewerApproved: true}, state: PRStateOpen},
		{name: "gitlab my change request stands", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ReviewDecision: "CHANGES_REQUESTED"}, detail: &PRListDetail{ViewerReviewState: "requested_changes"}, state: PRStateWaitingOnAuthor},
		{name: "gitlab another reviewer's change request, mine unreviewed", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ReviewDecision: "CHANGES_REQUESTED"}, detail: &PRListDetail{ViewerReviewState: "unreviewed"}, state: PRStateYourTurn},
		{name: "gitlab another reviewer's change request before the detail", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ReviewDecision: "CHANGES_REQUESTED"}, state: PRStateYourTurn},
		{name: "gitlab another reviewer's change request, mine approved", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true, ReviewDecision: "CHANGES_REQUESTED"}, detail: &PRListDetail{ViewerApproved: true, ViewerReviewState: "approved"}, state: PRStateOpen},
		{name: "gitlab reviewer commented", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true}, detail: &PRListDetail{ViewerReviewState: "reviewed"}, state: PRStateOpen},
		{name: "gitlab reviewer withdrew an approval", row: gitops.PRListRow{Ref: gl, ViewerIsReviewer: true}, detail: &PRListDetail{ViewerReviewState: "unapproved"}, state: PRStateYourTurn},
		{name: "gitlab not a reviewer", row: gitops.PRListRow{Ref: gl}, state: PRStateOpen},
		{name: "github failing checks at list stage", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, ChecksState: "failure", MergeStatus: "clean"}, state: PRStatePipelineFailed},
		{name: "detail checks pass over a failing list rollup", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, ChecksState: "failure"}, detail: &PRListDetail{ChecksState: "success", ReadinessState: gitops.MergeStateReady}, state: PRStateReady},
		{name: "gitlab row without detail is not failed", row: gitops.PRListRow{Ref: gl, ViewerIsAuthor: true, MergeStatus: "ci_still_running"}, state: PRStateOpen},
		{name: "gitlab detail pipeline failed", row: gitops.PRListRow{Ref: gl, ViewerIsAuthor: true, MergeStatus: "ci_must_pass"}, detail: &PRListDetail{PipelineStatus: gitops.CIStatusFailed}, state: PRStatePipelineFailed},
		{name: "github clean at list stage", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, MergeStatus: "clean"}, state: PRStateReady},
		{name: "github has hooks at list stage", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, MergeStatus: "has_hooks"}, state: PRStateReady},
		{name: "gitlab mergeable at list stage", row: gitops.PRListRow{Ref: gl, ViewerIsAuthor: true, MergeStatus: "mergeable"}, state: PRStateReady},
		{name: "detail blocked overrides a clean list status", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, MergeStatus: "clean"}, detail: &PRListDetail{ReadinessState: gitops.MergeStateBlocked}, state: PRStateOpen},
		{name: "mine with changes requested", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, ReviewDecision: "CHANGES_REQUESTED", MergeStatus: "blocked"}, state: PRStateChangesRequested},
		{name: "my draft", row: gitops.PRListRow{Ref: gh, ViewerIsAuthor: true, Draft: true, MergeStatus: "draft"}, state: PRStateDraft},
		{name: "someone's draft", row: gitops.PRListRow{Ref: gh, Draft: true}, state: PRStateDraft},
		{name: "review request outranks a draft", row: gitops.PRListRow{Ref: gh, Draft: true, ReviewRequested: true}, state: PRStateYourTurn},
		{name: "someone else's failing pipeline", row: gitops.PRListRow{Ref: gh, ChecksState: "failure"}, state: PRStateOpen},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			state, group := prAttention(tt.row, tt.detail)
			if state != tt.state {
				t.Fatalf("state = %q, want %q", state, tt.state)
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

func TestPRListSortsByGroupStateThenUpdated(t *testing.T) {
	t.Parallel()
	item := func(number int, state, updated string) PRListItem {
		_, group := prAttention(gitops.PRListRow{}, nil)
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
	}
	sortPRListItems(items)
	var got []int
	for _, it := range items {
		got = append(got, it.Ref.Number)
	}
	want := []int{4, 7, 3, 2, 6, 5, 8, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestPRListDetailSummarizesTheReadiness(t *testing.T) {
	t.Parallel()
	unresolved := 3
	detail := prListDetailOf(gitops.PRDetail{
		Readiness: gitops.MergeReadiness{
			State:      gitops.MergeStateBlocked,
			Blockers:   []gitops.MergeBlocker{{Kind: "approvals"}, {Kind: "threads"}},
			Approvals:  gitops.ApprovalCount{Given: 1, Required: 2},
			Unresolved: &unresolved,
		},
		Merge:             gitops.MergeOptions{ViewerApproved: true},
		ViewerReviewState: "approved",
		Checks:            gitops.CheckSummary{Total: 2, Failure: 1, Checks: []gitops.CheckStatus{{Status: "COMPLETED", Conclusion: "FAILURE"}, {Status: "COMPLETED", Conclusion: "SUCCESS"}}},
	})
	if detail.ReadinessState != gitops.MergeStateBlocked || len(detail.Blockers) != 2 || detail.Blockers[1] != "threads" ||
		detail.ApprovalsGiven != 1 || detail.ApprovalsRequired != 2 || detail.Unresolved != 3 || !detail.ViewerApproved || detail.ViewerReviewState != "approved" ||
		detail.ChecksState != "failure" || detail.PipelineStatus != gitops.CIStatusFailed {
		t.Fatalf("detail = %+v", detail)
	}
}
