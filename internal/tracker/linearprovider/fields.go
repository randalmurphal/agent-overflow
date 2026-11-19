package linearprovider

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// The fields Linear has beyond the card's and the description, by the
// names its API gives them. Linear has no custom fields.
const (
	FieldDueDate tracker.FieldID = "dueDate"
	FieldProject tracker.FieldID = "project"
)

// fieldSpec is one field an edit can set.
type fieldSpec struct {
	id   tracker.FieldID
	name string
	kind tracker.Kind
	list bool
}

// editable is every field Edit and Create take; Create takes all but the
// title through its fields. A field not listed is refused as
// unsupported.
var editable = []fieldSpec{
	{tracker.FieldTitle, "Title", tracker.KindText, false},
	{tracker.FieldDescription, "Description", tracker.KindRichText, false},
	{tracker.FieldAssignee, "Assignee", tracker.KindUser, false},
	{tracker.FieldPriority, "Priority", tracker.KindOption, false},
	{tracker.FieldLabels, "Labels", tracker.KindOption, true},
	{tracker.FieldParent, "Parent", tracker.KindOption, false},
	{FieldDueDate, "Due date", tracker.KindDate, false},
	{FieldProject, "Project", tracker.KindOption, false},
}

func spec(id tracker.FieldID) (fieldSpec, bool) {
	i := slices.IndexFunc(editable, func(f fieldSpec) bool { return f.id == id })
	if i < 0 {
		return fieldSpec{}, false
	}
	return editable[i], true
}

func (f fieldSpec) field(v tracker.Value) tracker.Field {
	return tracker.Field{ID: f.id, Name: f.name, Kind: f.kind, List: f.list, Editable: true, Value: v}
}

// detailFields are the issue's fields that are not the card's: its due
// date and its project.
func detailFields(is linear.Issue, n names) []tracker.Field {
	due, _ := spec(FieldDueDate)
	project, _ := spec(FieldProject)
	var dueValue, projectValue tracker.Value
	if d, err := tracker.ParseDate(string(is.DueDate)); err == nil {
		dueValue = tracker.DateValue(d)
	}
	if is.ProjectID != "" {
		projectValue = tracker.OptionValue(tracker.Choice{ID: is.ProjectID, Name: n.name(n.projects, is.ProjectID)})
	}
	return []tracker.Field{due.field(dueValue), project.field(projectValue)}
}

// CreateFields are the fields a create takes besides the title, none
// required. Linear has no issue types; one named is refused.
func (p *Provider) CreateFields(_ context.Context, project tracker.ProjectID, issueType tracker.IssueTypeID) ([]tracker.Field, error) {
	if project == "" || issueType != "" {
		return nil, fmt.Errorf("linearprovider: create fields: %w: a project and no issue type", tracker.ErrInvalidInput)
	}
	var out []tracker.Field
	for _, f := range editable {
		if f.id != tracker.FieldTitle {
			out = append(out, f.field(tracker.Value{}))
		}
	}
	return out, nil
}

// scopeTeam is the team of a field scope: the issue's, read, or the
// create's project.
func (p *Provider) scopeTeam(ctx context.Context, scope tracker.FieldScope) (string, error) {
	if issue, ok := scope.Issue(); ok {
		is, err := p.summary(ctx, issue)
		if err != nil {
			return "", err
		}
		return is.TeamID, nil
	}
	if project, issueType, ok := scope.Create(); ok && issueType == "" {
		return string(project), nil
	}
	return "", fmt.Errorf("linearprovider: %w: the scope names no issue and no project without an issue type", tracker.ErrInvalidInput)
}

// AssignableUsers are the active members of the scope's team, apps left
// out, whose name, display name or email starts with text, case
// ignored; every one for empty text.
func (p *Provider) AssignableUsers(ctx context.Context, scope tracker.FieldScope, text string) ([]tracker.User, error) {
	team, err := p.scopeTeam(ctx, scope)
	if err != nil {
		return nil, err
	}
	members, err := p.assignable(ctx, team)
	if err != nil {
		return nil, err
	}
	var out []tracker.User
	for _, u := range members {
		if hasPrefixFold(u.Name, text) || hasPrefixFold(u.DisplayName, text) || hasPrefixFold(u.Email, text) {
			out = append(out, user(u))
		}
	}
	return out, nil
}

// assignable are a team's active members that are people.
func (p *Provider) assignable(ctx context.Context, team string) ([]linear.User, error) {
	members, err := p.c.TeamMembers(ctx, team)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: members of team %s: %w", team, err)
	}
	return slices.DeleteFunc(members, func(u linear.User) bool { return !u.Active || u.App }), nil
}

// Choices are the values field takes in scope whose name starts with
// text: the fixed priorities, the team's labels that can be applied
// (not groups), the team's projects, or the assignable users. Title,
// description, parent (typed as an identifier) and due date offer none;
// any other field is unsupported.
func (p *Provider) Choices(ctx context.Context, scope tracker.FieldScope, field tracker.FieldID, text string) ([]tracker.Choice, error) {
	var out []tracker.Choice
	keep := func(id, name string) {
		if hasPrefixFold(name, text) {
			out = append(out, tracker.Choice{ID: id, Name: name})
		}
	}
	switch field {
	case tracker.FieldPriority:
		for v, name := range priorityNames {
			keep(strconv.Itoa(v), name)
		}
		return out, nil
	case tracker.FieldTitle, tracker.FieldDescription, tracker.FieldParent, FieldDueDate:
		return nil, nil
	case tracker.FieldAssignee, tracker.FieldLabels, FieldProject:
	default:
		return nil, fmt.Errorf("linearprovider: choices of %s: %w", field, tracker.ErrUnsupported)
	}
	team, err := p.scopeTeam(ctx, scope)
	if err != nil {
		return nil, err
	}
	switch field {
	case tracker.FieldAssignee:
		users, err := p.AssignableUsers(ctx, tracker.OnCreate(tracker.ProjectID(team), ""), text)
		for _, u := range users {
			out = append(out, tracker.Choice{ID: string(u.ID), Name: u.Name})
		}
		return out, err
	case tracker.FieldLabels:
		labels, err := p.applicableLabels(ctx, team)
		for _, l := range labels {
			keep(l.ID, l.Name)
		}
		return out, err
	default:
		projects, err := p.teamProjects(ctx, team)
		for _, pr := range projects {
			keep(pr.ID, pr.Name)
		}
		return out, err
	}
}

// applicableLabels are the labels team's issues can carry: the team's and
// the workspace's, groups left out.
func (p *Provider) applicableLabels(ctx context.Context, team string) ([]linear.Label, error) {
	labels, err := p.c.Labels(ctx, team)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: labels of team %s: %w", team, err)
	}
	return slices.DeleteFunc(labels, func(l linear.Label) bool { return l.IsGroup }), nil
}

// teamProjects are the projects team's issues can join.
func (p *Provider) teamProjects(ctx context.Context, team string) ([]linear.Project, error) {
	projects, err := p.c.Projects(ctx, team, false)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: projects of team %s: %w", team, err)
	}
	return projects, nil
}
