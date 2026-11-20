package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// mirrorWorklogLimit bounds one worklog read.
	mirrorWorklogLimit = 1000
	// mirrorMaxWorklogDays bounds the days one worklog replace covers,
	// and clearMirrorChunk the worklogs it carries and the stored ones it
	// can remove.
	mirrorMaxWorklogDays = 31
)

// MirrorWorklog is time logged on an issue. Day is YYYY-MM-DD in the
// author's own zone; SpentSeconds is positive.
type MirrorWorklog struct {
	ID, IssueID, AuthorID, Day string
	SpentSeconds               int64
	Description                string
	UpdatedAt                  int64
}

// MirrorWorklogChanges are the worklogs a replace stored new or changed
// and those it removed, each sorted by ID, and the days it left as they
// were because a read that began later changed them, sorted.
type MirrorWorklogChanges struct {
	Changed, Removed, SupersededDays []string
}

// mirrorDays lists the days from fromDay to toDay, both included, which
// checkMirrorWorklogRange has checked.
func mirrorDays(fromDay, toDay string) []string {
	from, _ := time.Parse(time.DateOnly, fromDay)
	var days []string
	for day := from; day.Format(time.DateOnly) <= toDay; day = day.AddDate(0, 0, 1) {
		days = append(days, day.Format(time.DateOnly))
	}
	return days
}

func parseMirrorDay(day string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: day %q is not YYYY-MM-DD", ErrInvalidTrackerCache, day)
	}
	return t, nil
}

func checkMirrorWorklogRange(authorID, fromDay, toDay string, readAt int64, logs []MirrorWorklog) error {
	from, fromErr := parseMirrorDay(fromDay)
	to, toErr := parseMirrorDay(toDay)
	if err := errors.Join(fromErr, toErr); err != nil {
		return err
	}
	switch days := int(to.Sub(from).Hours()/24) + 1; {
	case authorID == "" || readAt <= 0 || days < 1:
		return fmt.Errorf("%w: worklogs need an author, a read time and days in order", ErrInvalidTrackerCache)
	case days > mirrorMaxWorklogDays || len(logs) > clearMirrorChunk:
		return fmt.Errorf("%w: %d days and %d worklogs, over %d and %d", ErrInvalidTrackerCache, days, len(logs), mirrorMaxWorklogDays, clearMirrorChunk)
	}
	seen := make(map[string]bool, len(logs))
	for _, w := range logs {
		if w.ID == "" || seen[w.ID] || w.IssueID == "" || w.AuthorID != authorID || w.Day < fromDay || w.Day > toDay ||
			w.SpentSeconds <= 0 || w.UpdatedAt < 0 {
			return fmt.Errorf("%w: worklog %q is repeated, incomplete, or not %s's from %s to %s",
				ErrInvalidTrackerCache, w.ID, authorID, fromDay, toDay)
		}
		if _, err := parseMirrorDay(w.Day); err != nil {
			return err
		}
		seen[w.ID] = true
	}
	return nil
}

type mirrorWorklogRow struct {
	ID          string `json:"i"`
	Issue       string `json:"s"`
	Day         string `json:"y"`
	Spent       int64  `json:"t"`
	Description string `json:"d"`
	UpdatedAt   int64  `json:"u"`
}

// The worklog statements: ?1 root, ?2 author, ?3 first day, ?4 last day,
// ?5 when the read began, ?6 the JSON list. A day is superseded when a
// read that began later replaced it (tracker_cache_worklog_days).
const (
	mirrorWorklogDaySupersededSQL = `EXISTS (SELECT 1 FROM tracker_cache_worklog_days wd
		WHERE wd.root_id = ?1 AND wd.author_id = ?2 AND wd.day = %s AND wd.read_at > ?5)`
)

var (
	mirrorWorklogSupersededDaysSQL = `SELECT day FROM tracker_cache_worklog_days
		WHERE root_id = ?1 AND author_id = ?2 AND day BETWEEN ?3 AND ?4 AND read_at > ?5 ORDER BY day`
	mirrorWorklogCountSQL = `SELECT count(*) FROM (SELECT 1 FROM tracker_cache_worklogs
		WHERE root_id = ?1 AND author_id = ?2 AND day BETWEEN ?3 AND ?4 LIMIT ` + strconv.Itoa(clearMirrorChunk+1) + `)`
	mirrorWorklogsUpsertSQL = `INSERT INTO tracker_cache_worklogs(root_id, worklog_id, issue_id, author_id, day,
		        spent_seconds, description, updated_at)
		SELECT ?1, w.value ->> 'i', w.value ->> 's', ?2, w.value ->> 'y', w.value ->> 't', w.value ->> 'd', w.value ->> 'u'
		  FROM json_each(?6) w
		 WHERE NOT ` + fmt.Sprintf(mirrorWorklogDaySupersededSQL, `w.value ->> 'y'`) + `
		   AND NOT EXISTS (SELECT 1 FROM tracker_cache_worklogs o WHERE o.root_id = ?1 AND o.worklog_id = w.value ->> 'i'
		                    AND ` + fmt.Sprintf(mirrorWorklogDaySupersededSQL, `o.day`) + `)
		ON CONFLICT(root_id, worklog_id) DO UPDATE SET issue_id = excluded.issue_id, author_id = excluded.author_id,
		    day = excluded.day, spent_seconds = excluded.spent_seconds, description = excluded.description,
		    updated_at = excluded.updated_at
		WHERE issue_id IS NOT excluded.issue_id OR author_id IS NOT excluded.author_id OR day IS NOT excluded.day
		   OR spent_seconds IS NOT excluded.spent_seconds OR description IS NOT excluded.description
		   OR updated_at IS NOT excluded.updated_at
		RETURNING worklog_id, day`
	mirrorWorklogsRemoveSQL = `DELETE FROM tracker_cache_worklogs
		WHERE root_id = ?1 AND author_id = ?2 AND day BETWEEN ?3 AND ?4
		  AND worklog_id NOT IN (SELECT w.value ->> 'i' FROM json_each(?6) w)
		  AND NOT ` + fmt.Sprintf(mirrorWorklogDaySupersededSQL, `tracker_cache_worklogs.day`) + `
		RETURNING worklog_id, day`
	// mirrorWorklogDaysSQL records the read as the latest of each day ?4
	// (a JSON list) it replaced, empty or unchanged days included.
	mirrorWorklogDaysSQL = `INSERT INTO tracker_cache_worklog_days(root_id, author_id, day, read_at)
		SELECT ?1, ?2, value, ?3 FROM json_each(?4) WHERE true
		ON CONFLICT(root_id, author_id, day) DO UPDATE SET read_at = excluded.read_at WHERE excluded.read_at > read_at`
)

// ReplaceMirrorWorklogs makes logs the whole of authorID's worklogs whose
// day is fromDay to toDay, both included, as a read that began at readAt
// found them. Every log must be the author's and inside the range; the
// range covers at most mirrorMaxWorklogDays days and clearMirrorChunk
// logs, and must hold at most clearMirrorChunk stored logs (a longer one
// is replaced in shorter ranges). Each day of the range records the read
// as its latest, whether it changed, stayed the same or is empty, so no
// read that began earlier repopulates it. A day a read that began later
// replaced is left as it was and reported in SupersededDays; a replace
// from the same read as the stored one writes nothing.
func (s *Store) ReplaceMirrorWorklogs(rootID int64, authorID, fromDay, toDay string, readAt int64, logs []MirrorWorklog) (changes MirrorWorklogChanges, err error) {
	if err := checkMirrorWorklogRange(authorID, fromDay, toDay, readAt, logs); err != nil {
		return MirrorWorklogChanges{}, err
	}
	rows := make([]mirrorWorklogRow, len(logs))
	for i, w := range logs {
		rows[i] = mirrorWorklogRow{ID: w.ID, Issue: w.IssueID, Day: w.Day, Spent: w.SpentSeconds, Description: w.Description, UpdatedAt: w.UpdatedAt}
	}
	encoded, err := encodeMirrorJSON(rows)
	if err != nil {
		return MirrorWorklogChanges{}, err
	}
	rangeDays, err := encodeMirrorIDs(mirrorDays(fromDay, toDay))
	if err != nil {
		return MirrorWorklogChanges{}, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %d worklogs of %s", rootID, authorID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		args := []any{rootID, authorID, fromDay, toDay, readAt, encoded}
		var stored int
		if err := tx.QueryRow(mirrorWorklogCountSQL, args[:4]...).Scan(&stored); err != nil {
			return fmt.Errorf("count stored worklogs: %w", err)
		}
		if stored > clearMirrorChunk {
			return fmt.Errorf("%w: %s to %s holds over %d worklogs; replace shorter ranges", ErrInvalidTrackerCache, fromDay, toDay, clearMirrorChunk)
		}
		var err error
		if changes.SupersededDays, err = queryIDs(tx, mirrorWorklogSupersededDaysSQL, args[:5]...); err != nil {
			return fmt.Errorf("read superseded days: %w", err)
		}
		for _, step := range []struct {
			what  string
			query string
			out   *[]string
		}{
			{"write worklogs", mirrorWorklogsUpsertSQL, &changes.Changed},
			{"remove worklogs", mirrorWorklogsRemoveSQL, &changes.Removed},
		} {
			err := scanMirrorRows(tx, step.query, args, func(r rowScanner) error {
				var id, day string
				err := r.Scan(&id, &day)
				*step.out = append(*step.out, id)
				return err
			})
			if err != nil {
				return fmt.Errorf("%s: %w", step.what, err)
			}
		}
		if _, err := tx.Exec(mirrorWorklogDaysSQL, rootID, authorID, readAt, rangeDays); err != nil {
			return fmt.Errorf("record the days' read: %w", err)
		}
		return nil
	})
	if err != nil {
		return MirrorWorklogChanges{}, err
	}
	slices.Sort(changes.Changed)
	slices.Sort(changes.Removed)
	return changes, nil
}

const mirrorWorklogColumns = `w.worklog_id, w.issue_id, w.author_id, w.day, w.spent_seconds, w.description, w.updated_at`

// mirrorRootLiveSQL holds for a live root ?1.
const mirrorRootLiveSQL = `EXISTS (SELECT 1 FROM tracker_cache_roots live_root WHERE live_root.id = ?1 AND live_root.connection_id IS NOT NULL)`

// ListMirrorWorklogs reads authorID's worklogs whose day is fromDay to
// toDay, by day and ID, at most mirrorWorklogLimit; more reports that the
// range holds more.
func (s *Store) ListMirrorWorklogs(rootID int64, authorID, fromDay, toDay string) (logs []MirrorWorklog, more bool, err error) {
	logs, more, err = queryMirrorWorklogs(s.reader(), `SELECT `+mirrorWorklogColumns+` FROM tracker_cache_worklogs w
		WHERE w.root_id = ?1 AND w.author_id = ?2 AND w.day BETWEEN ?3 AND ?4 AND `+mirrorRootLiveSQL+`
		ORDER BY w.day, w.worklog_id LIMIT `+strconv.Itoa(mirrorWorklogLimit+1), rootID, authorID, fromDay, toDay)
	if err != nil {
		return nil, false, fmt.Errorf("store: list tracker mirror %d worklogs of %s: %w", rootID, authorID, err)
	}
	return logs, more, nil
}

// ListMirrorIssueWorklogs reads the worklogs of one issue, by day and ID,
// at most mirrorWorklogLimit; more reports that the issue has more.
func (s *Store) ListMirrorIssueWorklogs(rootID int64, issueID string) (logs []MirrorWorklog, more bool, err error) {
	logs, more, err = queryMirrorWorklogs(s.reader(), `SELECT `+mirrorWorklogColumns+` FROM tracker_cache_worklogs w
		WHERE w.root_id = ?1 AND w.issue_id = ?2 AND `+mirrorRootLiveSQL+`
		ORDER BY w.day, w.worklog_id LIMIT `+strconv.Itoa(mirrorWorklogLimit+1), rootID, issueID)
	if err != nil {
		return nil, false, fmt.Errorf("store: list tracker mirror %d issue %s worklogs: %w", rootID, issueID, err)
	}
	return logs, more, nil
}

func queryMirrorWorklogs(q sqlQueryer, query string, args ...any) ([]MirrorWorklog, bool, error) {
	var logs []MirrorWorklog
	err := scanMirrorRows(q, query, args, func(r rowScanner) error {
		var w MirrorWorklog
		err := r.Scan(&w.ID, &w.IssueID, &w.AuthorID, &w.Day, &w.SpentSeconds, &w.Description, &w.UpdatedAt)
		logs = append(logs, w)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if len(logs) > mirrorWorklogLimit {
		return logs[:mirrorWorklogLimit], true, nil
	}
	return logs, false, nil
}

// Failure kinds, aligned with tracker_cache_sync.failure_kind.
const (
	MirrorFailureTransient   = "transient"
	MirrorFailureRateLimited = "rate_limited"
	MirrorFailureSetup       = "setup"
	MirrorFailureForbidden   = "forbidden"
	MirrorFailureTracker     = "tracker"
)

// MirrorSyncFailure is a scope's last failure; the zero value is none.
type MirrorSyncFailure struct {
	At            int64
	Kind, Message string
}

// MirrorSync is a scope's sync state. A scope is a board, or with board ID
// empty the root's linked issues. Lifetime is a listed board's current
// lifetime (zero for the linked scope or a board not listed): the caller
// captures it before its reads and names it in every write of the board,
// so nothing read in an earlier lifetime is stored in a later one. Sweep
// is the root's sweep, set for every scope, ok or not: the caller captures
// it before reading content outside a board (cards, details, histories)
// and names it in the write, so nothing read before an issue was swept is
// stored after it is kept again (ErrTrackerCacheSwept).
type MirrorSync struct {
	Lifetime     int64
	Sweep        int64
	Cursor       []byte
	ReadAt       int64
	ReconciledAt int64
	Failure      MirrorSyncFailure
}

// mirrorSyncStateSQL reads whether the root is live (NULL: no root), a
// scope's sync state and, for a board listed through a live root and not
// retired, the board's lifetime.
var mirrorSyncStateSQL = `SELECT rt.connection_id IS NOT NULL, coalesce(rt.sweep, 0),
	  coalesce(bl.lifetime, 0), s.root_id IS NOT NULL, s.cursor, coalesce(s.read_at, 0),
	  coalesce(s.reconciled_at, 0), coalesce(s.failed_at, 0), coalesce(s.failure_kind, ''), coalesce(s.failure_message, '')
	FROM (SELECT 1)
	LEFT JOIN tracker_cache_roots rt ON rt.id = ?1
	LEFT JOIN tracker_cache_boards bl ON bl.root_id = ?1 AND bl.board_id = ?2 AND bl.retired = 0
	  AND EXISTS (SELECT 1 FROM tracker_cache_roots lr JOIN project_tracker_boards lp ON lp.connection_id = lr.connection_id
	               WHERE lr.id = ?1 AND lp.board_id = ?2)
	LEFT JOIN tracker_cache_sync s ON s.root_id = ?1 AND s.board_id = ?2 AND ` + mirrorRootLiveSQL + `
	  AND (?2 = '' OR ` + mirrorBoardShown("?1", "?2") + `)`

// GetMirrorSync reads a scope's sync state, if any was written: none for a
// board the mirror does not show. Lifetime is set whenever the board is
// listed, ok or not. A root that is detached or gone fails with
// ErrTrackerCacheGone, so a caller that keeps the root's ID reads a tick's
// whole state in this one statement and looks the root up again
// (GetOrCreateMirrorRoot) only then.
func (s *Store) GetMirrorSync(rootID int64, boardID string) (MirrorSync, bool, error) {
	var st MirrorSync
	var live sql.NullBool
	var ok bool
	err := s.reader().QueryRow(mirrorSyncStateSQL, rootID, boardID).Scan(&live, &st.Sweep, &st.Lifetime, &ok,
		&st.Cursor, &st.ReadAt, &st.ReconciledAt, &st.Failure.At, &st.Failure.Kind, &st.Failure.Message)
	if err == nil && !live.Bool {
		err = ErrTrackerCacheGone
	}
	if err != nil {
		return MirrorSync{}, false, fmt.Errorf("store: read tracker mirror %d sync of board %q: %w", rootID, boardID, err)
	}
	if !ok {
		return MirrorSync{Lifetime: st.Lifetime, Sweep: st.Sweep}, false, nil
	}
	return st, true, nil
}

// MirrorSyncAttempt is the outcome of one tick of a scope: ReadAt is when
// a read that succeeded began, ReconciledAt when a reconcile that
// succeeded began, and Failure a failure that followed them (its kind one
// of MirrorFailure*); a zero time is none, and an attempt names at least
// one. Lifetime is the board's lifetime its caller captured (zero for the
// linked scope).
type MirrorSyncAttempt struct {
	Lifetime             int64
	ReadAt, ReconciledAt int64
	Failure              MirrorSyncFailure
}

func (a MirrorSyncAttempt) check(boardID string) error {
	f := a.Failure
	switch {
	case a.ReadAt < 0 || a.ReconciledAt < 0 || f.At < 0 || a.ReadAt+a.ReconciledAt+f.At == 0:
		return fmt.Errorf("%w: an attempt needs the time of its success or failure", ErrInvalidTrackerCache)
	case boardID == "" && a.Lifetime != 0:
		return fmt.Errorf("%w: the linked scope has no lifetime", ErrInvalidTrackerCache)
	case f.At == 0 && (f.Kind != "" || f.Message != ""):
		return fmt.Errorf("%w: a failure with no time", ErrInvalidTrackerCache)
	}
	switch f.Kind {
	case MirrorFailureTransient, MirrorFailureRateLimited, MirrorFailureSetup, MirrorFailureForbidden, MirrorFailureTracker:
		return nil
	case "":
		if f.At == 0 {
			return nil
		}
	}
	return fmt.Errorf("%w: failure kind %q", ErrInvalidTrackerCache, f.Kind)
}

// mirrorSyncAttemptSQL records an attempt in one statement: ?1 root, ?2
// board, ?3 the read, ?4 the reconcile, ?5 to ?7 the failure's time, kind
// and message. Every SET expression reads the row as it was, so the
// fragments below are spelled out where they are used:
//   - {cleared}: a success clears a failure recorded no later than it;
//   - {admitted}: the failure replaces the stored one (after the clear)
//     unless the scope was read, reconciled, failed or cleared later, this
//     attempt's successes included, and it differs from it;
//   - {failed_at} and the other failure columns: their new values.
//
// The WHERE writes the row only when a value changes.
const mirrorSyncAttemptTemplate = `INSERT INTO tracker_cache_sync(root_id, board_id, read_at, reconciled_at,
	    failed_at, failure_kind, failure_message, outcome_at)
	SELECT ?1, ?2, ?3, ?4, iif(f, ?5, 0), iif(f, ?6, ''), iif(f, ?7, ''), iif(f, ?5, 0)
	  FROM (SELECT ?5 > 0 AND ?5 >= max(?3, ?4) AS f) WHERE true
	ON CONFLICT(root_id, board_id) DO UPDATE SET read_at = max(read_at, ?3), reconciled_at = max(reconciled_at, ?4),
	    failed_at = {failed_at}, failure_kind = {failure_kind}, failure_message = {failure_message}, outcome_at = {outcome_at}
	WHERE ?3 > read_at OR ?4 > reconciled_at OR {failed_at} IS NOT failed_at OR {failure_kind} IS NOT failure_kind
	   OR {failure_message} IS NOT failure_message OR {outcome_at} IS NOT outcome_at
	RETURNING board_id`

var mirrorSyncAttemptSQL = strings.NewReplacer(
	"{cleared}", `(failure_kind <> '' AND max(?3, ?4) > 0 AND max(?3, ?4) >= outcome_at)`,
).Replace(strings.NewReplacer(
	"{admitted}", `(?5 > 0 AND ?5 >= max(iif({cleared}, max(?3, ?4), outcome_at), read_at, reconciled_at, ?3, ?4)
		AND (iif({cleared}, 0, failed_at) IS NOT ?5 OR iif({cleared}, '', failure_kind) IS NOT ?6
		  OR iif({cleared}, '', failure_message) IS NOT ?7))`,
).Replace(strings.NewReplacer(
	"{failed_at}", `iif({admitted}, ?5, iif({cleared}, 0, failed_at))`,
	"{failure_kind}", `iif({admitted}, ?6, iif({cleared}, '', failure_kind))`,
	"{failure_message}", `iif({admitted}, ?7, iif({cleared}, '', failure_message))`,
	"{outcome_at}", `iif({admitted}, ?5, iif({cleared}, max(?3, ?4), outcome_at))`,
).Replace(mirrorSyncAttemptTemplate)))

// RecordMirrorSyncAttempt records the outcome of one tick of a scope in
// one statement and reports whether the scope's state changed; an attempt
// that changes nothing writes nothing. A success moves the read or
// reconcile time forward, never back, and clears a failure recorded no
// later than it. A failure replaces the stored one unless the scope was
// read, reconciled, failed or cleared later, by this attempt's successes
// included (superseded). A board scope must be listed in a.Lifetime
// (ErrTrackerCacheUnlisted).
func (s *Store) RecordMirrorSyncAttempt(rootID int64, boardID string, a MirrorSyncAttempt) (changed bool, err error) {
	if err := a.check(boardID); err != nil {
		return false, err
	}
	action := fmt.Sprintf("store: record tracker mirror %d sync attempt of board %q", rootID, boardID)
	write := func(tx *sql.Tx) error {
		var err error
		changed, err = execChangedTx(tx, mirrorSyncAttemptSQL, rootID, boardID, a.ReadAt, a.ReconciledAt,
			a.Failure.At, a.Failure.Kind, a.Failure.Message)
		return err
	}
	if boardID == "" {
		err = s.mirrorWrite(rootID, action, write)
	} else {
		err = s.mirrorBoardWrite(rootID, boardID, a.Lifetime, action, false, func(tx *sql.Tx, _ mirrorBoard) error { return write(tx) })
	}
	return changed, err
}
