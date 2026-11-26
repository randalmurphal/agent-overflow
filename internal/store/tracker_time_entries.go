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

// timeEntryRetentionDays is how long a posted or left-out entry is kept
// after its day: the draft write of the same author deletes older ones,
// so the table stays bounded with no sweep.
const timeEntryRetentionDays = 62

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
	SentTo     string
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
 thread_id, call_id, state, revision, refusal, sent_ms, sent_to, worklog_id, posted_ms, created_ms, updated_ms`

func scanTimeEntry(row interface{ Scan(...any) error }) (TimeEntry, error) {
	var e TimeEntry
	var offers string
	err := row.Scan(&e.ID, &e.Connection, &e.Author, &e.Day, &e.IssueID, &e.IssueKey, &e.IssueTitle, &e.Seconds, &e.Reason, &offers,
		&e.Source, &e.ThreadID, &e.CallID, &e.State, &e.Revision, &e.Refusal, &e.SentMs, &e.SentTo, &e.WorklogID, &e.PostedMs,
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
// made, the agent entries it replaced (whichever thread wrote them), and
// the entries it kept: every entry of those days it did not replace,
// those of the person, those sent or posted, and those for a ticket that
// already had an open entry that day, in place of the new one.
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

// ReplaceAgentTimeDays replaces the agent's drafted entries of days (each
// a day some entry of entries names) of author on connection with
// entries, in one transaction: an agent entry still drafted is deleted,
// whichever thread wrote it, and every other entry stays. A new entry
// whose ticket already has an open entry that day is skipped. Entries
// carry their day, ticket, seconds, reason, offers, thread and call; the
// rest is set here. Posted and left-out entries of author older than
// timeEntryRetentionDays before today are deleted with it.
func (s *Store) ReplaceAgentTimeDays(connection, author, today string, entries []TimeEntry, now int64) (TimeDraftWrite, error) {
	var out TimeDraftWrite
	days := map[string]bool{}
	var order []string
	for _, e := range entries {
		if e.ThreadID == "" || e.CallID == "" || e.Seconds <= 0 || e.Reason == "" {
			return out, fmt.Errorf("%w: an agent entry needs its thread, call, time and reason", ErrInvalidTimeEntry)
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
 WHERE connection_id = ?1 AND author_id = ?2 AND day = ?3 AND source = 'agent' AND state = 'drafted'
 RETURNING `+timeEntryColumns, connection, author, day))
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
 WHERE connection_id = ?1 AND author_id = ?2 AND day = ?3 AND id NOT IN (SELECT value FROM json_each(?4))
 ORDER BY id`, connection, author, day, string(made)))
		if err != nil {
			return out, fmt.Errorf("store: draft time: read %s: %w", day, err)
		}
		out.Kept = append(out.Kept, kept...)
	}
	if _, err := tx.Exec(`DELETE FROM tracker_time_entries WHERE connection_id = ?1 AND author_id = ?2
 AND state IN ('posted', 'left_out') AND day < date(?3, '-`+strconv.Itoa(timeEntryRetentionDays)+` days')`, connection, author, today); err != nil {
		return out, fmt.Errorf("store: draft time: retention: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("store: draft time: commit: %w", err)
	}
	return out, nil
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

// TimeEntriesOf are author's entries on connection whose day is from to
// to, both YYYY-MM-DD and included, by day and ID.
func (s *Store) TimeEntriesOf(connection, author, from, to string) ([]TimeEntry, error) {
	out, err := scanTimeEntries(s.reader().Query(`SELECT `+timeEntryColumns+` FROM tracker_time_entries
 WHERE connection_id = ?1 AND author_id = ?2 AND day >= ?3 AND day <= ?4 ORDER BY day, id LIMIT `+strconv.Itoa(timeEntriesShown),
		connection, author, from, to))
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

// TimeEntry is entry id on connection; false when there is none.
func (s *Store) TimeEntry(connection string, id int64) (TimeEntry, bool, error) {
	e, err := scanTimeEntry(s.reader().QueryRow(`SELECT `+timeEntryColumns+` FROM tracker_time_entries
 WHERE id = `+intParam("?1")+` AND connection_id = ?2`, id, connection))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TimeEntry{}, false, nil
	case err != nil:
		return TimeEntry{}, false, fmt.Errorf("store: time entry %d: %w", id, err)
	}
	return e, true, nil
}

// TimeEntryChange is the person's change of a drafted entry: the members
// set change, and LeaveOut closes a question with nothing posted. The
// entry becomes the person's.
type TimeEntryChange struct {
	Issue    *TimeOffer
	Seconds  *int64
	Reason   *string
	LeaveOut bool
}

// EditTimeEntry applies ch to entry ref on connection when it is drafted
// at ref's revision, and returns it as stored. false is a lost race: the
// entry moved on or is gone. A ticket that already has an open entry
// that day fails with ErrTimeEntryOpen.
func (s *Store) EditTimeEntry(connection string, ref TimeEntryRef, ch TimeEntryChange, now int64) (TimeEntry, bool, error) {
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
	e, err := scanTimeEntry(s.db.QueryRow(`UPDATE tracker_time_entries SET
 issue_id = iif(`+intParam("?4")+` = 1, ?5, issue_id), issue_key = iif(`+intParam("?4")+` = 1, ?6, issue_key),
 issue_title = iif(`+intParam("?4")+` = 1, ?7, issue_title), offers = iif(`+intParam("?4")+` = 1, '[]', offers),
 seconds = iif(`+intParam("?8")+` > 0, `+intParam("?8")+`, seconds), reason = iif(?9 <> '', ?9, reason),
 state = iif(`+intParam("?10")+` = 1, 'left_out', state), source = 'person', refusal = '',
 revision = revision + 1, updated_ms = `+intParam("?11")+`
 WHERE id = `+intParam("?2")+` AND connection_id = ?1 AND revision = `+intParam("?3")+` AND state = 'drafted'
 RETURNING `+timeEntryColumns, connection, ref.ID, ref.Revision, setIssue, issue, key, title, seconds, reason, leave, now))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TimeEntry{}, false, nil
	case isOpenConflict(err):
		return TimeEntry{}, false, ErrTimeEntryOpen
	case err != nil:
		return TimeEntry{}, false, fmt.Errorf("store: edit time entry %d: %w", ref.ID, err)
	}
	return e, true, nil
}

// RemoveTimeEntries deletes the entries refs name on connection that are
// drafted, unknown or left out at their revision, and returns those it
// deleted. An entry being sent or posted stays.
func (s *Store) RemoveTimeEntries(connection string, refs []TimeEntryRef) ([]TimeEntry, error) {
	list, err := refList(refs)
	if err != nil {
		return nil, err
	}
	out, err := scanTimeEntries(s.db.Query(`WITH r AS MATERIALIZED (SELECT json_extract(value, '$[0]') AS id, json_extract(value, '$[1]') AS revision FROM json_each(?2))
 DELETE FROM tracker_time_entries WHERE connection_id = ?1 AND state IN ('drafted', 'unknown', 'left_out')
 AND EXISTS (SELECT 1 FROM r WHERE r.id = tracker_time_entries.id AND r.revision = tracker_time_entries.revision)
 RETURNING `+timeEntryColumns, connection, list))
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

// MarkTimeEntriesSent marks the entries refs name on connection, each
// with a ticket and at its revision, drafted or unknown, or sent before
// running (sent_ms before running: a post this process did not make), as
// sent to sentTo at now, in one transaction before anything is sent, and
// returns those it marked. An entry another post marked since is left
// out.
func (s *Store) MarkTimeEntriesSent(connection string, refs []TimeEntryRef, sentTo string, now, running int64) ([]TimeEntry, error) {
	if sentTo == "" || now <= 0 {
		return nil, fmt.Errorf("%w: a mark needs where and when it is sent", ErrInvalidTimeEntry)
	}
	list, err := refList(refs)
	if err != nil {
		return nil, err
	}
	out, err := scanTimeEntries(s.db.Query(`WITH r AS MATERIALIZED (SELECT json_extract(value, '$[0]') AS id, json_extract(value, '$[1]') AS revision FROM json_each(?2))
 UPDATE tracker_time_entries SET state = 'sent', sent_ms = `+intParam("?4")+`, sent_to = ?3, refusal = '', revision = revision + 1,
 updated_ms = `+intParam("?4")+`
 WHERE connection_id = ?1 AND issue_id <> ''
 AND (state IN ('drafted', 'unknown') OR (state = 'sent' AND sent_ms < `+intParam("?5")+`))
 AND EXISTS (SELECT 1 FROM r WHERE r.id = tracker_time_entries.id AND r.revision = tracker_time_entries.revision)
 RETURNING `+timeEntryColumns, connection, list, sentTo, now, running))
	if err != nil {
		return nil, fmt.Errorf("store: mark time entries sent: %w", err)
	}
	return out, nil
}

// TimeEntrySettle is what became of one sent entry.
type TimeEntrySettle struct {
	ID int64
	// State is posted, drafted (refused, or not sent) or unknown.
	State     string
	WorklogID string
	Refusal   string
}

// SettleTimeEntries settles each entry of outcomes on connection that is
// still sent at sentMs, in one transaction, and returns them as stored.
func (s *Store) SettleTimeEntries(connection string, sentMs int64, outcomes []TimeEntrySettle, now int64) ([]TimeEntry, error) {
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
 sent_ms = iif(?3 = 'drafted', 0, sent_ms), sent_to = iif(?3 = 'drafted', '', sent_to),
 worklog_id = iif(?3 = 'posted', ?4, ''), posted_ms = iif(?3 = 'posted', `+intParam("?6")+`, 0), refusal = ?5,
 revision = revision + 1, updated_ms = `+intParam("?6")+`
 WHERE id = `+intParam("?2")+` AND connection_id = ?1 AND state = 'sent' AND sent_ms = `+intParam("?7")+`
 RETURNING `+timeEntryColumns, connection, o.ID, o.State, o.WorklogID, o.Refusal, now, sentMs))
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
