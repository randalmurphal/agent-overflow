package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// DisarmThreadRequestWakesForCaller turns `notify` off on every request one
// thread made. Archiving, deleting or moving the caller uses it so no wake
// lands on a thread that cannot receive it. It covers a request whose first
// answer was already delivered: that row can still take a late reply, and
// an armed one would wake the thread with it. A lineage-only row is already
// disarmed (clearLineagePayloadSQL), and nothing re-arms one.
func (s *Store) DisarmThreadRequestWakesForCaller(callerThreadID string) (int64, error) {
	action := fmt.Sprintf("store: disarm thread request wakes for %s", callerThreadID)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET notify = 0, updated_at = ?
		  WHERE caller_thread_id = ? AND notify = 1`,
		nowMillis(), callerThreadID,
	)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: rows affected: %w", action, err)
	}
	if affected > 0 {
		s.noteThreadRequestChanged(ThreadRequestChange{CallerThreadID: callerThreadID})
	}
	return affected, nil
}

// RescheduleThreadRequest moves the poller's next visit for one remote
// request and records the attempt count and last error text. `issue` is
// cleared by passing an empty string, which a successful poll does.
func (s *Store) RescheduleThreadRequest(token string, nextCheck int64, attempts int64, issue string) error {
	result, err := s.db.Exec(
		`UPDATE thread_requests SET next_check = ?, attempts = ?, issue = ?, updated_at = ?
		  WHERE token = ?`,
		nextCheck, attempts, issue, nowMillis(), token,
	)
	if err != nil {
		return fmt.Errorf("store: reschedule thread request %s: %w", token, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: reschedule thread request %s", token))
}

// DispatchThreadRequest records, before one more attempt of a request on
// another computer is sent, until when that attempt's handler could admit
// it there, and keeps the poller away until then: a status asked while the
// attempt is out could find the token not yet admitted. The deadline never
// moves earlier, since an earlier attempt's handler may still be running.
func (s *Store) DispatchThreadRequest(token string, admitUntil int64) error {
	result, err := s.db.Exec(
		`UPDATE thread_requests SET admit_until = MAX(admit_until, ?1), next_check = ?1, updated_at = ?2
		  WHERE token = ?3`,
		admitUntil, nowMillis(), token,
	)
	if err != nil {
		return fmt.Errorf("store: dispatch thread request %s: %w", token, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: dispatch thread request %s", token))
}

// RetireThreadRequestPoll takes one row out of the poller for good. A
// settled request whose destination has nothing left to report is retired:
// the late reply landed, or the destination's hold on the request ran out.
// It reports whether the row was still being polled.
//
// Retirement is durable because the alternative is a row that costs a call
// to another computer every few seconds until the retention floor deletes
// it, and because nothing recomputes the decision after a restart.
func (s *Store) RetireThreadRequestPoll(token string) (bool, error) {
	action := fmt.Sprintf("store: retire thread request poll %s", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET polling = 0, attempts = 0, issue = '', updated_at = ?
		  WHERE token = ? AND polling = 1`,
		nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// ScheduleThreadRequestWake books the next attempt at one undelivered wake.
// It is written before the attempt, so a delivery that fails or hangs cannot
// bring the retry round again at once. It reports false, booking nothing,
// when the row owes no wake: a delivery since the caller read it cleared the
// schedule, and a booking then would leave a retry on a delivered row.
func (s *Store) ScheduleThreadRequestWake(token string, nextCheck, attempts int64) (bool, error) {
	action := fmt.Sprintf("store: schedule thread request wake %s", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET wake_next_check = ?, wake_attempts = ?, updated_at = ?
		  WHERE token = ? AND notify = 1 AND `+threadWakeOwedSQL,
		nextCheck, attempts, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// NoteThreadRequestWakeIssue records why a wake could not be handed over.
// It is the reason the retry gives up with, and it is kept on the row so a
// person reading the ledger can see what stopped it.
func (s *Store) NoteThreadRequestWakeIssue(token, issue string) error {
	action := fmt.Sprintf("store: note thread request wake issue %s", token)
	if _, err := s.db.Exec(
		`UPDATE thread_requests SET wake_issue = ?, updated_at = ? WHERE token = ?`,
		issue, nowMillis(), token,
	); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}

// AbandonThreadRequestWake ends the retry of a wake that cannot be handed
// over, recording the reason. It disarms `notify`, which is what takes the
// row out of the undelivered set: the answer is still on the row and
// `thread_status` still returns it, so nothing is lost but the message.
func (s *Store) AbandonThreadRequestWake(token, issue string) (bool, error) {
	action := fmt.Sprintf("store: abandon thread request wake %s", token)
	if issue == "" {
		return false, fmt.Errorf("%s: a reason is required", action)
	}
	result, err := s.db.Exec(
		`UPDATE thread_requests SET notify = 0, wake_issue = ?, updated_at = ?
		  WHERE token = ? AND notify = 1`,
		issue, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return s.changedThreadRequest(token, result, action)
}

// DueThreadReminders returns the `remind` rows whose clock has passed. The
// partial index idx_thread_requests_due serves the predicate, which is
// restated here in full so SQLite can prove it. A reminder of a thread
// assistants' pause holds is not due (assistants_paused.go).
func (s *Store) DueThreadReminders(now int64, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE kind = 'remind' AND state = 'accepted' AND due_at <= ?
		    AND `+s.assistantPauseSQL("thread_requests.caller_thread_id")+`
		  ORDER BY due_at ASC LIMIT `+strconv.Itoa(limit), now)
	if err != nil {
		return nil, fmt.Errorf("store: list due thread reminders: %w", err)
	}
	return collectThreadRequests(rows, "store: list due thread reminders")
}

// threadsWithOpenRemindersSQL names each thread with a reminder that has not
// fired, through the partial index idx_thread_requests_due, whose predicate
// it restates.
const threadsWithOpenRemindersSQL = `SELECT DISTINCT caller_thread_id FROM thread_requests INDEXED BY idx_thread_requests_due
	WHERE kind = 'remind' AND state = 'accepted'`

// ThreadsWithOpenReminders names each thread with a reminder that has not
// fired: one read, for the boot's seed of the threads not at rest.
func (s *Store) ThreadsWithOpenReminders() ([]string, error) {
	return s.queryThreadIDs("store: list threads with open reminders", threadsWithOpenRemindersSQL)
}

// ListOpenThreadReminders returns a thread's reminders that have not fired,
// soonest first, at most limit of them, and how many it has in all.
func (s *Store) ListOpenThreadReminders(callerThreadID string, limit int) (_ []ThreadRequest, total int, err error) {
	action := "store: list open thread reminders for " + callerThreadID
	if limit < 1 {
		limit = 32
	}
	// One read transaction, so the count and the listing see the same rows
	// and the count is never below what was listed.
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	if err := tx.QueryRow(`SELECT count(*) FROM thread_requests
		  WHERE caller_thread_id = ? AND kind = 'remind' AND state = 'accepted'`, callerThreadID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("%s: count: %w", action, err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := tx.Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE caller_thread_id = ? AND kind = 'remind' AND state = 'accepted'
		  ORDER BY due_at ASC, token ASC LIMIT `+strconv.Itoa(limit), callerThreadID)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", action, err)
	}
	reminders, err := collectThreadRequests(rows, action)
	if err != nil {
		return nil, 0, err
	}
	return reminders, total, nil
}

// A capable destination reports accepted work as it changes. Its final
// status at the late reply deadline remains due across a suspended process.
const remotePollingSQL = `polling=1 AND target_computer_id <> '' AND state IN ('unconfirmed','accepted','running','finished')`
const clockedOwnerRequestSQL = `polling=1 AND target_computer_id <> '' AND state IN ('unconfirmed','finished')`
const ownerDeadlineSQL = `CASE WHEN state='finished' THEN MAX(next_check,COALESCE(expires_at,settled_at+86400000)) ELSE next_check END`

// Seek one index entry per computer. Each computer's clock then seeks its
// earliest deadline using the index for its advertised protocol.
const pollingComputersCTE = `computers(id) AS (SELECT '' UNION ALL
 SELECT (SELECT target_computer_id FROM thread_requests WHERE ` + remotePollingSQL + `
  AND target_computer_id > computers.id ORDER BY target_computer_id LIMIT 1)
 FROM computers WHERE id IS NOT NULL),
 modes(id,capable) AS (SELECT id, id IN (SELECT value FROM json_each(?1) WHERE value IS NOT NULL)
 FROM computers WHERE id <> '')`

const earliestRemotePollSQL = `WITH RECURSIVE ` + pollingComputersCTE + `
 SELECT MIN(CASE WHEN capable THEN (SELECT MIN(` + ownerDeadlineSQL + `) FROM thread_requests
  WHERE target_computer_id=m.id AND ` + clockedOwnerRequestSQL + `)
 ELSE (SELECT MIN(next_check) FROM thread_requests WHERE target_computer_id=m.id AND ` + remotePollingSQL + `) END)
 FROM modes m`

func dueRemotePollSQL(limit int) string {
	return `WITH RECURSIVE ` + pollingComputersCTE + `,
 due(key,at) AS (
 SELECT token,next_check FROM modes m CROSS JOIN thread_requests WHERE target_computer_id=m.id AND NOT m.capable
 AND ` + remotePollingSQL + ` AND next_check <= ?2
 UNION ALL SELECT token,` + ownerDeadlineSQL + ` FROM modes m CROSS JOIN thread_requests WHERE target_computer_id=m.id AND m.capable
 AND ` + clockedOwnerRequestSQL + ` AND ` + ownerDeadlineSQL + ` <= ?2
 ORDER BY 2 LIMIT ` + strconv.Itoa(limit) + `)
 SELECT ` + threadRequestColumns + ` FROM due CROSS JOIN thread_requests WHERE token=due.key ORDER BY due.at`
}

// DueThreadRequestPolls returns the remote requests the poller should visit.
// The predicate matches idx_thread_requests_poll exactly, including the
// `finished` state a late reply still needs polling for and the `polling`
// gate that retires a row once there is nothing left to ask about.
func (s *Store) DueThreadRequestPolls(now int64, limit int, owners []string) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	devices, err := json.Marshal(owners)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(dueRemotePollSQL(limit), string(devices), now)
	if err != nil {
		return nil, fmt.Errorf("store: list due thread request polls: %w", err)
	}
	return collectThreadRequests(rows, "store: list due thread request polls")
}

// OpenThreadRequestPollsOn returns the requests the given threads still have
// open on one computer, due or not, soonest first. A poll for a due request
// asks about its thread's other open ones with it, so answers written there
// by one event (a turn that consumed several messages, a restart) are
// collected in one pass. An `unconfirmed` row is left out: the call that
// makes it has not returned, and asking early would read a request the
// destination has not seen as one it refused.
//
// The predicate repeats idx_thread_requests_poll's so the read stays on the
// outstanding rows. The unary plus keeps the caller term out of index
// selection: idx_thread_requests_caller would read every request the callers
// ever made and sort them.
const openThreadRequestPollsOnSQL = `SELECT ` + threadRequestColumns + ` FROM thread_requests
		  WHERE polling = 1
		    AND target_computer_id <> ''
		    AND state IN ('unconfirmed','accepted','running','finished')
		    AND target_computer_id = ?
		    AND state IN ('accepted','running')
		    AND +caller_thread_id IN (SELECT value FROM json_each(?))
		  ORDER BY next_check ASC LIMIT `

func (s *Store) OpenThreadRequestPollsOn(computerID string, callerThreadIDs []string, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	callers, err := json.Marshal(callerThreadIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list open thread request polls on %s: %w", computerID, err)
	}
	rows, err := s.reader().Query(openThreadRequestPollsOnSQL+strconv.Itoa(limit), computerID, string(callers))
	if err != nil {
		return nil, fmt.Errorf("store: list open thread request polls on %s: %w", computerID, err)
	}
	return collectThreadRequests(rows, "store: list open thread request polls on "+computerID)
}

// The owed rule: a row has an answer no route has delivered when its first
// answer is owed or its late reply is. A late reply supersedes a first answer
// that was never handed over, as it does in `thread_status`, so such a row
// owes the reply alone. ThreadRequest.WakeOwed states the same rule for a row
// already read; TestWakeOwedAgreesWithItsSQL holds the two together.
const (
	threadFirstWakeOwedSQL = `(settled_at IS NOT NULL AND delivered_at IS NULL AND late_reply IS NULL)`
	threadLateWakeOwedSQL  = `(late_reply IS NOT NULL AND late_delivered_at IS NULL)`
	threadWakeOwedSQL      = `(` + threadFirstWakeOwedSQL + ` OR ` + threadLateWakeOwedSQL + `)`
)

// WakeOwed reports whether the row's first answer (late false) or its late
// reply (late true) is still to be delivered. `notify` is not part of it:
// that is whether anyone is to be told.
func (r ThreadRequest) WakeOwed(late bool) bool {
	if late {
		return len(r.LateReply) > 0 && r.LateDeliveredAt == 0
	}
	return r.SettledAt != 0 && r.DeliveredAt == 0 && len(r.LateReply) == 0
}

// UndeliveredThreadRequestWakes returns the settled requests that still owe
// their caller a message: `notify` is armed, the retry's own clock has come
// round, and the answer, or a late reply, has never been handed over. Oldest
// first, so a backlog drains in the order the answers arrived.
//
// It is the recovery read behind the wake: the settlement and the wake are
// separate transactions, and neither poll query revisits a settled row.
func (s *Store) UndeliveredThreadRequestWakes(now int64, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE notify = 1
		    AND wake_next_check <= ?
		    AND `+threadWakeOwedSQL+`
		  ORDER BY settled_at ASC, token ASC LIMIT `+strconv.Itoa(limit), now)
	if err != nil {
		return nil, fmt.Errorf("store: list undelivered thread request wakes: %w", err)
	}
	return collectThreadRequests(rows, "store: list undelivered thread request wakes")
}

// ScheduledMoment is a time the ledger has something due, or nothing (Set is
// false). It is separate from the moment because a row waiting on the zero
// moment is due now, not idle.
type ScheduledMoment struct {
	At  int64
	Set bool
}

// ThreadRequestSchedule is the earliest moment each unattended pass has
// anything to do: a reminder to fire, a remote request to poll or an
// acknowledgement owed to its computer, a wake to retry. They are reported
// apart because the sweep can leave one out (a poll already in flight)
// without going blind to the others.
type ThreadRequestSchedule struct {
	Reminder ScheduledMoment
	Poll     ScheduledMoment
	Wake     ScheduledMoment
	// OwnerOp is the earliest attempt of an operation owed to a paired
	// computer (thread_owner_ops.go).
	OwnerOp ScheduledMoment
}

// NextThreadRequestWork reports when the unattended passes next have work.
// On most computers nothing is scheduled, and the sweep sleeps on that answer
// rather than asking three questions a second for the life of the process.
//
// One query, five indexed minima and two probes, so the answer describes
// one snapshot.
func (s *Store) NextThreadRequestWork(owners []string) (ThreadRequestSchedule, error) {
	devices, err := json.Marshal(owners)
	if err != nil {
		return ThreadRequestSchedule{}, err
	}
	var reminder, poll, wake, ownerOp sql.NullInt64
	if err := s.reader().QueryRow(
		`SELECT
		   (SELECT MIN(due_at) FROM thread_requests
		     WHERE kind = 'remind' AND state = 'accepted'
		       AND `+s.assistantPauseSQL("thread_requests.caller_thread_id")+`),
		   (SELECT MIN(next_check) FROM (
		     SELECT (`+earliestRemotePollSQL+`) AS next_check
		     UNION ALL
		     SELECT MIN(next_check) FROM thread_requests WHERE `+ackOwedSQL+`)),
		   (SELECT MIN(wake_next_check) FROM thread_requests
		     WHERE notify = 1 AND `+threadWakeOwedSQL+`),
		   (SELECT MIN(next_at) FROM thread_owner_ops)`, string(devices),
	).Scan(&reminder, &poll, &wake, &ownerOp); err != nil {
		return ThreadRequestSchedule{}, fmt.Errorf("store: read next thread request work: %w", err)
	}
	moment := func(value sql.NullInt64) ScheduledMoment {
		return ScheduledMoment{At: value.Int64, Set: value.Valid}
	}
	return ThreadRequestSchedule{
		Reminder: moment(reminder), Poll: moment(poll), Wake: moment(wake),
		OwnerOp: moment(ownerOp),
	}, nil
}
