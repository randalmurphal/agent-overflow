package linearprovider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear/lineartest"
)

// The create form is the fixed list less the title; Linear has no issue
// types.
func TestCreateFields(t *testing.T) {
	t.Parallel()
	p, _ := newProvider(t)
	fields, err := p.CreateFields(context.Background(), web, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fields {
		got = append(got, string(f.ID)+":"+f.Kind.String())
		if !f.Editable || f.Required || !f.Value.Empty() {
			t.Errorf("field %+v", f)
		}
	}
	if strings.Join(got, " ") != "description:rich_text assignee:user priority:option labels:option parent:option dueDate:date project:option" {
		t.Errorf("fields %v", got)
	}
	if _, err := p.CreateFields(context.Background(), web, "type-1"); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("an issue type: %v", err)
	}
}

// Choices filter by prefix, case ignored: priorities without a request,
// assignees from the issue's team (inactive members and apps left out),
// labels without groups, the team's projects; other fields send nothing.
func TestChoices(t *testing.T) {
	t.Parallel()
	members := []lineartest.User{
		{ID: "user-1", Name: "User 1", DisplayName: "one", Email: "user1@example.com", Active: true},
		{ID: "user-2", Name: "User 2", Active: false},
		{ID: "app-1", Name: "Use bot", Active: true, App: true},
		{ID: "user-3", Name: "Other", DisplayName: "user3", Active: true},
	}
	p, _ := newProvider(t,
		summaryX(issueID(1), issue(1, 1)),
		membersX(web, members...),
		membersX(web, members...),
		labelsX(web, lineartest.Label{ID: "l-1", Name: "Bug"}, lineartest.Label{ID: "l-g", Name: "Bugs area", IsGroup: true}, lineartest.Label{ID: "l-2", Name: "Feature"}),
		lineartest.Exchange{Op: "Projects", Vars: `{"first":100,"filter":{"accessibleTeams":{"some":{"id":{"eq":"` + web + `"}}}}}`,
			Body: page(lineartest.Project{ID: "project-1", Name: "Launch"}.JSON())},
	)
	ctx := context.Background()
	onIssue, onCreate := tracker.OnIssue(tracker.IssueID(issueID(1))), tracker.OnCreate(web, "")
	names := func(cs []tracker.Choice) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID+"="+c.Name)
		}
		return strings.Join(out, " ")
	}
	if cs, err := p.Choices(ctx, onCreate, tracker.FieldPriority, "h"); err != nil || names(cs) != "2=High" {
		t.Errorf("priorities %v %v", cs, err)
	}
	users, err := p.AssignableUsers(ctx, onIssue, "USER")
	if err != nil || len(users) != 2 || users[0].ID != "user-1" || users[1].ID != "user-3" {
		t.Errorf("assignable %+v %v", users, err)
	}
	if cs, err := p.Choices(ctx, onCreate, tracker.FieldAssignee, "ONE"); err != nil || names(cs) != "user-1=User 1" {
		t.Errorf("assignee choices %v %v", cs, err)
	}
	if cs, err := p.Choices(ctx, onCreate, tracker.FieldLabels, "bug"); err != nil || names(cs) != "l-1=Bug" {
		t.Errorf("labels %v %v", cs, err)
	}
	if cs, err := p.Choices(ctx, onCreate, FieldProject, ""); err != nil || names(cs) != "project-1=Launch" {
		t.Errorf("projects %v %v", cs, err)
	}
	for _, f := range []tracker.FieldID{tracker.FieldTitle, tracker.FieldParent, FieldDueDate} {
		if cs, err := p.Choices(ctx, onCreate, f, ""); err != nil || cs != nil {
			t.Errorf("%s choices %v %v", f, cs, err)
		}
	}
	if _, err := p.Choices(ctx, onCreate, "customfield_10001", ""); !errors.Is(err, tracker.ErrUnsupported) {
		t.Errorf("unknown field: %v", err)
	}
	if _, err := p.AssignableUsers(ctx, tracker.OnCreate(web, "type-1"), ""); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("an issue type scope: %v", err)
	}
}
