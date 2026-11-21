package jiraprovider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// A board's list holds the issues its saved filter selects whose status
// is mapped to one of its columns, without epics on a scrum board
// (measured: no issue of the filter in an unmapped status was on either
// list, and the scrum list held none of the filter's epics). A kanban
// board's sub-query is required too: unverified, since the measured one
// left nothing out, and the safe direction, because requiring it can
// only call an issue off a board whose list shows it, which that board's
// next delta corrects, never store a row the list will not deliver.
//
// The lists come from Jira's search index, which shows a new issue some
// seconds after its create (measured: 0.6 s, 0.7 to 1.6 s and 23 s).
// jql/match judges the issue as it is: measured, the membership JQL below
// agreed with the board's list by ID on every sample and answered a new
// issue correctly 0.3 s after its create. So a list's miss is confirmed
// with it (BoardRows), and a new issue is placed with it (NewRows).

// pastQueries is why NewRows did not judge a board: one match request
// takes no more queries. A type, so the package runs no code at init.
type pastQueries struct{}

func (pastQueries) Error() string {
	return fmt.Sprintf("jiraprovider: one membership match judges at most %d boards' queries", jira.MaxMatchQueries)
}

// membershipJQL is the JQL that matches the issues b's list holds, built
// from its configuration alone: `filter = <id> AND status in (<mapped>)`,
// with `AND hierarchyLevel != 1` on a scrum board and `AND (<sub-query>)`
// on a kanban board that has one. It is empty with no error for a board
// that maps no status, which holds nothing, and an error for a
// configuration it cannot be built from.
func membershipJQL(cfg jira.BoardConfig) (string, error) {
	if _, err := numericID("filter", cfg.FilterID); err != nil {
		return "", fmt.Errorf("jiraprovider: board %d names no saved filter to judge membership by (%q)", cfg.BoardID, cfg.FilterID)
	}
	var statuses []string
	for _, col := range cfg.Columns {
		for _, s := range col.StatusIDs {
			if _, err := numericID("status", s); err != nil {
				return "", fmt.Errorf("jiraprovider: board %d maps status %q, which is not a status ID", cfg.BoardID, s)
			}
			if !slices.Contains(statuses, s) {
				statuses = append(statuses, s)
			}
		}
	}
	if len(statuses) == 0 {
		return "", nil
	}
	jql := "filter = " + cfg.FilterID + " AND status in (" + strings.Join(statuses, ", ") + ")"
	switch {
	case cfg.Type == jira.BoardScrum:
		jql += " AND hierarchyLevel != 1"
	case cfg.Type == jira.BoardKanban && strings.TrimSpace(cfg.SubQuery) != "":
		jql += " AND (" + cfg.SubQuery + ")"
	}
	return jql, nil
}

// NewRows places issue on boards with one jql/match request of every
// board's membership query (identical queries sent once, in the order of
// boards; a board past jira.MaxMatchQueries is Unknown), then reads the
// rows of the boards that hold it with one bulk fetch of their row
// fields, through the rowOf a list read uses: measured, the same issues
// read from a board list and from the bulk fetch differed only in URLs,
// which rowOf does not read. A board whose configuration cannot be read
// or built from, or whose query Jira answered with an error (a filter the
// user cannot see), is Unknown; a failed match or bulk fetch fails the
// call.
func (p *Provider) NewRows(ctx context.Context, issue tracker.IssueID, boards []tracker.BoardID) (tracker.Placement, error) {
	if err := tracker.CheckNewRows(issue, boards); err != nil {
		return tracker.Placement{}, err
	}
	if err := checkIssue(issue); err != nil {
		return tracker.Placement{}, err
	}
	var out tracker.Placement
	unknown := func(b tracker.BoardID, err error) {
		out.Unknown = append(out.Unknown, tracker.UnjudgedBoard{Board: b, Err: err})
	}
	type judged struct {
		b     board
		query int
	}
	var queries []string
	var held []judged
	for _, id := range boards {
		b, err := p.board(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return tracker.Placement{}, err
			}
			unknown(id, err)
			continue
		}
		jql, err := membershipJQL(b.cfg)
		switch {
		case err != nil:
			unknown(id, err)
			continue
		case jql == "":
			out.Off = append(out.Off, id)
			continue
		}
		i := slices.Index(queries, jql)
		if i < 0 {
			if len(queries) == jira.MaxMatchQueries {
				unknown(id, pastQueries{})
				continue
			}
			queries, i = append(queries, jql), len(queries)
		}
		held = append(held, judged{b: b, query: i})
	}
	if len(held) == 0 {
		return out, nil
	}
	matches, err := p.jira.MatchJQL(ctx, []string{string(issue)}, queries)
	if err != nil {
		return tracker.Placement{}, fmt.Errorf("jiraprovider: place issue %s on %d boards: %w", issue, len(held), err)
	}
	var on []board
	for _, h := range held {
		switch m := matches[h.query]; {
		case len(m.Errors) > 0:
			unknown(h.b.id, fmt.Errorf("jiraprovider: Jira refused board %s's membership query: %s", h.b.id, m.Errors[0]))
		case slices.Contains(m.Matched, string(issue)):
			on = append(on, h.b)
		default:
			out.Off = append(out.Off, h.b.id)
		}
	}
	if len(on) == 0 {
		return out, nil
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.Placement{}, err
	}
	var fields []string
	for _, b := range on {
		for _, f := range b.rowFields(s) {
			if !slices.Contains(fields, f) {
				fields = append(fields, f)
			}
		}
	}
	read, err := p.issuesByID(ctx, []tracker.IssueID{issue}, fields)
	if err != nil {
		return tracker.Placement{}, fmt.Errorf("jiraprovider: place issue %s: %w", issue, err)
	}
	if len(read) == 0 {
		for _, b := range on {
			unknown(b.id, fmt.Errorf("jiraprovider: Jira matched issue %s on board %s and then did not return it", issue, b.id))
		}
		return out, nil
	}
	cat := newCatalog()
	for _, b := range on {
		row, err := b.rowOf(s, read[0], cat)
		if err != nil {
			return tracker.Placement{}, err
		}
		out.Rows = append(out.Rows, tracker.BoardRow{Board: b.id, Row: row})
	}
	out.Catalog = cat.out
	return out, nil
}

// confirmMissing reads, with b's row fields, the issues of ids that b's
// list left out and that jql/match says b holds: a list misses a new
// issue until the index takes it. A board whose membership query cannot
// be built, or that Jira answers with an error, keeps the list's answer.
// A failed match or read fails the call.
func (p *Provider) confirmMissing(ctx context.Context, b board, s site, ids []tracker.IssueID, cat *catalog) ([]tracker.Row, error) {
	if jql, err := membershipJQL(b.cfg); err == nil && jql != "" {
		return p.confirmHeld(ctx, b, s, jql, ids, cat)
	}
	return nil, nil
}

// confirmHeld is confirmMissing with b's membership query jql.
func (p *Provider) confirmHeld(ctx context.Context, b board, s site, jql string, ids []tracker.IssueID, cat *catalog) ([]tracker.Row, error) {
	var held []tracker.IssueID
	for chunk := range slices.Chunk(ids, jira.MaxMatchIssues) {
		refs := make([]string, len(chunk))
		for i, id := range chunk {
			refs[i] = string(id)
		}
		m, err := p.jira.MatchJQL(ctx, refs, []string{jql})
		if err != nil {
			return nil, fmt.Errorf("confirm %d issues the list left out: %w", len(chunk), err)
		}
		// A query Jira answers with an error matches nothing, so the
		// list's answer stands.
		for _, id := range m[0].Matched {
			held = append(held, tracker.IssueID(id))
		}
	}
	if len(held) == 0 {
		return nil, nil
	}
	read, err := p.issuesByID(ctx, held, b.rowFields(s))
	if err != nil {
		return nil, fmt.Errorf("read %d issues the list left out: %w", len(held), err)
	}
	out := make([]tracker.Row, 0, len(read))
	for _, is := range read {
		row, err := b.rowOf(s, is, cat)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// issuesByID bulk-fetches ids with fields: the issues Jira returned of
// those asked. One Jira says it could not read fails the call, since it
// may exist.
func (p *Provider) issuesByID(ctx context.Context, ids []tracker.IssueID, fields []string) ([]*jira.Issue, error) {
	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = string(id)
	}
	res, err := p.jira.BulkFetch(ctx, refs, fields...)
	if err != nil {
		return nil, err
	}
	if len(res.Failed) > 0 {
		return nil, fmt.Errorf("jiraprovider: Jira could not read issue %s: %s", res.Failed[0].ID, res.Failed[0].Message)
	}
	return slices.DeleteFunc(res.Issues, func(is *jira.Issue) bool { return !slices.Contains(ids, tracker.IssueID(is.ID)) }), nil
}
