package git

import (
	"encoding/json"
	"testing"
)

// githubManageOf is the Details read of a PR and repository given as
// GitHub's JSON.
func githubManageOf(t *testing.T, pull, repo string) PRManage {
	t.Helper()
	var in githubMergeInputs
	in.pull = &githubPullRaw{}
	if err := json.Unmarshal([]byte(pull), in.pull); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(repo), &in.repo); err != nil {
		t.Fatal(err)
	}
	return in.manage()
}

// A reviewer's state and staleness come from the same review: a comment
// on the head after an approval or a change request on an older commit
// leaves the opinion standing and stale.
func TestGitHubReviewerStateAndStalenessFromOneReview(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, opinion, opinionCommit string
		state                        string
		stale                        bool
	}{
		{"approve, then comment on a newer commit", "APPROVED", "old", ReviewerApproved, true},
		{"request changes, then comment on a newer commit", "CHANGES_REQUESTED", "old", ReviewerChangesRequested, true},
		{"approve on the head", "APPROVED", "head", ReviewerApproved, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pull := `{"state":"OPEN","headRefOid":"head","author":{"login":"octo"},
				"latestReviews":{"nodes":[{"state":"COMMENTED","author":{"login":"bob"},"commit":{"oid":"head"}}]},
				"latestOpinionatedReviews":{"nodes":[{"state":"` + tc.opinion + `","author":{"login":"bob"},"commit":{"oid":"` + tc.opinionCommit + `"}}]}}`
			m := githubManageOf(t, pull, `{"viewerPermission":"WRITE"}`)
			if len(m.Reviewers) != 1 || m.Reviewers[0].State != tc.state || m.Reviewers[0].Stale != tc.stale {
				t.Fatalf("reviewers = %+v, want bob %s stale %v", m.Reviewers, tc.state, tc.stale)
			}
		})
	}
	m := githubManageOf(t, `{"state":"OPEN","headRefOid":"head","author":{"login":"octo"},
		"latestReviews":{"nodes":[{"state":"COMMENTED","author":{"login":"bob"},"commit":{"oid":"old"}}]}}`, `{}`)
	if len(m.Reviewers) != 1 || m.Reviewers[0].State != ReviewerCommented || !m.Reviewers[0].Stale {
		t.Fatalf("a comment alone on an older commit = %+v", m.Reviewers)
	}
}

// Each control follows its own viewer field; requesting and dismissing
// reviews follow the role.
func TestGitHubManageControlsFollowTheirViewerFields(t *testing.T) {
	t.Parallel()
	all := `"viewerCanUpdate":true,"viewerCanAssign":true,"viewerCanLabel":true,"viewerCanClose":true,"viewerCanReopen":true`
	for _, tc := range []struct {
		name, pull, role string
		want             PRManageCan
	}{
		{"every field", `{"state":"OPEN",` + all + `}`, "WRITE",
			PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Assignees: true, Labels: true, Close: true, ConvertToDraft: true, Rerequest: true, Dismiss: true}},
		{"cannot close", `{"state":"OPEN","viewerCanUpdate":true,"viewerCanAssign":true,"viewerCanLabel":true,"viewerCanReopen":true}`, "WRITE",
			PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Assignees: true, Labels: true, ConvertToDraft: true, Rerequest: true, Dismiss: true}},
		{"cannot assign", `{"state":"OPEN","viewerCanUpdate":true,"viewerCanLabel":true,"viewerCanClose":true}`, "TRIAGE",
			PRManageCan{EditTitle: true, EditDescription: true, Reviewers: true, Labels: true, Close: true, ConvertToDraft: true, Rerequest: true}},
		{"cannot label", `{"state":"OPEN","viewerCanUpdate":true,"viewerCanAssign":true}`, "READ",
			PRManageCan{EditTitle: true, EditDescription: true, Assignees: true, ConvertToDraft: true}},
		{"closed, may reopen", `{"state":"CLOSED",` + all + `}`, "WRITE",
			PRManageCan{EditTitle: true, EditDescription: true, Assignees: true, Labels: true, Reopen: true}},
		{"closed, may not reopen", `{"state":"CLOSED","viewerCanUpdate":true}`, "WRITE",
			PRManageCan{EditTitle: true, EditDescription: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := githubManageOf(t, tc.pull, `{"viewerPermission":"`+tc.role+`"}`).Can; got != tc.want {
				t.Fatalf("can = %+v, want %+v", got, tc.want)
			}
		})
	}
}
