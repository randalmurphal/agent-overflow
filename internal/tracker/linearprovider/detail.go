package linearprovider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// Detail is one issue as its ticket shows it: the issue with its first 50
// comments, every relation each way, its team's states as moves, its due
// date and project as fields, and its whole history with every ID named.
// Linear's attachments are links, not files, so there are none. The
// project's name, its labels' names, its team's states and its newest
// 250 history entries come with the issue in one request; the labels it
// does not name and the users, cycles, projects, teams and parent issues
// its history names are read by exactly their IDs, each kind only when
// something names one.
func (p *Provider) Detail(ctx context.Context, issue tracker.IssueID) (tracker.Detail, error) {
	if issue == "" {
		return tracker.Detail{}, fmt.Errorf("linearprovider: detail: %w: no issue named", tracker.ErrInvalidInput)
	}
	d, err := p.c.Issue(ctx, string(issue))
	if err != nil {
		return tracker.Detail{}, fmt.Errorf("linearprovider: detail of %s: %w", issue, err)
	}
	states := d.States
	if d.MoreStates {
		if states, err = p.c.WorkflowStates(ctx, d.TeamID); err != nil {
			return tracker.Detail{}, fmt.Errorf("linearprovider: workflow states of team %s: %w", d.TeamID, err)
		}
	}
	if states, err = boardStates(states); err != nil {
		return tracker.Detail{}, err
	}
	entries, err := p.restOfHistory(ctx, d.ID, d.History)
	if err != nil {
		return tracker.Detail{}, err
	}
	n, err := p.readNames(ctx, d, states, entries)
	if err != nil {
		return tracker.Detail{}, err
	}
	out := tracker.Detail{
		Card: card(d.Issue, n.labels), URL: d.URL, Description: d.Description,
		Fields:      detailFields(d.Issue, n),
		Comments:    commentPage(d.Comments),
		HistoryFrom: d.CreatedAt.Add(creationFold),
		Moves:       moves(states, d.StateID),
	}
	for _, r := range d.Relations {
		out.Links = append(out.Links, link(d.ID, r))
	}
	for i := len(entries) - 1; i >= 0; i-- {
		out.History = append(out.History, n.events(entries[i])...)
	}
	for _, s := range states {
		out.Statuses = append(out.Statuses, status(s))
	}
	return out, nil
}

// restOfHistory is every history entry of an issue, newest first: first,
// the page its detail held, and every page after it. History pages in
// Linear's order of creation, which no change moves an entry in.
func (p *Provider) restOfHistory(ctx context.Context, id string, first linear.Page[linear.HistoryEntry]) ([]linear.HistoryEntry, error) {
	out := first.Items
	page := first
	for page.HasNextPage {
		var err error
		if page, err = p.c.IssueHistory(ctx, id, page.EndCursor); err != nil {
			return nil, fmt.Errorf("linearprovider: history of %s: %w", id, err)
		}
		out = append(out, page.Items...)
	}
	return out, nil
}

func link(issueID string, r linear.Relation) tracker.Link {
	outward := r.Issue.ID == issueID
	other := r.Issue
	if outward {
		other = r.Related
	}
	return tracker.Link{
		ID: tracker.LinkID(r.ID), Type: linkType(r.Type), Outward: outward,
		Issue: tracker.IssueRef{ID: tracker.IssueID(other.ID), Key: other.Identifier, Title: other.Title, Status: tracker.StatusID(other.StateID)},
	}
}

// moves are the team's states other than the issue's own, in board order.
func moves(states []linear.WorkflowState, current string) []tracker.Move {
	var out []tracker.Move
	for _, s := range states {
		if s.ID != current {
			out = append(out, tracker.Move{ID: tracker.MoveID(s.ID), Name: s.Name, To: tracker.StatusID(s.ID)})
		}
	}
	return out
}

// Moves are the issue's team's states other than its own: Linear lets an
// issue move to any of them. The issue and its team's states are one
// read.
func (p *Provider) Moves(ctx context.Context, issue tracker.IssueID) ([]tracker.Move, error) {
	scope, states, err := p.issueStates(ctx, issue)
	if err != nil {
		return nil, err
	}
	return moves(states, scope.StateID), nil
}

// issueStates is an issue's scope with its team's states in board order.
func (p *Provider) issueStates(ctx context.Context, issue tracker.IssueID) (linear.IssueScope, []linear.WorkflowState, error) {
	if issue == "" {
		return linear.IssueScope{}, nil, fmt.Errorf("linearprovider: %w: no issue named", tracker.ErrInvalidInput)
	}
	scope, states, err := p.c.IssueStates(ctx, string(issue))
	if err != nil {
		return linear.IssueScope{}, nil, fmt.Errorf("linearprovider: read issue %s with its team's states: %w", issue, err)
	}
	states, err = boardStates(states)
	return scope, states, err
}

// summary is one issue with its description, without its detail.
func (p *Provider) summary(ctx context.Context, issue tracker.IssueID) (*linear.DescribedIssue, error) {
	if issue == "" {
		return nil, fmt.Errorf("linearprovider: %w: no issue named", tracker.ErrInvalidInput)
	}
	is, err := p.c.IssueSummary(ctx, string(issue))
	if err != nil {
		return nil, fmt.Errorf("linearprovider: read issue %s: %w", issue, err)
	}
	return is, nil
}

func commentPage(p linear.Page[linear.Comment]) tracker.CommentPage {
	out := tracker.CommentPage{}
	for _, c := range p.Items {
		out.Comments = append(out.Comments, tracker.Comment{
			ID: tracker.CommentID(c.ID), Author: tracker.UserID(c.UserID), Body: c.Body, Created: c.CreatedAt, Updated: c.UpdatedAt,
		})
	}
	if p.HasNextPage {
		out.Next = tracker.PageToken(p.EndCursor)
	}
	return out
}

// Comments is the page of an issue's comments after page, newest first,
// 100 to a page; replies are listed with the rest.
func (p *Provider) Comments(ctx context.Context, issue tracker.IssueID, page tracker.PageToken) (tracker.CommentPage, error) {
	if issue == "" {
		return tracker.CommentPage{}, fmt.Errorf("linearprovider: comments: %w: no issue named", tracker.ErrInvalidInput)
	}
	got, err := p.c.IssueComments(ctx, string(issue), string(page))
	if err != nil {
		return tracker.CommentPage{}, fmt.Errorf("linearprovider: comments of %s: %w", issue, err)
	}
	return commentPage(got), nil
}

// names maps the IDs an issue's detail shows to names. An ID it has no
// name for is shown as itself.
type names struct {
	states, labels, users, cycles, projects, teams, issues map[string]string
}

func (n names) name(of map[string]string, id string) string {
	if id == "" {
		return ""
	}
	if s, ok := of[id]; ok {
		return s
	}
	return id
}

// readNames reads the names of what the issue and its entries name, by
// exactly those IDs: one request for each kind something names.
func (p *Provider) readNames(ctx context.Context, d *linear.IssueDetail, states []linear.WorkflowState, entries []linear.HistoryEntry) (names, error) {
	n := names{states: map[string]string{}, labels: map[string]string{}}
	for _, l := range d.Labels {
		n.labels[l.ID] = l.Name
	}
	for _, s := range states {
		n.states[s.ID] = s.Name
	}
	labels := slices.Clone(d.LabelIDs)
	var users, cycles, projects, teams, parents []string
	both := func(c *linear.Change) []string { return []string{c.From, c.To} }
	for _, e := range entries {
		labels = append(append(labels, e.AddedLabelIDs...), e.RemovedLabelIDs...)
		if e.Assignee != nil {
			users = append(users, both(e.Assignee)...)
		}
		if e.Cycle != nil {
			cycles = append(cycles, both(e.Cycle)...)
		}
		if e.Project != nil {
			for _, id := range both(e.Project) {
				if id != d.ProjectID {
					projects = append(projects, id)
				}
			}
		}
		if e.Team != nil {
			teams = append(teams, both(e.Team)...)
		}
		if e.Parent != nil {
			parents = append(parents, both(e.Parent)...)
		}
	}
	var err error
	if n.labels, err = labelsByID(ctx, p, n.labels, labels); err != nil {
		return names{}, err
	}
	if n.users, err = namesOf(ctx, "users", map[string]string{}, users, p.c.UsersByID, func(u linear.User) (string, string) { return u.ID, userName(u) }); err != nil {
		return names{}, err
	}
	if n.cycles, err = namesOf(ctx, "cycles", map[string]string{}, cycles, p.c.CyclesByID, func(c linear.Cycle) (string, string) { return c.ID, sprint(c).Name }); err != nil {
		return names{}, err
	}
	if n.projects, err = namesOf(ctx, "projects", map[string]string{}, projects, p.c.ProjectsByID, func(pr linear.Project) (string, string) { return pr.ID, pr.Name }); err != nil {
		return names{}, err
	}
	if d.ProjectID != "" {
		n.projects[d.ProjectID] = d.ProjectName
	}
	if n.teams, err = namesOf(ctx, "teams", map[string]string{}, teams, p.c.TeamsByID, func(t linear.Team) (string, string) { return t.ID, t.Key + " " + t.Name }); err != nil {
		return names{}, err
	}
	if n.issues, err = p.identifiers(ctx, parents); err != nil {
		return names{}, err
	}
	return n, nil
}

// identifiers maps the issue IDs among ids to their identifiers.
func (p *Provider) identifiers(ctx context.Context, ids []string) (map[string]string, error) {
	var asked []tracker.IssueID
	for _, id := range ids {
		asked = append(asked, tracker.IssueID(id))
	}
	valid, _ := issueIDs(asked)
	out := map[string]string{}
	if len(valid) == 0 {
		return out, nil
	}
	issues, err := p.issuesByID(ctx, valid, linear.IssueFilter{}, true)
	if err != nil {
		return nil, err
	}
	for _, is := range issues {
		out[is.ID] = is.Identifier
	}
	return out, nil
}

// events are one history entry as the ticket shows it, a field each.
func (n names) events(e linear.HistoryEntry) []tracker.Event {
	var out []tracker.Event
	add := func(field, from, to string) {
		out = append(out, tracker.Event{At: e.CreatedAt, Author: tracker.UserID(e.ActorID), Field: field, From: from, To: to})
	}
	change := func(field string, c *linear.Change, of map[string]string) {
		if c != nil {
			add(field, n.name(of, c.From), n.name(of, c.To))
		}
	}
	change("Status", e.State, n.states)
	change("Assignee", e.Assignee, n.users)
	change("Cycle", e.Cycle, n.cycles)
	change("Project", e.Project, n.projects)
	change("Parent", e.Parent, n.issues)
	change("Team", e.Team, n.teams)
	if e.Title != nil {
		add("Title", e.Title.From, e.Title.To)
	}
	if c := e.Estimate; c != nil {
		add("Estimate", number(c.From), number(c.To))
	}
	if c := e.Priority; c != nil {
		add("Priority", priorityName(c.From), priorityName(c.To))
	}
	if len(e.AddedLabelIDs) > 0 || len(e.RemovedLabelIDs) > 0 {
		add("Labels", n.list(n.labels, e.RemovedLabelIDs), n.list(n.labels, e.AddedLabelIDs))
	}
	if e.Archived != nil {
		from, to := "", "archived"
		if !*e.Archived {
			from, to = "archived", ""
		}
		add("Archived", from, to)
	}
	return out
}

func (n names) list(of map[string]string, ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, n.name(of, id))
	}
	return strings.Join(parts, ", ")
}

func number(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(*v)
}

func priorityName(v *float64) string {
	if v == nil {
		return ""
	}
	if i := int(*v); float64(i) == *v && i >= 0 && i < len(priorityNames) {
		return priorityNames[i]
	}
	return number(v)
}
