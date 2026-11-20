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
// client carries into its section cursors, every section's total (a
// sprint or column absent from Sprints or Columns holds no row), and for
// each section asked, in order, the named issues' rows that are members of
// it now, in board order.
//
// The client's rule, for each ID of the event: remove it from every
// section held; insert each returned row at its (order, issue) place when
// that place is inside the pages already loaded or the section is loaded
// whole; take the totals; set the revision and lifetime of each cursor.
type BoardPatch struct {
	Lifetime int64          `json:"lifetime"`
	Revision int64          `json:"revision"`
	All      SectionTotal   `json:"all"`
	Backlog  SectionTotal   `json:"backlog"`
	Sprints  []SprintTotal  `json:"sprints,omitempty"`
	Columns  []ColumnTotal  `json:"columns,omitempty"`
	Sections []PatchSection `json:"sections"`
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
// maxEventIssues) in sections (at most maxViewSections). It fails with
// ErrNotMirrored for a board the mirror does not show: the client reads
// the board view again.
func (s *Service) BoardPatch(scope Scope, sections []Section, issues []tracker.IssueID) (BoardPatch, error) {
	if err := scope.boardOnly(); err != nil {
		return BoardPatch{}, err
	}
	if len(sections) > maxViewSections || len(issues) > maxEventIssues {
		return BoardPatch{}, fmt.Errorf("%w: a patch reads at most %d sections and %d issues", ErrInvalidRequest, maxViewSections, maxEventIssues)
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
	snap, ok, err := s.store.ReadMirrorBoardPatch(root, string(scope.Board), asked, idStrings(issues))
	if err != nil {
		return BoardPatch{}, fmt.Errorf("trackerapp: read a patch of board %s: %w", scope.Board, err)
	}
	if !ok {
		return BoardPatch{}, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	out := BoardPatch{
		Lifetime: snap.Lifetime, Revision: snap.Revision, All: totalOf(snap.Counts.All), Backlog: totalOf(snap.Counts.Backlog),
		Sections: make([]PatchSection, len(sections)),
	}
	for id, t := range snap.Counts.Sprints {
		out.Sprints = append(out.Sprints, SprintTotal{Sprint: id, Total: totalOf(t)})
	}
	slices.SortFunc(out.Sprints, func(a, b SprintTotal) int { return cmp.Compare(a.Sprint, b.Sprint) })
	for position, t := range snap.Counts.Columns {
		out.Columns = append(out.Columns, ColumnTotal{Column: position, Total: totalOf(t)})
	}
	slices.SortFunc(out.Columns, func(a, b ColumnTotal) int { return cmp.Compare(a.Column, b.Column) })
	cards := make([][]shownCard, len(sections))
	for i, rows := range snap.Sections {
		page, shown := sectionOf(sections[i], store.MirrorSectionPage{Rows: rows})
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
