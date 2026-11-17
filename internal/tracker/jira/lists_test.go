package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

const backlogURL = "/rest/software/1.0/board/3/backlog"

// listPages answers a list with three pages of two issues, tokens t1 and
// t2; page(n) may replace the nth page's body.
func listPages(t *testing.T, page func(n int) string) handler {
	n := 0
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		if n++; n > 10 {
			t.Error("the client kept asking for pages")
			reply(400, `{}`)(w, r, b)
			return
		}
		at := map[string]int{"": 0, "t1": 1, "t2": 2}[r.URL.Query().Get("nextPageToken")]
		if body := page(at); body != "" {
			reply(200, body)(w, r, b)
			return
		}
		next := fmt.Sprintf(`"t%d"`, at+1)
		if at == 2 {
			next = "null"
		}
		reply(200, fmt.Sprintf(`{"issues":[{"id":"%d","key":"WEB-%d"},{"id":"%d","key":"WEB-%d"}],"nextPageToken":%s,"isLast":%v}`,
			2*at+1, 2*at+1, 2*at+2, 2*at+2, next, at == 2))(w, r, b)
	}
}

func TestListIssuesToTheEnd(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	pages := listPages(t, func(int) string { return "" })
	f.on("GET", backlogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "jql", `assignee = "acc-user1"`)
		wantQuery(t, r, "maxResults", "100")
		if got := r.URL.Query()["fields"]; strings.Join(got, ",") != "summary,status" {
			t.Errorf("fields %v", got)
		}
		pages(w, r, b)
	})
	q := ListQuery{List: BacklogIssues(3), JQL: `assignee = "acc-user1"`, Fields: []string{"summary", "status"}}
	all, err := c.ListAllIssues(t.Context(), q, 0)
	if err != nil || !all.Complete || len(all.Issues) != 6 || all.Issues[5].Key != "WEB-6" {
		t.Fatalf("got %d issues, complete %v, %v", len(all.Issues), all.Complete, err)
	}
	f.on("GET", backlogURL, listPages(t, func(int) string { return "" }))
	some, err := c.ListAllIssues(t.Context(), ListQuery{List: BacklogIssues(3)}, 3)
	if err != nil || some.Complete || len(some.Issues) != 3 {
		t.Fatalf("limited: %d issues, complete %v, %v", len(some.Issues), some.Complete, err)
	}
}

// TestListPageEnds proves only isLast ends a list, and a repeated token
// or an empty page that names a next one fails the walk.
func TestListPageEnds(t *testing.T) {
	for name, page := range map[string]func(int) string{
		"a repeated token": func(n int) string {
			if n == 1 {
				return `{"issues":[{"id":"3","key":"WEB-3"}],"nextPageToken":"t1","isLast":false}`
			}
			return ""
		},
		"an empty page that names a next one": func(n int) string {
			if n == 1 {
				return `{"issues":[],"nextPageToken":"t2","isLast":false}`
			}
			return ""
		},
		"not the last and no next page": func(n int) string {
			if n == 1 {
				return `{"issues":[{"id":"3","key":"WEB-3"}],"isLast":false}`
			}
			return ""
		},
	} {
		f := newFake(t)
		c := newTestClient(t, f)
		f.on("GET", backlogURL, listPages(t, page))
		all, err := c.ListAllIssues(t.Context(), ListQuery{List: BacklogIssues(3)}, 0)
		// The pages before the bad one are kept: two issues, and the
		// repeated token's page too.
		want := 2
		if name == "a repeated token" {
			want = 3
		}
		if err == nil || all.Complete || len(all.Issues) != want {
			t.Errorf("%s: %d issues, complete %v, %v", name, len(all.Issues), all.Complete, err)
		}
	}
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", backlogURL, reply(200, `{"issues":[{"id":"3","key":"WEB-3"}],"isLast":false}`))
	if _, err := c.ListIssues(t.Context(), ListQuery{List: BacklogIssues(3)}); !isAnswerErr(err) {
		t.Errorf("one page: %v", err)
	}
	f.on("GET", backlogURL, reply(200, `{"issues":[],"isLast":true,"warningMessages":["The JQL was cut"]}`))
	all, err := c.ListAllIssues(t.Context(), ListQuery{List: BacklogIssues(3)}, 0)
	if err != nil || all.Complete || len(all.Warnings) != 1 || all.Warnings[0].Message != "The JQL was cut" {
		t.Errorf("a warning: %+v %v", all, err)
	}
}

// TestListCancelledMidWalk keeps the pages read before the cancel.
func TestListCancelledMidWalk(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	pages := listPages(t, func(int) string { return "" })
	f.on("GET", backlogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.URL.Query().Get("nextPageToken") == "t1" {
			cancel()
			<-r.Context().Done()
			return
		}
		pages(w, r, b)
	})
	all, err := c.ListAllIssues(ctx, ListQuery{List: BacklogIssues(3)}, 0)
	if !errors.Is(err, context.Canceled) || all.Complete || len(all.Issues) != 2 {
		t.Fatalf("got %d issues, complete %v, %v", len(all.Issues), all.Complete, err)
	}
}

func TestListPaths(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	for path, l := range map[string]IssueList{
		"/rest/software/1.0/board/3/issue":          BoardIssues(3),
		"/rest/software/1.0/sprint/7/issue":         SprintIssues(7),
		"/rest/software/1.0/board/3/sprint/7/issue": BoardSprintIssues(3, 7),
		"/rest/software/1.0/epic/WEB-9/issue":       EpicIssues("WEB-9"),
	} {
		f.on("GET", path, reply(200, `{"issues":[],"isLast":true}`))
		if _, err := c.ListIssues(t.Context(), ListQuery{List: l}); err != nil || f.count("GET", path) != 1 {
			t.Errorf("%s: %v", path, err)
		}
	}
	for name, l := range map[string]IssueList{
		"none": {}, "board 0": BoardIssues(0), "sprint 0": BoardSprintIssues(3, 0), "an epic path": EpicIssues("../x"), "not an epic": EpicIssues("not an issue"),
	} {
		if _, err := c.ListIssues(t.Context(), ListQuery{List: l}); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.ListIssues(t.Context(), ListQuery{List: BoardIssues(3), MaxResults: -1}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a negative page: %v", err)
	}
}

func TestCountListIssues(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", backlogURL+"/approximate-count", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "jql", "type = Bug")
		reply(200, `{"count":42}`)(w, r, b)
	})
	if n, err := c.CountListIssues(t.Context(), BacklogIssues(3), "type = Bug"); err != nil || n != 42 {
		t.Fatalf("got %d %v", n, err)
	}
	for _, l := range []IssueList{SprintIssues(7), EpicIssues("WEB-9"), BoardSprintIssues(3, 7)} {
		if _, err := c.CountListIssues(t.Context(), l, ""); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%+v: %v", l, err)
		}
	}
}
