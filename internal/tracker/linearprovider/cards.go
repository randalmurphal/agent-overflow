package linearprovider

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// idChunk is the most IDs one read by ID names: one page of the client's
// default size, about 1,000 points by Linear's documented rule.
const idChunk = 100

// priorityNames are Linear's fixed priorities by value.
var priorityNames = []string{"No priority", "Urgent", "High", "Medium", "Low"}

// priorityChoice is a priority as a card shows it: the zero Choice for
// none.
func priorityChoice(p linear.Priority) tracker.Choice {
	if p <= linear.PriorityNone || int(p) >= len(priorityNames) {
		return tracker.Choice{}
	}
	return tracker.Choice{ID: strconv.Itoa(int(p)), Name: priorityNames[p]}
}

// labelNames maps the label IDs issues carry to their names, reading
// each team's labels (its own and the workspace's) for the teams whose
// issues carry any; none when no issue carries a label. An ID it cannot
// name (an archived label) is left out.
func (p *Provider) labelNames(ctx context.Context, issues []linear.Issue) (map[string]string, error) {
	var teams []string
	for _, is := range issues {
		if len(is.LabelIDs) > 0 && !slices.Contains(teams, is.TeamID) {
			teams = append(teams, is.TeamID)
		}
	}
	names := map[string]string{}
	for _, team := range teams {
		if err := p.addLabelNames(ctx, team, names); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// addLabelNames adds the names of the labels team's issues can carry to
// names.
func (p *Provider) addLabelNames(ctx context.Context, team string, names map[string]string) error {
	labels, err := p.c.Labels(ctx, team)
	if err != nil {
		return fmt.Errorf("linearprovider: labels of team %s: %w", team, err)
	}
	for _, l := range labels {
		names[l.ID] = l.Name
	}
	return nil
}

// card is an issue as a board row draws it. A label labelNames could not
// name keeps an empty name.
func card(is linear.Issue, labels map[string]string) tracker.Card {
	c := tracker.Card{
		ID: tracker.IssueID(is.ID), Key: is.Identifier, Project: tracker.ProjectID(is.TeamID), Title: is.Title,
		Status: tracker.StatusID(is.StateID), Assignee: tracker.UserID(is.AssigneeID), Priority: priorityChoice(is.Priority),
		Parent: tracker.IssueID(is.ParentID), Created: is.CreatedAt, Updated: is.UpdatedAt,
	}
	for _, id := range is.LabelIDs {
		c.Labels = append(c.Labels, tracker.Choice{ID: id, Name: labels[id]})
	}
	if is.CycleID != "" {
		c.Sprints = []tracker.SprintID{tracker.SprintID(is.CycleID)}
	}
	switch {
	case is.CompletedAt != nil:
		c.Resolved = *is.CompletedAt
	case is.CanceledAt != nil:
		c.Resolved = *is.CanceledAt
	}
	return c
}

// cards are issues as cards, their labels named.
func (p *Provider) cards(ctx context.Context, issues []linear.Issue) ([]tracker.Card, error) {
	labels, err := p.labelNames(ctx, issues)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Card, 0, len(issues))
	for _, is := range issues {
		out = append(out, card(is, labels))
	}
	return out, nil
}

// rows are the issues of board among issues as its rows: the card, the
// order key of sortOrder, and the estimate when the team uses estimates.
// The team's settings are read only when an issue carries an estimate.
func (p *Provider) rows(ctx context.Context, b tracker.BoardID, issues []linear.Issue) ([]tracker.Row, error) {
	issues = slices.DeleteFunc(slices.Clone(issues), func(is linear.Issue) bool { return is.TeamID != string(b) })
	cards, err := p.cards(ctx, issues)
	if err != nil {
		return nil, err
	}
	estimates := false
	if slices.ContainsFunc(issues, func(is linear.Issue) bool { return is.Estimate != nil }) {
		t, err := p.team(ctx, string(b))
		if err != nil {
			return nil, err
		}
		estimates = estimate(t.Estimation).Unit != tracker.UnitNone
	}
	out := make([]tracker.Row, 0, len(issues))
	for i, is := range issues {
		key, err := tracker.FloatOrderKey(is.SortOrder)
		if err != nil {
			return nil, fmt.Errorf("linearprovider: issue %s: %w", is.ID, err)
		}
		r := tracker.Row{Card: cards[i], Order: key}
		if estimates && is.Estimate != nil {
			v := *is.Estimate
			r.Estimate = &v
		}
		out = append(out, r)
	}
	return out, nil
}

// issueIDs splits ids into the distinct issue IDs Linear can be asked
// for, in the order given, and the others, which name no issue.
func issueIDs(ids []tracker.IssueID) (valid []string, invalid []tracker.IssueID) {
	seen := map[tracker.IssueID]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if linear.IsIssueID(string(id)) {
			valid = append(valid, string(id))
		} else {
			invalid = append(invalid, id)
		}
	}
	return valid, invalid
}

// issuesByID reads the issues ids names that filter also selects, in
// requests of idChunk IDs, each read to its end. Archived issues come
// only with includeArchived.
func (p *Provider) issuesByID(ctx context.Context, ids []string, filter linear.IssueFilter, includeArchived bool) ([]linear.Issue, error) {
	var out []linear.Issue
	for chunk := range slices.Chunk(ids, idChunk) {
		f := filter
		f.IDs = chunk
		issues, err := p.listAll(ctx, linear.IssueQuery{Filter: f, IncludeArchived: includeArchived, Limit: len(chunk)}, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, issues...)
	}
	return out, nil
}

// listAll reads every page of q; maxPages bounds the walk (zero is no
// bound) and a walk it cuts short fails with errTooLong.
func (p *Provider) listAll(ctx context.Context, q linear.IssueQuery, maxPages int) ([]linear.Issue, error) {
	var out []linear.Issue
	after := ""
	for pages := 1; ; pages++ {
		page, err := p.c.ListIssues(ctx, q, after)
		if err != nil {
			return nil, fmt.Errorf("linearprovider: list issues: %w", err)
		}
		out = append(out, page.Items...)
		if !page.HasNextPage {
			return out, nil
		}
		if page.EndCursor == after {
			return nil, fmt.Errorf("linearprovider: list issues: the page cursor did not advance")
		}
		if maxPages > 0 && pages >= maxPages {
			return out, errTooLong
		}
		after = page.EndCursor
	}
}

// missing is asked less the IDs found, each once, in the order asked.
func missing(asked []tracker.IssueID, found map[tracker.IssueID]bool) []tracker.IssueID {
	var out []tracker.IssueID
	seen := map[tracker.IssueID]bool{}
	for _, id := range asked {
		if !found[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// BoardRows are the rows of the issues the team holds now, read in
// requests of 100 IDs. Archived and trashed issues, issues of another
// team and IDs that are not issue IDs are Missing.
func (p *Provider) BoardRows(ctx context.Context, b tracker.BoardID, ids []tracker.IssueID) (tracker.Rows, error) {
	if b == "" {
		return tracker.Rows{}, fmt.Errorf("linearprovider: board rows: %w: no board named", tracker.ErrInvalidInput)
	}
	valid, _ := issueIDs(ids)
	issues, err := p.issuesByID(ctx, valid, linear.IssueFilter{Teams: []string{string(b)}}, false)
	if err != nil {
		return tracker.Rows{}, err
	}
	rows, err := p.rows(ctx, b, issues)
	if err != nil {
		return tracker.Rows{}, err
	}
	found := make(map[tracker.IssueID]bool, len(rows))
	for _, r := range rows {
		found[r.Card.ID] = true
	}
	return tracker.Rows{Rows: rows, Missing: missing(ids, found)}, nil
}

// Cards are the cards of the issues that exist, archived ones included,
// read in requests of 100 IDs. Trashed issues and IDs that are not
// issue IDs are Missing.
func (p *Provider) Cards(ctx context.Context, ids []tracker.IssueID) (tracker.Cards, error) {
	valid, _ := issueIDs(ids)
	issues, err := p.issuesByID(ctx, valid, linear.IssueFilter{}, true)
	if err != nil {
		return tracker.Cards{}, err
	}
	issues = slices.DeleteFunc(issues, func(is linear.Issue) bool { return is.Trashed })
	cards, err := p.cards(ctx, issues)
	if err != nil {
		return tracker.Cards{}, err
	}
	found := make(map[tracker.IssueID]bool, len(cards))
	for _, c := range cards {
		found[c.ID] = true
	}
	return tracker.Cards{Cards: cards, Missing: missing(ids, found)}, nil
}
