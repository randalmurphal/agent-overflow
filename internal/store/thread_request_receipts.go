package store

import (
	"database/sql"
	"errors"
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
// carried, for the footer and the sender an agent row names. It is never an authorization
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
	// the request's message, and to no other. A `running` receipt with no
	// TurnID was handed to a provider that has not delivered it yet.
	MessageItemID string `json:"messageItemId,omitempty"`
	TurnID        string `json:"turnId,omitempty"`
	// OwnTurn reports that the TurnID turn opened for the request's message
	// (or the joined message carrying it), so a cancel may interrupt it.
	// False for a message drained into a turn already running.
	OwnTurn bool   `json:"ownTurn,omitempty"`
	State   string `json:"state"`
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
	// AnswerAgent is the target thread as a standing agent when the answer
	// or the late reply was written; nil for any other thread.
	AnswerAgent *ThreadAnswerAgent `json:"-"`
}

const threadReceiptColumns = `token, owner_device_id, source_computer_id, source_computer_name,
    source_thread_id, source_thread_title, kind, COALESCE(target_thread_id, ''),
    message_item_id, turn_id, state, answer, answer_kind, late_reply,
    COALESCE(late_reply_at, 0), revision, COALESCE(settled_at, 0),
    collected_revision, COALESCE(collected_at, 0), COALESCE(expires_at, 0),
    created_at, updated_at, answer_agent, answer_agent_role, own_turn`

func scanThreadReceipt(row rowScanner) (ThreadRequestReceipt, error) {
	var r ThreadRequestReceipt
	var answerAgent int64
	var answerAgentRole string
	err := row.Scan(
		&r.Token, &r.OwnerDeviceID, &r.SourceComputerID, &r.SourceComputerName,
		&r.SourceThreadID, &r.SourceThreadTitle, &r.Kind, &r.TargetThreadID,
		&r.MessageItemID, &r.TurnID, &r.State, &r.Answer, &r.AnswerKind, &r.LateReply,
		&r.LateReplyAt, &r.Revision, &r.SettledAt,
		&r.CollectedRevision, &r.CollectedAt, &r.ExpiresAt,
		&r.CreatedAt, &r.UpdatedAt, &answerAgent, &answerAgentRole, &r.OwnTurn,
	)
	r.AnswerAgent = answerAgentOf(answerAgent, answerAgentRole)
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
	r, err := r.validated(action)
	if err != nil {
		return ThreadRequestReceipt{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(insertThreadReceiptSQL+` ON CONFLICT(token) DO NOTHING`, r.insertArgs()...)
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

// AcceptLocalThreadRequest writes a request one of this computer's threads
// makes to another of them, both rows in one transaction: the source row
// and the receipt that answers it. A request whose target thread exists
// before it is accepted (a send) is written `accepted` and naming it. A
// spawn or an ask creates its thread after the receipt, so its row is
// written `unconfirmed` with no target, and NameLocalThreadRequestTarget
// names the thread and accepts it. The token is fresh, so a receipt
// already holding it is an error rather than a retry.
func (s *Store) AcceptLocalThreadRequest(request ThreadRequest, receipt ThreadRequestReceipt) (ThreadRequestReceipt, error) {
	action := fmt.Sprintf("store: accept local thread request %s", request.Token)
	if receipt.Token != request.Token || receipt.Kind != request.Kind ||
		receipt.TargetThreadID != request.TargetThreadID || request.TargetComputerID != "" {
		return ThreadRequestReceipt{}, fmt.Errorf("%s: the receipt must name the request's token, kind and local target", action)
	}
	want := ThreadRequestAccepted
	if request.TargetThreadID == "" {
		want = ThreadRequestUnconfirmed
	}
	if request.State != want {
		return ThreadRequestReceipt{}, fmt.Errorf("%s: a local request is written %s, not %q", action, want, request.State)
	}
	receipt, err := receipt.validated(action)
	if err != nil {
		return ThreadRequestReceipt{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ThreadRequestReceipt{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertThreadRequestTx(tx, request); err != nil {
		return ThreadRequestReceipt{}, err
	}
	stored, err := scanThreadReceipt(tx.QueryRow(insertThreadReceiptSQL+` RETURNING `+threadReceiptColumns, receipt.insertArgs()...))
	if err != nil {
		return ThreadRequestReceipt{}, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return ThreadRequestReceipt{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: request.Token, CallerThreadID: request.CallerThreadID})
	return stored, nil
}

// NameLocalThreadRequestTarget names the thread a local spawn or ask
// created, on its receipt and its source row in one transaction, and
// accepts the source row unless a settlement already moved it. The receipt
// must still have no target and the source row must be this computer's
// own: anything else is an error, and nothing is written.
func (s *Store) NameLocalThreadRequestTarget(token, threadID string) error {
	action := fmt.Sprintf("store: name local thread request %s target", token)
	if threadID == "" {
		return fmt.Errorf("%s: thread id is required", action)
	}
	now := nowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE thread_request_receipts SET target_thread_id = ?, updated_at = ?
		  WHERE token = ? AND target_thread_id IS NULL`, threadID, now, token)
	if err != nil {
		return fmt.Errorf("%s: receipt: %w", action, err)
	}
	if named, err := rowsChanged(result, action); err != nil {
		return err
	} else if !named {
		return fmt.Errorf("%s: no receipt is waiting for its thread", action)
	}
	var caller string
	if err := tx.QueryRow(`UPDATE thread_requests
		    SET target_thread_id = ?, target_thread_title = '', target_provider = '', updated_at = ?,
		        state = CASE WHEN state = ? THEN ? ELSE state END
		  WHERE token = ? AND target_computer_id = '' AND lineage_only = 0
		  RETURNING caller_thread_id`,
		threadID, now, ThreadRequestUnconfirmed, ThreadRequestAccepted, token).Scan(&caller); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s: no local source row", action)
		}
		return fmt.Errorf("%s: source: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token, CallerThreadID: caller})
	return nil
}

const insertThreadReceiptSQL = `INSERT INTO thread_request_receipts (
    token, owner_device_id, source_computer_id, source_computer_name,
    source_thread_id, source_thread_title, kind, target_thread_id,
    message_item_id, turn_id, state, created_at, updated_at
 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func (r ThreadRequestReceipt) insertArgs() []any {
	return []any{r.Token, r.OwnerDeviceID, r.SourceComputerID, r.SourceComputerName,
		r.SourceThreadID, r.SourceThreadTitle, r.Kind, nilIfEmpty(r.TargetThreadID),
		r.MessageItemID, r.TurnID, r.State, r.CreatedAt, r.UpdatedAt}
}

// validated checks a receipt about to be accepted and fills its defaults.
func (r ThreadRequestReceipt) validated(action string) (ThreadRequestReceipt, error) {
	if r.Token == "" || r.OwnerDeviceID == "" {
		return r, fmt.Errorf("%s: token and owner device are required", action)
	}
	if _, ok := threadRequestKinds[r.Kind]; !ok {
		return r, fmt.Errorf("%s: invalid kind %q", action, r.Kind)
	}
	// A spawn creates its thread and an ask forks its scratch thread only
	// after the token is durable, so those two may name their target later.
	// Everything else is addressed to a thread that already exists.
	if r.TargetThreadID == "" && r.Kind != ThreadRequestSpawn && r.Kind != ThreadRequestAsk {
		return r, fmt.Errorf("%s: a %s receipt requires a target thread", action, r.Kind)
	}
	if r.State == "" {
		r.State = ThreadReceiptAccepted
	}
	if _, ok := threadReceiptStates[r.State]; !ok {
		return r, fmt.Errorf("%s: invalid state %q", action, r.State)
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = nowMillis()
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = r.CreatedAt
	}
	return r, nil
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

// ClearThreadRequestReceipts removes every receipt. A settled receipt
// outlives its thread by design (DetachThreadReceiptsFromThread), so the
// project cascade leaves it behind; this is how the harness reset keeps one
// test's requests out of the next. Production never calls it: a receipt is
// the record a paired computer has not collected yet.
func (s *Store) ClearThreadRequestReceipts() error {
	if _, err := s.db.Exec(`DELETE FROM thread_request_receipts`); err != nil {
		return fmt.Errorf("store: clear thread request receipts: %w", err)
	}
	return nil
}

// MarkThreadReceiptDispatched records the user row the request wrote once
// the message is handed to the provider, and moves the receipt out of
// `accepted` with no turn bound. The provider can still hold the message for
// a turn of its own, so the turn waits for BindThreadReceiptTurn and no turn
// end settles the receipt until then. A source row on this computer moves
// from `accepted` to `running` in the same transaction, whether or not the
// receipt still was `accepted`: a delivery that bound it first moved the
// receipt, and the source follows either way.
func (s *Store) MarkThreadReceiptDispatched(token, messageItemID string) (bool, error) {
	action := fmt.Sprintf("store: mark thread receipt %s dispatched", token)
	return s.writeReceiptAndRunSource(action, token,
		`UPDATE thread_request_receipts
		    SET state = 'running', message_item_id = ?, updated_at = ?
		  WHERE token = ? AND state = 'accepted'`,
		messageItemID, nowMillis(), token)
}

// BindThreadReceiptTurn records the turn the request's message was
// delivered into, the turn whose end settles it, and whether that turn
// opened for the message (ownTurn). It and RebindThreadReceiptTurn are the
// only writers of turn_id and own_turn, and the first delivery wins: a
// receipt that already names a turn, or has settled, is left alone. A
// delivery can arrive before the dispatch mark, so it moves an `accepted`
// receipt too. A source row on this computer moves from `accepted` to
// `running` in the same transaction, as MarkThreadReceiptDispatched moves
// it.
func (s *Store) BindThreadReceiptTurn(token, messageItemID, turnID string, ownTurn bool) (bool, error) {
	action := fmt.Sprintf("store: bind thread receipt %s to its turn", token)
	if turnID == "" {
		return false, fmt.Errorf("%s: turn id is required", action)
	}
	return s.writeReceiptAndRunSource(action, token,
		`UPDATE thread_request_receipts
		    SET state = 'running', message_item_id = ?, turn_id = ?, own_turn = ?, updated_at = ?
		  WHERE token = ? AND (state = 'accepted' OR (state = 'running' AND turn_id = ''))`,
		messageItemID, turnID, ownTurn, nowMillis(), token)
}

// writeReceiptAndRunSource runs one conditional receipt write and advances
// the token's source row from `accepted` to `running`, in one transaction.
// It reports whether the receipt write applied. A request another computer
// made has no source row here, and the advance changes nothing.
func (s *Store) writeReceiptAndRunSource(action, token, query string, args ...any) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	applied, err := rowsChanged(result, action)
	if err != nil {
		return false, err
	}
	result, err = tx.Exec(`UPDATE thread_requests SET state = 'running', updated_at = ? WHERE token = ? AND state = 'accepted'`,
		nowMillis(), token)
	if err != nil {
		return false, fmt.Errorf("%s: advance the request to running: %w", action, err)
	}
	advanced, err := rowsChanged(result, action)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	if advanced {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return applied, nil
}

// RebindThreadReceiptTurn moves a running receipt from the turn its delivery
// named (fromTurnID, which is turn fromIndex of threadID) to toTurnID, once a
// merge fold has shown the provider ran the message in that later turn. It
// moves only from a turn that never ran, one with no rows left. Such a turn
// can carry a completion all the same, written when the next turn opened, and
// that completion answers nobody. A receipt bound anywhere else, or settled,
// is left alone.
func (s *Store) RebindThreadReceiptTurn(token, messageItemID, threadID string, fromIndex int, fromTurnID, toTurnID string, ownTurn bool) (bool, error) {
	action := fmt.Sprintf("store: rebind thread receipt %s to its turn", token)
	if fromTurnID == "" || toTurnID == "" {
		return false, fmt.Errorf("%s: both turn ids are required", action)
	}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET message_item_id = ?, turn_id = ?, own_turn = ?, updated_at = ?
		  WHERE token = ? AND state = 'running' AND turn_id = ?
		    AND NOT EXISTS (SELECT 1 FROM timeline_items WHERE thread_id = ? AND turn_index = ?)`,
		messageItemID, toTurnID, ownTurn, nowMillis(), token, fromTurnID, threadID, fromIndex,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// ReturnThreadReceipt takes a receipt back to `accepted` when the turn its
// message started was un-sent before it answered: the message goes back on
// the queue, and the next delivery binds the receipt again
// (BindThreadReceiptTurn). It reports whether the receipt is open and
// accepted afterwards; a settled or cancelled one is left as it is.
func (s *Store) ReturnThreadReceipt(token string) (bool, error) {
	action := fmt.Sprintf("store: return thread receipt %s", token)
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET state = 'accepted', message_item_id = '', turn_id = '', own_turn = 0, updated_at = ?
		  WHERE token = ? AND state IN ('accepted', 'running')`,
		nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// settleThreadReceiptRow is the receipt's conditional settlement on q, and
// the row as it left it.
func settleThreadReceiptRow(q sqlQueryer, token string, from []string, settlement ThreadRequestSettlement) (ThreadRequestReceipt, bool, error) {
	action := fmt.Sprintf("store: settle thread receipt %s", token)
	if err := settlement.validate(action, threadReceiptStates); err != nil {
		return ThreadRequestReceipt{}, false, err
	}
	clause, args, err := stateInClause(action, from, threadReceiptStates)
	if err != nil {
		return ThreadRequestReceipt{}, false, err
	}
	settledAt := settlement.SettledAt
	if settledAt == 0 {
		settledAt = nowMillis()
	}
	expiresAt := settlement.ExpiresAt
	if expiresAt == 0 {
		expiresAt = settledAt + threadAnswerHoldMillis
	}
	agent, role := answerAgentColumns(settlement.Agent)
	set := []any{settlement.State, nilIfEmptyBytes(settlement.Answer), settlement.AnswerKind,
		settledAt, expiresAt, agent, role, nowMillis(), token}
	receipt, err := scanThreadReceipt(q.QueryRow(
		`UPDATE thread_request_receipts
		    SET state = ?, answer = ?, answer_kind = ?, revision = revision + 1,
		        settled_at = ?, expires_at = ?, answer_agent = ?, answer_agent_role = ?, updated_at = ?
		  WHERE token = ? AND `+clause+` RETURNING `+threadReceiptColumns,
		append(set, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRequestReceipt{}, false, nil
	}
	if err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: %w", action, err)
	}
	return receipt, true, nil
}

// LocalSettlement is what SettleLocalThreadRequest wrote.
type LocalSettlement struct {
	// Settled is whether the receipt was still in one of the states the
	// settlement may replace; false is a lost race, and nothing was written.
	Settled bool
	// Receipt is the receipt as the settlement left it.
	Receipt ThreadRequestReceipt
	// Collected is whether this computer's source row took the answer, and
	// Notify whether that row's wake is armed. Both are false for a request
	// another computer made, whose poller collects it.
	Collected bool
	Notify    bool
	// Outbound is true when the token's source row names a request this
	// computer sent to another one: the receipt was minted elsewhere and
	// its answer is not that request's.
	Outbound bool
}

// SettleLocalThreadRequest settles a receipt and, when the request was made
// on this computer, hands the answer to its source row and acknowledges it,
// in one transaction: the receipt is written provided it is still in one
// of from, the source row takes the receipt's answer in sourceState
// provided it is still open, and the receipt records the revision the
// source stored. The receipt keeps its own copy of the answer. The
// turn-end observer passes {running} so a thread_reply that landed first
// wins; the cancel path passes both open states.
func (s *Store) SettleLocalThreadRequest(token string, from []string, settlement ThreadRequestSettlement, sourceState string) (LocalSettlement, error) {
	action := fmt.Sprintf("store: settle local thread request %s", token)
	if _, ok := threadRequestStates[sourceState]; !ok {
		return LocalSettlement{}, fmt.Errorf("%s: invalid source state %q", action, sourceState)
	}
	openClause, openArgs, err := stateInClause(action, threadRequestOpenStates, threadRequestStates)
	if err != nil {
		return LocalSettlement{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return LocalSettlement{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	receipt, settled, err := settleThreadReceiptRow(tx, token, from, settlement)
	if err != nil || !settled {
		return LocalSettlement{}, err
	}
	out := LocalSettlement{Settled: true, Receipt: receipt}
	var targetComputer string
	var notify int
	err = tx.QueryRow(`SELECT target_computer_id, notify FROM thread_requests WHERE token = ? AND lineage_only = 0`, token).
		Scan(&targetComputer, &notify)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return LocalSettlement{}, fmt.Errorf("%s: read the source row: %w", action, err)
	case targetComputer != "":
		out.Outbound = true
	default:
		agent, role := answerAgentColumns(receipt.AnswerAgent)
		now := nowMillis()
		result, err := tx.Exec(
			`UPDATE thread_requests
			    SET state = ?, answer = ?, answer_kind = ?, revision = revision + 1,
			        settled_at = ?, expires_at = ?, answer_agent = ?, answer_agent_role = ?, updated_at = ?
			  WHERE token = ? AND `+openClause,
			append([]any{sourceState, nilIfEmptyBytes(receipt.Answer), receipt.AnswerKind, receipt.SettledAt,
				nilIfZero(receipt.ExpiresAt), agent, role, now, token}, openArgs...)...)
		if err != nil {
			return LocalSettlement{}, fmt.Errorf("%s: settle the source row: %w", action, err)
		}
		if out.Collected, err = rowsChanged(result, action); err != nil {
			return LocalSettlement{}, err
		}
		if out.Collected {
			// The source has the answer durably; the receipt may drop its copy.
			if _, err := tx.Exec(`UPDATE thread_request_receipts SET collected_revision = ?, collected_at = ?, updated_at = ?
			    WHERE token = ? AND collected_revision < ?`, receipt.Revision, now, now, token, receipt.Revision); err != nil {
				return LocalSettlement{}, fmt.Errorf("%s: acknowledge: %w", action, err)
			}
			out.Receipt.CollectedRevision, out.Receipt.CollectedAt = receipt.Revision, now
			out.Notify = notify != 0
		}
	}
	if err := tx.Commit(); err != nil {
		return LocalSettlement{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	if out.Collected {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return out, nil
}

// threadAnswerHoldMillis is how long an uncollected answer waits for its
// caller before the sweep drops it (the spec's one day).
const threadAnswerHoldMillis = 24 * 60 * 60 * 1000

// StoreThreadReceiptLateReply records a thread_reply that arrived after the
// receipt had already finished without one. It is a new revision, so the
// source's poller collects it and a second wake lands. Only a `finished`
// receipt with no late reply yet and a hold still running accepts it: the
// source stops polling once the hold on the first answer runs out, so a reply
// after that would never be collected. Every other state is refused by the
// caller with its state as the reason. agent is the replying thread as a
// standing agent, and replaces the one the first answer recorded.
func (s *Store) StoreThreadReceiptLateReply(token string, reply []byte, at int64, agent *ThreadAnswerAgent) (bool, error) {
	action := fmt.Sprintf("store: store thread receipt late reply %s", token)
	if at == 0 {
		at = nowMillis()
	}
	isAgent, role := answerAgentColumns(agent)
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET late_reply = ?, late_reply_at = ?, answer_agent = ?, answer_agent_role = ?, revision = revision + 1,
		        expires_at = ?, updated_at = ?
		  WHERE token = ? AND state = 'finished' AND late_reply IS NULL
		    AND expires_at > ?`,
		nilIfEmptyBytes(reply), at, isAgent, role, at+threadAnswerHoldMillis, nowMillis(), token, at,
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
