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
	// pickLimit is the most rows a pick list returns: Choices and
	// AssignableUsers return at most this many, with More set when they
	// leave a match out.
	pickLimit = 50
	// assignableWindow is the window of users Jira's assignable search
	// checks in one call: the largest it takes, since it examines no user
	// past the thousandth.
	assignableWindow = 1000
)

// createSkipped are the create screen's fields CreateFields leaves out:
// the title, which a create sets in Title, and the project and type it
// names on its own.
var createSkipped = []string{jiraSummary, "project", jiraIssueType}

// CreateFields are the create screen's fields in project for issueType,
// besides the title, the project, the type and the sprint and rank fields
// (MoveToSprint and Rank place a new issue).
func (p *Provider) CreateFields(ctx context.Context, project tracker.ProjectID, issueType tracker.IssueTypeID) ([]tracker.CreateField, error) {
	screen, err := p.screen(ctx, tracker.OnCreate(project, issueType))
	if err != nil {
		return nil, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return nil, err
	}
	var out []tracker.CreateField
	for _, f := range screen.Fields {
		if slices.Contains(createSkipped, f.ID) || f.ID == s.sprint || f.Schema.Custom == rankType {
			continue
		}
		// A field Jira fills when none is given is offered, not asked.
		fd := tracker.CreateField{Field: metaField(f), Defaulted: f.Required && f.HasDefault}
		fd.Required = f.Demanded()
		if fd.Kind == tracker.KindOption {
			for _, a := range f.Allowed {
				if !a.Disabled {
					fd.Allowed = append(fd.Allowed, tracker.Choice{ID: a.ID, Name: a.Name})
				}
			}
		}
		out = append(out, fd)
	}
	return out, nil
}

// screen is the screen of scope's move on its issue, the edit screen of
// its issue, or the create screen of its project and issue type. A move's
// screen is read with the issue's transitions, one request as the edit
// screen is, and holds the fields and allowed values that move takes.
func (p *Provider) screen(ctx context.Context, scope tracker.FieldScope) (jira.Screen, error) {
	if issue, ok := scope.Issue(); ok {
		if err := checkIssue(issue); err != nil {
			return jira.Screen{}, err
		}
		if move, ok := scope.Move(); ok {
			return p.moveScreen(ctx, issue, move)
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

// moveScreen is the screen of move, a transition issue can make now.
func (p *Provider) moveScreen(ctx context.Context, issue tracker.IssueID, move tracker.MoveID) (jira.Screen, error) {
	ts, err := p.jira.Transitions(ctx, string(issue))
	if err != nil {
		return jira.Screen{}, fmt.Errorf("jiraprovider: moves of %s: %w", issue, err)
	}
	i := slices.IndexFunc(ts, func(t jira.Transition) bool { return t.ID == string(move) && t.Available })
	if i < 0 {
		return jira.Screen{}, invalidf("move %s is not one %s can make now", move, issue)
	}
	return ts[i].Fields, nil
}

// AssignableUsers are the users Jira says can be assigned in scope whose
// name or email starts with text, at most pickLimit, from one call that
// asks for the whole window Jira allows. Jira takes no empty query, so
// empty text returns none.
//
// More is always set. Jira's assignable search takes a window of users
// and returns only the assignable ones in it, and its answer does not say
// whether that window was full: an empty answer can hide an assignable
// user just past it. Nothing in the answer shows the list is whole, so a
// user missing from it is never proof that none matches.
func (p *Provider) AssignableUsers(ctx context.Context, scope tracker.FieldScope, text string) (tracker.UserList, error) {
	q := jira.AssignableQuery{Query: strings.TrimSpace(text), MaxResults: assignableWindow}
	if issue, ok := scope.Issue(); ok {
		if err := checkIssue(issue); err != nil {
			return tracker.UserList{}, err
		}
		q.Issue = string(issue)
	} else if project, _, ok := scope.Create(); ok {
		if err := checkProject(project); err != nil {
			return tracker.UserList{}, err
		}
		q.Project = string(project)
	} else {
		return tracker.UserList{}, invalidf("a field scope names neither an issue nor a project")
	}
	if q.Query == "" {
		return tracker.UserList{More: true}, nil
	}
	found, err := p.jira.AssignableUsers(ctx, q)
	if err != nil {
		return tracker.UserList{}, fmt.Errorf("jiraprovider: assignable users: %w", err)
	}
	out := tracker.UserList{More: true}
	for _, u := range found[:min(len(found), pickLimit)] {
		out.Users = append(out.Users, userOf(u))
	}
	return out, nil
}

// Choices are the values field takes in scope whose name starts with
// text, case ignored, at most pickLimit: the screen's allowed values (the
// site's priorities for a priority field that lists none), users for a
// user field, found by Jira's search, issues for the parent, and none for
// a field whose values are free.
func (p *Provider) Choices(ctx context.Context, scope tracker.FieldScope, field tracker.FieldID, text string) (tracker.ChoiceList, error) {
	screen, err := p.screen(ctx, scope)
	if err != nil {
		return tracker.ChoiceList{}, err
	}
	f, ok := screenField(screen, jiraField(field))
	if !ok {
		return tracker.ChoiceList{}, invalidf("field %s is not on the screen", field)
	}
	text = strings.TrimSpace(text)
	if f.Schema.System == "parent" {
		return p.parentChoices(ctx, scope, text)
	}
	switch kind, _ := kindOf(f.Schema); kind {
	case tracker.KindUser:
		return p.userChoices(ctx, scope, f, text)
	case tracker.KindOption:
		return p.allowedChoices(ctx, f, text)
	case tracker.KindText, tracker.KindRichText, tracker.KindNumber, tracker.KindDate, tracker.KindDateTime, tracker.KindReadOnly:
		// Free values, or none to set: no choices.
	}
	return tracker.ChoiceList{}, nil
}

// allowedChoices are f's allowed values starting with text: the screen's
// list, which the screen's metadata carries whole in one answer, or the
// site's priorities, read until Jira marks the last page. Either is the
// whole set, so More is set only when more than pickLimit match.
func (p *Provider) allowedChoices(ctx context.Context, f jira.FieldMeta, text string) (tracker.ChoiceList, error) {
	allowed := f.Allowed
	if allowed == nil && f.Schema.Type == "priority" {
		ps, err := p.jira.Priorities(ctx)
		if err != nil {
			return tracker.ChoiceList{}, fmt.Errorf("jiraprovider: priorities: %w", err)
		}
		for _, pr := range ps {
			allowed = append(allowed, jira.AllowedValue{ID: pr.ID, Name: pr.Name})
		}
	}
	var out tracker.ChoiceList
	for _, a := range allowed {
		if a.Disabled || !strings.HasPrefix(strings.ToLower(a.Name), strings.ToLower(text)) {
			continue
		}
		if len(out.Choices) == pickLimit {
			out.More = true
			break
		}
		out.Choices = append(out.Choices, tracker.Choice{ID: a.ID, Name: a.Name})
	}
	return out, nil
}

// parentChoices are the issues text finds, most recently updated first,
// as candidates for a parent: by Jira's text search and, for text shaped
// like a key, by that key; never the issue being edited. Each is named by
// its key and title. Jira's edit and create metadata list no allowed
// values for the parent, so which issues may be one (by hierarchy level
// and project) is not filtered here: a parent Jira does not take is
// refused by its answer to the edit. Empty text offers none, with More
// set. More false rests on an answer with no next page token, fewer
// issues than pickLimit and no warning that the search may be incomplete;
// any of the three sets More.
func (p *Provider) parentChoices(ctx context.Context, scope tracker.FieldScope, text string) (tracker.ChoiceList, error) {
	if text == "" {
		return tracker.ChoiceList{More: true}, nil
	}
	jql := "text ~ " + jira.QuoteJQLText(text)
	if jira.IsIssueKey(text) {
		jql = "(" + jql + " OR key in " + jira.QuoteJQLList(text) + ")"
	}
	if issue, ok := scope.Issue(); ok {
		jql += " AND id != " + string(issue) // checked numeric by screen
	}
	page, err := p.jira.Search(ctx, jira.SearchQuery{JQL: jql + " ORDER BY updated DESC", Fields: []string{jiraSummary}, MaxResults: pickLimit})
	if err != nil {
		return tracker.ChoiceList{}, fmt.Errorf("jiraprovider: parents for %q: %w", text, err)
	}
	out := tracker.ChoiceList{More: page.NextPageToken != "" || len(page.Issues) >= pickLimit || len(page.Warnings) > 0}
	for _, is := range page.Issues[:min(len(page.Issues), pickLimit)] {
		out.Choices = append(out.Choices, tracker.Choice{ID: is.ID, Name: is.Key + " " + is.Summary})
	}
	return out, nil
}

// userChoices are the users a user field offers for text: the assignable
// users for the assignee, any user otherwise. Empty text offers none,
// with More set.
//
// Jira's user search matches text first and then pages the matches, so
// asking for one more than pickLimit shows whether any is left out. A
// credential without the Browse users and groups permission is answered
// with no users, which is indistinguishable from no match, so an empty
// answer sets More too. More false rests on an answer that named at least
// one user (the credential may browse users) and at most pickLimit: no
// other active user whose name or email starts with text exists.
func (p *Provider) userChoices(ctx context.Context, scope tracker.FieldScope, f jira.FieldMeta, text string) (tracker.ChoiceList, error) {
	var users tracker.UserList
	switch {
	case f.ID == "assignee":
		var err error
		if users, err = p.AssignableUsers(ctx, scope, text); err != nil {
			return tracker.ChoiceList{}, err
		}
	case text == "":
		users.More = true
	default:
		found, err := p.jira.SearchUsers(ctx, text, pickLimit+1)
		if err != nil {
			return tracker.ChoiceList{}, fmt.Errorf("jiraprovider: users for %s: %w", f.Name, err)
		}
		users.More = len(found) == 0 || len(found) > pickLimit
		for _, u := range found[:min(len(found), pickLimit)] {
			users.Users = append(users.Users, userOf(u))
		}
	}
	out := tracker.ChoiceList{More: users.More}
	for _, u := range users.Users {
		out.Choices = append(out.Choices, tracker.Choice{ID: string(u.ID), Name: u.Name})
	}
	return out, nil
}
