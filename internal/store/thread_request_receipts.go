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

// localReceiptOwner is OwnerDeviceID for an in-process call.
const localReceiptOwner = "local"

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
	Answer     []byte `json:"-"`
	AnswerKind string `json:"answerKind,omitempty"`
	// Cause and CauseArg are ThreadRequest.Cause and CauseArg.
	Cause       string `json:"-"`
	CauseArg    string `json:"-"`
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
	// was written; nil for any other thread.
	AnswerAgent *ThreadAnswerAgent `json:"-"`
	// LateReplyAgent is AnswerAgent for the late reply.
	LateReplyAgent *ThreadAnswerAgent `json:"-"`
	// SpawnInstructions are a spawn's thread's instructions, recorded when
	// the receipt named its thread (spawnInstructionsAtNamingSQL); empty
	// for every other kind.
	SpawnInstructions string `json:"-"`
	// SpawnLink marks a paired computer's owned spawn: its thread is owned
	// by the source thread there while the link stands (migration v158,
	// spawn_links.go). Written at acceptance, ended by a release.
	SpawnLink  bool `json:"-"`
	TrackSpawn bool `json:"-"`
	// OwnerSeenAt is when the source computer last reached a link; the
	// acceptance writes the creation time (touchSpawnLinkOwner).
	OwnerSeenAt int64 `json:"-"`
	// OwnerOpSeq is the newest owner operation the link admitted.
	OwnerOpSeq int64 `json:"-"`
}

const threadReceiptColumns = `token, owner_device_id, source_computer_id, source_computer_name,
    source_thread_id, source_thread_title, kind, COALESCE(target_thread_id, ''),
    message_item_id, turn_id, state, answer, answer_kind, late_reply,
    COALESCE(late_reply_at, 0), revision, COALESCE(settled_at, 0),
    collected_revision, COALESCE(collected_at, 0), COALESCE(expires_at, 0),
    created_at, updated_at, answer_agent, answer_agent_role, own_turn,
    late_reply_agent, late_reply_agent_role, spawn_instructions,
    spawn_link, owner_seen_at, owner_op_seq, answer_cause, answer_cause_arg, track_spawn`

func scanThreadReceipt(row rowScanner) (ThreadRequestReceipt, error) {
	var r ThreadRequestReceipt
	var answerAgent, lateReplyAgent int64
	var answerAgentRole, lateReplyAgentRole string
	err := row.Scan(
		&r.Token, &r.OwnerDeviceID, &r.SourceComputerID, &r.SourceComputerName,
		&r.SourceThreadID, &r.SourceThreadTitle, &r.Kind, &r.TargetThreadID,
		&r.MessageItemID, &r.TurnID, &r.State, &r.Answer, &r.AnswerKind, &r.LateReply,
		&r.LateReplyAt, &r.Revision, &r.SettledAt,
		&r.CollectedRevision, &r.CollectedAt, &r.ExpiresAt,
		&r.CreatedAt, &r.UpdatedAt, &answerAgent, &answerAgentRole, &r.OwnTurn,
		&lateReplyAgent, &lateReplyAgentRole, &r.SpawnInstructions,
		&r.SpawnLink, &r.OwnerSeenAt, &r.OwnerOpSeq, &r.Cause, &r.CauseArg, &r.TrackSpawn,
	)
	r.AnswerAgent = answerAgentOf(answerAgent, answerAgentRole)
	r.LateReplyAgent = answerAgentOf(lateReplyAgent, lateReplyAgentRole)
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
	if created && stored.OwnerDeviceID != localReceiptOwner {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: stored.Token})
	}
	return stored, created, nil
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

// ThreadReceiptSettled reports whether a receipt state carries an answer a
// request can collect: anything but the two open states, and the empty word
// of a receipt nobody reported.
func ThreadReceiptSettled(state string) bool {
	switch state {
	case "", ThreadReceiptAccepted, ThreadReceiptRunning:
		return false
	}
	return true
}

// GetThreadRequestReceipt reads one receipt. The bool separates an unknown
// token, which is `thread_request_unknown` on the wire, from a read failure.
// A receipt the retention floor reduced to its lineage is unknown.
func (s *Store) GetThreadRequestReceipt(token string) (ThreadRequestReceipt, bool, error) {
	row, err := scanThreadReceipt(s.reader().QueryRow(
		`SELECT `+threadReceiptColumns+` FROM thread_request_receipts WHERE token = ? AND lineage_only = 0`, token))
	if err == sql.ErrNoRows {
		return ThreadRequestReceipt{}, false, nil
	}
	if err != nil {
		return ThreadRequestReceipt{}, false, fmt.Errorf("store: get thread receipt %s: %w", token, err)
	}
	return row, true, nil
}

// ListThreadRequestReceiptsForThread returns every receipt against one thread,
// newest first, less the lineage the retention floor kept. The settlement
// observer and the responder-enable rule read it.
// A receipt whose target is still undecided belongs to no thread yet and is
// listed by none of them; the boot sweep reaches it through
// ListOpenThreadRequestReceipts instead.
func (s *Store) ListThreadRequestReceiptsForThread(threadID string) ([]ThreadRequestReceipt, error) {
	action := fmt.Sprintf("store: list thread receipts for %s", threadID)
	rows, err := s.reader().Query(
		`SELECT `+threadReceiptColumns+` FROM thread_request_receipts
		  WHERE target_thread_id = ? AND lineage_only = 0 ORDER BY created_at DESC, token ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return collectThreadReceipts(rows, action)
}

// ThreadSpawnedForSource reports whether threadID is a thread a spawn from
// sourceThreadID on the computer owner names made here: a spawn receipt
// naming the three, its lineage past the retention floor included. One
// search of the receipts by target (idx_thread_request_receipts_target).
func (s *Store) ThreadSpawnedForSource(threadID, owner, sourceThreadID string) (bool, error) {
	var found bool
	err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_request_receipts
		 WHERE target_thread_id = ? AND kind = 'spawn' AND owner_device_id = ? AND source_thread_id = ?)`,
		threadID, owner, sourceThreadID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: read whether %s was spawned for %s: %w", threadID, sourceThreadID, err)
	}
	return found, nil
}

// PeerSpawnReceiptOf names the receipt of the spawn a paired computer made
// threadID with here: a spawn receipt naming it whose token no request on
// this computer holds. One search of the receipts by target.
func (s *Store) PeerSpawnReceiptOf(threadID string) (string, bool, error) {
	var token string
	err := s.reader().QueryRow(`SELECT rc.token FROM thread_request_receipts rc
		 WHERE rc.target_thread_id = ? AND rc.kind = 'spawn'
		   AND NOT EXISTS (SELECT 1 FROM thread_requests r WHERE r.token = rc.token)
		 LIMIT 1`, threadID).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read the paired computer's spawn of %s: %w", threadID, err)
	}
	return token, true, nil
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
	tokens, err := queryIDs(s.db,
		`UPDATE thread_request_receipts
		    SET answer = NULL, answer_cause = '', answer_cause_arg = '', late_reply = NULL, state = 'expired', updated_at = ?
		  WHERE settled_at IS NOT NULL
		    AND expires_at IS NOT NULL AND expires_at <= ?
		    AND collected_revision < revision
		    AND state <> 'expired' RETURNING token`,
		nowMillis(), now,
	)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	for _, token := range tokens {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return int64(len(tokens)), nil
}
