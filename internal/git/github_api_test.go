package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/forgeattach"
)

// forgeAPICall is one request a forge sent: a GitHub GraphQL operation
// with its variables, or a REST method and path (with query) relative to
// the forge's REST base.
type forgeAPICall struct {
	// RESTBase is the fake's REST base URL, for a Link header.
	RESTBase string
	Host     string
	Op       string
	Query    string
	Vars     map[string]any
	Method   string
	Path     string
	Header   http.Header
	Body     []byte
}

// forgeAPIAnswer is the fake's reply. A zero Status is 200.
type forgeAPIAnswer struct {
	Status int
	Header http.Header
	Body   string
}

type forgeAPICalls struct {
	mu    sync.Mutex
	calls []forgeAPICall
}

func (c *forgeAPICalls) all() []forgeAPICall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

func (c *forgeAPICalls) ops() []string {
	var out []string
	for _, call := range c.all() {
		if call.Op != "" {
			out = append(out, call.Op)
		} else {
			out = append(out, call.Method+" "+call.Path)
		}
	}
	return out
}

// newForgeAPICore is a Core whose forge API transport is pointed, as an
// isolated boot's is, at an httptest forge that answer serves: GitHub at
// /github/, GitLab at /gitlab/api/v4/.
func newForgeAPICore(t *testing.T, answer func(call forgeAPICall) forgeAPIAnswer) (*Core, *forgeAPICalls) {
	t.Helper()
	svc, calls := newForgeAPIService(t, answer)
	return NewCore(WithForgeAPI(svc)), calls
}

// newForgeAPIService is newForgeAPICore's transport, for a Core that
// needs other options beside it.
func newForgeAPIService(t *testing.T, answer func(call forgeAPICall) forgeAPIAnswer) (*forgeapi.Service, *forgeAPICalls) {
	t.Helper()
	calls := &forgeAPICalls{}
	var restBase string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := forgeAPICall{RESTBase: restBase, Host: r.Host, Method: r.Method, Header: r.Header.Clone(), Body: body}
		switch {
		case r.URL.Path == "/github/graphql":
			var payload struct {
				Query         string         `json:"query"`
				OperationName string         `json:"operationName"`
				Variables     map[string]any `json:"variables"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			call.Op, call.Query, call.Vars = payload.OperationName, payload.Query, payload.Variables
		case strings.HasPrefix(r.URL.Path, "/github/rest/"):
			call.Path = strings.TrimPrefix(r.URL.EscapedPath(), "/github/rest/")
		case strings.HasPrefix(r.URL.Path, "/gitlab/api/v4/"):
			call.Path = strings.TrimPrefix(r.URL.EscapedPath(), "/gitlab/api/v4/")
		default:
			call.Path = r.URL.EscapedPath()
		}
		if r.URL.RawQuery != "" {
			call.Path += "?" + r.URL.RawQuery
		}
		calls.mu.Lock()
		calls.calls = append(calls.calls, call)
		calls.mu.Unlock()
		reply := answer(call)
		for name, values := range reply.Header {
			w.Header()[name] = values
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		if reply.Status == 0 {
			reply.Status = http.StatusOK
		}
		w.WriteHeader(reply.Status)
		_, _ = io.WriteString(w, reply.Body)
	}))
	t.Cleanup(srv.Close)
	restBase = srv.URL + "/github/rest/"
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: srv.URL, Token: "test-token"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, calls
}

func githubData(data string) forgeAPIAnswer {
	return forgeAPIAnswer{Body: `{"data":` + data + `}`}
}

func forgeUnexpected(t *testing.T, call forgeAPICall) forgeAPIAnswer {
	t.Errorf("unexpected forge request %q %s %s", call.Op, call.Method, call.Path)
	return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"Not Found"}`}
}

func githubTestRef(number int) PRReference {
	return PRReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r", Number: number}
}

var allParts = PRReadParts{Detail: true, Threads: true, CI: true}

// The recorded PRTick answers parse into the detail, threads and pipeline
// the review pane shows, from one request each.
func TestReadPRParsesRecordedTicks(t *testing.T) {
	t.Parallel()
	read := func(t *testing.T, fixture string) PRRead {
		t.Helper()
		answer := readTestdata(t, fixture)
		core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if call.Op != "PRTick" {
				return forgeUnexpected(t, call)
			}
			return forgeAPIAnswer{Body: answer}
		})
		got, err := core.ReadPR(t.Context(), githubTestRef(1), allParts, nil, nil)
		if err != nil {
			t.Fatalf("ReadPR: %v", err)
		}
		if ops := calls.ops(); len(ops) != 1 {
			t.Fatalf("requests = %v, want the one PRTick", ops)
		}
		if !got.HasCI {
			t.Fatal("a read that asked for CI does not report it")
		}
		for _, check := range got.Detail.Checks.Checks {
			if strings.HasPrefix(check.StartedAt, "0001-") || strings.HasPrefix(check.CompletedAt, "0001-") {
				t.Fatalf("zero time in %+v", check)
			}
		}
		return got
	}

	t.Run("checks, threads and a named reviewer", func(t *testing.T) {
		t.Parallel()
		got := read(t, "github-pr-tick-cli-14571.json")
		d := got.Detail
		if d.Number != 14571 || d.State != "open" || d.AuthorLogin != "BagToad" || d.AuthorName != "Kynan Ware" ||
			d.ViewerIsAuthor || d.Mergeability != MergeabilityClean || d.ReviewDecision != "APPROVED" || d.HeadSHA == "" {
			t.Fatalf("detail = %+v", d)
		}
		if len(d.LatestReviews) != 1 || d.LatestReviews[0].AuthorLogin != "babakks" || d.LatestReviews[0].AuthorName != "Babak K. Shandiz" ||
			d.LatestReviews[0].State != "APPROVED" || d.LatestReviews[0].CommitSHA == "" {
			t.Fatalf("latest reviews = %+v", d.LatestReviews)
		}
		if c := d.Checks; c.Total != 39 || c.Success != 12 || c.Skipped != 27 || len(c.Checks) != 39 {
			t.Fatalf("check summary = %+v", c)
		}
		if len(got.Threads) != 8 {
			t.Fatalf("threads = %d, want 8", len(got.Threads))
		}
		for _, thread := range got.Threads {
			if thread.Path == "" || !thread.IsResolvable || thread.Line == nil || len(thread.Comments) != 1 || thread.Comments[0].DatabaseID == 0 {
				t.Fatalf("thread = %+v", thread)
			}
		}
		stages := map[string]int{}
		for _, stage := range got.CI.Stages {
			stages[stage.Name] = len(stage.Jobs)
		}
		want := map[string]int{"PR Triaging": 28, "Unit and Integration Tests": 6, "Code Scanning": 2, "Lint": 2, githubCIExternalStage: 1}
		if fmt.Sprint(stages) != fmt.Sprint(want) {
			t.Fatalf("stages = %v, want %v", stages, want)
		}
		if last := got.CI.Stages[len(got.CI.Stages)-1]; last.Name != githubCIExternalStage || last.Jobs[0].Name != "CodeQL" || last.Jobs[0].LogsAvailable {
			t.Fatalf("external stage = %+v", last)
		}
		if got.CI.Status != CIStatusSuccess {
			t.Fatalf("pipeline status = %q", got.CI.Status)
		}
	})

	t.Run("a conversation comment", func(t *testing.T) {
		t.Parallel()
		got := read(t, "github-pr-tick-cli-14580.json")
		if got.Detail.Checks.Total != 32 || len(got.Detail.LatestReviews) != 0 {
			t.Fatalf("detail = %+v", got.Detail)
		}
		if len(got.Threads) != 1 || got.Threads[0].Path != "" || got.Threads[0].IsResolvable ||
			got.Threads[0].Comments[0].AuthorLogin != "elopezgomez567-stack" || got.Threads[0].Comments[0].AuthorName != "" {
			t.Fatalf("threads = %+v", got.Threads)
		}
	})

	t.Run("the viewer's merged PR with no rollup", func(t *testing.T) {
		t.Parallel()
		got := read(t, "github-pr-tick-own-1.json")
		d := got.Detail
		if d.State != "merged" || !d.ViewerIsAuthor || d.AuthorName != "" || d.Checks.Total != 0 || d.Mergeability != MergeabilityChecking || d.ReviewDecision != "" {
			t.Fatalf("detail = %+v", d)
		}
		if len(got.CI.Stages) != 0 {
			t.Fatalf("pipeline without a rollup = %+v", got.CI)
		}
		if len(got.Threads) != 3 || got.Threads[2].Path != "" {
			t.Fatalf("threads = %+v, want two review threads then the comment", got.Threads)
		}
	})

	t.Run("a closed draft with nothing on it", func(t *testing.T) {
		t.Parallel()
		got := read(t, "github-pr-tick-cli-11000.json")
		d := got.Detail
		if d.State != "closed" || !d.Draft || d.ReviewDecision != "REVIEW_REQUIRED" || d.AuthorName != "Eugene" || d.Mergeability != MergeabilityClean {
			t.Fatalf("detail = %+v", d)
		}
		if len(got.Threads) != 0 || d.Checks.Total != 0 {
			t.Fatalf("threads %v checks %+v", got.Threads, d.Checks)
		}
	})
}

// Actor shapes the recorded ticks do not cover: a Bot PR author and a Bot
// reviewer (no name key), a User whose name is null, a deleted account (a
// null author), and a minimized conversation comment, which is not shown.
func TestReadPRActorShapes(t *testing.T) {
	t.Parallel()
	const answer = `{"data":{"viewer":{"login":"me"},"repository":{"pullRequest":{
		"number":5,"title":"t","body":"","state":"OPEN","isDraft":false,"baseRefName":"main","headRefName":"f","headRefOid":"abc","url":"u",
		"additions":0,"deletions":0,"changedFiles":0,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":null,
		"author":{"login":"dependabot"},
		"latestReviews":{"nodes":[
			{"author":{"login":"copilot-pull-request-reviewer"},"body":"","submittedAt":"2026-01-01T00:00:00Z","state":"COMMENTED","commit":{"oid":"abc"}},
			{"author":{"login":"plain","name":null},"body":"","submittedAt":"2026-01-01T00:00:00Z","state":"APPROVED","commit":{"oid":"abc"}},
			{"author":null,"body":"","submittedAt":"2026-01-01T00:00:00Z","state":"APPROVED","commit":{"oid":"abc"}}
		]},
		"commits":{"nodes":[{"commit":{"id":"C_1","statusCheckRollup":null}}]},
		"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
			{"id":"T1","isResolved":false,"isOutdated":false,"path":"a.go","line":3,"startLine":null,"diffSide":"RIGHT","startDiffSide":null,"subjectType":"LINE",
			 "comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
				{"id":"c1","databaseId":11,"author":{"login":"github-actions"},"body":"bot","createdAt":"2026-01-01T00:00:00Z","replyTo":null},
				{"id":"c2","databaseId":12,"author":null,"body":"ghost","createdAt":"2026-01-01T00:00:01Z","replyTo":{"id":"c1","databaseId":11}}
			 ]}}
		]},
		"comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
			{"id":"k1","databaseId":21,"author":{"login":"plain","name":null},"body":"shown","createdAt":"2026-01-01T00:00:00Z","isMinimized":false},
			{"id":"k2","databaseId":22,"author":{"login":"spammer","name":"S"},"body":"hidden","createdAt":"2026-01-01T00:00:01Z","isMinimized":true}
		]}
	}}}}`
	core, _ := newForgeAPICore(t, func(forgeAPICall) forgeAPIAnswer { return forgeAPIAnswer{Body: answer} })
	got, err := core.ReadPR(t.Context(), githubTestRef(5), allParts, nil, nil)
	if err != nil {
		t.Fatalf("ReadPR: %v", err)
	}
	d := got.Detail
	if d.AuthorLogin != "dependabot" || d.AuthorName != "" || d.ViewerIsAuthor {
		t.Fatalf("PR author = %q %q viewer %v", d.AuthorLogin, d.AuthorName, d.ViewerIsAuthor)
	}
	if len(d.LatestReviews) != 2 || d.LatestReviews[0].AuthorLogin != "copilot-pull-request-reviewer" || d.LatestReviews[0].AuthorName != "" ||
		d.LatestReviews[1].AuthorLogin != "plain" || d.LatestReviews[1].AuthorName != "" {
		t.Fatalf("latest reviews = %+v, want the two with an author", d.LatestReviews)
	}
	if len(got.Threads) != 2 {
		t.Fatalf("threads = %+v, want the review thread and the unminimized comment", got.Threads)
	}
	review := got.Threads[0].Comments
	if len(review) != 2 || review[0].AuthorLogin != "github-actions" || review[1].AuthorLogin != "" || review[1].ReplyTo == nil || review[1].ReplyTo.DatabaseID != 11 {
		t.Fatalf("review thread comments = %+v", review)
	}
	if c := got.Threads[1].Comments[0]; c.Body != "shown" || c.AuthorLogin != "plain" || c.AuthorName != "" {
		t.Fatalf("conversation comment = %+v", c)
	}
}

// The PRTick document is the same every read; the wanted parts are its
// @include variables, so a part nobody asked for is not requested.
func TestReadPRRequestsOnlyTheWantedParts(t *testing.T) {
	t.Parallel()
	answer := readTestdata(t, "github-pr-tick-cli-14571.json")
	cases := []struct {
		want                    PRReadParts
		detail, threads, checks bool
	}{
		{PRReadParts{CI: true}, false, false, true},
		{PRReadParts{Threads: true}, false, true, false},
		{PRReadParts{Detail: true}, true, false, true},
		{PRReadParts{Detail: true, Threads: true}, true, true, true},
	}
	var query string
	for _, tc := range cases {
		core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer { return forgeAPIAnswer{Body: answer} })
		got, err := core.ReadPR(t.Context(), githubTestRef(14571), tc.want, nil, nil)
		if err != nil {
			t.Fatalf("%+v: %v", tc.want, err)
		}
		sent := calls.all()
		if len(sent) != 1 || sent[0].Op != "PRTick" || sent[0].Host != testForgeHost {
			t.Fatalf("%+v: requests = %+v", tc.want, sent)
		}
		vars := sent[0].Vars
		if vars["owner"] != "o" || vars["name"] != "r" || vars["number"] != float64(14571) ||
			vars["wantDetail"] != tc.detail || vars["wantThreads"] != tc.threads || vars["wantChecks"] != tc.checks {
			t.Fatalf("%+v: variables = %v", tc.want, vars)
		}
		if query != "" && sent[0].Query != query {
			t.Fatalf("%+v: the PRTick document changed with the parts", tc.want)
		}
		query = sent[0].Query
		if (len(got.Threads) > 0) != tc.want.Threads || (got.Detail.Number != 0) != tc.want.Detail || got.HasCI != tc.want.CI {
			t.Fatalf("%+v: decoded parts %+v", tc.want, got)
		}
	}
	for _, gate := range []string{
		"viewer @include(if: $wantDetail)",
		"... @include(if: $wantDetail)",
		"commits(last: 1) @include(if: $wantChecks)",
		"reviewThreads(first: 100) @include(if: $wantThreads)",
		"comments(first: 100) @include(if: $wantThreads)",
	} {
		if !strings.Contains(query, gate) {
			t.Fatalf("PRTick lacks %q", gate)
		}
	}
	if _, err := NewCore().ReadPR(t.Context(), githubTestRef(1), PRReadParts{}, nil, nil); err == nil {
		t.Fatal("a read naming no part succeeded")
	}
}

func githubThreadJSON(id string, comments []string, hasNext bool) string {
	return fmt.Sprintf(`{"id":%q,"isResolved":false,"isOutdated":false,"path":"a.go","line":1,"startLine":null,"diffSide":"RIGHT","startDiffSide":null,"subjectType":"LINE","comments":{"pageInfo":{"hasNextPage":%t,"endCursor":"c-%s"},"nodes":[%s]}}`,
		id, hasNext, id, strings.Join(comments, ","))
}

func githubCommentsJSON(prefix string, from, n int, reply bool) []string {
	out := make([]string, 0, n)
	for i := from; i < from+n; i++ {
		replyTo := "null"
		if reply {
			replyTo = `{"id":"root","databaseId":1}`
		}
		out = append(out, fmt.Sprintf(`{"id":"%s%d","databaseId":%d,"author":{"login":"u"},"body":"b%d","createdAt":"2026-01-01T00:00:00Z","replyTo":%s,"isMinimized":false}`, prefix, i, i+1, i, replyTo))
	}
	return out
}

func githubContextsJSON(from, n int) []string {
	out := make([]string, 0, n)
	for i := from; i < from+n; i++ {
		out = append(out, fmt.Sprintf(`{"__typename":"CheckRun","databaseId":%d,"name":"job%d","status":"COMPLETED","conclusion":"SUCCESS","startedAt":null,"completedAt":null,"detailsUrl":"https://github.com/o/r/actions/runs/7/job/%d","checkSuite":{"workflowRun":{"databaseId":7,"url":"https://github.com/o/r/actions/runs/7","workflow":{"name":"CI"}}}}`, i+1, i, i+1))
	}
	return out
}

// Every connection past its first page is read through its page
// operation within the same read: review threads, a thread's comments
// past 100, conversation comments past 100 and check contexts.
func TestReadPRPagesEveryConnection(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Op {
		case "PRTick":
			return githubData(`{"viewer":{"login":"v"},"repository":{"pullRequest":{"number":5,"title":"t","author":{"login":"a"},"state":"OPEN",
"latestReviews":{"nodes":[]},
"commits":{"nodes":[{"commit":{"id":"C_1","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":true,"endCursor":"ctx-1"},"nodes":[` + strings.Join(githubContextsJSON(0, 100), ",") + `]}}}}]},
"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"threads-1"},"nodes":[` + githubThreadJSON("T1", githubCommentsJSON("t1c", 0, 100, false), true) + `]},
"comments":{"pageInfo":{"hasNextPage":true,"endCursor":"comments-1"},"nodes":[` + strings.Join(githubCommentsJSON("ic", 0, 100, false), ",") + `]}}}}`)
		case "PRThreadsPage":
			if call.Vars["after"] != "threads-1" || call.Vars["number"] != float64(5) {
				t.Errorf("PRThreadsPage variables = %v", call.Vars)
			}
			return githubData(`{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"threads-2"},"nodes":[` + githubThreadJSON("T2", githubCommentsJSON("t2c", 0, 1, false), false) + `]}}}}`)
		case "ThreadCommentsPage":
			if call.Vars["threadID"] != "T1" || call.Vars["after"] != "c-T1" {
				t.Errorf("ThreadCommentsPage variables = %v", call.Vars)
			}
			return githubData(`{"node":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":"x"},"nodes":[` + strings.Join(githubCommentsJSON("t1c", 100, 30, true), ",") + `]}}}`)
		case "PRCommentsPage":
			if call.Vars["after"] != "comments-1" {
				t.Errorf("PRCommentsPage variables = %v", call.Vars)
			}
			return githubData(`{"repository":{"pullRequest":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":"y"},"nodes":[` + strings.Join(githubCommentsJSON("ic", 100, 50, false), ",") + `]}}}}`)
		case "RollupContextsPage":
			if call.Vars["commitID"] != "C_1" || call.Vars["after"] != "ctx-1" {
				t.Errorf("RollupContextsPage variables = %v", call.Vars)
			}
			return githubData(`{"node":{"statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":"z"},"nodes":[` + strings.Join(githubContextsJSON(100, 20), ",") + `]}}}}`)
		}
		return forgeUnexpected(t, call)
	})
	got, err := core.ReadPR(t.Context(), githubTestRef(5), allParts, nil, nil)
	if err != nil {
		t.Fatalf("ReadPR: %v", err)
	}
	if ops := calls.ops(); fmt.Sprint(ops) != "[PRTick RollupContextsPage PRThreadsPage ThreadCommentsPage PRCommentsPage]" {
		t.Fatalf("requests = %v", ops)
	}
	if len(got.Threads) != 2+150 {
		t.Fatalf("threads = %d, want 2 review threads and 150 conversation comments", len(got.Threads))
	}
	if first := got.Threads[0]; len(first.Comments) != 130 || first.Comments[129].Body != "b129" || first.Comments[129].ReplyTo == nil {
		t.Fatalf("thread T1 has %d comments, want 130 in order", len(first.Comments))
	}
	if got.Threads[1].ID != "T2" || got.Threads[151].Comments[0].Body != "b149" {
		t.Fatalf("thread order = %s ... %+v", got.Threads[1].ID, got.Threads[151])
	}
	if got.Detail.Checks.Total != 120 || len(got.CI.Stages) != 1 || len(got.CI.Stages[0].Jobs) != 120 {
		t.Fatalf("checks = %d, stages = %+v", got.Detail.Checks.Total, len(got.CI.Stages))
	}
}

// A connection that never ends is read for githubReadMaxPages pages and
// no more.
func TestReadPRStopsAtThePageCap(t *testing.T) {
	t.Parallel()
	page := 0
	var mu sync.Mutex
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		mu.Lock()
		page++
		n := page
		mu.Unlock()
		thread := githubThreadJSON("T"+strconv.Itoa(n), githubCommentsJSON("c", 0, 1, false), false)
		switch call.Op {
		case "PRTick":
			return githubData(`{"repository":{"pullRequest":{"number":5,"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"p"},"nodes":[` + thread + `]},"comments":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`)
		case "PRThreadsPage":
			return githubData(`{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"p"},"nodes":[` + thread + `]}}}}`)
		}
		return forgeUnexpected(t, call)
	})
	got, err := core.ReadPR(t.Context(), githubTestRef(5), PRReadParts{Threads: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls.all()); n != githubReadMaxPages || len(got.Threads) != githubReadMaxPages {
		t.Fatalf("requests = %d, threads = %d, want %d of each", n, len(got.Threads), githubReadMaxPages)
	}
}

// A PR GitHub cannot resolve is its GraphQL error, unchanged.
func TestReadPRReturnsTheForgeError(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Body: `{"data":{"viewer":{"login":"v"},"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 5."}]}`}
	})
	_, err := core.ReadPR(t.Context(), githubTestRef(5), allParts, nil, nil)
	var gql *forgeapi.GraphQLError
	if !errors.As(err, &gql) || gql.Problems[0].Type != "NOT_FOUND" {
		t.Fatalf("ReadPR = %v, want the GraphQL NOT_FOUND", err)
	}
}

// A Core without a forge API transport refuses GitHub requests with
// ErrNoForgeAPI rather than reaching anything.
func TestGitHubWithoutATransportIsErrNoForgeAPI(t *testing.T) {
	t.Parallel()
	core := NewCore()
	if _, err := core.ReadPR(t.Context(), githubTestRef(1), allParts, nil, nil); !errors.Is(err, ErrNoForgeAPI) {
		t.Fatalf("ReadPR = %v", err)
	}
	if _, err := core.GetCIJobLog(t.Context(), githubTestRef(1), CIJobLogRequest{JobID: "9"}); !errors.Is(err, ErrNoForgeAPI) {
		t.Fatalf("GetCIJobLog = %v", err)
	}
}

// githubCIFake answers PRTick's rollup from a mutable check list and each
// run's REST jobs list from a mutable map, paging by Link as GitHub does.
type githubCIFake struct {
	mu       sync.Mutex
	contexts string
	jobs     map[string]string
}

func (f *githubCIFake) set(contexts string, jobs map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts, f.jobs = contexts, jobs
}

func newGitHubCICore(t *testing.T) (*Core, *githubCIFake, func() []string) {
	t.Helper()
	fake := &githubCIFake{}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if call.Op == "PRTick" {
			return githubData(`{"repository":{"pullRequest":{"number":5,"commits":{"nodes":[{"commit":{"id":"C","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[` + fake.contexts + `]}}}}]}}}}`)
		}
		path, query, _ := strings.Cut(call.Path, "?")
		run := strings.TrimSuffix(strings.TrimPrefix(path, "repos/o/r/actions/runs/"), "/jobs")
		if body, ok := fake.jobs[run+"?"+query]; ok {
			return forgeAPIAnswer{Body: body}
		}
		if body, ok := fake.jobs[run]; ok && query == "per_page=100" {
			return forgeAPIAnswer{Body: body}
		}
		return forgeUnexpected(t, call)
	})
	return core, fake, func() []string {
		var rest []string
		for _, call := range calls.all() {
			if call.Op == "" {
				rest = append(rest, call.Path)
			}
		}
		return rest
	}
}

func githubCheckRun(name, workflow, status, conclusion, run, job, started, completed string) string {
	quote := func(s string) string {
		if s == "" {
			return "null"
		}
		return strconv.Quote(s)
	}
	return fmt.Sprintf(`{"__typename":"CheckRun","name":%q,"status":%q,"conclusion":%s,"startedAt":%s,"completedAt":%s,"detailsUrl":"https://github.com/o/r/actions/runs/%s/job/%s","checkSuite":{"workflowRun":{"databaseId":%s,"url":"https://github.com/o/r/actions/runs/%s","workflow":{"name":%q}}}}`,
		name, status, quote(conclusion), quote(started), quote(completed), run, job, run, run, workflow)
}

func TestGitHubPipelineFromTheRollup(t *testing.T) {
	t.Parallel()
	core, fake, rest := newGitHubCICore(t)
	fake.set(githubCheckRun("build", "CI", "COMPLETED", "FAILURE", "111", "901", "2026-07-06T14:08:19Z", "2026-07-06T14:08:47Z")+
		`,{"__typename":"StatusContext","context":"codecov/patch","state":"SUCCESS","targetUrl":"https://codecov.example/x","createdAt":"2026-07-06T14:08:19Z","description":"ok"}`,
		map[string]string{"111": `{"total_count":1,"jobs":[{"id":901,"steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Build","status":"completed","conclusion":"failure"}]}]}`})
	pipeline, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, []string{"901"})
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	if pipeline.Status != CIStatusFailed || len(pipeline.Stages) != 2 {
		t.Fatalf("pipeline = %+v, want CI + External", pipeline)
	}
	job := pipeline.Stages[0].Jobs[0]
	if pipeline.Stages[0].Name != "CI" || job.ID != "901" || !job.LogsAvailable || job.DurationSeconds != 28 {
		t.Fatalf("CI stage = %+v", pipeline.Stages[0])
	}
	if len(job.Steps) != 2 || job.Steps[0].Name != "Set up job" || job.Steps[1].Status != CIStatusFailed {
		t.Fatalf("steps = %+v", job.Steps)
	}
	if ext := pipeline.Stages[1]; ext.Name != githubCIExternalStage || len(ext.Jobs) != 1 || ext.Jobs[0].LogsAvailable || ext.Jobs[0].Status != CIStatusSuccess {
		t.Fatalf("external stage = %+v", ext)
	}
	if got := rest(); fmt.Sprint(got) != "[repos/o/r/actions/runs/111/jobs?per_page=100]" {
		t.Fatalf("REST calls = %v", got)
	}
}

// The rollup lists every job with its status and duration; steps cost one
// REST jobs list per run holding a followed job whose steps can still
// change, and land on the followed job alone. Once that job completed and
// the previous observation holds its settled steps, they are reused with
// no call; a re-run (the check back in progress) reads them again.
func TestGitHubPipelineReadsStepsOnlyForFollowedJobs(t *testing.T) {
	t.Parallel()
	core, fake, rest := newGitHubCICore(t)
	runningRollup := githubCheckRun("build", "CI", "COMPLETED", "SUCCESS", "111", "901", "2026-07-06T14:08:19Z", "2026-07-06T14:08:47Z") + "," +
		githubCheckRun("lint", "Lint", "IN_PROGRESS", "", "222", "902", "2026-07-06T14:08:19Z", "") + "," +
		githubCheckRun("vet", "Lint", "QUEUED", "", "222", "903", "", "")
	doneRollup := githubCheckRun("build", "CI", "COMPLETED", "SUCCESS", "111", "901", "2026-07-06T14:08:19Z", "2026-07-06T14:08:47Z") + "," +
		githubCheckRun("lint", "Lint", "COMPLETED", "FAILURE", "222", "902", "2026-07-06T14:08:19Z", "2026-07-06T14:09:19Z") + "," +
		githubCheckRun("vet", "Lint", "COMPLETED", "SUCCESS", "222", "903", "2026-07-06T14:09:19Z", "2026-07-06T14:09:29Z")
	const runningJobs = `{"total_count":2,"jobs":[
{"id":902,"name":"lint","status":"in_progress","conclusion":null,"steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success","started_at":"2026-07-06T14:08:19Z","completed_at":"2026-07-06T14:08:20Z"},{"number":4,"name":"Lint","status":"in_progress","conclusion":null,"started_at":"2026-07-06T14:08:20Z","completed_at":null},{"number":5,"name":"Post Lint","status":"queued","conclusion":null,"started_at":null,"completed_at":null}]},
{"id":903,"name":"vet","status":"queued","conclusion":null,"steps":[]}]}`
	const doneJobs = `{"total_count":2,"jobs":[
{"id":902,"name":"lint","status":"completed","conclusion":"failure","steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Lint","status":"completed","conclusion":"failure"}]},
{"id":903,"name":"vet","status":"completed","conclusion":"success","steps":[{"number":1,"name":"Vet","status":"completed","conclusion":"success"}]}]}`
	fake.set(runningRollup, map[string]string{"222": runningJobs})
	list := func(prev *CIPipeline, stepsFor ...string) CIPipeline {
		t.Helper()
		pipeline, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), prev, stepsFor)
		if err != nil {
			t.Fatalf("ListPRCIJobs: %v", err)
		}
		return pipeline
	}

	plain := list(nil)
	if got := rest(); len(got) != 0 {
		t.Fatalf("an unfollowed pipeline read %v", got)
	}
	if len(plain.Stages) != 2 || plain.Stages[0].Name != "CI" || plain.Stages[1].Name != "Lint" || plain.Status != CIStatusRunning {
		t.Fatalf("stages = %+v", plain.Stages)
	}
	if build := plain.Stages[0].Jobs[0]; build.Status != CIStatusSuccess || build.DurationSeconds != 28 || build.Steps != nil {
		t.Fatalf("build = %+v", build)
	}
	if lint, vet := plain.Stages[1].Jobs[0], plain.Stages[1].Jobs[1]; lint.Status != CIStatusRunning || lint.DurationSeconds != 0 || vet.Status != CIStatusPending || vet.LogsAvailable {
		t.Fatalf("lint = %+v, vet = %+v", lint, vet)
	}

	first := list(&plain, "902")
	if got := rest(); len(got) != 1 || got[0] != "repos/o/r/actions/runs/222/jobs?per_page=100" {
		t.Fatalf("REST calls = %v, want one jobs list of run 222", got)
	}
	// Steps keep GitHub's numbers, gaps included, and their times; a time
	// the API has not set yet is empty.
	wantSteps := []CIStep{
		{Number: 1, Name: "Set up job", Status: CIStatusSuccess, StartedAt: "2026-07-06T14:08:19Z", CompletedAt: "2026-07-06T14:08:20Z"},
		{Number: 4, Name: "Lint", Status: CIStatusRunning, StartedAt: "2026-07-06T14:08:20Z"},
		{Number: 5, Name: "Post Lint", Status: CIStatusPending},
	}
	if lint := FindCIJob(first, "902"); !slices.Equal(lint.Steps, wantSteps) {
		t.Fatalf("lint steps = %+v, want %+v", lint.Steps, wantSteps)
	}
	if FindCIJob(first, "903").Steps != nil || FindCIJob(first, "901").Steps != nil {
		t.Fatalf("an unfollowed job carries steps: %+v", first.Stages)
	}

	second := list(&first, "902")
	if got := rest(); len(got) != 2 {
		t.Fatalf("REST calls = %v, want a second read while the job runs", got)
	}

	fake.set(doneRollup, map[string]string{"222": doneJobs})
	third := list(&second, "902")
	if got := rest(); len(got) != 3 {
		t.Fatalf("REST calls = %v, want a read for the completion", got)
	}
	if lint := FindCIJob(third, "902"); lint.Status != CIStatusFailed || lint.DurationSeconds != 60 || lint.Steps[1].Status != CIStatusFailed {
		t.Fatalf("completed lint = %+v", lint)
	}

	fourth := list(&third, "902")
	if got := rest(); len(got) != 3 {
		t.Fatalf("REST calls = %v, want settled steps reused", got)
	}
	if lint := FindCIJob(fourth, "902"); len(lint.Steps) != 2 || lint.Steps[1].Status != CIStatusFailed {
		t.Fatalf("reused lint steps = %+v", lint.Steps)
	}

	fake.set(runningRollup, map[string]string{"222": runningJobs})
	rerun := list(&fourth, "902")
	if got := rest(); len(got) != 4 {
		t.Fatalf("REST calls = %v, want a read for the re-run", got)
	}
	if lint := FindCIJob(rerun, "902"); lint.Steps[1].Status != CIStatusRunning {
		t.Fatalf("re-run lint steps = %+v", lint.Steps)
	}
}

// Steps read while the job ran are not reused once it completes, and
// steps the jobs list left live for a job the rollup calls complete are
// read again: the jobs list can trail the rollup.
func TestGitHubPipelineRereadsUnsettledSteps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                  string
		firstCheck, firstJobs string
	}{
		{"read while running", githubCheckRun("lint", "Lint", "IN_PROGRESS", "", "222", "902", "", ""),
			`{"total_count":1,"jobs":[{"id":902,"status":"in_progress","conclusion":null,"steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"}]}]}`},
		{"jobs list trailing", githubCheckRun("lint", "Lint", "COMPLETED", "SUCCESS", "222", "902", "", ""),
			`{"total_count":1,"jobs":[{"id":902,"status":"in_progress","conclusion":null,"steps":[{"number":1,"name":"Lint","status":"in_progress","conclusion":null}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, fake, rest := newGitHubCICore(t)
			fake.set(tc.firstCheck, map[string]string{"222": tc.firstJobs})
			first, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, []string{"902"})
			if err != nil {
				t.Fatal(err)
			}
			fake.set(githubCheckRun("lint", "Lint", "COMPLETED", "SUCCESS", "222", "902", "", ""), map[string]string{"222": `{"total_count":1,"jobs":[{"id":902,"status":"completed","conclusion":"success","steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Lint","status":"completed","conclusion":"success"}]}]}`})
			second, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), &first, []string{"902"})
			if err != nil {
				t.Fatal(err)
			}
			if got := rest(); len(got) != 2 {
				t.Fatalf("REST calls = %v, want the steps read again", got)
			}
			if steps := FindCIJob(second, "902").Steps; len(steps) != 2 || steps[1].Status != CIStatusSuccess {
				t.Fatalf("steps = %+v", steps)
			}
		})
	}
}

// A run past 100 jobs is read page by page, following GitHub's Link
// header, until the followed job turns up.
func TestGitHubPipelinePagesToTheFollowedJob(t *testing.T) {
	t.Parallel()
	var page1 strings.Builder
	page1.WriteString(`{"total_count":101,"jobs":[`)
	for i := range 100 {
		if i > 0 {
			page1.WriteString(",")
		}
		page1.WriteString(`{"id":` + strconv.Itoa(1000+i) + `,"steps":[]}`)
	}
	page1.WriteString(`]}`)
	const jobs = "repos/o/r/actions/runs/333/jobs"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "PRTick":
			return githubData(`{"repository":{"pullRequest":{"number":5,"commits":{"nodes":[{"commit":{"id":"C","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[` +
				githubCheckRun("last", "Matrix", "IN_PROGRESS", "", "333", "2000", "", "") + `]}}}}]}}}}`)
		case call.Path == jobs+"?per_page=100":
			return forgeAPIAnswer{Body: page1.String(), Header: http.Header{"Link": {`<` + call.RESTBase + jobs + `?per_page=100&page=2>; rel="next"`}}}
		case call.Path == jobs+"?per_page=100&page=2":
			return forgeAPIAnswer{Body: `{"total_count":101,"jobs":[{"id":2000,"steps":[{"number":1,"name":"Run","status":"in_progress"}]}]}`}
		}
		return forgeUnexpected(t, call)
	})
	pipeline, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, []string{"2000"})
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	if ops := calls.ops(); fmt.Sprint(ops) != "[PRTick GET "+jobs+"?per_page=100 GET "+jobs+"?per_page=100&page=2]" {
		t.Fatalf("requests = %v, want two pages", ops)
	}
	if steps := FindCIJob(pipeline, "2000").Steps; len(steps) != 1 || steps[0].Status != CIStatusRunning {
		t.Fatalf("steps = %+v", steps)
	}
}

// A job log has its BOM stripped; a 404 is ErrCIJobLogNotFound carrying
// the forge's error, and any other failure is not.
func TestGitHubJobLog(t *testing.T) {
	t.Parallel()
	status := http.StatusOK
	var mu sync.Mutex
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		mu.Lock()
		defer mu.Unlock()
		if call.Path != "repos/o/r/actions/jobs/901/logs" {
			return forgeUnexpected(t, call)
		}
		if status != http.StatusOK {
			return forgeAPIAnswer{Status: status, Body: `{"message":"` + http.StatusText(status) + `"}`}
		}
		return forgeAPIAnswer{Body: "\ufefflog line one\n", Header: http.Header{"Content-Type": {"text/plain"}}}
	})
	read, err := core.GetCIJobLog(t.Context(), githubTestRef(1), CIJobLogRequest{JobID: "901"})
	log := read.Text
	if err != nil || log != "log line one\n" {
		t.Fatalf("GetCIJobLog = %q, %v; want the BOM stripped", log, err)
	}
	if _, err := core.GetCIJobLog(t.Context(), githubTestRef(1), CIJobLogRequest{JobID: "901/logs"}); err == nil {
		t.Fatal("a non-numeric job id was requested")
	}
	if n := len(calls.all()); n != 1 {
		t.Fatalf("requests = %d, want the one valid read", n)
	}
	for _, tc := range []struct {
		status   int
		notFound bool
	}{{http.StatusNotFound, true}, {http.StatusInternalServerError, false}, {http.StatusForbidden, false}} {
		mu.Lock()
		status = tc.status
		mu.Unlock()
		_, err := core.GetCIJobLog(t.Context(), githubTestRef(1), CIJobLogRequest{JobID: "901"})
		var statusErr *forgeapi.StatusError
		if errors.Is(err, ErrCIJobLogNotFound) != tc.notFound || !errors.As(err, &statusErr) || statusErr.Status != tc.status {
			t.Fatalf("HTTP %d: GetCIJobLog = %v, want ErrCIJobLogNotFound %v over the *StatusError", tc.status, err, tc.notFound)
		}
	}
}

// A log over maxCILogBytes is read from its tail and starts at a whole
// line.
func TestGitHubJobLogKeepsTheTail(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("x", 99) + "\n"
	body := strings.Repeat(line, maxCILogBytes/len(line)+50)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if spec := call.Header.Get("Range"); spec != "" {
			start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(spec, "bytes="), "-"))
			if err != nil {
				t.Errorf("Range %q", spec)
			}
			return forgeAPIAnswer{Status: http.StatusPartialContent, Body: body[start:], Header: http.Header{
				"Content-Range":  {fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body))},
				"Content-Length": {strconv.Itoa(len(body) - start)}, "Content-Type": {"text/plain"}}}
		}
		// The log blob declares its length, as GitHub's does.
		return forgeAPIAnswer{Body: body, Header: http.Header{"Content-Type": {"text/plain"}, "Content-Length": {strconv.Itoa(len(body))}}}
	})
	read, err := core.GetCIJobLog(t.Context(), githubTestRef(1), CIJobLogRequest{JobID: "901"})
	log := read.Text
	if err != nil {
		t.Fatal(err)
	}
	if len(log) > maxCILogBytes || len(log) < maxCILogBytes-len(line) || !strings.HasPrefix(log, "xxx") || !strings.HasSuffix(body, log) || len(log)%len(line) != 0 {
		t.Fatalf("tail of %d bytes, want whole lines within %d", len(log), maxCILogBytes)
	}
	if n := len(calls.all()); n != 2 {
		t.Fatalf("requests = %d, want the probe and the Range", n)
	}
}

// A review is one REST POST; each file-level comment is another, against
// the head commit the PR read reports. A failed file comment after the
// review landed is a partial submit.
func TestGitHubSubmitReview(t *testing.T) {
	t.Parallel()
	line := 7
	var failComments atomic.Bool
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "PRTick":
			return githubData(`{"viewer":{"login":"v"},"repository":{"pullRequest":{"number":5,"headRefOid":"abc123","author":{"login":"a"},"latestReviews":{"nodes":[]},"commits":{"nodes":[]}}}}`)
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/reviews":
			return forgeAPIAnswer{Body: `{"id":1}`}
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/comments":
			if failComments.Load() {
				return forgeAPIAnswer{Status: http.StatusUnprocessableEntity, Body: `{"message":"Validation Failed"}`}
			}
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":2}`}
		}
		return forgeUnexpected(t, call)
	})
	review := SubmitReviewRequest{Verdict: ReviewVerdictRequestChanges, Body: "please fix", Comments: []ReviewLineComment{
		{Path: "a.go", Body: "here", Line: &line, Side: "right"},
		{Path: "b.go", Body: "whole file", Side: "file"},
	}}
	result, err := core.SubmitReview(t.Context(), githubTestRef(5), review)
	if err != nil || !result.PostedReview || result.PostedFileComments != 1 {
		t.Fatalf("SubmitReview = %+v, %v", result, err)
	}
	sent := calls.all()
	if len(sent) != 3 || sent[0].Op != "PRTick" {
		t.Fatalf("requests = %v", calls.ops())
	}
	var reviewBody struct {
		Event    string
		Body     string
		Comments []map[string]any
	}
	if err := json.Unmarshal(sent[1].Body, &reviewBody); err != nil || reviewBody.Event != "REQUEST_CHANGES" || reviewBody.Body != "please fix" ||
		len(reviewBody.Comments) != 1 || reviewBody.Comments[0]["path"] != "a.go" || reviewBody.Comments[0]["side"] != "RIGHT" || reviewBody.Comments[0]["line"] != float64(7) {
		t.Fatalf("review body = %s (%v)", sent[1].Body, err)
	}
	var fileBody map[string]string
	if err := json.Unmarshal(sent[2].Body, &fileBody); err != nil || fileBody["commit_id"] != "abc123" || fileBody["subject_type"] != "file" || fileBody["path"] != "b.go" {
		t.Fatalf("file comment body = %s (%v)", sent[2].Body, err)
	}

	failComments.Store(true)
	_, err = core.SubmitReview(t.Context(), githubTestRef(5), review)
	partial, ok := errors.AsType[*PartialSubmitError](err)
	if !ok || !partial.PostedReview || partial.FailedPath != "b.go" {
		t.Fatalf("failed file comment = %v, want a partial submit for b.go", err)
	}
	if _, ok := errors.AsType[*forgeapi.StatusError](err); !ok {
		t.Fatalf("partial submit lost the forge's error: %v", err)
	}
}

func TestGitHubReplyToThread(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/comments/42/replies" {
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":43}`}
		}
		return forgeUnexpected(t, call)
	})
	if err := core.ReplyToThread(t.Context(), githubTestRef(5), "PRRT_1", 42, "thanks"); err != nil {
		t.Fatal(err)
	}
	if sent := calls.all(); len(sent) != 1 || string(sent[0].Body) != `{"body":"thanks"}` {
		t.Fatalf("reply = %+v", sent)
	}
	if err := core.ReplyToThread(t.Context(), githubTestRef(5), "PRRT_1", 0, "thanks"); err == nil {
		t.Fatal("a reply without the root comment id was sent")
	}
}

// Resolve and unresolve are one mutation document, the thread and the
// wanted state as variables; the answer is read back.
func TestGitHubSetThreadResolved(t *testing.T) {
	t.Parallel()
	var answer string
	var mu sync.Mutex
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		mu.Lock()
		defer mu.Unlock()
		if call.Op != "SetThreadResolved" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: answer}
	})
	set := func(body string) {
		mu.Lock()
		answer = body
		mu.Unlock()
	}
	set(`{"data":{"resolve":{"thread":{"isResolved":true}}}}`)
	if err := core.SetThreadResolved(t.Context(), githubTestRef(5), "PRRT_x", true); err != nil {
		t.Fatal(err)
	}
	set(`{"data":{"unresolve":{"thread":{"isResolved":false}}}}`)
	if err := core.SetThreadResolved(t.Context(), githubTestRef(5), "PRRT_x", false); err != nil {
		t.Fatal(err)
	}
	sent := calls.all()
	if len(sent) != 2 || sent[0].Vars["threadID"] != "PRRT_x" || sent[0].Vars["resolved"] != true || sent[1].Vars["resolved"] != false ||
		sent[0].Query != sent[1].Query || strings.Contains(sent[0].Query, "PRRT_x") {
		t.Fatalf("mutations = %+v", sent)
	}
	for _, gate := range []string{"resolveReviewThread(input: {threadId: $threadID}) @include(if: $resolved)", "unresolveReviewThread(input: {threadId: $threadID}) @skip(if: $resolved)"} {
		if !strings.Contains(sent[0].Query, gate) {
			t.Fatalf("SetThreadResolved lacks %q", gate)
		}
	}
	set(`{"data":{"resolve":{"thread":{"isResolved":false}}}}`)
	if err := core.SetThreadResolved(t.Context(), githubTestRef(5), "PRRT_x", true); err == nil || !strings.Contains(err.Error(), "isResolved=false") {
		t.Fatalf("contradicting answer = %v", err)
	}
	set(`{"data":{"resolve":null},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`)
	err := core.SetThreadResolved(t.Context(), githubTestRef(5), "PRRT_x", true)
	if _, ok := errors.AsType[*forgeapi.GraphQLError](err); !ok || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("GraphQL failure = %v", err)
	}
	if err := core.SetThreadResolved(t.Context(), githubTestRef(5), " ", true); err == nil {
		t.Fatal("a resolve without a thread id was sent")
	}
}

// The repository-scoped reads name the repository the cwd's origin does,
// by variables.
func TestGitHubOriginScopedLists(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Op {
		case "OpenPRsByHead":
			return githubData(`{"repository":{"pullRequests":{"nodes":[{"url":"https://github.com/acme/repo/pull/3","number":3,"title":"Fix","state":"OPEN"}]}}}`)
		case "MergedPRs":
			first := int(call.Vars["first"].(float64))
			after, _ := call.Vars["after"].(string)
			offset := 0
			if after != "" {
				offset, _ = strconv.Atoi(after)
			}
			nodes := make([]string, 0, first)
			for i := offset; i < offset+first; i++ {
				nodes = append(nodes, fmt.Sprintf(`{"headRefName":"b%d","headRefOid":"%040d","url":"u%d"}`, i, i, i))
			}
			return githubData(fmt.Sprintf(`{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":true,"endCursor":"%d"},"nodes":[%s]}}}`, offset+first, strings.Join(nodes, ",")))
		}
		return forgeUnexpected(t, call)
	})
	cwd := t.TempDir()
	seedForgeCacheGitHubOrigin(t, core, cwd, "git@github.com:Acme/Repo.git")
	pulls, err := core.ListOpenPRs(t.Context(), cwd, "feature/x")
	if err != nil || len(pulls) != 1 || pulls[0].Number != 3 || pulls[0].State != "open" {
		t.Fatalf("ListOpenPRs = %+v, %v", pulls, err)
	}
	heads, err := core.ListMergedPRHeads(t.Context(), cwd, 150)
	if err != nil || len(heads) != 150 || heads[149].HeadRefName != "b149" {
		t.Fatalf("ListMergedPRHeads = %d heads, %v", len(heads), err)
	}
	sent := calls.all()
	if len(sent) != 3 || sent[0].Host != "github.com" {
		t.Fatalf("requests = %+v", calls.ops())
	}
	if v := sent[0].Vars; v["owner"] != "acme" || v["name"] != "repo" || v["head"] != "feature/x" || strings.Contains(sent[0].Query, "feature/x") {
		t.Fatalf("OpenPRsByHead variables = %v", v)
	}
	if v := sent[1].Vars; v["first"] != float64(100) || v["after"] != nil {
		t.Fatalf("first MergedPRs page = %v", v)
	}
	if v := sent[2].Vars; v["first"] != float64(50) || v["after"] != "100" {
		t.Fatalf("second MergedPRs page = %v", v)
	}
}

// An open-PR read the forge refuses is the transport's status error, and
// a head with no open PR is an empty answer, not an error.
func TestGitHubListOpenPRsAnswers(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Vars["head"] == "broken" {
			return forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`}
		}
		return githubData(`{"repository":{"pullRequests":{"nodes":[]}}}`)
	})
	cwd := t.TempDir()
	seedForgeCacheGitHubOrigin(t, core, cwd, "https://github.com/acme/repo.git")
	if pulls, err := core.ListOpenPRs(t.Context(), cwd, "quiet"); err != nil || len(pulls) != 0 {
		t.Fatalf("ListOpenPRs(quiet) = %+v, %v", pulls, err)
	}
	_, err := core.ListOpenPRs(t.Context(), cwd, "broken")
	if status, ok := errors.AsType[*forgeapi.StatusError](err); !ok || status.Status != http.StatusBadGateway {
		t.Fatalf("ListOpenPRs(broken) = %v, want a 502 status error", err)
	}
}

// A repository-scoped read on a checkout with no readable origin fails
// with *OriginUnknownError and sends nothing.
func TestGitHubOriginUnknown(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer { return forgeUnexpected(t, call) })
	forge := core.ForgeByID("github")
	cases := map[string]func(cwd string){
		"no origin": func(string) {},
		"not a forge": func(cwd string) {
			core.recordOrigin(cwd, originIdentity{url: "https://example.com/acme/repo.git", known: true}, core.nowFn())
		},
		"no repository path": func(cwd string) {
			core.recordOrigin(cwd, originIdentity{url: "https://github.com/acme", known: true}, core.nowFn())
		},
	}
	for name, seed := range cases {
		cwd := t.TempDir()
		seed(cwd)
		_, err := forge.ListOpenPRs(t.Context(), cwd, "b")
		var unknown *OriginUnknownError
		if !errors.As(err, &unknown) || unknown.Cwd != cwd {
			t.Fatalf("%s: ListOpenPRs = %v, want *OriginUnknownError", name, err)
		}
		if _, err := forge.ListMergedPRHeads(t.Context(), cwd, 5); !errors.As(err, &unknown) {
			t.Fatalf("%s: ListMergedPRHeads = %v, want *OriginUnknownError", name, err)
		}
	}
	if n := len(calls.all()); n != 0 {
		t.Fatalf("requests = %v", calls.ops())
	}
}

// A GitHub attachment is a transport request for its absolute URL, byte
// for byte; a larger body than the cap is refused.
func TestGitHubFetchAttachment(t *testing.T) {
	t.Parallel()
	content := "\x1b\x00binary\xff"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path != "/github/absolute/github.com/user-attachments/assets/0f0e0d0c-0b0a-0908-0706-050403020100" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: content, Header: http.Header{"Content-Type": {"image/png"}}}
	})
	ref := PRReference{Forge: "github", Host: "github.com", Namespace: "o", Repo: "r", Number: 1}
	href := "https://github.com/user-attachments/assets/0f0e0d0c-0b0a-0908-0706-050403020100"
	data, name, err := core.FetchAttachment(t.Context(), ref, href, 1<<20)
	if err != nil || string(data) != content || name != "0f0e0d0c-0b0a-0908-0706-050403020100" {
		t.Fatalf("FetchAttachment = %q %q %v", data, name, err)
	}
	if sent := calls.all(); len(sent) != 1 || sent[0].Header.Get("Accept") != "*/*" || sent[0].Header.Get("Authorization") != "Bearer test-token" {
		t.Fatalf("attachment request = %+v", sent)
	}
	if _, _, err := core.FetchAttachment(t.Context(), ref, href, 4); err == nil || !strings.Contains(err.Error(), "larger than 4 bytes") {
		t.Fatalf("over-cap attachment = %v", err)
	}
	if _, err := core.ForgeByID("github").FetchAttachment(t.Context(), ref, forgeattach.Target{Forge: "gitlab", Request: "projects/1/uploads/x/y"}, 10); err == nil {
		t.Fatal("a GitLab target went to GitHub")
	}
	if n := len(calls.all()); n != 2 {
		t.Fatalf("requests = %d, want the two fetches", n)
	}
}

// A failed GitHub attachment is the forge's status error with the signed
// query of its URL redacted.
func TestGitHubFetchAttachmentFailure(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"Not Found"}`}
	})
	ref := PRReference{Forge: "github", Host: "github.com", Namespace: "o", Repo: "r", Number: 1}
	_, _, err := core.FetchAttachment(t.Context(), ref, "https://private-user-images.githubusercontent.com/1/2.png?jwt=SIGNED", 1<<20)
	status, ok := errors.AsType[*forgeapi.StatusError](err)
	if !ok || status.Status != http.StatusNotFound || strings.Contains(err.Error(), "SIGNED") {
		t.Fatalf("FetchAttachment = %v, want a redacted 404", err)
	}
}
