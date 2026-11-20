package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	// mirrorHistoryDuePage bounds one ListMirrorHistoryDue read: two read
	// batches of histories.
	mirrorHistoryDuePage = 100
	// mirrorHistoryBatch bounds the histories one ReplaceMirrorHistories
	// call takes.
	mirrorHistoryBatch = 50
	// mirrorMaxBatchChanges bounds the changes one batch transaction
	// writes; a history that does not fit is staged (replaceStagedHistory).
	mirrorMaxBatchChanges = 2000
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

// MirrorHistory is one issue's changes, each kind oldest first, of any
// number. They are complete from CompleteFrom (zero: from the issue's
// creation) and final through CompleteTo, which is always set. BoardsRev
// is the issue's MirrorHistoryDue.BoardsRev the read was asked with: a
// history read for an earlier set of boards than the stored one is
// superseded, and one read for an earlier set than the issue's leaves it
// due.
type MirrorHistory struct {
	IssueID                  string
	CompleteFrom, CompleteTo int64
	BoardsRev                int64
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
// JSON, with its issue and the generation it is written to.
type mirrorChangeRow struct {
	Issue      string   `json:"i"`
	Gen        int64    `json:"g"`
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
// it: its change count per kind (index kind-1), the generation it is
// written to, and its times.
type mirrorHistoryRow struct {
	Issue     string `json:"i"`
	Gen       int64  `json:"g"`
	Counts    [4]int `json:"n"`
	From      int64  `json:"f"`
	To        int64  `json:"t"`
	BoardsRev int64  `json:"b"`
}

// MirrorHistoryResult is what a history write did for one issue: whether
// its stored history changed, whether it was superseded by one final
// through a later time, or whether the mirror does not keep the issue, so
// nothing was stored.
type MirrorHistoryResult struct {
	IssueID                     string
	Changed, Superseded, Unkept bool
}

// mirrorStatusSinceOf names, as SQL expressions, what the time an issue's
// status began derives from: the card (issue, status, update and creation
// times) and the stored history (the times it is complete from and final
// through, and its generation).
type mirrorStatusSinceOf struct {
	issue, status, updatedAt, createdAt, from, to, gen string
}

// mirrorStatusSinceSQL is the one rule for when a card's status began, by
// the stored history, in statements whose ?1 is the root: zero unless the
// history is final through the card's update time; then the time of the
// history's last status change when that change led to the card's status,
// zero when it led elsewhere, and the issue's creation when the history is
// complete from creation and holds no status change. Every writer of
// either side derives it again (the card upsert, the history writes), so
// the order the two arrive in does not matter. The last status change is
// one key probe, newest first.
func mirrorStatusSinceSQL(of mirrorStatusSinceOf) string {
	return `CASE WHEN ` + of.updatedAt + ` > ` + of.to + ` THEN 0 ELSE coalesce(
	  (SELECT iif(sc.to_value = ` + of.status + `, sc.at, 0) FROM tracker_cache_changes sc
	    WHERE sc.root_id = ?1 AND sc.issue_id = ` + of.issue + ` AND sc.gen = ` + of.gen + ` AND sc.kind = 1
	    ORDER BY sc.seq DESC LIMIT 1),
	  iif(` + of.from + ` = 0, ` + of.createdAt + `, 0)) END`
}

// mirrorRemovalsSQL counts, capped at a chunk and one, the stored changes
// of issue i at generation i.history_gen that the history h, by its
// counts per kind, would remove: one key range per kind.
var mirrorRemovalsSQL = func() string {
	parts := make([]string, 4)
	for k := range parts {
		parts[k] = fmt.Sprintf(`(SELECT count(*) FROM (SELECT 1 FROM tracker_cache_changes c
		    WHERE c.root_id = ?1 AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = %d
		      AND c.seq >= h.value -> 'n' ->> %d LIMIT %d))`, k+1, k, clearMirrorChunk+1)
	}
	return strings.Join(parts, " + ")
}()

// The history statements read ?1 root and ?2 the JSON list of
// mirrorHistoryRow or mirrorChangeRow; the list drives each (CROSS JOIN),
// probing by key.
var (
	// mirrorHistoryStateSQL reads, per listed issue the mirror holds, what
	// admission needs and how many stored changes the history would remove.
	mirrorHistoryStateSQL = `SELECT h.value ->> 'i', i.history_from, i.history_to, i.history_boards_rev, i.history_gen, i.history_dirty, ` +
		mirrorIssueKept("i.root_id", "i.issue_id") + `, ` + mirrorRemovalsSQL + `
		FROM json_each(?2) h CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = h.value ->> 'i'`
	mirrorChangesUpsertSQL = `INSERT INTO tracker_cache_changes(root_id, issue_id, gen, kind, seq, at, author_id, board_id,
		        from_value, to_value, from_number, to_number)
		SELECT ?1, c.value ->> 'i', c.value ->> 'g', c.value ->> 'k', c.value ->> 'q', c.value ->> 't', c.value ->> 'a',
		       c.value ->> 'b', c.value ->> 'f', c.value ->> 'o', c.value ->> 'x', c.value ->> 'y' FROM json_each(?2) c WHERE true
		ON CONFLICT(root_id, issue_id, gen, kind, seq) DO UPDATE SET at = excluded.at, author_id = excluded.author_id,
		    board_id = excluded.board_id, from_value = excluded.from_value, to_value = excluded.to_value,
		    from_number = excluded.from_number, to_number = excluded.to_number
		WHERE at IS NOT excluded.at OR author_id IS NOT excluded.author_id OR board_id IS NOT excluded.board_id
		   OR from_value IS NOT excluded.from_value OR to_value IS NOT excluded.to_value
		   OR from_number IS NOT excluded.from_number OR to_number IS NOT excluded.to_number
		RETURNING issue_id`
	mirrorChangesRemoveSQL = `DELETE FROM tracker_cache_changes WHERE root_id = ?1 AND (issue_id, gen, kind, seq) IN (
		SELECT c.issue_id, c.gen, c.kind, c.seq FROM json_each(?2) h
		  CROSS JOIN tracker_cache_changes c ON c.root_id = ?1 AND c.issue_id = h.value ->> 'i' AND c.gen = h.value ->> 'g'
		 WHERE c.seq >= h.value -> 'n' ->> (c.kind - 1))
		RETURNING issue_id`
	// mirrorHistoryTimesSQL writes each listed issue's times and derives
	// its status time from its stored history, finding each issue by key
	// from the list; an UPDATE FROM the list would walk the root's issues.
	mirrorHistoryTimesSQL = `UPDATE tracker_cache_issues SET (history_from, history_to, history_boards_rev, status_since) = (` + mirrorHistoryTimesOfSQL + `)
		WHERE root_id = ?1 AND issue_id IN (SELECT value ->> 'i' FROM json_each(?2))
		  AND (history_from, history_to, history_boards_rev, status_since) IS NOT (` + mirrorHistoryTimesOfSQL + `)
		RETURNING issue_id`
)

// mirrorHistoryTimesOfSQL reads the times the list holds for the issue
// being updated, and the status time they give its card.
var mirrorHistoryTimesOfSQL = `SELECT h.value ->> 'f', h.value ->> 't', h.value ->> 'b', ` + mirrorStatusSinceSQL(mirrorStatusSinceOf{
	issue: "tracker_cache_issues.issue_id", status: "tracker_cache_issues.status_id", updatedAt: "tracker_cache_issues.updated_at",
	createdAt: "tracker_cache_issues.created_at", from: "(h.value ->> 'f')", to: "(h.value ->> 't')",
	gen: "tracker_cache_issues.history_gen"}) + ` FROM json_each(?2) h WHERE h.value ->> 'i' = tracker_cache_issues.issue_id`

// mirrorHistoryInput is one checked history, encoded at generation 0; a
// writer sets the generation it writes.
type mirrorHistoryInput struct {
	h       MirrorHistory
	row     mirrorHistoryRow
	changes []mirrorChangeRow
}

// rowAt returns in's row for generation gen.
func (in mirrorHistoryInput) rowAt(gen int64) mirrorHistoryRow {
	row := in.row
	row.Gen = gen
	return row
}

// chunkAt copies into buf, for generation gen, the clearMirrorChunk changes
// of in from start, or those left.
func (in mirrorHistoryInput) chunkAt(gen int64, start int, buf []mirrorChangeRow) []mirrorChangeRow {
	buf = append(buf[:0], in.changes[start:min(start+clearMirrorChunk, len(in.changes))]...)
	for i := range buf {
		buf[i].Gen = gen
	}
	return buf
}

// setGen sets the generation of in's own changes, for a batch write that
// writes them whole.
func (in mirrorHistoryInput) setGen(gen int64) {
	for i := range in.changes {
		in.changes[i].Gen = gen
	}
}

func newMirrorHistoryInput(h MirrorHistory) (mirrorHistoryInput, error) {
	if h.IssueID == "" || h.CompleteTo <= 0 || h.CompleteFrom < 0 || h.BoardsRev < 0 {
		return mirrorHistoryInput{}, fmt.Errorf("%w: a history needs its issue and the time it is final through", ErrInvalidTrackerCache)
	}
	in := mirrorHistoryInput{h: h, row: mirrorHistoryRow{Issue: h.IssueID, From: h.CompleteFrom, To: h.CompleteTo, BoardsRev: h.BoardsRev},
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
// mirrorHistoryBatch histories of distinct issues, of any length. The
// histories that fit are written in one transaction of four statements,
// in more when their changes pass mirrorMaxBatchChanges or what they
// remove passes clearMirrorChunk; a history that alone passes either is
// staged in transactions of at most clearMirrorChunk changes and made the
// issue's history in one more (replaceStagedHistory). No transaction
// deletes more than clearMirrorChunk changes, and no read sees part of a
// history. sweep is the root's sweep captured before the reads
// (ErrTrackerCacheSwept).
func (s *Store) ReplaceMirrorHistories(rootID, sweep int64, histories []MirrorHistory) ([]MirrorHistoryResult, error) {
	if len(histories) > mirrorHistoryBatch || sweep <= 0 {
		return nil, fmt.Errorf("%w: %d histories, over %d, or no sweep", ErrInvalidTrackerCache, len(histories), mirrorHistoryBatch)
	}
	inputs := make([]mirrorHistoryInput, len(histories))
	results := make([]MirrorHistoryResult, len(histories))
	index := make(map[string]int, len(histories))
	for i, h := range histories {
		in, err := newMirrorHistoryInput(h)
		if err != nil {
			return nil, err
		}
		if _, ok := index[h.IssueID]; ok {
			return nil, fmt.Errorf("%w: issue %s has two histories", ErrInvalidTrackerCache, h.IssueID)
		}
		inputs[i], index[h.IssueID] = in, i
		results[i] = MirrorHistoryResult{IssueID: h.IssueID, Unkept: true}
	}
	action := fmt.Sprintf("store: replace tracker mirror %d histories", rootID)
	var staged []mirrorHistoryInput
	for pending := inputs; len(pending) > 0; {
		var err error
		err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
			var stage []mirrorHistoryInput
			pending, stage, err = replaceMirrorHistoriesTx(tx, rootID, pending, index, results)
			staged = append(staged, stage...)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	if len(staged) == 0 {
		return results, nil
	}
	// A staged history runs with the cleanup's lock, so no cleanup deletes
	// its staging and no other staging of the issue runs meanwhile.
	s.mirrorCleanup.Lock()
	defer s.mirrorCleanup.Unlock()
	for _, in := range staged {
		if err := s.replaceStagedHistory(rootID, sweep, in, &results[index[in.h.IssueID]]); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// replaceMirrorHistoriesTx writes the admitted histories of pending that
// fit one transaction, records each outcome in results, and returns the
// histories left for the next transaction and those to stage: a history
// whose changes or removals alone pass a chunk, or whose issue holds other
// generations a staging left (history_dirty).
func replaceMirrorHistoriesTx(tx *sql.Tx, rootID int64, pending []mirrorHistoryInput, index map[string]int,
	results []MirrorHistoryResult) (next, staged []mirrorHistoryInput, err error) {
	rows := make([]mirrorHistoryRow, len(pending))
	for i, in := range pending {
		rows[i] = in.row
	}
	listed, err := encodeMirrorJSON(rows)
	if err != nil {
		return nil, nil, err
	}
	states, err := readMirrorHistoryStatesTx(tx, rootID, listed)
	if err != nil {
		return nil, nil, err
	}
	var write []mirrorHistoryRow
	var changes []mirrorChangeRow
	removeBudget, writeBudget := clearMirrorChunk, mirrorMaxBatchChanges
	for n, in := range pending {
		st, ok := states[in.h.IssueID]
		result := &results[index[in.h.IssueID]]
		switch {
		case !ok || !st.kept:
			continue
		case st.supersedes(in):
			result.Unkept, result.Superseded = false, true
			continue
		case st.dirty || len(in.changes) > clearMirrorChunk || st.removes > clearMirrorChunk:
			staged = append(staged, in)
			continue
		case (st.removes > removeBudget || len(in.changes) > writeBudget) && len(write) > 0:
			return pending[n:], staged, writeMirrorHistoriesTx(tx, rootID, write, changes, index, results)
		}
		result.Unkept = false
		removeBudget -= st.removes
		writeBudget -= len(in.changes)
		in.setGen(st.gen)
		write, changes = append(write, in.rowAt(st.gen)), append(changes, in.changes...)
	}
	return nil, staged, writeMirrorHistoriesTx(tx, rootID, write, changes, index, results)
}

// mirrorHistoryState is an issue's stored history as a write of it reads
// it.
type mirrorHistoryState struct {
	storedFrom, storedTo, storedBoardsRev, gen int64
	dirty, kept                                bool
	removes                                    int
}

// supersedes reports that the stored history is later than in: final
// through a later time, or read for a later set of the issue's boards.
func (st mirrorHistoryState) supersedes(in mirrorHistoryInput) bool {
	return in.h.CompleteTo < st.storedTo || in.h.BoardsRev < st.storedBoardsRev
}

// sameTimes reports that in has the stored history's times and boards.
func (st mirrorHistoryState) sameTimes(in mirrorHistoryInput) bool {
	return st.storedFrom == in.h.CompleteFrom && st.storedTo == in.h.CompleteTo && st.storedBoardsRev == in.h.BoardsRev
}

// readMirrorHistoryStatesTx reads mirrorHistoryStateSQL for the encoded
// list of mirrorHistoryRow, by issue; an issue the mirror lacks is absent.
func readMirrorHistoryStatesTx(q sqlQueryer, rootID int64, listed string) (map[string]mirrorHistoryState, error) {
	states := map[string]mirrorHistoryState{}
	err := scanMirrorRows(q, mirrorHistoryStateSQL, []any{rootID, listed}, func(r rowScanner) error {
		var id string
		var st mirrorHistoryState
		err := r.Scan(&id, &st.storedFrom, &st.storedTo, &st.storedBoardsRev, &st.gen, &st.dirty, &st.kept, &st.removes)
		states[id] = st
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read issues: %w", err)
	}
	return states, nil
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

// MirrorHistoryDue is an issue due a history read, with the boards the
// mirror shows it on, sorted (none for an issue only a link keeps), and the
// revision of that set the read names (MirrorHistory.BoardsRev).
type MirrorHistoryDue struct {
	IssueID   string
	Boards    []string
	BoardsRev int64
}

// mirrorHistoryDueSQL states the predicate of
// idx_tracker_cache_issues_history_due, so the read walks that index.
var mirrorHistoryDueSQL = `SELECT i.issue_id,
	  (SELECT json_group_array(b.board_id) FROM tracker_cache_board_issues b
	    WHERE b.root_id = i.root_id AND b.issue_id = i.issue_id AND ` + mirrorBoardShown("b.root_id", "b.board_id") + `), i.boards_rev
	FROM tracker_cache_issues i
	WHERE i.root_id = ?1 AND (i.history_to < i.updated_at OR i.history_boards_rev < i.boards_rev) AND i.issue_id > ?2 AND ` + mirrorIssueKept("i.root_id", "i.issue_id") + `
	ORDER BY i.issue_id LIMIT ` + strconv.Itoa(mirrorHistoryDuePage+1)

// ListMirrorHistoryDue lists, by issue ID after after, up to
// mirrorHistoryDuePage kept issues of rootID whose update time is later
// than the time their stored history is final through, that have none, or
// whose stored history was read for fewer of their boards' estimates:
// the issues whose history sync reads next, each with its boards. more
// reports that issues follow the last one, so a walk of n pages is n reads.
func (s *Store) ListMirrorHistoryDue(rootID int64, after string) (due []MirrorHistoryDue, more bool, err error) {
	err = scanMirrorRows(s.reader(), mirrorHistoryDueSQL, []any{rootID, after}, func(r rowScanner) error {
		var d MirrorHistoryDue
		var boards []byte
		if err := r.Scan(&d.IssueID, &boards, &d.BoardsRev); err != nil {
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
