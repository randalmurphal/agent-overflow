package store

import (
	"database/sql"
	"fmt"
)

// Receipt states. The source row has more (`unconfirmed`, `refused`) because
// they describe a dispatch the destination never saw.
const (
	ThreadReceiptAccepted    = "accepted"
	ThreadReceiptRunning     = "running"
	ThreadReceiptReplied     = "replied"
	ThreadReceiptFinished    = "finished"
	ThreadReceiptErrored     = "errored"
	ThreadReceiptCancelled   = "cancelled"
	ThreadReceiptInterrupted = "interrupted"
	ThreadReceiptExpired     = "expired"
)

var threadReceiptStates = map[string]struct{}{
	ThreadReceiptAccepted: {}, ThreadReceiptRunning: {}, ThreadReceiptReplied: {},
	ThreadReceiptFinished: {}, ThreadReceiptErrored: {}, ThreadReceiptCancelled: {},
	ThreadReceiptInterrupted: {}, ThreadReceiptExpired: {},
}

// threadReceiptOpenStates is what a settlement may replace and what the boot
// sweep settles as interrupted.
var threadReceiptOpenStates = []string{ThreadReceiptAccepted, ThreadReceiptRunning}

// ThreadReceiptOpenStates returns the states a settlement may replace.
func ThreadReceiptOpenStates() []string {
	return append([]string(nil), threadReceiptOpenStates...)
}

// ThreadRequestReceipt is the destination row: one request against this
// computer's threads, local ones included.
//
// The display metadata (source computer and thread names) is what the call
// carried, for the footer and the origin chip. It is never an authorization
// input: OwnerDeviceID is, and it is the authenticated device of the source
// computer, or "local" for an in-process call.
type ThreadRequestReceipt struct {
	Token              string `json:"token"`
	OwnerDeviceID      string `json:"ownerDeviceId"`
	SourceComputerID   string `json:"sourceComputerId,omitempty"`
	SourceComputerName string `json:"sourceComputerName,omitempty"`
	SourceThreadID     string `json:"sourceThreadId,omitempty"`
	SourceThreadTitle  string `json:"sourceThreadTitle,omitempty"`
	Kind               string `json:"kind"`
	// TargetThreadID is empty until the destination has a thread to name:
	// a spawn's new thread and an ask's scratch fork are created after the
	// receipt, so the token can be accepted before either exists.
	// SetThreadReceiptTarget fills it in once.
	TargetThreadID string `json:"targetThreadId,omitempty"`
	// MessageItemID and TurnID bind settlement to the turn that consumed
	// the request's message, and to no other.
	MessageItemID string `json:"messageItemId,omitempty"`
	TurnID        string `json:"turnId,omitempty"`
	State         string `json:"state"`
	// Answer is the copy of record for a remote caller until collected.
	Answer      []byte `json:"-"`
	AnswerKind  string `json:"answerKind,omitempty"`
	LateReply   []byte `json:"-"`
	LateReplyAt int64  `json:"lateReplyAt,omitempty"`
	Revision    int64  `json:"revision"`
	SettledAt   int64  `json:"settledAt,omitempty"`
	// CollectedRevision is the revision the source acknowledged. A late
	// reply is a new revision, collected again.
	CollectedRevision int64 `json:"collectedRevision"`
	CollectedAt       int64 `json:"collectedAt,omitempty"`
	ExpiresAt         int64 `json:"expiresAt,omitempty"`
	CreatedAt         int64 `json:"createdAt"`
	UpdatedAt         int64 `json:"updatedAt"`
}

const threadReceiptColumns = `token, owner_device_id, source_computer_id, source_computer_name,
    source_thread_id, source_thread_title, kind, COALESCE(target_thread_id, ''),
    message_item_id, turn_id, state, answer, answer_kind, late_reply,
    COALESCE(late_reply_at, 0), revision, COALESCE(settled_at, 0),
    collected_revision, COALESCE(collected_at, 0), COALESCE(expires_at, 0),
    created_at, updated_at`

func scanThreadReceipt(row rowScanner) (ThreadRequestReceipt, error) {
	var r ThreadRequestReceipt
	err := row.Scan(
		&r.Token, &r.OwnerDeviceID, &r.SourceComputerID, &r.SourceComputerName,
		&r.SourceThreadID, &r.SourceThreadTitle, &r.Kind, &r.TargetThreadID,
		&r.MessageItemID, &r.TurnID, &r.State, &r.Answer, &r.AnswerKind, &r.LateReply,
		&r.LateReplyAt, &r.Revision, &r.SettledAt,
		&r.CollectedRevision, &r.CollectedAt, &r.ExpiresAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	return r, err
}

func collectThreadReceipts(rows *sql.Rows, action string) ([]ThreadRequestReceipt, error) {
	defer rows.Close()
	out := []ThreadRequestReceipt{}
	for rows.Next() {
		row, err := scanThreadReceipt(rows)
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

// AcceptThreadRequestReceipt inserts the destination row and returns the row
// that now owns the token, plus whether this call created it. A retry with the
// same token returns the existing acceptance and creates nothing: that is what
// makes a lost reply safe to retry, with no second spawn, fork or message.
func (s *Store) AcceptThreadRequestReceipt(r ThreadRequestReceipt) (ThreadRequestReceipt, bool, error) {
	action := fmt.Sprintf("store: accept thread request receipt %s", r.Token)
	if r.Token == "" || r.OwnerDeviceID == "" {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: token and owner device are required", action)
	}
	if _, ok := threadRequestKinds[r.Kind]; !ok {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: invalid kind %q", action, r.Kind)
	}
	// A spawn creates its thread and an ask forks its scratch thread only
	// after the token is durable, so those two may name their target later.
	// Everything else is addressed to a thread that already exists.
	if r.TargetThreadID == "" && r.Kind != ThreadRequestSpawn && r.Kind != ThreadRequestAsk {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: a %s receipt requires a target thread", action, r.Kind)
	}
	if r.State == "" {
		r.State = ThreadReceiptAccepted
	}
	if _, ok := threadReceiptStates[r.State]; !ok {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: invalid state %q", action, r.State)
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = nowMillis()
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = r.CreatedAt
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`INSERT INTO thread_request_receipts (
		    token, owner_device_id, source_computer_id, source_computer_name,
		    source_thread_id, source_thread_title, kind, target_thread_id,
		    message_item_id, turn_id, state, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token) DO NOTHING`,
		r.Token, r.OwnerDeviceID, r.SourceComputerID, r.SourceComputerName,
		r.SourceThreadID, r.SourceThreadTitle, r.Kind, nilIfEmpty(r.TargetThreadID),
		r.MessageItemID, r.TurnID, r.State, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: %w", action, err)
	}
	created, err := rowsChanged(result, action)
	if err != nil {
		return ThreadRequestReceipt{}, false, err
	}
	stored, err := scanThreadReceipt(tx.QueryRow(
		`SELECT `+threadReceiptColumns+` FROM thread_request_receipts WHERE token = ?`, r.Token))
	if err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: read back: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return stored, created, nil
}

// SetThreadReceiptTarget names the thread a spawn or ask receipt answers for,
// once the destination has created it. It applies only while the target is
// still undecided, so a retry that raced the first attempt cannot repoint a
// receipt at a second thread; false means the target was already set and the
// caller's thread is the loser of that race.
func (s *Store) SetThreadReceiptTarget(token, threadID string) (bool, error) {
	action := fmt.Sprintf("store: set thread receipt %s target", token)
	if threadID == "" {
		return false, fmt.Errorf("%s: thread id is required", action)
	}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts SET target_thread_id = ?, updated_at = ?
		  WHERE token = ? AND target_thread_id IS NULL`,
		threadID, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// DetachThreadReceiptsFromThread clears the thread binding of every settled
// receipt of one thread, and returns how many it cleared.
//
// It is called where that thread is about to stop existing here: deleted,
// moved away, or the hidden fork an ask ran in. The binding is a foreign
// key with ON DELETE CASCADE, so without this the answer would go with the
// thread, and the answer is the record a paired computer has not collected
// yet. An open receipt keeps its thread: its caller settles it first.
func (s *Store) DetachThreadReceiptsFromThread(threadID string) (int64, error) {
	action := fmt.Sprintf("store: detach thread receipts from %s", threadID)
	if threadID == "" {
		return 0, nil
	}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts SET target_thread_id = NULL, updated_at = ?
		  WHERE target_thread_id = ? AND settled_at IS NOT NULL`,
		nowMillis(), threadID,
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

// MarkThreadReceiptRunning records the user row the request wrote and the
// turn that consumed it, and moves the receipt out of `accepted`. Until it
// applies, the request's message is still queued and no turn can settle it.
func (s *Store) MarkThreadReceiptRunning(token, messageItemID, turnID string) (bool, error) {
	action := fmt.Sprintf("store: mark thread receipt %s running", token)
	if turnID == "" {
		return false, fmt.Errorf("%s: turn id is required", action)
	}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET state = 'running', message_item_id = ?, turn_id = ?, updated_at = ?
		  WHERE token = ? AND state = 'accepted'`,
		messageItemID, turnID, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// SettleThreadRequestReceipt stores the whole answer on the destination row
// and bumps the revision, provided the row is still in one of `from`. The
// turn-end observer passes {running} so a thread_reply that landed first
// wins; the cancel path passes both open states.
func (s *Store) SettleThreadRequestReceipt(token string, from []string, settlement ThreadRequestSettlement) (bool, error) {
	action := fmt.Sprintf("store: settle thread receipt %s", token)
	if err := settlement.validate(action, threadReceiptStates); err != nil {
		return false, err
	}
	clause, args, err := stateInClause(action, from, threadReceiptStates)
	if err != nil {
		return false, err
	}
	settledAt := settlement.SettledAt
	if settledAt == 0 {
		settledAt = nowMillis()
	}
	expiresAt := settlement.ExpiresAt
	if expiresAt == 0 {
		expiresAt = settledAt + threadAnswerHoldMillis
	}
	set := []any{settlement.State, nilIfEmptyBytes(settlement.Answer), settlement.AnswerKind,
		settledAt, expiresAt, nowMillis(), token}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
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

// threadAnswerHoldMillis is how long an uncollected answer waits for its
// caller before the sweep drops it (the spec's one day).
const threadAnswerHoldMillis = 24 * 60 * 60 * 1000

// StoreThreadReceiptLateReply records a thread_reply that arrived after the
// receipt had already finished without one. It is a new revision, so the
// source's poller collects it and a second wake lands. Only a `finished`
// receipt with no late reply yet accepts it; every other state is refused by
// the caller with its state as the reason.
func (s *Store) StoreThreadReceiptLateReply(token string, reply []byte, at int64) (bool, error) {
	action := fmt.Sprintf("store: store thread receipt late reply %s", token)
	if at == 0 {
		at = nowMillis()
	}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET late_reply = ?, late_reply_at = ?, revision = revision + 1,
		        expires_at = ?, updated_at = ?
		  WHERE token = ? AND state = 'finished' AND late_reply IS NULL`,
		nilIfEmptyBytes(reply), at, at+threadAnswerHoldMillis, nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// AckThreadRequestReceipt records the revision the source has durably stored.
// The sweep only drops an answer whose latest revision nobody collected, so
// the acknowledgement is what makes expiry safe.
func (s *Store) AckThreadRequestReceipt(token string, revision, at int64) (bool, error) {
	action := fmt.Sprintf("store: acknowledge thread receipt %s", token)
	if at == 0 {
		at = nowMillis()
	}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET collected_revision = ?, collected_at = ?, updated_at = ?
		  WHERE token = ? AND collected_revision < ?`,
		revision, at, nowMillis(), token, revision,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// GetThreadRequestReceipt reads one receipt. The bool separates an unknown
// token, which is `thread_request_unknown` on the wire, from a read failure.
func (s *Store) GetThreadRequestReceipt(token string) (ThreadRequestReceipt, bool, error) {
	row, err := scanThreadReceipt(s.reader().QueryRow(
		`SELECT `+threadReceiptColumns+` FROM thread_request_receipts WHERE token = ?`, token))
	if err == sql.ErrNoRows {
		return ThreadRequestReceipt{}, false, nil
	}
	if err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("store: get thread receipt %s: %w", token, err)
	}
	return row, true, nil
}

// ListThreadRequestReceiptsForThread returns every receipt against one thread,
// newest first. The settlement observer and the responder-enable rule read it.
// A receipt whose target is still undecided belongs to no thread yet and is
// listed by none of them; the boot sweep reaches it through
// ListOpenThreadRequestReceipts instead.
func (s *Store) ListThreadRequestReceiptsForThread(threadID string) ([]ThreadRequestReceipt, error) {
	action := fmt.Sprintf("store: list thread receipts for %s", threadID)
	rows, err := s.reader().Query(
		`SELECT `+threadReceiptColumns+` FROM thread_request_receipts
		  WHERE target_thread_id = ? ORDER BY created_at DESC, token ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return collectThreadReceipts(rows, action)
}

// ListOpenThreadRequestReceipts returns every receipt still `accepted` or
// `running`, targeted or not. Boot settles all of them as interrupted: a
// queued send that never reached the provider can no longer settle on its own,
// and neither can a spawn accepted before the restart that was to create its
// thread.
func (s *Store) ListOpenThreadRequestReceipts() ([]ThreadRequestReceipt, error) {
	action := "store: list open thread receipts"
	rows, err := s.reader().Query(
		`SELECT ` + threadReceiptColumns + ` FROM thread_request_receipts
		  WHERE state IN ('accepted','running') ORDER BY created_at ASC, token ASC`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return collectThreadReceipts(rows, action)
}

// ExpireThreadRequestAnswers drops the answer text of every settled receipt
// whose latest revision nobody collected and whose hold has run out, and marks
// it `expired`. The row itself stays until the retention floor so the token
// keeps answering "already ran". Returns how many answers were dropped.
func (s *Store) ExpireThreadRequestAnswers(now int64) (int64, error) {
	action := "store: expire thread request answers"
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET answer = NULL, late_reply = NULL, state = 'expired', updated_at = ?
		  WHERE settled_at IS NOT NULL
		    AND expires_at IS NOT NULL AND expires_at <= ?
		    AND collected_revision < revision
		    AND state <> 'expired'`,
		nowMillis(), now,
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
