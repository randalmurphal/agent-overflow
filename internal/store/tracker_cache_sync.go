package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
// read that began later changed it (tracker_cache_worklog_days).
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
	mirrorWorklogDaysSQL = `INSERT INTO tracker_cache_worklog_days(root_id, author_id, day, read_at)
		SELECT ?1, ?2, value, ?3 FROM json_each(?4) WHERE true
		ON CONFLICT(root_id, author_id, day) DO UPDATE SET read_at = excluded.read_at WHERE excluded.read_at > read_at`
)

// ReplaceMirrorWorklogs makes logs the whole of authorID's worklogs whose
// day is fromDay to toDay, both included, as a read that began at readAt
// found them. Every log must be the author's and inside the range; the
// range covers at most mirrorMaxWorklogDays days and clearMirrorChunk
// logs, and must hold at most clearMirrorChunk stored logs (a longer one
// is replaced in shorter ranges). A day a read that began later changed
// is left as it was and reported in SupersededDays; an identical read
// writes nothing.
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
		days := map[string]bool{}
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
				days[day] = true
				return err
			})
			if err != nil {
				return fmt.Errorf("%s: %w", step.what, err)
			}
		}
		if len(days) == 0 {
			return nil
		}
		changedDays, err := encodeMirrorIDs(slices.Sorted(func(yield func(string) bool) {
			for day := range days {
				if !yield(day) {
					return
				}
			}
		}))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(mirrorWorklogDaysSQL, rootID, authorID, readAt, changedDays); err != nil {
			return fmt.Errorf("record the changed days: %w", err)
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
// empty the root's linked issues.
type MirrorSync struct {
	Cursor       []byte
	ReadAt       int64
	ReconciledAt int64
	Failure      MirrorSyncFailure
}

// GetMirrorSync reads a scope's sync state, if any was written: none for a
// detached root or a board the mirror does not show.
func (s *Store) GetMirrorSync(rootID int64, boardID string) (MirrorSync, bool, error) {
	var st MirrorSync
	err := s.reader().QueryRow(`SELECT s.cursor, s.read_at, s.reconciled_at, s.failed_at, s.failure_kind, s.failure_message
		FROM tracker_cache_sync s WHERE s.root_id = ?1 AND s.board_id = ?2 AND `+mirrorRootLiveSQL+`
		  AND (?2 = '' OR `+mirrorBoardShown("?1", "?2")+`)`, rootID, boardID).Scan(
		&st.Cursor, &st.ReadAt, &st.ReconciledAt, &st.Failure.At, &st.Failure.Kind, &st.Failure.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorSync{}, false, nil
	}
	if err != nil {
		return MirrorSync{}, false, fmt.Errorf("store: read tracker mirror %d sync of board %q: %w", rootID, boardID, err)
	}
	return st, true, nil
}

// MirrorSyncAttempt is the outcome of one read or reconcile of a scope:
// At is when it began, Reconcile tells a reconcile from a read, and a
// failure has its kind (MirrorFailure*) and message; an attempt with no
// failure kind succeeded.
type MirrorSyncAttempt struct {
	At                          int64
	Reconcile                   bool
	FailureKind, FailureMessage string
}

// The attempt statements: ?1 root, ?2 board, ?3 when the attempt began.
// Every SET expression reads the row as it was.
const (
	// A success moves the read or reconcile time forward and clears a
	// failure recorded no later than it. The WHERE keeps the time forward:
	// a stored failure is never older than the read and reconcile times
	// (mirrorSyncFailureSQL), so the clear's arm admits no earlier one.
	mirrorSyncSuccessSQL = `INSERT INTO tracker_cache_sync(root_id, board_id, %[1]s) VALUES (?1, ?2, ?3)
		ON CONFLICT(root_id, board_id) DO UPDATE SET %[1]s = excluded.%[1]s,
		    failed_at = iif(failure_kind <> '' AND ?3 >= outcome_at, 0, failed_at),
		    failure_message = iif(failure_kind <> '' AND ?3 >= outcome_at, '', failure_message),
		    outcome_at = iif(failure_kind <> '' AND ?3 >= outcome_at, ?3, outcome_at),
		    failure_kind = iif(failure_kind <> '' AND ?3 >= outcome_at, '', failure_kind)
		WHERE ?3 > %[1]s OR (failure_kind <> '' AND ?3 >= outcome_at)
		RETURNING board_id`
	// A failure replaces the stored one unless the scope was read,
	// reconciled, failed or cleared later.
	mirrorSyncFailureSQL = `INSERT INTO tracker_cache_sync(root_id, board_id, failed_at, failure_kind, failure_message, outcome_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?3)
		ON CONFLICT(root_id, board_id) DO UPDATE SET failed_at = excluded.failed_at, failure_kind = excluded.failure_kind,
		    failure_message = excluded.failure_message, outcome_at = excluded.outcome_at
		WHERE excluded.failed_at >= max(outcome_at, read_at, reconciled_at)
		  AND (failed_at IS NOT excluded.failed_at OR failure_kind IS NOT excluded.failure_kind OR failure_message IS NOT excluded.failure_message)
		RETURNING board_id`
)

var (
	mirrorSyncReadSQL      = fmt.Sprintf(mirrorSyncSuccessSQL, "read_at")
	mirrorSyncReconcileSQL = fmt.Sprintf(mirrorSyncSuccessSQL, "reconciled_at")
)

// RecordMirrorSyncAttempt records the outcome of a read or reconcile of a
// scope in one statement and reports whether the scope's state changed;
// an attempt that changes nothing writes nothing. A success moves the read
// or reconcile time forward, never back, and clears a failure recorded no
// later than it. A failure replaces the stored one unless the scope was
// read, reconciled, failed or cleared later (superseded). A board scope
// must be listed (ErrTrackerCacheUnlisted).
func (s *Store) RecordMirrorSyncAttempt(rootID int64, boardID string, a MirrorSyncAttempt) (changed bool, err error) {
	if a.At <= 0 {
		return false, fmt.Errorf("%w: an attempt needs its time", ErrInvalidTrackerCache)
	}
	query, values := mirrorSyncReadSQL, []any{a.At}
	switch a.FailureKind {
	case "":
		if a.FailureMessage != "" {
			return false, fmt.Errorf("%w: a success with a failure message", ErrInvalidTrackerCache)
		}
		if a.Reconcile {
			query = mirrorSyncReconcileSQL
		}
	case MirrorFailureTransient, MirrorFailureRateLimited, MirrorFailureSetup, MirrorFailureForbidden, MirrorFailureTracker:
		query, values = mirrorSyncFailureSQL, []any{a.At, a.FailureKind, a.FailureMessage}
	default:
		return false, fmt.Errorf("%w: failure kind %q", ErrInvalidTrackerCache, a.FailureKind)
	}
	action := fmt.Sprintf("store: record tracker mirror %d sync attempt of board %q", rootID, boardID)
	write := func(tx *sql.Tx) error {
		var err error
		changed, err = execChangedTx(tx, query, append([]any{rootID, boardID}, values...)...)
		return err
	}
	if boardID == "" {
		err = s.mirrorWrite(rootID, action, write)
	} else {
		err = s.mirrorBoardWrite(rootID, boardID, action, false, func(tx *sql.Tx, _ mirrorBoard) error { return write(tx) })
	}
	return changed, err
}
