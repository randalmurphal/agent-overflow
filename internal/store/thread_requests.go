package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	NestedOpen bool   `json:"-"`
	LinkEnd    string `json:"-"`
	// OriginThreadID is the thread an `ask` forked; empty otherwise.
	OriginThreadID string `json:"originThreadId,omitempty"`
	// Notify is whether a wake is owed on settlement. Every positive wait
	// that ends unsettled arms it.
	Notify bool   `json:"notify"`
	State  string `json:"state"`
	// Answer is the whole settled text, never truncated. For an
	// interruption with a Cause it is what the turn had written, if
	// anything; the words an agent reads are rendered from both.
	Answer     []byte `json:"-"`
	AnswerKind string `json:"answerKind,omitempty"`
	// Cause and CauseArg are why an `interrupted` answer was interrupted
	// (threadtools.Cause), empty for every other answer and for one an
	// earlier build or computer recorded as text alone.
	Cause       string `json:"-"`
	CauseArg    string `json:"-"`
	LateReply   []byte `json:"-"`
	LateReplyAt int64  `json:"lateReplyAt,omitempty"`
	// LateReplyExpired marks a finished request whose late reply its
	// destination dropped, the hold on it run out, before this computer
	// collected it. LateReplyAt is then when it arrived there; Answer is
	// still the first answer.
	LateReplyExpired bool  `json:"-"`
	Revision         int64 `json:"revision"`
	// AckOwed is the destination's revision this request holds and has not
	// yet told it it holds, 0 when none (CollectThreadRequest).
	AckOwed     int64 `json:"-"`
	SettledAt   int64 `json:"settledAt,omitempty"`
	DeliveredAt int64 `json:"deliveredAt,omitempty"`
	// DeliveredHow is the route the first answer reached the caller by, and
	// LateDeliveredHow the late reply's: an answer delivered by a wake keeps
	// its wake row after a late reply is read inline, and the other way round.
	DeliveredHow string `json:"deliveredHow,omitempty"`
	// DeliveredWith is the token of the request whose wake message carried
	// this answer, when one message answered several requests: the carrier,
	// whose send id the message is queued and echoed under. Its confirmation
	// and give-back apply to this row with it. Empty when the request's own
	// message carried it, or none did.
	DeliveredWith    string `json:"-"`
	LateDeliveredAt  int64  `json:"lateDeliveredAt,omitempty"`
	LateDeliveredHow string `json:"lateDeliveredHow,omitempty"`
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
	// AdmitUntil is, for a request on another computer, the last moment a
	// handler there could still admit an attempt this computer sent (the
	// attempt's dispatch plus the call's wait), 0 before any was sent. Past
	// it, a destination that does not hold the token never started it.
	AdmitUntil int64 `json:"-"`
	// WakeAttempts, WakeNextCheck and WakeIssue schedule and bound the
	// retry of a wake the settlement could not hand over.
	WakeAttempts  int64  `json:"-"`
	WakeNextCheck int64  `json:"-"`
	WakeIssue     string `json:"wakeIssue,omitempty"`
	// LineageOnly marks a local spawn row the retention sweep kept past
	// the floor with its payload cleared: the record of which thread
	// started its target, and nothing else. The ledger reads skip it; the
	// Agents tree still nests its target under it.
	LineageOnly bool  `json:"-"`
	CreatedAt   int64 `json:"createdAt"`
	UpdatedAt   int64 `json:"updatedAt"`
	// AnswerAgent is set when the thread that answered is a standing agent,
	// as recorded with the answer; nil for any other thread.
	AnswerAgent *ThreadAnswerAgent `json:"-"`
	// LateReplyAgent is AnswerAgent for the late reply, recorded with it.
	LateReplyAgent *ThreadAnswerAgent `json:"-"`
	// MadeInThreadID is the assistant's past thread that made this request
	// when New Thread, Same Assistant moved it to CallerThreadID; empty
	// when the caller made it.
	MadeInThreadID string `json:"-"`
	// RestUnreported marks a `finished` request a paired computer settled
	// when its thread's turn ended, a computer that cannot report whether
	// the thread is at rest: a spawn there, or a send to a thread the
	// caller spawned there, whose answer means its turn ended
	// (ThreadReceiptAnswer.RestUnreported).
	RestUnreported bool `json:"-"`
	// TargetComputerName is what the person called that computer when the
	// request was admitted, or when its pairing ended for a request that
	// kept none (EndPairing), which names it once no pairing here does.
	TargetComputerName string `json:"-"`
}

// RemoteRequestSource is the thread a destination's receipts name for the
// requests callerThreadID holds to one thread on another computer: the
// caller when it made one itself, else the past thread that made the newest
// one before New Thread, Same Assistant moved it. Empty when it holds none.
func (s *Store) RemoteRequestSource(callerThreadID, computerID, targetThreadID string) (string, error) {
	var source string
	err := s.reader().QueryRow(`SELECT CASE WHEN made_in_thread_id = '' THEN caller_thread_id ELSE made_in_thread_id END
		FROM thread_requests
		WHERE caller_thread_id = ? AND target_computer_id = ? AND target_thread_id = ?
		ORDER BY made_in_thread_id = '' DESC, created_at DESC LIMIT 1`, callerThreadID, computerID, targetThreadID).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read the source of %s's requests to %s: %w", callerThreadID, targetThreadID, err)
	}
	return source, nil
}

// MadeIn is the thread that made the request: the one a destination's
// receipt names as its source.
func (r ThreadRequest) MadeIn() string {
	if r.MadeInThreadID != "" {
		return r.MadeInThreadID
	}
	return r.CallerThreadID
}

// DeliveryRoute is how the first answer (late false) or the late reply
// (late true) reached the caller, empty while it has not.
func (r ThreadRequest) DeliveryRoute(late bool) string {
	if late {
		return r.LateDeliveredHow
	}
	return r.DeliveredHow
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
	// Cause and CauseArg are why an `interrupted` settlement was
	// interrupted (ThreadRequest.Cause); no other state carries one.
	Cause     string
	CauseArg  string
	SettledAt int64
	// ExpiresAt is when an uncollected answer is dropped. The destination
	// sets it to SettledAt plus a day; the source copies what the
	// destination reported.
	ExpiresAt int64
	// Agent is the answering thread as a standing agent, read by the
	// receipt's settlement when the answer is written and copied to the
	// source row with it.
	Agent *ThreadAnswerAgent
	// ArmWake arms `notify` on the source row with the settlement. An
	// explicit reply sets it: the reply is owed to the caller as a message
	// whether or not the caller asked to be told. An archived caller is
	// never armed (threadRequestArmSQL). A receipt has no wake, and its
	// settlement refuses the flag.
	ArmWake bool
	// DisarmWake turns `notify` off with the settlement, for a request its
	// own sender cancelled: the cancel's report says what happened. It is
	// written with the settlement so a cancel that fails leaves the wake
	// armed. A receipt refuses it too.
	DisarmWake bool
}

// threadRequestArmSQL is the `notify` value a statement that stores a reply
// writes; its one parameter is whether the reply arms the wake. It refuses
// an archived caller in the statement itself: archiving disarms the caller's
// requests, and a reply that read the thread before the archive must not arm
// one after it.
const threadRequestArmSQL = `CASE WHEN ? = 1 AND NOT EXISTS (
		        SELECT 1 FROM threads
		         WHERE threads.id = thread_requests.caller_thread_id AND threads.archived = 1)
		     THEN 1 ELSE notify END`

func (s ThreadRequestSettlement) validate(action string, states map[string]struct{}) error {
	if _, ok := states[s.State]; !ok || s.State == "" {
		return fmt.Errorf("%s: invalid settled state %q", action, s.State)
	}
	if _, ok := threadAnswerKinds[s.AnswerKind]; !ok {
		return fmt.Errorf("%s: invalid answer kind %q", action, s.AnswerKind)
	}
	if s.ArmWake && s.DisarmWake {
		return fmt.Errorf("%s: a settlement cannot both arm and disarm the wake", action)
	}
	return validateAnswerCause(action, s.State, s.Cause, s.CauseArg)
}

// validateAnswerCause refuses a cause on an answer that is no interruption,
// and an argument with no cause. The two tables spell `interrupted` alike.
func validateAnswerCause(action, state, cause, arg string) error {
	if cause != "" && state != ThreadRequestInterrupted {
		return fmt.Errorf("%s: a %s answer has no cause, this one says %q", action, state, cause)
	}
	if cause == "" && arg != "" {
		return fmt.Errorf("%s: a cause argument %q with no cause", action, arg)
	}
	return nil
}

const threadRequestColumns = `token, caller_thread_id, kind, COALESCE(due_at, 0),
    target_computer_id, target_thread_id, target_thread_title, origin_thread_id, notify, state,
    answer, answer_kind, late_reply, COALESCE(late_reply_at, 0), revision,
    COALESCE(settled_at, 0), COALESCE(delivered_at, 0), delivered_how, delivered_with,
    COALESCE(late_delivered_at, 0), COALESCE(expires_at, 0),
    polling, next_check, attempts, issue,
    wake_attempts, wake_next_check, wake_issue, lineage_only, created_at, updated_at,
    COALESCE(confirmed_at, 0), COALESCE(late_confirmed_at, 0), answer_agent, answer_agent_role,
    target_provider, nested_open, late_delivered_how, made_in_thread_id,
    late_reply_agent, late_reply_agent_role, rest_unreported, answer_cause, answer_cause_arg, admit_until,
    late_reply_expired, ack_owed, target_computer_name, link_end`

func scanThreadRequest(row rowScanner) (ThreadRequest, error) {
	return scanThreadRequestWith(row, nil, nil)
}

// RemoteListingReach includes a placed worker after its owner releases it.
func (r ThreadRequest) RemoteListingReach() bool {
	return r.Kind == ThreadRequestSpawn && r.TargetComputerID != "" && r.TargetThreadID != "" && (r.LinkEnd == "" || r.LinkEnd == "released")
}

// scanThreadRequestWith scans a projection in threadRequestColumns' order
// whose answer and late reply columns go to answer and lateReply instead of
// the row's own fields when those are set, and whose columns after them go
// to extra.
func scanThreadRequestWith(row rowScanner, answer, lateReply any, extra ...any) (ThreadRequest, error) {
	var r ThreadRequest
	var notify, polling, lineageOnly, nestedOpen, restUnreported, lateReplyExpired int
	var answerAgentRole, lateReplyAgentRole string
	var answerAgent, lateReplyAgent int64
	if answer == nil {
		answer = &r.Answer
	}
	if lateReply == nil {
		lateReply = &r.LateReply
	}
	err := row.Scan(append([]any{
		&r.Token, &r.CallerThreadID, &r.Kind, &r.DueAt,
		&r.TargetComputerID, &r.TargetThreadID, &r.TargetThreadTitle, &r.OriginThreadID, &notify, &r.State,
		answer, &r.AnswerKind, lateReply, &r.LateReplyAt, &r.Revision,
		&r.SettledAt, &r.DeliveredAt, &r.DeliveredHow, &r.DeliveredWith,
		&r.LateDeliveredAt, &r.ExpiresAt,
		&polling, &r.NextCheck, &r.Attempts, &r.Issue,
		&r.WakeAttempts, &r.WakeNextCheck, &r.WakeIssue, &lineageOnly, &r.CreatedAt, &r.UpdatedAt,
		&r.ConfirmedAt, &r.LateConfirmedAt, &answerAgent, &answerAgentRole,
		&r.TargetProvider, &nestedOpen, &r.LateDeliveredHow, &r.MadeInThreadID,
		&lateReplyAgent, &lateReplyAgentRole, &restUnreported, &r.Cause, &r.CauseArg, &r.AdmitUntil,
		&lateReplyExpired, &r.AckOwed, &r.TargetComputerName, &r.LinkEnd,
	}, extra...)...)
	r.LateReplyExpired = lateReplyExpired != 0
	r.NestedOpen = nestedOpen != 0
	r.RestUnreported = restUnreported != 0
	r.AnswerAgent = answerAgentOf(answerAgent, answerAgentRole)
	r.LateReplyAgent = answerAgentOf(lateReplyAgent, lateReplyAgentRole)
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
	if err := insertThreadRequestTx(s.db, r); err != nil {
		return err
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: r.Token, CallerThreadID: r.CallerThreadID})
	return nil
}

// insertThreadRequestTx validates and writes one source row on ex.
func insertThreadRequestTx(ex sqlExecutor, r ThreadRequest) error {
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
	if _, err := ex.Exec(
		`INSERT INTO thread_requests (
		    token, caller_thread_id, kind, due_at, target_computer_id, target_thread_id,
		    origin_thread_id, notify, state, answer, answer_kind, revision,
		    next_check, attempts, issue, lineage_only, created_at, updated_at, admit_until, target_computer_name
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Token, r.CallerThreadID, r.Kind, nilIfZero(r.DueAt), r.TargetComputerID, r.TargetThreadID,
		r.OriginThreadID, boolToInt(r.Notify), r.State, nilIfEmptyBytes(r.Answer), r.AnswerKind, r.Revision,
		r.NextCheck, r.Attempts, r.Issue, boolToInt(r.LineageOnly), r.CreatedAt, r.UpdatedAt, r.AdmitUntil, r.TargetComputerName,
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
	settled, err := settleThreadRequest(s.db, token, from, settlement, "")
	if settled {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return settled, err
}

// settleThreadRequest is the settlement write on db, which reports nothing:
// a caller on a transaction reports the change once it commits. `where`
// narrows the rows it may settle further.
func settleThreadRequest(db sqlExecutor, token string, from []string, settlement ThreadRequestSettlement, where string) (bool, error) {
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
	set := []any{settlement.State, nilIfEmptyBytes(settlement.Answer), settlement.AnswerKind, settlement.Cause, settlement.CauseArg,
		settledAt, nilIfZero(settlement.ExpiresAt), agent, role, boolToInt(settlement.DisarmWake), boolToInt(settlement.ArmWake),
		nowMillis(), token}
	result, err := db.Exec(
		`UPDATE thread_requests
		    SET state = ?, answer = ?, answer_kind = ?, answer_cause = ?, answer_cause_arg = ?, revision = revision + 1,
		        settled_at = ?, expires_at = ?, answer_agent = ?, answer_agent_role = ?,
		        notify = CASE WHEN ? = 1 THEN 0 ELSE `+threadRequestArmSQL+` END, updated_at = ?
		  WHERE token = ? AND `+clause+where,
		append(set, args...)...,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// SettleThreadRequestDelivered settles a source row and records the answer
// as delivered inline, in one transaction. It is for a settlement the
// caller's own tool call returns: a request refused before any thread ran it
// is answered by that call's error, so no message is owed for it and no pass
// may find the row settled and undelivered in between.
func (s *Store) SettleThreadRequestDelivered(token string, from []string, settlement ThreadRequestSettlement) (_ bool, err error) {
	action := fmt.Sprintf("store: settle delivered thread request %s", token)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	settled, err := settleThreadRequest(tx, token, from, settlement, "")
	if err != nil || !settled {
		return false, err
	}
	marked, err := MarkThreadRequestDeliveredTx(tx, token, ThreadWakeInline, 0, false)
	if err != nil {
		return false, err
	}
	if !marked {
		// The settlement left the row open (a state that is not an end),
		// so there is nothing to deliver and nothing is written.
		return false, fmt.Errorf("%s: state %q does not end the request", action, settlement.State)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	return true, nil
}

// ThreadRequestOpenStates returns the states a settlement may replace.
func ThreadRequestOpenStates() []string {
	return append([]string(nil), threadRequestOpenStates...)
}

// threadRequestLocalSQL limits a write to a request this computer answers
// itself. A row naming another computer is a request this one sent there,
// and a receipt here carrying the same token was minted by a paired
// computer: writing its answer into that row would be wrong.
const threadRequestLocalSQL = ` AND target_computer_id = ''`

// ThreadReceiptAnswer is everything a receipt holds for its request at one
// revision: the settlement and the late reply that followed it, each with the
// standing agent that wrote it. A request collects it whole
// (collectThreadRequest), so a revision acknowledged after a collection is one
// whose every part the request holds.
type ThreadReceiptAnswer struct {
	Revision int64
	// State is the receipt's word; the request stores its own for it.
	State          string
	Answer         []byte
	AnswerKind     string
	Cause          string
	CauseArg       string
	SettledAt      int64
	ExpiresAt      int64
	Agent          *ThreadAnswerAgent
	LateReply      []byte
	LateReplyAt    int64
	LateReplyAgent *ThreadAnswerAgent
	// RestUnreported is set by the source collecting a `finished` answer
	// from a computer that settles when the turn ends rather than when the
	// thread rests, for a request that waits for rest there
	// (ThreadRequest.RestUnreported). A receipt never carries it.
	RestUnreported bool
}

// Snapshot is what the receipt holds at its current revision.
func (r ThreadRequestReceipt) Snapshot() ThreadReceiptAnswer {
	return ThreadReceiptAnswer{
		Revision: r.Revision, State: r.State, Answer: r.Answer, AnswerKind: r.AnswerKind,
		Cause: r.Cause, CauseArg: r.CauseArg, SettledAt: r.SettledAt, ExpiresAt: r.ExpiresAt, Agent: r.AnswerAgent,
		LateReply: r.LateReply, LateReplyAt: r.LateReplyAt, LateReplyAgent: r.LateReplyAgent,
	}
}

// ThreadRequestWake is the request's half of a collection. ReplyArms is
// whether an explicit reply the collection carries is owed to the caller as a
// message (ThreadRequestSettlement.ArmWake); a caller may pass false for a
// collection that carries no reply. A sender's own cancel disarms its wake
// with the settlement instead (SettleLocalThreadRequest, DisarmWake).
type ThreadRequestWake struct {
	ReplyArms bool
}

// ThreadRequestCollected is what a collection wrote on the request: the
// receipt's settlement, its late reply, or both; or, from another computer,
// that a late reply arrived there and expired before it was collected.
type ThreadRequestCollected struct {
	Settled          bool
	LateReply        bool
	LateReplyExpired bool
}

// Any reports whether the collection wrote anything.
func (c ThreadRequestCollected) Any() bool { return c.Settled || c.LateReply || c.LateReplyExpired }

// collectThreadRequest copies one receipt answer onto its request: the
// settlement while the request is still open, and the late reply while the
// request has none. Each write is conditional, so a collection repeated with
// the same answer writes nothing. `where` narrows the request rows it may
// write. It reports nothing; the caller reports the change once it commits.
func collectThreadRequest(db sqlExecutor, token string, answer ThreadReceiptAnswer, wake ThreadRequestWake, where string) (ThreadRequestCollected, error) {
	if !ThreadReceiptSettled(answer.State) {
		return ThreadRequestCollected{}, nil
	}
	arms := wake.ReplyArms
	settled, err := settleThreadRequest(db, token, threadRequestOpenStates, ThreadRequestSettlement{
		State:      ThreadRequestStateForReceipt(answer.State),
		Answer:     answer.Answer,
		AnswerKind: answer.AnswerKind,
		Cause:      answer.Cause,
		CauseArg:   answer.CauseArg,
		SettledAt:  answer.SettledAt,
		ExpiresAt:  answer.ExpiresAt,
		Agent:      answer.Agent,
		ArmWake:    arms && answer.AnswerKind == ThreadAnswerReply,
	}, where)
	if err != nil {
		return ThreadRequestCollected{}, err
	}
	collected := ThreadRequestCollected{Settled: settled}
	if len(answer.LateReply) > 0 {
		collected.LateReply, err = storeThreadRequestLateReply(db, token, answer.LateReply, answer.LateReplyAt,
			answer.LateReplyAgent, arms, where)
		if err != nil {
			return ThreadRequestCollected{}, err
		}
	}
	return collected, nil
}

// CollectThreadRequest copies an answer another computer reported onto the
// request this computer sent there, and owes that computer the
// acknowledgement of answer.Revision, in one transaction. What it copies is
// the settlement and the late reply (collectThreadRequest), and that a newer
// reply arrived there and expired before it was collected, with when, beside
// the first answer this or an earlier collection took: the revision is held
// all the same, since nothing of it is left to collect. A
// revision the request can take nothing from (it settled here first) is
// owed too: there is nothing left for that computer to hold it for. The
// caller sends what is owed and clears it once that computer takes it
// (ClearThreadRequestAcks).
func (s *Store) CollectThreadRequest(token string, answer ThreadReceiptAnswer, wake ThreadRequestWake) (_ ThreadRequestCollected, err error) {
	action := fmt.Sprintf("store: collect thread request %s", token)
	tx, err := s.db.Begin()
	if err != nil {
		return ThreadRequestCollected{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	collected, err := collectThreadRequest(tx, token, answer, wake, "")
	if err != nil {
		return ThreadRequestCollected{}, err
	}
	if answer.State == ThreadReceiptExpired && answer.LateReplyAt != 0 {
		// The receipt expired with a late reply the request never took:
		// recorded beside the first answer, whether an earlier collection
		// took that answer or this one settled the request `expired`.
		result, err := tx.Exec(`UPDATE thread_requests
			    SET late_reply_expired = 1, late_reply_at = ?, revision = MAX(revision, ?), updated_at = ?
			  WHERE token = ? AND state IN ('`+ThreadRequestFinished+`', '`+ThreadRequestExpired+`')
			    AND late_reply IS NULL AND late_reply_expired = 0 AND lineage_only = 0`, answer.LateReplyAt, answer.Revision, nowMillis(), token)
		if err != nil {
			return ThreadRequestCollected{}, fmt.Errorf("%s: record the late reply that expired: %w", action, err)
		}
		if collected.LateReplyExpired, err = rowsChanged(result, action); err != nil {
			return ThreadRequestCollected{}, err
		}
	}
	if collected.Settled && answer.RestUnreported {
		if _, err := tx.Exec(`UPDATE thread_requests SET rest_unreported = 1 WHERE token = ?`, token); err != nil {
			return ThreadRequestCollected{}, fmt.Errorf("%s: mark its rest unreported: %w", action, err)
		}
	}
	if _, err := oweThreadRequestAck(tx, token, answer.Revision); err != nil {
		return ThreadRequestCollected{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadRequestCollected{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	if collected.Any() {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return collected, nil
}

// collectLocalThreadReceiptTx is the one collection of a receipt by the
// request on this computer it answers: it reads the receipt at its current
// revision, copies that whole answer onto the request, and acknowledges that
// revision, all on the caller's transaction. A collection that wrote nothing
// acknowledges nothing; one that wrote either part acknowledges the revision
// it read, because the request now holds every part that it will ever take.
// A request already settled another way (a refusal its own call returned)
// keeps that settlement. A receipt the retention floor reduced to its
// lineage holds nothing to collect.
func collectLocalThreadReceiptTx(tx *sql.Tx, token string, wake ThreadRequestWake) (ThreadRequestCollected, error) {
	receipt, err := scanThreadReceipt(tx.QueryRow(
		`SELECT `+threadReceiptColumns+` FROM thread_request_receipts WHERE token = ? AND lineage_only = 0`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRequestCollected{}, nil
	}
	if err != nil {
		return ThreadRequestCollected{}, fmt.Errorf("store: read thread receipt %s to collect: %w", token, err)
	}
	collected, err := collectThreadRequest(tx, token, receipt.Snapshot(), wake, threadRequestLocalSQL)
	if err != nil || !collected.Any() {
		return ThreadRequestCollected{}, err
	}
	if _, err := ackThreadReceipt(tx, token, receipt.Revision, nowMillis()); err != nil {
		return ThreadRequestCollected{}, err
	}
	return collected, nil
}

// StoreLocalThreadRequestLateReply stores a late reply on a receipt and
// collects it into the request on this computer it answers, in one
// transaction, for the reason SettleLocalThreadRequest is one: a reply stored
// on the receipt alone is lost, because a retry of the same text is the same
// reply and stores nothing. agent is the replying thread as a standing agent
// (StoreThreadReceiptLateReply). It reports whether the receipt took the
// reply and what the request collected with it; a request this computer
// sent to another one (a token collision) collects nothing.
func (s *Store) StoreLocalThreadRequestLateReply(token string, reply []byte, at int64, agent *ThreadAnswerAgent, replyArms bool) (_ bool, _ ThreadRequestCollected, err error) {
	action := fmt.Sprintf("store: store local thread request late reply %s", token)
	if at == 0 {
		at = nowMillis()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, ThreadRequestCollected{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	stored, err := storeThreadReceiptLateReply(tx, token, reply, at, agent)
	if err != nil || !stored {
		return false, ThreadRequestCollected{}, err
	}
	collected, err := collectLocalThreadReceiptTx(tx, token, ThreadRequestWake{ReplyArms: replyArms})
	if err != nil {
		return false, ThreadRequestCollected{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, ThreadRequestCollected{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	return true, collected, nil
}

// CollectLocalThreadReceipt collects a receipt that is already settled into
// the request on this computer it answers, and acknowledges it, in one
// transaction. It is the boot's repair of a request left open beside its
// settled receipt, or holding a late reply its receipt took without it;
// collecting one that holds everything already writes nothing.
func (s *Store) CollectLocalThreadReceipt(token string, wake ThreadRequestWake) (_ ThreadRequestCollected, err error) {
	action := fmt.Sprintf("store: collect local thread receipt %s", token)
	tx, err := s.db.Begin()
	if err != nil {
		return ThreadRequestCollected{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	collected, err := collectLocalThreadReceiptTx(tx, token, wake)
	if err != nil {
		return ThreadRequestCollected{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadRequestCollected{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	if collected.Any() {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return collected, nil
}

// ThreadRequestStateForReceipt maps a receipt state onto the request's
// vocabulary. The two sets differ only where the source knows something the
// destination cannot (`unconfirmed`, `refused`), so everything settled has a
// name on both sides.
func ThreadRequestStateForReceipt(state string) string {
	switch state {
	case ThreadReceiptReplied:
		return ThreadRequestReplied
	case ThreadReceiptFinished:
		return ThreadRequestFinished
	case ThreadReceiptErrored:
		return ThreadRequestErrored
	case ThreadReceiptCancelled:
		return ThreadRequestCancelled
	case ThreadReceiptInterrupted:
		return ThreadRequestInterrupted
	case ThreadReceiptExpired:
		return ThreadRequestExpired
	default:
		return ThreadRequestErrored
	}
}

// StoreThreadRequestLateReply records a thread_reply that arrived after the
// request had already settled. It is a new revision, so a `thread_status`
// waiting past the collected revision returns and the wake path delivers a
// second message. Only the first late reply is stored, and never on a
// lineage-only row, whose token the ledger no longer knows. agent is the
// replying thread as a standing agent, recorded by the receipt with the
// reply; it is kept beside the first answer's (LateReplyAgent), each wake
// naming its own.
//
// `armWake` arms `notify` with the reply, as a settlement's ArmWake does and
// under the same rule.
func (s *Store) StoreThreadRequestLateReply(token string, reply []byte, at int64, agent *ThreadAnswerAgent, armWake bool) (bool, error) {
	stored, err := storeThreadRequestLateReply(s.db, token, reply, at, agent, armWake, "")
	if stored {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return stored, err
}

// storeThreadRequestLateReply is the late reply's write on db, which reports
// nothing: a caller on a transaction reports the change once it commits.
// `where` narrows the rows it may write further.
func storeThreadRequestLateReply(db sqlExecutor, token string, reply []byte, at int64, agent *ThreadAnswerAgent, armWake bool, where string) (bool, error) {
	action := fmt.Sprintf("store: store thread request late reply %s", token)
	if at == 0 {
		at = nowMillis()
	}
	isAgent, role := answerAgentColumns(agent)
	result, err := db.Exec(
		`UPDATE thread_requests
		    SET late_reply = ?, late_reply_at = ?, late_reply_agent = ?, late_reply_agent_role = ?, revision = revision + 1,
		        notify = `+threadRequestArmSQL+`, updated_at = ?
		  WHERE token = ? AND settled_at IS NOT NULL AND late_reply IS NULL AND late_reply_expired = 0 AND lineage_only = 0`+where,
		nilIfEmptyBytes(reply), at, isAgent, role, boolToInt(armWake), nowMillis(), token,
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
	query := `UPDATE thread_requests SET delivered_at = ?, delivered_how = ?, confirmed_at = ?, wake_item_id = '', updated_at = ?` + retry +
		` WHERE token = ? AND delivered_at IS NULL` + settled
	if late {
		query = `UPDATE thread_requests SET late_delivered_at = ?, late_delivered_how = ?, late_confirmed_at = ?, late_wake_item_id = '', updated_at = ?` + retry +
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

// RecordThreadRequestInstructions records on a spawn's source row the
// instructions its thread on another computer was given, as that
// computer's answer stated them from its receipt. The first record stands:
// the receipt recorded them once, when it named the thread, so a later
// answer restates the same text.
func (s *Store) RecordThreadRequestInstructions(token, instructions string) error {
	if instructions == "" {
		return nil
	}
	if _, err := s.db.Exec(`UPDATE thread_requests SET spawn_instructions = ?
		  WHERE token = ? AND kind = '`+ThreadRequestSpawn+`' AND spawn_instructions = ''`, instructions, token); err != nil {
		return fmt.Errorf("store: record instructions of thread request %s: %w", token, err)
	}
	return nil
}

// ThreadRequestSpawnInstructions reads what a spawn's source row recorded
// of its thread's instructions: at naming for a spawn here, from the other
// computer's answer for one there. Empty for any other request, or one
// that recorded none.
func (s *Store) ThreadRequestSpawnInstructions(token string) (string, error) {
	var instructions string
	err := s.reader().QueryRow(`SELECT spawn_instructions FROM thread_requests WHERE token = ?`, token).Scan(&instructions)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: read instructions of thread request %s: %w", token, err)
	}
	return instructions, nil
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
// changed. A remote spawn's lineage takes it as the spawn did, since that
// lineage is its thread's link here (spawnLineageSQL); any other
// lineage-only row is left alone.
func (s *Store) SetThreadRequestNestedOpen(token string, open bool) (bool, error) {
	action := fmt.Sprintf("store: set thread request %s nested open", token)
	result, err := s.db.Exec(
		`UPDATE thread_requests SET nested_open = ?, updated_at = ?
		  WHERE token = ? AND nested_open IS NOT ? AND (lineage_only = 0 OR target_computer_id <> '')`,
		boolToInt(open), nowMillis(), token, boolToInt(open),
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
// the same way; the lineage reads (spawnLineageSQL, threadColumns) and the
// Agents tree (agentSpawnLinkSQL) do not.
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
	list, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("store: get thread requests: encode tokens: %w", err)
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE token IN (SELECT value FROM json_each(?)) AND lineage_only = 0
		  ORDER BY created_at DESC, token ASC`, string(list))
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

// nestedRemoteSpawnSQL matches a spawn on another computer whose last
// agent_rows answer listed open work, the predicate of
// idx_thread_requests_nested_caller.
func nestedRemoteSpawnSQL(alias string) string {
	return alias + `.nested_open = 1 AND ` + alias + `.kind = 'spawn' AND ` + alias + `.target_computer_id <> ''`
}

// ListOpenLocalThreadRequests returns every open spawn, send and ask this
// computer's threads made of its own threads, oldest first. Each is settled
// by its receipt, so at boot, once the sweep has settled every open receipt,
// a row still listed here has nothing left that can settle it. A reminder is
// left out: its clock settles it.
func (s *Store) ListOpenLocalThreadRequests() ([]ThreadRequest, error) {
	action := "store: list open local thread requests"
	clause, args, err := stateInClause(action, threadRequestOpenStates, threadRequestStates)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE kind <> 'remind' AND `+clause+threadRequestLocalSQL+`
		  ORDER BY created_at ASC, token ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return collectThreadRequests(rows, action)
}

// hasOpenThreadRequestsSQL probes a caller for an open request. A
// lineage-only row is settled, so the term changes nothing but the index:
// the probe reads the caller's open rows, not its history.
const hasOpenThreadRequestsSQL = `SELECT EXISTS(SELECT 1 FROM thread_requests
   WHERE caller_thread_id = ? AND lineage_only = 0 AND settled_at IS NULL)`

// HasOpenThreadRequests reports whether a thread still owns an undelivered
// request. The copy refusal and the forget-computer refusal read it.
func (s *Store) HasOpenThreadRequests(callerThreadID string) (bool, error) {
	var open bool
	if err := s.reader().QueryRow(hasOpenThreadRequestsSQL, callerThreadID).Scan(&open); err != nil {
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

// ErrThreadWakeNotOwed is QueueThreadWake refusing a message because one of
// the requests it answers no longer owes that wake: another route delivered
// it, a late reply superseded its first answer, or the wake was disarmed
// (the caller was archived, deleted or moved). Nothing was written. The
// caller reads the requests again and queues what is still owed.
var ErrThreadWakeNotOwed = errors.New("store: a request in this wake no longer owes it")

// QueueThreadWake hands a wake to the ordinary durable message queue and
// records the delivery in the same transaction: the queue row is the
// message's only copy from that moment on, and `delivered_at` is what keeps a
// second observation of the same settlement from queueing it twice.
//
// One message can answer several requests. `token` is the carrier, the
// request whose send id the message is queued under; `riders` are the other
// requests it answers, each marked delivered with the carrier recorded on it.
//
// The message is written only if every request it names was made by the
// thread the message is queued on, is still armed (`notify`) and still owes
// this wake, checked by the statement that marks it. Otherwise nothing is written and the error is
// ErrThreadWakeNotOwed: a message naming a request that was already answered
// would deliver that answer twice.
//
// It is the `persist` hook of the caller's registerQueueItem, so it runs
// BEFORE the in-memory queue register, exactly where InsertFlushQueueItem
// runs for an ordinary send.
//
// `late` marks the second wake (a late reply) instead of the first, and
// takes no riders.
func (s *Store) QueueThreadWake(token string, riders []string, late bool, item FlushQueueItem) (err error) {
	action := fmt.Sprintf("store: queue thread wake %s", token)
	if token == "" {
		return fmt.Errorf("%s: token is required", action)
	}
	if late && len(riders) > 0 {
		return fmt.Errorf("%s: a late reply carries no other request", action)
	}
	named := make(map[string]struct{}, len(riders)+1)
	named[token] = struct{}{}
	for _, rider := range riders {
		if _, twice := named[rider]; twice || rider == "" {
			return fmt.Errorf("%s: rider %q is empty or named twice", action, rider)
		}
		named[rider] = struct{}{}
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer release()
	defer rollbackTx(tx, action, &err)
	if err := markThreadWakeQueuedTx(tx, token, "", late, item); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	for _, rider := range riders {
		if err := markThreadWakeQueuedTx(tx, rider, token, late, item); err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
	}
	if err := insertFlushQueueItem(tx, item); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	for marked := range named {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: marked})
	}
	return nil
}

// markThreadWakeQueuedTx marks one request delivered by the queued message
// `item`, recording `carrier` as the request whose message it is (empty for
// the carrier itself). It is stricter than an inline delivery, which hands
// over whatever the row holds: the request must belong to the thread the
// message is queued on, and a first answer a late reply has superseded is
// not sent, because the reply's own message is. The mark is the first phase
// of the delivery: it is unconfirmed, with no timeline row, until the
// provider echoes the message (ConfirmThreadWake).
func markThreadWakeQueuedTx(tx *sql.Tx, token, carrier string, late bool, item FlushQueueItem) error {
	at := item.EnqueuedAt
	if at == 0 {
		at = nowMillis()
	}
	// `delivered_with` describes the first answer's message, so a late
	// reply's mark leaves it as it is.
	set := "delivered_at = ?, delivered_with = ?, delivered_how = ?, confirmed_at = NULL, wake_item_id = ''"
	owed, args := threadFirstWakeOwedSQL, []any{at, carrier}
	if late {
		set = "late_delivered_at = ?, late_delivered_how = ?, late_confirmed_at = NULL, late_wake_item_id = ''"
		owed, args = threadLateWakeOwedSQL, []any{at}
	}
	args = append(args, ThreadWakeQueued, nowMillis(), token, item.ThreadID)
	result, err := tx.Exec(
		`UPDATE thread_requests
		    SET `+set+`, updated_at = ?,
		        wake_attempts = 0, wake_next_check = 0, wake_issue = ''
		  WHERE token = ? AND caller_thread_id = ? AND notify = 1 AND `+owed,
		args...,
	)
	if err != nil {
		return fmt.Errorf("mark request %s delivered: %w", token, err)
	}
	marked, err := rowsChanged(result, "mark request "+token+" delivered")
	if err != nil {
		return err
	}
	if !marked {
		return fmt.Errorf("request %s: %w", token, ErrThreadWakeNotOwed)
	}
	return nil
}

// threadWakeRidersSQL matches the requests that rode with carrier ?1's first
// wake, restating idx_thread_requests_delivered_with's predicate. A late
// reply has no riders. Every write that changes a carrier's queued first
// wake (its confirmation, its give-back) applies to these rows with it,
// because the provider echoes, and a give-back returns, the one message.
const threadWakeRidersSQL = `delivered_with = ?1 AND delivered_with <> ''`

// WithdrawThreadWake takes a wake that never reached the agent back for its
// request: its queue row goes, if it still has one, and with it the
// delivery mark the queue write set and the request's notify, in one
// transaction. The answer is owed again but disarmed, so only a later wait
// or thread_status re-arms it, and thread_status still returns it.
// Otherwise a row already gone means the wake was sent, and changes
// nothing. A wake the provider was handed and never confirmed
// (unconfirmed) has no row left and goes back all the same, while the
// request still reads queued. A wake the provider confirmed is never
// given back, by any caller: a queue row can outlive its message's echo
// (the process died between the provider write and the settlement that
// deletes the row, or that delete failed), and such a row goes with the
// mark left standing. A late wake clears only its own mark; the first
// answer's delivery stands. A lineage-only row keeps its marks
// (clearLineagePayloadSQL): a cleared delivered_at would read as a wake
// still owed.
//
// token is the carrier: a first wake that answered other requests too
// (delivered_with) gives every one of them back with it, since they were
// one message. It returns the requests it gave back, the carrier first
// when it was among them; a rebuilt message folds them again
// (threadWakeMessages).
func (s *Store) WithdrawThreadWake(token string, late bool, rowID string, scope WakeWithdrawal) (_ []string, err error) {
	action := fmt.Sprintf("store: withdraw thread wake %s", token)
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer release()
	defer rollbackTx(tx, action, &err)
	removed := false
	if rowID != "" {
		result, err := tx.Exec(`DELETE FROM flush_queue_items WHERE id = ?`, rowID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		if removed, err = rowsChanged(result, action); err != nil {
			return nil, err
		}
	}
	var returned []string
	if removed || scope != WithdrawQueued {
		statements := []string{threadWakeGiveBackSQL(late, false)}
		if !late {
			statements = append(statements, threadWakeGiveBackSQL(late, true))
		}
		for _, statement := range statements {
			tokens, err := queryIDs(tx, statement, token, nowMillis(), ThreadWakeQueued)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", action, err)
			}
			returned = append(returned, tokens...)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", action, err)
	}
	for _, returnedToken := range returned {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: returnedToken})
	}
	return returned, nil
}

// threadWakeGiveBackSQL clears one wake's delivery mark (the late reply's
// when late) and disarms it, on the carrier ?1 itself or, with riders, on
// the requests that rode with it, returning each token it gave back. A
// wake an echo confirmed is left as it is. ?2 is the time and ?3 the
// queued route.
func threadWakeGiveBackSQL(late, riders bool) string {
	delivered, confirmed, route, item, with := "delivered_at", "confirmed_at", "delivered_how", "wake_item_id", ", delivered_with = ''"
	if late {
		delivered, confirmed, route, item, with = "late_delivered_at", "late_confirmed_at", "late_delivered_how", "late_wake_item_id", ""
	}
	return `UPDATE thread_requests SET ` + delivered + ` = NULL, ` + route + ` = '', ` + item + ` = ''` + with + `,
	           notify = 0, updated_at = ?2
	     WHERE ` + delivered + ` IS NOT NULL AND ` + route + ` = ?3 AND lineage_only = 0
	       AND ` + confirmed + ` IS NULL` + threadWakeTargetSQL(riders) + ` RETURNING token`
}

// threadWakeTargetSQL picks the rows a carrier ?1's wake write applies to:
// the carrier, or the requests that rode with it (threadWakeRidersSQL).
func threadWakeTargetSQL(riders bool) string {
	if riders {
		return ` AND ` + threadWakeRidersSQL
	}
	return ` AND token = ?1`
}

// ConfirmThreadWake records the provider's echo of a queued answer, or of a
// late reply (late): the second phase of its delivery mark, after which no
// session end or restart gives it back, with itemID, the caller's timeline
// row that carried it. It reports whether the mark changed. An echo of a
// wake already confirmed changes only the row it names: a merge the
// provider made of several messages leaves one row carrying them all
// (triage's fold). An empty itemID names no row and leaves the one recorded.
// A lineage-only row is left as the sweep left it.
//
// token is the carrier the echo names by its send id. A first wake that
// answered other requests too (delivered_with) confirms every one of them
// with the same row, in the same transaction.
func (s *Store) ConfirmThreadWake(token string, late bool, itemID string, at int64) (_ bool, err error) {
	action := fmt.Sprintf("store: confirm thread wake %s", token)
	statements := []string{threadWakeConfirmSQL(late, false)}
	if !late {
		statements = append(statements, threadWakeConfirmSQL(late, true))
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var changed []string
	for _, statement := range statements {
		tokens, err := queryIDs(tx, statement, token, at, itemID, nowMillis())
		if err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
		changed = append(changed, tokens...)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	for _, changedToken := range changed {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: changedToken})
	}
	return len(changed) > 0, nil
}

// threadWakeConfirmSQL confirms one wake (the late reply's when late) on
// the carrier ?1 or, with riders, on the requests that rode with it, at ?2
// with the row ?3, returning each token it changed. ?4 is the time.
func threadWakeConfirmSQL(late, riders bool) string {
	delivered, confirmed, item := "delivered_at", "confirmed_at", "wake_item_id"
	if late {
		delivered, confirmed, item = "late_delivered_at", "late_confirmed_at", "late_wake_item_id"
	}
	return `UPDATE thread_requests
	    SET ` + confirmed + ` = COALESCE(` + confirmed + `, ?2), ` + item + ` = CASE WHEN ?3 = '' THEN ` + item + ` ELSE ?3 END, updated_at = ?4
	  WHERE ` + delivered + ` IS NOT NULL AND lineage_only = 0
	    AND (` + confirmed + ` IS NULL OR (?3 <> '' AND ` + item + ` IS NOT ?3))` + threadWakeTargetSQL(riders) + ` RETURNING token`
}

// UnconfirmedThreadWakes returns the requests whose answer or late reply
// was queued and never confirmed: at boot, no session holds them, so the
// agent may never have taken them in. A request that rode with another's
// first wake is among them, its DeliveredWith naming the message to give
// back. A lineage-only row answers nothing any more and is not among them.
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

// WakeCarrier is the request whose send id carried this row's first answer:
// the carrier it rode with, or the row itself.
func (r ThreadRequest) WakeCarrier() string {
	if r.DeliveredWith != "" {
		return r.DeliveredWith
	}
	return r.Token
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

// ThreadWakeSendID names the message a request's wake is queued under: its
// first answer, or its late reply (late). The two are distinct so a late
// reply is never mistaken for a retry of the first wake and dropped by send
// admission.
func ThreadWakeSendID(token string, late bool) string {
	if late {
		return threadWakeLateSendIDPrefix + token
	}
	return threadWakeSendIDPrefix + token
}

const (
	threadWakeSendIDPrefix     = "thread-wake:"
	threadWakeLateSendIDPrefix = "thread-wake-late:"
)

// ParseThreadWakeSendID recovers the request a wake's send id belongs to:
// the carrier of every request its message answers (WakeCarrier). The two
// prefixes are disjoint, so either order reads the same send id; a bare
// prefix names no request.
func ParseThreadWakeSendID(sendID string) (token string, late bool, ok bool) {
	if rest, found := strings.CutPrefix(sendID, threadWakeLateSendIDPrefix); found {
		return rest, true, rest != ""
	}
	if rest, found := strings.CutPrefix(sendID, threadWakeSendIDPrefix); found {
		return rest, false, rest != ""
	}
	return "", false, false
}
