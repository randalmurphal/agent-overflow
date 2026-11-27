package store

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// The sources of an issue's acts (tracker_cache_issue_acts.source).
const (
	// MirrorActComment is a comment: when it was made, by whom.
	MirrorActComment = 1
	// MirrorActWorklog is a worklog: when the work started, by whom, and
	// the seconds spent.
	MirrorActWorklog = 2
)

// MirrorAct is one comment or worklog of an issue as credit counts it:
// its ID within the issue and source, its author's account (Person), when
// it was made (At, Unix ms: a comment's creation, a worklog's start) and a
// worklog's seconds (zero for a comment).
type MirrorAct struct {
	ID      string
	Person  string
	At      int64
	Seconds int64
}

// MirrorIssueActs is the whole of an issue's acts of one source as one
// read found them from From (Unix ms; 0 for every one, which a comment's
// read always is), and the issue's update time that read returned
// (ReadFor), which its receipt records.
type MirrorIssueActs struct {
	IssueID string
	Source  int
	ReadFor int64
	From    int64
	Acts    []MirrorAct
}

// MirrorActsAttempt is a read of an issue's acts of one source that
// failed for that issue alone, and why, in words a person can act on.
type MirrorActsAttempt struct {
	IssueID string
	Source  int
	Reason  string
}

// mirrorActsKeyRow is a MirrorIssueActs or a MirrorActsAttempt, and
// mirrorActRow a MirrorAct, as the acts statements read them from
// json_each.
type mirrorActsKeyRow struct {
	Issue   string `json:"i"`
	Source  int    `json:"s"`
	ReadFor int64  `json:"f,omitempty"`
	From    int64  `json:"o,omitempty"`
	Reason  string `json:"r,omitempty"`
}

type mirrorActRow struct {
	Issue   string `json:"i"`
	Source  int    `json:"s"`
	ID      string `json:"a"`
	Person  string `json:"p"`
	At      int64  `json:"t"`
	Seconds int64  `json:"x"`
}

// The acts statements, of ?1 root, and except where one says otherwise ?2
// the read's order time, ?3 the JSON list of the issues and sources
// replaced or attempted (mirrorActsKeyRow), ?4 the JSON list of the
// replaced ones' acts (mirrorActRow), ?5 the read's wall time. Each finds
// an issue's rows by the primary key's prefix.
var (
	// mirrorActsStateSQL reads, for each listed issue and source, whether
	// the mirror keeps the issue, the read order of its receipt (0 for
	// none), the card update and the worklogs' start it is for, whether a
	// read failed since, and how many acts it holds.
	mirrorActsStateSQL = `SELECT e.value ->> 'i', e.value ->> 's',
	    iif(i.issue_id IS NULL, 0, ` + mirrorIssueKept("i.root_id", "i.issue_id") + `), coalesce(r.read_at, 0), coalesce(r.read_for, 0), coalesce(r.read_from, 0),
	    coalesce(r.tried_at, 0) > 0,
	    (SELECT count(*) FROM tracker_cache_issue_acts a WHERE a.root_id = CAST(?1 AS INTEGER) AND a.issue_id = e.value ->> 'i' AND a.source = e.value ->> 's')
	  FROM json_each(?3) e
	  LEFT JOIN tracker_cache_issues i ON i.root_id = CAST(?1 AS INTEGER) AND i.issue_id = e.value ->> 'i'
	  LEFT JOIN tracker_cache_issue_act_reads r ON r.root_id = CAST(?1 AS INTEGER) AND r.issue_id = e.value ->> 'i'
	    AND r.source = e.value ->> 's'`
	// mirrorActsRemoveSQL removes the acts of issue ?2's source ?3 that
	// the read did not find, ?4 the JSON list of their IDs, which SQLite
	// reads once into an index: each stored act is one probe of it.
	mirrorActsRemoveSQL = `DELETE FROM tracker_cache_issue_acts
	  WHERE root_id = CAST(?1 AS INTEGER) AND issue_id = ?2 AND source = ` + intParam("?3") + `
	    AND act_id NOT IN (SELECT value FROM json_each(?4))`
	mirrorActsUpsertSQL = `INSERT INTO tracker_cache_issue_acts(root_id, issue_id, source, act_id, person, at, seconds)
	  SELECT ?1, a.value ->> 'i', a.value ->> 's', a.value ->> 'a', a.value ->> 'p', a.value ->> 't', a.value ->> 'x' FROM json_each(?4) a WHERE true
	  ON CONFLICT(root_id, issue_id, source, act_id) DO UPDATE SET person = excluded.person, at = excluded.at, seconds = excluded.seconds
	  WHERE person IS NOT excluded.person OR at IS NOT excluded.at OR seconds IS NOT excluded.seconds
	  RETURNING issue_id`
	// mirrorActsCreditSQL advances the totals revision of each board of
	// root ?1 that holds a row of an issue of the JSON list ?2, each row
	// found by key, once a board: credit, which the Backlog's pace counts
	// (readMirrorPaceTx), read the issues' evidence.
	mirrorActsCreditSQL = `UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1
	  WHERE root_id = CAST(?1 AS INTEGER) AND board_id IN (
	    SELECT b.board_id FROM json_each(?2) l
	    CROSS JOIN tracker_cache_board_issues b ON b.root_id = CAST(?1 AS INTEGER) AND b.issue_id = l.value)`
	// mirrorActReadsSQL records each listed read's receipt and clears an
	// attempt that failed before it.
	mirrorActReadsSQL = `INSERT INTO tracker_cache_issue_act_reads(root_id, issue_id, source, read_for, read_at, read_wall, read_from)
	  SELECT ?1, e.value ->> 'i', e.value ->> 's', e.value ->> 'f', ?2, ?5, coalesce(e.value ->> 'o', 0) FROM json_each(?3) e WHERE true
	  ON CONFLICT(root_id, issue_id, source) DO UPDATE SET read_for = excluded.read_for, read_at = excluded.read_at,
	    read_wall = excluded.read_wall, read_from = excluded.read_from, tried_at = 0, tried_reason = ''`
	// mirrorActAttemptsSQL records each listed failed attempt, beside the
	// receipt the row holds.
	mirrorActAttemptsSQL = `INSERT INTO tracker_cache_issue_act_reads(root_id, issue_id, source, tried_at, tried_reason)
	  SELECT ?1, e.value ->> 'i', e.value ->> 's', ?5, e.value ->> 'r' FROM json_each(?3) e WHERE true
	  ON CONFLICT(root_id, issue_id, source) DO UPDATE SET tried_at = excluded.tried_at, tried_reason = excluded.tried_reason`
)

// ReplaceMirrorActs makes each of issues the whole of its issue's acts of
// its source from its From, with a receipt of the read (begun at read)
// that found them, and records each of tried as a failed attempt of its
// issue's source at the read's wall time, which keeps the receipt the
// row holds. An issue the mirror does not keep stores nothing; one whose
// receipt is from a read that began at or after read is superseded and
// changes nothing. The authors' names (catalog) are stored as a history
// read's are. sourceGen is the time source generation of the root's
// connection the read began under (TrackerConnection.TimeSourceGen): the
// worklogs and the worklog attempts of a call under another are not
// stored, since the change deleted the root's worklog acts (migration
// v177); the rest is, and the call returns ErrTrackerCacheSourceChanged.
//
// It takes at most 2 × mirrorHistoryBatch issues and sources, each once
// across issues and tried. They are written in transactions of at most
// mirrorMaxBatchChanges acts, counting those stored and those read, an
// issue's acts of a source never split, the catalog in the first; a call
// that fails after a transaction committed has stored those before it.
func (s *Store) ReplaceMirrorActs(rootID, sourceGen int64, read MirrorRead, issues []MirrorIssueActs, tried []MirrorActsAttempt, catalog MirrorCatalog) (out MirrorActsWrite, err error) {
	times, err := read.in(rootID)
	if err != nil {
		return MirrorActsWrite{}, err
	}
	if times.at <= 0 || len(issues)+len(tried) > 2*mirrorHistoryBatch {
		return MirrorActsWrite{}, fmt.Errorf("%w: %d issues' acts and attempts, over %d, or no read", ErrInvalidTrackerCache, len(issues)+len(tried), 2*mirrorHistoryBatch)
	}
	if err := catalog.check(); err != nil {
		return MirrorActsWrite{}, err
	}
	if err := checkMirrorIssueActs(issues, tried); err != nil {
		return MirrorActsWrite{}, err
	}
	keys := make([]mirrorActsKeyRow, 0, len(issues)+len(tried))
	for _, in := range issues {
		keys = append(keys, mirrorActsKeyRow{Issue: in.IssueID, Source: in.Source, ReadFor: in.ReadFor, From: in.From})
	}
	for _, a := range tried {
		keys = append(keys, mirrorActsKeyRow{Issue: a.IssueID, Source: a.Source, Reason: a.Reason})
	}
	stored, err := s.storedMirrorActs(rootID, keys)
	if err != nil {
		return MirrorActsWrite{}, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %d issue acts", rootID)
	changedSource := false
	for start, first := 0, true; start < len(keys) || first; first = false {
		end, work := start, 0
		for end < len(keys) {
			w := stored[end]
			if end < len(issues) {
				w += len(issues[end].Acts)
			}
			if end > start && work+w > mirrorMaxBatchChanges {
				break
			}
			work += w
			end++
		}
		var changed MirrorCatalogWrite
		var credited []string
		err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
			var err error
			changed, credited = MirrorCatalogWrite{}, nil
			if first && len(catalog.Users)+len(catalog.Statuses) > 0 {
				if changed, err = upsertMirrorCatalogTx(tx, rootID, catalog, times.at); err != nil {
					return err
				}
			}
			var refused bool
			acts := issues[min(start, len(issues)):min(end, len(issues))]
			refused, credited, err = replaceMirrorActsTx(tx, rootID, sourceGen, times, keys[start:end], acts)
			changedSource = changedSource || refused
			return err
		})
		if err != nil {
			return out, err
		}
		out.Catalog.Changed = out.Catalog.Changed || changed.Changed
		out.Catalog.Threads = unionSorted(out.Catalog.Threads, changed.Threads)
		out.Credited = unionSorted(out.Credited, credited)
		start = end
	}
	if changedSource {
		return out, fmt.Errorf("worklogs read under time source generation %d: %w", sourceGen, ErrTrackerCacheSourceChanged)
	}
	return out, nil
}

// MirrorActsWrite is what a ReplaceMirrorActs call committed: its
// catalog's change, and Credited, the issues whose evidence credit reads
// changed (sorted), whose boards' totals it moved (mirrorActsCreditSQL),
// so their subscribers count credit again.
type MirrorActsWrite struct {
	Catalog  MirrorCatalogWrite
	Credited []string
}

// storedMirrorActs is how many acts the mirror holds of each of keys'
// issue and source, in keys' order: what a replace of it may remove.
func (s *Store) storedMirrorActs(rootID int64, keys []mirrorActsKeyRow) ([]int, error) {
	listed, err := encodeMirrorJSON(keys)
	if err != nil {
		return nil, err
	}
	at := make(map[string]int, len(keys))
	for i, k := range keys {
		at[k.Issue+"\x00"+strconv.Itoa(k.Source)] = i
	}
	return mirrorReadSnapshot(s, fmt.Sprintf("count tracker mirror %d stored acts", rootID), func(tx sqlQueryer) ([]int, error) {
		out := make([]int, len(keys))
		err := scanMirrorRows(tx, mirrorActsStateSQL, []any{rootID, nil, listed}, func(r rowScanner) error {
			var issue string
			var source, n int
			var kept, failed bool
			var readAt, readFor, readFrom int64
			if err := r.Scan(&issue, &source, &kept, &readAt, &readFor, &readFrom, &failed, &n); err != nil {
				return err
			}
			out[at[issue+"\x00"+strconv.Itoa(source)]] = n
			return nil
		})
		return out, err
	})
}

// checkMirrorIssueActs refuses an input with no issue, a source that is
// neither, no update time, a comment read from a start, an issue and
// source twice across issues and tried, an attempt with no reason, or an
// act with no ID or author, no time, negative seconds, a comment with
// seconds, or an ID twice within its issue and source.
func checkMirrorIssueActs(issues []MirrorIssueActs, tried []MirrorActsAttempt) error {
	type key struct {
		issue  string
		source int
	}
	seen := make(map[key]bool, len(issues)+len(tried))
	once := func(issue string, source int) error {
		k := key{issue, source}
		switch {
		case issue == "" || source != MirrorActComment && source != MirrorActWorklog:
			return fmt.Errorf("%w: issue %q source %d", ErrInvalidTrackerCache, issue, source)
		case seen[k]:
			return fmt.Errorf("%w: issue %s source %d twice", ErrInvalidTrackerCache, issue, source)
		}
		seen[k] = true
		return nil
	}
	for _, in := range issues {
		if err := once(in.IssueID, in.Source); err != nil {
			return err
		}
		if in.ReadFor <= 0 || in.From < 0 || in.Source == MirrorActComment && in.From != 0 {
			return fmt.Errorf("%w: issue %s source %d read for %d from %d", ErrInvalidTrackerCache, in.IssueID, in.Source, in.ReadFor, in.From)
		}
		ids := make(map[string]bool, len(in.Acts))
		for _, a := range in.Acts {
			if a.ID == "" || ids[a.ID] || a.Person == "" || a.At <= 0 || a.Seconds < 0 || in.Source == MirrorActComment && a.Seconds != 0 {
				return fmt.Errorf("%w: act %q of issue %s source %d: empty, repeated, or with no author, time or valid seconds",
					ErrInvalidTrackerCache, a.ID, in.IssueID, in.Source)
			}
			ids[a.ID] = true
		}
	}
	for _, a := range tried {
		if err := once(a.IssueID, a.Source); err != nil {
			return err
		}
		if a.Reason == "" {
			return fmt.Errorf("%w: the attempt of issue %s source %d names no reason", ErrInvalidTrackerCache, a.IssueID, a.Source)
		}
	}
	return nil
}

// replaceMirrorActsTx writes the admitted of keys in tx: the first
// len(issues) are issues' reads, the rest attempts. Under another time
// source generation than sourceGen it writes no worklog and reports
// refused. It advances the totals revision of each board holding an
// issue whose evidence it changed, once (mirrorActsCreditSQL), and
// returns those issues (credited, sorted): a read that stores changes it
// when it is the first, for another card update or worklogs' start, or
// after a failed attempt, or when an act was added, changed or removed;
// an attempt when it is the first to fail since the source's read
// (mirrorCreditCurrentSQL counts a source a read failed for since as not
// read).
func replaceMirrorActsTx(tx *sql.Tx, rootID, sourceGen int64, times mirrorReadTimes, keys []mirrorActsKeyRow, issues []MirrorIssueActs) (refused bool, credited []string, _ error) {
	if len(keys) == 0 {
		return false, nil, nil
	}
	if slices.ContainsFunc(keys, func(k mirrorActsKeyRow) bool { return k.Source == MirrorActWorklog }) {
		var current int64
		if err := tx.QueryRow(mirrorWorklogSourceGenSQL, rootID).Scan(&current); err != nil {
			return false, nil, fmt.Errorf("read the time source generation: %w", err)
		}
		refused = current != sourceGen
	}
	listed, err := encodeMirrorJSON(keys)
	if err != nil {
		return false, nil, err
	}
	index := make(map[string]int, len(keys))
	for i, k := range keys {
		index[k.Issue+"\x00"+strconv.Itoa(k.Source)] = i
	}
	admitted := make([]bool, len(keys))
	// moved holds the issues whose evidence credit reads changed.
	moved := map[string]bool{}
	err = scanMirrorRows(tx, mirrorActsStateSQL, []any{rootID, nil, listed}, func(r rowScanner) error {
		var issue string
		var source, n int
		var kept, failed bool
		var storedAt, storedFor, storedFrom int64
		if err := r.Scan(&issue, &source, &kept, &storedAt, &storedFor, &storedFrom, &failed, &n); err != nil {
			return err
		}
		i := index[issue+"\x00"+strconv.Itoa(source)]
		admitted[i] = kept && storedAt < times.at && (!refused || source != MirrorActWorklog)
		switch {
		case !admitted[i]:
		case i < len(issues) && (storedAt == 0 || failed || storedFor != keys[i].ReadFor || storedFrom != keys[i].From):
			moved[issue] = true
		case i >= len(issues) && !failed:
			moved[issue] = true
		}
		return nil
	})
	if err != nil {
		return refused, nil, fmt.Errorf("read the issues' acts state: %w", err)
	}
	var reads, attempts []mirrorActsKeyRow
	acts := []mirrorActRow{}
	for i, k := range keys {
		switch {
		case !admitted[i]:
		case i < len(issues):
			reads = append(reads, k)
			for _, a := range issues[i].Acts {
				acts = append(acts, mirrorActRow{Issue: k.Issue, Source: k.Source, ID: a.ID, Person: a.Person, At: a.At, Seconds: a.Seconds})
			}
		default:
			attempts = append(attempts, k)
		}
	}
	if len(reads) > 0 {
		listedReads, err := encodeMirrorJSON(reads)
		if err != nil {
			return refused, nil, err
		}
		encodedActs, err := encodeMirrorJSON(acts)
		if err != nil {
			return refused, nil, err
		}
		for i, k := range keys {
			if !admitted[i] || i >= len(issues) {
				continue
			}
			ids := make([]string, len(issues[i].Acts))
			for j, a := range issues[i].Acts {
				ids[j] = a.ID
			}
			found, err := encodeMirrorIDs(ids)
			if err != nil {
				return refused, nil, err
			}
			res, err := tx.Exec(mirrorActsRemoveSQL, rootID, k.Issue, k.Source, found)
			if err != nil {
				return refused, nil, fmt.Errorf("remove the acts of issue %s source %d: %w", k.Issue, k.Source, err)
			}
			removed, err := res.RowsAffected()
			if err != nil {
				return refused, nil, fmt.Errorf("count the acts removed of issue %s source %d: %w", k.Issue, k.Source, err)
			}
			if removed > 0 {
				moved[k.Issue] = true
			}
		}
		args := []any{rootID, times.at, listedReads, encodedActs, times.wall}
		err = scanMirrorRows(tx, mirrorActsUpsertSQL, args[:4], func(r rowScanner) error {
			var issue string
			err := r.Scan(&issue)
			moved[issue] = true
			return err
		})
		if err != nil {
			return refused, nil, fmt.Errorf("write acts: %w", err)
		}
		if _, err := tx.Exec(mirrorActReadsSQL, args...); err != nil {
			return refused, nil, fmt.Errorf("record the reads: %w", err)
		}
	}
	credited = slices.Sorted(maps.Keys(moved))
	if len(credited) > 0 {
		listedMoved, err := encodeMirrorIDs(credited)
		if err != nil {
			return refused, nil, err
		}
		if _, err := tx.Exec(mirrorActsCreditSQL, rootID, listedMoved); err != nil {
			return refused, nil, fmt.Errorf("advance the totals of the boards whose credit the evidence moved: %w", err)
		}
	}
	if len(attempts) > 0 {
		listedAttempts, err := encodeMirrorJSON(attempts)
		if err != nil {
			return refused, nil, err
		}
		if _, err := tx.Exec(mirrorActAttemptsSQL, rootID, times.at, listedAttempts, nil, times.wall); err != nil {
			return refused, nil, fmt.Errorf("record the attempts: %w", err)
		}
	}
	return refused, credited, nil
}

// RecordMirrorEvidenceOutcome records how the evidence unit's last read
// of root rootID's connection ended: failure, why it failed as a whole,
// at wall (Unix ms on the wall clock), or "" for a read that did not, which
// clears it. A failure of one ticket's source alone is its attempt
// (MirrorActsAttempt), not this. changed reports that the outcome the root
// holds changed: a failure that started, changed or cleared. A detached
// root fails with ErrTrackerCacheGone. Its one writer is the evidence
// unit, and its one reader the credit sentence (GetMirrorEvidenceFailure).
func (s *Store) RecordMirrorEvidenceOutcome(rootID int64, failure string, wall int64) (changed bool, err error) {
	if failure != "" && wall <= 0 {
		return false, fmt.Errorf("%w: an evidence failure with no time", ErrInvalidTrackerCache)
	}
	err = s.mirrorWrite(rootID, fmt.Sprintf("store: record tracker mirror %d evidence outcome", rootID), func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE tracker_cache_roots SET evidence_failure = ?2, evidence_failed_wall = iif(?2 = '', 0, ?3)
		  WHERE id = ?1 AND evidence_failure IS NOT ?2`, rootID, failure, wall)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		changed = n > 0
		return err
	})
	return changed, err
}

// GetMirrorEvidenceFailure is why the evidence unit's last read of root
// rootID's connection failed as a whole, "" while it did not
// (RecordMirrorEvidenceOutcome). A root gone or detached fails with
// ErrTrackerCacheGone.
func (s *Store) GetMirrorEvidenceFailure(rootID int64) (string, error) {
	var failure sql.NullString
	err := s.db.QueryRow(`SELECT evidence_failure FROM tracker_cache_roots WHERE id = ? AND connection_id IS NOT NULL`, rootID).Scan(&failure)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("store: read tracker mirror %d evidence outcome: %w", rootID, ErrTrackerCacheGone)
	case err != nil:
		return "", fmt.Errorf("store: read tracker mirror %d evidence outcome: %w", rootID, err)
	}
	return failure.String, nil
}
