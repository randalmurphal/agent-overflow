package store

import (
	"encoding/json"
	"fmt"
	"slices"
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
// creation). For is the issue's update time the read was made for, its
// MirrorHistoryDue.UpdatedAt. AnsweredAt is when the answer arrived and
// Wait how long after it the issue is due again, both by the caller's
// steady clock, Wait zero when the read is final for For
// (mirrorHistoryDue); the store compares them with nothing but the
// caller's now. BoardsRev is the issue's MirrorHistoryDue.BoardsRev the
// read was asked with: one read for an earlier set than the issue's
// leaves it due. Receipt is the issue's MirrorHistoryDue.Receipt the read
// was listed with: a history whose receipt no longer matches the issue's
// was listed before a later write or the cleanup's reset and is
// superseded, storing nothing: a write is admitted only against the state
// its read was listed from. A history naming an update time or a boards
// revision the issue has not reached is refused with
// ErrInvalidTrackerCache, the call storing nothing.
type MirrorHistory struct {
	IssueID          string
	CompleteFrom     int64
	For              int64
	AnsweredAt, Wait int64
	BoardsRev        int64
	Receipt          int64
	Changes          []MirrorChange
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
// written to, and its times. Changes reports that a statement before the
// times' changed its changes, so the times' advances its receipt too.
type mirrorHistoryRow struct {
	Issue     string `json:"i"`
	Gen       int64  `json:"g"`
	Counts    [4]int `json:"n"`
	From      int64  `json:"f"`
	For       int64  `json:"v"`
	Answered  int64  `json:"a"`
	Wait      int64  `json:"w"`
	BoardsRev int64  `json:"b"`
	Changes   bool   `json:"c,omitempty"`
}

// MirrorHistoryResult is what a history write did for one issue: whether
// its stored history changed, whether it was superseded (its receipt went
// stale), or whether the mirror does not keep the issue, so nothing was
// stored.
type MirrorHistoryResult struct {
	IssueID                     string
	Changed, Superseded, Unkept bool
}

// mirrorStatusSinceOf names, as SQL expressions, what the time an issue's
// status began derives from: the card (issue, status, update and creation
// times) and the stored history (the time it is complete from, the update
// time it was read for and its wait, and its generation).
type mirrorStatusSinceOf struct {
	issue, status, updatedAt, createdAt, from, readFor, wait, gen string
}

// mirrorStatusSinceSQL is the one rule for when a card's status began, by
// the stored history, in statements whose ?1 is the root: zero unless the
// history was read for the card's update time or a later one and is final
// for it (no wait); then the time of the
// history's last status change when that change led to the card's status,
// zero when it led elsewhere, and the issue's creation when the history is
// complete from creation and holds no status change. Every writer of
// either side derives it again (the card upsert, the history writes), so
// the order the two arrive in does not matter. The last status change is
// one key probe, newest first.
func mirrorStatusSinceSQL(of mirrorStatusSinceOf) string {
	return `CASE WHEN ` + of.updatedAt + ` > ` + of.readFor + ` OR ` + of.wait + ` > 0 THEN 0 ELSE coalesce(
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
	// admission needs (the issue's current boards_rev included) and how
	// many stored changes the history would remove.
	mirrorHistoryStateSQL = `SELECT h.value ->> 'i', i.history_from, i.history_for, i.history_answered_at, i.history_wait,
		i.history_boards_rev, i.history_receipt, i.boards_rev, i.updated_at, i.history_gen, i.history_dirty, ` +
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
	// mirrorHistoryTimesSQL writes each listed issue's times, derives its
	// status time from its stored history and advances its receipt when
	// they or its changes changed, finding each issue by key from the
	// list; an UPDATE FROM the list would walk the root's issues.
	mirrorHistoryTimesSQL = `UPDATE tracker_cache_issues
		SET (history_from, history_for, history_answered_at, history_wait, history_boards_rev, status_since, history_receipt) = (` +
		mirrorHistoryTimesOf(", tracker_cache_issues.history_receipt + 1") + `)
		WHERE root_id = ?1 AND issue_id IN (SELECT value ->> 'i' FROM json_each(?2))
		  AND ((history_from, history_for, history_answered_at, history_wait, history_boards_rev, status_since) IS NOT (` +
		mirrorHistoryTimesOf("") + `)
		    OR coalesce((SELECT h.value ->> 'c' FROM json_each(?2) h WHERE h.value ->> 'i' = tracker_cache_issues.issue_id), 0))
		RETURNING issue_id`
)

// mirrorHistoryTimesOf reads the times the list holds for the issue being
// updated, the status time they give its card, and then extra.
func mirrorHistoryTimesOf(extra string) string {
	return `SELECT h.value ->> 'f', h.value ->> 'v', h.value ->> 'a', h.value ->> 'w', h.value ->> 'b', ` +
		mirrorStatusSinceSQL(mirrorStatusSinceOf{
			issue: "tracker_cache_issues.issue_id", status: "tracker_cache_issues.status_id", updatedAt: "tracker_cache_issues.updated_at",
			createdAt: "tracker_cache_issues.created_at", from: "(h.value ->> 'f')", readFor: "(h.value ->> 'v')", wait: "(h.value ->> 'w')",
			gen: "tracker_cache_issues.history_gen"}) + extra + ` FROM json_each(?2) h WHERE h.value ->> 'i' = tracker_cache_issues.issue_id`
}

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
	if h.IssueID == "" || h.For <= 0 || h.CompleteFrom < 0 || h.BoardsRev < 0 || h.AnsweredAt <= 0 || h.Wait < 0 || h.Receipt < 0 {
		return mirrorHistoryInput{}, fmt.Errorf("%w: a history needs its issue, the update time it was read for, "+
			"the time its answer arrived, a wait of zero or more and its receipt", ErrInvalidTrackerCache)
	}
	in := mirrorHistoryInput{h: h, row: mirrorHistoryRow{Issue: h.IssueID, From: h.CompleteFrom, For: h.For, Answered: h.AnsweredAt,
		Wait: h.Wait, BoardsRev: h.BoardsRev},
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
