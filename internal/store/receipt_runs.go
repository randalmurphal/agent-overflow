package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// A paired computer's request to a thread here keeps its run on its receipt
// (migration v159): this computer alone sees the worker rest, and the
// worker's prompts go to that computer's thread only while a run it started
// is open (SpawnOwnerOf). A spawn, and a send from the thread whose spawn
// made the worker here, start one when their receipt is written
// (receiptRunStartSQL); a receipt written before v159, and every other
// receipt, tracks none (linkRunOpenSQL says how such a link reads). The
// run ends with the runs on the thread here, when the worker is at rest
// after the receipt settled (endThreadRunsTx), and keeps the worker's
// figures over its life then, which the requesting computer takes as the
// run's end (ReceiptRunOf).

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

// linkRunOpenSQL is whether link, a paired computer's spawn receipt here,
// has a run of its source thread's open on its thread: its own, or a later
// send's from the same device and thread. A link that tracks no run, one
// written before v159, reads open until it settles and closed once it has,
// as ownRunOpenSQL reads a spawn of this computer's.
func linkRunOpenSQL(link string) string {
	l := link + "."
	return `((` + l + `run_open IS NULL AND ` + l + `settled_at IS NULL) OR EXISTS (SELECT 1 FROM thread_request_receipts rr INDEXED BY idx_thread_request_receipts_run_open
	          WHERE rr.target_thread_id = ` + l + `target_thread_id AND rr.run_open = 1
	            AND rr.owner_device_id = ` + l + `owner_device_id AND rr.source_thread_id = ` + l + `source_thread_id))`
}

// endReceiptRunsTx ends the open runs of the settled receipts to thread,
// recording its figures over its life at now.
func endReceiptRunsTx(tx *sql.Tx, threadID string, now int64) error {
	tally := tallyFiguresSQL("?1", "?2")
	_, err := tx.Exec(`UPDATE thread_request_receipts INDEXED BY idx_thread_request_receipts_run_open
		    SET run_open = 0, run_tool_calls = `+tally[0]+`, run_tokens = `+tally[1]+`, run_turn_ms = `+tally[2]+`
		  WHERE target_thread_id = ?1 AND run_open = 1 AND settled_at IS NOT NULL`, threadID, now)
	if err != nil {
		return fmt.Errorf("end the runs of the receipts to %s: %w", threadID, err)
	}
	return nil
}

// ThreadRunsEnding reports whether a run is open on threadID that its rest
// would end: a settled request's of this computer, or a paired computer's
// on its settled receipt here.
func (s *Store) ThreadRunsEnding(threadID string) (bool, error) {
	var open bool
	err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_requests ro INDEXED BY idx_thread_requests_run_open
		   WHERE `+runOpenSQL("ro")+` AND ro.target_computer_id = '' AND ro.target_thread_id = ?1 AND ro.settled_at IS NOT NULL)
		 OR EXISTS (SELECT 1 FROM thread_request_receipts rr INDEXED BY idx_thread_request_receipts_run_open
		   WHERE rr.target_thread_id = ?1 AND rr.run_open = 1 AND rr.settled_at IS NOT NULL)`, threadID).Scan(&open)
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
}

// ReceiptRunOf reads the runs the thread sourceThreadID on the computer
// whose device is owner started on threadID here, through their receipts.
func (s *Store) ReceiptRunOf(owner, sourceThreadID, threadID string) (ReceiptRun, error) {
	var open bool
	var calls, tokens, ms sql.NullInt64
	err := s.reader().QueryRow(`SELECT run_open, run_tool_calls, run_tokens, run_turn_ms
		  FROM thread_request_receipts INDEXED BY idx_thread_request_receipts_target
		 WHERE target_thread_id = ?1 AND owner_device_id = ?2 AND source_thread_id = ?3 AND run_open IS NOT NULL
		 ORDER BY run_open DESC, settled_at DESC, created_at DESC LIMIT 1`,
		threadID, owner, sourceThreadID).Scan(&open, &calls, &tokens, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return ReceiptRun{}, nil
	}
	if err != nil {
		return ReceiptRun{}, fmt.Errorf("store: read the runs of %s on %s: %w", sourceThreadID, threadID, err)
	}
	run := ReceiptRun{Tracked: true, Open: open}
	if !open && calls.Valid {
		run.End = &RunFigures{ToolCalls: calls.Int64, Tokens: tokens.Int64, TurnMs: ms.Int64}
	}
	return run, nil
}
