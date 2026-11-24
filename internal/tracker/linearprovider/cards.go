package linearprovider

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// idChunk is the most IDs or identifiers one read by key names: one page
// of Linear's largest size. Linear charged a page of 42 issues with card
// fields 8 points at first 100 and at 250 (measured), so a full chunk
// costs no more than a short one.
const idChunk = 250

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

// labelNames maps the label IDs issues carry to their names: those each
// issue's read listed with it, and for any other ID (an issue with more
// labels than the nested list holds, an archived label) a read of
// exactly the IDs left. An ID Linear does not answer is left out.
func (p *Provider) labelNames(ctx context.Context, issues []linear.Issue) (map[string]string, error) {
	names := map[string]string{}
	var ids []string
	for _, is := range issues {
		for _, l := range is.Labels {
			names[l.ID] = l.Name
		}
		ids = append(ids, is.LabelIDs...)
	}
	return labelsByID(ctx, p, names, ids)
}

// labelsByID adds to names the names of the distinct labels among ids it
// lacks, read by exactly those IDs, and returns it; no request when it
// lacks none.
func labelsByID(ctx context.Context, p *Provider, names map[string]string, ids []string) (map[string]string, error) {
	var asked []string
	for _, id := range ids {
		if _, ok := names[id]; !ok && id != "" && !slices.Contains(asked, id) {
			asked = append(asked, id)
		}
	}
	if len(asked) == 0 {
		return names, nil
	}
	found, err := p.c.LabelsByID(ctx, asked)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: labels by ID: %w", err)
	}
	for _, l := range found {
		names[l.ID] = l.Name
	}
	return names, nil
}

// card is an issue as a board row draws it. A label labelNames could not
// name keeps an empty name.
func card(is linear.Issue, labels map[string]string) tracker.Card {
	c := tracker.Card{
		ID: tracker.IssueID(is.ID), Key: is.Identifier, Project: tracker.ProjectID(is.TeamID), Title: is.Title,
		Status: tracker.StatusID(is.StateID), Assignee: tracker.UserID(is.AssigneeID), Priority: priorityChoice(is.Priority),
		Parent: tracker.IssueID(is.ParentID), Created: is.CreatedAt, Updated: is.UpdatedAt, URL: is.URL,
	}
	if len(is.LabelIDs) > 0 {
		c.Labels = make([]tracker.Choice, 0, len(is.LabelIDs))
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
// order key of sortOrder, and the estimate when the team uses estimates,
// which each issue's read says with its team's scale.
func (p *Provider) rows(ctx context.Context, b tracker.BoardID, issues []linear.Issue) ([]tracker.Row, error) {
	issues = slices.DeleteFunc(slices.Clone(issues), func(is linear.Issue) bool { return is.TeamID != string(b) })
	cards, err := p.cards(ctx, issues)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Row, 0, len(issues))
	for i, is := range issues {
		key, err := tracker.FloatOrderKey(is.SortOrder)
		if err != nil {
			return nil, fmt.Errorf("linearprovider: issue %s: %w", is.ID, err)
		}
		r := tracker.Row{Card: cards[i], Order: key}
		if estimates(is.TeamEstimateScale) && is.Estimate != nil {
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
// requests of idChunk IDs. Archived issues come only with
// includeArchived.
func (p *Provider) issuesByID(ctx context.Context, ids []string, filter linear.IssueFilter, includeArchived bool) ([]linear.Issue, error) {
	return byKeys(ctx, ids, func(ctx context.Context, chunk []string) (linear.Page[linear.Issue], error) {
		f := filter
		f.IDs = chunk
		return p.c.ListIssues(ctx, linear.IssueQuery{Filter: f, IncludeArchived: includeArchived, Limit: len(chunk)}, "")
	})
}

// byKeys reads what read lists for each chunk of idChunk keys, one page
// a chunk. Each key names at most one issue (an ID, or an identifier,
// which one issue holds at a time) and the page is as large as the
// chunk, so a page holds every issue its keys name and no walk by
// cursor follows; a page that says otherwise fails the read.
func byKeys[T any](ctx context.Context, keys []string, read func(context.Context, []string) (linear.Page[T], error)) ([]T, error) {
	var out []T
	for chunk := range slices.Chunk(keys, idChunk) {
		page, err := read(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("linearprovider: list issues: %w", err)
		}
		if page.HasNextPage && len(page.Items) < len(chunk) {
			return nil, fmt.Errorf("linearprovider: list issues: %d keys answered with more than one page", len(chunk))
		}
		out = append(out, page.Items...)
	}
	return out, nil
}

// walk reads every page read returns, from the first, until one says no
// more follow, however short or empty; maxPages bounds the walk (zero is
// no bound) and a walk it cuts short returns what it read with
// errTooLong. A failed page fails the walk; the client fails a page whose
// cursor did not advance.
func walk[T any](ctx context.Context, read func(context.Context, string) (linear.Page[T], error), maxPages int) ([]T, error) {
	var out []T
	after := ""
	for pages := 1; ; pages++ {
		page, err := read(ctx, after)
		if err != nil {
			return nil, fmt.Errorf("linearprovider: list issues: %w", err)
		}
		out = append(out, page.Items...)
		if !page.HasNextPage {
			return out, nil
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

// BoardRows are the rows of the issues the team holds now that rule
// lists (ruleFilter, BoardIssues' filter), read in requests of 250 IDs.
// Archived and trashed issues, issues of another team or that rule
// leaves out, and IDs that are not issue IDs are Missing.
func (p *Provider) BoardRows(ctx context.Context, b tracker.BoardID, ids []tracker.IssueID, rule tracker.Eligibility) (tracker.Rows, error) {
	if b == "" {
		return tracker.Rows{}, fmt.Errorf("linearprovider: board rows: %w: no board named", tracker.ErrInvalidInput)
	}
	valid, _ := issueIDs(ids)
	issues, err := p.issuesByID(ctx, valid, ruleFilter(b, rule), false)
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

// NewRows reads the issue's row from the lists, as BoardRows does: Linear's
// lists show an issue as soon as it exists. A board is one team's issues,
// so one request filtered to the boards' teams finds the issue on its own
// team's board, and every other board is Off; Linear has no board it
// cannot judge.
func (p *Provider) NewRows(ctx context.Context, issue tracker.IssueID, boards []tracker.BoardID) (tracker.Placement, error) {
	if err := tracker.CheckNewRows(issue, boards); err != nil {
		return tracker.Placement{}, err
	}
	valid, _ := issueIDs([]tracker.IssueID{issue})
	teams := make([]string, len(boards))
	for i, b := range boards {
		teams[i] = string(b)
	}
	issues, err := p.issuesByID(ctx, valid, linear.IssueFilter{Teams: teams}, false)
	if err != nil {
		return tracker.Placement{}, err
	}
	var out tracker.Placement
	for _, b := range boards {
		rows, err := p.rows(ctx, b, issues)
		if err != nil {
			return tracker.Placement{}, err
		}
		if len(rows) == 1 {
			out.Rows = append(out.Rows, tracker.BoardRow{Board: b, Row: rows[0]})
		} else {
			out.Off = append(out.Off, b)
		}
	}
	return out, nil
}

// Cards are the cards of the issues that exist, archived ones included,
// read in requests of 250 IDs. Trashed issues and IDs that are not
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
