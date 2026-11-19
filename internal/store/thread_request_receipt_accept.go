package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// A local request's acceptance and the naming of the thread that answers a
// receipt: the source row and the receipt in one write, the naming write a
// spawn or an ask makes once its thread exists, and the insert and checks
// every acceptance shares.

// spawnInstructionsAtNamingSQL is the spawn_instructions a naming write
// records on a request row or receipt with kind column `kind` that it names
// thread ?1 on: that thread's, for a spawn. Recorded once, here, so the
// spawn's result says what its thread was given whatever becomes of the
// thread later.
func spawnInstructionsAtNamingSQL(kind string) string {
	return `CASE WHEN ` + kind + ` = '` + ThreadRequestSpawn + `'
	    THEN COALESCE((SELECT spawn_instructions FROM threads WHERE id = ?1), '') ELSE '' END`
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
	if receipt.SpawnLink {
		return ThreadRequestReceipt{}, fmt.Errorf("%s: a local spawn is linked by its request row, not its receipt", action)
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
	if request.Kind == ThreadRequestSpawn && request.RunBase == nil {
		// The spawn's run is its thread's first: it starts from nothing.
		request.RunBase = &RunFigures{}
	}
	if err := insertThreadRequestTx(tx, request, request.Kind == ThreadRequestSend); err != nil {
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
// must still have no target and be open (openReceiptSQL), or it is an error
// and nothing is written. A
// source row that is gone, its caller deleted while the thread was being
// created, is not an error: the thread exists and runs, and the receipt
// names it. own makes a spawn's thread owned by its spawner in the same
// transaction, when the source row is there to own it. It returns how the
// naming moved the thread across the sidebar listing: a spawn's thread is
// real from this write (threadRealClause), so one left standalone or
// grouped joins the list here, and an owned one does not.
func (s *Store) NameLocalThreadRequestTarget(token, threadID string, own bool) (ListingChange, error) {
	action := fmt.Sprintf("store: name local thread request %s target", token)
	if threadID == "" {
		return ListingUnchanged, fmt.Errorf("%s: thread id is required", action)
	}
	now := nowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return ListingUnchanged, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	listedBefore, err := threadListed(tx, threadID)
	if err != nil {
		return ListingUnchanged, fmt.Errorf("%s: %w", action, err)
	}
	result, err := tx.Exec(`UPDATE thread_request_receipts SET target_thread_id = ?1, updated_at = ?2,
		    spawn_instructions = `+spawnInstructionsAtNamingSQL("kind")+`
		  WHERE token = ?3 AND target_thread_id IS NULL AND `+openReceiptSQL, threadID, now, token)
	if err != nil {
		return ListingUnchanged, fmt.Errorf("%s: receipt: %w", action, err)
	}
	if named, err := rowsChanged(result, action); err != nil {
		return ListingUnchanged, err
	} else if !named {
		return ListingUnchanged, fmt.Errorf("%s: no receipt is waiting for its thread", action)
	}
	var caller string
	if err := tx.QueryRow(`UPDATE thread_requests
		    SET target_thread_id = ?1, target_thread_title = '', target_provider = '', updated_at = ?2,
		        state = CASE WHEN state = ?3 THEN ?4 ELSE state END,
		        spawn_instructions = `+spawnInstructionsAtNamingSQL("kind")+`
		  WHERE token = ?5 AND target_computer_id = '' AND lineage_only = 0
		  RETURNING caller_thread_id`,
		threadID, now, ThreadRequestUnconfirmed, ThreadRequestAccepted, token).Scan(&caller); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ListingUnchanged, fmt.Errorf("%s: source: %w", action, err)
	}
	if own && caller != "" {
		if err := ownSpawnedThreadTx(tx, threadID); err != nil {
			return ListingUnchanged, fmt.Errorf("%s: %w", action, err)
		}
	}
	listedAfter, err := threadListed(tx, threadID)
	if err != nil {
		return ListingUnchanged, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return ListingUnchanged, fmt.Errorf("%s: commit: %w", action, err)
	}
	if caller != "" {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token, CallerThreadID: caller})
	}
	return listingChange(listedBefore, listedAfter), nil
}

// openReceiptSQL holds for a receipt that can still name its thread: a
// settled one, settled by a retry that found it unnamed, names none, so the
// creation that lost to it discards what it made.
const openReceiptSQL = `state IN ('` + ThreadReceiptAccepted + `', '` + ThreadReceiptRunning + `')`

// insertThreadReceiptSQL writes a receipt. A link is written only while its
// owner's device can reach this computer, in the insert itself: a worker is
// owned only while that device holds an unrevoked session here
// (endSessionsTx), so a revocation that committed after the request was
// authenticated and before this insert leaves a plain receipt, and its
// thread a sidebar row, as the release would. A revocation after the insert
// releases the link it finds (endSessionsTx), named or not.
const insertThreadReceiptSQL = `INSERT INTO thread_request_receipts (
    token, owner_device_id, source_computer_id, source_computer_name,
    source_thread_id, source_thread_title, kind, target_thread_id,
    message_item_id, turn_id, state, created_at, updated_at, spawn_link, owner_seen_at, track_spawn
 ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13,
    ?14 AND EXISTS (SELECT 1 FROM sessions WHERE sessions.device_id = ?2 AND sessions.revoked_at IS NULL), ?15,
    ?16 AND EXISTS (SELECT 1 FROM sessions WHERE sessions.device_id = ?2 AND sessions.revoked_at IS NULL))`

// insertArgs writes a link as seen by its owner when it was made.
func (r ThreadRequestReceipt) insertArgs() []any {
	return []any{r.Token, r.OwnerDeviceID, r.SourceComputerID, r.SourceComputerName,
		r.SourceThreadID, r.SourceThreadTitle, r.Kind, nilIfEmpty(r.TargetThreadID),
		r.MessageItemID, r.TurnID, r.State, r.CreatedAt, r.UpdatedAt, boolToInt(r.SpawnLink), r.CreatedAt, r.TrackSpawn || r.SpawnLink}
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
	// A link names the computer and thread that own its thread, and only a
	// paired computer's spawn makes one.
	if (r.SpawnLink || r.TrackSpawn) && (r.Kind != ThreadRequestSpawn || r.OwnerDeviceID == localReceiptOwner ||
		r.SourceComputerID == "" || r.SourceThreadID == "") {
		return r, fmt.Errorf("%s: a spawn link is a paired computer's spawn naming its computer and thread", action)
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
// still undecided and the receipt open (openReceiptSQL), so a retry that
// raced the first attempt cannot repoint a receipt at a second thread, and a
// receipt settled meanwhile names nothing; false means the caller's thread
// lost and is to be discarded. own makes the thread owned in
// the same transaction when the receipt is a spawn link, as
// NameLocalThreadRequestTarget does for a local spawn; a receipt that is
// not a link owns nothing. It returns how the naming moved the thread
// across the sidebar listing: a paired computer's spawn makes its thread
// real (threadRealClause), and an owned one stays unlisted.
func (s *Store) SetThreadReceiptTarget(token, threadID string, own bool) (bool, ListingChange, error) {
	action := fmt.Sprintf("store: set thread receipt %s target", token)
	if threadID == "" {
		return false, ListingUnchanged, fmt.Errorf("%s: thread id is required", action)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, ListingUnchanged, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	listedBefore, err := threadListed(tx, threadID)
	if err != nil {
		return false, ListingUnchanged, fmt.Errorf("%s: %w", action, err)
	}
	var link bool
	err = tx.QueryRow(
		`UPDATE thread_request_receipts SET target_thread_id = ?1, updated_at = ?2,
		    spawn_instructions = `+spawnInstructionsAtNamingSQL("kind")+`
		  WHERE token = ?3 AND target_thread_id IS NULL AND `+openReceiptSQL+`
		  RETURNING spawn_link`,
		threadID, nowMillis(), token,
	).Scan(&link)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ListingUnchanged, nil
	}
	if err != nil {
		return false, ListingUnchanged, fmt.Errorf("%s: %w", action, err)
	}
	if own && link {
		if err := ownSpawnedThreadTx(tx, threadID); err != nil {
			return false, ListingUnchanged, fmt.Errorf("%s: %w", action, err)
		}
	}
	listedAfter, err := threadListed(tx, threadID)
	if err != nil {
		return false, ListingUnchanged, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, ListingUnchanged, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	return true, listingChange(listedBefore, listedAfter), nil
}
