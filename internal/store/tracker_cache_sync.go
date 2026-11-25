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
	// MaxReplacedWorklogs is the most worklogs one replace carries or
	// removes (clearMirrorChunk).
	MaxReplacedWorklogs = clearMirrorChunk
)

// ErrTooManyWorklogs is matched by a worklog replace refused because its
// range carries or holds more than MaxReplacedWorklogs worklogs: the
// caller reads fewer days at a time.
var ErrTooManyWorklogs = errors.New("store: more worklogs than one replace stores")

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

// mirrorDays lists n days from from on, by count: a string comparison
// with the last day would pass 9999-12-31, since 10000-01-01 sorts
// before it.
func mirrorDays(from time.Time, n int) []string {
	days := make([]string, n)
	for i := range n {
		days[i] = from.AddDate(0, 0, i).Format(time.DateOnly)
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

// checkMirrorWorklogRange checks a replace and returns the days of its
// range.
func checkMirrorWorklogRange(authorID, fromDay, toDay string, readAt int64, logs []MirrorWorklog) ([]string, error) {
	from, fromErr := parseMirrorDay(fromDay)
	to, toErr := parseMirrorDay(toDay)
	if err := errors.Join(fromErr, toErr); err != nil {
		return nil, err
	}
	days := int(to.Sub(from).Hours()/24) + 1
	switch {
	case authorID == "" || readAt <= 0 || days < 1:
		return nil, fmt.Errorf("%w: worklogs need an author, a read time and days in order", ErrInvalidTrackerCache)
	case days > mirrorMaxWorklogDays:
		return nil, fmt.Errorf("%w: %d days, over %d", ErrInvalidTrackerCache, days, mirrorMaxWorklogDays)
	case len(logs) > MaxReplacedWorklogs:
		return nil, fmt.Errorf("%w: %w: %d worklogs", ErrInvalidTrackerCache, ErrTooManyWorklogs, len(logs))
	}
	seen := make(map[string]bool, len(logs))
	for _, w := range logs {
		if w.ID == "" || seen[w.ID] || w.IssueID == "" || w.AuthorID != authorID || w.Day < fromDay || w.Day > toDay ||
			w.SpentSeconds <= 0 || w.UpdatedAt < 0 {
			return nil, fmt.Errorf("%w: worklog %q is repeated, incomplete, or not %s's from %s to %s",
				ErrInvalidTrackerCache, w.ID, authorID, fromDay, toDay)
		}
		if _, err := parseMirrorDay(w.Day); err != nil {
			return nil, err
		}
		seen[w.ID] = true
	}
	return mirrorDays(from, days), nil
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
	// mirrorWorklogSourceGenSQL is live root ?1's connection's time source
	// generation (migration v168).
	mirrorWorklogSourceGenSQL = `SELECT c.time_source_gen FROM tracker_cache_roots r
		JOIN tracker_connections c ON c.id = r.connection_id WHERE r.id = ?1`
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
//
// sourceGen is the time source generation of the root's connection that
// the read began under (TrackerConnection.TimeSourceGen). A replace under
// another generation is refused with ErrTrackerCacheSourceChanged in the
// write's transaction: the source changed since, and the change deleted
// the root's worklogs and read days (migration v168).
func (s *Store) ReplaceMirrorWorklogs(rootID, sourceGen int64, authorID, fromDay, toDay string, read MirrorRead, logs []MirrorWorklog) (changes MirrorWorklogChanges, err error) {
	times, err := read.in(rootID)
	if err != nil {
		return MirrorWorklogChanges{}, err
	}
	readAt := times.at
	days, err := checkMirrorWorklogRange(authorID, fromDay, toDay, readAt, logs)
	if err != nil {
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
	rangeDays, err := encodeMirrorIDs(days)
	if err != nil {
		return MirrorWorklogChanges{}, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %d worklogs of %s", rootID, authorID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRow(mirrorWorklogSourceGenSQL, rootID).Scan(&current); err != nil {
			return fmt.Errorf("read the time source generation: %w", err)
		}
		if current != sourceGen {
			return fmt.Errorf("read under time source generation %d, now %d: %w", sourceGen, current, ErrTrackerCacheSourceChanged)
		}
		args := []any{rootID, authorID, fromDay, toDay, readAt, encoded}
		var stored int
		if err := tx.QueryRow(mirrorWorklogCountSQL, args[:4]...).Scan(&stored); err != nil {
			return fmt.Errorf("count stored worklogs: %w", err)
		}
		if stored > clearMirrorChunk {
			return fmt.Errorf("%w: %w: %s to %s holds over that many", ErrInvalidTrackerCache, ErrTooManyWorklogs, fromDay, toDay)
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

// mirrorAuthorWorklogsSQL reads root ?1's worklogs of author ?2 from day
// ?3 to ?4, by day and ID, at most mirrorWorklogLimit and one more.
var mirrorAuthorWorklogsSQL = `SELECT ` + mirrorWorklogColumns + ` FROM tracker_cache_worklogs w
	WHERE w.root_id = ?1 AND w.author_id = ?2 AND w.day BETWEEN ?3 AND ?4 AND ` + mirrorRootLiveSQL + `
	ORDER BY w.day, w.worklog_id LIMIT ` + strconv.Itoa(mirrorWorklogLimit+1)

// MirrorWorklogDays is a connection's time of an author's days as one
// snapshot holds it (ReadMirrorWorklogDays).
type MirrorWorklogDays struct {
	// Connection is the connection's row; Found is false when it has none.
	Connection TrackerConnection
	Found      bool
	// Logs are the author's worklogs of the range by day and ID, at most
	// mirrorWorklogLimit, and More reports that the range holds more;
	// ReadDays are the days of the range a read replaced, sorted. A
	// connection with no mirror root has neither.
	Logs     []MirrorWorklog
	More     bool
	ReadDays []string
}

// ReadMirrorWorklogDays reads connectionID's row, and its mirror's
// worklogs of authorID from fromDay to toDay with the days of that range
// a read replaced, in one snapshot, so the rows and the days they cover
// describe one state of the mirror and of the connection.
func (s *Store) ReadMirrorWorklogDays(connectionID, authorID, fromDay, toDay string) (MirrorWorklogDays, error) {
	label := fmt.Sprintf("read tracker connection %s worklogs of %s", connectionID, authorID)
	return mirrorReadSnapshot(s, label, func(q sqlQueryer) (MirrorWorklogDays, error) {
		var out MirrorWorklogDays
		row, err := scanTrackerConnection(q.QueryRow(`SELECT `+trackerConnectionColumns+` FROM tracker_connections WHERE id = ?`, connectionID).Scan)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return out, nil
		case err != nil:
			return out, fmt.Errorf("read the connection: %w", err)
		}
		out.Connection, out.Found = row, true
		var root int64
		err = q.QueryRow(`SELECT id FROM tracker_cache_roots WHERE connection_id = ?`, connectionID).Scan(&root)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return out, nil
		case err != nil:
			return out, fmt.Errorf("read the mirror root: %w", err)
		}
		if out.ReadDays, err = queryIDs(q, `SELECT day FROM tracker_cache_worklog_days
			WHERE root_id = ?1 AND author_id = ?2 AND day BETWEEN ?3 AND ?4 ORDER BY day`, root, authorID, fromDay, toDay); err != nil {
			return out, fmt.Errorf("read the days read: %w", err)
		}
		if out.Logs, out.More, err = queryMirrorWorklogs(q, mirrorAuthorWorklogsSQL, root, authorID, fromDay, toDay); err != nil {
			return out, fmt.Errorf("read the worklogs: %w", err)
		}
		return out, nil
	})
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

// MirrorSyncFailure is a scope's last failure; the zero value is none. At
// is the order time of the read it followed and Wall the wall time a
// person is shown (zero: stored before v165).
type MirrorSyncFailure struct {
	At, Wall      int64
	Kind, Message string
}

// MirrorReconcileFailure is a board's last reconcile failure; the zero
// value is none. Its Wall is when the failing call returned, which is
// also when a refusal's wait began. Refused reports that the tracker
// refused the query that carried the listing's rule (tracker.ErrRuleRefused).
type MirrorReconcileFailure struct {
	MirrorSyncFailure
	Refused bool
}

// MirrorSyncOutcome is what a scope's ticks recorded, which the store
// orders: the latest read and reconcile that succeeded, and two failures.
// Failure is the last failure of any unit but a board's reconcile, which
// only a later read clears; ReconcileFailure is a board's last reconcile
// failure, which only a later reconcile clears. ReadWall and
// ReconciledWall are the wall times the read and the reconcile began (zero:
// stored before v165): what the reconcile interval is measured from and
// what a person is shown, where ReadAt and ReconciledAt are order times,
// which order the writes.
type MirrorSyncOutcome struct {
	ReadAt           int64
	ReadWall         int64
	ReconciledAt     int64
	ReconciledWall   int64
	Failure          MirrorSyncFailure
	ReconcileFailure MirrorReconcileFailure
}

// MirrorSync is a scope's sync state. A scope is a board, or with board ID
// empty the root's linked issues. Lifetime is a listed board's current
// lifetime (zero for the linked scope or a board not listed): the caller
// captures it before its reads and names it in every write of the board,
// so nothing read in an earlier lifetime is stored in a later one. Sweep
// is the root's sweep, set for every scope, ok or not: the caller captures
// it before reading content outside a board (cards, details, histories)
// and names it in the write, so nothing read before an issue was swept is
// stored after it is kept again (ErrTrackerCacheSwept). StructureDueAt,
// set for a board listed or not, is the latest read of a row page that
// named a status, user or sprint the mirror lacked and no complete
// structure read marked unexplained, when no structure read that began
// later is stored (zero: none due). EstimateResetAt, set for a listed
// board, is the read of the structure that last changed the board's
// estimate definition: the caller captures it with the cursor and names
// it in each page of the walk (MirrorRowPage.EstimateResetAt), so a walk
// begun under an earlier definition stores no cursor. StructureWall, set
// for a listed board, is the wall time the stored structure's read began
// (zero: none stored, or stored before v165). HistoryDue, set for every
// scope, reports that an issue the root keeps is due a history read at
// the time GetMirrorSync was given (ListMirrorHistoryDue lists them), so
// a caller deciding whether to run history reads nothing more.
type MirrorSync struct {
	Lifetime        int64
	Sweep           int64
	StructureDueAt  int64
	EstimateResetAt int64
	StructureWall   int64
	HistoryDue      bool
	Cursor          []byte
	MirrorSyncOutcome
}

// mirrorSyncOutcomeColumns are the columns of a MirrorSyncOutcome, in the
// order scanOutcome reads them.
const mirrorSyncOutcomeColumns = `read_at, read_wall, reconciled_at, reconciled_wall,
	  failed_at, failed_wall, failure_kind, failure_message,
	  reconcile_failed_at, reconcile_failed_wall, reconcile_failure_kind, reconcile_failure_message, reconcile_refused`

// outcomeDest are the scan destinations of mirrorSyncOutcomeColumns.
func (o *MirrorSyncOutcome) outcomeDest() []any {
	r := &o.ReconcileFailure
	return []any{&o.ReadAt, &o.ReadWall, &o.ReconciledAt, &o.ReconciledWall,
		&o.Failure.At, &o.Failure.Wall, &o.Failure.Kind, &o.Failure.Message,
		&r.At, &r.Wall, &r.Kind, &r.Message, &r.Refused}
}

// mirrorSyncStateSQL reads whether the root is live (NULL: no root), a
// scope's sync state and, for a board listed through a live root and not
// retired, the board's lifetime, due state (mirrorBoard.structureDueAt),
// estimate reset and structure read's wall time, and whether a kept issue is due a
// history read at ?3 (mirrorHistoryDueExistsSQL): a probe per arm of the
// due rule that stops at the first kept issue, so issues waiting out their
// wait cost nothing.
var mirrorSyncStateSQL = `SELECT rt.connection_id IS NOT NULL, coalesce(rt.sweep, 0),
	  coalesce(bl.lifetime, 0), coalesce((SELECT iif(max(p.named_at) >= bl.read_at, max(p.named_at), 0) FROM tracker_cache_board_unexplained p
	    WHERE p.root_id = ?1 AND p.board_id = ?2 AND p.named_at > 0), 0), coalesce(bl.estimate_reset_at, 0), coalesce(bl.structure_wall, 0),
	  ` + mirrorHistoryDueExistsSQL("?3") + `,
	  s.root_id IS NOT NULL, s.cursor, coalesce(s.read_at, 0), coalesce(s.read_wall, 0),
	  coalesce(s.reconciled_at, 0), coalesce(s.reconciled_wall, 0), coalesce(s.failed_at, 0), coalesce(s.failed_wall, 0),
	  coalesce(s.failure_kind, ''), coalesce(s.failure_message, ''),
	  coalesce(s.reconcile_failed_at, 0), coalesce(s.reconcile_failed_wall, 0), coalesce(s.reconcile_failure_kind, ''),
	  coalesce(s.reconcile_failure_message, ''), coalesce(s.reconcile_refused, 0)
	FROM (SELECT 1)
	LEFT JOIN tracker_cache_roots rt ON rt.id = ?1
	LEFT JOIN tracker_cache_boards bl ON bl.root_id = ?1 AND bl.board_id = ?2 AND bl.retired = 0
	  AND EXISTS (SELECT 1 FROM tracker_cache_roots lr JOIN project_tracker_boards lp ON lp.connection_id = lr.connection_id
	               WHERE lr.id = ?1 AND lp.board_id = ?2)
	LEFT JOIN tracker_cache_sync s ON s.root_id = ?1 AND s.board_id = ?2 AND ` + mirrorRootLiveSQL + `
	  AND (?2 = '' OR ` + mirrorBoardShown("?1", "?2") + `)`

// GetMirrorSync reads a scope's sync state, if any was written: none for a
// board the mirror does not show, with HistoryDue as of now, the caller's
// steady clock in Unix milliseconds (mirrorHistoryDue). Lifetime is set
// whenever the board is listed, ok or not. A root that is detached or gone fails with
// ErrTrackerCacheGone, so a caller that keeps the root's ID reads a tick's
// whole state in this one statement and looks the root up again
// (GetOrCreateMirrorRoot) only then.
func (s *Store) GetMirrorSync(rootID int64, boardID string, now int64) (MirrorSync, bool, error) {
	var st MirrorSync
	var live sql.NullBool
	var ok bool
	dest := append([]any{&live, &st.Sweep, &st.Lifetime, &st.StructureDueAt, &st.EstimateResetAt, &st.StructureWall, &st.HistoryDue, &ok, &st.Cursor},
		st.outcomeDest()...)
	err := s.reader().QueryRow(mirrorSyncStateSQL, rootID, boardID, now).Scan(dest...)
	if err == nil && !live.Bool {
		err = ErrTrackerCacheGone
	}
	if err != nil {
		return MirrorSync{}, false, fmt.Errorf("store: read tracker mirror %d sync of board %q: %w", rootID, boardID, err)
	}
	if !ok {
		return MirrorSync{Lifetime: st.Lifetime, Sweep: st.Sweep, StructureDueAt: st.StructureDueAt, EstimateResetAt: st.EstimateResetAt,
			StructureWall: st.StructureWall, HistoryDue: st.HistoryDue}, false, nil
	}
	return st, true, nil
}

// MirrorSyncAttempt is the outcome of one tick of a scope: ReadAt is when
// a read that succeeded began, ReconciledAt when a reconcile that
// completed began, Failure the failure of a unit other than a board's
// reconcile that followed them, and ReconcileFailure a board's failed
// reconcile; a zero time is none, and an attempt names at least one. A
// tick names a reconcile's completion or its failure, never both.
// Lifetime is the board's lifetime its caller captured (zero for the
// linked scope, which names no reconcile).
type MirrorSyncAttempt struct {
	Lifetime             int64
	ReadAt, ReconciledAt MirrorRead
	Failure              MirrorAttemptFailure
	ReconcileFailure     MirrorReconcileAttempt
}

// MirrorAttemptFailure is an attempt's failure: when the unit that failed
// began, and its kind and sentence (MirrorSyncFailure as read back).
type MirrorAttemptFailure struct {
	At            MirrorRead
	Kind, Message string
}

// MirrorReconcileAttempt is an attempt's reconcile failure: At is the read
// the failing call began, which orders it, and Returned the wall clock in
// Unix ms when that call returned, which is shown and which a refusal's
// wait runs from (MirrorReconcileFailure.Wall): one call can make several
// requests, the refused one last.
type MirrorReconcileAttempt struct {
	At            MirrorRead
	Returned      int64
	Kind, Message string
	Refused       bool
}

// mirrorSyncTimes are an attempt's reads once checked against the root it
// is recorded in (MirrorSyncAttempt.in).
type mirrorSyncTimes struct{ succeeded, reconciled, failed, reconcileFailed mirrorReadTimes }

func (a MirrorSyncAttempt) in(rootID int64) (t mirrorSyncTimes, err error) {
	for _, r := range []struct {
		of MirrorRead
		to *mirrorReadTimes
	}{{a.ReadAt, &t.succeeded}, {a.ReconciledAt, &t.reconciled}, {a.Failure.At, &t.failed}, {a.ReconcileFailure.At, &t.reconcileFailed}} {
		if *r.to, err = r.of.in(rootID); err != nil {
			return mirrorSyncTimes{}, err
		}
	}
	return t, nil
}

func (a MirrorSyncAttempt) check(boardID string, t mirrorSyncTimes) error {
	rf := a.ReconcileFailure
	switch {
	case t.succeeded.at < 0 || t.reconciled.at < 0 || t.failed.at < 0 || t.reconcileFailed.at < 0 ||
		t.succeeded.at+t.reconciled.at+t.failed.at+t.reconcileFailed.at == 0:
		return fmt.Errorf("%w: an attempt needs the time of its success or failure", ErrInvalidTrackerCache)
	case boardID == "" && a.Lifetime != 0:
		return fmt.Errorf("%w: the linked scope has no lifetime", ErrInvalidTrackerCache)
	case boardID == "" && (t.reconciled.at != 0 || rf != (MirrorReconcileAttempt{})):
		return fmt.Errorf("%w: the linked scope has no reconcile", ErrInvalidTrackerCache)
	case t.reconciled.at != 0 && t.reconcileFailed.at != 0:
		return fmt.Errorf("%w: a reconcile that completed and failed", ErrInvalidTrackerCache)
	case t.reconcileFailed.at == 0 && rf != (MirrorReconcileAttempt{}):
		return fmt.Errorf("%w: a reconcile failure with no time", ErrInvalidTrackerCache)
	case t.reconcileFailed.at != 0 && (rf.Returned <= 0 || !mirrorFailureKind(rf.Kind)):
		return fmt.Errorf("%w: reconcile failure kind %q returned at %d", ErrInvalidTrackerCache, rf.Kind, rf.Returned)
	case t.failed.at == 0 && (a.Failure.Kind != "" || a.Failure.Message != ""):
		return fmt.Errorf("%w: a failure with no time", ErrInvalidTrackerCache)
	case t.failed.at != 0 && !mirrorFailureKind(a.Failure.Kind):
		return fmt.Errorf("%w: failure kind %q", ErrInvalidTrackerCache, a.Failure.Kind)
	}
	return nil
}

// mirrorFailureKind reports kind as one of MirrorFailure*.
func mirrorFailureKind(kind string) bool {
	switch kind {
	case MirrorFailureTransient, MirrorFailureRateLimited, MirrorFailureSetup, MirrorFailureForbidden, MirrorFailureTracker:
		return true
	}
	return false
}

// mirrorSyncAttemptSQL records an attempt in one statement: ?1 root, ?2
// board, ?3 the read, ?4 the reconcile, ?5 to ?7 the failure's time, kind
// and message, ?8 to ?10 the wall times of the reconcile, the read and the
// failure, each stored with the order time it began at, and ?11 to ?15
// the reconcile failure's time, kind, message, wall time and refusal.
// Every SET expression reads the row as it was, so the fragments below are
// spelled out where they are used. A success wins a tie with a failure of
// its own slot in either order, and a failure replaces only an older one:
//   - {cleared}: a read clears the failure recorded at or before it;
//   - {admitted}: the failure replaces the stored one when it is later
//     than the scope's last read, failure and clear, this attempt's read
//     included;
//   - {rcleared} and {radmitted}: the same of the reconcile failure, by
//     the reconcile's completions;
//   - {failed_at} and the other failure columns: their new values.
//
// The WHERE writes the row only when a value changes, and RETURNING
// returns the outcome the row holds then.
const mirrorSyncAttemptTemplate = `INSERT INTO tracker_cache_sync(root_id, board_id, read_at, read_wall, reconciled_at, reconciled_wall,
	    failed_at, failed_wall, failure_kind, failure_message, outcome_at,
	    reconcile_failed_at, reconcile_failed_wall, reconcile_failure_kind, reconcile_failure_message, reconcile_refused)
	SELECT ?1, ?2, ?3, ?9, ?4, ?8, iif(f, ?5, 0), iif(f, ?10, 0), iif(f, ?6, ''), iif(f, ?7, ''), iif(f, ?5, 0),
	    iif(rf, ?11, 0), iif(rf, ?14, 0), iif(rf, ?12, ''), iif(rf, ?13, ''), iif(rf, ?15, 0)
	  FROM (SELECT ?5 > 0 AND ?5 > ?3 AS f, ?11 > 0 AND ?11 > ?4 AS rf) WHERE true
	ON CONFLICT(root_id, board_id) DO UPDATE SET read_at = max(read_at, ?3), read_wall = iif(?3 > read_at, ?9, read_wall),
	    reconciled_at = max(reconciled_at, ?4), reconciled_wall = iif(?4 > reconciled_at, ?8, reconciled_wall),
	    failed_at = {failed_at}, failed_wall = {failed_wall},
	    failure_kind = {failure_kind}, failure_message = {failure_message}, outcome_at = {outcome_at},
	    reconcile_failed_at = {reconcile_failed_at}, reconcile_failed_wall = {reconcile_failed_wall},
	    reconcile_failure_kind = {reconcile_failure_kind}, reconcile_failure_message = {reconcile_failure_message},
	    reconcile_refused = {reconcile_refused}
	WHERE ?3 > read_at OR ?4 > reconciled_at OR {failed_at} IS NOT failed_at OR {failure_kind} IS NOT failure_kind
	   OR {failure_message} IS NOT failure_message OR {outcome_at} IS NOT outcome_at
	   OR {reconcile_failed_at} IS NOT reconcile_failed_at OR {reconcile_failure_kind} IS NOT reconcile_failure_kind
	   OR {reconcile_failure_message} IS NOT reconcile_failure_message OR {reconcile_refused} IS NOT reconcile_refused
	RETURNING ` + mirrorSyncOutcomeColumns

var mirrorSyncAttemptSQL = strings.NewReplacer(
	"{cleared}", `(failure_kind <> '' AND ?3 > 0 AND ?3 >= outcome_at)`,
	"{rcleared}", `(reconcile_failure_kind <> '' AND ?4 > 0 AND ?4 >= reconcile_failed_at)`,
).Replace(strings.NewReplacer(
	"{admitted}", `(?5 > 0 AND ?5 > max(outcome_at, read_at, ?3))`,
	"{radmitted}", `(?11 > 0 AND ?11 > max(reconcile_failed_at, reconciled_at, ?4))`,
).Replace(strings.NewReplacer(
	"{failed_at}", `iif({admitted}, ?5, iif({cleared}, 0, failed_at))`,
	"{failed_wall}", `iif({admitted}, ?10, iif({cleared}, 0, failed_wall))`,
	"{failure_kind}", `iif({admitted}, ?6, iif({cleared}, '', failure_kind))`,
	"{failure_message}", `iif({admitted}, ?7, iif({cleared}, '', failure_message))`,
	"{outcome_at}", `iif({admitted}, ?5, iif({cleared}, ?3, outcome_at))`,
	"{reconcile_failed_at}", `iif({radmitted}, ?11, iif({rcleared}, 0, reconcile_failed_at))`,
	"{reconcile_failed_wall}", `iif({radmitted}, ?14, iif({rcleared}, 0, reconcile_failed_wall))`,
	"{reconcile_failure_kind}", `iif({radmitted}, ?12, iif({rcleared}, '', reconcile_failure_kind))`,
	"{reconcile_failure_message}", `iif({radmitted}, ?13, iif({rcleared}, '', reconcile_failure_message))`,
	"{reconcile_refused}", `iif({radmitted}, ?15, iif({rcleared}, 0, reconcile_refused))`,
).Replace(mirrorSyncAttemptTemplate)))

// mirrorSyncOutcomeSQL reads a scope's outcome, for an attempt that
// changed nothing.
var mirrorSyncOutcomeSQL = `SELECT ` + mirrorSyncOutcomeColumns + ` FROM tracker_cache_sync WHERE root_id = ?1 AND board_id = ?2`

// RecordMirrorSyncAttempt records the outcome of one tick of a scope in
// one upsert, after the root check (and a board's lifetime check) every
// mirror write makes, and returns the outcome the scope holds after it and
// whether it changed; an attempt that changes nothing writes nothing and
// reads the outcome in the same transaction. A read moves the read time
// forward and clears the failure recorded at or before it; a completed
// reconcile does the same of the reconcile time and the reconcile
// failure. A failure replaces the one of its slot only when it is later
// than that slot's last success, failure and clear, this attempt's success
// included (superseded). A board scope must be listed in a.Lifetime
// (ErrTrackerCacheUnlisted).
func (s *Store) RecordMirrorSyncAttempt(rootID int64, boardID string, a MirrorSyncAttempt) (out MirrorSyncOutcome, changed bool, err error) {
	times, err := a.in(rootID)
	if err != nil {
		return MirrorSyncOutcome{}, false, err
	}
	if err := a.check(boardID, times); err != nil {
		return MirrorSyncOutcome{}, false, err
	}
	rf := a.ReconcileFailure
	action := fmt.Sprintf("store: record tracker mirror %d sync attempt of board %q", rootID, boardID)
	write := func(tx *sql.Tx) error {
		out, changed = MirrorSyncOutcome{}, false
		err := tx.QueryRow(mirrorSyncAttemptSQL, rootID, boardID, times.succeeded.at, times.reconciled.at,
			times.failed.at, a.Failure.Kind, a.Failure.Message, times.reconciled.wall, times.succeeded.wall, times.failed.wall,
			times.reconcileFailed.at, rf.Kind, rf.Message, rf.Returned, rf.Refused).Scan(out.outcomeDest()...)
		switch {
		case err == nil:
			changed = true
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		return tx.QueryRow(mirrorSyncOutcomeSQL, rootID, boardID).Scan(out.outcomeDest()...)
	}
	if boardID == "" {
		err = s.mirrorWrite(rootID, action, write)
	} else {
		err = s.mirrorBoardWrite(rootID, boardID, a.Lifetime, action, false, func(tx *sql.Tx, _ mirrorBoard) error { return write(tx) })
	}
	if err != nil {
		return MirrorSyncOutcome{}, false, err
	}
	return out, changed, nil
}
