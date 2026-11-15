package git

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func githubTestRepo() RepoReference {
	return RepoReference{Forge: "github", Host: testForgeHost, Namespace: "acme", Repo: "widgets"}
}

func gitlabTestRepo() RepoReference {
	return RepoReference{Forge: "gitlab", Host: testForgeHost, Namespace: "grp/sub", Repo: "app"}
}

// The recorded PullsSearch answer parses into one row per pull request:
// the viewer's own draft, a fork's PR with a review requested from the
// viewer (a team request beside it), and a PR the viewer requested
// changes on.
func TestGitHubListProjectPRsParsesTheSearch(t *testing.T) {
	t.Parallel()
	answer := readTestdata(t, "github-pulls-search.json")
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PullsSearch" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: answer}
	})
	rows, err := core.ListProjectPRs(t.Context(), githubTestRepo())
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	all := calls.all()
	if len(all) != 1 {
		t.Fatalf("requests = %v, want one PullsSearch", calls.ops())
	}
	if q := all[0].Vars["query"]; q != "is:pr is:open repo:acme/widgets" || all[0].Vars["after"] != nil {
		t.Fatalf("PullsSearch variables = %v", all[0].Vars)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	own, fork, changes := rows[0], rows[1], rows[2]
	want := PRListRow{
		Ref:    PRReference{Forge: "github", Host: testForgeHost, Namespace: "acme", Repo: "widgets", Number: 12},
		NodeID: "PR_kwDOAAAB12", URL: "https://github.com/acme/widgets/pull/12", Title: "Draft: widgets", Draft: true,
		Author:    PRListAuthor{Login: "alice", Name: "Alice Ng"},
		CreatedAt: "2026-10-01T09:00:00Z", UpdatedAt: "2026-10-19T12:00:00Z",
		HeadSHA: strings.Repeat("1", 40), HeadRefName: "alice/widgets", BaseRefName: "main",
		Comments: 4, Commits: 3, LastCommitAt: "2026-10-19T11:58:00Z",
		ViewerIsAuthor: true, ReviewDecision: "REVIEW_REQUIRED", MergeStatus: "blocked", ChecksState: "success",
	}
	// The base requires the larger of its protection's and its rulesets'
	// approvals; one of two threads is open.
	if own.ApprovalsGiven == nil || *own.ApprovalsGiven != 1 || own.ApprovalsRequired == nil || *own.ApprovalsRequired != 2 ||
		own.Unresolved == nil || *own.Unresolved != 1 || own.PipelineStatus != CIStatusSuccess {
		t.Errorf("own detail = given %v required %v unresolved %v ci %q", own.ApprovalsGiven, own.ApprovalsRequired, own.Unresolved, own.PipelineStatus)
	}
	own.ApprovalsGiven, own.ApprovalsRequired, own.Unresolved, own.PipelineStatus = nil, nil, nil, ""
	if own != want {
		t.Errorf("own draft = %+v\nwant       %+v", own, want)
	}
	// Comments by others (and a deleted account) are not the author's. A
	// review requested from the viewer is its turn; a base without
	// protection requires no approval.
	if !fork.CrossRepository || !fork.ReviewRequested || !fork.ViewerIsReviewer || fork.ViewerIsAuthor ||
		!fork.Conflicts || fork.MergeStatus != "dirty" || fork.ChecksState != "failure" || fork.ReviewDecision != "" ||
		fork.Author != (PRListAuthor{Login: "carol"}) || fork.ViewerReview != (PRListReview{}) || fork.AuthorActivityAt != "" ||
		fork.ViewerReviewState != "unreviewed" || fork.PipelineStatus != CIStatusFailed ||
		*fork.ApprovalsGiven != 1 || *fork.ApprovalsRequired != 0 || *fork.Unresolved != 0 {
		t.Errorf("fork row = %+v", fork)
	}
	// The author's latest activity is their review thread reply, later than
	// their conversation comment; the viewer's later comment and a pending
	// review are not it. Both of the author's replies follow the viewer's
	// review, which is its activity and its standing change request.
	if changes.ReviewRequested || !changes.ViewerIsReviewer || changes.ViewerReview != (PRListReview{State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-01T10:00:00Z"}) ||
		changes.ChecksState != "" || changes.Conflicts || changes.Commits != 7 || changes.BaseRefName != "release" ||
		changes.AuthorActivityAt != "2026-10-01T12:30:00Z" || changes.ViewerReviewState != "requested_changes" ||
		changes.ViewerActivityAt != "2026-10-01T10:00:00Z" || changes.AuthorReplies != 2 || changes.PipelineStatus != "" ||
		*changes.Unresolved != 2 || *changes.ApprovalsRequired != 0 || *changes.ApprovalsGiven != 0 {
		t.Errorf("changes-requested row = %+v", changes)
	}
}

// A search past one page is read from the page's endCursor, and the read
// stops at prListMaxPages pages even when GitHub says there are more.
func TestGitHubListProjectPRsPagesTheSearch(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PullsSearch" {
			return forgeUnexpected(t, call)
		}
		page := 0
		if after, ok := call.Vars["after"].(string); ok {
			if _, err := fmt.Sscanf(after, "c%d", &page); err != nil {
				t.Errorf("cursor %q: %v", after, err)
			}
		}
		nodes := make([]string, 0, 100)
		for i := range 100 {
			nodes = append(nodes, fmt.Sprintf(`{"id":"PR_%d","number":%d,"author":{"login":"x"},"commits":{"totalCount":1,"nodes":[]}}`, page*100+i, page*100+i+1))
		}
		// An issue node from a search that matched one answers {}.
		nodes = append(nodes, `{}`)
		return githubData(fmt.Sprintf(`{"viewer":{"login":"alice"},"search":{"pageInfo":{"hasNextPage":true,"endCursor":"c%d"},"nodes":[%s]}}`, page+1, strings.Join(nodes, ",")))
	})
	rows, err := core.ListProjectPRs(t.Context(), githubTestRepo())
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	if got := len(calls.all()); got != prListMaxPages {
		t.Fatalf("requests = %d, want %d", got, prListMaxPages)
	}
	if len(rows) != prListMaxPages*100 || rows[len(rows)-1].Ref.Number != prListMaxPages*100 {
		t.Fatalf("rows = %d, last %+v", len(rows), rows[len(rows)-1].Ref)
	}
	if after := calls.all()[1].Vars["after"]; after != "c1" {
		t.Fatalf("second page after = %v, want the first page's endCursor", after)
	}
}

// A search refused only in its rows' check states is read again without
// them: the rows land with a notice saying so, and a refusal of the token
// is remembered, so the next list leaves them out at once. An error
// elsewhere, or a single sign-on refusal, fails the list.
func TestGitHubListLeavesOutRefusedCheckStates(t *testing.T) {
	t.Parallel()
	refusal := `{"type":"FORBIDDEN","message":"Resource not accessible by personal access token","path":["search","nodes",0,"commits","nodes",0,"commit","statusCheckRollup"]}`
	for _, tc := range []struct {
		name       string
		errors     string
		header     http.Header
		fails      bool
		remembered bool
		notice     string
	}{
		{name: "a fine-grained token", errors: `[` + refusal + `]`, remembered: true, notice: "Check states are not shown: forge.example does not let this fine-grained token read them."},
		{name: "a passing error", errors: `[{"message":"Something went wrong","path":["search","nodes",0,"commits","nodes",0,"commit","statusCheckRollup"]}]`, notice: "Check states are not shown: forge.example refused them (Something went wrong)."},
		{name: "an error elsewhere too", errors: `[` + refusal + `,{"message":"no","path":["search","nodes",0,"reviewThreads"]}]`, fails: true},
		{name: "single sign-on", errors: `[` + refusal + `]`, header: http.Header{"X-Github-Sso": {"required; url=https://github.com/orgs/acme/sso?authorization_request=x"}}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				if call.Op != "PullsSearch" {
					return forgeUnexpected(t, call)
				}
				rollup, errs := `,"statusCheckRollup":{"state":"SUCCESS"}`, ""
				if call.Vars["wantChecks"] != true {
					rollup = ""
				} else {
					rollup, errs = `,"statusCheckRollup":null`, `,"errors":`+tc.errors
				}
				return forgeAPIAnswer{Header: tc.header, Body: `{"data":{"viewer":{"login":"alice"},"search":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[` +
					`{"id":"PR_1","number":1,"author":{"login":"x"},"commits":{"totalCount":1,"nodes":[{"commit":{"committedDate":"2026-01-01T00:00:00Z"` + rollup + `}}]}}]}}` + errs + `}`}
			})
			rows, err := core.ListProjectPRs(t.Context(), githubTestRepo())
			if tc.fails {
				if err == nil || errors.As(err, new(*PRListDetailError)) || len(calls.all()) != 1 {
					t.Fatalf("ListProjectPRs = %d rows, %v after %d requests; want the one failed request", len(rows), err, len(calls.all()))
				}
				return
			}
			notice, ok := PRListNotices(err)
			if !ok || notice != tc.notice || len(rows) != 1 || rows[0].ChecksState != "" {
				t.Fatalf("ListProjectPRs = %+v, %v; want the row without its check state and %q", rows, err, tc.notice)
			}
			if got := calls.all(); len(got) != 2 || got[0].Vars["wantChecks"] != true || got[1].Vars["wantChecks"] != false {
				t.Fatalf("requests = %+v, want the list then the list without check states", got)
			}
			if _, err := core.ListProjectPRs(t.Context(), githubTestRepo()); err == nil {
				t.Fatal("the next list carries no notice")
			}
			if first := calls.all()[2].Vars["wantChecks"] == false; first != tc.remembered {
				t.Fatalf("the next list leaves check states out at once = %v, want %v", first, tc.remembered)
			}
		})
	}
}

// Core validates the repository before dispatch and sends nothing for
// one it cannot address; a forge it does not integrate answers
// ErrUnsupportedForge.
func TestCoreListProjectPRsValidatesTheRepository(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer { return forgeUnexpected(t, call) })
	if _, err := core.ListProjectPRs(t.Context(), RepoReference{Forge: "github", Namespace: "acme", Repo: "widgets"}); err == nil || !strings.Contains(err.Error(), "host is required") {
		t.Fatalf("no host: %v", err)
	}
	if _, err := core.ListProjectPRs(t.Context(), RepoReference{Forge: "bitbucket", Host: "bitbucket.org", Namespace: "a", Repo: "b"}); !errors.Is(err, ErrUnsupportedForge) {
		t.Fatalf("unknown forge: %v", err)
	}
	if _, err := (nullForge{}).ListProjectPRs(t.Context(), githubTestRepo()); !errors.Is(err, ErrUnsupportedForge) {
		t.Fatalf("nullForge: %v", err)
	}
	if n := len(calls.all()); n != 0 {
		t.Fatalf("requests = %v", calls.ops())
	}
	if _, err := NewCore().ListProjectPRs(t.Context(), githubTestRepo()); !errors.Is(err, ErrNoForgeAPI) {
		t.Fatalf("no transport: %v", err)
	}
}

// A PR whose review threads run past the search's first 100 has an
// unknown unresolved count, not the count of the first 100: with 101
// threads only the last of which is open, the first page reads all
// resolved. One whose threads fit the page is counted.
func TestGitHubListUnresolvedIsUnknownPastTheFirstThreadPage(t *testing.T) {
	t.Parallel()
	resolved := strings.TrimSuffix(strings.Repeat(`{"isResolved":true},`, 100), ",")
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PullsSearch" {
			return forgeUnexpected(t, call)
		}
		if !strings.Contains(call.Query, "reviewThreads(first: 100) { pageInfo { hasNextPage } nodes { isResolved } }") {
			t.Errorf("PullsSearch does not ask whether the threads run past 100: %s", call.Query)
		}
		node := func(number int, more bool) string {
			return fmt.Sprintf(`{"id":"PR_%d","number":%d,"author":{"login":"x"},"commits":{"totalCount":1,"nodes":[]},`+
				`"reviewThreads":{"pageInfo":{"hasNextPage":%t},"nodes":[%s]}}`, number, number, more, resolved)
		}
		return githubData(fmt.Sprintf(`{"viewer":{"login":"alice"},"search":{"pageInfo":{"hasNextPage":false},"nodes":[%s,%s]}}`, node(1, true), node(2, false)))
	})
	rows, err := core.ListProjectPRs(t.Context(), githubTestRepo())
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListProjectPRs = %d rows, %v", len(rows), err)
	}
	if rows[0].Unresolved != nil {
		t.Errorf("101 threads: unresolved = %d, want unknown", *rows[0].Unresolved)
	}
	if rows[1].Unresolved == nil || *rows[1].Unresolved != 0 {
		t.Errorf("100 resolved threads: unresolved = %v, want 0", rows[1].Unresolved)
	}
}
