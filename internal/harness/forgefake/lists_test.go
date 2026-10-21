package forgefake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
		mine["viewerLatestReview"] != nil || mine["id"] == "" || mustJSON(t, mine["reviewThreads"]) != `{"nodes":[{"isResolved":false}],"pageInfo":{"endCursor":"cursor:1","hasNextPage":false}}` ||
		mustJSON(t, mine["baseRef"]) != `{"refUpdateRule":null,"rules":{"nodes":[]}}` {
		t.Errorf("own draft = %v", mustJSON(t, mine))
	}
	if fork["isCrossRepository"] != true || fork["mergeable"] != "CONFLICTING" || fork["mergeStateStatus"] != "DIRTY" ||
		mustJSON(t, fork["reviewRequests"]) != `{"nodes":[{"requestedReviewer":{"__typename":"User","login":"octo"}},{"requestedReviewer":{"__typename":"User","login":"dave"}}]}` ||
		!strings.Contains(mustJSON(t, fork["commits"]), `"statusCheckRollup":{"state":"FAILURE"}`) {
		t.Errorf("fork = %v", mustJSON(t, fork))
	}
	if mustJSON(t, asked["viewerLatestReview"]) != `{"state":"CHANGES_REQUESTED","submittedAt":"2026-10-03T00:00:00Z"}` ||
		mustJSON(t, asked["latestOpinionatedReviews"]) != `{"nodes":[{"state":"CHANGES_REQUESTED","viewerDidAuthor":true}]}` || asked["reviewDecision"] != "CHANGES_REQUESTED" ||
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
	for name, seed := range map[string]string{
		"github change request":  `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"t","reviewers":["a"],"changeRequests":["a"]}]}]}`,
		"not a reviewer":         `{"repos":[{"forge":"gitlab","project":"a/b","pulls":[{"number":1,"title":"t","changeRequests":["a"]}]}]}`,
		"approved and requested": `{"repos":[{"forge":"gitlab","project":"a/b","pulls":[{"number":1,"title":"t","reviewers":["a"],"changeRequests":["a"],"reviews":[{"author":"a","state":"APPROVED"}]}]}]}`,
	} {
		if _, err := seedJSON(New(Options{}), seed); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
}

// gitlabGraphQLCall sends one operation to the GitLab GraphQL mount for
// host, declaring its operation's variables.
func gitlabGraphQLCall(t *testing.T, srv *httptest.Server, host, op string, vars map[string]any) httpAnswer {
	t.Helper()
	return httpRequest(t, srv, "POST", host, "/gitlab/api/graphql", mustJSON(t, map[string]any{
		"query":         "query " + op + "($fullPath: ID!, $viewer: String!, $after: String) { project(fullPath: $fullPath) { id } }",
		"operationName": op, "variables": vars,
	}), http.Header{"Private-Token": {testAPIToken}})
}

type gitlabPullsAnswer struct {
	Data struct {
		Project *struct {
			MergeRequests struct {
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []map[string]any `json:"nodes"`
			} `json:"mergeRequests"`
		} `json:"project"`
	} `json:"data"`
}

// GitLab's reviewing reads answer the open merge requests the viewer
// reviews, newest update first, with each reviewer's state (an approval
// replaces the viewer's change request, a comment makes it reviewed),
// who approved and the head pipeline; the activity reads the thread
// counts and the head commit's time; the authored reads the viewer's own.
func TestGitLabPullsReadsAnswerTheViewersMergeRequests(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"viewer": "octo", "repos": [{"forge": "gitlab", "project": "grp/r", "pulls": [
	  {"number": 1, "title": "t", "author": "carol", "reviewers": ["octo", "bob", "dave"], "changeRequests": ["octo"],
	   "reviews": [{"author": "bob", "state": "APPROVED"}], "updatedAt": "2026-10-02T00:00:00Z", "committedAt": "2026-10-01T05:00:00Z",
	   "ci": {"jobs": [{"name": "unit", "status": "failed"}]},
	   "threads": [{"path": "a.go", "line": 1, "comments": [{"author": "dave", "body": "x"}]}, {"path": "b.go", "line": 1, "resolved": true, "comments": [{"body": "y"}]}]},
	  {"number": 2, "title": "t", "author": "carol", "reviewers": ["Octo"], "updatedAt": "2026-10-03T00:00:00Z", "comments": [{"author": "octo", "body": "hm"}]},
	  {"number": 3, "title": "mine", "author": "octo", "reviewers": ["bob"]},
	  {"number": 4, "title": "closed", "author": "carol", "reviewers": ["octo"], "state": "closed"}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	read := func(op string) []map[string]any {
		t.Helper()
		var got gitlabPullsAnswer
		decodeBody(t, gitlabGraphQLCall(t, srv, "gitlab.com", op, map[string]any{"fullPath": "grp/r", "viewer": "octo"}), &got)
		if got.Data.Project == nil || got.Data.Project.MergeRequests.PageInfo.HasNextPage {
			t.Fatalf("%s = %+v", op, got)
		}
		return got.Data.Project.MergeRequests.Nodes
	}
	reviewing := read("PullsReviewing")
	if len(reviewing) != 2 || mustJSON(t, reviewing[0]) != `{"approvedBy":{"nodes":[]},"headPipeline":null,"iid":"2","reviewers":{"nodes":[{"mergeRequestInteraction":{"reviewState":"REVIEWED"},"username":"Octo"}]}}` ||
		mustJSON(t, reviewing[1]) != `{"approvedBy":{"nodes":[{"username":"bob"}]},"headPipeline":{"status":"FAILED"},"iid":"1","reviewers":{"nodes":[`+
			`{"mergeRequestInteraction":{"reviewState":"REQUESTED_CHANGES"},"username":"octo"},{"mergeRequestInteraction":{"reviewState":"APPROVED"},"username":"bob"},`+
			`{"mergeRequestInteraction":{"reviewState":"REVIEWED"},"username":"dave"}]}}` {
		t.Fatalf("PullsReviewing = %s", mustJSON(t, reviewing))
	}
	if activity := read("PullsReviewingActivity"); mustJSON(t, activity[1]) != `{"commits":{"nodes":[{"committedDate":"2026-10-01T05:00:00Z"}]},"iid":"1","resolvableDiscussionsCount":2,"resolvedDiscussionsCount":1}` {
		t.Fatalf("PullsReviewingActivity = %s", mustJSON(t, activity))
	}
	if authored := read("PullsAuthored"); len(authored) != 1 || authored[0]["iid"] != "3" || len(read("PullsAuthoredActivity")) != 1 {
		t.Fatalf("PullsAuthored = %s", mustJSON(t, authored))
	}
	// The approval reads answer each merge request's rules: one
	// any-approver rule requiring its readiness approvals.
	if approvals := read("PullsReviewingApprovals"); mustJSON(t, approvals[1]) != `{"approvalState":{"rules":[{"approvalsRequired":0,"type":"ANY_APPROVER"}]},"iid":"1"}` {
		t.Fatalf("PullsReviewingApprovals = %s", mustJSON(t, approvals))
	}
	if approvals := read("PullsAuthoredApprovals"); len(approvals) != 1 || approvals[0]["iid"] != "3" {
		t.Fatalf("PullsAuthoredApprovals = %s", mustJSON(t, approvals))
	}
	var mr map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Fr/merge_requests/1"), &mr)
	if got := glabSend(t, srv, "POST", "projects/grp%2Fr/merge_requests/1/approve", `{"sha":"`+mr["sha"].(string)+`"}`); got.status != 201 {
		t.Fatalf("approve = %d %s", got.status, got.body)
	}
	if after := mustJSON(t, read("PullsReviewing")[1]["reviewers"]); !strings.Contains(after, `{"mergeRequestInteraction":{"reviewState":"APPROVED"},"username":"octo"}`) {
		t.Fatalf("reviewers after approving = %s", after)
	}

	var missing gitlabPullsAnswer
	decodeBody(t, gitlabGraphQLCall(t, srv, "gitlab.com", "PullsReviewing", map[string]any{"fullPath": "grp/none", "viewer": "octo"}), &missing)
	if missing.Data.Project != nil {
		t.Errorf("unknown project = %+v, want a null project", missing)
	}
	drift := httpRequest(t, srv, "POST", "gitlab.com", "/gitlab/api/graphql", mustJSON(t, map[string]any{
		"query": "query PullsReviewing($fullPath: ID!) { project(fullPath: $fullPath) { id } }", "operationName": "PullsReviewing",
		"variables": map[string]any{"fullPath": "grp/r"},
	}), http.Header{"Private-Token": {testAPIToken}})
	if drift.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Errorf("a drifted document = %d, want unhandled", drift.status)
	}
	if github := gitlabGraphQLCall(t, srv, "gitlab.com", "PullsSearch", map[string]any{"fullPath": "grp/r", "viewer": "octo"}); github.status != 404 {
		t.Errorf("a GitHub operation on GitLab = %d, want unhandled", github.status)
	}
	if err := e.SetRefusal(Refusal{Route: "glab graphql PullsAuthored", Status: 403, Times: 1}); err != nil {
		t.Fatal(err)
	}
	if refused := gitlabGraphQLCall(t, srv, "gitlab.com", "PullsAuthored", map[string]any{"fullPath": "grp/r", "viewer": "octo"}); refused.status != 403 || lastInvocation(t, e).Route != "glab graphql PullsAuthored" {
		t.Errorf("refused = %d %s", refused.status, lastInvocation(t, e).Route)
	}
}

// A GraphQL read pages by after, 100 a page.
func TestGitLabPullsReadsPageByAfter(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	pulls := make([]string, 150)
	for i := range pulls {
		pulls[i] = fmt.Sprintf(`{"number":%d,"title":"p%d","author":"octo"}`, i+1, i+1)
	}
	if _, err := seedJSON(e, `{"viewer":"octo","repos":[{"forge":"gitlab","project":"a/many","pulls":[`+strings.Join(pulls, ",")+`]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var first, second gitlabPullsAnswer
	decodeBody(t, gitlabGraphQLCall(t, srv, "gitlab.com", "PullsAuthored", map[string]any{"fullPath": "a/many", "viewer": "octo"}), &first)
	page := first.Data.Project.MergeRequests
	if len(page.Nodes) != 100 || !page.PageInfo.HasNextPage || page.Nodes[0]["iid"] != "150" {
		t.Fatalf("first page = %d nodes, %+v", len(page.Nodes), page.PageInfo)
	}
	decodeBody(t, gitlabGraphQLCall(t, srv, "gitlab.com", "PullsAuthored", map[string]any{"fullPath": "a/many", "viewer": "octo", "after": *page.PageInfo.EndCursor}), &second)
	if rest := second.Data.Project.MergeRequests; len(rest.Nodes) != 50 || rest.PageInfo.HasNextPage || rest.Nodes[49]["iid"] != "1" {
		t.Fatalf("second page = %d nodes, %+v", len(rest.Nodes), rest.PageInfo)
	}
}

// The viewer's events are its comments and approvals on the host's merge
// requests, newest first, after the date, in pages; another user is
// GitLab's 404 and a parameter the route does not implement is unhandled.
func TestGitLabUserEventsAreTheViewersCommentsAndApprovals(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"viewer": "octo", "repos": [
	  {"forge": "gitlab", "project": "grp/a", "id": 11, "pulls": [
	    {"number": 1, "title": "t", "author": "carol", "comments": [{"author": "octo", "body": "a", "createdAt": "2026-10-05T00:00:00Z"}, {"author": "carol", "body": "b", "createdAt": "2026-10-06T00:00:00Z"}],
	     "reviews": [{"author": "octo", "state": "APPROVED", "submittedAt": "2026-10-07T00:00:00Z"}]},
	    {"number": 2, "title": "t", "author": "carol", "threads": [{"path": "a.go", "line": 1, "comments": [{"author": "octo", "body": "c", "createdAt": "2026-09-01T00:00:00Z"}]}]}]},
	  {"forge": "gitlab", "project": "grp/b", "id": 12, "pulls": [
	    {"number": 1, "title": "t", "author": "carol", "comments": [{"author": "octo", "body": "d", "createdAt": "2026-10-08T00:00:00Z"}]}]},
	  {"forge": "gitlab", "host": "gitlab.example", "project": "grp/c", "pulls": [
	    {"number": 1, "title": "t", "author": "carol", "comments": [{"author": "octo", "body": "e", "createdAt": "2026-10-09T00:00:00Z"}]}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var events []map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "users/1/events?per_page=100&after=2026-09-15"), &events)
	// GitLab files a conversation note under target type Note and a
	// comment on the diff under DiffNote, both naming the merge request.
	describe := func(events []map[string]any) string {
		var got []string
		for _, event := range events {
			iid := event["target_iid"]
			if note, ok := event["note"].(map[string]any); ok {
				iid = note["noteable_iid"]
			}
			got = append(got, fmt.Sprintf("%v %v %v !%v %v", event["project_id"], event["action_name"], event["target_type"], iid, event["created_at"]))
		}
		return strings.Join(got, "|")
	}
	if got, want := describe(events), "12 commented on Note !1 2026-10-08T00:00:00Z|11 approved MergeRequest !1 2026-10-07T00:00:00Z|11 commented on Note !1 2026-10-05T00:00:00Z"; got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	var all []map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "users/1/events?per_page=100&after=2026-08-01"), &all)
	if got := describe(all); !strings.HasSuffix(got, "|11 commented on DiffNote !2 2026-09-01T00:00:00Z") {
		t.Fatalf("a thread comment's event = %s, want a DiffNote", got)
	}
	paged := glabGET(t, srv, "gitlab.com", "users/1/events?per_page=2")
	var first []map[string]any
	decodeBody(t, paged, &first)
	if len(first) != 2 || paged.header.Get("X-Next-Page") != "2" {
		t.Fatalf("first page = %d events, next %q", len(first), paged.header.Get("X-Next-Page"))
	}
	if other := glabGET(t, srv, "gitlab.com", "users/2/events?per_page=100"); other.status != 404 || lastInvocation(t, e).Unhandled {
		t.Errorf("another user = %d, want a handled 404", other.status)
	}
	// The action filter keeps one kind, as GitLab's does.
	var comments, approvals []map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "users/1/events?per_page=100&after=2026-09-15&action=commented"), &comments)
	decodeBody(t, glabGET(t, srv, "gitlab.com", "users/1/events?per_page=100&after=2026-09-15&action=approved"), &approvals)
	if got, want := describe(comments), "12 commented on Note !1 2026-10-08T00:00:00Z|11 commented on Note !1 2026-10-05T00:00:00Z"; got != want {
		t.Errorf("commented = %s\nwant        %s", got, want)
	}
	if got, want := describe(approvals), "11 approved MergeRequest !1 2026-10-07T00:00:00Z"; got != want {
		t.Errorf("approved = %s\nwant       %s", got, want)
	}
	if bad := glabGET(t, srv, "gitlab.com", "users/1/events?action=pushed"); bad.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Errorf("an unimplemented action = %d, want unhandled", bad.status)
	}
	if bad := glabGET(t, srv, "gitlab.com", "users/1/events?target_type=note"); bad.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Errorf("an unimplemented filter = %d, want unhandled", bad.status)
	}
}

// A project's hidden events fill their place in a page and are dropped
// from it after, so the page comes back short and still names the next,
// as GitLab's does.
func TestGitLabUserEventsDropHiddenEventsAfterPaging(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"viewer": "octo", "repos": [
	  {"forge": "gitlab", "project": "grp/gone", "id": 21, "eventsHidden": true, "pulls": [
	    {"number": 1, "title": "t", "author": "carol", "comments": [{"author": "octo", "body": "a", "createdAt": "2026-10-09T00:00:00Z"}, {"author": "octo", "body": "b", "createdAt": "2026-10-08T00:00:00Z"}]}]},
	  {"forge": "gitlab", "project": "grp/a", "id": 22, "pulls": [
	    {"number": 1, "title": "t", "author": "carol", "comments": [{"author": "octo", "body": "c", "createdAt": "2026-10-07T00:00:00Z"}, {"author": "octo", "body": "d", "createdAt": "2026-10-06T00:00:00Z"}]}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var first, second []map[string]any
	answer := glabGET(t, srv, "gitlab.com", "users/1/events?per_page=3&action=commented")
	decodeBody(t, answer, &first)
	if len(first) != 1 || first[0]["created_at"] != "2026-10-07T00:00:00Z" || answer.header.Get("X-Next-Page") != "2" {
		t.Fatalf("first page = %v, next %q; want grp/a's newer comment alone and page 2", first, answer.header.Get("X-Next-Page"))
	}
	answer = glabGET(t, srv, "gitlab.com", "users/1/events?per_page=3&page=2&action=commented")
	decodeBody(t, answer, &second)
	if len(second) != 1 || second[0]["created_at"] != "2026-10-06T00:00:00Z" || answer.header.Get("X-Next-Page") != "" {
		t.Fatalf("second page = %v, next %q; want the older comment and no next", second, answer.header.Get("X-Next-Page"))
	}
	if _, err := seedJSON(e, `{"viewer": "octo", "repos": [{"forge": "github", "project": "o/r", "eventsHidden": true}]}`); err == nil || !strings.Contains(err.Error(), "eventsHidden") {
		t.Errorf("eventsHidden on a GitHub repository = %v, want refused", err)
	}
}

// PullsSearch answers a pull's first 100 review threads and says when
// more exist, as GitHub does: with 101, the open last one is past the
// page.
func TestPullsSearchPagesReviewThreads(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	threads := make([]string, 101)
	for i := range threads {
		threads[i] = fmt.Sprintf(`{"path":"a.go","line":%d,"resolved":%t,"comments":[{"body":"x"}]}`, i+1, i < 100)
	}
	if _, err := seedJSON(e, `{"viewer":"octo","repos":[{"forge":"github","project":"o/r","pulls":[{"number":1,"title":"t","author":"carol","threads":[`+strings.Join(threads, ",")+`]}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var got struct {
		Data struct {
			Search struct {
				Nodes []struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	decodeBody(t, graphQL(t, srv, "github.com", "PullsSearch", map[string]any{"query": "is:pr is:open repo:o/r"}), &got)
	if len(got.Data.Search.Nodes) != 1 {
		t.Fatalf("search = %+v", got)
	}
	threadsPage := got.Data.Search.Nodes[0].ReviewThreads
	if len(threadsPage.Nodes) != 100 || !threadsPage.PageInfo.HasNextPage || slices.ContainsFunc(threadsPage.Nodes, func(n struct {
		IsResolved bool `json:"isResolved"`
	}) bool {
		return !n.IsResolved
	}) {
		t.Fatalf("review threads = %d nodes, more %v", len(threadsPage.Nodes), threadsPage.PageInfo.HasNextPage)
	}
}
