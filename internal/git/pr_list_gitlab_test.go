package git

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitlabListAnswers answers a GitLab project read from the recorded
// answers: the list, the six GraphQL reads and the viewer's events. over
// replaces one call's answer when it reports true.
func gitlabListAnswers(t *testing.T, over func(call forgeAPICall) (forgeAPIAnswer, bool)) func(call forgeAPICall) forgeAPIAnswer {
	t.Helper()
	answers := map[string]string{
		"PullsReviewing":          readTestdata(t, "gitlab-pulls-reviewing.json"),
		"PullsAuthored":           readTestdata(t, "gitlab-pulls-authored.json"),
		"PullsReviewingActivity":  readTestdata(t, "gitlab-pulls-reviewing-activity.json"),
		"PullsAuthoredActivity":   readTestdata(t, "gitlab-pulls-authored-activity.json"),
		"PullsReviewingApprovals": readTestdata(t, "gitlab-pulls-reviewing-approvals.json"),
		"PullsAuthoredApprovals":  readTestdata(t, "gitlab-pulls-authored-approvals.json"),
	}
	list := readTestdata(t, "gitlab-mr-list.json")
	events := readTestdata(t, "gitlab-user-events.json")
	return func(call forgeAPICall) forgeAPIAnswer {
		if over != nil {
			if answer, ok := over(call); ok {
				return answer
			}
		}
		if body, ok := answers[call.Op]; ok {
			return forgeAPIAnswer{Body: body}
		}
		switch {
		case call.Path == "user":
			return forgeAPIAnswer{Body: `{"id":1,"username":"alice"}`}
		case strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/merge_requests?"):
			return forgeAPIAnswer{Body: list}
		case strings.HasPrefix(call.Path, "users/1/events?"):
			return forgeAPIAnswer{Body: events}
		}
		return forgeUnexpected(t, call)
	}
}

// The recorded answers parse into rows: the viewer's own draft (the
// authored reads cover it), a fork's merge request the viewer reviews
// (the reviewing reads and its events cover it) and one the viewer has no
// part in, which no detail read covers. A GraphQL node the list does not
// hold is dropped. The events feed ended within its first page, so a row
// with no activity in it carries the window's floor: the day after the
// after date.
func TestGitLabListProjectPRsParsesTheReads(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, gitlabListAnswers(t, nil))
	core.nowFn = func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	ops := map[string]forgeAPICall{}
	for _, call := range calls.all() {
		key := call.Op
		if key == "" {
			key, _, _ = strings.Cut(call.Path, "?")
		}
		ops[key] = call
	}
	if len(ops) != 9 || len(calls.all()) != 9 {
		t.Fatalf("requests = %v %v, want user, the list, six GraphQL reads and events once each", calls.ops(), calls.all())
	}
	for _, op := range []string{"PullsReviewing", "PullsAuthored", "PullsReviewingActivity", "PullsAuthoredActivity", "PullsReviewingApprovals", "PullsAuthoredApprovals"} {
		vars := ops[op].Vars
		if vars["fullPath"] != "grp/sub/app" || vars["viewer"] != "alice" || vars["after"] != nil {
			t.Errorf("%s variables = %v", op, vars)
		}
	}
	for op, filter := range map[string]string{
		"PullsReviewing": "reviewerUsername: $viewer", "PullsAuthoredActivity": "authorUsername: $viewer",
		"PullsReviewingApprovals": "approvalState { rules { type approvalsRequired } }",
	} {
		if !strings.Contains(ops[op].Query, filter) {
			t.Errorf("%s does not filter by %s: %s", op, filter, ops[op].Query)
		}
	}
	query, _ := url.ParseQuery(strings.SplitN(ops["users/1/events"].Path, "?", 2)[1])
	if query.Encode() != "after=2026-09-21&per_page=100" {
		t.Errorf("events query = %s", query.Encode())
	}
	list, _ := url.ParseQuery(strings.SplitN(ops["projects/grp%2Fsub%2Fapp/merge_requests"].Path, "?", 2)[1])
	if list.Encode() != "order_by=updated_at&per_page=100&sort=desc&state=opened" {
		t.Fatalf("list query = %s", list.Encode())
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	own, fork, other := rows[0], rows[1], rows[2]
	two, zero := 2, 0
	want := PRListRow{
		Ref: PRReference{Forge: "gitlab", Host: testForgeHost, Namespace: "grp/sub", Repo: "app", Number: 21},
		URL: "https://gitlab.example/grp/sub/app/-/merge_requests/21", Title: "Draft: speed up builds", Draft: true,
		Author:    PRListAuthor{Login: "alice", Name: "Alice Ng"},
		CreatedAt: "2026-10-20T22:05:15.053Z", UpdatedAt: "2026-10-20T22:05:16.934Z",
		HeadSHA: strings.Repeat("4", 40), HeadRefName: "alice/builds", BaseRefName: "main",
		Comments: 1, ViewerIsAuthor: true, MergeStatus: "draft_status",
		LastCommitAt: "2026-10-20T22:04:00Z", PipelineStatus: CIStatusRunning, ProjectID: 77,
		ApprovalsGiven: &zero, ApprovalsRequired: &two, Unresolved: &zero,
		ViewerActivityFloor: "2026-09-22T00:00:00Z",
	}
	if fmt.Sprintf("%+v", deref(own)) != fmt.Sprintf("%+v", deref(want)) {
		t.Errorf("own draft = %+v\nwant       %+v", deref(own), deref(want))
	}
	// The viewer reviewed (its reviewer state), commented after its
	// approval (its discussion note, the latest of its three events on
	// !20; an issue note and another project's merge request are not it),
	// one of two approvals stands (the regular and code owner rules
	// outnumber the any-approver one), two of three threads are open and
	// the pipeline failed.
	if !fork.CrossRepository || !fork.ViewerIsReviewer || !fork.ReviewRequested || fork.ViewerIsAuthor ||
		!fork.Conflicts || !fork.UnresolvedBlocking || fork.ReviewDecision != "REVIEW_REQUIRED" || fork.Comments != 9 ||
		fork.ViewerReviewState != "reviewed" || fork.ViewerActivityAt != "2026-10-19T08:00:00.000Z" ||
		fork.PipelineStatus != CIStatusFailed || fork.LastCommitAt != "2026-10-19T09:00:00Z" ||
		*fork.ApprovalsGiven != 1 || *fork.ApprovalsRequired != 2 || *fork.Unresolved != 2 || fork.ViewerActivityFloor != "" {
		t.Errorf("fork row = %+v", deref(fork))
	}
	// Neither reviewer nor author: only the list.
	if other.ViewerIsReviewer || other.ReviewDecision != "CHANGES_REQUESTED" || other.ViewerReviewState != "" ||
		other.ViewerActivityAt != "" || other.ApprovalsGiven != nil || other.ApprovalsRequired != nil || other.Unresolved != nil ||
		other.PipelineStatus != "" || other.LastCommitAt != "" {
		t.Errorf("uncovered row = %+v", deref(other))
	}
}

// deref spells a row's pointer fields as values, for comparison.
func deref(row PRListRow) map[string]any {
	value := func(p *int) any {
		if p == nil {
			return nil
		}
		return *p
	}
	given, required, unresolved := value(row.ApprovalsGiven), value(row.ApprovalsRequired), value(row.Unresolved)
	row.ApprovalsGiven, row.ApprovalsRequired, row.Unresolved = nil, nil, nil
	return map[string]any{"row": row, "given": given, "required": required, "unresolved": unresolved}
}

// A failed detail request leaves its fields unknown and is reported
// beside the rows, which still list; the other detail reads still fill
// theirs. A failed list fails the read whole.
func TestGitLabListProjectPRsReportsAFailedDetailBesideTheRows(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, gitlabListAnswers(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		switch {
		case call.Op == "PullsReviewing":
			return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"500 Internal Server Error"}`}, true
		case strings.HasPrefix(call.Path, "users/1/events?"):
			return forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"403 Forbidden"}`}, true
		}
		return forgeAPIAnswer{}, false
	}))
	rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
	var detail *PRListDetailError
	if !errors.As(err, &detail) || len(detail.Errs) != 2 {
		t.Fatalf("ListProjectPRs err = %v, want a PRListDetailError of the review and events reads", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the list's 3", len(rows))
	}
	fork := rows[1]
	if fork.ViewerReviewState != "" || fork.ApprovalsGiven != nil || fork.ViewerActivityAt != "" || fork.PipelineStatus != "" {
		t.Errorf("fork row kept a failed read's fields: %+v", deref(fork))
	}
	if fork.Unresolved == nil || *fork.Unresolved != 2 || fork.LastCommitAt == "" || *fork.ApprovalsRequired != 2 || fork.ViewerActivityFloor != "" {
		t.Errorf("fork row lost a read that landed: %+v", deref(fork))
	}

	failing, _ := newForgeAPICore(t, gitlabListAnswers(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/merge_requests?") {
			return forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"403 Forbidden"}`}, true
		}
		return forgeAPIAnswer{}, false
	}))
	rows, err = failing.ListProjectPRs(t.Context(), gitlabTestRepo())
	if err == nil || errors.As(err, &detail) || rows != nil {
		t.Fatalf("a failed list = %d rows, %v; want the list's failure alone", len(rows), err)
	}
}

// The viewer's events are read once per host per round: reads of two
// projects under one round share one request, a later round reads again,
// and reads under no round read each time.
func TestGitLabViewerEventsAreReadOncePerRound(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	events := 0
	core, _ := newForgeAPICore(t, gitlabListAnswers(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		switch {
		case strings.HasPrefix(call.Path, "users/1/events?"):
			mu.Lock()
			events++
			mu.Unlock()
			return forgeAPIAnswer{Body: `[]`}, true
		case strings.HasPrefix(call.Path, "projects/grp%2Fother/merge_requests?"):
			return forgeAPIAnswer{Body: `[]`}, true
		}
		return forgeAPIAnswer{}, false
	}))
	other := RepoReference{Forge: "gitlab", Host: testForgeHost, Namespace: "grp", Repo: "other"}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return events
	}
	round := WithPRListRound(t.Context())
	var wg sync.WaitGroup
	for _, repo := range []RepoReference{gitlabTestRepo(), other} {
		wg.Go(func() {
			if _, err := core.ListProjectPRs(round, repo); err != nil {
				t.Errorf("ListProjectPRs %s: %v", repo.Project(), err)
			}
		})
	}
	wg.Wait()
	if got := count(); got != 1 {
		t.Fatalf("events requests in one round = %d, want 1", got)
	}
	if _, err := core.ListProjectPRs(WithPRListRound(t.Context()), other); err != nil || count() != 2 {
		t.Fatalf("a new round: err %v, events requests %d, want 2", err, count())
	}
	for range 2 {
		if _, err := core.ListProjectPRs(t.Context(), other); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(); got != 4 {
		t.Fatalf("reads under no round sent %d events requests in all, want 4", got)
	}
}

// The events feed is a lower bound. A feed that ended within its pages
// reaches back to the after date; one cut at gitlabActivityMaxPages full
// pages reaches back only to its oldest event, which is the floor a row
// with no activity in the window carries.
func TestGitLabViewerActivityFloorIsTheWindowsOldestReach(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	pages := 0
	core, _ := newForgeAPICore(t, gitlabListAnswers(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if !strings.HasPrefix(call.Path, "users/1/events?") {
			return forgeAPIAnswer{}, false
		}
		query, _ := url.ParseQuery(strings.SplitN(call.Path, "?", 2)[1])
		page := 1
		if raw := query.Get("page"); raw != "" {
			if _, err := fmt.Sscanf(raw, "%d", &page); err != nil {
				t.Errorf("page %q: %v", raw, err)
			}
		}
		mu.Lock()
		pages++
		mu.Unlock()
		// A pushes-only feed, one event a minute back from 2026-10-21
		// 12:00, with more pages than the read follows.
		events := make([]string, 100)
		for i := range events {
			at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC).Add(-time.Duration((page-1)*100+i) * time.Minute)
			events[i] = fmt.Sprintf(`{"project_id":77,"action_name":"pushed to","created_at":%q}`, at.Format(time.RFC3339))
		}
		return forgeAPIAnswer{Header: http.Header{"X-Next-Page": {fmt.Sprint(page + 1)}}, Body: "[" + strings.Join(events, ",") + "]"}, true
	}))
	core.nowFn = func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	mu.Lock()
	read := pages
	mu.Unlock()
	if read != gitlabActivityMaxPages {
		t.Fatalf("events pages read = %d, want %d", read, gitlabActivityMaxPages)
	}
	// The 300th event, 299 minutes back.
	floor := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC).Add(-299 * time.Minute).Format(time.RFC3339)
	for _, row := range rows {
		if row.ViewerActivityAt != "" || row.ViewerActivityFloor != floor {
			t.Errorf("!%d activity %q floor %q, want none and %s", row.Ref.Number, row.ViewerActivityAt, row.ViewerActivityFloor, floor)
		}
	}
}

// A login GitLab will not name fails the list read: every attention
// state hangs on the viewer, so rows that read as nobody's would be
// wrong, not empty. The refusal is cached, so the second read fails
// without asking again, and nothing else is read without a viewer.
func TestGitLabListProjectPRsWithoutAViewerFails(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, gitlabListAnswers(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Path == "user" {
			return forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"403 Forbidden"}`}, true
		}
		return forgeAPIAnswer{}, false
	}))
	for i := range 2 {
		rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
		if err == nil || len(rows) != 0 {
			t.Fatalf("read %d: ListProjectPRs = %d rows, %v; want the viewer failure", i, len(rows), err)
		}
	}
	if ops := calls.ops(); len(calls.all()) != 1 || calls.all()[0].Path != "user" {
		t.Fatalf("requests = %v, want one GET user", ops)
	}
}

// The list follows X-Next-Page for full pages and stops at
// prListMaxPages; a GraphQL read follows its endCursor to the same bound.
func TestGitLabListProjectPRsPagesTheReads(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	graphQLPages := map[string]int{}
	core, calls := newForgeAPICore(t, gitlabListAnswers(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Op != "" {
			mu.Lock()
			graphQLPages[call.Op]++
			n := graphQLPages[call.Op]
			mu.Unlock()
			if call.Op == "PullsReviewing" && n > 1 && call.Vars["after"] != fmt.Sprintf("c%d", n-1) {
				t.Errorf("page %d after = %v", n, call.Vars["after"])
			}
			return githubData(fmt.Sprintf(`{"project":{"mergeRequests":{"pageInfo":{"hasNextPage":true,"endCursor":"c%d"},"nodes":[{"iid":"%d"}]}}}`, n, n)), true
		}
		if !strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/merge_requests?") {
			return forgeAPIAnswer{}, false
		}
		query, _ := url.ParseQuery(strings.SplitN(call.Path, "?", 2)[1])
		page := 1
		if raw := query.Get("page"); raw != "" {
			if _, err := fmt.Sscanf(raw, "%d", &page); err != nil {
				t.Errorf("page %q: %v", raw, err)
			}
		}
		mrs := make([]string, 100)
		for i := range mrs {
			mrs[i] = fmt.Sprintf(`{"iid":%d,"project_id":1,"source_project_id":1,"blocking_discussions_resolved":true}`, (page-1)*100+i+1)
		}
		return forgeAPIAnswer{Header: http.Header{"X-Next-Page": {fmt.Sprint(page + 1)}}, Body: "[" + strings.Join(mrs, ",") + "]"}, true
	}))
	rows, err := core.ListProjectPRs(t.Context(), gitlabTestRepo())
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	lists := 0
	for _, call := range calls.all() {
		if strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/merge_requests?") {
			lists++
		}
	}
	if lists != prListMaxPages || len(rows) != prListMaxPages*100 {
		t.Fatalf("list pages = %d, rows = %d", lists, len(rows))
	}
	for op, n := range graphQLPages {
		if n != prListMaxPages {
			t.Errorf("%s pages = %d, want %d", op, n, prListMaxPages)
		}
	}
}
