package git

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	if own != want {
		t.Errorf("own draft = %+v\nwant       %+v", own, want)
	}
	if !fork.CrossRepository || !fork.ReviewRequested || !fork.ViewerIsReviewer || fork.ViewerIsAuthor ||
		!fork.Conflicts || fork.MergeStatus != "dirty" || fork.ChecksState != "failure" || fork.ReviewDecision != "" ||
		fork.Author != (PRListAuthor{Login: "carol"}) || fork.ViewerReview != (PRListReview{}) {
		t.Errorf("fork row = %+v", fork)
	}
	if changes.ReviewRequested || !changes.ViewerIsReviewer || changes.ViewerReview != (PRListReview{State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-01T10:00:00Z"}) ||
		changes.ChecksState != "" || changes.Conflicts || changes.Commits != 7 || changes.BaseRefName != "release" {
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
			fmt.Sscanf(after, "c%d", &page)
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

// The recorded list answer parses into rows: the viewer's own draft, a
// fork's merge request with the viewer among its reviewers, and one a
// reviewer requested changes on.
func TestGitLabListProjectPRsParsesTheList(t *testing.T) {
	t.Parallel()
	answer := readTestdata(t, "gitlab-mr-list.json")
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "user":
			return forgeAPIAnswer{Body: `{"id":1,"username":"alice"}`}
		case strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/merge_requests?"):
			return forgeAPIAnswer{Body: answer}
		}
		return forgeUnexpected(t, call)
	})
	rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	list := calls.all()[0]
	query, _ := url.ParseQuery(strings.SplitN(list.Path, "?", 2)[1])
	if query.Encode() != "order_by=updated_at&per_page=100&sort=desc&state=opened" {
		t.Fatalf("list query = %s", query.Encode())
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	own, fork, changes := rows[0], rows[1], rows[2]
	want := PRListRow{
		Ref: PRReference{Forge: "gitlab", Host: testForgeHost, Namespace: "grp/sub", Repo: "app", Number: 21},
		URL: "https://gitlab.example/grp/sub/app/-/merge_requests/21", Title: "Draft: speed up builds", Draft: true,
		Author:    PRListAuthor{Login: "alice", Name: "Alice Ng"},
		CreatedAt: "2026-10-20T22:05:15.053Z", UpdatedAt: "2026-10-20T22:05:16.934Z",
		HeadSHA: strings.Repeat("4", 40), HeadRefName: "alice/builds", BaseRefName: "main",
		Comments: 1, ViewerIsAuthor: true, MergeStatus: "draft_status",
	}
	if own != want {
		t.Errorf("own draft = %+v\nwant       %+v", own, want)
	}
	if !fork.CrossRepository || !fork.ViewerIsReviewer || !fork.ReviewRequested || fork.ViewerIsAuthor ||
		!fork.Conflicts || !fork.UnresolvedBlocking || fork.ReviewDecision != "REVIEW_REQUIRED" || fork.Comments != 9 || fork.NodeID != "" {
		t.Errorf("fork row = %+v", fork)
	}
	if changes.ViewerIsReviewer || changes.ReviewRequested || changes.ReviewDecision != "CHANGES_REQUESTED" || changes.UnresolvedBlocking || changes.CrossRepository {
		t.Errorf("changes-requested row = %+v", changes)
	}
}

// A login GitLab will not name reads as no one's: the rows still list,
// with every viewer flag false.
func TestGitLabListProjectPRsWithoutAViewer(t *testing.T) {
	t.Parallel()
	answer := readTestdata(t, "gitlab-mr-list.json")
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path == "user" {
			return forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"403 Forbidden"}`}
		}
		return forgeAPIAnswer{Body: answer}
	})
	rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListProjectPRs = %d rows, %v", len(rows), err)
	}
	for _, row := range rows {
		if row.ViewerIsAuthor || row.ViewerIsReviewer || row.ReviewRequested {
			t.Errorf("row %d reads as the unknown viewer's: %+v", row.Ref.Number, row)
		}
	}
}

// The list follows X-Next-Page for full pages and stops at
// prListMaxPages.
func TestGitLabListProjectPRsPagesTheList(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path == "user" {
			return forgeAPIAnswer{Body: `{"username":"alice"}`}
		}
		query, _ := url.ParseQuery(strings.SplitN(call.Path, "?", 2)[1])
		page := 1
		fmt.Sscanf(query.Get("page"), "%d", &page)
		mrs := make([]string, 100)
		for i := range mrs {
			mrs[i] = fmt.Sprintf(`{"iid":%d,"project_id":1,"source_project_id":1,"blocking_discussions_resolved":true}`, (page-1)*100+i+1)
		}
		return forgeAPIAnswer{Header: http.Header{"X-Next-Page": {fmt.Sprint(page + 1)}}, Body: "[" + strings.Join(mrs, ",") + "]"}
	})
	rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	lists := 0
	for _, call := range calls.all() {
		if call.Path != "user" {
			lists++
		}
	}
	if lists != prListMaxPages || len(rows) != prListMaxPages*100 {
		t.Fatalf("list pages = %d, rows = %d", lists, len(rows))
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
