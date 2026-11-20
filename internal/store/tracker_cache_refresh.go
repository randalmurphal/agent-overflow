package store

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// MirrorRefreshTargets is where the mirror holds what a write may have
// changed, as one statement read it (ReadMirrorRefreshTargets): the
// fences a refresh names in its writes and which resources it has to
// read again at all.
type MirrorRefreshTargets struct {
	// Sweep is the root's sweep, which a card or detail write of the
	// refresh names (MirrorSync.Sweep).
	Sweep int64
	// Rows are the rows of the issues asked on boards the mirror shows,
	// each with its board's lifetime, sorted by issue and board.
	Rows []MirrorRowTarget
	// Kept are the issues asked that the mirror keeps, sorted.
	Kept []string
	// Details are the issues asked whose detail the mirror holds, sorted.
	Details []string
	// Boards are the boards asked that the mirror shows, with their
	// lifetimes, sorted.
	Boards []MirrorBoardTarget
}

// MirrorRowTarget is an issue's row on a board the mirror shows, with the
// board's lifetime.
type MirrorRowTarget struct {
	IssueID, BoardID string
	Lifetime         int64
}

// MirrorBoardTarget is a board the mirror shows, with its lifetime.
type MirrorBoardTarget struct {
	BoardID  string
	Lifetime int64
}

// The parts of mirrorRefreshTargetsSQL, by its first column.
const (
	refreshRoot = iota
	refreshRow
	refreshKept
	refreshDetail
	refreshBoard
)

// mirrorShownBoardRow holds for a tracker_cache_boards row bl that the
// mirror shows (mirrorBoardShown), read from the row itself.
const mirrorShownBoardRow = `bl.retired = 0 AND bl.read_at > 0 AND EXISTS (SELECT 1 FROM tracker_cache_roots sr
	  JOIN project_tracker_boards sp ON sp.connection_id = sr.connection_id AND sp.board_id = bl.board_id
	 WHERE sr.id = bl.root_id)`

// mirrorRefreshTargetsSQL reads, for root ?1, the issues of the JSON list
// ?2 and the boards of the JSON list ?3: the live root's sweep, the rows of
// the issues on shown boards with each board's lifetime (each issue probed
// by its key prefix), the issues kept, those with a stored detail, and the
// boards shown with their lifetimes.
var mirrorRefreshTargetsSQL = `SELECT 0, '', '', r.sweep FROM tracker_cache_roots r
	WHERE r.id = ?1 AND r.connection_id IS NOT NULL
	UNION ALL
	SELECT 1, b.issue_id, b.board_id, bl.lifetime FROM json_each(?2) l
	CROSS JOIN tracker_cache_board_issues b ON b.root_id = ?1 AND b.issue_id = l.value
	CROSS JOIN tracker_cache_boards bl ON bl.root_id = b.root_id AND bl.board_id = b.board_id
	WHERE ` + mirrorShownBoardRow + `
	UNION ALL
	SELECT 2, i.issue_id, '', 0 FROM json_each(?2) l
	CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = l.value
	WHERE ` + mirrorIssueKept("i.root_id", "i.issue_id") + `
	UNION ALL
	SELECT 3, d.issue_id, '', 0 FROM json_each(?2) l
	CROSS JOIN tracker_cache_issue_details d ON d.root_id = ?1 AND d.issue_id = l.value
	WHERE d.data IS NOT NULL AND ` + mirrorIssueKept("d.root_id", "d.issue_id") + `
	UNION ALL
	SELECT 4, '', bl.board_id, bl.lifetime FROM json_each(?3) l
	CROSS JOIN tracker_cache_boards bl ON bl.root_id = ?1 AND bl.board_id = l.value
	WHERE ` + mirrorShownBoardRow

// ReadMirrorRefreshTargets reads, in one statement, where the mirror holds
// issueIDs and boardIDs (each at most mirrorMaxLookup, a repeated one read
// once): the rows of the issues on the boards it shows with each board's
// lifetime, which of the issues it keeps and holds a detail of, which of
// the boards it shows with their lifetimes, and the root's sweep. A
// caller reads it before the tracker reads it stores, and names the
// lifetimes and the sweep in those writes. A root that is detached or gone
// fails with ErrTrackerCacheGone.
func (s *Store) ReadMirrorRefreshTargets(rootID int64, issueIDs, boardIDs []string) (MirrorRefreshTargets, error) {
	if len(issueIDs) > mirrorMaxLookup || len(boardIDs) > mirrorMaxLookup {
		return MirrorRefreshTargets{}, fmt.Errorf("%w: %d issues and %d boards, over %d", ErrInvalidTrackerCache,
			len(issueIDs), len(boardIDs), mirrorMaxLookup)
	}
	issues, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return MirrorRefreshTargets{}, err
	}
	boards, err := encodeMirrorIDs(boardIDs)
	if err != nil {
		return MirrorRefreshTargets{}, err
	}
	var out MirrorRefreshTargets
	live := false
	err = scanMirrorRows(s.reader(), mirrorRefreshTargetsSQL, []any{rootID, issues, boards}, func(r rowScanner) error {
		var part int
		var issue, board string
		var n int64
		if err := r.Scan(&part, &issue, &board, &n); err != nil {
			return err
		}
		switch part {
		case refreshRoot:
			live, out.Sweep = true, n
		case refreshRow:
			out.Rows = append(out.Rows, MirrorRowTarget{IssueID: issue, BoardID: board, Lifetime: n})
		case refreshKept:
			out.Kept = append(out.Kept, issue)
		case refreshDetail:
			out.Details = append(out.Details, issue)
		case refreshBoard:
			out.Boards = append(out.Boards, MirrorBoardTarget{BoardID: board, Lifetime: n})
		}
		return nil
	})
	if err == nil && !live {
		err = ErrTrackerCacheGone
	}
	if err != nil {
		return MirrorRefreshTargets{}, fmt.Errorf("store: read tracker mirror %d refresh targets: %w", rootID, err)
	}
	slices.SortFunc(out.Rows, func(a, b MirrorRowTarget) int {
		return cmp.Or(strings.Compare(a.IssueID, b.IssueID), strings.Compare(a.BoardID, b.BoardID))
	})
	slices.Sort(out.Kept)
	slices.Sort(out.Details)
	slices.SortFunc(out.Boards, func(a, b MirrorBoardTarget) int { return strings.Compare(a.BoardID, b.BoardID) })
	return out, nil
}
