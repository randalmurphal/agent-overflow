package trackerapp

import (
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// Section kinds a board view and a section page name.
const (
	SectionSprint  = "sprint"
	SectionBacklog = "backlog"
	SectionColumn  = "column"
)

// Section names one section of a board: a sprint by ID, the backlog
// (rows in no open sprint whose status is not done or canceled), or a
// column by its position.
type Section struct {
	Kind   string `json:"kind"`
	Sprint string `json:"sprint,omitempty"`
	Column int    `json:"column,omitempty"`
}

func (sec Section) mirror() (store.MirrorSection, error) {
	switch {
	case sec.Kind == SectionSprint && sec.Sprint != "" && sec.Column == 0:
		return store.MirrorSection{Kind: store.MirrorSectionSprint, SprintID: sec.Sprint}, nil
	case sec.Kind == SectionBacklog && sec.Sprint == "" && sec.Column == 0:
		return store.MirrorSection{Kind: store.MirrorSectionBacklog}, nil
	case sec.Kind == SectionColumn && sec.Sprint == "" && sec.Column >= 0:
		return store.MirrorSection{Kind: store.MirrorSectionColumn, Column: sec.Column}, nil
	}
	return store.MirrorSection{}, fmt.Errorf("%w: section %+v names no section", ErrInvalidRequest, sec)
}

// ErrStalePage is a section page asked for after a cursor read at another
// revision or lifetime of the board: rows moved since, so the page could
// repeat or skip some. The caller reads the section again from its start.
var ErrStalePage = errors.New("trackerapp: the board changed since the section was read; read it again from its start")

// SectionCursor is where a section's next page starts, and the lifetime
// and revision of the board the page before it was read at.
type SectionCursor struct {
	Order    string `json:"order"`
	Issue    string `json:"issue"`
	Lifetime int64  `json:"lifetime"`
	Revision int64  `json:"revision"`
}

// SectionPage is a page of a section's rows in board order. Next is where
// the following page starts; nil when no rows follow. Stale lists the
// rows whose card the mirror holds only in part (a stored encoding this
// version cannot read): they show what the columns hold, and a read of
// their rows through the board replaces them.
type SectionPage struct {
	Section Section        `json:"section"`
	Rows    []RowView      `json:"rows"`
	Next    *SectionCursor `json:"next,omitempty"`
	Stale   []string       `json:"stale,omitempty"`
}

// EstimateView is a board's estimate: its unit (none, points or time in
// seconds) and the values a person picks from when the tracker fixes
// them.
type EstimateView struct {
	Unit    string               `json:"unit"`
	Choices []EstimateChoiceView `json:"choices,omitempty"`
}

// EstimateChoiceView is one value of a fixed estimate scale.
type EstimateChoiceView struct {
	Value float64 `json:"value"`
	Label string  `json:"label,omitempty"`
}

// SectionTotal is how many rows a section holds and the sum of their
// estimates in the board's unit, a row with none adding nothing; the
// store counts both.
type SectionTotal struct {
	Rows     int     `json:"rows"`
	Estimate float64 `json:"estimate"`
}

// totalOf is t as a view.
func totalOf(t store.MirrorSectionTotal) SectionTotal { return SectionTotal(t) }

// ColumnView is a board column, its statuses and its rows' total.
type ColumnView struct {
	Name     string       `json:"name"`
	Statuses []StatusView `json:"statuses"`
	Total    SectionTotal `json:"total"`
}

// SprintView is a sprint of a board and the total of the board's rows in
// it. State is future, active or closed; times are Unix milliseconds, 0
// when the sprint has not reached them.
type SprintView struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Goal      string       `json:"goal,omitempty"`
	State     string       `json:"state"`
	Start     int64        `json:"start,omitempty"`
	End       int64        `json:"end,omitempty"`
	Completed int64        `json:"completed,omitempty"`
	Total     SectionTotal `json:"total"`
}

// SyncView is a scope's sync state: when it was last read and
// reconciled (Unix milliseconds by this computer's wall clock as each
// began, 0 for never or for one stored before the mirror kept wall times)
// and its last failure.
type SyncView struct {
	ReadAt       int64        `json:"read_at,omitempty"`
	ReconciledAt int64        `json:"reconciled_at,omitempty"`
	Failure      *FailureView `json:"failure,omitempty"`
}

// FailureView is a scope's last failure: its kind (FailureKind), the
// sentence a person reads and when it happened, by this computer's wall
// clock.
type FailureView struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

// BoardView is a board as the pane opens it: its structure, the totals of
// its sections (All its rows) at its TotalsRevision, its sync state and
// the first page of each section asked for, at the board's Lifetime and
// Revision (which a patch moves forward, BoardPatch). Project is empty for
// a board of no one project.
type BoardView struct {
	ID             string        `json:"id"`
	Lifetime       int64         `json:"lifetime"`
	Revision       int64         `json:"revision"`
	TotalsRevision int64         `json:"totals_revision"`
	Name           string        `json:"name"`
	Project        string        `json:"project,omitempty"`
	PlansSprints   bool          `json:"plans_sprints"`
	Estimate       EstimateView  `json:"estimate"`
	Columns        []ColumnView  `json:"columns"`
	Sprints        []SprintView  `json:"sprints"`
	All            SectionTotal  `json:"all"`
	Backlog        SectionTotal  `json:"backlog"`
	Sync           SyncView      `json:"sync"`
	Sections       []SectionPage `json:"sections"`
}

// maxViewSections bounds the sections one board view reads pages of.
const maxViewSections = 20

// The wire names of the model's enums, indexed by value: arrays, which
// cost nothing to link, read through nameOf.
var (
	groupNames  = [...]string{tracker.GroupBacklog: "backlog", tracker.GroupToDo: "todo", tracker.GroupInProgress: "in_progress", tracker.GroupDone: "done", tracker.GroupCanceled: "canceled"}
	unitNames   = [...]string{tracker.UnitNone: "none", tracker.UnitPoints: "points", tracker.UnitTime: "time"}
	sprintNames = [...]string{tracker.SprintFuture: "future", tracker.SprintActive: "active", tracker.SprintClosed: "closed"}
)

// nameOf is names[v], empty for a value names does not hold.
func nameOf[T ~uint8](names []string, v T) string {
	if int(v) < len(names) {
		return names[v]
	}
	return ""
}

// Board is the view of scope's board with the first page of each of
// sections, at most maxViewSections. The structure, the totals and the
// pages are one state of the mirror (store ReadMirrorBoardView), so the
// totals count the rows the pages show, and a page's Next continues that
// state or fails with ErrStalePage. A board whose structure the mirror
// does not hold fails with ErrNotMirrored.
func (s *Service) Board(scope Scope, sections []Section) (BoardView, error) {
	if err := scope.boardOnly(); err != nil {
		return BoardView{}, err
	}
	if len(sections) > maxViewSections {
		return BoardView{}, fmt.Errorf("%w: %d sections, over %d", ErrInvalidRequest, len(sections), maxViewSections)
	}
	asked := make([]store.MirrorSection, len(sections))
	for i, sec := range sections {
		m, err := sec.mirror()
		if err != nil {
			return BoardView{}, err
		}
		asked[i] = m
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return BoardView{}, err
	}
	defer s.leave()
	snap, ok, err := s.store.ReadMirrorBoardView(root, string(scope.Board), asked)
	if err != nil {
		return BoardView{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", scope.Board, err)
	}
	if !ok {
		return BoardView{}, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	pages := make([]SectionPage, len(asked))
	cards := make([][]shownCard, len(asked))
	for i, page := range snap.Pages {
		pages[i], cards[i] = sectionOf(sections[i], page)
	}
	// The columns' statuses and the cards of every page are named from the
	// snapshot their rows came from.
	views, names := namedCardViews(slices.Concat(cards...), snap.Names)
	for i := range pages {
		for j := range pages[i].Rows {
			pages[i].Rows[j].Card, views = views[0], views[1:]
		}
	}
	v := boardViewOf(snap.Structure, snap.Counts, names)
	v.Lifetime, v.Revision, v.TotalsRevision = snap.Structure.Lifetime, snap.Counts.Revision, snap.Counts.TotalsRevision
	sync, _, err := s.store.GetMirrorSync(root, string(scope.Board), s.steady().UnixMilli())
	if err != nil {
		return BoardView{}, fmt.Errorf("trackerapp: read the sync state of board %s: %w", scope.Board, err)
	}
	v.Sync = syncViewOf(sync.MirrorSyncOutcome)
	v.Sections = pages
	return v, nil
}

// boardViewOf is the board view of m with the totals counts, without its
// sync state and sections; names names the columns' statuses.
func boardViewOf(m store.MirrorStructure, counts store.MirrorSectionCounts, names catalogNames) BoardView {
	st := structureOf(m, tracker.Catalog{})
	v := BoardView{
		ID: string(st.Board.ID), Name: st.Board.Name, Project: string(st.Board.Project), PlansSprints: st.Board.Sprints,
		Estimate: EstimateView{Unit: nameOf(unitNames[:], st.Estimate.Unit)}, All: totalOf(counts.All), Backlog: totalOf(counts.Backlog),
		Columns: []ColumnView{}, Sprints: []SprintView{}, Sections: []SectionPage{},
	}
	for _, ch := range st.Estimate.Choices {
		v.Estimate.Choices = append(v.Estimate.Choices, EstimateChoiceView(ch))
	}
	for i, col := range st.Columns {
		cv := ColumnView{Name: col.Name, Statuses: []StatusView{}, Total: totalOf(counts.Columns[i])}
		for _, id := range col.Statuses {
			cv.Statuses = append(cv.Statuses, names.status(string(id)))
		}
		v.Columns = append(v.Columns, cv)
	}
	for _, sp := range st.Sprints {
		v.Sprints = append(v.Sprints, SprintView{
			ID: string(sp.ID), Name: sp.Name, Goal: sp.Goal, State: nameOf(sprintNames[:], sp.State),
			Start: millis(sp.Start), End: millis(sp.End), Completed: millis(sp.Completed), Total: totalOf(counts.Sprints[string(sp.ID)]),
		})
	}
	return v
}

// SectionRows is the page of scope's board's section after after; nil
// after starts at the first row. A cursor from another revision of the
// board fails with ErrStalePage.
func (s *Service) SectionRows(scope Scope, section Section, after *SectionCursor) (SectionPage, error) {
	if err := scope.boardOnly(); err != nil {
		return SectionPage{}, err
	}
	sec, err := section.mirror()
	if err != nil {
		return SectionPage{}, err
	}
	var key store.MirrorRowKey
	if after != nil {
		if after.Issue == "" {
			return SectionPage{}, fmt.Errorf("%w: a section cursor names its issue", ErrInvalidRequest)
		}
		key = store.MirrorRowKey{OrderKey: after.Order, IssueID: after.Issue, Lifetime: after.Lifetime, Revision: after.Revision}
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return SectionPage{}, err
	}
	defer s.leave()
	return s.sectionPage(root, scope.Board, section, sec, key)
}

func (s *Service) sectionPage(root int64, board tracker.BoardID, section Section, sec store.MirrorSection, after store.MirrorRowKey) (SectionPage, error) {
	page, err := s.store.ListMirrorSection(root, string(board), sec, after)
	if errors.Is(err, store.ErrTrackerCacheStale) {
		return SectionPage{}, fmt.Errorf("%w: board %s: %w", ErrStalePage, board, err)
	}
	if err != nil {
		return SectionPage{}, fmt.Errorf("trackerapp: read a section of board %s: %w", board, err)
	}
	out, cards := sectionOf(section, page)
	views, _ := namedCardViews(cards, page.Names)
	for i := range out.Rows {
		out.Rows[i].Card = views[i]
	}
	return out, nil
}

// sectionOf is page of section as a view with its cards, which it leaves
// unnamed in the page's rows for the caller to name.
func sectionOf(section Section, page store.MirrorSectionPage) (SectionPage, []shownCard) {
	out := SectionPage{Section: section, Rows: make([]RowView, len(page.Rows))}
	cards := make([]shownCard, len(page.Rows))
	for i, m := range page.Rows {
		row, ok := rowOf(m)
		if !ok {
			out.Stale = append(out.Stale, m.Card.IssueID)
		}
		cards[i] = shownCard{card: row.Card, since: m.Card.StatusSince}
		out.Rows[i] = RowView{Order: string(row.Order), Estimate: row.Estimate}
	}
	if page.More {
		out.Next = &SectionCursor{Order: page.Next.OrderKey, Issue: page.Next.IssueID, Lifetime: page.Next.Lifetime, Revision: page.Next.Revision}
	}
	return out, cards
}
