package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidTimeEntry is a tracker_time_entries write the table refuses.
var ErrInvalidTimeEntry = errors.New("store: invalid time entry")

// ErrTimeEntryOpen is an entry for a ticket and day that already have an
// entry not yet posted (idx_tracker_time_entries_open).
var ErrTimeEntryOpen = errors.New("store: the ticket already has an unposted entry that day")

// The states of a time entry (migration v173).
const (
	TimeEntryDrafted = "drafted"
	TimeEntrySent    = "sent"
	TimeEntryPosted  = "posted"
	TimeEntryUnknown = "unknown"
	TimeEntryLeftOut = "left_out"
)

// Who last wrote a time entry.
const (
	TimeSourceAgent  = "agent"
	TimeSourcePerson = "person"
)

// TimeOffer is a ticket a question offers.
type TimeOffer struct {
	Issue string `json:"issue"`
	Key   string `json:"key"`
	Title string `json:"title"`
}

// TimeEntry is one row of tracker_time_entries. Times are Unix
// milliseconds; Day is YYYY-MM-DD. A question has no issue and may offer
// tickets.
type TimeEntry struct {
	ID         int64
	Connection string
	Author     string
	Day        string
	IssueID    string
	IssueKey   string
	IssueTitle string
	Seconds    int64
	Reason     string
	Offers     []TimeOffer
	Source     string
	ThreadID   string
	CallID     string
	State      string
	Revision   int64
	Refusal    string
	SentMs     int64
	WorklogID  string
	PostedMs   int64
	CreatedMs  int64
	UpdatedMs  int64
}

// TimeEntryRef names an entry as a screen read it.
type TimeEntryRef struct {
	ID       int64
	Revision int64
}

const timeEntryColumns = `id, connection_id, author_id, day, issue_id, issue_key, issue_title, seconds, reason, offers, source,
 thread_id, call_id, state, revision, refusal, sent_ms, worklog_id, posted_ms, created_ms, updated_ms`

func scanTimeEntry(row interface{ Scan(...any) error }) (TimeEntry, error) {
	var e TimeEntry
	var offers string
	err := row.Scan(&e.ID, &e.Connection, &e.Author, &e.Day, &e.IssueID, &e.IssueKey, &e.IssueTitle, &e.Seconds, &e.Reason, &offers,
		&e.Source, &e.ThreadID, &e.CallID, &e.State, &e.Revision, &e.Refusal, &e.SentMs, &e.WorklogID, &e.PostedMs,
		&e.CreatedMs, &e.UpdatedMs)
	if err != nil {
		return TimeEntry{}, err
	}
	if err := json.Unmarshal([]byte(offers), &e.Offers); err != nil {
		return TimeEntry{}, fmt.Errorf("store: time entry %d offers: %w", e.ID, err)
	}
	return e, nil
}

func scanTimeEntries(rows *sql.Rows, err error) ([]TimeEntry, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TimeEntry
	for rows.Next() {
		e, err := scanTimeEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// isOpenConflict reports the unique index's refusal of a second open
// entry for one ticket and day.
func isOpenConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: tracker_time_entries.")
}

// TimeDraftWrite is what a draft write did to its days: the entries it
// made, the drafted entries of its thread it replaced, and the entries
// not yet logged it kept: every drafted, sent or unknown entry of those
// days it did not make, another thread's, the person's, and those for a
// ticket that already had an open entry that day, in place of the new one.
// Posted and left-out entries are not read: a day's settled history does
// not grow the write.
type TimeDraftWrite struct {
	Made     []TimeEntry
	Replaced []TimeEntry
	Kept     []TimeEntry
	// Skipped are the new entries not made because their ticket already
	// had an open entry that day (in Kept).
	Skipped []TimeEntry
}

// Changed reports whether the write changed any row.
func (w TimeDraftWrite) Changed() bool { return len(w.Made)+len(w.Replaced) > 0 }

// ReplaceAgentTimeDays replaces the drafted entries one thread drafted
// on days (each a day some entry of entries names) of author on
// connection with entries, all of that thread, in one transaction: the
// thread's entries still drafted and still the agent's are deleted, and
// every other entry stays, another thread's, the person's and those sent
// or settled. A new entry whose ticket already has an open entry that
// day is skipped. A write that would leave a day's entries not yet logged
// over MaxDaySeconds fails with a TimeDayOverError and writes nothing.
// Entries carry their day, ticket, seconds, reason, offers, thread and
// call; the rest is set here.
func (s *Store) ReplaceAgentTimeDays(connection, author string, entries []TimeEntry, now int64) (TimeDraftWrite, error) {
	var out TimeDraftWrite
	days := map[string]bool{}
	var order []string
	thread := ""
	if len(entries) > 0 {
		thread = entries[0].ThreadID
	}
	for _, e := range entries {
		if e.ThreadID == "" || e.CallID == "" || e.Seconds <= 0 || e.Reason == "" {
			return out, fmt.Errorf("%w: an agent entry needs its thread, call, time and reason", ErrInvalidTimeEntry)
		}
		if e.ThreadID != thread {
			return out, fmt.Errorf("%w: one draft is of one thread, not %s and %s", ErrInvalidTimeEntry, thread, e.ThreadID)
		}
		if !days[e.Day] {
			days[e.Day] = true
			order = append(order, e.Day)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, fmt.Errorf("store: draft time: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, day := range order {
		replaced, err := scanTimeEntries(tx.Query(`DELETE FROM tracker_time_entries
 WHERE connection_id = ?1 AND author_id = ?2 AND day = ?3 AND thread_id = ?4 AND source = 'agent' AND state = 'drafted'
 RETURNING `+timeEntryColumns, connection, author, day, thread))
		if err != nil {
			return out, fmt.Errorf("store: draft time: replace %s: %w", day, err)
		}
		out.Replaced = append(out.Replaced, replaced...)
	}
	for _, e := range entries {
		e.Connection, e.Author, e.Source, e.State, e.Revision, e.CreatedMs, e.UpdatedMs = connection, author, TimeSourceAgent, TimeEntryDrafted, 1, now, now
		made, err := insertTimeEntryTx(tx, e)
		if isOpenConflict(err) {
			out.Skipped = append(out.Skipped, e)
			continue
		}
		if err != nil {
			return out, fmt.Errorf("store: draft time: %w", err)
		}
		out.Made = append(out.Made, made)
	}
	for _, day := range order {
		if err := checkDayTx(tx, connection, author, day); err != nil {
			return out, err
		}
	}
	madeIDs := make([]int64, len(out.Made))
	for i, m := range out.Made {
		madeIDs[i] = m.ID
	}
	made, err := json.Marshal(madeIDs)
	if err != nil {
		return out, fmt.Errorf("store: draft time: %w", err)
	}
	for _, day := range order {
		kept, err := scanTimeEntries(tx.Query(`SELECT `+timeEntryColumns+` FROM tracker_time_entries
 WHERE connection_id = ?1 AND author_id = ?2 AND day = ?3 AND state IN ('drafted', 'sent', 'unknown')
 AND id NOT IN (SELECT value FROM json_each(?4))
 ORDER BY id`, connection, author, day, string(made)))
		if err != nil {
			return out, fmt.Errorf("store: draft time: read %s: %w", day, err)
		}
		out.Kept = append(out.Kept, kept...)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("store: draft time: commit: %w", err)
	}
	return out, nil
}

// MaxDaySeconds is the most time a day's entries not yet logged hold.
const MaxDaySeconds = 86400

// ErrTimeDayOver is a write that would put the time of a day's entries
// not yet logged (drafted, sent or unknown) over MaxDaySeconds.
var ErrTimeDayOver = errors.New("store: the day's entries would hold over 24h")

// TimeDayOverError is ErrTimeDayOver for Day, which would hold Seconds.
type TimeDayOverError struct {
	Day     string
	Seconds int64
}

func (e *TimeDayOverError) Error() string {
	return fmt.Sprintf("store: %s would hold %ds of entries not yet logged, over %ds", e.Day, e.Seconds, MaxDaySeconds)
}

func (e *TimeDayOverError) Is(target error) bool { return target == ErrTimeDayOver }

// dayTotalTx is the time of author's entries not yet logged on day of
// connection, as the caller's transaction holds them.
func dayTotalTx(tx *sql.Tx, connection, author, day string) (int64, error) {
	var total int64
	if err := tx.QueryRow(`SELECT coalesce(sum(seconds), 0) FROM tracker_time_entries
 WHERE connection_id = ?1 AND author_id = ?2 AND day = ?3 AND state IN ('drafted', 'sent', 'unknown')`, connection, author, day).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: the time of %s: %w", day, err)
	}
	return total, nil
}

// checkDayTx fails with a TimeDayOverError when author's entries not yet
// logged on day of connection hold more than MaxDaySeconds, as the
// caller's transaction left them.
func checkDayTx(tx *sql.Tx, connection, author, day string) error {
	total, err := dayTotalTx(tx, connection, author, day)
	if err != nil {
		return err
	}
	if total > MaxDaySeconds {
		return &TimeDayOverError{Day: day, Seconds: total}
	}
	return nil
}

func insertTimeEntryTx(tx *sql.Tx, e TimeEntry) (TimeEntry, error) {
	if e.Offers == nil {
		e.Offers = []TimeOffer{}
	}
	offers, err := json.Marshal(e.Offers)
	if err != nil {
		return TimeEntry{}, err
	}
	row := tx.QueryRow(`INSERT INTO tracker_time_entries(connection_id, author_id, day, issue_id, issue_key, issue_title, seconds, reason,
 offers, source, thread_id, call_id, state, revision, created_ms, updated_ms)
 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16) RETURNING `+timeEntryColumns,
		e.Connection, e.Author, e.Day, e.IssueID, e.IssueKey, e.IssueTitle, e.Seconds, e.Reason, string(offers), e.Source,
		e.ThreadID, e.CallID, e.State, e.Revision, e.CreatedMs, e.UpdatedMs)
	return scanTimeEntry(row)
}

// timeEntriesShown bounds one read of entries: a year of a working
// day's entries.
const timeEntriesShown = 5000

// timeEntriesOfSQL reads connection ?1's entries of author ?2 from day ?3
// to ?4, by day and ID.
var timeEntriesOfSQL = `SELECT ` + timeEntryColumns + ` FROM tracker_time_entries
 WHERE connection_id = ?1 AND author_id = ?2 AND day >= ?3 AND day <= ?4 ORDER BY day, id LIMIT ` + strconv.Itoa(timeEntriesShown)

// TimeEntriesOf are author's entries on connection whose day is from to
// to, both YYYY-MM-DD and included, by day and ID.
func (s *Store) TimeEntriesOf(connection, author, from, to string) ([]TimeEntry, error) {
	out, err := scanTimeEntries(s.reader().Query(timeEntriesOfSQL, connection, author, from, to))
	if err != nil {
		return nil, fmt.Errorf("store: time entries of %s: %w", author, err)
	}
	return out, nil
}

// ThreadTimeEntries are the entries thread's agent drafted that still
// exist, on every connection, by ID.
func (s *Store) ThreadTimeEntries(thread string) ([]TimeEntry, error) {
	out, err := scanTimeEntries(s.reader().Query(`SELECT `+timeEntryColumns+` FROM tracker_time_entries
 WHERE thread_id = ?1 AND thread_id <> '' ORDER BY id LIMIT `+strconv.Itoa(timeEntriesShown), thread))
	if err != nil {
		return nil, fmt.Errorf("store: time entries of thread %s: %w", thread, err)
	}
	return out, nil
}

// OpenTimeEntriesShown bounds one read of open entries: one more than a
// post takes (tracker.MaxWorklogInputs), so a reader sees that it holds
// too many.
const OpenTimeEntriesShown = 251

// openTimeEntriesSQL reads connection ?1's entries of author ?2 not yet
// logged (drafted, sent or unknown) whose day ?3, a JSON array, names, by
// day and ID.
var openTimeEntriesSQL = `SELECT ` + timeEntryColumns + ` FROM tracker_time_entries
 WHERE connection_id = ?1 AND author_id = ?2 AND day IN (SELECT value FROM json_each(?3)) AND state IN ('drafted', 'sent', 'unknown')
 ORDER BY day, id LIMIT ` + strconv.Itoa(OpenTimeEntriesShown)

// OpenTimeEntries are author's entries on connection not yet logged
// (drafted, sent or unknown, questions included) whose day is one of
// days, each YYYY-MM-DD, by day and ID: at most OpenTimeEntriesShown.
func (s *Store) OpenTimeEntries(connection, author string, days []string) ([]TimeEntry, error) {
	list, err := json.Marshal(days)
	if err != nil {
		return nil, fmt.Errorf("store: open time entries: %w", err)
	}
	out, err := scanTimeEntries(s.reader().Query(openTimeEntriesSQL, connection, author, string(list)))
	if err != nil {
		return nil, fmt.Errorf("store: open time entries of %s: %w", author, err)
	}
	return out, nil
}

// TimeEntriesByID are the entries ids name on connection that exist, in
// one read, by ID.
func (s *Store) TimeEntriesByID(connection string, ids []int64) ([]TimeEntry, error) {
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("store: time entries: %w", err)
	}
	out, err := scanTimeEntries(s.reader().Query(timeEntriesByIDSQL, connection, string(list)))
	if err != nil {
		return nil, fmt.Errorf("store: time entries: %w", err)
	}
	return out, nil
}

var timeEntriesByIDSQL = `SELECT ` + timeEntryColumns + ` FROM tracker_time_entries
 WHERE id IN (SELECT value FROM json_each(?2)) AND +connection_id = ?1 ORDER BY id`

// TimeEntryChange is the person's change of a drafted entry: the members
// set change, and LeaveOut closes a question with nothing posted. The
// entry becomes the person's.
type TimeEntryChange struct {
	Issue    *TimeOffer
	Seconds  *int64
	Reason   *string
	LeaveOut bool
}

// MaxTimeEntryReason is the longest reason an entry holds, in bytes.
const MaxTimeEntryReason = 1024

// TimeEntryEdited is what EditTimeEntry did. Entry is the entry as
// stored: the one edited or, for a question merged, its ticket's entry
// with the question's time added. Merged is that question, deleted.
type TimeEntryEdited struct {
	Entry  TimeEntry
	Merged *TimeEntry
}

// TimeEntryOpenError is an edit onto a ticket whose open entry that day,
// Open, it cannot merge into: Open is being sent or of unknown outcome,
// or the entry edited is not a question.
type TimeEntryOpenError struct{ Open TimeEntry }

func (e *TimeEntryOpenError) Error() string {
	return fmt.Sprintf("store: %s already has a %s entry on %s", e.Open.IssueKey, e.Open.State, e.Open.Day)
}

func (e *TimeEntryOpenError) Is(target error) bool { return target == ErrTimeEntryOpen }

// ErrTimeEntryMergeOver is a question whose merge would put its ticket's
// entry's reason over MaxTimeEntryReason.
var ErrTimeEntryMergeOver = errors.New("store: the merged entry's reason would be over 1 KiB")

// editTimeEntrySQL applies a change to drafted entry ?2 on connection ?1
// at revision ?3.
var editTimeEntrySQL = `UPDATE tracker_time_entries SET
 issue_id = iif(` + intParam("?4") + ` = 1, ?5, issue_id), issue_key = iif(` + intParam("?4") + ` = 1, ?6, issue_key),
 issue_title = iif(` + intParam("?4") + ` = 1, ?7, issue_title), offers = iif(` + intParam("?4") + ` = 1, '[]', offers),
 seconds = iif(` + intParam("?8") + ` > 0, ` + intParam("?8") + `, seconds), reason = iif(?9 <> '', ?9, reason),
 state = iif(` + intParam("?10") + ` = 1, 'left_out', state), source = 'person', refusal = '',
 revision = revision + 1, updated_ms = ` + intParam("?11") + `
 WHERE id = ` + intParam("?2") + ` AND connection_id = ?1 AND revision = ` + intParam("?3") + ` AND state = 'drafted'
 RETURNING ` + timeEntryColumns

// EditTimeEntry applies ch to author's entry ref on connection when it
// is drafted at ref's revision, in one transaction, and returns it as stored. false
// is a lost race: the entry moved on or is gone. A question answered with
// a ticket that has a drafted entry that day merges into that entry: its
// time is added, its reason ends the entry's as ", plus <reason>", the
// entry becomes the person's, and the question is deleted. Any other edit
// onto a ticket with an open entry that day fails with a
// TimeEntryOpenError, a merge past MaxTimeEntryReason with
// ErrTimeEntryMergeOver, and an edit or merge that would put the day's
// entries not yet logged over MaxDaySeconds with a TimeDayOverError.
func (s *Store) EditTimeEntry(connection, author string, ref TimeEntryRef, ch TimeEntryChange, now int64) (TimeEntryEdited, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return TimeEntryEdited{}, false, fmt.Errorf("store: edit time entry %d: begin: %w", ref.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := scanTimeEntry(tx.QueryRow(`SELECT `+timeEntryColumns+` FROM tracker_time_entries
 WHERE id = `+intParam("?2")+` AND connection_id = ?1 AND author_id = ?4 AND revision = `+intParam("?3")+` AND state = 'drafted'`, connection, ref.ID, ref.Revision, author))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TimeEntryEdited{}, false, nil
	case err != nil:
		return TimeEntryEdited{}, false, fmt.Errorf("store: edit time entry %d: %w", ref.ID, err)
	}
	var out TimeEntryEdited
	if ch.Issue != nil && ch.Issue.Issue != cur.IssueID {
		open, err := scanTimeEntry(tx.QueryRow(`SELECT `+timeEntryColumns+` FROM tracker_time_entries
 WHERE connection_id = ?1 AND author_id = ?2 AND day = ?3 AND issue_id = `+textParam("?4")+` AND issue_id <> ''
 AND state IN ('drafted', 'sent', 'unknown')`, connection, cur.Author, cur.Day, ch.Issue.Issue))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return TimeEntryEdited{}, false, fmt.Errorf("store: edit time entry %d: the ticket's open entry: %w", ref.ID, err)
		case cur.IssueID != "" || open.State != TimeEntryDrafted:
			return TimeEntryEdited{}, false, &TimeEntryOpenError{Open: open}
		default:
			out, err = mergeQuestionTx(tx, cur, open, ch, now)
			if err != nil {
				return TimeEntryEdited{}, false, err
			}
		}
	}
	if out.Merged == nil {
		issue, key, title, setIssue := "", "", "", 0
		if ch.Issue != nil {
			issue, key, title, setIssue = ch.Issue.Issue, ch.Issue.Key, ch.Issue.Title, 1
		}
		var seconds int64
		if ch.Seconds != nil {
			seconds = *ch.Seconds
		}
		reason := ""
		if ch.Reason != nil {
			reason = *ch.Reason
		}
		leave := 0
		if ch.LeaveOut {
			leave = 1
		}
		out.Entry, err = scanTimeEntry(tx.QueryRow(editTimeEntrySQL, connection, ref.ID, ref.Revision, setIssue, issue, key, title, seconds, reason, leave, now))
		if err != nil {
			return TimeEntryEdited{}, false, fmt.Errorf("store: edit time entry %d: %w", ref.ID, err)
		}
	}
	if err := checkDayTx(tx, connection, cur.Author, cur.Day); err != nil {
		return TimeEntryEdited{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return TimeEntryEdited{}, false, fmt.Errorf("store: edit time entry %d: commit: %w", ref.ID, err)
	}
	return out, true, nil
}

// mergeQuestionTx adds question q, as ch changes its time and reason, to
// drafted entry into, and deletes q.
func mergeQuestionTx(tx *sql.Tx, q, into TimeEntry, ch TimeEntryChange, now int64) (TimeEntryEdited, error) {
	seconds, phrase := q.Seconds, q.Reason
	if ch.Seconds != nil {
		seconds = *ch.Seconds
	}
	if ch.Reason != nil {
		phrase = *ch.Reason
	}
	if seconds+into.Seconds > MaxDaySeconds {
		// Past the column's bound, which the day's bound covers.
		total, err := dayTotalTx(tx, q.Connection, q.Author, q.Day)
		if err != nil {
			return TimeEntryEdited{}, err
		}
		return TimeEntryEdited{}, &TimeDayOverError{Day: q.Day, Seconds: total - q.Seconds + seconds}
	}
	seconds += into.Seconds
	reason := strings.TrimRight(strings.TrimSpace(into.Reason), ".") + ", plus " + strings.TrimSpace(phrase)
	if len(reason) > MaxTimeEntryReason {
		return TimeEntryEdited{}, ErrTimeEntryMergeOver
	}
	merged, err := scanTimeEntry(tx.QueryRow(`UPDATE tracker_time_entries SET seconds = `+intParam("?3")+`, reason = ?4,
 source = 'person', refusal = '', revision = revision + 1, updated_ms = `+intParam("?5")+`
 WHERE id = `+intParam("?2")+` AND connection_id = ?1 AND state = 'drafted'
 RETURNING `+timeEntryColumns, into.Connection, into.ID, seconds, reason, now))
	if err != nil {
		return TimeEntryEdited{}, fmt.Errorf("store: merge time entry %d into %d: %w", q.ID, into.ID, err)
	}
	gone, err := scanTimeEntry(tx.QueryRow(`DELETE FROM tracker_time_entries WHERE id = `+intParam("?2")+` AND connection_id = ?1
 RETURNING `+timeEntryColumns, q.Connection, q.ID))
	if err != nil {
		return TimeEntryEdited{}, fmt.Errorf("store: merge time entry %d into %d: delete it: %w", q.ID, into.ID, err)
	}
	return TimeEntryEdited{Entry: merged, Merged: &gone}, nil
}

// refRows reads ?2, a JSON list of [ID, revision] pairs, as rows r.
const refRows = `WITH r AS MATERIALIZED (SELECT json_extract(value, '$[0]') AS id, json_extract(value, '$[1]') AS revision FROM json_each(?2))`

// A statement that finds its rows by ID (id IN r) writes +connection_id
// and +author_id, so the planner reads the IDs by primary key instead of
// the connection's whole day index.

// atRefRevision is a statement's test that its row is at the revision r
// names for it, after the row was found by ID (id IN r).
const atRefRevision = `revision = (SELECT r.revision FROM r WHERE r.id = tracker_time_entries.id)`

var removeTimeEntriesSQL = refRows + `
 DELETE FROM tracker_time_entries WHERE id IN (SELECT id FROM r) AND +connection_id = ?1 AND +author_id = ?3
 AND state IN ('drafted', 'unknown', 'left_out') AND ` + atRefRevision + `
 RETURNING ` + timeEntryColumns

// RemoveTimeEntries deletes author's entries refs name on connection that
// are drafted, unknown or left out at their revision, and returns those it
// deleted. An entry being sent or posted stays.
func (s *Store) RemoveTimeEntries(connection, author string, refs []TimeEntryRef) ([]TimeEntry, error) {
	list, err := refList(refs)
	if err != nil {
		return nil, err
	}
	out, err := scanTimeEntries(s.db.Query(removeTimeEntriesSQL, connection, list, author))
	if err != nil {
		return nil, fmt.Errorf("store: remove time entries: %w", err)
	}
	return out, nil
}

func refList(refs []TimeEntryRef) (string, error) {
	pairs := make([][2]int64, len(refs))
	for i, r := range refs {
		pairs[i] = [2]int64{r.ID, r.Revision}
	}
	b, err := json.Marshal(pairs)
	if err != nil {
		return "", fmt.Errorf("store: time entry refs: %w", err)
	}
	return string(b), nil
}

var markTimeEntriesSentSQL = refRows + `
 UPDATE tracker_time_entries SET state = 'sent', sent_ms = ` + intParam("?3") + `, refusal = '', revision = revision + 1,
 updated_ms = ` + intParam("?3") + `
 WHERE id IN (SELECT id FROM r) AND +connection_id = ?1 AND issue_id <> '' AND state = 'drafted' AND ` + atRefRevision + `
 RETURNING ` + timeEntryColumns

// MarkTimeEntriesSent marks the entries refs name on connection, each
// with a ticket and drafted at its revision, as sent at now, in one
// statement before anything is sent, and returns those it marked. sent
// means that this run is sending the entry: nothing else changes or
// removes it until its settle (SettleTimeEntries), and an earlier run's
// are unknown before this run reads any (RecoverEarlierTimeSends). An
// unknown entry is never marked again: no read proves its first send did
// not land. An entry another post marked since is left out. Which of them
// a post may send is the caller's rule (trackerapp's PostTimeEntries).
func (s *Store) MarkTimeEntriesSent(connection string, refs []TimeEntryRef, now int64) ([]TimeEntry, error) {
	if now <= 0 {
		return nil, fmt.Errorf("%w: a mark needs when it is sent", ErrInvalidTimeEntry)
	}
	list, err := refList(refs)
	if err != nil {
		return nil, err
	}
	out, err := scanTimeEntries(s.db.Query(markTimeEntriesSentSQL, connection, list, now))
	if err != nil {
		return nil, fmt.Errorf("store: mark time entries sent: %w", err)
	}
	return out, nil
}

// TimeEntrySettle is what became of one entry a run marked sent, at the
// revision the mark left it.
type TimeEntrySettle struct {
	ID       int64
	Revision int64
	// State is posted, unknown (the answer was lost) or drafted (refused
	// or not sent).
	State     string
	WorklogID string
	Refusal   string
}

// SettleTimeEntries settles each entry of outcomes on connection that is
// still sent at the revision its mark left, in one transaction, and
// returns them as stored. A posted or unknown entry keeps the time it was
// sent; a drafted one has none.
func (s *Store) SettleTimeEntries(connection string, outcomes []TimeEntrySettle, now int64) ([]TimeEntry, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: settle time entries: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var out []TimeEntry
	for _, o := range outcomes {
		switch o.State {
		case TimeEntryPosted, TimeEntryDrafted, TimeEntryUnknown:
		default:
			return nil, fmt.Errorf("%w: settle %d as %q", ErrInvalidTimeEntry, o.ID, o.State)
		}
		e, err := scanTimeEntry(tx.QueryRow(`UPDATE tracker_time_entries SET state = ?3,
 sent_ms = iif(?3 = 'drafted', 0, sent_ms),
 worklog_id = iif(?3 = 'posted', ?4, ''), posted_ms = iif(?3 = 'posted', `+intParam("?6")+`, 0), refusal = ?5,
 revision = revision + 1, updated_ms = `+intParam("?6")+`
 WHERE id = `+intParam("?2")+` AND connection_id = ?1 AND state = 'sent' AND revision = `+intParam("?7")+`
 RETURNING `+timeEntryColumns, connection, o.ID, o.State, o.WorklogID, o.Refusal, now, o.Revision))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: settle time entry %d: %w", o.ID, err)
		}
		out = append(out, e)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: settle time entries: commit: %w", err)
	}
	return out, nil
}

// recoverTimeSendsSQL reads the sent entries through the open-entry index
// (a sent entry has a ticket), so its cost is the entries not yet logged,
// not the table's settled history.
var recoverTimeSendsSQL = `UPDATE tracker_time_entries SET state = 'unknown', revision = revision + 1, updated_ms = ` + intParam("?1") + `
 WHERE state IN ('drafted', 'sent', 'unknown') AND issue_id <> '' AND state = 'sent'
 RETURNING ` + timeEntryColumns

// RecoverEarlierTimeSends makes every sent entry unknown, at now, and
// returns them. An entry is sent only while a run sends it, so the caller
// calls this once per process before it marks any (trackerapp's
// recoverEarlier): each sent entry it finds is one an earlier process
// stopped sending before it stored the tracker's answer, whose post may
// have happened. The time it was sent stays.
func (s *Store) RecoverEarlierTimeSends(now int64) ([]TimeEntry, error) {
	out, err := scanTimeEntries(s.db.Query(recoverTimeSendsSQL, now))
	if err != nil {
		return nil, fmt.Errorf("store: recover the time entries an earlier process was sending: %w", err)
	}
	return out, nil
}
