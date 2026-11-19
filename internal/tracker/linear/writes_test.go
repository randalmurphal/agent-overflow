package linear

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

func issueResult(issue string) string {
	return `{"data":{"result":{"success":true,"value":` + issue + `}}}`
}

func TestCreateIssue(t *testing.T) {
	t.Parallel()
	issue := lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", -5)
	f := newFake(t,
		lineartest.Exchange{Op: "CreateIssue", Body: issueResult(issue), Vars: `{"input":{"teamId":"team-web","title":"Fix the login page",` +
			`"description":"Steps:\n1. open","stateId":"state-todo","assigneeId":null,"priority":0,"estimate":0,` +
			`"labelIds":["label-bug"],"cycleId":"cycle-1","projectId":"project-1","parentId":"WEB-0"}}`},
		lineartest.Exchange{Op: "CreateIssue", Body: issueResult(issue), Vars: `{"input":{"teamId":"team-web","title":"Minimal"}}`},
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
	issue := issueResult(lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1))
	f := newFake(t,
		lineartest.Exchange{Op: "UpdateIssue", Body: issue, Vars: `{"id":"WEB-1","input":{"title":"New title"}}`},
		lineartest.Exchange{Op: "UpdateIssue", Body: issue, Vars: `{"id":"WEB-1","input":{"assigneeId":null,"estimate":null,"cycleId":null,"projectId":null,"parentId":null}}`},
		lineartest.Exchange{Op: "UpdateIssue", Body: issue, Vars: `{"id":"WEB-1","input":{"assigneeId":"user-2","estimate":0,"priority":0,"cycleId":"cycle-2","stateId":"state-done","teamId":"team-app"}}`},
		lineartest.Exchange{Op: "UpdateIssue", Body: issue, Vars: `{"id":"WEB-1","input":{"description":"","labelIds":[]}}`},
		lineartest.Exchange{Op: "UpdateIssue", Body: issue, Vars: `{"id":"WEB-1","input":{"addedLabelIds":["label-a"],"removedLabelIds":["label-b"]}}`},
		lineartest.Exchange{Op: "UpdateIssue", Body: issue, Vars: `{"id":"WEB-1","input":{"labelIds":[]}}`},
		lineartest.Exchange{Op: "UpdateIssue", Body: issue, Vars: `{"id":"WEB-1","input":{"labelIds":[]}}`},
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

// A due date is sent as a calendar day on create and update, and Null
// clears it; a value that is not a calendar day is refused before
// sending.
func TestDueDate(t *testing.T) {
	t.Parallel()
	issue := lineartest.Result(lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1))
	f := newFake(t,
		lineartest.Exchange{Op: "CreateIssue", Vars: `{"input":{"teamId":"team-web","title":"T","dueDate":"2026-12-01"}}`, Body: issue},
		lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"dueDate":"2027-01-31"}}`, Body: issue},
		lineartest.Exchange{Op: "UpdateIssue", Vars: `{"id":"WEB-1","input":{"dueDate":null}}`, Body: issue},
	)
	c := f.client(t)
	ctx := context.Background()
	if _, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "team-web", Title: "T", DueDate: Set(Date("2026-12-01"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{DueDate: Set(Date("2027-01-31"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{DueDate: Null[Date]()}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Date{"", "2026-02-30", "2026-1-5", "01/12/2026", "2026-12-01T00:00:00Z"} {
		if _, err := c.UpdateIssue(ctx, "WEB-1", UpdateIssueInput{DueDate: Set(bad)}); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("update %q: %v", bad, err)
		}
		if _, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "t", Title: "x", DueDate: Set(bad)}); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("create %q: %v", bad, err)
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
		} else if !WriteNotApplied(err) || !errors.Is(err, trackerapi.ErrInvalidInput) || !strings.HasPrefix(err.Error(), "linear ") {
			t.Errorf("%s: %v is not a named refusal before sending", name, err)
		}
	}
}

func TestArchiveUnarchiveDelete(t *testing.T) {
	t.Parallel()
	archived := strings.Replace(lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1), `"archivedAt":null`, `"archivedAt":"2026-11-02T10:00:00.000Z"`, 1)
	f := newFake(t,
		lineartest.Exchange{Op: "ArchiveIssue", Vars: `{"id":"WEB-1"}`, Body: issueResult(archived)},
		lineartest.Exchange{Op: "UnarchiveIssue", Vars: `{"id":"WEB-1"}`, Body: issueResult(lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:01:00.000Z", 1))},
		lineartest.Exchange{Op: "DeleteIssue", Vars: `{"id":"WEB-1"}`, Body: `{"data":{"result":{"success":true}}}`},
		lineartest.Exchange{Op: "DeleteIssue", Vars: `{"id":"WEB-2","permanently":true}`, Body: `{"data":{"result":{"success":true}}}`},
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

var commentJSON = lineartest.NewComment().JSON()

func TestComments(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		lineartest.Exchange{Op: "CreateComment", Vars: `{"input":{"issueId":"WEB-1","body":"Agreed, **ship it**.","parentId":"comment-1"}}`, Body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		lineartest.Exchange{Op: "CreateComment", Vars: `{"input":{"issueId":"WEB-1","body":"Top level"}}`, Body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		lineartest.Exchange{Op: "UpdateComment", Vars: `{"id":"comment-2","input":{"body":"Edited"}}`, Body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		lineartest.Exchange{Op: "UpdateComment", Vars: `{"id":"comment-2","input":{"body":""}}`, Body: `{"data":{"result":{"success":true,"value":` + commentJSON + `}}}`},
		lineartest.Exchange{Op: "DeleteComment", Vars: `{"id":"comment-2"}`, Body: `{"data":{"result":{"success":true}}}`},
		lineartest.Exchange{Op: "DeleteComment", Vars: `{"id":"comment-3"}`, Body: `{"data":{"result":{"success":false}}}`},
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
	cycle := lineartest.Cycle{ID: "cycle-2", Number: 2, Team: "team-web", StartsAt: "2026-11-16T00:00:00.000Z", EndsAt: "2026-11-30T00:00:00.000Z", Active: true}.JSON()
	f := newFake(t,
		lineartest.Exchange{Op: "CreateRelation", Vars: `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"blocks"}}`, Body: `{"data":{"result":{"success":true,"value":` + relation + `}}}`},
		lineartest.Exchange{Op: "DeleteRelation", Vars: `{"id":"rel-1"}`, Body: `{"data":{"result":{"success":true}}}`},
		lineartest.Exchange{Op: "LinkURL", Vars: `{"input":{"issueId":"WEB-1","url":"https://git.example.com/web/pull/12","title":"Pull request 12","subtitle":"Open"}}`, Body: `{"data":{"result":{"success":true,"value":` + attachment + `}}}`},
		lineartest.Exchange{Op: "StartUpcomingCycle", Vars: `{"id":"cycle-2"}`, Body: `{"data":{"result":{"success":true,"value":` + cycle + `}}}`},
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
