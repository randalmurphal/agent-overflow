package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// Change kinds, aligned with tracker_cache_changes.kind through
// mirrorChangeKinds.
const (
	MirrorChangeStatus   = "status"
	MirrorChangeSprint   = "sprint"
	MirrorChangeEstimate = "estimate"
	MirrorChangeAssignee = "assignee"
)

// mirrorChangeKinds numbers the change kinds as tracker_cache_changes.kind
// stores them.
var mirrorChangeKinds = map[string]int{MirrorChangeStatus: 1, MirrorChangeSprint: 2, MirrorChangeEstimate: 3, MirrorChangeAssignee: 4}

const (
	// mirrorMaxChanges bounds one issue's stored changes, so replacing
	// them deletes at most a chunk: a longer history is stored from a
	// later CompleteFrom.
	mirrorMaxChanges = clearMirrorChunk
	// mirrorHistoryDuePage bounds one ListMirrorHistoryDue read: two read
	// batches of histories.
	mirrorHistoryDuePage = 100
)

// MirrorChange is one change of an issue: when, by whom (empty for the
// tracker itself), and its values, by kind:
//   - status, assignee: From and To are IDs, empty for none;
//   - sprint: FromSprints and ToSprints are the whole sets;
//   - estimate: BoardID is the board whose estimate changed, FromNumber
//     and ToNumber its values, nil for none.
type MirrorChange struct {
	Kind                   string
	At                     int64
	AuthorID               string
	From, To               string
	FromSprints, ToSprints []string
	BoardID                string
	FromNumber, ToNumber   *float64
}

// MirrorHistory is one issue's changes, each kind oldest first, at most
// mirrorMaxChanges. They are complete from CompleteFrom (zero: from the
// issue's creation) and final through CompleteTo, which is always set.
type MirrorHistory struct {
	IssueID                  string
	CompleteFrom, CompleteTo int64
	Changes                  []MirrorChange
}

func (c MirrorChange) check() error {
	if c.At <= 0 {
		return fmt.Errorf("%w: a %s change needs its time", ErrInvalidTrackerCache, c.Kind)
	}
	ids := c.From != "" || c.To != ""
	sprints := c.FromSprints != nil || c.ToSprints != nil
	estimate := c.BoardID != "" || c.FromNumber != nil || c.ToNumber != nil
	var ok bool
	switch c.Kind {
	case MirrorChangeStatus, MirrorChangeAssignee:
		ok = !sprints && !estimate
	case MirrorChangeSprint:
		ok = !ids && !estimate
	case MirrorChangeEstimate:
		ok = !ids && !sprints && c.BoardID != ""
	}
	if !ok {
		return fmt.Errorf("%w: a %q change with values of another kind", ErrInvalidTrackerCache, c.Kind)
	}
	return nil
}

// values are the change's from_value and to_value columns.
func (c MirrorChange) values() (from, to string, err error) {
	if c.Kind != MirrorChangeSprint {
		return c.From, c.To, nil
	}
	encode := func(set []string) (string, error) {
		sorted := slices.Sorted(slices.Values(set))
		if sorted == nil {
			sorted = []string{}
		}
		encoded, err := json.Marshal(sorted)
		return string(encoded), err
	}
	if from, err = encode(c.FromSprints); err != nil {
		return "", "", fmt.Errorf("encode sprints: %w", err)
	}
	if to, err = encode(c.ToSprints); err != nil {
		return "", "", fmt.Errorf("encode sprints: %w", err)
	}
	return from, to, nil
}

// mirrorChangeRow is a change as the history statements read it from
// JSON, with its issue.
type mirrorChangeRow struct {
	Issue      string   `json:"i"`
	Kind       int      `json:"k"`
	Seq        int      `json:"q"`
	At         int64    `json:"t"`
	Author     string   `json:"a"`
	Board      string   `json:"b"`
	From       string   `json:"f"`
	To         string   `json:"o"`
	FromNumber *float64 `json:"x"`
	ToNumber   *float64 `json:"y"`
}

// mirrorHistoryRow is one issue's history as the history statements read
// it: its change count per kind (index kind-1), and the times to store.
type mirrorHistoryRow struct {
	Issue       string `json:"i"`
	Counts      [4]int `json:"n"`
	From        int64  `json:"f"`
	To          int64  `json:"t"`
	StatusSince int64  `json:"s"`
}

// MirrorHistoryResult is what a history write did for one issue: whether
// its stored history changed, whether it was superseded by one final
// through a later time, or whether the mirror does not keep the issue, so
// nothing was stored.
type MirrorHistoryResult struct {
	IssueID                     string
	Changed, Superseded, Unkept bool
}

const (
	// mirrorHistoryBatch bounds the histories one ReplaceMirrorHistories
	// call takes, and mirrorMaxBatchChanges their changes together.
	mirrorHistoryBatch    = 50
	mirrorMaxBatchChanges = 2000
)

// The history statements read ?1 root and ?2 the JSON list of
// mirrorHistoryRow; the list drives each (CROSS JOIN), probing by key.
var (
	// mirrorHistoryStateSQL reads, per listed issue the mirror holds, what
	// admission and status_since need, and how many stored changes the
	// history would remove.
	mirrorHistoryStateSQL = `SELECT h.value ->> 'i', i.history_to, i.updated_at, i.status_id, i.created_at, ` +
		mirrorIssueKept("i.root_id", "i.issue_id") + `,
		  (SELECT count(*) FROM tracker_cache_changes c WHERE c.root_id = ?1 AND c.issue_id = i.issue_id
		      AND c.seq >= h.value -> 'n' ->> (c.kind - 1))
		FROM json_each(?2) h CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = h.value ->> 'i'`
	mirrorChangesUpsertSQL = `INSERT INTO tracker_cache_changes(root_id, issue_id, kind, seq, at, author_id, board_id,
		        from_value, to_value, from_number, to_number)
		SELECT ?1, c.value ->> 'i', c.value ->> 'k', c.value ->> 'q', c.value ->> 't', c.value ->> 'a', c.value ->> 'b',
		       c.value ->> 'f', c.value ->> 'o', c.value ->> 'x', c.value ->> 'y' FROM json_each(?2) c WHERE true
		ON CONFLICT(root_id, issue_id, kind, seq) DO UPDATE SET at = excluded.at, author_id = excluded.author_id,
		    board_id = excluded.board_id, from_value = excluded.from_value, to_value = excluded.to_value,
		    from_number = excluded.from_number, to_number = excluded.to_number
		WHERE at IS NOT excluded.at OR author_id IS NOT excluded.author_id OR board_id IS NOT excluded.board_id
		   OR from_value IS NOT excluded.from_value OR to_value IS NOT excluded.to_value
		   OR from_number IS NOT excluded.from_number OR to_number IS NOT excluded.to_number
		RETURNING issue_id`
	mirrorChangesRemoveSQL = `DELETE FROM tracker_cache_changes WHERE root_id = ?1 AND (issue_id, kind, seq) IN (
		SELECT c.issue_id, c.kind, c.seq FROM json_each(?2) h
		  CROSS JOIN tracker_cache_changes c ON c.root_id = ?1 AND c.issue_id = h.value ->> 'i'
		 WHERE c.seq >= h.value -> 'n' ->> (c.kind - 1))
		RETURNING issue_id`
	// mirrorHistoryTimesSQL finds each issue by key from the list; an
	// UPDATE FROM the list would walk the root's issues instead.
	mirrorHistoryTimesSQL = `UPDATE tracker_cache_issues SET (history_from, history_to, status_since) = (` + mirrorHistoryTimesOfSQL + `)
		WHERE root_id = ?1 AND issue_id IN (SELECT value ->> 'i' FROM json_each(?2))
		  AND (history_from, history_to, status_since) IS NOT (` + mirrorHistoryTimesOfSQL + `)
		RETURNING issue_id`
)

// mirrorHistoryTimesOfSQL reads the times the list holds for the issue
// being updated.
const mirrorHistoryTimesOfSQL = `SELECT h.value ->> 'f', h.value ->> 't', h.value ->> 's' FROM json_each(?2) h
	WHERE h.value ->> 'i' = tracker_cache_issues.issue_id`

// mirrorHistoryInput is one checked history, encoded.
type mirrorHistoryInput struct {
	h       MirrorHistory
	row     mirrorHistoryRow
	changes []mirrorChangeRow
}

func newMirrorHistoryInput(h MirrorHistory) (mirrorHistoryInput, error) {
	if h.IssueID == "" || h.CompleteTo <= 0 || h.CompleteFrom < 0 || len(h.Changes) > mirrorMaxChanges {
		return mirrorHistoryInput{}, fmt.Errorf("%w: a history needs its issue, the time it is final through and at most %d changes",
			ErrInvalidTrackerCache, mirrorMaxChanges)
	}
	in := mirrorHistoryInput{h: h, row: mirrorHistoryRow{Issue: h.IssueID, From: h.CompleteFrom, To: h.CompleteTo},
		changes: make([]mirrorChangeRow, len(h.Changes))}
	for i, c := range h.Changes {
		if err := c.check(); err != nil {
			return mirrorHistoryInput{}, err
		}
		from, to, err := c.values()
		if err != nil {
			return mirrorHistoryInput{}, err
		}
		kind := mirrorChangeKinds[c.Kind]
		in.changes[i] = mirrorChangeRow{Issue: h.IssueID, Kind: kind, Seq: in.row.Counts[kind-1], At: c.At, Author: c.AuthorID,
			Board: c.BoardID, From: from, To: to, FromNumber: c.FromNumber, ToNumber: c.ToNumber}
		in.row.Counts[kind-1]++
	}
	return in, nil
}

// ReplaceMirrorHistories makes each history the whole stored history of
// its issue, with the time the issue's current status began, and returns
// each issue's outcome in input order. A history final through an earlier
// time than the stored one is superseded and changes nothing; an issue the
// mirror does not keep stores nothing (Unkept). It takes at most
// mirrorHistoryBatch histories of distinct issues, mirrorMaxBatchChanges
// changes in all, and writes them in one transaction of four statements,
// unless the changes they would remove pass clearMirrorChunk: then it
// writes them in as many transactions as keep each within it.
func (s *Store) ReplaceMirrorHistories(rootID int64, histories []MirrorHistory) ([]MirrorHistoryResult, error) {
	if len(histories) > mirrorHistoryBatch {
		return nil, fmt.Errorf("%w: %d histories, over %d", ErrInvalidTrackerCache, len(histories), mirrorHistoryBatch)
	}
	inputs := make([]mirrorHistoryInput, len(histories))
	results := make([]MirrorHistoryResult, len(histories))
	index := make(map[string]int, len(histories))
	total := 0
	for i, h := range histories {
		in, err := newMirrorHistoryInput(h)
		if err != nil {
			return nil, err
		}
		if _, ok := index[h.IssueID]; ok {
			return nil, fmt.Errorf("%w: issue %s has two histories", ErrInvalidTrackerCache, h.IssueID)
		}
		if total += len(h.Changes); total > mirrorMaxBatchChanges {
			return nil, fmt.Errorf("%w: over %d changes in one batch", ErrInvalidTrackerCache, mirrorMaxBatchChanges)
		}
		inputs[i], index[h.IssueID] = in, i
		results[i] = MirrorHistoryResult{IssueID: h.IssueID, Unkept: true}
	}
	action := fmt.Sprintf("store: replace tracker mirror %d histories", rootID)
	for pending := inputs; len(pending) > 0; {
		var err error
		err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
			pending, err = replaceMirrorHistoriesTx(tx, rootID, pending, index, results)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

// replaceMirrorHistoriesTx writes the admitted histories of pending whose
// removals fit in clearMirrorChunk, records each outcome in results, and
// returns the histories left for the next transaction.
func replaceMirrorHistoriesTx(tx *sql.Tx, rootID int64, pending []mirrorHistoryInput, index map[string]int,
	results []MirrorHistoryResult) ([]mirrorHistoryInput, error) {
	rows := make([]mirrorHistoryRow, len(pending))
	for i, in := range pending {
		rows[i] = in.row
	}
	listed, err := encodeMirrorJSON(rows)
	if err != nil {
		return nil, err
	}
	type state struct {
		storedTo, updatedAt, createdAt int64
		status                         string
		kept                           bool
		removes                        int
	}
	states := make(map[string]state, len(pending))
	err = scanMirrorRows(tx, mirrorHistoryStateSQL, []any{rootID, listed}, func(r rowScanner) error {
		var id string
		var st state
		err := r.Scan(&id, &st.storedTo, &st.updatedAt, &st.status, &st.createdAt, &st.kept, &st.removes)
		states[id] = st
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read issues: %w", err)
	}
	var write []mirrorHistoryRow
	var changes []mirrorChangeRow
	budget := clearMirrorChunk
	for n, in := range pending {
		st, ok := states[in.h.IssueID]
		result := &results[index[in.h.IssueID]]
		switch {
		case !ok || !st.kept:
			continue
		case in.h.CompleteTo < st.storedTo:
			result.Unkept, result.Superseded = false, true
			continue
		case st.removes > budget && len(write) > 0:
			return pending[n:], writeMirrorHistoriesTx(tx, rootID, write, changes, index, results)
		}
		result.Unkept = false
		budget -= st.removes
		row := in.row
		row.StatusSince = mirrorStatusSince(in.h, st.updatedAt, st.status, st.createdAt)
		write, changes = append(write, row), append(changes, in.changes...)
	}
	return nil, writeMirrorHistoriesTx(tx, rootID, write, changes, index, results)
}

func writeMirrorHistoriesTx(tx *sql.Tx, rootID int64, write []mirrorHistoryRow, changes []mirrorChangeRow, index map[string]int,
	results []MirrorHistoryResult) error {
	if len(write) == 0 {
		return nil
	}
	histories, err := encodeMirrorJSON(write)
	if err != nil {
		return err
	}
	var rows string
	if len(changes) > 0 {
		if rows, err = encodeMirrorJSON(changes); err != nil {
			return err
		}
	}
	for _, step := range []struct{ what, query, list string }{
		{"write changes", mirrorChangesUpsertSQL, rows},
		{"remove changes", mirrorChangesRemoveSQL, histories},
		{"write history times", mirrorHistoryTimesSQL, histories},
	} {
		if step.list == "" {
			continue
		}
		changed, err := queryIDs(tx, step.query, rootID, step.list)
		if err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
		for _, id := range changed {
			results[index[id]].Changed = true
		}
	}
	return nil
}

// mirrorStatusSince is when the card's status began by history h, zero
// when h does not show it. The card must be no newer than the time h is
// final through, so h covers it; then its status began with h's last
// status change when that change led to it, or at the issue's creation
// when h is complete from creation and changed no status.
func mirrorStatusSince(h MirrorHistory, cardUpdatedAt int64, cardStatus string, createdAt int64) int64 {
	if cardUpdatedAt > h.CompleteTo {
		return 0
	}
	for i := len(h.Changes) - 1; i >= 0; i-- {
		if c := h.Changes[i]; c.Kind == MirrorChangeStatus {
			if c.To == cardStatus {
				return c.At
			}
			return 0
		}
	}
	if h.CompleteFrom == 0 {
		return createdAt
	}
	return 0
}

// MirrorHistoryDue is an issue due a history read, with the boards the
// mirror shows it on, sorted (none for an issue only a link keeps).
type MirrorHistoryDue struct {
	IssueID string
	Boards  []string
}

// mirrorHistoryDueSQL states the predicate of
// idx_tracker_cache_issues_history_due, so the read walks that index.
var mirrorHistoryDueSQL = `SELECT i.issue_id,
	  (SELECT json_group_array(b.board_id) FROM tracker_cache_board_issues b
	    WHERE b.root_id = i.root_id AND b.issue_id = i.issue_id AND ` + mirrorBoardShown("b.root_id", "b.board_id") + `)
	FROM tracker_cache_issues i
	WHERE i.root_id = ?1 AND i.history_to < i.updated_at AND i.issue_id > ?2 AND ` + mirrorIssueKept("i.root_id", "i.issue_id") + `
	ORDER BY i.issue_id LIMIT ` + strconv.Itoa(mirrorHistoryDuePage+1)

// ListMirrorHistoryDue lists, by issue ID after after, up to
// mirrorHistoryDuePage kept issues of rootID whose update time is later
// than the time their stored history is final through, or that have none:
// the issues whose history sync reads next, each with its boards. more
// reports that issues follow the last one, so a walk of n pages is n reads.
func (s *Store) ListMirrorHistoryDue(rootID int64, after string) (due []MirrorHistoryDue, more bool, err error) {
	err = scanMirrorRows(s.reader(), mirrorHistoryDueSQL, []any{rootID, after}, func(r rowScanner) error {
		var d MirrorHistoryDue
		var boards []byte
		if err := r.Scan(&d.IssueID, &boards); err != nil {
			return err
		}
		if len(boards) > 2 {
			if err := json.Unmarshal(boards, &d.Boards); err != nil {
				return fmt.Errorf("decode issue %s boards: %w", d.IssueID, err)
			}
		}
		due = append(due, d)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("store: list tracker mirror %d history due: %w", rootID, err)
	}
	if len(due) > mirrorHistoryDuePage {
		return due[:mirrorHistoryDuePage], true, nil
	}
	return due, false, nil
}
