package linear

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func issueResult(issue string) string {
	return `{"data":{"result":{"success":true,"value":` + issue + `}}}`
}

func TestCreateIssue(t *testing.T) {
	t.Parallel()
	issue := issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", -5)
	f := newFake(t,
		exchange{op: "CreateIssue", body: issueResult(issue), vars: `{"input":{"teamId":"team-web","title":"Fix the login page",` +
			`"description":"Steps:\n1. open","stateId":"state-todo","assigneeId":null,"priority":0,"estimate":0,` +
			`"labelIds":["label-bug"],"cycleId":"cycle-1","projectId":"project-1","parentId":"WEB-0"}}`},
		exchange{op: "CreateIssue", body: issueResult(issue), vars: `{"input":{"teamId":"team-web","title":"Minimal"}}`},
	)
	c := f.client(t)
	got, err := c.CreateIssue(context.Background(), CreateIssueInput{
		TeamID: "team-web", Title: "Fix the login page", Description: "Steps:\n1. open",
		StateID: Set("state-todo"), AssigneeID: Null[string](), Priority: Set(PriorityNone), Estimate: Set(0),
		LabelIDs: []string{"label-bug"}, CycleID: Set("cycle-1"), ProjectID: Set("project-1"), ParentID: Set("WEB-0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Identifier != "WEB-1" || got.SortOrder != -5 || *got.Estimate != 3 || got.Priority != PriorityHigh ||
		got.TeamID != "team-web" || got.StateID != "state-todo" || got.CycleID != "" || got.LabelIDs[0] != "label-bug" ||
		got.DueDate != "2026-12-01" || got.SubIssueSortOrder != nil {
		t.Errorf("issue %+v", got)
	}
	if _, err := c.CreateIssue(context.Background(), CreateIssueInput{TeamID: "team-web", Title: "Minimal"}); err != nil {
		t.Fatal(err)
	}
}

// Each Optional state reaches the wire as itself: an unset field is
// absent, Set sends the value (a zero value included), Null sends null;
// clearing a description or the labels sends what Linear clears with.
func TestUpdateIssueOptionalStates(t *testing.T) {
	t.Parallel()
	issue := issueResult(issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1))
	f := newFake(t,
		exchange{op: "UpdateIssue", body: issue, vars: `{"id":"WEB-1","input":{"title":"New title"}}`},
		exchange{op: "UpdateIssue", body: issue, vars: `{"id":"WEB-1","input":{"assigneeId":null,"estimate":null,"cycleId":null,"projectId":null,"parentId":null}}`},
		exchange{op: "UpdateIssue", body: issue, vars: `{"id":"WEB-1","input":{"assigneeId":"user-2","estimate":0,"priority":0,"cycleId":"cycle-2","stateId":"state-done","teamId":"team-app"}}`},
		exchange{op: "UpdateIssue", body: issue, vars: `{"id":"WEB-1","input":{"description":"","labelIds":[]}}`},
		exchange{op: "UpdateIssue", body: issue, vars: `{"id":"WEB-1","input":{"addedLabelIds":["label-a"],"removedLabelIds":["label-b"]}}`},
		exchange{op: "UpdateIssue", body: issue, vars: `{"id":"WEB-1","input":{"labelIds":[]}}`},
		exchange{op: "UpdateIssue", body: issue, vars: `{"id":"WEB-1","input":{"labelIds":[]}}`},
	)
	c := f.client(t)
	ctx := context.Background()
	for _, in := range []UpdateIssueInput{
		{Title: Set("New title")},
		{AssigneeID: Null[string](), Estimate: Null[int](), CycleID: Null[string](), ProjectID: Null[string](), ParentID: Null[string]()},
		{AssigneeID: Set("user-2"), Estimate: Set(0), Priority: Set(PriorityNone), CycleID: Set("cycle-2"), StateID: Set("state-done"), TeamID: Set("team-app")},
		{Description: Null[string](), LabelIDs: Null[[]string]()},
		{AddedLabelIDs: []string{"label-a"}, RemovedLabelIDs: []string{"label-b"}},
		// Set of a nil or an empty list sends an empty list, never null.
		{LabelIDs: Set[[]string](nil)},
		{LabelIDs: Set([]string{})},
	} {
		if _, err := c.UpdateIssue(ctx, "WEB-1", in); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

// Inputs Linear would refuse or misread are refused before sending, as
// errors that prove nothing was applied. The fake expects no request.
func TestWriteValidation(t *testing.T) {
	t.Parallel()
	c := newFake(t).client(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"create without team":  func() error { _, err := c.CreateIssue(ctx, CreateIssueInput{Title: "x"}); return err },
		"create without title": func() error { _, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "t"}); return err },
		"create null state": func() error {
			_, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "t", Title: "x", StateID: Null[string]()})
			return err
		},
		"create null priority": func() error {
			_, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "t", Title: "x", Priority: Null[Priority]()})
			return err
		},
		"create empty label": func() error {
			_, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "t", Title: "x", LabelIDs: []string{""}})
			return err
		},
		"update nothing":    func() error { _, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{}); return err },
		"update without id": func() error { _, err := c.UpdateIssue(ctx, "", UpdateIssueInput{Title: Set("x")}); return err },
		"clear title": func() error {
			_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{Title: Null[string]()})
			return err
		},
		"empty title": func() error { _, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{Title: Set("")}); return err },
		"clear state": func() error {
			_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{StateID: Null[string]()})
			return err
		},
		"clear team": func() error {
			_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{TeamID: Null[string]()})
			return err
		},
		"null priority": func() error {
			_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{Priority: Null[Priority]()})
			return err
		},
		"set an empty assignee": func() error { _, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{AssigneeID: Set("")}); return err },
		"labels and added": func() error {
			_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{LabelIDs: Set([]string{"a"}), AddedLabelIDs: []string{"b"}})
			return err
		},
		"empty removed label": func() error {
			_, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{RemovedLabelIDs: []string{""}})
			return err
		},
		"archive without id":   func() error { _, err := c.ArchiveIssue(ctx, ""); return err },
		"delete without id":    func() error { return c.DeleteIssue(ctx, "", false) },
		"comment without body": func() error { _, err := c.CreateComment(ctx, CreateCommentInput{IssueID: "WEB-1"}); return err },
		"update comment no id": func() error { _, err := c.UpdateComment(ctx, "", "x"); return err },
		"delete comment no id": func() error { return c.DeleteComment(ctx, "") },
		"relation without type": func() error {
			_, err := c.CreateRelation(ctx, CreateRelationInput{IssueID: "a", RelatedIssueID: "b"})
			return err
		},
		"deprecated similar relation": func() error {
			_, err := c.CreateRelation(ctx, CreateRelationInput{IssueID: "a", RelatedIssueID: "b", Type: RelationSimilar})
			return err
		},
		"link without title": func() error {
			_, err := c.LinkURL(ctx, LinkInput{IssueID: "WEB-1", URL: "https://example.com/pr/1"})
			return err
		},
		"start without cycle": func() error { _, err := c.StartUpcomingCycle(ctx, ""); return err },
	} {
		err := call()
		if err == nil {
			t.Errorf("%s: no error", name)
		} else if !WriteNotApplied(err) || !strings.HasPrefix(err.Error(), "linear ") {
			t.Errorf("%s: %v is not a named refusal before sending", name, err)
		}
	}
}

func TestArchiveUnarchiveDelete(t *testing.T) {
	t.Parallel()
	archived := strings.Replace(issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1), `"archivedAt":null`, `"archivedAt":"2026-11-02T10:00:00.000Z"`, 1)
	f := newFake(t,
		exchange{op: "ArchiveIssue", vars: `{"id":"WEB-1"}`, body: issueResult(archived)},
		exchange{op: "UnarchiveIssue", vars: `{"id":"WEB-1"}`, body: issueResult(issueJSON("issue-1", "WEB-1", "2026-11-02T10:01:00.000Z", 1))},
		exchange{op: "DeleteIssue", vars: `{"id":"WEB-1"}`, body: `{"data":{"result":{"success":true}}}`},
		exchange{op: "DeleteIssue", vars: `{"id":"WEB-2","permanently":true}`, body: `{"data":{"result":{"success":true}}}`},
	)
	c := f.client(t)
	ctx := context.Background()
	got, err := c.ArchiveIssue(ctx, "WEB-1")
	if err != nil || got.ArchivedAt == nil {
		t.Fatalf("archive: %+v %v", got, err)
	}
	if got, err := c.UnarchiveIssue(ctx, "WEB-1"); err != nil || got.ArchivedAt != nil {
		t.Fatalf("unarchive: %+v %v", got, err)
	}
	if err := c.DeleteIssue(ctx, "WEB-1", false); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteIssue(ctx, "WEB-2", true); err != nil {
		t.Fatal(err)
	}
}

const commentJSON = `{"id":"comment-2","issue":{"id":"issue-1"},"parent":{"id":"comment-1"},"user":{"id":"user-1"},"body":"Agreed, **ship it**.",` +
	`"url":"https://linear.app/example/issue/WEB-1#comment-2","createdAt":"2026-11-02T10:00:00.000Z","updatedAt":"2026-11-02T10:00:00.000Z","editedAt":null,"resolvedAt":null}`

func TestComments(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		exchange{op: "CreateComment", vars: `{"input":{"issueId":"WEB-1","body":"Agreed, **ship it**.","parentId":"comment-1"}}`, body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		exchange{op: "CreateComment", vars: `{"input":{"issueId":"WEB-1","body":"Top level"}}`, body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		exchange{op: "UpdateComment", vars: `{"id":"comment-2","input":{"body":"Edited"}}`, body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		exchange{op: "UpdateComment", vars: `{"id":"comment-2","input":{"body":""}}`, body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		exchange{op: "DeleteComment", vars: `{"id":"comment-2"}`, body: `{"data":{"result":{"success":true}}}`},
		exchange{op: "DeleteComment", vars: `{"id":"comment-3"}`, body: `{"data":{"result":{"success":false}}}`},
	)
	c := f.client(t)
	ctx := context.Background()
	got, err := c.CreateComment(ctx, CreateCommentInput{IssueID: "WEB-1", Body: "Agreed, **ship it**.", ParentID: "comment-1"})
	if err != nil || got.ParentID != "comment-1" || got.IssueID != "issue-1" || got.UserID != "user-1" {
		t.Fatalf("create: %+v %v", got, err)
	}
	if _, err := c.CreateComment(ctx, CreateCommentInput{IssueID: "WEB-1", Body: "Top level"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateComment(ctx, "comment-2", "Edited"); err != nil {
		t.Fatal(err)
	}
	// The schema documents an empty body as clearing the comment.
	if _, err := c.UpdateComment(ctx, "comment-2", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteComment(ctx, "comment-2"); err != nil {
		t.Fatal(err)
	}
	err = c.DeleteComment(ctx, "comment-3")
	if e, ok := err.(*Error); !ok || !e.Unsuccessful || !WriteNotApplied(err) {
		t.Errorf("success false: %v", err)
	}
}

func TestRelationsLinksAndCycles(t *testing.T) {
	t.Parallel()
	relation := `{"id":"rel-1","type":"blocks","issue":{"id":"issue-1","identifier":"WEB-1","title":"A","state":{"id":"s"}},"relatedIssue":{"id":"issue-2","identifier":"WEB-2","title":"B","state":{"id":"s"}}}`
	attachment := `{"id":"att-1","issue":{"id":"issue-1"},"title":"Pull request 12","subtitle":"Open","url":"https://git.example.com/web/pull/12","sourceType":"api","creator":{"id":"user-1"},"createdAt":"2026-11-02T10:00:00.000Z","updatedAt":"2026-11-02T10:00:00.000Z"}`
	cycle := `{"id":"cycle-2","number":2,"name":null,"description":null,"team":{"id":"team-web"},"startsAt":"2026-11-16T00:00:00.000Z","endsAt":"2026-11-30T00:00:00.000Z",` +
		`"completedAt":null,"archivedAt":null,"isActive":true,"isFuture":false,"isPast":false,"isNext":false,"isPrevious":false,"progress":0,` +
		`"scopeHistory":[3],"completedScopeHistory":[0],"inProgressScopeHistory":[0],"issueCountHistory":[2],"completedIssueCountHistory":[0]}`
	f := newFake(t,
		exchange{op: "CreateRelation", vars: `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"blocks"}}`, body: `{"data":{"result":{"success":true,"value":` + relation + `}}}`},
		exchange{op: "DeleteRelation", vars: `{"id":"rel-1"}`, body: `{"data":{"result":{"success":true}}}`},
		exchange{op: "LinkURL", vars: `{"input":{"issueId":"WEB-1","url":"https://git.example.com/web/pull/12","title":"Pull request 12","subtitle":"Open"}}`, body: `{"data":{"result":{"success":true,"value":` + attachment + `}}}`},
		exchange{op: "StartUpcomingCycle", vars: `{"id":"cycle-2"}`, body: `{"data":{"result":{"success":true,"value":` + cycle + `}}}`},
	)
	c := f.client(t)
	ctx := context.Background()
	rel, err := c.CreateRelation(ctx, CreateRelationInput{IssueID: "WEB-1", RelatedIssueID: "WEB-2", Type: RelationBlocks})
	if err != nil || rel.Type != RelationBlocks || rel.Issue.Identifier != "WEB-1" || rel.Related.Identifier != "WEB-2" {
		t.Fatalf("relation %+v %v", rel, err)
	}
	if err := c.DeleteRelation(ctx, "rel-1"); err != nil {
		t.Fatal(err)
	}
	att, err := c.LinkURL(ctx, LinkInput{IssueID: "WEB-1", URL: "https://git.example.com/web/pull/12", Title: "Pull request 12", Subtitle: "Open"})
	if err != nil || att.Title != "Pull request 12" || att.CreatorID != "user-1" {
		t.Fatalf("link %+v %v", att, err)
	}
	cy, err := c.StartUpcomingCycle(ctx, "cycle-2")
	if err != nil || !cy.IsActive || cy.Number != 2 || cy.ScopeHistory[0] != 3 {
		t.Fatalf("cycle %+v %v", cy, err)
	}
}

// The three Optional states marshal as themselves, and an unset field
// without omitzero fails the encoding instead of clearing the field.
func TestOptionalMarshal(t *testing.T) {
	t.Parallel()
	type in struct {
		A Optional[int]    `json:"a,omitzero"`
		B Optional[int]    `json:"b,omitzero"`
		C Optional[string] `json:"c,omitzero"`
	}
	got, err := json.Marshal(in{A: Set(0), B: Null[int]()})
	if err != nil || string(got) != `{"a":0,"b":null}` {
		t.Errorf("marshal %s %v", got, err)
	}
	var bare struct{ A Optional[int] }
	if _, err := json.Marshal(bare); err == nil {
		t.Error("an unset Optional without omitzero encoded")
	}
	o := Set("x")
	if !o.IsSet() || o.IsNull() || o.IsZero() || o.Value() != "x" {
		t.Errorf("Set: %+v", o)
	}
	if n := Null[string](); n.IsSet() || !n.IsNull() || n.IsZero() {
		t.Errorf("Null: %+v", n)
	}
	var u Optional[string]
	if u.IsSet() || u.IsNull() || !u.IsZero() {
		t.Errorf("unset: %+v", u)
	}
}
