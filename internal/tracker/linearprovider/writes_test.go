package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

const (
	userErrorBody = `{"data":null,"errors":[{"message":"Assignee is not a member of the team","extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	inputBody     = `{"errors":[{"message":"Argument Validation Error","extensions":{"type":"invalid input","code":"INPUT_ERROR","userError":true}}]}`
	limitBody     = `{"errors":[{"message":"Rate limit exceeded","extensions":{"type":"ratelimited","code":"RATELIMITED","userError":true}}]}`
)

func applied(value string) string {
	return `{"data":{"result":{"success":true,"value":` + value + `}}}`
}

// Each answer to a write is reported by what it proves: only a present
// success: false, an HTTP 4xx or user errors with no payload are
// refusals; a rate limit or a call that never left is not sent; anything
// else short of a confirmed success is unknown. Nothing is sent twice.
func TestWriteOutcomes(t *testing.T) {
	t.Parallel()
	vars := `{"id":"` + issueID(1) + `","input":{"assigneeId":"user-2"}}`
	for _, tc := range []struct {
		name string
		x    lineartest.Exchange
		want string
	}{
		{"success", lineartest.Exchange{Body: applied(issue(1, 1).JSON())}, "1:edit:applied"},
		{"success false", lineartest.Exchange{Body: `{"data":{"result":{"success":false,"value":null}}}`}, "1:edit:refused(tracker)"},
		{"HTTP 400", lineartest.Exchange{Status: 400, Body: inputBody}, "1:edit:refused(tracker)"},
		{"user errors without payload", lineartest.Exchange{Body: userErrorBody}, "1:edit:refused(tracker)"},
		{"HTTP 500", lineartest.Exchange{Status: 500, Body: `{"errors":[{"message":"boom"}]}`}, "1:edit:unknown"},
		{"success missing", lineartest.Exchange{Body: `{"data":{"result":{"value":` + issue(1, 1).JSON() + `}}}`}, "1:edit:unknown"},
		{"success without the issue", lineartest.Exchange{Body: `{"data":{"result":{"success":true,"value":null}}}`}, "1:edit:unknown"},
		{"connection reset", lineartest.Exchange{Reset: true}, "1:edit:unknown"},
		{"rate limited", lineartest.Exchange{Status: 429, Body: limitBody, Header: map[string]string{"Retry-After": "30"}}, "1:edit:not sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := tc.x
			x.Op, x.Vars = "UpdateIssue", vars
			p, _ := newProvider(t, x)
			r := p.Edit(context.Background(), tracker.EditInput{Issue: tracker.IssueID(issueID(1)),
				Fields: []tracker.FieldEdit{{Field: tracker.FieldAssignee, Value: tracker.PickValues("user-2")}}})
			expectResult(t, r, tc.want)
			if r.Parts[0].Outcome.State() != tracker.StateApplied && r.Parts[0].Outcome.Err() == nil {
				t.Error("a failed part carries no error")
			}
			if !reflect.DeepEqual(r.Parts[0].Changed, []tracker.Resource{tracker.CardOf(tracker.IssueID(issueID(1))), tracker.DetailOf(tracker.IssueID(issueID(1)))}) {
				t.Errorf("changed %+v", r.Parts[0].Changed)
			}
		})
	}
	// A refusal carries Linear's own words.
	p, _ := newProvider(t, lineartest.Exchange{Op: "UpdateIssue", Vars: vars, Body: userErrorBody})
	r := p.Edit(context.Background(), tracker.EditInput{Issue: tracker.IssueID(issueID(1)),
		Fields: []tracker.FieldEdit{{Field: tracker.FieldAssignee, Value: tracker.PickValues("user-2")}}})
	if ref, _ := r.Parts[0].Outcome.Refusal(); ref.Message != "Assignee is not a member of the team" {
		t.Errorf("refusal message %q", ref.Message)
	}
}

// A call cancelled before it is sent is not sent; input the client
// refuses before sending is refused as invalid, not reported not sent.
func TestWriteNotSentAndClientRefusal(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := p.Move(ctx, tracker.MoveInput{Issue: tracker.IssueID(issueID(1)), Move: "s-done"})
	expectResult(t, r, "1:move:not sent")
	_, err := p.c.UpdateIssue(context.Background(), issueID(1), linear.UpdateIssueInput{Title: linear.Null[string]()})
	if o := outcome(err); o.State() != tracker.StateRefused {
		t.Errorf("client refusal: %s (%v)", o.State(), err)
	} else if ref, _ := o.Refusal(); ref.Reason != tracker.ReasonInvalid {
		t.Errorf("client refusal reason %d", ref.Reason)
	}
}

// Writes Linear cannot make are refused as unsupported before any
// request: the fake fails the test on any request.
func TestUnsupportedWrites(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t)
	ctx := context.Background()
	id := tracker.IssueID(issueID(1))
	start, end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	name := "Next"
	for name, r := range map[string]tracker.Result{
		"create sprint":   p.CreateSprint(ctx, tracker.SprintInput{Board: web, Name: "Cycle", Start: start, End: end}),
		"start sprint":    p.StartSprint(ctx, tracker.SprintStart{Board: web, Sprint: "c1", Start: start, End: end}),
		"complete sprint": p.CompleteSprint(ctx, tracker.SprintCompletion{Board: web, Sprint: "c1"}),
		"edit sprint":     p.EditSprint(ctx, tracker.SprintEdit{Board: web, Sprint: "c1", Name: &name}),
		"attach":          p.Attach(ctx, tracker.AttachInput{Issue: id, Files: []tracker.File{{Name: "a.txt", Content: strings.NewReader("x")}}}),
		"move with fields": p.Move(ctx, tracker.MoveInput{Issue: id, Move: "s-done",
			Fields: []tracker.FieldEdit{{Field: tracker.FieldAssignee, Value: tracker.PickValues("user-1")}}}),
		"field beyond the list": p.Edit(ctx, tracker.EditInput{Issue: id,
			Fields: []tracker.FieldEdit{{Field: "customfield_10001", Value: tracker.TypedValues("x")}}}),
		"create with a field beyond the list": p.Create(ctx, tracker.CreateInput{Project: web, Title: "T",
			Fields: []tracker.FieldEdit{{Field: "components", Value: tracker.PickValues("x")}}}),
		"local images": p.Create(ctx, tracker.CreateInput{Project: web, Title: "T", ImageDir: "/tmp/x"}),
	} {
		checkResult(t, r)
		ref, ok := r.Parts[0].Outcome.Refusal()
		if !ok || ref.Reason != tracker.ReasonUnsupported || len(r.Parts) != 1 {
			t.Errorf("%s: %s", name, describe(r))
		}
	}
	err := p.Download(ctx, tracker.DownloadInput{Issue: id, Attachment: "a", To: io.Discard, Limit: 1})
	if !errors.Is(err, tracker.ErrUnsupported) {
		t.Errorf("download: %v", err)
	}
	// Input checks still come first.
	expectResult(t, p.CreateSprint(ctx, tracker.SprintInput{}), "-:sprint:refused(invalid)")
	if err := p.Download(ctx, tracker.DownloadInput{}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("download without input: %v", err)
	}
}

// A create resolves typed values in the team (each catalog read once),
// sends IDs, and names the new issue's card, row and detail.
func TestCreate(t *testing.T) {
	t.Parallel()
	created := issue(5, 1)
	p, _ := newProvider(t,
		membersX(web, lineartest.User{ID: "user-1", Name: "User 1", Email: "user1@example.com", Active: true}),
		labelsX(web, lineartest.Label{ID: "l-1", Name: "Bug"}, lineartest.Label{ID: "l-2", Name: "Feature"}),
		lineartest.Exchange{Op: "Projects", Vars: `{"first":100,"filter":{"accessibleTeams":{"some":{"id":{"eq":"` + web + `"}}}}}`,
			Body: page(lineartest.Project{ID: "project-1", Name: "Launch"}.JSON())},
		lineartest.Exchange{Op: "CreateIssue", Body: applied(created.JSON()), Vars: `{"input":{"teamId":"` + web + `","title":"New",` +
			`"description":"Body","assigneeId":"user-1","priority":2,"labelIds":["l-1","l-2"],"parentId":"` + issueID(2) + `",` +
			`"projectId":"project-1","dueDate":"2026-12-24"}}`},
	)
	twice := []tracker.FieldEdit{{Field: FieldDueDate, Value: tracker.ClearField()}, {Field: FieldDueDate, Value: tracker.ClearField()}}
	expectResult(t, p.Create(context.Background(), tracker.CreateInput{Project: web, Title: "New", Fields: twice}), "-:create:refused(invalid)")
	r := p.Create(context.Background(), tracker.CreateInput{Project: web, Title: "New", Fields: []tracker.FieldEdit{
		{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit("Body", "")},
		{Field: tracker.FieldAssignee, Value: tracker.TypedValues("USER1@example.com")},
		{Field: tracker.FieldPriority, Value: tracker.TypedValues("high")},
		{Field: tracker.FieldLabels, Value: tracker.TypedValues("bug", " Feature ")},
		{Field: tracker.FieldParent, Value: tracker.PickValues(issueID(2))},
		{Field: FieldProject, Value: tracker.TypedValues("launch")},
		{Field: FieldDueDate, Value: tracker.TypedValues("2026-12-24")},
	}})
	expectResult(t, r, "5:create:applied")
	id := tracker.IssueID(issueID(5))
	if got, ok := r.Created(); !ok || got != id {
		t.Errorf("created %q", got)
	}
	if !reflect.DeepEqual(r.Parts[0].Changed, []tracker.Resource{tracker.CardOf(id), tracker.RowOf(web, id), tracker.DetailOf(id)}) {
		t.Errorf("changed %+v", r.Parts[0].Changed)
	}
}

// A create whose answer is lost is one unknown create part naming no
// issue; one Linear refuses is refused; neither is sent again.
func TestCreateLostAndRefused(t *testing.T) {
	t.Parallel()
	vars := `{"input":{"teamId":"` + web + `","title":"New"}}`
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "CreateIssue", Vars: vars, Reset: true},
		lineartest.Exchange{Op: "CreateIssue", Vars: vars, Body: `{"data":{"result":{"success":false,"value":null}}}`},
		lineartest.Exchange{Op: "CreateIssue", Vars: vars, Status: 500, Body: `{"errors":[{"message":"boom"}]}`},
	)
	in := tracker.CreateInput{Project: web, Title: "New"}
	r := p.Create(context.Background(), in)
	expectResult(t, r, "-:create:unknown")
	if !r.CreateUnknown() {
		t.Error("a lost create is not reported unknown")
	}
	expectResult(t, p.Create(context.Background(), in), "-:create:refused(tracker)")
	expectResult(t, p.Create(context.Background(), in), "-:create:unknown")
	expectResult(t, p.Create(context.Background(), tracker.CreateInput{Project: web, Type: "type-1", Title: "New"}), "-:create:refused(invalid)")
}

// Typed text that names nothing is refused with the candidates that
// start with it, or the first ones; text that names several is refused
// as ambiguous with all of them. Nothing is sent.
func TestTypedResolution(t *testing.T) {
	t.Parallel()
	members := []lineartest.User{
		{ID: "user-1", Name: "User 1", DisplayName: "u1", Active: true},
		{ID: "user-2", Name: "User 1", DisplayName: "u2", Active: true},
		{ID: "user-3", Name: "Other", DisplayName: "o3", Active: true},
	}
	p, _ := newProvider(t, membersX(web, members...), membersX(web, members...), membersX(web, members...))
	ctx := context.Background()
	create := func(text string) tracker.Result {
		return p.Create(ctx, tracker.CreateInput{Project: web, Title: "T",
			Fields: []tracker.FieldEdit{{Field: tracker.FieldAssignee, Value: tracker.TypedValues(text)}}})
	}
	candidates := func(r tracker.Result) string {
		ref, _ := r.Parts[0].Outcome.Refusal()
		var ids []string
		for _, c := range ref.Candidates {
			ids = append(ids, c.ID+"="+c.Name)
		}
		return strings.Join(ids, " ")
	}
	r := create("user 1")
	expectResult(t, r, "-:create:refused(ambiguous)")
	if candidates(r) != "user-1=User 1 user-2=User 1" {
		t.Errorf("ambiguous candidates %s", candidates(r))
	}
	r = create("Use")
	expectResult(t, r, "-:create:refused(no match)")
	if candidates(r) != "user-1=User 1 user-2=User 1" {
		t.Errorf("prefix candidates %s", candidates(r))
	}
	r = create("nobody")
	expectResult(t, r, "-:create:refused(no match)")
	if candidates(r) != "user-1=User 1 user-2=User 1 user-3=Other" {
		t.Errorf("fallback candidates %s", candidates(r))
	}
	for name, e := range map[string]tracker.FieldEdit{
		"priority":           {Field: tracker.FieldPriority, Value: tracker.TypedValues("critical")},
		"due date":           {Field: FieldDueDate, Value: tracker.TypedValues("next week")},
		"typed description":  {Field: tracker.FieldDescription, Value: tracker.TypedValues("x")},
		"two assignees":      {Field: tracker.FieldAssignee, Value: tracker.PickValues("user-1", "user-2")},
		"title cleared":      {Field: tracker.FieldTitle, Value: tracker.ClearField()},
		"picked due date":    {Field: FieldDueDate, Value: tracker.PickValues("2026-12-01")},
		"priority not a num": {Field: tracker.FieldPriority, Value: tracker.PickValues("high")},
	} {
		r := p.Edit(ctx, tracker.EditInput{Issue: tracker.IssueID(issueID(1)), Fields: []tracker.FieldEdit{e}})
		checkResult(t, r)
		if ref, ok := r.Parts[0].Outcome.Refusal(); !ok || (ref.Reason != tracker.ReasonNoMatch && ref.Reason != tracker.ReasonInvalid) {
			t.Errorf("%s: %s", name, describe(r))
		}
	}
}

// An edit sends the changes as one update; a Markdown description is
// refused as stale when Linear holds other text than it was written
// from.
func TestEdit(t *testing.T) {
	t.Parallel()
	current := issue(1, 1)
	current.Description = "Steps"
	id := tracker.IssueID(issueID(1))
	p, _ := newProvider(t,
		updateX(issueID(1), `{"assigneeId":null,"labelIds":[],"dueDate":null,"priority":0,"title":"Renamed"}`, applied(current.JSON())),
		summaryX(issueID(1), current),
		summaryX(issueID(1), current),
		updateX(issueID(1), `{"description":"New steps"}`, applied(current.JSON())),
		summaryX(issueID(1), current),
		updateX(issueID(1), `{"description":""}`, applied(current.JSON())),
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"` + issueID(1) + `"}`, Body: notFound},
	)
	ctx := context.Background()
	r := p.Edit(ctx, tracker.EditInput{Issue: id, Fields: []tracker.FieldEdit{
		{Field: tracker.FieldAssignee, Value: tracker.ClearField()},
		{Field: tracker.FieldLabels, Value: tracker.ClearField()},
		{Field: FieldDueDate, Value: tracker.ClearField()},
		{Field: tracker.FieldPriority, Value: tracker.ClearField()},
		{Field: tracker.FieldTitle, Value: tracker.TypedValues("Renamed")},
	}})
	expectResult(t, r, "1:edit:applied")
	md := func(text, from string) tracker.EditInput {
		return tracker.EditInput{Issue: id, Fields: []tracker.FieldEdit{{Field: tracker.FieldDescription, Value: tracker.MarkdownEdit(text, from)}}}
	}
	expectResult(t, p.Edit(ctx, md("New steps", "Old steps")), "1:edit:refused(stale)")
	expectResult(t, p.Edit(ctx, md("New steps", "Steps")), "1:edit:applied")
	expectResult(t, p.Edit(ctx, md("", "Steps")), "1:edit:applied")
	expectResult(t, p.Edit(ctx, md("x", "")), "1:edit:refused(tracker)")
}

// Move reads the issue with its team's states in one request and
// refuses a state the team lacks before the update, as stale.
func TestMove(t *testing.T) {
	t.Parallel()
	states := issueStatesX(issueID(1), "state-todo", lineartest.State{ID: "s-done", Name: "Done", Type: "completed", Team: web})
	p, _ := newProvider(t,
		states, updateX(issueID(1), `{"stateId":"s-done"}`, applied(issue(1, 1).JSON())),
		states,
		lineartest.Exchange{Op: "IssueStates", Vars: `{"id":"` + issueID(1) + `"}`, Body: notFound},
	)
	ctx := context.Background()
	move := func(to tracker.MoveID) tracker.Result {
		return p.Move(ctx, tracker.MoveInput{Issue: tracker.IssueID(issueID(1)), Move: to})
	}
	expectResult(t, move("s-done"), "1:move:applied")
	expectResult(t, move("s-app-done"), "1:move:refused(stale)")
	expectResult(t, move("s-done"), "1:move:refused(tracker)")
}

// commentX expects the read of comment c.
func commentX(c lineartest.Comment) lineartest.Exchange {
	return lineartest.Exchange{Op: "Comment", Vars: `{"id":"` + c.ID + `"}`, Body: `{"data":{"comment":` + c.JSON() + `}}`}
}

// Comment writes: an add; an edit and a delete that read the comment
// first, refusing one on another issue and, as stale, one that is gone
// or changed; a delete Linear refuses.
func TestCommentWrites(t *testing.T) {
	t.Parallel()
	c := lineartest.NewComment()
	c.Issue = issueID(1)
	elsewhere := c
	elsewhere.ID, elsewhere.Issue = "comment-b", issueID(2)
	gone := `{"data":null,"errors":[{"message":"Entity not found: Comment","path":["comment"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	id := tracker.IssueID(issueID(1))
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "CreateComment", Vars: `{"input":{"issueId":"` + issueID(1) + `","body":"Hi"}}`, Body: applied(c.JSON())},
		commentX(c),
		lineartest.Exchange{Op: "UpdateComment", Vars: `{"id":"comment-2","input":{"body":"Edited"}}`, Body: applied(c.JSON())},
		commentX(c),
		lineartest.Exchange{Op: "Comment", Vars: `{"id":"comment-9"}`, Body: gone},
		commentX(elsewhere),
		commentX(c),
		lineartest.Exchange{Op: "DeleteComment", Vars: `{"id":"comment-2"}`, Body: `{"data":{"result":{"success":false}}}`},
		commentX(c),
		lineartest.Exchange{Op: "DeleteComment", Vars: `{"id":"comment-2"}`, Body: `{"data":{"result":{"success":true,"value":"comment-2"}}}`},
		commentX(elsewhere),
		lineartest.Exchange{Op: "Comment", Vars: `{"id":"comment-9"}`, Body: gone},
	)
	ctx := context.Background()
	r := p.AddComment(ctx, tracker.CommentInput{Issue: id, Markdown: "Hi"})
	expectResult(t, r, "1:comment:applied")
	if !reflect.DeepEqual(r.Parts[0].Changed, []tracker.Resource{tracker.DetailOf(id)}) {
		t.Errorf("changed %+v", r.Parts[0].Changed)
	}
	expectResult(t, p.AddComment(ctx, tracker.CommentInput{Issue: id, Markdown: "Hi", Audience: tracker.AudiencePublic}), "1:comment:refused(invalid)")
	edit := func(comment tracker.CommentID, from string) tracker.Result {
		return p.EditComment(ctx, tracker.CommentEdit{Issue: id, Comment: comment, Markdown: "Edited", From: from})
	}
	expectResult(t, edit("comment-2", c.Body), "1:comment:applied")
	expectResult(t, edit("comment-2", "Older text"), "1:comment:refused(stale)")
	expectResult(t, edit("comment-9", c.Body), "1:comment:refused(stale)")
	r = edit("comment-b", c.Body)
	expectResult(t, r, "1:comment:refused(invalid)")
	if ref, _ := r.Parts[0].Outcome.Refusal(); ref.Message != "comment comment-b is not on issue "+issueID(1) {
		t.Errorf("refusal %q", ref.Message)
	}
	del := func(comment tracker.CommentID) tracker.Result {
		return p.DeleteComment(ctx, tracker.CommentDelete{Issue: id, Comment: comment})
	}
	expectResult(t, del("comment-2"), "1:comment:refused(tracker)")
	r = del("comment-2")
	expectResult(t, r, "1:comment:applied")
	if !reflect.DeepEqual(r.Parts[0].Changed, []tracker.Resource{tracker.DetailOf(id)}) {
		t.Errorf("changed %+v", r.Parts[0].Changed)
	}
	expectResult(t, del("comment-b"), "1:comment:refused(invalid)")
	expectResult(t, del("comment-9"), "1:comment:refused(stale)")
}

// relationX expects the read of relation id, from issue n to issue m.
func relationX(id string, n, m int) lineartest.Exchange {
	ref := func(n int) lineartest.IssueRef {
		return lineartest.IssueRef{ID: issueID(n), Identifier: fmt.Sprintf("WEB-%d", n)}
	}
	return lineartest.Exchange{Op: "Relation", Vars: `{"id":"` + id + `"}`,
		Body: `{"data":{"issueRelation":` + lineartest.RelationJSON(id, "blocks", ref(n), ref(m)) + `}}`}
}

// A link names both issues' detail. An unlink reads the relation first:
// one between the two issues named, either way round, is deleted and
// names both details; one between other issues is refused and one that
// is gone is refused as stale, before any delete.
func TestLinkWrites(t *testing.T) {
	t.Parallel()
	a, b := tracker.IssueID(issueID(1)), tracker.IssueID(issueID(2))
	rel := lineartest.RelationJSON("rel-1", "blocks", lineartest.IssueRef{ID: issueID(1), Identifier: "WEB-1"}, lineartest.IssueRef{ID: issueID(2), Identifier: "WEB-2"})
	gone := `{"data":null,"errors":[{"message":"Entity not found: IssueRelation","path":["issueRelation"],"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "CreateRelation", Vars: `{"input":{"issueId":"` + issueID(1) + `","relatedIssueId":"` + issueID(2) + `","type":"blocks"}}`, Body: applied(rel)},
		relationX("rel-1", 1, 2),
		lineartest.Exchange{Op: "DeleteRelation", Vars: `{"id":"rel-1"}`, Status: 500, Body: `{"errors":[{"message":"boom"}]}`},
		relationX("rel-2", 2, 1),
		lineartest.Exchange{Op: "DeleteRelation", Vars: `{"id":"rel-2"}`, Body: `{"data":{"result":{"success":true,"value":"rel-2"}}}`},
		relationX("rel-3", 3, 4),
		relationX("rel-4", 1, 3),
		lineartest.Exchange{Op: "Relation", Vars: `{"id":"rel-9"}`, Body: gone},
	)
	ctx := context.Background()
	r := p.Link(ctx, tracker.LinkInput{Type: "blocks", Outward: a, Inward: b})
	expectResult(t, r, "1:link:applied")
	if !reflect.DeepEqual(r.Parts[0].Changed, []tracker.Resource{tracker.DetailOf(a), tracker.DetailOf(b)}) {
		t.Errorf("changed %+v", r.Parts[0].Changed)
	}
	expectResult(t, p.Link(ctx, tracker.LinkInput{Type: "similar", Outward: a, Inward: b}), "1:link:refused(invalid)")
	unlink := func(link tracker.LinkID) tracker.Result {
		return p.Unlink(ctx, tracker.UnlinkInput{Issue: a, Link: link, Other: b})
	}
	expectResult(t, unlink("rel-1"), "1:link:unknown")
	r = unlink("rel-2")
	expectResult(t, r, "1:link:applied")
	if !reflect.DeepEqual(r.Parts[0].Changed, []tracker.Resource{tracker.DetailOf(a), tracker.DetailOf(b)}) {
		t.Errorf("changed %+v", r.Parts[0].Changed)
	}
	expectResult(t, unlink("rel-3"), "1:link:refused(invalid)")
	expectResult(t, unlink("rel-4"), "1:link:refused(invalid)")
	expectResult(t, unlink("rel-9"), "1:link:refused(stale)")
}

// A read a write needs that fails leaves the write not sent.
func TestWriteAfterFailedRead(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t, lineartest.Exchange{Op: "IssueTeam", Vars: `{"id":"` + issueID(1) + `"}`, Status: 500, Body: `{"errors":[{"message":"boom"}]}`})
	r := p.SetEstimate(context.Background(), tracker.EstimateInput{Board: web, Issue: tracker.IssueID(issueID(1)), Value: f64(3)})
	expectResult(t, r, "1:estimate:not sent")
	if _, ok := errors.AsType[*trackerapi.StatusError](r.Parts[0].Outcome.Err()); !ok {
		t.Errorf("the read's error is not reachable: %v", r.Parts[0].Outcome.Err())
	}
}
