package linear

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
)

func TestListIssuesVariables(t *testing.T) {
	t.Parallel()
	page := lineartest.Data(lineartest.PageJSON([]string{lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)}, true, "c1"))
	f := newFake(t,
		lineartest.Exchange{Op: "Issues", Vars: `{"first":250,"sort":[{"manual":{"order":"Ascending"}}],"filter":{"and":[{"team":{"id":{"in":["team-web"]}}},{"state":{"id":{"in":["state-todo"]}}}]}}`, Body: page},
		lineartest.Exchange{Op: "Issues", Vars: `{"first":250,"after":"c1","sort":[{"priority":{"order":"Ascending"}}],"includeArchived":true}`, Body: page},
		lineartest.Exchange{Op: "Issues", Vars: `{"first":250,"sort":[{"createdAt":{"order":"Descending"}}]}`, Body: page},
		lineartest.Exchange{Op: "Issues", Vars: `{"first":250,"sort":[{"createdAt":{"order":"Ascending"}}],"filter":{"and":[{"or":[{"and":[{"state":{"type":{"in":["started"]}}}]},{"and":[{"updatedAt":{"gte":"2026-11-01T00:00:00.000Z"}}]}]}]}}`, Body: page},
	)
	c := f.client(t)
	ctx := context.Background()
	p, err := c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{Teams: []string{"team-web"}, States: []string{"state-todo"}}}, "")
	if err != nil || len(p.Items) != 1 || !p.HasNextPage || p.EndCursor != "c1" || p.Items[0].ProjectID != "project-1" ||
		p.Items[0].TeamID != "team-web" || p.Items[0].TeamEstimateScale != EstimateFibonacci ||
		len(p.Items[0].Labels) != 1 || p.Items[0].Labels[0] != (LabelRef{ID: "label-bug", Name: "Bug"}) {
		t.Fatalf("page %+v %v", p, err)
	}
	for _, q := range []struct {
		q     IssueQuery
		after string
	}{
		{IssueQuery{Order: OrderPriority, IncludeArchived: true}, "c1"},
		{IssueQuery{Order: OrderCreatedNewest}, ""},
		{IssueQuery{Order: OrderCreatedOldest, Filter: IssueFilter{AnyOf: []IssueFilter{
			{StateTypes: []StateType{StateStarted}}, {UpdatedSince: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		}}}, ""},
	} {
		if _, err := c.ListIssues(ctx, q.q, q.after); err != nil {
			t.Fatal(err)
		}
	}
	// No page in an order an update changes follows a page cursor.
	for _, o := range []IssueOrder{orderUpdatedOldest, orderUpdatedNewest} {
		if _, err := issueVars(IssueQuery{Order: o}, "c1"); err == nil {
			t.Errorf("order %d paged by cursor", o)
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
	node := lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	vars := `{"first":250,"sort":[{"manual":{"order":"Ascending"}}]}`
	for name, info := range map[string]string{
		"missing hasNextPage": `{"endCursor":"c1"}`,
		"null hasNextPage":    `{"hasNextPage":null,"endCursor":"c1"}`,
		"next without cursor": `{"hasNextPage":true,"endCursor":null}`,
	} {
		f := newFake(t, lineartest.Exchange{Op: "Issues", Vars: vars, Body: lineartest.Data(`"page":{"nodes":[` + node + `],"pageInfo":` + info + `}`)})
		if p, err := f.client(t).ListIssues(context.Background(), IssueQuery{}, ""); err == nil {
			t.Errorf("%s: page %+v", name, p)
		}
	}
}

func TestIssueDetail(t *testing.T) {
	t.Parallel()
	ref := func(id, key string) lineartest.IssueRef {
		return lineartest.IssueRef{ID: id, Identifier: key, Title: "T", State: "state-todo"}
	}
	parent := ref("issue-0", "WEB-0")
	detail := lineartest.DetailJSON(lineartest.NewIssue("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1), lineartest.Detail{
		Parent: &parent, Children: []string{ref("issue-2", "WEB-2").JSON()}, MoreChildren: true,
		Comments: []string{commentJSON}, MoreComments: true, CommentCursor: "cc1",
		Relations:        []string{lineartest.RelationJSON("rel-1", "blocks", ref("issue-1", "WEB-1"), ref("issue-3", "WEB-3"))},
		InverseRelations: []string{lineartest.RelationJSON("rel-2", "related", ref("issue-4", "WEB-4"), ref("issue-1", "WEB-1"))},
	})
	three := 3.0
	history := lineartest.HistoryEntry{
		ID: "h1", CreatedAt: "2026-11-03T10:00:00.000Z", UpdatedAt: "2026-11-03T10:00:05.000Z", Actor: "user-1", FromState: "state-todo", ToState: "state-done",
		ToAssignee: "user-2", FromCycle: "cycle-1", ToEstimate: &three, AddedLabelIDs: []string{"label-bug"},
	}.JSON()
	span := `{"id":"span-1","stateId":"state-todo","startedAt":"2026-11-01T09:00:00.000Z","endedAt":null}`
	f := newFake(t,
		lineartest.Exchange{Op: "Issue", Vars: `{"id":"WEB-1"}`, Body: `{"data":{"issue":` + detail + `}}`},
		lineartest.Exchange{Op: "IssueComments", Vars: `{"id":"issue-1","first":100,"after":"cc1"}`, Body: `{"data":{"node":{` + lineartest.PageJSON([]string{commentJSON}, false, "cc2") + `}}}`},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"issue-1","first":250}`, Body: `{"data":{"node":{` + lineartest.PageJSON([]string{history}, false, "") + `}}}`},
		lineartest.Exchange{Op: "IssueStateSpans", Vars: `{"id":"issue-1","first":250}`, Body: `{"data":{"node":{` + lineartest.PageJSON([]string{span}, false, "") + `}}}`},
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
		e.Project != nil || e.Estimate.From != nil || *e.Estimate.To != 3 || e.Priority != nil || e.Title != nil || e.AddedLabelIDs[0] != "label-bug" ||
		e.UpdatedAt.Sub(e.CreatedAt) != 5*time.Second {
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
