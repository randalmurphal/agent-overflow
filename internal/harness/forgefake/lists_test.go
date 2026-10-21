package forgefake

import (
	"fmt"
	"strings"
	"testing"
)

// listFixture seeds the list fields: a GitHub repository whose viewer
// octo authored a draft, has a review requested on a fork's pull and
// requested changes on a third, and a GitLab project with reviewers.
const listFixture = `{
  "viewer": "octo",
  "repos": [
    {"forge": "github", "project": "acme/list", "pulls": [
      {"number": 1, "title": "Mine", "author": "octo", "draft": true, "createdAt": "2026-10-01T00:00:00Z", "updatedAt": "2026-10-02T00:00:00Z",
       "committedAt": "2026-10-01T12:00:00Z", "commits": 3, "comments": [{"body": "one"}],
       "threads": [{"path": "a.go", "line": 1, "comments": [{"body": "two"}, {"body": "three"}]}],
       "ci": {"jobs": [{"name": "unit", "status": "success"}]}},
      {"number": 2, "title": "Fork", "author": "carol", "headRepo": "carol/list", "reviewers": ["octo", "dave"], "mergeable": "conflicts",
       "ci": {"jobs": [{"name": "unit", "status": "running"}, {"name": "lint", "status": "failed"}]}},
      {"number": 3, "title": "Asked", "author": "carol", "reviews": [{"author": "octo", "state": "CHANGES_REQUESTED", "body": "no", "submittedAt": "2026-10-03T00:00:00Z"}]},
      {"number": 4, "title": "Closed", "state": "closed"}
    ]},
    {"forge": "gitlab", "project": "grp/list", "id": 4400, "merge": {"discussionsMustResolve": true}, "pulls": [
      {"number": 7, "title": "Draft: mine", "author": "octo", "reviewers": ["bob"], "updatedAt": "2026-10-05T00:00:00Z"},
      {"number": 8, "title": "Fork", "author": "carol", "headRepo": "carol/list", "reviewers": ["dave", "octo"], "mergeable": "conflicts",
       "readiness": {"requiredApprovals": 1}, "threads": [{"path": "a.go", "line": 1, "comments": [{"body": "fix"}]}]}
    ]}
  ]
}`

type searchAnswer struct {
	Data struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Search struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []map[string]any `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
}

func TestPullsSearchAnswersTheRepositorysOpenPulls(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, listFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var got searchAnswer
	decodeBody(t, graphQL(t, srv, "github.com", "PullsSearch", map[string]any{"query": "is:pr is:open repo:acme/list"}), &got)
	nodes := got.Data.Search.Nodes
	if got.Data.Viewer.Login != "octo" || len(nodes) != 3 || got.Data.Search.PageInfo.HasNextPage {
		t.Fatalf("search = %+v", got)
	}
	asked, fork, mine := nodes[0], nodes[1], nodes[2]
	if mine["number"] != float64(1) || mine["isDraft"] != true || mine["viewerDidAuthor"] != true || mine["totalCommentsCount"] != float64(3) ||
		mine["createdAt"] != "2026-10-01T00:00:00Z" || mine["updatedAt"] != "2026-10-02T00:00:00Z" ||
		mustJSON(t, mine["commits"]) != `{"nodes":[{"commit":{"committedDate":"2026-10-01T12:00:00Z","statusCheckRollup":{"state":"SUCCESS"}}}],"totalCount":3}` ||
		mine["viewerLatestReview"] != nil || mine["id"] == "" {
		t.Errorf("own draft = %v", mustJSON(t, mine))
	}
	if fork["isCrossRepository"] != true || fork["mergeable"] != "CONFLICTING" || fork["mergeStateStatus"] != "DIRTY" ||
		mustJSON(t, fork["reviewRequests"]) != `{"nodes":[{"requestedReviewer":{"__typename":"User","login":"octo"}},{"requestedReviewer":{"__typename":"User","login":"dave"}}]}` ||
		!strings.Contains(mustJSON(t, fork["commits"]), `"statusCheckRollup":{"state":"FAILURE"}`) {
		t.Errorf("fork = %v", mustJSON(t, fork))
	}
	if mustJSON(t, asked["viewerLatestReview"]) != `{"state":"CHANGES_REQUESTED","submittedAt":"2026-10-03T00:00:00Z"}` ||
		mustJSON(t, asked["latestOpinionatedReviews"]) != `{"nodes":[{"viewerDidAuthor":true}]}` || asked["reviewDecision"] != "CHANGES_REQUESTED" ||
		!strings.Contains(mustJSON(t, asked["commits"]), `"statusCheckRollup":null`) {
		t.Errorf("changes requested = %v", mustJSON(t, asked))
	}

	for _, query := range []string{"is:pr repo:acme/list", "is:pr is:open repo:acme/list sort:updated", "is:issue is:open repo:acme/list"} {
		if other := graphQL(t, srv, "github.com", "PullsSearch", map[string]any{"query": query}); other.status != 404 {
			t.Errorf("query %q = %+v, want unhandled", query, other)
		}
	}
	unknown := graphQL(t, srv, "github.com", "PullsSearch", map[string]any{"query": "is:pr is:open repo:acme/none"})
	if unknown.status != 200 || !strings.Contains(unknown.body, `"search":null`) || !strings.Contains(unknown.body, "cannot be searched") {
		t.Errorf("unknown repository = %+v", unknown)
	}
	if elsewhere := graphQL(t, srv, "ghe.example.com", "PullsSearch", map[string]any{"query": "is:pr is:open repo:acme/list"}); !strings.Contains(elsewhere.body, `"search":null`) {
		t.Errorf("another host = %+v", elsewhere)
	}
}

func TestPullsSearchPagesByAfter(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	pulls := make([]string, 150)
	for i := range pulls {
		pulls[i] = fmt.Sprintf(`{"number":%d,"title":"p%d"}`, i+1, i+1)
	}
	if _, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/many","pulls":[`+strings.Join(pulls, ",")+`]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var first, second searchAnswer
	decodeBody(t, graphQL(t, srv, "github.com", "PullsSearch", map[string]any{"query": "is:pr is:open repo:a/many"}), &first)
	page := first.Data.Search
	if len(page.Nodes) != 100 || !page.PageInfo.HasNextPage || page.Nodes[0]["number"] != float64(150) {
		t.Fatalf("first page = %d nodes, %+v, first #%v", len(page.Nodes), page.PageInfo, page.Nodes[0]["number"])
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PullsSearch", map[string]any{"query": "is:pr is:open repo:a/many", "after": page.PageInfo.EndCursor}), &second)
	if rest := second.Data.Search; len(rest.Nodes) != 50 || rest.PageInfo.HasNextPage || rest.Nodes[49]["number"] != float64(1) {
		t.Fatalf("second page = %d nodes, %+v", len(rest.Nodes), rest.PageInfo)
	}
}

// A review ends the reviewer's GitHub request, as GitHub does.
func TestGitHubReviewEndsTheReviewersRequest(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, listFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	if review := httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/list/pulls/2/reviews", `{"event":"COMMENT","body":"hm"}`, nil); review.status != 200 {
		t.Fatalf("review = %+v", review)
	}
	var got searchAnswer
	decodeBody(t, graphQL(t, srv, "github.com", "PullsSearch", map[string]any{"query": "is:pr is:open repo:acme/list"}), &got)
	if requests := mustJSON(t, got.Data.Search.Nodes[1]["reviewRequests"]); requests != `{"nodes":[{"requestedReviewer":{"__typename":"User","login":"dave"}}]}` {
		t.Fatalf("requests after the viewer's review = %s", requests)
	}
}

func TestGitLabMergeRequestListCarriesTheListFields(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, listFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var got []map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Flist/merge_requests?order_by=updated_at&per_page=100&sort=desc&state=opened"), &got)
	if len(got) != 2 {
		t.Fatalf("list = %v", got)
	}
	fork, mine := got[0], got[1]
	if mine["draft"] != true || mine["detailed_merge_status"] != "draft_status" || mine["updated_at"] != "2026-10-05T00:00:00Z" ||
		mine["created_at"] != "2026-01-01T00:00:00Z" || mustJSON(t, mine["author"]) != `{"name":"","username":"octo"}` ||
		mustJSON(t, mine["reviewers"]) != `[{"name":"","state":"active","username":"bob"}]` || mine["user_notes_count"] != float64(0) ||
		mine["source_project_id"] != float64(4400) || mine["blocking_discussions_resolved"] != true || mine["has_conflicts"] != false {
		t.Errorf("own draft = %v", mustJSON(t, mine))
	}
	if fork["source_project_id"] == float64(4400) || fork["project_id"] != float64(4400) || fork["has_conflicts"] != true ||
		fork["blocking_discussions_resolved"] != false || fork["detailed_merge_status"] != "conflict" || fork["user_notes_count"] != float64(1) ||
		!strings.Contains(mustJSON(t, fork["reviewers"]), `"username":"octo"`) || fork["web_url"] != "https://gitlab.com/grp/list/-/merge_requests/8" {
		t.Errorf("fork = %v", mustJSON(t, fork))
	}
	if _, has := fork["head_pipeline"]; has {
		t.Error("the list answered a head pipeline, which GitLab's list does not carry")
	}
}

func TestSeedRejectsMalformedListFields(t *testing.T) {
	t.Parallel()
	for name, pull := range map[string]string{
		"empty reviewer":     `{"number":1,"title":"t","reviewers":[""]}`,
		"duplicate reviewer": `{"number":1,"title":"t","reviewers":["a","A"]}`,
		"bad createdAt":      `{"number":1,"title":"t","createdAt":"yesterday"}`,
		"bad updatedAt":      `{"number":1,"title":"t","updatedAt":"2026-10-01"}`,
		"bad committedAt":    `{"number":1,"title":"t","committedAt":"soon"}`,
		"negative commits":   `{"number":1,"title":"t","commits":-1}`,
	} {
		if _, err := seedJSON(New(Options{}), `{"repos":[{"forge":"github","project":"a/b","pulls":[`+pull+`]}]}`); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
}
