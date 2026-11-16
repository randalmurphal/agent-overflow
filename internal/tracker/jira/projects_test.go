package jira

import (
	"fmt"
	"net/http"
	"testing"
)

// projectPages answers project/search with pages of two from a list of
// five, failing at failAt when it is not negative.
func projectPages(t *testing.T, failAt int, emptyAt int) handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "maxResults", "100")
		start := r.URL.Query().Get("startAt")
		var n int
		_, _ = fmt.Sscan(start, &n)
		switch n {
		case failAt:
			reply(500, `{"errorMessages":["down"]}`)(w, r, b)
			return
		case emptyAt:
			reply(200, `{"values":[],"isLast":false}`)(w, r, b)
			return
		}
		var values string
		for i := n; i < min(n+2, 5); i++ {
			if values != "" {
				values += ","
			}
			values += fmt.Sprintf(`{"id":"1000%d","key":"P%d","name":"Project %d","projectTypeKey":"software"}`, i, i, i)
		}
		reply(200, fmt.Sprintf(`{"startAt":%d,"values":[%s],"isLast":%v}`, n, values, n+2 >= 5))(w, r, b)
	}
}

func TestProjectsPagesToTheEnd(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/project/search", projectPages(t, -1, -1))
	ps, err := c.Projects(t.Context(), ProjectQuery{})
	if err != nil || len(ps) != 5 || ps[4].Key != "P4" {
		t.Fatalf("got %d projects, %v", len(ps), err)
	}
	if n := f.count("GET", "/rest/api/3/project/search"); n != 3 {
		t.Errorf("%d pages, want 3", n)
	}
	limited, err := c.Projects(t.Context(), ProjectQuery{Limit: 3})
	if err != nil || len(limited) != 3 {
		t.Errorf("limit: %d %v", len(limited), err)
	}
}

func TestProjectsFailureMidPageKeepsWhatWasRead(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/project/search", projectPages(t, 2, -1))
	ps, err := c.Projects(t.Context(), ProjectQuery{})
	if err == nil || len(ps) != 2 {
		t.Fatalf("got %d projects and %v", len(ps), err)
	}
	f.on("GET", "/rest/api/3/project/search", projectPages(t, -1, 2))
	if _, err := c.Projects(t.Context(), ProjectQuery{}); err == nil {
		t.Error("an empty page that is not the last must fail, not loop")
	}
}

func TestProjectAndStatuses(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/project/WEB", reply(200, `{"id":"10000","key":"WEB","name":"Web","projectTypeKey":"service_desk",
		"issueTypes":[{"id":"1","name":"Task","hierarchyLevel":0},{"id":"2","name":"Sub-task","subtask":true,"hierarchyLevel":-1}]}`))
	f.on("GET", "/rest/api/3/project/WEB/statuses", reply(200, `[{"id":"1","name":"Task","subtask":false,"self":"x",
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

func TestPrioritiesAndLinkTypes(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/priority/search", reply(200, `{"values":[{"id":"1","name":"High","statusColor":"#f00"},{"id":"2","name":"Low"}],"isLast":true}`))
	f.on("GET", "/rest/api/3/issueLinkType", reply(200, `{"issueLinkTypes":[{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"}]}`))
	ps, err := c.Priorities(t.Context())
	if err != nil || len(ps) != 2 || ps[0].Color != "#f00" {
		t.Fatalf("got %+v %v", ps, err)
	}
	lts, err := c.IssueLinkTypes(t.Context())
	if err != nil || lts[0].Outward != "blocks" {
		t.Fatalf("got %+v %v", lts, err)
	}
}
