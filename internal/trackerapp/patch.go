package trackerapp

import (
	"cmp"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// BoardPatch is what a client holding some sections of a board needs to
// apply a BoardEvent's IDs, read in one snapshot (store
// ReadMirrorBoardPatch): the board's lifetime and revision, which the
// client carries into its section cursors, every section's total unless
// the client holds them (Totals nil), and for each section asked, in
// order, the named issues' rows that are members of it now, in board
// order.
//
// The client's rule, for each ID of the event: remove it from every
// section held; insert each returned row at its (order, issue) place when
// that place is inside the pages already loaded or the section is loaded
// whole; take the totals when the patch has them; set the revision and
// lifetime of each cursor.
type BoardPatch struct {
	Lifetime int64          `json:"lifetime"`
	Revision int64          `json:"revision"`
	Totals   *BoardTotals   `json:"totals,omitempty"`
	Sections []PatchSection `json:"sections"`
}

// BoardTotals is every section's total at the board's totals Revision; a
// sprint or column absent from Sprints or Columns holds no row.
type BoardTotals struct {
	Revision int64         `json:"revision"`
	All      SectionTotal  `json:"all"`
	Backlog  SectionTotal  `json:"backlog"`
	Sprints  []SprintTotal `json:"sprints,omitempty"`
	Columns  []ColumnTotal `json:"columns,omitempty"`
}

// TotalsAt names the totals a client holds: those of the board's Lifetime
// at its totals Revision (BoardView.TotalsRevision, BoardTotals.Revision).
// The zero value holds none. Groups is the version of the status groups
// the client's sections and totals were read with (BoardView.Groups),
// which it sends whether it holds totals or not.
type TotalsAt struct {
	Lifetime int64  `json:"lifetime"`
	Revision int64  `json:"revision"`
	Groups   string `json:"groups,omitempty"`
}

// SprintTotal is a sprint's total in a patch.
type SprintTotal struct {
	Sprint string       `json:"sprint"`
	Total  SectionTotal `json:"total"`
}

// ColumnTotal is a column's total in a patch, the column by position.
type ColumnTotal struct {
	Column int          `json:"column"`
	Total  SectionTotal `json:"total"`
}

// PatchSection is the rows of one section of a patch; Stale is as
// SectionPage has it.
type PatchSection struct {
	Section Section   `json:"section"`
	Rows    []RowView `json:"rows"`
	Stale   []string  `json:"stale,omitempty"`
}

// BoardPatch reads scope's board's patch for issues (at most
// maxEventIssues) in sections (at most store.MirrorMaxViewSections, every
// section a board can have, since a patch reads only the named issues'
// rows of each), read with groups, with the totals unless held names the
// ones the board has now. It fails with ErrNotMirrored for a board the
// mirror does not show, and with ErrStalePage when the client's sections
// were read with other status groups (held.Groups): the client reads the
// board view again.
func (s *Service) BoardPatch(scope Scope, groups StatusGroups, sections []Section, issues []tracker.IssueID, held TotalsAt) (BoardPatch, error) {
	if err := scope.boardOnly(); err != nil {
		return BoardPatch{}, err
	}
	if len(sections) > store.MirrorMaxViewSections || len(issues) > maxEventIssues {
		return BoardPatch{}, fmt.Errorf("%w: a patch reads at most %d sections and %d issues", ErrInvalidRequest, store.MirrorMaxViewSections, maxEventIssues)
	}
	if held.Groups != groups.version {
		return BoardPatch{}, fmt.Errorf("%w: board %s: the sections were read with status groups %q, and the project's are %q now",
			ErrStalePage, scope.Board, held.Groups, groups.version)
	}
	asked := make([]store.MirrorSection, len(sections))
	for i, sec := range sections {
		m, err := sec.mirror()
		if err != nil {
			return BoardPatch{}, err
		}
		asked[i] = m
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return BoardPatch{}, err
	}
	defer s.leave()
	snap, ok, err := s.store.ReadMirrorBoardPatch(root, string(scope.Board), groups.regroup, asked, idStrings(issues),
		store.MirrorTotalsAt{Lifetime: held.Lifetime, Revision: held.Revision})
	if err != nil {
		return BoardPatch{}, fmt.Errorf("trackerapp: read a patch of board %s: %w", scope.Board, err)
	}
	if !ok {
		return BoardPatch{}, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	out := BoardPatch{Lifetime: snap.Lifetime, Revision: snap.Revision, Sections: make([]PatchSection, len(sections))}
	if snap.Counts != nil {
		out.Totals = totalsOf(*snap.Counts)
	}
	cards := make([][]shownCard, len(sections))
	for i, rows := range snap.Sections {
		page, shown := sectionOf(sections[i], store.MirrorSectionPage{Rows: rows}, groups.version)
		out.Sections[i] = PatchSection{Section: page.Section, Rows: page.Rows, Stale: page.Stale}
		cards[i] = shown
	}
	views, _ := namedCardViews(slices.Concat(cards...), snap.Names)
	for i := range out.Sections {
		for j := range out.Sections[i].Rows {
			out.Sections[i].Rows[j].Card, views = views[0], views[1:]
		}
	}
	return out, nil
}

// totalsOf is c as a patch's totals, sprints and columns in order.
func totalsOf(c store.MirrorSectionCounts) *BoardTotals {
	out := &BoardTotals{Revision: c.TotalsRevision, All: totalOf(c.All), Backlog: totalOf(c.Backlog)}
	for id, t := range c.Sprints {
		out.Sprints = append(out.Sprints, SprintTotal{Sprint: id, Total: totalOf(t)})
	}
	slices.SortFunc(out.Sprints, func(a, b SprintTotal) int { return cmp.Compare(a.Sprint, b.Sprint) })
	for position, t := range c.Columns {
		out.Columns = append(out.Columns, ColumnTotal{Column: position, Total: totalOf(t)})
	}
	slices.SortFunc(out.Columns, func(a, b ColumnTotal) int { return cmp.Compare(a.Column, b.Column) })
	return out
}
