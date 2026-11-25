package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// MirrorMaxViewSections bounds the sections one board view or patch reads:
// every sprint, the backlog and every column, so no board has more.
const MirrorMaxViewSections = mirrorMaxSprints + 1 + mirrorMaxColumns

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
var mirrorBoardViewSQL = `SELECT b.board_id, b.project_id, b.name, b.plans_sprints, b.estimate_unit, b.estimate_field, b.data,
	  b.lifetime, b.revision, b.totals_revision, b.estimate_reset_at FROM tracker_cache_boards b
	WHERE b.root_id = ?1 AND b.board_id = ?2 AND ` + mirrorBoardShown("?1", "?2")

// ReadMirrorBoardView reads, in one snapshot (mirrorReadSnapshot), a
// board's structure (as GetMirrorBoard), its section totals (as
// CountMirrorSections), the first page of each section pick names from
// that structure (as ListMirrorSection) and what they name (MirrorNames):
// one statement per part, the board's columns and their statuses read once
// for the structure and the totals. pick runs inside the snapshot, again on
// each of its retries, so the sections it chooses are the structure's. ok is false for a board
// the mirror does not show.
func (s *Store) ReadMirrorBoardView(rootID int64, boardID string, pick func(MirrorStructure) []MirrorSection) (view MirrorBoardView, ok bool, err error) {
	label := fmt.Sprintf("read tracker mirror %d board %s view", rootID, boardID)
	view, err = mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorBoardView, error) {
		var v MirrorBoardView
		st := &v.Structure
		b := &st.Board
		var board mirrorShownBoard
		err := tx.QueryRow(mirrorBoardViewSQL, rootID, boardID).Scan(&b.ID, &b.ProjectID, &b.Name, &b.PlansSprints, &b.EstimateUnit, &b.EstimateField, &b.Data,
			&board.lifetime, &board.revision, &board.totalsRevision, &board.estimateResetAt)
		if errors.Is(err, sql.ErrNoRows) {
			return MirrorBoardView{}, nil
		}
		if err != nil {
			return MirrorBoardView{}, err
		}
		ok = true
		st.Lifetime, board.unit = board.lifetime, b.EstimateUnit
		if st.Columns, err = readMirrorColumnsTx(tx, rootID, boardID); err != nil {
			return MirrorBoardView{}, err
		}
		if st.Sprints, err = readMirrorSprintsTx(tx, rootID, boardID); err != nil {
			return MirrorBoardView{}, err
		}
		columns := map[string]int{}
		for position, column := range st.Columns {
			for _, status := range column.Statuses {
				columns[status] = position
			}
		}
		if v.Counts, err = countMirrorSectionsTx(tx, rootID, boardID, board, columns); err != nil {
			return MirrorBoardView{}, err
		}
		sections := pick(*st)
		if len(sections) > MirrorMaxViewSections {
			return MirrorBoardView{}, fmt.Errorf("%w: %d sections, over %d", ErrInvalidTrackerCache, len(sections), MirrorMaxViewSections)
		}
		for _, section := range sections {
			if _, known := mirrorSectionSQL[section.Kind]; !known {
				return MirrorBoardView{}, fmt.Errorf("%w: section kind %d", ErrInvalidTrackerCache, section.Kind)
			}
		}
		v.Pages = make([]MirrorSectionPage, len(sections))
		rows := make([][]MirrorRow, len(sections))
		for i, section := range sections {
			if v.Pages[i], err = readMirrorSectionTx(tx, rootID, boardID, board, section, MirrorRowKey{}); err != nil {
				return MirrorBoardView{}, fmt.Errorf("section %d: %w", i, err)
			}
			rows[i] = v.Pages[i].Rows
		}
		var statuses []string
		for _, column := range st.Columns {
			statuses = append(statuses, column.Statuses...)
		}
		v.Names, err = readMirrorNamesTx(tx, rootID, rows, statuses)
		return v, err
	})
	if err != nil {
		return MirrorBoardView{}, false, err
	}
	return view, ok, nil
}
