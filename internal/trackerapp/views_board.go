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
	Groups   string `json:"groups,omitempty"`
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

// SectionTotal is how many rows a section holds, the sum of their
// estimates in the board's unit, a row with none adding nothing, and how
// many have none (always 0 on a board with no estimate); the store counts
// all three.
type SectionTotal struct {
	Rows        int     `json:"rows"`
	Estimate    float64 `json:"estimate"`
	Unestimated int     `json:"unestimated,omitempty"`
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
// its sections (All its rows) at its TotalsRevision, its sync state, the
// sections the Board screen shows (Screen) and the Backlog screen shows
// (Plan), the pace of its last finished sprints (Pace, nil for a board
// that plans none or has finished none), the unit the project counts its
// work in (Counts: the estimate's unit, or none when the project counts
// tickets, Estimates; a card's own estimate keeps Estimate's), and the first page of each
// section read, at the board's Lifetime and Revision (which a patch moves
// forward, BoardPatch). Groups is the version of the status groups the
// project gives the board's site (StatusGroups), which the sections and
// totals were read with: a client sends it back in its cursors and in the
// totals it holds (TotalsAt). Regrouped is those groups: each status the
// project puts in a group other than the tracker's, with that group
// (backlog, todo, in_progress, done, canceled, or left_out, out of
// counts), by which a count the client makes of rows it holds counts each
// row, as the view's totals do; a status it does not name counts in its
// tracker group. Project is empty for a board
// of no one project. IssueURL is the address of a card of the board less
// its key, which completes it escaped as a path segment
// (jiraprovider.IssueURLPrefix); empty on a tracker whose cards carry
// their own (CardView.URL).
type BoardView struct {
	ID             string            `json:"id"`
	Lifetime       int64             `json:"lifetime"`
	Revision       int64             `json:"revision"`
	TotalsRevision int64             `json:"totals_revision"`
	Groups         string            `json:"groups,omitempty"`
	Regrouped      map[string]string `json:"regrouped,omitempty"`
	Name           string            `json:"name"`
	Project        string            `json:"project,omitempty"`
	PlansSprints   bool              `json:"plans_sprints"`
	Estimate       EstimateView      `json:"estimate"`
	Counts         string            `json:"counts"`
	Columns        []ColumnView      `json:"columns"`
	Sprints        []SprintView      `json:"sprints"`
	All            SectionTotal      `json:"all"`
	Backlog        SectionTotal      `json:"backlog"`
	Sync           SyncView          `json:"sync"`
	Screen         []Section         `json:"screen"`
	Plan           []Section         `json:"plan"`
	Pace           *PaceView         `json:"pace,omitempty"`
	Sections       []SectionPage     `json:"sections"`
	IssueURL       string            `json:"issue_url,omitempty"`
}

// PaceView is what a board's last finished sprints (at most
// store.MirrorPaceSprints, one with no member on the board as zero)
// finished on average: how many sprints it averages, the tickets whose
// status counted as done when their sprint was completed, and the sum of
// their estimates as the board holds them now. No credit rule is applied.
// It is part of the section totals (BoardTotals), read with them.
type PaceView struct {
	Sprints  int     `json:"sprints"`
	Rows     float64 `json:"rows"`
	Estimate float64 `json:"estimate"`
}

// paceOf is the average of finished, nil for none.
func paceOf(finished []store.MirrorSprintFinished) *PaceView {
	if len(finished) == 0 {
		return nil
	}
	var p PaceView
	for _, f := range finished {
		p.Rows += float64(f.Done)
		p.Estimate += f.Estimate
	}
	p.Sprints = len(finished)
	p.Rows /= float64(p.Sprints)
	p.Estimate /= float64(p.Sprints)
	return &p
}

// planSections is the sections the Backlog screen shows of a board of
// structure m, in order: its active sprints, then its future ones, in the
// structure's order, then the backlog; only the backlog when it plans no
// sprints.
func planSections(m store.MirrorStructure) []Section {
	plan := []Section{}
	if m.Board.PlansSprints {
		for _, state := range []int{store.MirrorSprintActive, store.MirrorSprintFuture} {
			for _, sp := range m.Sprints {
				if sp.State == state {
					plan = append(plan, Section{Kind: SectionSprint, Sprint: sp.ID})
				}
			}
		}
	}
	return append(plan, Section{Kind: SectionBacklog})
}

// maxViewSections bounds the sections one board view reads pages of. The
// Board screen of a board with more reads the rest a page at a time.
const maxViewSections = 20

// screenSections is the sections the Board screen shows of a board of
// structure m, in order, as the wire and the store name them: its active
// sprints when it plans sprints, else every column.
func screenSections(m store.MirrorStructure) (screen []Section, stored []store.MirrorSection) {
	screen = []Section{}
	if m.Board.PlansSprints {
		for _, sp := range m.Sprints {
			if sp.State == store.MirrorSprintActive {
				screen = append(screen, Section{Kind: SectionSprint, Sprint: sp.ID})
				stored = append(stored, store.MirrorSection{Kind: store.MirrorSectionSprint, SprintID: sp.ID})
			}
		}
		return screen, stored
	}
	for i := range m.Columns {
		screen = append(screen, Section{Kind: SectionColumn, Column: i})
		stored = append(stored, store.MirrorSection{Kind: store.MirrorSectionColumn, Column: i})
	}
	return screen, stored
}

// The wire names of the model's enums, indexed by value: arrays, which
// cost nothing to link, read through nameOf.
var (
	// The mirror stores a tracker.StatusGroup as its number.
	groupNames  = store.MirrorGroupNames
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
// sections, at most maxViewSections, read with groups. No sections asks
// for the Board screen's (Screen), chosen from the same state as the view:
// the first page of the first maxViewSections of them. The structure, the
// totals and the pages are one state of the mirror (store
// ReadMirrorBoardView), so the totals count the rows the pages show, and a
// page's Next continues that state or fails with ErrStalePage. A board
// whose structure the mirror does not hold fails with ErrNotMirrored.
func (s *Service) Board(scope Scope, groups StatusGroups, sections []Section) (BoardView, error) {
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
	// read is the sections the pages are of: those asked, or the screen's
	// first. The pick may run again when the snapshot is retried.
	var screen, plan, read []Section
	snap, ok, err := s.store.ReadMirrorBoardView(root, string(scope.Board), groups.regroup, func(m store.MirrorStructure) []store.MirrorSection {
		var stored []store.MirrorSection
		screen, stored = screenSections(m)
		plan = planSections(m)
		if len(sections) > 0 {
			read = sections
			return asked
		}
		n := min(len(screen), maxViewSections)
		read = screen[:n]
		return stored[:n]
	})
	if err != nil {
		return BoardView{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", scope.Board, err)
	}
	if !ok {
		return BoardView{}, fmt.Errorf("%w: board %s", ErrNotMirrored, scope.Board)
	}
	pages := make([]SectionPage, len(snap.Pages))
	cards := make([][]shownCard, len(snap.Pages))
	for i, page := range snap.Pages {
		pages[i], cards[i] = sectionOf(read[i], page, groups.version)
	}
	// The columns' statuses and the cards of every page are named from the
	// snapshot their rows came from.
	views, names := namedCardViews(slices.Concat(cards...), snap.Names)
	for i := range pages {
		for j := range pages[i].Rows {
			pages[i].Rows[j].Card, views = views[0], views[1:]
		}
	}
	v := boardViewOf(snap.Structure, snap.Counts, names, groups)
	v.Lifetime, v.Revision, v.TotalsRevision = snap.Structure.Lifetime, snap.Counts.Revision, snap.Counts.TotalsRevision
	v.Groups, v.Regrouped = groups.version, groups.statuses
	sync, _, err := s.store.GetMirrorSync(root, string(scope.Board), s.steady().UnixMilli())
	if err != nil {
		return BoardView{}, fmt.Errorf("trackerapp: read the sync state of board %s: %w", scope.Board, err)
	}
	v.Sync = syncViewOf(sync.MirrorSyncOutcome)
	v.Screen, v.Plan, v.Pace = screen, plan, paceOf(snap.Counts.Pace)
	v.Sections = pages
	row, err := s.viewRow(scope.Connection)
	if err != nil {
		return BoardView{}, err
	}
	v.IssueURL = boardIssueURL(row)
	return v, nil
}

// boardViewOf is the board view of m with the totals counts, without its
// sync state and sections; names names the columns' statuses, and groups
// is how the project counts the board.
func boardViewOf(m store.MirrorStructure, counts store.MirrorSectionCounts, names catalogNames, groups StatusGroups) BoardView {
	st := structureOf(m, tracker.Catalog{})
	v := BoardView{
		ID: string(st.Board.ID), Name: st.Board.Name, Project: string(st.Board.Project), PlansSprints: st.Board.Sprints,
		Estimate: EstimateView{Unit: nameOf(unitNames[:], st.Estimate.Unit)}, Counts: nameOf(unitNames[:], groups.unit(st.Estimate.Unit)),
		All: totalOf(counts.All), Backlog: totalOf(counts.Backlog),
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

// SectionRows is the page of scope's board's section after after, read
// with groups; nil after starts at the first row. A cursor from another
// revision of the board, or one read with other status groups, fails with
// ErrStalePage.
func (s *Service) SectionRows(scope Scope, groups StatusGroups, section Section, after *SectionCursor) (SectionPage, error) {
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
		if after.Groups != groups.version {
			return SectionPage{}, fmt.Errorf("%w: board %s: the cursor was read with status groups %q, and the project's are %q now",
				ErrStalePage, scope.Board, after.Groups, groups.version)
		}
		key = store.MirrorRowKey{OrderKey: after.Order, IssueID: after.Issue, Lifetime: after.Lifetime, Revision: after.Revision}
	}
	root, err := s.readRoot(scope.Connection)
	if err != nil {
		return SectionPage{}, err
	}
	defer s.leave()
	return s.sectionPage(root, scope.Board, groups, section, sec, key)
}

func (s *Service) sectionPage(root int64, board tracker.BoardID, groups StatusGroups, section Section, sec store.MirrorSection,
	after store.MirrorRowKey) (SectionPage, error) {
	page, err := s.store.ListMirrorSection(root, string(board), groups.regroup, sec, after)
	if errors.Is(err, store.ErrTrackerCacheStale) {
		return SectionPage{}, fmt.Errorf("%w: board %s: %w", ErrStalePage, board, err)
	}
	if err != nil {
		return SectionPage{}, fmt.Errorf("trackerapp: read a section of board %s: %w", board, err)
	}
	out, cards := sectionOf(section, page, groups.version)
	views, _ := namedCardViews(cards, page.Names)
	for i := range out.Rows {
		out.Rows[i].Card = views[i]
	}
	return out, nil
}

// sectionOf is page of section as a view with its cards, which it leaves
// unnamed in the page's rows for the caller to name.
func sectionOf(section Section, page store.MirrorSectionPage, groups string) (SectionPage, []shownCard) {
	out := SectionPage{Section: section, Rows: make([]RowView, len(page.Rows))}
	cards := make([]shownCard, len(page.Rows))
	for i, m := range page.Rows {
		row, ok := rowOf(m)
		if !ok {
			out.Stale = append(out.Stale, m.Card.IssueID)
		}
		cards[i] = shownCard{card: row.Card, since: m.Card.StatusSince}
		out.Rows[i] = RowView{Order: string(row.Order), Estimate: row.Estimate, Support: m.Support}
	}
	if page.More {
		out.Next = &SectionCursor{Order: page.Next.OrderKey, Issue: page.Next.IssueID, Lifetime: page.Next.Lifetime, Revision: page.Next.Revision,
			Groups: groups}
	}
	return out, cards
}

// BoardSummary is a board as the mirror holds it, without its rows: its
// name, its project (the tracker's ID), whether it plans sprints, its
// estimate unit and the field it estimates by (empty when the estimate
// is no field of the ticket's), and its sprints as the board lists them.
type BoardSummary struct {
	Name          string
	Project       string
	PlansSprints  bool
	Unit          string
	EstimateField string
	Sprints       []SprintView
}

// BoardSummaryOf is board of connection id as the mirror holds it, with
// no tracker request; ErrNotMirrored when the mirror holds no structure
// of it.
func (s *Service) BoardSummaryOf(id, board string) (BoardSummary, error) {
	root, err := s.readRoot(id)
	if err != nil {
		return BoardSummary{}, err
	}
	defer s.leave()
	m, ok, err := s.store.GetMirrorBoard(root, board)
	if err != nil {
		return BoardSummary{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", board, err)
	}
	if !ok {
		return BoardSummary{}, fmt.Errorf("%w: board %s", ErrNotMirrored, board)
	}
	st := structureOf(m, tracker.Catalog{})
	out := BoardSummary{Name: st.Board.Name, Project: string(st.Board.Project), PlansSprints: st.Board.Sprints,
		Unit: nameOf(unitNames[:], st.Estimate.Unit), EstimateField: st.Estimate.Field}
	for _, sp := range st.Sprints {
		out.Sprints = append(out.Sprints, SprintView{ID: string(sp.ID), Name: sp.Name, State: nameOf(sprintNames[:], sp.State)})
	}
	return out, nil
}
