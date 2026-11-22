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
// 100 history entries, each with the users, cycles, projects, teams,
// parent issues and labels it names, come with the issue in one request,
// and each further page of history with its names in one more; only the
// labels the issue carries beyond its nested list are read by exactly
// their IDs. An entity Linear answers null for, deleted or hidden from the
// key, is shown by its ID.
func (p *Provider) Detail(ctx context.Context, issue tracker.IssueID) (tracker.Detail, error) {
	if issue == "" {
		return tracker.Detail{}, fmt.Errorf("linearprovider: detail: %w: no issue named", tracker.ErrInvalidInput)
	}
	d, err := p.c.Issue(ctx, string(issue))
	if err != nil {
		return tracker.Detail{}, fmt.Errorf("linearprovider: detail of %s: %w", issue, err)
	}
	out, err := p.details(ctx, []*linear.IssueDetail{d})
	if err != nil {
		return tracker.Detail{}, err
	}
	return out[0], nil
}

// Details reads issues as Detail does, linear.MaxIssueDetails to a
// request (Linear charges a request of several the points of one), then
// what each issue needs beyond its first pages, as Detail does, and the
// labels any of them carries beyond its nested list in one request for
// all of them. An ID Linear does not return, or that is no issue ID, is
// Missing.
func (p *Provider) Details(ctx context.Context, issues []tracker.IssueID) (tracker.Details, error) {
	for i, issue := range issues {
		if issue == "" || slices.Contains(issues[:i], issue) {
			return tracker.Details{}, fmt.Errorf("linearprovider: details: %w: each issue is named once", tracker.ErrInvalidInput)
		}
	}
	valid, invalid := issueIDs(issues)
	out := tracker.Details{Missing: invalid}
	var read []*linear.IssueDetail
	for chunk := range slices.Chunk(valid, linear.MaxIssueDetails) {
		got, err := p.c.IssueDetails(ctx, chunk)
		if err != nil {
			return tracker.Details{}, fmt.Errorf("linearprovider: details of %d issues: %w", len(chunk), err)
		}
		read = append(read, got...)
	}
	for _, id := range valid {
		if !slices.ContainsFunc(read, func(d *linear.IssueDetail) bool { return d.ID == id }) {
			out.Missing = append(out.Missing, tracker.IssueID(id))
		}
	}
	if len(read) == 0 {
		return out, nil
	}
	var err error
	if out.Details, err = p.details(ctx, read); err != nil {
		return tracker.Details{}, err
	}
	return out, nil
}

// details are ds as their tickets show them: each issue's team's states
// and history past the first page read, then the label names all of them
// need.
func (p *Provider) details(ctx context.Context, ds []*linear.IssueDetail) ([]tracker.Detail, error) {
	n := names{states: map[string]string{}, labels: map[string]string{}, users: map[string]string{}, cycles: map[string]string{},
		projects: map[string]string{}, teams: map[string]string{}, issues: map[string]string{}}
	states := make([][]linear.WorkflowState, len(ds))
	older := make([][]linear.HistoryEntry, len(ds))
	var labels []string
	for i, d := range ds {
		st := d.States
		var err error
		if d.MoreStates {
			if st, err = p.c.WorkflowStates(ctx, d.TeamID); err != nil {
				return nil, fmt.Errorf("linearprovider: workflow states of team %s: %w", d.TeamID, err)
			}
		}
		if states[i], err = boardStates(st); err != nil {
			return nil, err
		}
		n.addIssue(d, states[i])
		labels = append(labels, d.LabelIDs...)
		if older[i], err = p.olderHistory(ctx, d, n); err != nil {
			return nil, err
		}
	}
	var err error
	if n.labels, err = labelsByID(ctx, p, n.labels, labels); err != nil {
		return nil, err
	}
	out := make([]tracker.Detail, len(ds))
	for i, d := range ds {
		out[i] = n.detail(d, states[i], older[i])
	}
	return out, nil
}

// detail is d as its ticket shows it, with its team's states and its
// whole history, older being the entries after d's own page, its IDs
// named by n.
func (n names) detail(d *linear.IssueDetail, states []linear.WorkflowState, older []linear.HistoryEntry) tracker.Detail {
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
	for i := len(older) - 1; i >= 0; i-- {
		out.History = append(out.History, n.events(&older[i])...)
	}
	for i := len(d.History.Items) - 1; i >= 0; i-- {
		out.History = append(out.History, n.events(&d.History.Items[i].HistoryEntry)...)
	}
	for _, s := range states {
		out.Statuses = append(out.Statuses, status(s))
	}
	return out
}

// olderHistory is every history entry of d after the page d holds, newest
// first, with what each page names added to n as the page arrives: the
// entities a page came with are not kept past it. History pages in
// Linear's order of creation, which no change moves an entry in.
func (p *Provider) olderHistory(ctx context.Context, d *linear.IssueDetail, n names) ([]linear.HistoryEntry, error) {
	var out []linear.HistoryEntry
	for page := d.History; page.HasNextPage; {
		var err error
		if page, err = p.c.IssueHistory(ctx, d.ID, page.EndCursor); err != nil {
			return nil, fmt.Errorf("linearprovider: history of %s: %w", d.ID, err)
		}
		out = slices.Grow(out, len(page.Items))
		for i := range page.Items {
			n.add(&page.Items[i])
			out = append(out, page.Items[i].HistoryEntry)
		}
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

// addIssue names what d and its own page of history name: its labels and
// project from the issue, the states from its team's, and what each entry
// names from what came with it. A label d carries that neither its nested
// list nor an entry names is read by its ID afterwards, in one request
// for every issue.
func (n names) addIssue(d *linear.IssueDetail, states []linear.WorkflowState) {
	for _, l := range d.Labels {
		n.labels[l.ID] = l.Name
	}
	for _, s := range states {
		n.states[s.ID] = s.Name
	}
	if d.ProjectID != "" {
		n.projects[d.ProjectID] = d.ProjectName
	}
	for i := range d.History.Items {
		n.add(&d.History.Items[i])
	}
}

// add names what e came with. A side Linear answered null for is left
// out, so the event shows its ID.
func (n names) add(e *linear.NamedEntry) {
	for _, u := range []*linear.UserRef{e.FromAssignee, e.ToAssignee} {
		if u != nil {
			n.users[u.ID] = userName(u.Name, u.DisplayName)
		}
	}
	for _, c := range []*linear.CycleRef{e.FromCycle, e.ToCycle} {
		if c != nil {
			n.cycles[c.ID] = cycleName(c.Number, c.Name)
		}
	}
	for _, pr := range []*linear.ProjectRef{e.FromProject, e.ToProject} {
		if pr != nil {
			n.projects[pr.ID] = pr.Name
		}
	}
	for _, t := range []*linear.TeamRef{e.FromTeam, e.ToTeam} {
		if t != nil {
			n.teams[t.ID] = t.Key + " " + t.Name
		}
	}
	for _, is := range []*linear.IssueKey{e.FromParent, e.ToParent} {
		if is != nil {
			n.issues[is.ID] = is.Identifier
		}
	}
	for _, ls := range [][]linear.LabelRef{e.AddedLabels, e.RemovedLabels} {
		for _, l := range ls {
			n.labels[l.ID] = l.Name
		}
	}
}

// events are one history entry as the ticket shows it, a field each.
func (n names) events(e *linear.HistoryEntry) []tracker.Event {
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
