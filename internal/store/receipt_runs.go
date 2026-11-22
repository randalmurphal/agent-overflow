package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// A paired computer's request to a thread here keeps its run on its receipt
// (migration v159): this computer alone sees the worker rest. A spawn, and
// a send from the thread whose spawn made the worker here, start one when
// their receipt is written (receiptRunStartSQL); a receipt written before
// v159, and every other receipt, tracks none. The run ends with the runs on
// the thread here, when the worker is at rest after the receipt settled
// (endThreadRunsTx), and keeps the worker's figures over its life then,
// which the requesting computer takes as the run's end (ReceiptRunOf).

// receiptRunStartSQL is the run_open a receipt is inserted with
// (insertThreadReceiptSQL's parameters): 1 for a paired computer's request
// that waits for its worker's rest, a spawn or a send from the thread that
// spawned its target here (ThreadSpawnedForSource); NULL, no run, for any
// other, and for this computer's own requests, which keep theirs on the
// request row.
const receiptRunStartSQL = `CASE
    WHEN ?2 = '` + localReceiptOwner + `' THEN NULL
    WHEN ?7 = 'spawn' THEN 1
    WHEN ?7 = 'send' AND EXISTS (SELECT 1 FROM thread_request_receipts sl INDEXED BY idx_thread_request_receipts_target
                                  WHERE sl.target_thread_id = ?8 AND sl.kind = 'spawn'
                                    AND sl.owner_device_id = ?2 AND sl.source_thread_id = ?5) THEN 1
  END`

// spawnRunOpenSQL matches a paired computer's spawn receipt alias while a
// run its source thread started is open on its thread here: a receipt of
// that thread's, the spawn's or a later send's, whose run is open, or, for
// a link written before v159 that tracks none, the spawn until it settles.
// A release ends those runs (releaseSpawnLinksTx).
func spawnRunOpenSQL(alias string) string {
	a := alias + "."
	return `((` + a + `run_open IS NULL AND ` + a + `spawn_link = 1 AND ` + a + `settled_at IS NULL)
	    OR EXISTS (SELECT 1 FROM thread_request_receipts ro INDEXED BY idx_thread_request_receipts_run_open
	                WHERE ro.target_thread_id = ` + a + `target_thread_id AND ro.run_open = 1
	                  AND ro.owner_device_id = ` + a + `owner_device_id AND ro.source_thread_id = ` + a + `source_thread_id))`
}

// endReceiptRunsTx ends the open runs of the settled receipts to thread,
// and the open nudge runs on it, recording its figures over its life at
// now; thread is at rest (tallyFiguresAtRestSQL).
func endReceiptRunsTx(tx *sql.Tx, threadID string, now int64) error {
	tally := tallyFiguresAtRestSQL("?1", "?2")
	_, err := tx.Exec(`UPDATE thread_request_receipts INDEXED BY idx_thread_request_receipts_run_open
		    SET run_open = 0, run_tool_calls = `+tally[0]+`, run_tokens = `+tally[1]+`, run_turn_ms = `+tally[2]+`
		  WHERE target_thread_id = ?1 AND run_open = 1 AND settled_at IS NOT NULL`, threadID, now)
	if err != nil {
		return fmt.Errorf("end the runs of the receipts to %s: %w", threadID, err)
	}
	return endReceiptNudgesTx(tx, tally, "", threadID, now)
}

// A paired computer's spawn link keeps the runs nudges of its worker here
// start (migration v161), one row each in thread_receipt_nudges: this
// computer alone sees the worker rest. It is the rule of StartNudgeRun for a
// link here: a message the person or a thread other than the spawner there
// sends the owned worker while no run of the spawner's is open on it, on its
// receipts or its link. A run keeps its start and the worker's figures over
// its life then, and once it ended, at the worker's rest (endReceiptRunsTx),
// its sidebar row (endNudgeRunsTx) or the link's end (releaseSpawnLinksTx),
// the worker's figures over its life then. A request of the spawner's during
// it starts a run of its own on its receipt, as any does, which the
// spawner's computer joins to the nudge's row. The spawner's computer reads
// the link's runs (ReceiptRunOf), records each as its nudge's row there
// (RecordRemoteNudgeRun), and says which ended ones it took, which are then
// forgotten here (TakeNudgeRuns). An ended run that computer never takes
// goes at the retention floor, or once its device holds no session here
// (endSessionsTx).

// peerNudgeLinkSQL is the FROM and WHERE of the paired computer's spawn
// link a message to thread from by is a nudge through, as `link`
// (idx_thread_request_receipts_spawn_link): the link stands, the worker is
// owned, by is not the link's source thread, no nudge of it is open, and no
// run of the source thread's is open on the worker (spawnRunOpenSQL).
func peerNudgeLinkSQL(thread, by string) string {
	return `FROM thread_request_receipts AS link INDEXED BY idx_thread_request_receipts_spawn_link
		   CROSS JOIN threads AS w ON w.id = link.target_thread_id
		  WHERE link.spawn_link = 1 AND link.target_thread_id IS NOT NULL AND link.target_thread_id = ` + thread + `
		    AND w.placement = 'owned' AND link.source_thread_id <> ` + by + `
		    AND NOT EXISTS (SELECT 1 FROM thread_receipt_nudges n INDEXED BY idx_thread_receipt_nudges_open
		                     WHERE n.target_thread_id = link.target_thread_id AND n.run_open = 1 AND n.link_token = link.token)
		    AND NOT ` + spawnRunOpenSQL("link")
}

// startPeerNudgeRun records on the link of threadID the run a nudge by
// byThreadID starts, as token from start, when peerNudgeLinkSQL holds. It
// reports whether it recorded one.
func (s *Store) startPeerNudgeRun(token, threadID, byThreadID string, start NudgeStart) (bool, error) {
	var link string
	err := s.db.QueryRow(`INSERT INTO thread_receipt_nudges (
		    token, link_token, target_thread_id, started_at, run_open, run_base_tool_calls, run_base_tokens, run_base_turn_ms)
		 SELECT ?1, link.token, ?2, ?3, 1, ?5, ?6, ?7
		 `+peerNudgeLinkSQL("?2", "?4")+`
		 RETURNING link_token`, token, threadID, start.At, byThreadID,
		start.Base.ToolCalls, start.Base.Tokens, start.Base.TurnMs).Scan(&link)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: record on its link the run a nudge of %s starts: %w", threadID, err)
	}
	// The link's owner reads it again (ActivityOwners).
	s.noteThreadRequestChanged(ThreadRequestChange{Token: link})
	return true, nil
}

// endReceiptNudgesTx ends the open nudge runs on threadID (?1) at now (?2)
// where also holds, recording the thread's figures over its life then, as
// tally reads them with those parameters.
func endReceiptNudgesTx(tx *sql.Tx, tally [3]string, also string, threadID string, now int64, args ...any) error {
	_, err := tx.Exec(`UPDATE thread_receipt_nudges INDEXED BY idx_thread_receipt_nudges_open
		    SET run_open = 0, ended_at = ?2, run_tool_calls = `+tally[0]+`, run_tokens = `+tally[1]+`, run_turn_ms = `+tally[2]+`
		  WHERE target_thread_id = ?1 AND run_open = 1`+also, append([]any{threadID, now}, args...)...)
	if err != nil {
		return fmt.Errorf("end the nudge runs on %s: %w", threadID, err)
	}
	return nil
}

// TakeNudgeRuns forgets the ended nudge runs named in tokens of the link to
// threadID that the source thread sourceThreadID on owner's computer holds:
// that computer recorded their ends. An open run, one of another link, and
// a token already forgotten are left as they are, so a repeated take is
// harmless.
func (s *Store) TakeNudgeRuns(owner, sourceThreadID, threadID string, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("store: take the nudge runs on %s: %w", threadID, err)
	}
	if _, err := s.db.Exec(`DELETE FROM thread_receipt_nudges
		  WHERE token IN (SELECT value FROM json_each(?4)) AND run_open = 0
		    AND link_token IN (SELECT token FROM thread_request_receipts INDEXED BY idx_thread_request_receipts_target
		                        WHERE target_thread_id = ?1 AND owner_device_id = ?2 AND source_thread_id = ?3 AND kind = 'spawn')`,
		threadID, owner, sourceThreadID, string(encoded)); err != nil {
		return fmt.Errorf("store: take the nudge runs on %s: %w", threadID, err)
	}
	return nil
}

// ThreadRunsEnding reports whether a run is open on threadID that its rest
// would end: a settled request's of this computer, or a paired computer's
// on its settled receipt here or on its link, a nudge's.
func (s *Store) ThreadRunsEnding(threadID string) (bool, error) {
	var open bool
	err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_requests ro INDEXED BY idx_thread_requests_run_open
		   WHERE `+runOpenSQL("ro")+` AND ro.target_computer_id = '' AND ro.target_thread_id = ?1 AND ro.settled_at IS NOT NULL)
		 OR EXISTS (SELECT 1 FROM thread_request_receipts rr INDEXED BY idx_thread_request_receipts_run_open
		   WHERE rr.target_thread_id = ?1 AND rr.run_open = 1 AND rr.settled_at IS NOT NULL)
		 OR EXISTS (SELECT 1 FROM thread_receipt_nudges rn INDEXED BY idx_thread_receipt_nudges_open
		   WHERE rn.target_thread_id = ?1 AND rn.run_open = 1)`, threadID).Scan(&open)
	if err != nil {
		return false, fmt.Errorf("store: read whether a run is open on %s: %w", threadID, err)
	}
	return open, nil
}

// ReceiptRun is where the runs a paired computer's thread started on a
// thread here stand.
type ReceiptRun struct {
	// Tracked is false when none of its requests here tracks a run.
	Tracked bool
	// Open is whether one of them is open.
	Open bool
	// End is the worker's figures over its life when the latest run ended,
	// nil while one is open or none ended.
	End *RunFigures
	// Nudges is the nudge runs of the spawn's link its source has not
	// taken: the open one first, then the ended ones oldest first, at most
	// nudgeRunsReported. They are apart from the runs above: Open and End
	// are those of the source thread's requests alone.
	Nudges []NudgeRun
}

// nudgeRunsReported bounds the nudge runs one answer reports: a source that
// takes them reads the rest in its next answers.
const nudgeRunsReported = 16

// NudgeRun is a nudge's run on a paired computer's worker, as the worker's
// computer recorded it for the spawn's link.
type NudgeRun struct {
	Token string
	// StartedAt is when it began, on the worker's computer's clock.
	StartedAt int64
	// Base is the worker's figures over its life when it began.
	Base RunFigures
	// End is the worker's figures over its life when it ended, nil while it
	// is open.
	End *RunFigures
}

// ReceiptRunOf reads the runs the thread sourceThreadID on the computer
// whose device is owner started on threadID here, through their receipts,
// and the nudge runs of its spawn's link it has not taken.
func (s *Store) ReceiptRunOf(owner, sourceThreadID, threadID string) (_ ReceiptRun, err error) {
	var run ReceiptRun
	var open bool
	var calls, tokens, ms sql.NullInt64
	err = s.reader().QueryRow(`SELECT run_open, run_tool_calls, run_tokens, run_turn_ms
		  FROM thread_request_receipts INDEXED BY idx_thread_request_receipts_target
		 WHERE target_thread_id = ?1 AND owner_device_id = ?2 AND source_thread_id = ?3 AND run_open IS NOT NULL
		 ORDER BY run_open DESC, settled_at DESC, created_at DESC LIMIT 1`,
		threadID, owner, sourceThreadID).Scan(&open, &calls, &tokens, &ms)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return ReceiptRun{}, fmt.Errorf("store: read the runs of %s on %s: %w", sourceThreadID, threadID, err)
	default:
		run.Tracked, run.Open = true, open
		if !open && calls.Valid {
			run.End = &RunFigures{ToolCalls: calls.Int64, Tokens: tokens.Int64, TurnMs: ms.Int64}
		}
	}
	rows, err := s.reader().Query(`SELECT n.token, n.run_open, n.started_at, n.run_base_tool_calls, n.run_base_tokens, n.run_base_turn_ms,
		    n.run_tool_calls, n.run_tokens, n.run_turn_ms
		  FROM thread_request_receipts AS link INDEXED BY idx_thread_request_receipts_target
		 CROSS JOIN thread_receipt_nudges AS n INDEXED BY idx_thread_receipt_nudges_link ON n.link_token = link.token
		 WHERE link.target_thread_id = ?1 AND link.owner_device_id = ?2 AND link.source_thread_id = ?3 AND link.kind = 'spawn'
		 ORDER BY n.run_open DESC, n.started_at, n.token LIMIT `+strconv.Itoa(nudgeRunsReported),
		threadID, owner, sourceThreadID)
	if err != nil {
		return ReceiptRun{}, fmt.Errorf("store: read the nudge runs of %s on %s: %w", sourceThreadID, threadID, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var nudge NudgeRun
		var nudgeOpen bool
		if err := rows.Scan(&nudge.Token, &nudgeOpen, &nudge.StartedAt, &nudge.Base.ToolCalls, &nudge.Base.Tokens, &nudge.Base.TurnMs,
			&calls, &tokens, &ms); err != nil {
			return ReceiptRun{}, fmt.Errorf("store: read the nudge runs of %s on %s: %w", sourceThreadID, threadID, err)
		}
		if !nudgeOpen {
			nudge.End = &RunFigures{ToolCalls: calls.Int64, Tokens: tokens.Int64, TurnMs: ms.Int64}
		}
		run.Nudges = append(run.Nudges, nudge)
	}
	if err := rows.Err(); err != nil {
		return ReceiptRun{}, fmt.Errorf("store: read the nudge runs of %s on %s: %w", sourceThreadID, threadID, err)
	}
	return run, nil
}
