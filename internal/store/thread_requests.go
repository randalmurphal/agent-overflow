package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// Request kinds. `remind` has no target: the clock settles it.
const (
	ThreadRequestSpawn  = "spawn"
	ThreadRequestSend   = "send"
	ThreadRequestAsk    = "ask"
	ThreadRequestRemind = "remind"
)

// Source-row states. `blocked` is deliberately absent: it is derived at wait
// time from the target's live state, never stored.
const (
	ThreadRequestUnconfirmed = "unconfirmed"
	ThreadRequestAccepted    = "accepted"
	ThreadRequestRunning     = "running"
	ThreadRequestReplied     = "replied"
	ThreadRequestFinished    = "finished"
	ThreadRequestErrored     = "errored"
	ThreadRequestCancelled   = "cancelled"
	ThreadRequestInterrupted = "interrupted"
	ThreadRequestExpired     = "expired"
	ThreadRequestRefused     = "refused"
)

// Answer kinds, as reported to the caller beside the answer itself.
const (
	ThreadAnswerReply = "reply"
	ThreadAnswerFinal = "final"
	ThreadAnswerError = "error"
	ThreadAnswerNote  = "note"
)

// Delivery routes for a wake. `draft` is a queued wake that boot recovery
// restored into the composer instead of dispatching.
const (
	ThreadWakeInline = "inline"
	ThreadWakeQueued = "queued"
	ThreadWakeDraft  = "draft"
)

// ThreadRequestRetentionDays is the floor past which both sides of a request
// are deleted. Until then a receipt keeps answering "already ran" to a late
// retry and keeps accepting a late thread_reply.
const ThreadRequestRetentionDays = 30

var threadRequestKinds = map[string]struct{}{
	ThreadRequestSpawn: {}, ThreadRequestSend: {}, ThreadRequestAsk: {}, ThreadRequestRemind: {},
}

var threadRequestStates = map[string]struct{}{
	ThreadRequestUnconfirmed: {}, ThreadRequestAccepted: {}, ThreadRequestRunning: {},
	ThreadRequestReplied: {}, ThreadRequestFinished: {}, ThreadRequestErrored: {},
	ThreadRequestCancelled: {}, ThreadRequestInterrupted: {}, ThreadRequestExpired: {},
	ThreadRequestRefused: {},
}

var threadAnswerKinds = map[string]struct{}{
	"": {}, ThreadAnswerReply: {}, ThreadAnswerFinal: {}, ThreadAnswerError: {}, ThreadAnswerNote: {},
}

var threadWakeRoutes = map[string]struct{}{
	ThreadWakeInline: {}, ThreadWakeQueued: {}, ThreadWakeDraft: {},
}

// ThreadRequest is the source row: one spawn, send, ask or remind this
// computer's threads made.
type ThreadRequest struct {
	Token          string `json:"token"`
	CallerThreadID string `json:"callerThreadId"`
	Kind           string `json:"kind"`
	// DueAt is when the clock settles a `remind`, zero otherwise.
	DueAt int64 `json:"dueAt,omitempty"`
	// TargetComputerID is empty for this computer, else the paired backend
	// id. It is rewritten when the target moves.
	TargetComputerID string `json:"targetComputerId,omitempty"`
	TargetThreadID   string `json:"targetThreadId,omitempty"`
	// TargetThreadTitle is the answering thread's name on another computer,
	// as that computer reported it. A local target has a row here and is
	// named from it, so this stays empty for one.
	TargetThreadTitle string `json:"targetThreadTitle,omitempty"`
	// OriginThreadID is the thread an `ask` forked; empty otherwise.
	OriginThreadID string `json:"originThreadId,omitempty"`
	// Notify is whether a wake is owed on settlement. Every positive wait
	// that ends unsettled arms it.
	Notify bool   `json:"notify"`
	State  string `json:"state"`
	// Answer is the whole settled text, never truncated.
	Answer          []byte `json:"-"`
	AnswerKind      string `json:"answerKind,omitempty"`
	LateReply       []byte `json:"-"`
	LateReplyAt     int64  `json:"lateReplyAt,omitempty"`
	Revision        int64  `json:"revision"`
	SettledAt       int64  `json:"settledAt,omitempty"`
	DeliveredAt     int64  `json:"deliveredAt,omitempty"`
	DeliveredHow    string `json:"deliveredHow,omitempty"`
	LateDeliveredAt int64  `json:"lateDeliveredAt,omitempty"`
	// ExpiresAt is the destination's deadline for collecting the answer.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// Polling is whether the poller still visits this row. It is turned off
	// when a settled request's destination has nothing left to report.
	Polling   bool   `json:"-"`
	NextCheck int64  `json:"-"`
	Attempts  int64  `json:"-"`
	Issue     string `json:"issue,omitempty"`
	// WakeAttempts, WakeNextCheck and WakeIssue schedule and bound the
	// retry of a wake the settlement could not hand over.
	WakeAttempts  int64  `json:"-"`
	WakeNextCheck int64  `json:"-"`
	WakeIssue     string `json:"wakeIssue,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// ThreadRequestSettlement is one settled answer, written identically to the
// source row and to the receipt.
type ThreadRequestSettlement struct {
	State      string
	Answer     []byte
	AnswerKind string
	SettledAt  int64
	// ExpiresAt is when an uncollected answer is dropped. The destination
	// sets it to SettledAt plus a day; the source copies what the
	// destination reported.
	ExpiresAt int64
}

func (s ThreadRequestSettlement) validate(action string, states map[string]struct{}) error {
	if _, ok := states[s.State]; !ok || s.State == "" {
		return fmt.Errorf("%s: invalid settled state %q", action, s.State)
	}
	if _, ok := threadAnswerKinds[s.AnswerKind]; !ok {
		return fmt.Errorf("%s: invalid answer kind %q", action, s.AnswerKind)
	}
	return nil
}

const threadRequestColumns = `token, caller_thread_id, kind, COALESCE(due_at, 0),
    target_computer_id, target_thread_id, target_thread_title, origin_thread_id, notify, state,
    answer, answer_kind, late_reply, COALESCE(late_reply_at, 0), revision,
    COALESCE(settled_at, 0), COALESCE(delivered_at, 0), delivered_how,
    COALESCE(late_delivered_at, 0), COALESCE(expires_at, 0),
    polling, next_check, attempts, issue,
    wake_attempts, wake_next_check, wake_issue, created_at, updated_at`

func scanThreadRequest(row rowScanner) (ThreadRequest, error) {
	var r ThreadRequest
	var notify, polling int
	err := row.Scan(
		&r.Token, &r.CallerThreadID, &r.Kind, &r.DueAt,
		&r.TargetComputerID, &r.TargetThreadID, &r.TargetThreadTitle, &r.OriginThreadID, &notify, &r.State,
		&r.Answer, &r.AnswerKind, &r.LateReply, &r.LateReplyAt, &r.Revision,
		&r.SettledAt, &r.DeliveredAt, &r.DeliveredHow,
		&r.LateDeliveredAt, &r.ExpiresAt,
		&polling, &r.NextCheck, &r.Attempts, &r.Issue,
		&r.WakeAttempts, &r.WakeNextCheck, &r.WakeIssue, &r.CreatedAt, &r.UpdatedAt,
	)
	r.Notify = notify != 0
	r.Polling = polling != 0
	return r, err
}

func collectThreadRequests(rows *sql.Rows, action string) ([]ThreadRequest, error) {
	defer rows.Close()
	out := []ThreadRequest{}
	for rows.Next() {
		row, err := scanThreadRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", action, err)
	}
	return out, nil
}

// InsertThreadRequest writes the source row before anything is dispatched.
// The token is the caller-minted idempotency key, so a duplicate insert is an
// error rather than a silent overwrite: a retry re-reads the existing row.
func (s *Store) InsertThreadRequest(r ThreadRequest) error {
	action := fmt.Sprintf("store: insert thread request %s", r.Token)
	if r.Token == "" || r.CallerThreadID == "" {
		return fmt.Errorf("%s: token and caller thread id are required", action)
	}
	if _, ok := threadRequestKinds[r.Kind]; !ok {
		return fmt.Errorf("%s: invalid kind %q", action, r.Kind)
	}
	if _, ok := threadRequestStates[r.State]; !ok {
		return fmt.Errorf("%s: invalid state %q", action, r.State)
	}
	if _, ok := threadAnswerKinds[r.AnswerKind]; !ok {
		return fmt.Errorf("%s: invalid answer kind %q", action, r.AnswerKind)
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = nowMillis()
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = r.CreatedAt
	}
	if _, err := s.db.Exec(
		`INSERT INTO thread_requests (
		    token, caller_thread_id, kind, due_at, target_computer_id, target_thread_id,
		    origin_thread_id, notify, state, answer, answer_kind, revision,
		    next_check, attempts, issue, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Token, r.CallerThreadID, r.Kind, nilIfZero(r.DueAt), r.TargetComputerID, r.TargetThreadID,
		r.OriginThreadID, boolToInt(r.Notify), r.State, nilIfEmptyBytes(r.Answer), r.AnswerKind, r.Revision,
		r.NextCheck, r.Attempts, r.Issue, r.CreatedAt, r.UpdatedAt,
	); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}

// AdvanceThreadRequestState is the conditional transition every source-side
// state change uses. It reports whether the update applied; false means
// another path (a settlement that arrived during dispatch, a cancel) already
// moved the row, which is a normal outcome and not an error.
func (s *Store) AdvanceThreadRequestState(token, from, to string) (bool, error) {
	action := fmt.Sprintf("store: advance thread request %s", token)
	if _, ok := threadRequestStates[from]; !ok {
		return false, fmt.Errorf("%s: invalid current state %q", action, from)
	}
	if _, ok := threadRequestStates[to]; !ok {
		return false, fmt.Errorf("%s: invalid next state %q", action, to)
	}
	result, err := s.db.Exec(
		`UPDATE thread_requests SET state = ?, updated_at = ? WHERE token = ? AND state = ?`,
		to, nowMillis(), token, from,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// threadRequestOpenStates is the set a settlement may replace: everything
// before an answer was stored.
var threadRequestOpenStates = []string{ThreadRequestUnconfirmed, ThreadRequestAccepted, ThreadRequestRunning}

// SettleThreadRequest stores the whole answer on the source row and bumps the
// revision, provided the row is still in one of `from`. It reports whether it
// applied: a lost race against another collector is a no-op, never an error.
//
// Pass ThreadRequestOpenStates() for the ordinary collection path.
func (s *Store) SettleThreadRequest(token string, from []string, settlement ThreadRequestSettlement) (bool, error) {
	action := fmt.Sprintf("store: settle thread request %s", token)
	if err := settlement.validate(action, threadRequestStates); err != nil {
		return false, err
	}
	clause, args, err := stateInClause(action, from, threadRequestStates)
	if err != nil {
		return false, err
	}
	settledAt := settlement.SettledAt
	if settledAt == 0 {
		settledAt = nowMillis()
	}
	set := []any{settlement.State, nilIfEmptyBytes(settlement.Answer), settlement.AnswerKind,
		settledAt, nilIfZero(settlement.ExpiresAt), nowMillis(), token}
	result, err := s.db.Exec(
		`UPDATE thread_requests
		    SET state = ?, answer = ?, answer_kind = ?, revision = revision + 1,
		        settled_at = ?, expires_at = ?, updated_at = ?
		  WHERE token = ? AND `+clause,
		append(set, args...)...,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// ThreadRequestOpenStates returns the states a settlement may replace.
func ThreadRequestOpenStates() []string {
	return append([]string(nil), threadRequestOpenStates...)
}

// StoreThreadRequestLateReply records a thread_reply that arrived after the
// request had already settled. It is a new revision, so a `thread_status`
// waiting past the collected revision returns and the wake path delivers a
// second message. Only the first late reply is stored.
func (s *Store) StoreThreadRequestLateReply(token string, reply []byte, at int64) (bool, error) {
	action := fmt.Sprintf("store: store thread request late reply %s", token)
	if at == 0 {
		at = nowMillis()
	}
	result, err := s.db.Exec(
		`UPDATE thread_requests
		    SET late_reply = ?, late_reply_at = ?, revision = revision + 1, updated_at = ?
		  WHERE token = ? AND settled_at IS NOT NULL AND late_reply IS NULL`,
		nilIfEmptyBytes(reply), at, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// MarkThreadRequestDelivered records how the answer reached the caller. It
// runs on the caller's transaction so the queue insert and the delivery mark
// commit together: a repeated observation cannot inject the wake twice.
//
// Only a settled row can be delivered. An open row may already hold an
// answer (a reminder stores its note when it is armed), and marking that
// one delivered would disarm the wake the settlement still owes, so the
// condition is checked here rather than left to every caller.
//
// `late` marks the second wake (a late reply) instead of the first.
func MarkThreadRequestDeliveredTx(tx *sql.Tx, token, how string, at int64, late bool) (bool, error) {
	action := fmt.Sprintf("store: mark thread request %s delivered", token)
	if _, ok := threadWakeRoutes[how]; !ok {
		return false, fmt.Errorf("%s: invalid delivery route %q", action, how)
	}
	if at == 0 {
		at = nowMillis()
	}
	openStates, states, err := stateInClause(action, threadRequestOpenStates, threadRequestStates)
	if err != nil {
		return false, err
	}
	settled := " AND NOT (" + openStates + ")"
	// The retry schedule is cleared with the delivery: what it counted is
	// over, and the row's second wake starts its own attempts from zero.
	retry := `, wake_attempts = 0, wake_next_check = 0, wake_issue = ''`
	query := `UPDATE thread_requests SET delivered_at = ?, delivered_how = ?, updated_at = ?` + retry +
		` WHERE token = ? AND delivered_at IS NULL` + settled
	if late {
		query = `UPDATE thread_requests SET late_delivered_at = ?, delivered_how = ?, updated_at = ?` + retry +
			` WHERE token = ? AND late_reply IS NOT NULL AND late_delivered_at IS NULL` + settled
	}
	args := append([]any{at, how, nowMillis(), token}, states...)
	result, err := tx.Exec(query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// MarkThreadRequestDelivered is MarkThreadRequestDeliveredTx on its own
// transaction, for inline delivery, which has no queue row to write.
func (s *Store) MarkThreadRequestDelivered(token, how string, at int64, late bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin mark thread request %s delivered: %w", token, err)
	}
	defer tx.Rollback()
	changed, err := MarkThreadRequestDeliveredTx(tx, token, how, at, late)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit mark thread request %s delivered: %w", token, err)
	}
	return changed, nil
}

// SetThreadRequestTarget records where a request is being answered: the
// computer that accepted it, and the thread that owns the answer, which for an
// `ask` is the scratch fork the destination reports back. It is unconditional
// on state because the target moves for reasons the state says nothing about:
// a conversation transferred to another computer is re-pointed while the
// request is still running. It reports whether the row existed.
//
// `title` names that thread on another computer, where this store has no row
// to read it from. A local target is named from its own row, so it passes an
// empty title.
func (s *Store) SetThreadRequestTarget(token, computerID, threadID, title string) (bool, error) {
	action := fmt.Sprintf("store: set thread request %s target", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests
		    SET target_computer_id = ?, target_thread_id = ?, target_thread_title = ?, updated_at = ?
		  WHERE token = ?`,
		computerID, threadID, title, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// SetThreadRequestNotify arms or disarms the wake a settlement owes the
// caller. A timed-out wait arms it; archiving the caller disarms it.
func (s *Store) SetThreadRequestNotify(token string, notify bool) (bool, error) {
	action := fmt.Sprintf("store: set thread request %s notify", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET notify = ?, updated_at = ? WHERE token = ? AND notify IS NOT ?`,
		boolToInt(notify), nowMillis(), token, boolToInt(notify),
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// SetThreadRequestsNotifyForCaller disarms (or arms) every open request one
// thread owns. Archiving, deleting or moving the caller uses it so no wake
// lands on a thread that cannot receive it.
func (s *Store) SetThreadRequestsNotifyForCaller(callerThreadID string, notify bool) (int64, error) {
	action := fmt.Sprintf("store: set thread requests notify for %s", callerThreadID)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET notify = ?, updated_at = ?
		  WHERE caller_thread_id = ? AND delivered_at IS NULL AND notify IS NOT ?`,
		boolToInt(notify), nowMillis(), callerThreadID, boolToInt(notify),
	)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: rows affected: %w", action, err)
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
// bring the retry round again at once.
func (s *Store) ScheduleThreadRequestWake(token string, nextCheck, attempts int64) error {
	action := fmt.Sprintf("store: schedule thread request wake %s", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET wake_next_check = ?, wake_attempts = ?, updated_at = ?
		  WHERE token = ?`,
		nextCheck, attempts, nowMillis(), token,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return requireRowsAffected(result, action)
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
	return rowsChanged(result, action)
}

// GetThreadRequest reads one source row. The bool separates "no such token"
// from a read failure, because the two get different answers on the wire.
func (s *Store) GetThreadRequest(token string) (ThreadRequest, bool, error) {
	row, err := scanThreadRequest(s.reader().QueryRow(
		`SELECT `+threadRequestColumns+` FROM thread_requests WHERE token = ?`, token))
	if err == sql.ErrNoRows {
		return ThreadRequest{}, false, nil
	}
	if err != nil {
		return ThreadRequest{}, false, fmt.Errorf("store: get thread request %s: %w", token, err)
	}
	return row, true, nil
}

// GetThreadRequests reads the named source rows in one query, newest first. A
// token with no row is simply absent from the result; the caller reports it as
// unknown.
func (s *Store) GetThreadRequests(tokens []string) ([]ThreadRequest, error) {
	if len(tokens) == 0 {
		return []ThreadRequest{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(tokens)), ",")
	args := make([]any, len(tokens))
	for i, token := range tokens {
		args[i] = token
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE token IN (`+placeholders+`) ORDER BY created_at DESC, token ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: get thread requests: %w", err)
	}
	return collectThreadRequests(rows, "store: get thread requests")
}

// ListThreadRequestsByCaller returns one thread's requests, open ones first
// and newest first within each group, which is how `thread_status` lists them
// and how an agent recovers its tokens after a compaction. limit and offset
// page the list; the tool layer encodes them into its opaque cursor.
func (s *Store) ListThreadRequestsByCaller(callerThreadID string, limit, offset int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE caller_thread_id = ?
		  ORDER BY (settled_at IS NULL) DESC, created_at DESC, token ASC
		  LIMIT `+strconv.Itoa(limit)+` OFFSET ?`,
		callerThreadID, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list thread requests for %s: %w", callerThreadID, err)
	}
	return collectThreadRequests(rows, fmt.Sprintf("store: list thread requests for %s", callerThreadID))
}

// ListOpenThreadRequestsForComputer returns the unsettled requests one
// paired computer owes this one, oldest first. Forgetting that computer
// reads it twice: once to refuse with the list, once to settle them.
func (s *Store) ListOpenThreadRequestsForComputer(computerID string, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 50
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE target_computer_id = ? AND settled_at IS NULL
		  ORDER BY created_at ASC, token ASC LIMIT `+strconv.Itoa(limit),
		computerID)
	if err != nil {
		return nil, fmt.Errorf("store: list open thread requests for computer %s: %w", computerID, err)
	}
	return collectThreadRequests(rows, fmt.Sprintf("store: list open thread requests for computer %s", computerID))
}

// DueThreadReminders returns the `remind` rows whose clock has passed. The
// partial index idx_thread_requests_due serves the predicate, which is
// restated here in full so SQLite can prove it.
func (s *Store) DueThreadReminders(now int64, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE kind = 'remind' AND state = 'accepted' AND due_at <= ?
		  ORDER BY due_at ASC LIMIT `+strconv.Itoa(limit), now)
	if err != nil {
		return nil, fmt.Errorf("store: list due thread reminders: %w", err)
	}
	return collectThreadRequests(rows, "store: list due thread reminders")
}

// DueThreadRequestPolls returns the remote requests the poller should visit.
// The predicate matches idx_thread_requests_poll exactly, including the
// `finished` state a late reply still needs polling for and the `polling`
// gate that retires a row once there is nothing left to ask about.
func (s *Store) DueThreadRequestPolls(now int64, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE polling = 1
		    AND target_computer_id <> ''
		    AND state IN ('unconfirmed','accepted','running','finished')
		    AND next_check <= ?
		  ORDER BY next_check ASC LIMIT `+strconv.Itoa(limit), now)
	if err != nil {
		return nil, fmt.Errorf("store: list due thread request polls: %w", err)
	}
	return collectThreadRequests(rows, "store: list due thread request polls")
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
		    AND ((settled_at IS NOT NULL AND delivered_at IS NULL)
		      OR (late_reply IS NOT NULL AND late_delivered_at IS NULL))
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
// anything to do: a reminder to fire, a remote request to poll, a wake to
// retry. They are reported apart because the sweep can leave one out (a poll
// already in flight) without going blind to the others.
type ThreadRequestSchedule struct {
	Reminder ScheduledMoment
	Poll     ScheduledMoment
	Wake     ScheduledMoment
}

// NextThreadRequestWork reports when the unattended passes next have work.
// On most computers nothing is scheduled, and the sweep sleeps on that answer
// rather than asking three questions a second for the life of the process.
//
// One query, three indexed minima, so the answer describes one snapshot.
func (s *Store) NextThreadRequestWork() (ThreadRequestSchedule, error) {
	var reminder, poll, wake sql.NullInt64
	if err := s.reader().QueryRow(
		`SELECT
		   (SELECT MIN(due_at) FROM thread_requests
		     WHERE kind = 'remind' AND state = 'accepted'),
		   (SELECT MIN(next_check) FROM thread_requests
		     WHERE polling = 1 AND target_computer_id <> ''
		       AND state IN ('unconfirmed','accepted','running','finished')),
		   (SELECT MIN(wake_next_check) FROM thread_requests
		     WHERE notify = 1
		       AND ((settled_at IS NOT NULL AND delivered_at IS NULL)
		         OR (late_reply IS NOT NULL AND late_delivered_at IS NULL)))`,
	).Scan(&reminder, &poll, &wake); err != nil {
		return ThreadRequestSchedule{}, fmt.Errorf("store: read next thread request work: %w", err)
	}
	moment := func(value sql.NullInt64) ScheduledMoment {
		return ScheduledMoment{At: value.Int64, Set: value.Valid}
	}
	return ThreadRequestSchedule{Reminder: moment(reminder), Poll: moment(poll), Wake: moment(wake)}, nil
}

// HasOpenThreadRequests reports whether a thread still owns an undelivered
// request. The copy refusal and the forget-computer refusal read it.
func (s *Store) HasOpenThreadRequests(callerThreadID string) (bool, error) {
	var open bool
	if err := s.reader().QueryRow(
		`SELECT EXISTS(SELECT 1 FROM thread_requests
		   WHERE caller_thread_id = ? AND settled_at IS NULL)`, callerThreadID,
	).Scan(&open); err != nil {
		return false, fmt.Errorf("store: probe open thread requests for %s: %w", callerThreadID, err)
	}
	return open, nil
}

// DeleteThreadRequest drops one source row. `thread_cancel` on a reminder is
// the only caller: every other request keeps its row as lineage for
// `thread_cancel` by thread id until the retention floor.
func (s *Store) DeleteThreadRequest(token string) (bool, error) {
	action := fmt.Sprintf("store: delete thread request %s", token)
	result, err := s.db.Exec(`DELETE FROM thread_requests WHERE token = ?`, token)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// DeleteThreadRequestsBefore removes both sides of every request that has
// been settled longer than the retention floor. Past it a token stops
// answering, so a late reply gets `thread_request_unknown` rather than
// resurrecting a month-old exchange.
//
// Two rows survive the floor whatever their age. An OPEN row is still work:
// deleting it would leave a receipt nobody can settle or cancel, so the row
// waits for its own settlement first. And a row's age is measured from the
// moment it finished, not from when it was made: a reminder armed for next
// month is due in the future and older than the floor at the same time, and
// an answer written yesterday on a request made last year has been readable
// for a day.
func (s *Store) DeleteThreadRequestsBefore(cutoff int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin thread request retention sweep: %w", err)
	}
	defer tx.Rollback()
	sweeps := []struct {
		table string
		open  []string
		// age is the terminal moment the floor is measured against,
		// falling back through what the row has.
		age string
	}{
		{"thread_requests", threadRequestOpenStates, "COALESCE(settled_at, due_at, created_at)"},
		{"thread_request_receipts", threadReceiptOpenStates, "COALESCE(settled_at, created_at)"},
	}
	var total int64
	for _, sweep := range sweeps {
		legal := threadRequestStates
		if sweep.table == "thread_request_receipts" {
			legal = threadReceiptStates
		}
		openClause, openArgs, err := stateInClause("store: sweep "+sweep.table, sweep.open, legal)
		if err != nil {
			return 0, err
		}
		args := append([]any{}, openArgs...)
		args = append(args, cutoff)
		result, err := tx.Exec(`DELETE FROM `+sweep.table+` WHERE NOT `+openClause+
			` AND `+sweep.age+` < ?`, args...)
		if err != nil {
			return 0, fmt.Errorf("store: sweep %s before %d: %w", sweep.table, cutoff, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: count swept %s rows: %w", sweep.table, err)
		}
		total += affected
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit thread request retention sweep: %w", err)
	}
	return total, nil
}

// stateInClause renders a validated `state IN (...)` predicate. A typo would
// otherwise produce a permanent no-op that reads exactly like a lost race.
func stateInClause(action string, states []string, legal map[string]struct{}) (string, []any, error) {
	if len(states) == 0 {
		return "", nil, fmt.Errorf("%s: no expected states given", action)
	}
	args := make([]any, len(states))
	for i, state := range states {
		if _, ok := legal[state]; !ok {
			return "", nil, fmt.Errorf("%s: invalid expected state %q", action, state)
		}
		args[i] = state
	}
	return "state IN (" + strings.TrimRight(strings.Repeat("?,", len(states)), ",") + ")", args, nil
}

// rowsChanged reports whether a conditional update matched. The error path is
// separate from the no-op so a caller can act on a real failure.
func rowsChanged(result sql.Result, action string) (bool, error) {
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: rows affected: %w", action, err)
	}
	return affected > 0, nil
}

func nilIfZero(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nilIfEmptyBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

// QueueThreadWake hands one settled request's wake to the ordinary durable
// message queue and records the delivery in the same transaction: the queue
// row is the message's only copy from that moment on, and `delivered_at` is
// what keeps a second observation of the same settlement from queueing it
// twice.
//
// It is the `persist` hook of the caller's registerQueueItem, so it runs
// BEFORE the in-memory queue register, exactly where InsertFlushQueueItem
// runs for an ordinary send.
//
// `late` marks the second wake (a late reply) instead of the first. A
// delivery mark that does not apply means another path already delivered
// this revision, and the wake is refused rather than duplicated.
func (s *Store) QueueThreadWake(token string, late bool, item FlushQueueItem) error {
	action := fmt.Sprintf("store: queue thread wake %s", token)
	if token == "" {
		return fmt.Errorf("%s: token is required", action)
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer release()
	defer tx.Rollback()
	marked, err := MarkThreadRequestDeliveredTx(tx, token, ThreadWakeQueued, item.EnqueuedAt, late)
	if err != nil {
		return err
	}
	if !marked {
		return fmt.Errorf("%s: the answer was already delivered", action)
	}
	if err := insertFlushQueueItem(tx, item); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// MarkThreadRequestDeliveredAsDraft records that boot recovery restored a
// queued wake into the composer instead of dispatching it. The row is
// already `queued`; this is the correction, so it applies to a delivered
// row rather than an undelivered one.
func (s *Store) MarkThreadRequestDeliveredAsDraft(token string, late bool) (bool, error) {
	action := fmt.Sprintf("store: mark thread request %s restored to draft", token)
	query := `UPDATE thread_requests SET delivered_how = ?, updated_at = ?
	           WHERE token = ? AND delivered_at IS NOT NULL AND delivered_how = ?`
	if late {
		query = `UPDATE thread_requests SET delivered_how = ?, updated_at = ?
		          WHERE token = ? AND late_delivered_at IS NOT NULL AND delivered_how = ?`
	}
	result, err := s.db.Exec(query, ThreadWakeDraft, nowMillis(), token, ThreadWakeQueued)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}
