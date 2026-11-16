package linear

import (
	"context"
	"testing"
)

func TestListIssuesVariables(t *testing.T) {
	t.Parallel()
	page := data(pageJSON([]string{issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)}, true, "c1"))
	f := newFake(t,
		exchange{op: "Issues", vars: `{"first":100,"sort":[{"manual":{"order":"Ascending"}}],"filter":{"and":[{"team":{"id":{"in":["team-web"]}}},{"state":{"id":{"in":["state-todo"]}}}]}}`, body: page},
		exchange{op: "Issues", vars: `{"first":100,"after":"c1","sort":[{"priority":{"order":"Ascending"}}],"includeArchived":true}`, body: page},
		exchange{op: "Issues", vars: `{"first":100,"sort":[{"updatedAt":{"order":"Descending"}}]}`, body: page},
		exchange{op: "Issues", vars: `{"first":100,"sort":[{"createdAt":{"order":"Descending"}}]}`, body: page},
	)
	c := f.client(t)
	ctx := context.Background()
	p, err := c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{Teams: []string{"team-web"}, States: []string{"state-todo"}}}, "")
	if err != nil || len(p.Items) != 1 || !p.HasNextPage || p.EndCursor != "c1" || p.Items[0].ProjectID != "project-1" {
		t.Fatalf("page %+v %v", p, err)
	}
	for _, q := range []struct {
		q     IssueQuery
		after string
	}{
		{IssueQuery{Order: OrderPriority, IncludeArchived: true}, "c1"},
		{IssueQuery{Order: OrderUpdatedNewest}, ""},
		{IssueQuery{Order: OrderCreatedNewest}, ""},
	} {
		if _, err := c.ListIssues(ctx, q.q, q.after); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ListIssues(ctx, IssueQuery{Order: 99}, ""); err == nil {
		t.Error("unknown order accepted")
	}
	if _, err := c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{Teams: []string{""}}}, ""); err == nil {
		t.Error("bad filter accepted")
	}
}

// A page that does not say whether more follow, or says so without a
// cursor to continue from, is an error, never the end of the list.
func TestPageInfoChecked(t *testing.T) {
	t.Parallel()
	node := issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	vars := `{"first":100,"sort":[{"manual":{"order":"Ascending"}}]}`
	for name, info := range map[string]string{
		"missing hasNextPage": `{"endCursor":"c1"}`,
		"null hasNextPage":    `{"hasNextPage":null,"endCursor":"c1"}`,
		"next without cursor": `{"hasNextPage":true,"endCursor":null}`,
	} {
		f := newFake(t, exchange{op: "Issues", vars: vars, body: data(`"page":{"nodes":[` + node + `],"pageInfo":` + info + `}`)})
		if p, err := f.client(t).ListIssues(context.Background(), IssueQuery{}, ""); err == nil {
			t.Errorf("%s: page %+v", name, p)
		}
	}
}

func TestIssueDetail(t *testing.T) {
	t.Parallel()
	ref := func(id, key string) string {
		return `{"id":"` + id + `","identifier":"` + key + `","title":"T","state":{"id":"state-todo"}}`
	}
	detail := issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	detail = detail[:len(detail)-1] + `,"parent":` + ref("issue-0", "WEB-0") +
		`,"children":{"nodes":[` + ref("issue-2", "WEB-2") + `],"pageInfo":{"hasNextPage":true}}` +
		`,"comments":{"nodes":[` + commentJSON + `],"pageInfo":{"hasNextPage":true,"endCursor":"cc1"}}` +
		`,"attachments":{"nodes":[],"pageInfo":{"hasNextPage":false}}` +
		`,"relations":{"nodes":[{"id":"rel-1","type":"blocks","issue":` + ref("issue-1", "WEB-1") + `,"relatedIssue":` + ref("issue-3", "WEB-3") + `}],"pageInfo":{"hasNextPage":false}}` +
		`,"inverseRelations":{"nodes":[{"id":"rel-2","type":"related","issue":` + ref("issue-4", "WEB-4") + `,"relatedIssue":` + ref("issue-1", "WEB-1") + `}],"pageInfo":{"hasNextPage":false}}}`
	history := `{"id":"h1","createdAt":"2026-11-03T10:00:00.000Z","actorId":"user-1","fromStateId":"state-todo","toStateId":"state-done",` +
		`"fromAssigneeId":null,"toAssigneeId":"user-2","fromCycleId":"cycle-1","toCycleId":null,"fromProjectId":null,"toProjectId":null,` +
		`"fromParentId":null,"toParentId":null,"fromTeamId":null,"toTeamId":null,"fromEstimate":null,"toEstimate":3,"fromPriority":null,"toPriority":null,` +
		`"fromTitle":null,"toTitle":null,"addedLabelIds":["label-bug"],"removedLabelIds":null,"archived":null}`
	span := `{"id":"span-1","stateId":"state-todo","startedAt":"2026-11-01T09:00:00.000Z","endedAt":null}`
	f := newFake(t,
		exchange{op: "Issue", vars: `{"id":"WEB-1"}`, body: `{"data":{"issue":` + detail + `}}`},
		exchange{op: "IssueComments", vars: `{"id":"issue-1","first":100,"after":"cc1"}`, body: `{"data":{"node":{` + pageJSON([]string{commentJSON}, false, "cc2") + `}}}`},
		exchange{op: "IssueHistory", vars: `{"id":"issue-1","first":100}`, body: `{"data":{"node":{` + pageJSON([]string{history}, false, "") + `}}}`},
		exchange{op: "IssueStateSpans", vars: `{"id":"issue-1","first":100}`, body: `{"data":{"node":{` + pageJSON([]string{span}, false, "") + `}}}`},
	)
	c := f.client(t)
	ctx := context.Background()
	d, err := c.Issue(ctx, "WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Identifier != "WEB-1" || d.Parent == nil || d.Parent.Identifier != "WEB-0" || d.ParentID != "issue-0" ||
		len(d.Children) != 1 || !d.MoreChildren || len(d.Comments.Items) != 1 || d.Comments.EndCursor != "cc1" || !d.Comments.HasNextPage ||
		len(d.Attachments) != 0 || d.MoreAttachments || len(d.Relations) != 2 || d.Relations[1].Issue.Identifier != "WEB-4" {
		t.Fatalf("detail %+v", d)
	}
	comments, err := c.IssueComments(ctx, "issue-1", d.Comments.EndCursor)
	if err != nil || len(comments.Items) != 1 || comments.HasNextPage {
		t.Fatalf("comments %+v %v", comments, err)
	}
	h, err := c.IssueHistory(ctx, "issue-1", "")
	if err != nil || len(h.Items) != 1 {
		t.Fatalf("history %+v %v", h, err)
	}
	e := h.Items[0]
	if *e.State != (Change{"state-todo", "state-done"}) || *e.Assignee != (Change{"", "user-2"}) || *e.Cycle != (Change{"cycle-1", ""}) ||
		e.Project != nil || e.Estimate.From != nil || *e.Estimate.To != 3 || e.Priority != nil || e.Title != nil || e.AddedLabelIDs[0] != "label-bug" {
		t.Errorf("history entry %+v", e)
	}
	spans, err := c.IssueStateSpans(ctx, "issue-1", "")
	if err != nil || len(spans.Items) != 1 || spans.Items[0].EndedAt != nil || spans.Items[0].StateID != "state-todo" {
		t.Fatalf("spans %+v %v", spans, err)
	}
	for name, err := range map[string]error{
		"issue":    func() error { _, err := c.Issue(ctx, ""); return err }(),
		"comments": func() error { _, err := c.IssueComments(ctx, "", ""); return err }(),
		"history":  func() error { _, err := c.IssueHistory(ctx, "", ""); return err }(),
		"spans":    func() error { _, err := c.IssueStateSpans(ctx, "", ""); return err }(),
	} {
		if err == nil {
			t.Errorf("%s: an empty ID was accepted", name)
		}
	}
}
