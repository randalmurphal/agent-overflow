package git

import (
	"encoding/json"
	"strings"
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

// A PRTick answer whose errors all lie under the management selections
// publishes the PR with its controls unavailable; an error anywhere else
// fails the read.
func TestGitHubManageFieldErrorHidesOnlyTheControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		errors string
		fails  bool
		labels any
	}{
		{"labels refused", `[{"type":"FORBIDDEN","message":"Resource not accessible by integration","path":["repository","pullRequest","labels"]}]`, false, nil},
		{"assignees and a viewer flag", `[{"message":"Something went wrong","path":["repository","pullRequest","assignees"]},{"message":"x","path":["repository","pullRequest","viewerCanLabel"]}]`, false, nil},
		{"review requests", `[{"type":"FORBIDDEN","message":"no","path":["repository","pullRequest","reviewRequests","nodes",0,"requestedReviewer"]}]`, false, nil},
		{"elsewhere too", `[{"type":"FORBIDDEN","message":"no","path":["repository","pullRequest","labels"]},{"message":"no","path":["repository","pullRequest","latestReviews"]}]`, true, nil},
		{"no path", `[{"message":"Something went wrong"}]`, true, nil},
		{"data that does not decode", `[{"type":"FORBIDDEN","message":"no","path":["repository","pullRequest","labels"]}]`, true, "not a connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pull := githubMergePull(map[string]any{"labels": tc.labels, "viewerCanUpdate": true, "viewerCanLabel": true})
			body := githubTickJSON(t, githubMergeRepo(nil), pull)
			body = body[:len(body)-1] + `,"errors":` + tc.errors + `}`
			core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				if call.Op != "PRTick" {
					return forgeUnexpected(t, call)
				}
				return forgeAPIAnswer{Body: body}
			})
			read, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true}, nil, nil)
			if tc.fails {
				if err == nil {
					t.Fatal("an error outside the management fields was published")
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadPR = %v, want the PR without its controls", err)
			}
			m := read.Detail.Manage
			if read.Detail.Number != 7 || read.Detail.HeadSHA != "abc1234" {
				t.Fatalf("detail = %+v", read.Detail)
			}
			if !strings.HasPrefix(m.Unavailable, "Reviewers, assignees, labels and the PR's edit controls are not shown") ||
				m.Can != (PRManageCan{}) || len(m.Reviewers)+len(m.Assignees)+len(m.Labels) != 0 {
				t.Fatalf("manage = %+v", m)
			}
		})
	}
}
