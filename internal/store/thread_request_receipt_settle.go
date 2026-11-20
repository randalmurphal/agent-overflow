package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Settling a receipt: its conditional settlement, the local settlement that
// hands the answer to this computer's source row in the same transaction, a
// late reply, and the source's acknowledgement of what it stored.

// settleThreadReceiptRow is the receipt's conditional settlement on q, and
// the row as it left it. A receipt has no wake: the settlement refuses
// ArmWake and DisarmWake, which are the source row's half
// (SettleLocalThreadRequest).
func settleThreadReceiptRow(q sqlQueryer, token string, from []string, settlement ThreadRequestSettlement) (ThreadRequestReceipt, bool, error) {
	action := fmt.Sprintf("store: settle thread receipt %s", token)
	if err := settlement.validate(action, threadReceiptStates); err != nil {
		return ThreadRequestReceipt{}, false, err
	}
	if settlement.ArmWake || settlement.DisarmWake {
		return ThreadRequestReceipt{}, false, fmt.Errorf("%s: a receipt has no wake to arm or disarm", action)
	}
	clause, args, err := stateInClause(action, from, threadReceiptStates)
	if err != nil {
		return ThreadRequestReceipt{}, false, err
	}
	if settlement.SettledAt == 0 {
		settlement.SettledAt = nowMillis()
	}
	if settlement.ExpiresAt == 0 {
		settlement.ExpiresAt = settlement.SettledAt + threadAnswerHoldMillis
	}
	agent, role := answerAgentColumns(settlement.Agent)
	set := []any{settlement.State, nilIfEmptyBytes(settlement.Answer), settlement.AnswerKind, settlement.Cause, settlement.CauseArg,
		settlement.SettledAt, settlement.ExpiresAt, agent, role, nowMillis(), token}
	receipt, err := scanThreadReceipt(q.QueryRow(
		`UPDATE thread_request_receipts
		    SET state = ?, answer = ?, answer_kind = ?, answer_cause = ?, answer_cause_arg = ?, revision = revision + 1,
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
	// Unrested is whether the settlement's RestGuard found its thread not
	// at rest inside the write's transaction; nothing was written.
	Unrested bool
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
	// AnsweredBySpawned is whether the thread that answered was spawned by
	// the thread that asked, so the answer may mark it read
	// (docs/specs/standing-agents.md ruling 30): on this computer as
	// Thread.SpawnedByThreadID reads it, set only with Collected; or, for a
	// request a paired computer made, through the link that names the same
	// device and source thread as the request's receipt.
	AnsweredBySpawned bool
}

// answeredByLinkedTx reports whether a paired computer's receipt comes from
// the thread whose link owns its target: the link (spawn_link, at most one
// per thread) names the receipt's device and source thread.
func answeredByLinkedTx(tx *sql.Tx, receipt ThreadRequestReceipt) (bool, error) {
	if receipt.TargetThreadID == "" || receipt.OwnerDeviceID == localReceiptOwner {
		return false, nil
	}
	var linked bool
	err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_request_receipts
		 WHERE target_thread_id = ? AND spawn_link = 1 AND owner_device_id = ? AND source_thread_id = ?)`,
		receipt.TargetThreadID, receipt.OwnerDeviceID, receipt.SourceThreadID).Scan(&linked)
	if err != nil {
		return false, fmt.Errorf("read whether %s answered its owner's link: %w", receipt.TargetThreadID, err)
	}
	return linked, nil
}

// SettleLocalThreadRequest settles a receipt and, when the request was made
// on this computer, hands the answer to its source row and acknowledges it,
// in one transaction: the receipt is written provided it is still in one
// of from, the source row takes the receipt's answer in sourceState
// provided it is still open, and the receipt records the revision the
// source stored. The receipt keeps its own copy of the answer. The
// turn-end observer passes {running} so a thread_reply that landed first
// wins; the cancel path passes both open states. settlement.ArmWake and
// settlement.DisarmWake are the source row's half and are written only to
// a source row on this computer: a sender's own cancel disarms its wake in
// the statement that settles it, so a cancel that fails leaves it armed.
func (s *Store) SettleLocalThreadRequest(token string, from []string, settlement ThreadRequestSettlement, sourceState string) (LocalSettlement, error) {
	return s.SettleLocalThreadRequestAtRest(token, from, settlement, sourceState, nil)
}

// SettleLocalThreadRequestAtRest is SettleLocalThreadRequest for a
// settlement that holds only while its thread is at rest: rest, a non-nil
// guard over the receipt's thread, is read inside the settlement's
// transaction first, and a thread not at rest there settles nothing
// (LocalSettlement.Unrested).
func (s *Store) SettleLocalThreadRequestAtRest(token string, from []string, settlement ThreadRequestSettlement, sourceState string, rest *RestGuard) (_ LocalSettlement, err error) {
	action := fmt.Sprintf("store: settle local thread request %s", token)
	if _, ok := threadRequestStates[sourceState]; !ok {
		return LocalSettlement{}, fmt.Errorf("%s: invalid source state %q", action, sourceState)
	}
	if settlement.ArmWake && settlement.DisarmWake {
		return LocalSettlement{}, fmt.Errorf("%s: a settlement cannot both arm and disarm the wake", action)
	}
	openClause, openArgs, err := stateInClause(action, threadRequestOpenStates, threadRequestStates)
	if err != nil {
		return LocalSettlement{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return LocalSettlement{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if rest != nil {
		rested, err := rest.atRestTx(tx, s.run)
		if err != nil {
			return LocalSettlement{}, fmt.Errorf("%s: %w", action, err)
		}
		if len(rested) == 0 {
			return LocalSettlement{Unrested: true}, nil
		}
	}
	// The wake is the source row's; the receipt has none.
	armWake, disarmWake := settlement.ArmWake, settlement.DisarmWake
	settlement.ArmWake, settlement.DisarmWake = false, false
	receipt, settled, err := settleThreadReceiptRow(tx, token, from, settlement)
	if err != nil || !settled {
		return LocalSettlement{}, err
	}
	out := LocalSettlement{Settled: true, Receipt: receipt}
	var targetComputer string
	var notify, answeredBySpawned int
	// The spawner is read as threadColumns reads it (spawnerOfSQL).
	err = tx.QueryRow(`SELECT target_computer_id, notify,
	        COALESCE(`+spawnerOfSQL("?")+`, '') = thread_requests.caller_thread_id
	   FROM thread_requests WHERE token = ? AND lineage_only = 0`, receipt.TargetThreadID, token).
		Scan(&targetComputer, &notify, &answeredBySpawned)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A paired computer's request, which its poller collects. It was
		// made by the thread that owns the target when it comes from that
		// thread's link: the same device and source thread.
		if out.AnsweredBySpawned, err = answeredByLinkedTx(tx, receipt); err != nil {
			return LocalSettlement{}, fmt.Errorf("%s: %w", action, err)
		}
	case err != nil:
		return LocalSettlement{}, fmt.Errorf("%s: read the source row: %w", action, err)
	case targetComputer != "":
		out.Outbound = true
	default:
		agent, role := answerAgentColumns(receipt.AnswerAgent)
		now := nowMillis()
		// RETURNING reads the wake as the settlement left it: a reply arms
		// it in this statement (threadRequestArmSQL), and a sender's cancel
		// disarms it.
		err := tx.QueryRow(
			`UPDATE thread_requests
			    SET state = ?, answer = ?, answer_kind = ?, answer_cause = ?, answer_cause_arg = ?, revision = revision + 1,
			        settled_at = ?, expires_at = ?, answer_agent = ?, answer_agent_role = ?,
			        notify = CASE WHEN ? = 1 THEN 0 ELSE `+threadRequestArmSQL+` END, updated_at = ?
			  WHERE token = ? AND `+openClause+`
			  RETURNING notify`,
			append([]any{sourceState, nilIfEmptyBytes(receipt.Answer), receipt.AnswerKind, receipt.Cause, receipt.CauseArg, receipt.SettledAt,
				nilIfZero(receipt.ExpiresAt), agent, role, boolToInt(disarmWake), boolToInt(armWake), now, token},
				openArgs...)...).Scan(&notify)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return LocalSettlement{}, fmt.Errorf("%s: settle the source row: %w", action, err)
		default:
			out.Collected = true
		}
		if out.Collected {
			// The source has the answer durably; the receipt may drop its copy.
			if _, err := ackThreadReceipt(tx, token, receipt.Revision, now); err != nil {
				return LocalSettlement{}, err
			}
			out.Receipt.CollectedRevision, out.Receipt.CollectedAt = receipt.Revision, now
			out.Notify = notify != 0
			out.AnsweredBySpawned = answeredBySpawned != 0 && receipt.TargetThreadID != ""
		}
	}
	var runs []endedRun
	if rest != nil && receipt.TargetThreadID != "" {
		// The thread is at rest: every run on it ends, this request's
		// among them (request_runs.go).
		if runs, err = endThreadRunsTx(tx, receipt.TargetThreadID); err != nil {
			return LocalSettlement{}, fmt.Errorf("%s: end the runs on %s: %w", action, receipt.TargetThreadID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return LocalSettlement{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	if out.Collected || receipt.OwnerDeviceID != localReceiptOwner {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	s.noteRunsEnded(runs)
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
// standing agent, kept beside the first answer's (LateReplyAgent). A receipt
// whose request was made on this computer stores it with that request
// instead (StoreLocalThreadRequestLateReply).
func (s *Store) StoreThreadReceiptLateReply(token string, reply []byte, at int64, agent *ThreadAnswerAgent) (bool, error) {
	changed, err := storeThreadReceiptLateReply(s.db, token, reply, at, agent)
	if changed {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return changed, err
}

func storeThreadReceiptLateReply(db sqlExecutor, token string, reply []byte, at int64, agent *ThreadAnswerAgent) (bool, error) {
	action := fmt.Sprintf("store: store thread receipt late reply %s", token)
	if at == 0 {
		at = nowMillis()
	}
	isAgent, role := answerAgentColumns(agent)
	result, err := db.Exec(
		`UPDATE thread_request_receipts
		    SET late_reply = ?, late_reply_at = ?, late_reply_agent = ?, late_reply_agent_role = ?, revision = revision + 1,
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
// the acknowledgement is what makes expiry safe. A revision past the
// receipt's own is recorded as the receipt's: nobody can have stored what the
// receipt does not hold yet.
func (s *Store) AckThreadRequestReceipt(token string, revision, at int64) (bool, error) {
	if at == 0 {
		at = nowMillis()
	}
	return ackThreadReceipt(s.db, token, revision, at)
}

func ackThreadReceipt(db sqlExecutor, token string, revision, at int64) (bool, error) {
	action := fmt.Sprintf("store: acknowledge thread receipt %s", token)
	result, err := db.Exec(
		`UPDATE thread_request_receipts
		    SET collected_revision = MIN(?, revision), collected_at = ?, updated_at = ?
		  WHERE token = ? AND collected_revision < MIN(?, revision) AND lineage_only = 0`,
		revision, at, nowMillis(), token, revision,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}
