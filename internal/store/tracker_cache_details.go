package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// mirrorDetailCap is how many details a root keeps: the most recently
// opened.
const mirrorDetailCap = 200

// MirrorDetail is an issue's stored detail document: when the issue was
// last opened, when the document's read began, and the document, with the
// update time of the mirror's card of the issue as the same read found it.
type MirrorDetail struct {
	IssueID          string
	OpenedAt, ReadAt int64
	Data             []byte
	CardUpdatedAt    int64
	// row and sweep name the document for ConfirmMirrorDetail: its row,
	// and the root's sweep when it was read.
	row, sweep int64
}

// MirrorDetailWrite is what a detail write did for one issue, as its
// transaction committed: whether the mirror holds the document, or one
// from a read that began later, after it (Stored), whether its document
// changed, whether the document was superseded by one from a read that
// began later, and whether it is owed its card (CardOwed): the mirror
// holds no card of the issue, or one older than the document's, so the
// document is not stored until a cards read stores that card.
type MirrorDetailWrite struct {
	Stored, Changed, Superseded, CardOwed bool
}

// MirrorDetailWrites is what StoreMirrorDetails did, as its transaction
// committed: each document's outcome, in the order of its documents, and
// the issues whose details it dropped to keep the mirrorDetailCap most
// recently opened, sorted. Dropped names only details held before the
// write: a new detail the cap drops in the same write was never held, and
// its outcome says so (Stored false).
type MirrorDetailWrites struct {
	Docs    []MirrorDetailWrite
	Dropped []string
}

// mirrorDetailEvictSQL drops the details of a root beyond the
// mirrorDetailCap most recently opened, walking the opened index.
var mirrorDetailEvictSQL = `DELETE FROM tracker_cache_issue_details WHERE root_id = ?1 AND issue_id IN (
	SELECT issue_id FROM tracker_cache_issue_details WHERE root_id = ?1
	 ORDER BY opened_at DESC, issue_id DESC LIMIT -1 OFFSET ` + strconv.Itoa(mirrorDetailCap) + `)
	RETURNING issue_id`

// The detail statements: ?1 root, ?2 issue.
var (
	// mirrorDetailWriteStatesSQL reads, for each issue of the JSON list ?2 by
	// its place in the list, whether the mirror keeps it, its card's update
	// time (zero: no card), and its held detail: the opening, the read
	// time and the document, which a write compares with the one it
	// carries.
	mirrorDetailWriteStatesSQL = `SELECT l.key, ` + mirrorIssueKept("?1", "l.value") + `, coalesce(i.updated_at, 0),
		    d.opened_at, d.read_at, d.data IS NOT NULL, d.data
		FROM json_each(?2) l
		LEFT JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = l.value
		LEFT JOIN tracker_cache_issue_details d ON d.root_id = ?1 AND d.issue_id = l.value`
	// mirrorDetailStoreSQL stores the document ?4 read from ?3 and moves the
	// opening forward to ?5 (zero: none). Only an opening starts a detail,
	// and the opening is the new detail's read watermark: its document is
	// stored only from a read that began no earlier. A write that is no
	// opening reaches the insert for a held detail alone, whose conflict
	// keeps its opening, so its inserted 1 only meets the CHECK, which
	// SQLite applies before the conflict.
	mirrorDetailStoreSQL = `INSERT INTO tracker_cache_issue_details(root_id, issue_id, opened_at, read_at, data)
		VALUES (?1, ?2, max(?5, 1), max(?3, ?5), iif(?3 >= ?5, ?4, NULL))
		ON CONFLICT(root_id, issue_id) DO UPDATE SET read_at = ?3, data = ?4, opened_at = max(opened_at, ?5)`
	// mirrorDetailConfirmSQL is whether the detail is still row ?3 holding
	// the document read from ?4 of a kept issue, and its opening.
	mirrorDetailConfirmSQL = `SELECT d.rowid = ?3 AND d.read_at = ?4 AND d.data IS NOT NULL AND ` + mirrorIssueKept("?1", "?2") + `,
		d.opened_at FROM tracker_cache_issue_details d WHERE d.root_id = ?1 AND d.issue_id = ?2`
	// mirrorDetailOpenSQL moves the opening forward to ?3.
	mirrorDetailOpenSQL = `UPDATE tracker_cache_issue_details SET opened_at = max(opened_at, ?3) WHERE root_id = ?1 AND issue_id = ?2`
	// mirrorDetailMoveSQL moves the read time of the held details of the
	// issues of the JSON list ?2 to ?3 and their opening to ?4, each only
	// forward, leaving the documents.
	mirrorDetailMoveSQL = `UPDATE tracker_cache_issue_details SET read_at = max(read_at, ?3), opened_at = max(opened_at, ?4)
		WHERE root_id = ?1 AND issue_id IN (SELECT value FROM json_each(?2))`
)

// mirrorDetailState is an issue's detail as a write of it reads it, with
// its card's update time (zero: no card).
type mirrorDetailState struct {
	kept, held, differs      bool
	cardAt, openedAt, readAt int64
}

// readMirrorDetailStatesTx reads the state of the detail of each of docs
// in one statement, in docs' order, each compared with its document.
func readMirrorDetailStatesTx(tx *sql.Tx, rootID int64, docs []MirrorDetailDoc) ([]mirrorDetailState, error) {
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.IssueID
	}
	list, err := encodeMirrorIDs(ids)
	if err != nil {
		return nil, err
	}
	states := make([]mirrorDetailState, len(docs))
	err = scanMirrorRows(tx, mirrorDetailWriteStatesSQL, []any{rootID, list}, func(r rowScanner) error {
		var at int
		var kept bool
		var cardAt int64
		var opened, read sql.NullInt64
		var hasData sql.NullBool
		var data sql.RawBytes
		if err := r.Scan(&at, &kept, &cardAt, &opened, &read, &hasData, &data); err != nil {
			return err
		}
		if at < 0 || at >= len(docs) {
			return fmt.Errorf("a state of document %d of %d", at, len(docs))
		}
		states[at] = mirrorDetailState{kept: kept, held: opened.Valid, cardAt: cardAt, openedAt: opened.Int64, readAt: read.Int64,
			differs: !hasData.Bool || !bytes.Equal(data, mirrorData(docs[at].Data))}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read details: %w", err)
	}
	return states, nil
}

// ConfirmMirrorDetail marks d, a document GetMirrorDetail read, opened at
// openedAt (only forward) when the mirror still holds it: the same row,
// the document of the same read, of an issue the mirror keeps, with no
// sweep since d was read. Stored reports the confirmation. Unconfirmed,
// nothing is written: the caller reads the detail again rather than show
// d's document, which may predate a sweep. It never adds a detail, so it
// drops none, and it never changes a document: only a detail read does.
func (s *Store) ConfirmMirrorDetail(rootID int64, d MirrorDetail, opened MirrorRead) (result MirrorDetailWrite, err error) {
	openedAt := opened.at
	if d.IssueID == "" || d.row <= 0 || d.sweep <= 0 || d.ReadAt <= 0 || openedAt <= 0 {
		return MirrorDetailWrite{}, fmt.Errorf("%w: a confirmation needs a detail GetMirrorDetail read and the time it was opened", ErrInvalidTrackerCache)
	}
	action := fmt.Sprintf("store: confirm tracker mirror %d issue %s detail", rootID, d.IssueID)
	err = s.mirrorSweptWrite(rootID, d.sweep, action, func(tx *sql.Tx) error {
		var same bool
		var opened int64
		err := tx.QueryRow(mirrorDetailConfirmSQL, rootID, d.IssueID, d.row, d.ReadAt).Scan(&same, &opened)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("read detail: %w", err)
		case !same:
			return nil
		}
		result.Stored = true
		if opened >= openedAt {
			return nil
		}
		if _, err := tx.Exec(mirrorDetailOpenSQL, rootID, d.IssueID, openedAt); err != nil {
			return fmt.Errorf("record the opening: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, ErrTrackerCacheSwept):
		return MirrorDetailWrite{}, nil
	case err != nil:
		return MirrorDetailWrite{}, err
	}
	return result, nil
}

// MirrorDetailDoc is one issue's detail document as a read found it, with
// the update time of the card the document shows (CardUpdatedAt), which
// fences it against the mirror's card (StoreMirrorDetails).
type MirrorDetailDoc struct {
	IssueID       string
	Data          []byte
	CardUpdatedAt int64
}

// StoreMirrorDetail is StoreMirrorDetails of one document, returning its
// outcome alone: a caller that must learn which other details the write
// dropped calls StoreMirrorDetails.
func (s *Store) StoreMirrorDetail(rootID, sweep int64, doc MirrorDetailDoc, opened, read MirrorRead) (MirrorDetailWrite, error) {
	out, err := s.StoreMirrorDetails(rootID, sweep, []MirrorDetailDoc{doc}, opened, read)
	if err != nil {
		return MirrorDetailWrite{}, err
	}
	return out.Docs[0], nil
}

// StoreMirrorDetails stores, in one transaction, the documents of docs
// (each issue at most once) that one read begun at readAt found, each for
// an issue the mirror keeps, unless a read that began later was admitted
// for it (Superseded when that read stored another document); the same
// document from a later read moves its read time alone. openedAt is when
// a person opened or refreshed the issues (zero: no person asked for the
// read). Only an opening starts a detail: a write that is none stores
// nothing for an issue whose detail the mirror does not hold, and one
// that starts it stores its document only when its read began no earlier
// than the opening (Superseded otherwise), so a document read before its
// detail was dropped (the cap) or swept does not come back. sweep is the
// root's sweep captured before the read (ErrTrackerCacheSwept).
//
// A document writes no card: a card has one source, a cards or rows read
// (storeMirrorCardsTx), which owns what a card brings with it (names,
// sprints, the marks that make a board due a structure read, the bounds
// and eligibility its provider applies). A document is fenced by its
// card's update time instead, the one order a document and a card share.
// A document of the update the mirror's card has is stored; one of a
// later update, or of an issue the mirror holds no card of, is owed its
// card (CardOwed) and not stored, so the caller reads the card and stores
// the document again; one of an earlier update is stale and not stored
// (Stored false), its opening alone recorded. A document and the mirror's
// card of the same update time can still differ (a project key rename
// moves no update time): the card owns the fields it holds (key, title,
// status, assignee, priority, labels, parent, sprints, type and times),
// and the document owns the rest, so a ticket shows the card's fields
// beside the document's (trackerapp shownDetailOf). The fence compares
// update times alone, not read times: a card read owed to a document
// begins after the document's read, and the card rule's tie on read times
// would refuse every document it was owed for.
//
// One statement reads every document's state with its card's update time,
// one moves the read times of the documents found the same, and each
// other document costs at most one write; a write that started a detail
// then drops, in one statement after every document is written, those
// beyond the mirrorDetailCap most recently opened, and the outcomes are of
// the committed state: a document the cap dropped, by an opening of the
// same write or another, is not Stored.
func (s *Store) StoreMirrorDetails(rootID, sweep int64, docs []MirrorDetailDoc, opened, read MirrorRead) (out MirrorDetailWrites, err error) {
	openedAt, readAt := opened.at, read.at
	if len(docs) == 0 || readAt <= 0 || openedAt < 0 || sweep <= 0 {
		return MirrorDetailWrites{}, fmt.Errorf("%w: a detail write needs documents, the time their read began and the root's sweep", ErrInvalidTrackerCache)
	}
	for i, d := range docs {
		if d.IssueID == "" || slices.ContainsFunc(docs[:i], func(o MirrorDetailDoc) bool { return o.IssueID == d.IssueID }) {
			return MirrorDetailWrites{}, fmt.Errorf("%w: a detail write names each of its issues once", ErrInvalidTrackerCache)
		}
		if d.CardUpdatedAt <= 0 {
			return MirrorDetailWrites{}, fmt.Errorf("%w: the document of issue %s names no card update time", ErrInvalidTrackerCache, d.IssueID)
		}
	}
	action := fmt.Sprintf("store: store tracker mirror %d details of %d issues", rootID, len(docs))
	err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
		out = MirrorDetailWrites{Docs: make([]MirrorDetailWrite, len(docs))}
		states, err := readMirrorDetailStatesTx(tx, rootID, docs)
		if err != nil {
			return err
		}
		started := make([]bool, len(docs))
		var moved []string
		for i, d := range docs {
			var move bool
			if started[i], move, err = storeMirrorDetailTx(tx, rootID, d, openedAt, readAt, states[i], &out.Docs[i]); err != nil {
				return fmt.Errorf("issue %s: %w", d.IssueID, err)
			}
			if move {
				moved = append(moved, d.IssueID)
			}
		}
		if len(moved) > 0 {
			list, err := encodeMirrorIDs(moved)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(mirrorDetailMoveSQL, rootID, list, readAt, openedAt); err != nil {
				return fmt.Errorf("record the details' read: %w", err)
			}
		}
		if !slices.Contains(started, true) {
			return nil
		}
		dropped, err := queryIDs(tx, mirrorDetailEvictSQL, rootID)
		if err != nil {
			return fmt.Errorf("drop details: %w", err)
		}
		for i, d := range docs {
			if !slices.Contains(dropped, d.IssueID) {
				continue
			}
			out.Docs[i].Stored, out.Docs[i].Changed = false, false
			if started[i] {
				dropped = slices.DeleteFunc(dropped, func(id string) bool { return id == d.IssueID })
			}
		}
		if len(dropped) > 0 {
			slices.Sort(dropped)
			out.Dropped = dropped
		}
		return nil
	})
	if err != nil {
		return MirrorDetailWrites{}, err
	}
	return out, nil
}

// storeMirrorDetailTx is StoreMirrorDetails of one document, whose detail
// is in state st, before the cap's eviction, reporting whether it started
// a detail the mirror did not hold and whether the held detail's read
// time or opening is to move, which the caller does for every such
// document in one statement.
func storeMirrorDetailTx(tx *sql.Tx, rootID int64, doc MirrorDetailDoc, openedAt, readAt int64, st mirrorDetailState,
	result *MirrorDetailWrite) (started, move bool, err error) {
	data := mirrorData(doc.Data)
	switch {
	case !st.kept || (!st.held && openedAt == 0):
		return false, false, nil
	case st.cardAt < doc.CardUpdatedAt:
		// No card, or an older one: the caller reads the card first.
		result.CardOwed = true
		return false, false, nil
	case st.cardAt > doc.CardUpdatedAt:
		// A stale document. A person's opening of it still counts, so the
		// detail is not the first the cap drops, nor lost when the
		// mirror held none: a detail started with no document, which the
		// next read stores.
		switch {
		case openedAt == 0:
			return false, false, nil
		case st.held:
			if _, err := tx.Exec(mirrorDetailOpenSQL, rootID, doc.IssueID, openedAt); err != nil {
				return false, false, fmt.Errorf("record the opening: %w", err)
			}
			return false, false, nil
		}
		if _, err := tx.Exec(mirrorDetailStoreSQL, rootID, doc.IssueID, readAt, nil, openedAt); err != nil {
			return false, false, fmt.Errorf("record the opening: %w", err)
		}
		return true, false, nil
	case st.held && (st.readAt > readAt || !st.differs):
		// A later read's document, or the same one: the opening and the
		// read time move forward, a narrow write that the opened index
		// reads only when the opening moves.
		result.Stored, result.Superseded = true, st.readAt > readAt && st.differs
		return false, st.readAt < readAt || st.openedAt < openedAt, nil
	}
	if _, err := tx.Exec(mirrorDetailStoreSQL, rootID, doc.IssueID, readAt, data, openedAt); err != nil {
		return false, false, fmt.Errorf("write detail: %w", err)
	}
	result.Stored, result.Changed = true, st.held || readAt >= openedAt
	result.Superseded = !result.Changed
	return !st.held, false, nil
}

// GetMirrorDetail reads the stored document of an issue the mirror keeps,
// if it has one, with the update time of the mirror's card of the issue,
// in one statement.
func (s *Store) GetMirrorDetail(rootID int64, issueID string) (MirrorDetail, bool, error) {
	d := MirrorDetail{IssueID: issueID}
	err := s.reader().QueryRow(`SELECT d.rowid, r.sweep, d.opened_at, d.read_at, d.data, i.updated_at FROM tracker_cache_issue_details d
		JOIN tracker_cache_issues i ON i.root_id = d.root_id AND i.issue_id = d.issue_id
		JOIN tracker_cache_roots r ON r.id = d.root_id
		WHERE d.root_id = ?1 AND d.issue_id = ?2 AND d.data IS NOT NULL AND `+mirrorIssueKept("?1", "?2"),
		rootID, issueID).Scan(&d.row, &d.sweep, &d.OpenedAt, &d.ReadAt, &d.Data, &d.CardUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorDetail{}, false, nil
	}
	if err != nil {
		return MirrorDetail{}, false, fmt.Errorf("store: read tracker mirror %d issue %s detail: %w", rootID, issueID, err)
	}
	return d, true, nil
}

// mirrorDetailStatesSQL is the stored detail of each issue of the JSON
// list ?2 that the mirror keeps, without its document, probing the
// details' key per issue.
var mirrorDetailStatesSQL = `SELECT d.issue_id, d.rowid, r.sweep, d.opened_at, d.read_at, i.updated_at
	FROM json_each(?2) l
	CROSS JOIN tracker_cache_issue_details d ON d.root_id = ?1 AND d.issue_id = l.value
	JOIN tracker_cache_issues i ON i.root_id = d.root_id AND i.issue_id = d.issue_id
	JOIN tracker_cache_roots r ON r.id = d.root_id
	WHERE d.data IS NOT NULL AND ` + mirrorIssueKept("d.root_id", "d.issue_id")

// ListMirrorDetailStates is GetMirrorDetail of each of issueIDs (at most
// mirrorMaxLookup) without the documents, in one statement: the details
// the mirror holds, in no order, each as ConfirmMirrorDetail names it.
func (s *Store) ListMirrorDetailStates(rootID int64, issueIDs []string) ([]MirrorDetail, error) {
	switch {
	case len(issueIDs) == 0:
		return nil, nil
	case len(issueIDs) > mirrorMaxLookup:
		return nil, fmt.Errorf("%w: %d issues, over %d", ErrInvalidTrackerCache, len(issueIDs), mirrorMaxLookup)
	}
	list, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return nil, err
	}
	var out []MirrorDetail
	err = scanMirrorRows(s.reader(), mirrorDetailStatesSQL, []any{rootID, list}, func(r rowScanner) error {
		var d MirrorDetail
		if err := r.Scan(&d.IssueID, &d.row, &d.sweep, &d.OpenedAt, &d.ReadAt, &d.CardUpdatedAt); err != nil {
			return err
		}
		out = append(out, d)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d details of %d issues: %w", rootID, len(issueIDs), err)
	}
	return out, nil
}
