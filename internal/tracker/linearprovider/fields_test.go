package linearprovider

import (
	"context"
	"errors"
	"strconv"
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
	var nodes []string
	for _, u := range members {
		nodes = append(nodes, u.JSON())
	}
	p, _ := newProvider(t,
		lineartest.Exchange{Op: "IssueMembers", Vars: `{"id":"` + issueID(1) + `","first":100}`, Body: lineartest.Data(`"issue":{"id":"` + issueID(1) +
			`","state":{"id":"state-todo"},"team":{"id":"` + web + `","page":` + lineartest.ConnectionJSON(nodes, false, "") + `}}`)},
		membersX(web, members...),
		labelsX(web, lineartest.Label{ID: "l-1", Name: "Bug"}, lineartest.Label{ID: "l-g", Name: "Bugs area", IsGroup: true}, lineartest.Label{ID: "l-2", Name: "Feature"}),
		lineartest.Exchange{Op: "Projects", Vars: `{"first":100,"filter":{"accessibleTeams":{"some":{"id":{"eq":"` + web + `"}}}}}`,
			Body: page(lineartest.Project{ID: "project-1", Name: "Launch"}.JSON())},
	)
	ctx := context.Background()
	onIssue, onCreate := tracker.OnIssue(tracker.IssueID(issueID(1))), tracker.OnCreate(web, "")
	// The lists are read whole, so none says it leaves matches out.
	names := func(cs tracker.ChoiceList) string {
		var out []string
		for _, c := range cs.Choices {
			out = append(out, c.ID+"="+c.Name)
		}
		if cs.More {
			out = append(out, "more")
		}
		return strings.Join(out, " ")
	}
	if cs, err := p.Choices(ctx, onCreate, tracker.FieldPriority, "h"); err != nil || names(cs) != "2=High" {
		t.Errorf("priorities %v %v", cs, err)
	}
	users, err := p.AssignableUsers(ctx, onIssue, "USER")
	if err != nil || len(users.Users) != 2 || users.Users[0].ID != "user-1" || users.Users[1].ID != "user-3" || users.More {
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
		if cs, err := p.Choices(ctx, onCreate, f, ""); err != nil || cs.Choices != nil || cs.More {
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

// A pick list returns at most pickLimit rows: a longer match list is cut
// to that many with More set, and one of exactly pickLimit is whole.
func TestPickListLimit(t *testing.T) {
	t.Parallel()
	users := func(n int) []lineartest.User {
		var out []lineartest.User
		for i := range n {
			out = append(out, lineartest.User{ID: "user-" + strconv.Itoa(i), Name: "User " + strconv.Itoa(i), Active: true})
		}
		return out
	}
	labels := func(n int) []lineartest.Label {
		var out []lineartest.Label
		for i := range n {
			out = append(out, lineartest.Label{ID: "label-" + strconv.Itoa(i), Name: "Label " + strconv.Itoa(i)})
		}
		return out
	}
	p, _ := newProvider(t,
		membersX(web, users(pickLimit)...),
		membersX(web, users(pickLimit+1)...),
		membersX(web, users(pickLimit+1)...),
		labelsX(web, labels(pickLimit)...),
		labelsX(web, labels(pickLimit+1)...),
	)
	ctx, onCreate := context.Background(), tracker.OnCreate(web, "")
	if got, err := p.AssignableUsers(ctx, onCreate, ""); err != nil || len(got.Users) != pickLimit || got.More {
		t.Errorf("exactly the limit: %d users, more %t, %v", len(got.Users), got.More, err)
	}
	got, err := p.AssignableUsers(ctx, onCreate, "")
	if err != nil || len(got.Users) != pickLimit || !got.More || got.Users[pickLimit-1].ID != tracker.UserID("user-"+strconv.Itoa(pickLimit-1)) {
		t.Errorf("one over the limit: %d users, more %t, %v", len(got.Users), got.More, err)
	}
	if cs, err := p.Choices(ctx, onCreate, tracker.FieldAssignee, ""); err != nil || len(cs.Choices) != pickLimit || !cs.More {
		t.Errorf("assignee choices over the limit: %d, more %t, %v", len(cs.Choices), cs.More, err)
	}
	if cs, err := p.Choices(ctx, onCreate, tracker.FieldLabels, ""); err != nil || len(cs.Choices) != pickLimit || cs.More {
		t.Errorf("labels at the limit: %d, more %t, %v", len(cs.Choices), cs.More, err)
	}
	if cs, err := p.Choices(ctx, onCreate, tracker.FieldLabels, ""); err != nil || len(cs.Choices) != pickLimit || !cs.More {
		t.Errorf("labels over the limit: %d, more %t, %v", len(cs.Choices), cs.More, err)
	}
}
