package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// What a spawner's computer owes the paired computers its threads spawned
// on (migration v157): when an owner is archived, deleted or given to
// another computer, each remote spawn under it owes that computer one
// operation on the worker the spawn made there. thread_owner_ops keeps one
// row per spawn token, written in the transaction that decided it, and it
// has no foreign key: a delete's owed rows outlive the spawn rows that go
// with their caller. Each owed operation takes the spawn row's next
// owner_op_seq, and every delivery write names the sequence it acts on, so
// an answer to an operation a newer one replaced changes nothing.

// Owner operations.
const (
	OwnerOpArchive   = "archive"
	OwnerOpDelete    = "delete"
	OwnerOpRelease   = "release"
	OwnerOpSupersede = "supersede"
)

// OwnerOp is one operation owed for a remote spawn.
type OwnerOp struct {
	// Token is the spawn's token, which the destination's receipt keeps.
	Token string
	// ComputerID is the computer the spawn went to.
	ComputerID string
	// SourceThreadID is the thread the destination's receipt names as its
	// source (ThreadRequest.MadeIn).
	SourceThreadID string
	Op             string
	Seq            int64
	// Sent is whether an attempt was made at this Seq.
	Sent      bool
	OwedAt    int64
	NextAt    int64
	Attempts  int
	LastError string
}

const ownerOpColumns = `token, computer_id, source_thread_id, op, seq, sent, owed_at, next_at, attempts, last_error`

func scanOwnerOp(row rowScanner) (OwnerOp, error) {
	var op OwnerOp
	err := row.Scan(&op.Token, &op.ComputerID, &op.SourceThreadID, &op.Op, &op.Seq, &op.Sent,
		&op.OwedAt, &op.NextAt, &op.Attempts, &op.LastError)
	return op, err
}

// oweRemoteOpsTx owes op, in tx and as of now, for every remote spawn whose
// caller is one of callers: whether or not its target was named, since a
// spawn whose acceptance reply was lost still made a thread there. Each
// takes the spawn row's next sequence and is owed unsent and due at now. A
// delete replaces any operation owed for the spawn; nothing replaces an
// owed delete, and a spawn with one owes nothing more.
func oweRemoteOpsTx(tx *sql.Tx, callers []string, op string, now int64) (owed []OwnerOp, err error) {
	if len(callers) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(callers)
	if err != nil {
		return nil, fmt.Errorf("owe %s: encode callers: %w", op, err)
	}
	rows, err := tx.Query(`UPDATE thread_requests SET owner_op_seq = owner_op_seq + 1
		 WHERE caller_thread_id IN (SELECT value FROM json_each(?1))
		   AND kind = 'spawn' AND target_computer_id <> ''
		   AND (?2 = 'delete' OR NOT EXISTS (SELECT 1 FROM thread_owner_ops AS owed
		         WHERE owed.token = thread_requests.token AND owed.op = 'delete'))
		 RETURNING token, target_computer_id,
		   CASE WHEN made_in_thread_id <> '' THEN made_in_thread_id ELSE caller_thread_id END, owner_op_seq`,
		string(encoded), op)
	if err != nil {
		return nil, fmt.Errorf("owe %s: %w", op, err)
	}
	defer closeAgentWakeRows(rows, "store: owe "+op, &err)
	for rows.Next() {
		row := OwnerOp{Op: op, OwedAt: now, NextAt: now}
		if err := rows.Scan(&row.Token, &row.ComputerID, &row.SourceThreadID, &row.Seq); err != nil {
			return nil, fmt.Errorf("owe %s: scan: %w", op, err)
		}
		owed = append(owed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("owe %s: %w", op, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("owe %s: %w", op, err)
	}
	for _, row := range owed {
		if _, err := tx.Exec(`INSERT INTO thread_owner_ops (`+ownerOpColumns+`)
			VALUES (?1, ?2, ?3, ?4, ?5, 0, ?6, ?6, 0, '')
			ON CONFLICT(token) DO UPDATE SET computer_id = excluded.computer_id,
			    source_thread_id = excluded.source_thread_id, op = excluded.op, seq = excluded.seq,
			    sent = 0, owed_at = excluded.owed_at, next_at = excluded.next_at, attempts = 0, last_error = ''`,
			row.Token, row.ComputerID, row.SourceThreadID, row.Op, row.Seq, now); err != nil {
			return nil, fmt.Errorf("owe %s for spawn %s: %w", op, row.Token, err)
		}
	}
	return owed, nil
}

// OweRemoteArchives owes an archive for the remote spawns of ownerID and of
// the archived threads it owns at any depth through owned threads already
// archived (ListOwnedWorkersToArchive's walk), at the end of ownerID's
// archive walk. It owes nothing once ownerID is no longer archived. One
// transaction.
func (s *Store) OweRemoteArchives(ownerID string) (owed []OwnerOp, err error) {
	action := fmt.Sprintf("store: owe the remote archives of %s", ownerID)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var archived bool
	switch err := tx.QueryRow(`SELECT archived FROM threads WHERE id = ?`, ownerID).Scan(&archived); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if !archived {
		return nil, nil
	}
	callers, err := queryIDs(tx, archivedOwnedCallersSQL, ownerID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if owed, err = oweRemoteOpsTx(tx, callers, OwnerOpArchive, nowMillis()); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", action, err)
	}
	return owed, nil
}

// archivedOwnedCallersSQL is ?1 and the archived threads it owns at any
// depth through owned threads already archived.
var archivedOwnedCallersSQL = `WITH RECURSIVE under(id) AS (
		  SELECT ?1
		  UNION
		  SELECT threads.id FROM under CROSS JOIN ` + ownedStepSQL("under.id", "owned_threads") + ` AND threads.archived = 1
		)
		SELECT id FROM under`

// HasRemoteSpawns reports whether any of threadIDs spawned a thread on a
// paired computer, its spawn's acceptance answered or not: what a delete of
// them owes there (CommitDeleteClaim), for its confirmation to say. One
// probe of idx_thread_requests_caller per thread.
func (s *Store) HasRemoteSpawns(threadIDs []string) (bool, error) {
	ids := uniqueNonEmptyStrings(threadIDs)
	if len(ids) == 0 {
		return false, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return false, fmt.Errorf("store: encode the threads to probe for remote spawns: %w", err)
	}
	var found bool
	if err := s.reader().QueryRow(hasRemoteSpawnsSQL, string(encoded)).Scan(&found); err != nil {
		return false, fmt.Errorf("store: probe for remote spawns: %w", err)
	}
	return found, nil
}

// hasRemoteSpawnsSQL is true when a thread of the JSON array ?1 made a
// spawn on another computer.
const hasRemoteSpawnsSQL = `SELECT EXISTS (SELECT 1 FROM json_each(?1) AS caller
	  CROSS JOIN thread_requests ON thread_requests.caller_thread_id = caller.value
	 WHERE thread_requests.kind = 'spawn' AND thread_requests.target_computer_id <> '')`

// OwesWorkerEnd reports whether an archive or delete is owed to the worker
// the spawn token made on a paired computer (owed): its spawner's archive
// or delete is on its way there, and ends that worker's prompts with it.
// archivedCaller is the spawn's caller while it is archived, whose archive
// walk, while it runs, owes that archive at its end (OweRemoteArchives);
// empty otherwise. Two primary key probes.
func (s *Store) OwesWorkerEnd(token string) (owed bool, archivedCaller string, err error) {
	if err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_owner_ops
		 WHERE token = ?1 AND op IN ('archive', 'delete')),
		 COALESCE((SELECT threads.id FROM thread_requests JOIN threads ON threads.id = thread_requests.caller_thread_id
		 WHERE thread_requests.token = ?1 AND thread_requests.kind = 'spawn' AND thread_requests.target_computer_id <> ''
		   AND threads.archived = 1), '')`, token).Scan(&owed, &archivedCaller); err != nil {
		return false, "", fmt.Errorf("store: probe the operation owed for spawn %s: %w", token, err)
	}
	return owed, archivedCaller, nil
}

// dueOwnerOpsSQL reads up to limit owed operations due by ?, the earliest
// first.
func dueOwnerOpsSQL(limit int) string {
	return `SELECT ` + ownerOpColumns + ` FROM thread_owner_ops WHERE next_at <= ? ORDER BY next_at LIMIT ` + strconv.Itoa(limit)
}

// DueOwnerOps returns up to limit operations owed by now, the earliest
// first.
func (s *Store) DueOwnerOps(now int64, limit int) (ops []OwnerOp, err error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: read due owner operations: a page of %d", limit)
	}
	rows, err := s.reader().Query(dueOwnerOpsSQL(limit), now)
	if err != nil {
		return nil, fmt.Errorf("store: read due owner operations: %w", err)
	}
	defer closeAgentWakeRows(rows, "store: read due owner operations", &err)
	for rows.Next() {
		op, err := scanOwnerOp(rows)
		if err != nil {
			return nil, fmt.Errorf("store: read due owner operations: %w", err)
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read due owner operations: %w", err)
	}
	return ops, nil
}

// ownerOpWrite runs a delivery write, whose statement names the token and
// sequence it acts on, and reports whether that operation was still the
// one owed.
func (s *Store) ownerOpWrite(action, statement string, args ...any) (bool, error) {
	result, err := s.db.Exec(statement, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// MarkOwnerOpSent records an attempt at the operation owed for token at
// seq, before its call.
func (s *Store) MarkOwnerOpSent(token string, seq int64) (bool, error) {
	return s.ownerOpWrite("store: mark owner operation "+token+" sent",
		`UPDATE thread_owner_ops SET sent = 1, attempts = attempts + 1 WHERE token = ? AND seq = ?`, token, seq)
}

// RescheduleOwnerOp sets when the operation owed for token at seq is tried
// again, and why the last attempt did not settle it.
func (s *Store) RescheduleOwnerOp(token string, seq, next int64, lastError string) (bool, error) {
	return s.ownerOpWrite("store: reschedule owner operation "+token,
		`UPDATE thread_owner_ops SET next_at = ?, last_error = ? WHERE token = ? AND seq = ?`, next, lastError, token, seq)
}

// SettleOwnerOp drops the operation owed for token at seq once its computer
// answered it.
func (s *Store) SettleOwnerOp(token string, seq int64) (bool, error) {
	return s.ownerOpWrite("store: settle owner operation "+token,
		`DELETE FROM thread_owner_ops WHERE token = ? AND seq = ?`, token, seq)
}

// forgetOwnerOpsSQL drops what this computer owes computer ?.
const forgetOwnerOpsSQL = `DELETE FROM thread_owner_ops WHERE computer_id = ?`

// remoteSpawnLinkToSQL reads the spawn that named thread ?2 on computer ?1.
var remoteSpawnLinkToSQL = `SELECT ` + threadRequestColumns + ` FROM thread_requests
	 WHERE kind = 'spawn' AND target_computer_id = ` + textParam("?1") + ` AND target_computer_id <> '' AND target_thread_id = ?2
	 ORDER BY created_at DESC LIMIT 1`

// RemoteSpawnLinkTo returns the spawn that named threadID on computerID,
// its lineage included: the thread's link from here. Found is false for a
// thread no spawn from here made.
func (s *Store) RemoteSpawnLinkTo(computerID, threadID string) (ThreadRequest, bool, error) {
	if computerID == "" || threadID == "" {
		return ThreadRequest{}, false, nil
	}
	row, err := scanThreadRequest(s.reader().QueryRow(remoteSpawnLinkToSQL, computerID, threadID))
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRequest{}, false, nil
	}
	if err != nil {
		return ThreadRequest{}, false, fmt.Errorf("store: read the spawn of %s on %s: %w", threadID, computerID, err)
	}
	return row, true, nil
}
