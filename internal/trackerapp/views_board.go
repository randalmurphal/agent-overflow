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
// reconciled (Unix milliseconds, 0 for never) and its last failure.
type SyncView struct {
	ReadAt       int64        `json:"read_at,omitempty"`
	ReconciledAt int64        `json:"reconciled_at,omitempty"`
	Failure      *FailureView `json:"failure,omitempty"`
}

// FailureView is a scope's last failure: its kind (FailureKind), the
// sentence a person reads and when it happened.
type FailureView struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

// BoardView is a board as the pane opens it: its structure, the totals of
// its sections (All its rows), its sync state and the first page of each
// section asked for. Project is empty for a board of no one project.
type BoardView struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Project      string        `json:"project,omitempty"`
	PlansSprints bool          `json:"plans_sprints"`
	Estimate     EstimateView  `json:"estimate"`
	Columns      []ColumnView  `json:"columns"`
	Sprints      []SprintView  `json:"sprints"`
	All          SectionTotal  `json:"all"`
	Backlog      SectionTotal  `json:"backlog"`
	Sync         SyncView      `json:"sync"`
	Sections     []SectionPage `json:"sections"`
}

// maxViewSections bounds the sections one board view reads pages of.
const maxViewSections = 20

var (
	groupNames  = map[tracker.StatusGroup]string{tracker.GroupBacklog: "backlog", tracker.GroupToDo: "todo", tracker.GroupInProgress: "in_progress", tracker.GroupDone: "done", tracker.GroupCanceled: "canceled"}
	unitNames   = map[tracker.EstimateUnit]string{tracker.UnitNone: "none", tracker.UnitPoints: "points", tracker.UnitTime: "time"}
	sprintNames = map[tracker.SprintState]string{tracker.SprintFuture: "future", tracker.SprintActive: "active", tracker.SprintClosed: "closed"}
)

// Board is the view of scope's board with the first page of each of
// sections, at most maxViewSections. A board whose structure the mirror
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
	v, err := s.boardStructure(root, scope.Board)
	if err != nil {
		return BoardView{}, err
	}
	pages := make([]SectionPage, len(asked))
	cards := make([][]shownCard, len(asked))
	for i, sec := range asked {
		if pages[i], cards[i], err = s.readSection(root, scope.Board, sections[i], sec, store.MirrorRowKey{}); err != nil {
			return BoardView{}, err
		}
	}
	// The cards of every page are named together: one catalog read and
	// one read of their parents for the view, not one each per section.
	views, err := s.cardViews(root, slices.Concat(cards...))
	if err != nil {
		return BoardView{}, err
	}
	for i := range pages {
		for j := range pages[i].Rows {
			pages[i].Rows[j].Card, views = views[0], views[1:]
		}
	}
	v.Sections = pages
	return v, nil
}

// boardStructure is the board view without its sections.
func (s *Service) boardStructure(root int64, board tracker.BoardID) (BoardView, error) {
	m, ok, err := s.store.GetMirrorBoard(root, string(board))
	if err != nil {
		return BoardView{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", board, err)
	}
	if !ok {
		return BoardView{}, fmt.Errorf("%w: board %s", ErrNotMirrored, board)
	}
	counts, err := s.store.CountMirrorSections(root, string(board))
	if err != nil {
		return BoardView{}, fmt.Errorf("trackerapp: count the sections of board %s: %w", board, err)
	}
	var statusIDs []string
	for _, col := range m.Columns {
		statusIDs = append(statusIDs, col.Statuses...)
	}
	names, err := s.names(root, nil, statusIDs)
	if err != nil {
		return BoardView{}, err
	}
	st := structureOf(m, tracker.Catalog{})
	v := BoardView{
		ID: string(st.Board.ID), Name: st.Board.Name, Project: string(st.Board.Project), PlansSprints: st.Board.Sprints,
		Estimate: EstimateView{Unit: unitNames[st.Estimate.Unit]}, All: totalOf(counts.All), Backlog: totalOf(counts.Backlog),
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
			ID: string(sp.ID), Name: sp.Name, Goal: sp.Goal, State: sprintNames[sp.State],
			Start: millis(sp.Start), End: millis(sp.End), Completed: millis(sp.Completed), Total: totalOf(counts.Sprints[string(sp.ID)]),
		})
	}
	sync, _, err := s.store.GetMirrorSync(root, string(board))
	if err != nil {
		return BoardView{}, fmt.Errorf("trackerapp: read the sync state of board %s: %w", board, err)
	}
	v.Sync = SyncView{ReadAt: sync.ReadAt, ReconciledAt: sync.ReconciledAt}
	if sync.Failure.Kind != "" {
		v.Sync.Failure = &FailureView{Kind: sync.Failure.Kind, Message: sync.Failure.Message, At: sync.Failure.At}
	}
	return v, nil
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
	out, cards, err := s.readSection(root, board, section, sec, after)
	if err != nil {
		return SectionPage{}, err
	}
	views, err := s.cardViews(root, cards)
	if err != nil {
		return SectionPage{}, err
	}
	for i := range out.Rows {
		out.Rows[i].Card = views[i]
	}
	return out, nil
}

// readSection is the page of section after after with its cards, which
// it leaves unnamed in the page's rows for the caller to name.
func (s *Service) readSection(root int64, board tracker.BoardID, section Section, sec store.MirrorSection, after store.MirrorRowKey) (SectionPage, []shownCard, error) {
	page, err := s.store.ListMirrorSection(root, string(board), sec, after)
	if errors.Is(err, store.ErrTrackerCacheStale) {
		return SectionPage{}, nil, fmt.Errorf("%w: board %s: %w", ErrStalePage, board, err)
	}
	if err != nil {
		return SectionPage{}, nil, fmt.Errorf("trackerapp: read a section of board %s: %w", board, err)
	}
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
	return out, cards, nil
}
