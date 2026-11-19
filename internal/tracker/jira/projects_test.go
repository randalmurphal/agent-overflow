package jira

import (
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// projectPages answers project/search with pages of two from a list of
// five, failing at failAt when it is not negative. A client that asks
// for more than ten pages is looping: the test fails and the page is
// refused, so the loop ends.
func projectPages(t *testing.T, failAt int, emptyAt int) jiratest.Handler {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		if calls.Add(1) > 10 {
			t.Error("the client kept asking for pages")
			jiratest.Reply(400, `{}`)(w, r, b)
			return
		}
		jiratest.WantQuery(t, r, "maxResults", "100")
		start := r.URL.Query().Get("startAt")
		var n int
		_, _ = fmt.Sscan(start, &n)
		switch n {
		case failAt:
			jiratest.Reply(500, `{"errorMessages":["down"]}`)(w, r, b)
			return
		case emptyAt:
			jiratest.Reply(200, `{"values":[],"isLast":false}`)(w, r, b)
			return
		}
		var values string
		for i := n; i < min(n+2, 5); i++ {
			if values != "" {
				values += ","
			}
			values += fmt.Sprintf(`{"id":"1000%d","key":"P%d","name":"Project %d","projectTypeKey":"software"}`, i, i, i)
		}
		jiratest.Reply(200, fmt.Sprintf(`{"startAt":%d,"values":[%s],"isLast":%v}`, n, values, n+2 >= 5))(w, r, b)
	}
}

func TestProjectsPagesToTheEnd(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/project/search", projectPages(t, -1, -1))
	ps, err := c.Projects(t.Context(), ProjectQuery{})
	if err != nil || len(ps.Projects) != 5 || ps.Projects[4].Key != "P4" || !ps.Complete {
		t.Fatalf("got %+v, %v", ps, err)
	}
	if n := f.Count("GET", "/rest/api/3/project/search"); n != 3 {
		t.Errorf("%d pages, want 3", n)
	}
	for limit, complete := range map[int]bool{3: false, 4: false, 5: true, 6: true} {
		f.On("GET", "/rest/api/3/project/search", projectPages(t, -1, -1))
		got, err := c.Projects(t.Context(), ProjectQuery{Limit: limit})
		if err != nil || len(got.Projects) != min(limit, 5) || got.Complete != complete {
			t.Errorf("limit %d: %d projects, complete %v, %v", limit, len(got.Projects), got.Complete, err)
		}
	}
	// A negative limit is not "no limit".
	n := f.Count("GET", "/rest/api/3/project/search")
	if _, err := c.Projects(t.Context(), ProjectQuery{Limit: -1}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a negative limit: %v", err)
	}
	if f.Count("GET", "/rest/api/3/project/search") != n {
		t.Error("a negative limit was sent")
	}
}

func TestProjectsFailureMidPageKeepsWhatWasRead(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/project/search", projectPages(t, 2, -1))
	ps, err := c.Projects(t.Context(), ProjectQuery{})
	if err == nil || len(ps.Projects) != 2 || ps.Complete {
		t.Fatalf("got %+v and %v", ps, err)
	}
	f.On("GET", "/rest/api/3/project/search", projectPages(t, -1, 2))
	if _, err := c.Projects(t.Context(), ProjectQuery{}); err == nil {
		t.Error("an empty page that is not the last must fail, not loop")
	}
}

func TestProjectAndStatuses(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/project/WEB", jiratest.Reply(200, `{"id":"10000","key":"WEB","name":"Web","projectTypeKey":"service_desk",
		"issueTypes":[{"id":"1","name":"Task","hierarchyLevel":0},{"id":"2","name":"Sub-task","subtask":true,"hierarchyLevel":-1}]}`))
	f.On("GET", "/rest/api/3/project/WEB/statuses", jiratest.Reply(200, `[{"id":"1","name":"Task","subtask":false,"self":"x",
		"statuses":[{"id":"3","name":"In Progress","statusCategory":{"id":4,"key":"indeterminate","name":"In Progress","colorName":"yellow"}}]}]`))
	p, err := c.Project(t.Context(), "WEB")
	if err != nil || !p.ServiceDesk() || len(p.IssueTypes) != 2 || !p.IssueTypes[1].Subtask || p.IssueTypes[1].HierarchyLevel != -1 {
		t.Fatalf("got %+v %v", p, err)
	}
	sts, err := c.ProjectStatuses(t.Context(), "WEB")
	if err != nil || len(sts) != 1 || sts[0].Statuses[0].Category.Key != "indeterminate" {
		t.Fatalf("got %+v %v", sts, err)
	}
}

func TestStatuses(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/status", jiratest.Reply(200, `[
		{"id":"10000","name":"To Do","self":"x","statusCategory":{"id":2,"key":"new","name":"To Do","colorName":"blue-gray"}},
		{"id":"5","name":"Closed","statusCategory":{"id":3,"key":"done","colorName":"green"}}]`))
	got, err := c.Statuses(t.Context())
	want := []Status{
		{ID: "10000", Name: "To Do", Category: StatusCategory{ID: 2, Key: "new", Name: "To Do", ColorName: "blue-gray"}},
		{ID: "5", Name: "Closed", Category: StatusCategory{ID: 3, Key: "done", ColorName: "green"}},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %+v %v", got, err)
	}
	f.On("GET", "/rest/api/3/status", jiratest.Reply(500, `{"errorMessages":["down"]}`))
	if got, err := c.Statuses(t.Context()); err == nil || got != nil {
		t.Errorf("a failed read: %+v %v", got, err)
	}
}

func TestPrioritiesAndLinkTypes(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/priority/search", jiratest.Reply(200, `{"values":[{"id":"1","name":"High","statusColor":"#f00"},{"id":"2","name":"Low"}],"isLast":true}`))
	f.On("GET", "/rest/api/3/issueLinkType", jiratest.Reply(200, `{"issueLinkTypes":[{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"}]}`))
	ps, err := c.Priorities(t.Context())
	if err != nil || len(ps) != 2 || ps[0].Color != "#f00" {
		t.Fatalf("got %+v %v", ps, err)
	}
	lts, err := c.IssueLinkTypes(t.Context())
	if err != nil || lts[0].Outward != "blocks" {
		t.Fatalf("got %+v %v", lts, err)
	}
}
