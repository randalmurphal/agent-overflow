package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
// restored into the composer instead of dispatching; nothing writes it now,
// since a wake that never reached the agent goes back to its request
// (WithdrawThreadWake), but rows written before keep it.
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
	// TargetProvider is that thread's provider on another computer, as its
	// computer reported it; empty for a local target, read from its row.
	TargetProvider string `json:"targetProvider,omitempty"`
	// NestedOpen is whether the last agent_rows answer for a spawn on
	// another computer listed open work, which keeps the settled spawn
	// listed in the Agents segment.
	NestedOpen bool `json:"-"`
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
	// ConfirmedAt and LateConfirmedAt record the provider's echo of a
	// queued answer and late reply. A queued one without it may never have
	// reached the agent. An inline delivery is confirmed when it is marked.
	ConfirmedAt     int64 `json:"-"`
	LateConfirmedAt int64 `json:"-"`
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
	// LineageOnly marks a local spawn row the retention sweep kept past
	// the floor with its payload cleared: the record of which thread
	// started its target, and nothing else. The ledger reads skip it.
	LineageOnly bool  `json:"-"`
	CreatedAt   int64 `json:"createdAt"`
	UpdatedAt   int64 `json:"updatedAt"`
	// AnswerAgent is set when the thread that answered is a standing agent,
	// as recorded with the answer; nil for any other thread.
	AnswerAgent *ThreadAnswerAgent `json:"-"`
	// AnswerAgentUnrecorded is true on a row written before v149, which
	// recorded nothing about the thread that answered.
	AnswerAgentUnrecorded bool `json:"-"`
}

// ThreadAnswerAgent names the thread that answered a request as a standing
// agent. Role is its role line as the wake quotes it (agenthome.BoundRole),
// empty when its instructions.md holds none.
type ThreadAnswerAgent struct {
	Role string
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
	// Agent is the answering thread as a standing agent, read by the
	// receipt's settlement when the answer is written and copied to the
	// source row with it.
	Agent *ThreadAnswerAgent
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
    wake_attempts, wake_next_check, wake_issue, lineage_only, created_at, updated_at,
    COALESCE(confirmed_at, 0), COALESCE(late_confirmed_at, 0), answer_agent, answer_agent_role,
    target_provider, nested_open`

func scanThreadRequest(row rowScanner) (ThreadRequest, error) {
	var r ThreadRequest
	var notify, polling, lineageOnly, nestedOpen int
	var answerAgent sql.NullInt64
	var answerAgentRole string
	err := row.Scan(
		&r.Token, &r.CallerThreadID, &r.Kind, &r.DueAt,
		&r.TargetComputerID, &r.TargetThreadID, &r.TargetThreadTitle, &r.OriginThreadID, &notify, &r.State,
		&r.Answer, &r.AnswerKind, &r.LateReply, &r.LateReplyAt, &r.Revision,
		&r.SettledAt, &r.DeliveredAt, &r.DeliveredHow,
		&r.LateDeliveredAt, &r.ExpiresAt,
		&polling, &r.NextCheck, &r.Attempts, &r.Issue,
		&r.WakeAttempts, &r.WakeNextCheck, &r.WakeIssue, &lineageOnly, &r.CreatedAt, &r.UpdatedAt,
		&r.ConfirmedAt, &r.LateConfirmedAt, &answerAgent, &answerAgentRole,
		&r.TargetProvider, &nestedOpen,
	)
	r.NestedOpen = nestedOpen != 0
	r.AnswerAgent = answerAgentOf(answerAgent.Int64, answerAgentRole)
	r.AnswerAgentUnrecorded = !answerAgent.Valid
	r.Notify = notify != 0
	r.Polling = polling != 0
	r.LineageOnly = lineageOnly != 0
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
		    next_check, attempts, issue, lineage_only, created_at, updated_at, answer_agent
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		r.Token, r.CallerThreadID, r.Kind, nilIfZero(r.DueAt), r.TargetComputerID, r.TargetThreadID,
		r.OriginThreadID, boolToInt(r.Notify), r.State, nilIfEmptyBytes(r.Answer), r.AnswerKind, r.Revision,
		r.NextCheck, r.Attempts, r.Issue, boolToInt(r.LineageOnly), r.CreatedAt, r.UpdatedAt,
	); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: r.Token, CallerThreadID: r.CallerThreadID})
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
	return s.changedThreadRequest(token, result, action)
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
	agent, role := answerAgentColumns(settlement.Agent)
	set := []any{settlement.State, nilIfEmptyBytes(settlement.Answer), settlement.AnswerKind,
		settledAt, nilIfZero(settlement.ExpiresAt), agent, role, nowMillis(), token}
	result, err := s.db.Exec(
		`UPDATE thread_requests
		    SET state = ?, answer = ?, answer_kind = ?, revision = revision + 1,
		        settled_at = ?, expires_at = ?, answer_agent = ?, answer_agent_role = ?, updated_at = ?
		  WHERE token = ? AND `+clause,
		append(set, args...)...,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return s.changedThreadRequest(token, result, action)
}

// ThreadRequestOpenStates returns the states a settlement may replace.
func ThreadRequestOpenStates() []string {
	return append([]string(nil), threadRequestOpenStates...)
}

// StoreThreadRequestLateReply records a thread_reply that arrived after the
// request had already settled. It is a new revision, so a `thread_status`
// waiting past the collected revision returns and the wake path delivers a
// second message. Only the first late reply is stored, and never on a
// lineage-only row, whose token the ledger no longer knows. agent is the
// replying thread as a standing agent, recorded by the receipt with the
// reply; it replaces the one the first answer recorded, since the late
// reply's wake names it.
func (s *Store) StoreThreadRequestLateReply(token string, reply []byte, at int64, agent *ThreadAnswerAgent) (bool, error) {
	action := fmt.Sprintf("store: store thread request late reply %s", token)
	if at == 0 {
		at = nowMillis()
	}
	isAgent, role := answerAgentColumns(agent)
	result, err := s.db.Exec(
		`UPDATE thread_requests
		    SET late_reply = ?, late_reply_at = ?, answer_agent = ?, answer_agent_role = ?, revision = revision + 1, updated_at = ?
		  WHERE token = ? AND settled_at IS NOT NULL AND late_reply IS NULL AND lineage_only = 0`,
		nilIfEmptyBytes(reply), at, isAgent, role, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return s.changedThreadRequest(token, result, action)
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
// A queued delivery is unconfirmed until the provider echoes it
// (ConfirmThreadWake); an inline one is confirmed by this mark, since the
// tool reply that carried it was the agent's.
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
	query := `UPDATE thread_requests SET delivered_at = ?, delivered_how = ?, confirmed_at = ?, updated_at = ?` + retry +
		` WHERE token = ? AND delivered_at IS NULL` + settled
	if late {
		query = `UPDATE thread_requests SET late_delivered_at = ?, delivered_how = ?, late_confirmed_at = ?, updated_at = ?` + retry +
			` WHERE token = ? AND late_reply IS NOT NULL AND late_delivered_at IS NULL` + settled
	}
	var confirmed any
	if how != ThreadWakeQueued {
		confirmed = at
	}
	args := append([]any{at, how, confirmed, nowMillis(), token}, states...)
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
	if changed {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return changed, nil
}

// SetThreadRequestTarget records where a request is being answered: the
// computer that accepted it, and the thread that owns the answer, which for an
// `ask` is the scratch fork the destination reports back. It is unconditional
// on state because the target moves for reasons the state says nothing about:
// a conversation transferred to another computer is re-pointed while the
// request is still running. It reports whether the row existed. A write
// that changes nothing is not reported as a change (OnThreadRequestChanged):
// the poller restates the target on every visit.
//
// `title` and `provider` name that thread on another computer, where this
// store has no row to read them from. A local target is named from its own
// row, so it passes them empty.
func (s *Store) SetThreadRequestTarget(token, computerID, threadID, title, provider string) (bool, error) {
	action := fmt.Sprintf("store: set thread request %s target", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests
		    SET target_computer_id = ?, target_thread_id = ?, target_thread_title = ?, target_provider = ?, updated_at = ?
		  WHERE token = ? AND (target_computer_id IS NOT ? OR target_thread_id IS NOT ? OR target_thread_title IS NOT ?
		    OR target_provider IS NOT ?)`,
		computerID, threadID, title, provider, nowMillis(), token, computerID, threadID, title, provider,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	changed, err := s.changedThreadRequest(token, result, action)
	if err != nil || changed {
		return changed, err
	}
	var exists bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_requests WHERE token = ?)`, token).Scan(&exists); err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return exists, nil
}

// SetThreadRequestNotify arms or disarms the wake a settlement owes the
// caller. A timed-out wait arms it; archiving the caller disarms it. A
// lineage-only row owes nothing and is never re-armed.
func (s *Store) SetThreadRequestNotify(token string, notify bool) (bool, error) {
	action := fmt.Sprintf("store: set thread request %s notify", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET notify = ?, updated_at = ?
		  WHERE token = ? AND notify IS NOT ? AND lineage_only = 0`,
		boolToInt(notify), nowMillis(), token, boolToInt(notify),
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return s.changedThreadRequest(token, result, action)
}

// SetThreadRequestNestedOpen records whether the last agent_rows answer for
// a spawn on another computer listed open work. It reports whether the row
// changed; a lineage-only row is left alone.
func (s *Store) SetThreadRequestNestedOpen(token string, open bool) (bool, error) {
	action := fmt.Sprintf("store: set thread request %s nested open", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET nested_open = ?, updated_at = ?
		  WHERE token = ? AND nested_open IS NOT ? AND lineage_only = 0`,
		boolToInt(open), nowMillis(), token, boolToInt(open),
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return s.changedThreadRequest(token, result, action)
}

// ClearThreadRequestsNestedOpenForComputer clears nested_open on every
// spawn to one computer, for a computer no longer paired: nothing will
// answer for it again. It returns the callers whose rows changed. A
// forgotten computer is rare, so the read is the table's.
func (s *Store) ClearThreadRequestsNestedOpenForComputer(computerID string) ([]string, error) {
	rows, err := s.db.Query(
		`UPDATE thread_requests SET nested_open = 0, updated_at = ?
		  WHERE target_computer_id = ? AND target_computer_id <> '' AND nested_open = 1 AND lineage_only = 0
		  RETURNING caller_thread_id`,
		nowMillis(), computerID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: clear nested open for computer %s: %w", computerID, err)
	}
	var callers []string
	for rows.Next() {
		var caller string
		if err := rows.Scan(&caller); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: clear nested open for computer %s: %w", computerID, err)
		}
		if !slices.Contains(callers, caller) {
			callers = append(callers, caller)
		}
	}
	// The update finishes when its rows close: either error is the write's.
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: clear nested open for computer %s: %w", computerID, err)
	}
	return callers, nil
}

// ListNestedOpenRemoteSpawns returns the settled spawns on other computers
// whose last agent_rows answer listed open work, after the one settled at
// afterSettledAt with afterToken, in settled order: the rows the poller
// reads agent_rows for again until that work is done. The poller pages
// through them by the last row of each page, so a row that stays open
// does not keep the ones after it from being asked.
func (s *Store) ListNestedOpenRemoteSpawns(afterSettledAt int64, afterToken string, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		limit = 32
	}
	rows, err := s.reader().Query(nestedOpenRemoteSpawnsSQL(limit), afterSettledAt, afterToken)
	if err != nil {
		return nil, fmt.Errorf("store: list nested-open remote spawns: %w", err)
	}
	return collectThreadRequests(rows, "store: list nested-open remote spawns")
}

// nestedOpenRemoteSpawnsSQL reads one page of
// idx_thread_requests_nested_open, whose predicate it states.
func nestedOpenRemoteSpawnsSQL(limit int) string {
	return `SELECT ` + threadRequestColumns + ` FROM thread_requests
	  WHERE nested_open = 1 AND settled_at IS NOT NULL AND kind = 'spawn'
	    AND target_computer_id <> '' AND lineage_only = 0
	    AND (settled_at, token) > (?, ?)
	  ORDER BY settled_at ASC, token ASC LIMIT ` + strconv.Itoa(limit)
}

// SetThreadRequestsNotifyForCaller disarms (or arms) every open request one
// thread owns. Archiving, deleting or moving the caller uses it so no wake
// lands on a thread that cannot receive it. Lineage-only rows are left
// disarmed.
func (s *Store) SetThreadRequestsNotifyForCaller(callerThreadID string, notify bool) (int64, error) {
	action := fmt.Sprintf("store: set thread requests notify for %s", callerThreadID)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET notify = ?, updated_at = ?
		  WHERE caller_thread_id = ? AND delivered_at IS NULL AND notify IS NOT ? AND lineage_only = 0`,
		boolToInt(notify), nowMillis(), callerThreadID, boolToInt(notify),
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
	return s.changedThreadRequest(token, result, action)
}

// GetThreadRequest reads one source row. The bool separates "no such token"
// from a read failure, because the two get different answers on the wire.
// A lineage-only row is no such token: past the retention floor a spawn's
// token is unknown, as a deleted row's is. The ledger reads below skip it
// the same way; the lineage reads (spawnLineageSQL, threadColumns) do not.
func (s *Store) GetThreadRequest(token string) (ThreadRequest, bool, error) {
	row, err := scanThreadRequest(s.reader().QueryRow(
		`SELECT `+threadRequestColumns+` FROM thread_requests WHERE token = ? AND lineage_only = 0`, token))
	if err == sql.ErrNoRows {
		return ThreadRequest{}, false, nil
	}
	if err != nil {
		return ThreadRequest{}, false, fmt.Errorf("store: get thread request %s: %w", token, err)
	}
	return row, true, nil
}

// GetThreadRequests reads the named source rows in one query, newest first. A
// token with no row, or only a lineage-only one, is simply absent from the
// result; the caller reports it as unknown.
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
		  WHERE token IN (`+placeholders+`) AND lineage_only = 0
		  ORDER BY created_at DESC, token ASC`, args...)
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
		  WHERE caller_thread_id = ? AND lineage_only = 0
		  ORDER BY (settled_at IS NULL) DESC, created_at DESC, token ASC
		  LIMIT `+strconv.Itoa(limit)+` OFFSET ?`,
		callerThreadID, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list thread requests for %s: %w", callerThreadID, err)
	}
	return collectThreadRequests(rows, fmt.Sprintf("store: list thread requests for %s", callerThreadID))
}

// ListThreadAgentRequests returns the spawns, sends and asks one thread
// made that its activity rail lists as agents, oldest first: every spawn,
// and a send or ask whose answer the thread is still owed a wake for
// (`notify`) or is parked waiting on (one of `waiting`). Each is open,
// settled at or after settledSince, or a spawn something under it is still
// open in: a local spawn whose thread, or a thread under it through local
// spawns down to depth levels, has a request open or a remote spawn whose
// computer last reported open work; or a remote spawn whose computer last
// reported open work (`nested_open`). That is the row the open work nests
// under. At most limit rows, and whether there were more.
func (s *Store) ListThreadAgentRequests(callerThreadID string, waiting []string, settledSince int64, depth, limit int) (_ []ThreadRequest, more bool, err error) {
	action := "store: list agent requests for " + callerThreadID
	if limit < 1 {
		limit = 64
	}
	if depth < 1 {
		depth = 1
	}
	if waiting == nil {
		waiting = []string{}
	}
	tokens, err := json.Marshal(waiting)
	if err != nil {
		return nil, false, fmt.Errorf("%s: encode waiting tokens: %w", action, err)
	}
	rows, err := s.reader().Query(threadAgentRequestsSQL+strconv.Itoa(limit+1),
		callerThreadID, string(tokens), settledSince, depth)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	out, err := collectThreadRequests(rows, action)
	if err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// threadAgentRequestsSQL is ListThreadAgentRequests' read, its LIMIT
// appended. `sub` walks the caller's local spawns down to ?4 levels, each
// step probing the queued thread's requests by caller and carrying the
// path of threads above it; the threads on the path of one with work open
// in it are the ones a settled spawn into is kept for.
var threadAgentRequestsSQL = `WITH RECURSIVE
  sub(thread_id, path, depth) AS (
    SELECT ?1, json_array(?1), 0
    UNION
    SELECT r.target_thread_id, json_insert(sub.path, '$[#]', r.target_thread_id), sub.depth + 1
      FROM sub CROSS JOIN thread_requests r
     WHERE r.caller_thread_id = sub.thread_id AND r.kind = 'spawn' AND r.target_computer_id = ''
       AND r.target_thread_id <> '' AND r.lineage_only = 0 AND sub.depth < ?4),
  hot(thread_id) AS (
    SELECT json_each.value FROM sub, json_each(sub.path)
     WHERE EXISTS (SELECT 1 FROM thread_requests c
                    WHERE c.caller_thread_id = sub.thread_id AND (` + openAgentRequestSQL("c") + `
                      OR (c.kind = 'spawn' AND c.target_computer_id <> '' AND c.lineage_only = 0 AND c.nested_open = 1))))
SELECT ` + threadRequestColumns + ` FROM thread_requests r
 WHERE r.caller_thread_id = ?1 AND r.lineage_only = 0
   AND r.kind IN ('spawn','send','ask')
   AND (r.kind = 'spawn' OR r.notify = 1 OR r.token IN (SELECT value FROM json_each(?2)))
   AND (r.settled_at IS NULL OR r.settled_at >= ?3
     OR (r.kind = 'spawn' AND r.target_computer_id = '' AND r.target_thread_id IN (SELECT thread_id FROM hot))
     OR (r.kind = 'spawn' AND r.target_computer_id <> '' AND r.nested_open = 1))
 ORDER BY r.created_at ASC, r.token ASC LIMIT `

// openAgentRequestSQL matches an open request the activity rail lists as an
// agent: a spawn, or a send or ask whose wake is armed.
func openAgentRequestSQL(alias string) string {
	return alias + `.lineage_only = 0 AND ` + alias + `.settled_at IS NULL AND (` + alias + `.kind = 'spawn' OR (` +
		alias + `.kind IN ('send','ask') AND ` + alias + `.notify = 1))`
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
	// NestedOpen is whether a settled spawn on another computer still has
	// open work there, which the poller reads agent_rows for at its pace.
	NestedOpen bool
}

// NextThreadRequestWork reports when the unattended passes next have work.
// On most computers nothing is scheduled, and the sweep sleeps on that answer
// rather than asking three questions a second for the life of the process.
//
// One query, three indexed minima and one probe, so the answer describes
// one snapshot.
func (s *Store) NextThreadRequestWork() (ThreadRequestSchedule, error) {
	var reminder, poll, wake sql.NullInt64
	var nestedOpen bool
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
		         OR (late_reply IS NOT NULL AND late_delivered_at IS NULL))),
		   EXISTS (SELECT 1 FROM thread_requests
		     WHERE nested_open = 1 AND settled_at IS NOT NULL AND kind = 'spawn'
		       AND target_computer_id <> '' AND lineage_only = 0)`,
	).Scan(&reminder, &poll, &wake, &nestedOpen); err != nil {
		return ThreadRequestSchedule{}, fmt.Errorf("store: read next thread request work: %w", err)
	}
	moment := func(value sql.NullInt64) ScheduledMoment {
		return ScheduledMoment{At: value.Int64, Set: value.Valid}
	}
	return ThreadRequestSchedule{Reminder: moment(reminder), Poll: moment(poll), Wake: moment(wake), NestedOpen: nestedOpen}, nil
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
	return s.changedThreadRequest(token, result, action)
}

// spawnLineageSQL matches a local spawn request whose thread still exists.
// The row is the only record of which thread started that one: the sidebar
// nests a spawned thread under its spawner and counts a standing agent's
// finished children through it, so its lineage lives as long as the thread
// it made. Past the retention floor the sweep keeps only that lineage
// (clearLineagePayloadSQL). ON DELETE CASCADE on the caller takes it when
// the spawner goes, and a thread deleted on its own leaves a row nothing
// reads, which the sweep deletes once it is past the floor.
const spawnLineageSQL = `kind = 'spawn' AND target_computer_id = ''
			AND target_thread_id IN (SELECT id FROM threads)`

// clearLineagePayloadSQL marks a lineage row lineage_only, empties
// everything it answered and disarms its wake, so past the floor the row
// names its caller, kind, target, state and times and nothing else: the
// ledger reads skip it, an old answer is not kept for the life of the
// thread, and a wake it never handed over is not delivered empty. The
// delivery marks stay, because a cleared delivered_at would read as a wake
// still owed. The sweep appends its floor to the WHERE, which skips rows
// already marked.
const clearLineagePayloadSQL = `UPDATE thread_requests
	SET lineage_only = 1, answer = NULL, answer_kind = '', late_reply = NULL, late_reply_at = NULL,
	    answer_agent = 0, answer_agent_role = '', notify = 0, issue = '', wake_issue = '', expires_at = NULL,
	    nested_open = 0, target_provider = ''
	WHERE lineage_only = 0`

// DeleteThreadRequestsBefore removes both sides of every request that has
// been settled longer than the retention floor, and reports how many rows
// it deleted or cleared. Past it a token stops answering, so a late reply
// gets `thread_request_unknown` rather than resurrecting a month-old
// exchange.
//
// Two rows survive the floor whatever their age. An OPEN row is still work:
// deleting it would leave a receipt nobody can settle or cancel, so the row
// waits for its own settlement first. And a row's age is measured from the
// moment it finished, not from when it was made: a reminder armed for next
// month is due in the future and older than the floor at the same time, and
// an answer written yesterday on a request made last year has been readable
// for a day. A local spawn request whose thread exists keeps its lineage
// past the floor, marked lineage_only with its payload cleared
// (spawnLineageSQL).
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
		// keep names rows the floor never deletes, however old, and clear
		// is the update applied to them instead.
		keep, clear string
	}{
		{"thread_requests", threadRequestOpenStates, "COALESCE(settled_at, due_at, created_at)", spawnLineageSQL, clearLineagePayloadSQL},
		{"thread_request_receipts", threadReceiptOpenStates, "COALESCE(settled_at, created_at)", "0", ""},
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
		pastFloor := `NOT ` + openClause + ` AND ` + sweep.age + ` < ?`
		if sweep.clear != "" {
			result, err := tx.Exec(sweep.clear+` AND `+pastFloor+` AND (`+sweep.keep+`)`, args...)
			if err != nil {
				return 0, fmt.Errorf("store: clear kept %s before %d: %w", sweep.table, cutoff, err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("store: count cleared %s rows: %w", sweep.table, err)
			}
			total += affected
		}
		result, err := tx.Exec(`DELETE FROM `+sweep.table+` WHERE `+pastFloor+` AND NOT (`+sweep.keep+`)`, args...)
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

// answerAgentColumns is how a ThreadAnswerAgent is stored: a flag, so an
// agent with no role line is still one, and its role.
func answerAgentColumns(agent *ThreadAnswerAgent) (int, string) {
	if agent == nil {
		return 0, ""
	}
	return 1, agent.Role
}

// answerAgentOf reads the two stored columns back.
func answerAgentOf(flag int64, role string) *ThreadAnswerAgent {
	if flag == 0 {
		return nil
	}
	return &ThreadAnswerAgent{Role: role}
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
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	return nil
}

// WithdrawThreadWake takes a wake that never reached the agent back for its
// request: its queue row goes, if it still has one, and with it the
// delivery mark the queue write set and the request's notify, in one
// transaction. The answer is owed again but disarmed, so only a later wait
// or thread_status re-arms it, and thread_status still returns it.
// Otherwise a row already gone means the wake was sent, and changes
// nothing. A wake the provider was handed and never confirmed
// (unconfirmed) has no row left and goes back all the same, while the
// request still reads queued and the provider has not confirmed it. A late
// wake clears only its own mark; the first answer's delivery stands. A
// lineage-only row keeps its marks (clearLineagePayloadSQL): a cleared
// delivered_at would read as a wake still owed.
func (s *Store) WithdrawThreadWake(token string, late bool, rowID string, unconfirmed bool) error {
	action := fmt.Sprintf("store: withdraw thread wake %s", token)
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer release()
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`DELETE FROM flush_queue_items WHERE id = ?`, rowID)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	removed, err := rowsChanged(result, action)
	if err != nil {
		return err
	}
	if removed || unconfirmed {
		delivered, confirmed, clear := "delivered_at", "confirmed_at", "delivered_how = '', "
		if late {
			delivered, confirmed, clear = "late_delivered_at", "late_confirmed_at", ""
		}
		query := `UPDATE thread_requests SET ` + delivered + ` = NULL, ` + confirmed + ` = NULL, ` + clear + `notify = 0, updated_at = ?
		           WHERE token = ? AND ` + delivered + ` IS NOT NULL AND delivered_how = ? AND lineage_only = 0`
		// A message still in the queue never reached the provider. One that
		// left it goes back only while no echo has confirmed it.
		if !removed {
			query += ` AND ` + confirmed + ` IS NULL`
		}
		if _, err := tx.Exec(query, nowMillis(), token, ThreadWakeQueued); err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	return nil
}

// ConfirmThreadWake records the provider's echo of a queued answer, or of a
// late reply (late): the second phase of its delivery mark, after which no
// session end or restart gives it back. It reports whether the mark was
// still unconfirmed. A lineage-only row is left as the sweep left it.
func (s *Store) ConfirmThreadWake(token string, late bool, at int64) (bool, error) {
	action := fmt.Sprintf("store: confirm thread wake %s", token)
	query := `UPDATE thread_requests SET confirmed_at = ?, updated_at = ?
	           WHERE token = ? AND delivered_at IS NOT NULL AND confirmed_at IS NULL AND lineage_only = 0`
	if late {
		query = `UPDATE thread_requests SET late_confirmed_at = ?, updated_at = ?
		          WHERE token = ? AND late_delivered_at IS NOT NULL AND late_confirmed_at IS NULL AND lineage_only = 0`
	}
	result, err := s.db.Exec(query, at, nowMillis(), token)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return s.changedThreadRequest(token, result, action)
}

// UnconfirmedThreadWakes returns the requests whose answer or late reply
// was queued and never confirmed: at boot, no session holds them, so the
// agent may never have taken them in. A lineage-only row answers nothing
// any more and is not among them.
func (s *Store) UnconfirmedThreadWakes() ([]ThreadRequest, error) {
	rows, err := s.reader().Query(
		`SELECT ` + threadRequestColumns + ` FROM thread_requests
		  WHERE lineage_only = 0
		    AND ((delivered_at IS NOT NULL AND confirmed_at IS NULL)
		      OR (late_delivered_at IS NOT NULL AND late_confirmed_at IS NULL))`)
	if err != nil {
		return nil, fmt.Errorf("store: list unconfirmed thread wakes: %w", err)
	}
	return collectThreadRequests(rows, "store: list unconfirmed thread wakes")
}

// ThreadRequestChange names a source row a committed write changed: by
// token, or every row of one caller when Token is empty.
type ThreadRequestChange struct {
	Token          string
	CallerThreadID string
}

// OnThreadRequestChanged sets the function the store calls after it commits
// a write that changed a source row: inserted, moved state, settled, took a
// late reply, re-targeted, armed or disarmed its wake, delivered, confirmed
// or withdrew it, or deleted it. The retention sweep is not reported: it
// takes rows long settled. fn must not block. nil clears it.
func (s *Store) OnThreadRequestChanged(fn func(ThreadRequestChange)) {
	if fn == nil {
		s.threadRequestChanged.Store(nil)
		return
	}
	s.threadRequestChanged.Store(&fn)
}

func (s *Store) noteThreadRequestChanged(change ThreadRequestChange) {
	if fn := s.threadRequestChanged.Load(); fn != nil {
		(*fn)(change)
	}
}

// changedThreadRequest is rowsChanged for a write to one source row, which
// it reports when the write applied.
func (s *Store) changedThreadRequest(token string, result sql.Result, action string) (bool, error) {
	changed, err := rowsChanged(result, action)
	if changed {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return changed, err
}
