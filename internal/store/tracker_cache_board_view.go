package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// mirrorMaxViewSections bounds the sections one board view reads: every
// sprint, the backlog and every column.
const mirrorMaxViewSections = mirrorMaxSprints + 1 + mirrorMaxColumns

// MirrorBoardView is one board's structure (with no catalog), section
// totals and the first page of each section asked for, in its order, read
// in one snapshot: the totals count the rows the pages hold, and each
// page's Next names the snapshot's lifetime and revision, so a later page
// continues it or fails with ErrTrackerCacheStale. Names are what every
// page's cards and the columns name, from the same snapshot; the pages'
// own Names are empty.
type MirrorBoardView struct {
	Structure MirrorStructure
	Counts    MirrorSectionCounts
	Pages     []MirrorSectionPage
	Names     MirrorNames
}

// mirrorBoardViewSQL reads a shown board's own fields and what its rows'
// reads need.
var mirrorBoardViewSQL = `SELECT b.board_id, b.project_id, b.name, b.plans_sprints, b.estimate_unit, b.data,
	  b.lifetime, b.revision, b.estimate_reset_at FROM tracker_cache_boards b
	WHERE b.root_id = ?1 AND b.board_id = ?2 AND ` + mirrorBoardShown("?1", "?2")

// ReadMirrorBoardView reads, in one read transaction, a board's structure
// (as GetMirrorBoard), its section totals (as CountMirrorSections), the
// first page of each of sections (as ListMirrorSection) and what they name
// (MirrorNames): one statement per part, the board's columns and their
// statuses read once for the structure and the totals. ok is false for a
// board the mirror does not show.
func (s *Store) ReadMirrorBoardView(rootID int64, boardID string, sections []MirrorSection) (view MirrorBoardView, ok bool, err error) {
	if len(sections) > mirrorMaxViewSections {
		return MirrorBoardView{}, false, fmt.Errorf("%w: %d sections, over %d", ErrInvalidTrackerCache, len(sections), mirrorMaxViewSections)
	}
	for _, section := range sections {
		if _, ok := mirrorSectionSQL[section.Kind]; !ok {
			return MirrorBoardView{}, false, fmt.Errorf("%w: section kind %d", ErrInvalidTrackerCache, section.Kind)
		}
	}
	action := fmt.Sprintf("store: read tracker mirror %d board %s view", rootID, boardID)
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorBoardView{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	st := &view.Structure
	b := &st.Board
	var board mirrorShownBoard
	err = tx.QueryRow(mirrorBoardViewSQL, rootID, boardID).Scan(&b.ID, &b.ProjectID, &b.Name, &b.PlansSprints, &b.EstimateUnit, &b.Data,
		&board.lifetime, &board.revision, &board.estimateResetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorBoardView{}, false, tx.Commit()
	}
	if err != nil {
		return MirrorBoardView{}, false, fmt.Errorf("%s: %w", action, err)
	}
	st.Lifetime, board.unit = board.lifetime, b.EstimateUnit
	if st.Columns, err = readMirrorColumnsTx(tx, rootID, boardID); err != nil {
		return MirrorBoardView{}, false, fmt.Errorf("%s: %w", action, err)
	}
	if st.Sprints, err = readMirrorSprintsTx(tx, rootID, boardID); err != nil {
		return MirrorBoardView{}, false, fmt.Errorf("%s: %w", action, err)
	}
	columns := map[string]int{}
	for position, column := range st.Columns {
		for _, status := range column.Statuses {
			columns[status] = position
		}
	}
	if view.Counts, err = countMirrorSectionsTx(tx, rootID, boardID, board, columns); err != nil {
		return MirrorBoardView{}, false, fmt.Errorf("%s: %w", action, err)
	}
	view.Pages = make([]MirrorSectionPage, len(sections))
	rows := make([][]MirrorRow, len(sections))
	for i, section := range sections {
		if view.Pages[i], err = readMirrorSectionTx(tx, rootID, boardID, board, section, MirrorRowKey{}); err != nil {
			return MirrorBoardView{}, false, fmt.Errorf("%s: section %d: %w", action, i, err)
		}
		rows[i] = view.Pages[i].Rows
	}
	var statuses []string
	for _, column := range st.Columns {
		statuses = append(statuses, column.Statuses...)
	}
	if view.Names, err = readMirrorNamesTx(tx, rootID, rows, statuses); err != nil {
		return MirrorBoardView{}, false, fmt.Errorf("%s: %w", action, err)
	}
	return view, true, tx.Commit()
}
