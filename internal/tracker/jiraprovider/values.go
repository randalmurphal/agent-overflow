package jiraprovider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

const (
	// userChoices is the most users a user field's choices list.
	userChoices = 50
	// parentChoices is the most issues a parent field's choices list.
	parentChoices = 20
)

// createSkipped are the create screen's fields CreateFields leaves out:
// the title, which a create sets in Title, and the project and type it
// names on its own.
var createSkipped = []string{jiraSummary, "project", jiraIssueType}

// CreateFields are the create screen's fields in project for issueType,
// besides the title, the project, the type and the sprint and rank fields
// (MoveToSprint and Rank place a new issue).
func (p *Provider) CreateFields(ctx context.Context, project tracker.ProjectID, issueType tracker.IssueTypeID) ([]tracker.Field, error) {
	screen, err := p.screen(ctx, tracker.OnCreate(project, issueType))
	if err != nil {
		return nil, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return nil, err
	}
	var out []tracker.Field
	for _, f := range screen.Fields {
		if slices.Contains(createSkipped, f.ID) || f.ID == s.sprint || f.Schema.Custom == rankType {
			continue
		}
		out = append(out, metaField(f))
	}
	return out, nil
}

// screen is the edit screen of scope's issue, or the create screen of
// its project and issue type.
func (p *Provider) screen(ctx context.Context, scope tracker.FieldScope) (jira.Screen, error) {
	if issue, ok := scope.Issue(); ok {
		if err := checkIssue(issue); err != nil {
			return jira.Screen{}, err
		}
		screen, err := p.jira.EditMeta(ctx, string(issue))
		if err != nil {
			return jira.Screen{}, fmt.Errorf("jiraprovider: edit screen of %s: %w", issue, err)
		}
		return screen, nil
	}
	project, issueType, ok := scope.Create()
	if !ok {
		return jira.Screen{}, invalidf("a field scope names neither an issue nor a project")
	}
	if err := checkProject(project); err != nil {
		return jira.Screen{}, err
	}
	if _, err := numericID("issue type", string(issueType)); err != nil {
		return jira.Screen{}, err
	}
	screen, err := p.jira.CreateMeta(ctx, string(project), string(issueType))
	if err != nil {
		return jira.Screen{}, fmt.Errorf("jiraprovider: create screen of %s type %s: %w", project, issueType, err)
	}
	return screen, nil
}

// AssignableUsers are the users Jira says can be assigned in scope whose
// name or email starts with text. Jira takes no empty query, so empty
// text returns none.
func (p *Provider) AssignableUsers(ctx context.Context, scope tracker.FieldScope, text string) ([]tracker.User, error) {
	q := jira.AssignableQuery{Query: strings.TrimSpace(text)}
	if issue, ok := scope.Issue(); ok {
		if err := checkIssue(issue); err != nil {
			return nil, err
		}
		q.Issue = string(issue)
	} else if project, _, ok := scope.Create(); ok {
		if err := checkProject(project); err != nil {
			return nil, err
		}
		q.Project = string(project)
	} else {
		return nil, invalidf("a field scope names neither an issue nor a project")
	}
	if q.Query == "" {
		return nil, nil
	}
	found, err := p.jira.AssignableUsers(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: assignable users: %w", err)
	}
	out := make([]tracker.User, len(found))
	for i, u := range found {
		out[i] = userOf(u)
	}
	return out, nil
}

// Choices are the values field takes in scope whose name starts with
// text, case ignored: the screen's allowed values (the site's priorities
// for a priority field that lists none), users for a user field, found by
// Jira's search, and none for a field whose values are free.
func (p *Provider) Choices(ctx context.Context, scope tracker.FieldScope, field tracker.FieldID, text string) ([]tracker.Choice, error) {
	screen, err := p.screen(ctx, scope)
	if err != nil {
		return nil, err
	}
	f, ok := screenField(screen, jiraField(field))
	if !ok {
		return nil, invalidf("field %s is not on the screen", field)
	}
	text = strings.TrimSpace(text)
	if f.Schema.System == "parent" {
		return p.parentChoices(ctx, scope, text)
	}
	switch kind, _ := kindOf(f.Schema); kind {
	case tracker.KindUser:
		return p.userChoices(ctx, scope, f, text)
	case tracker.KindOption:
		allowed := f.Allowed
		if allowed == nil && f.Schema.Type == "priority" {
			ps, err := p.jira.Priorities(ctx)
			if err != nil {
				return nil, fmt.Errorf("jiraprovider: priorities: %w", err)
			}
			for _, pr := range ps {
				allowed = append(allowed, jira.AllowedValue{ID: pr.ID, Name: pr.Name})
			}
		}
		var out []tracker.Choice
		for _, a := range allowed {
			if !a.Disabled && strings.HasPrefix(strings.ToLower(a.Name), strings.ToLower(text)) {
				out = append(out, tracker.Choice{ID: a.ID, Name: a.Name})
			}
		}
		return out, nil
	}
	return nil, nil
}

// parentChoices are the issues text finds, most recently updated first,
// as candidates for a parent: by Jira's text search and, for text shaped
// like a key, by that key; never the issue being edited. Each is named by
// its key and title. Jira's edit and create metadata list no allowed
// values for the parent, so which issues may be one (by hierarchy level
// and project) is not filtered here: a parent Jira does not take is
// refused by its answer to the edit. Empty text offers none.
func (p *Provider) parentChoices(ctx context.Context, scope tracker.FieldScope, text string) ([]tracker.Choice, error) {
	if text == "" {
		return nil, nil
	}
	jql := "text ~ " + jira.QuoteJQLText(text)
	if keyShape.MatchString(text) {
		jql = "(" + jql + " OR key in " + jira.QuoteJQLList(text) + ")"
	}
	if issue, ok := scope.Issue(); ok {
		jql += " AND id != " + string(issue) // checked numeric by screen
	}
	page, err := p.jira.Search(ctx, jira.SearchQuery{JQL: jql + " ORDER BY updated DESC", Fields: []string{jiraSummary}, MaxResults: parentChoices})
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: parents for %q: %w", text, err)
	}
	out := make([]tracker.Choice, len(page.Issues))
	for i, is := range page.Issues {
		out[i] = tracker.Choice{ID: is.ID, Name: is.Key + " " + is.Summary}
	}
	return out, nil
}

// userChoices are the users a user field offers for text: the assignable
// users for the assignee, any user otherwise. Empty text offers none.
func (p *Provider) userChoices(ctx context.Context, scope tracker.FieldScope, f jira.FieldMeta, text string) ([]tracker.Choice, error) {
	var users []tracker.User
	if f.ID == "assignee" {
		var err error
		if users, err = p.AssignableUsers(ctx, scope, text); err != nil {
			return nil, err
		}
	} else if text != "" {
		found, err := p.jira.SearchUsers(ctx, text, userChoices)
		if err != nil {
			return nil, fmt.Errorf("jiraprovider: users for %s: %w", f.Name, err)
		}
		for _, u := range found {
			users = append(users, userOf(u))
		}
	}
	out := make([]tracker.Choice, len(users))
	for i, u := range users {
		out[i] = tracker.Choice{ID: string(u.ID), Name: u.Name}
	}
	return out, nil
}
