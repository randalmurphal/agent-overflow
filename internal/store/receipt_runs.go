package store

import (
	"database/sql"
	"errors"
	"fmt"
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

// endReceiptRunsTx ends the open runs of the settled receipts to thread,
// and the open nudge run of its link, recording its figures over its life
// at now.
func endReceiptRunsTx(tx *sql.Tx, threadID string, now int64) error {
	tally := tallyFiguresSQL("?1", "?2")
	_, err := tx.Exec(`UPDATE thread_request_receipts INDEXED BY idx_thread_request_receipts_run_open
		    SET run_open = 0, run_tool_calls = `+tally[0]+`, run_tokens = `+tally[1]+`, run_turn_ms = `+tally[2]+`
		  WHERE target_thread_id = ?1 AND run_open = 1 AND settled_at IS NOT NULL`, threadID, now)
	if err != nil {
		return fmt.Errorf("end the runs of the receipts to %s: %w", threadID, err)
	}
	return endReceiptNudgesTx(tx, `target_thread_id = ?1`, threadID, now)
}

// A paired computer's spawn link keeps the run a nudge of its worker here
// starts (migration v161): this computer alone sees the worker rest. It is
// the rule of StartNudgeRun for a link here: a message the person or a
// thread other than the spawner there sends the owned worker while no run of
// the spawner's is open on it, on its receipts here. The link keeps its
// latest nudge: its token, its start and the worker's figures over its life
// then, and once it ended, at the worker's rest (endReceiptRunsTx), its
// sidebar row (endNudgeRunsTx) or the link's end (releaseSpawnLinksTx),
// the worker's figures over its life then. A request of the spawner's during
// it starts a run of its own on its receipt, as any does, which the
// spawner's computer joins to the nudge's row. The spawner's computer reads
// it with the link's runs (ReceiptRunOf) and records the nudge's row there
// (RecordRemoteNudgeRun).

// peerNudgeLinkSQL is the FROM and WHERE of the paired computer's spawn
// link a message to thread from by is a nudge through, as `link`
// (idx_thread_request_receipts_spawn_link): the link stands, the worker is
// owned, by is not the link's source thread, no nudge of it is open, and no
// run of the source thread's is open on its receipts to the worker.
func peerNudgeLinkSQL(thread, by string) string {
	return `FROM thread_request_receipts AS link INDEXED BY idx_thread_request_receipts_spawn_link
		   CROSS JOIN threads AS w ON w.id = link.target_thread_id
		  WHERE link.spawn_link = 1 AND link.target_thread_id IS NOT NULL AND link.target_thread_id = ` + thread + `
		    AND w.placement = 'owned' AND link.source_thread_id <> ` + by + ` AND link.nudge_open IS NOT 1
		    AND NOT EXISTS (SELECT 1 FROM thread_request_receipts ro INDEXED BY idx_thread_request_receipts_run_open
		                     WHERE ro.target_thread_id = link.target_thread_id AND ro.run_open = 1
		                       AND ro.owner_device_id = link.owner_device_id AND ro.source_thread_id = link.source_thread_id)`
}

// startPeerNudgeRun records on the link of threadID the run a nudge by
// byThreadID starts, as token, when peerNudgeLinkSQL holds, its base the
// worker's figures over its life now. It reports whether it recorded one.
func (s *Store) startPeerNudgeRun(token, threadID, byThreadID string) (bool, error) {
	tally := tallyFiguresSQL("?2", "?3")
	var link string
	err := s.db.QueryRow(`UPDATE thread_request_receipts
		    SET nudge_token = ?1, nudge_open = 1, nudge_started_at = ?3,
		        nudge_base_tool_calls = `+tally[0]+`, nudge_base_tokens = `+tally[1]+`, nudge_base_turn_ms = `+tally[2]+`,
		        nudge_tool_calls = NULL, nudge_tokens = NULL, nudge_turn_ms = NULL, updated_at = ?3
		  WHERE token = (SELECT link.token `+peerNudgeLinkSQL("?2", "?4")+`)
		 RETURNING token`, token, threadID, nowMillis(), byThreadID).Scan(&link)
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

// dropPeerNudgeRun clears the nudge run token from its link, for a nudge
// that started no work: the link then keeps no nudge.
func (s *Store) dropPeerNudgeRun(token string) error {
	var link string
	err := s.db.QueryRow(`UPDATE thread_request_receipts
		    SET nudge_token = NULL, nudge_open = NULL, nudge_started_at = NULL,
		        nudge_base_tool_calls = NULL, nudge_base_tokens = NULL, nudge_base_turn_ms = NULL
		  WHERE spawn_link = 1 AND nudge_token = ?1 AND nudge_open = 1
		 RETURNING token`, token).Scan(&link)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: drop the nudge run %s from its link: %w", token, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: link})
	return nil
}

// endReceiptNudgesTx ends the open nudge runs of the links where matches,
// with ?1 the thread they reach and ?2 now, recording the thread's figures
// over its life then.
func endReceiptNudgesTx(tx *sql.Tx, where string, threadID string, now int64) error {
	tally := tallyFiguresSQL("?1", "?2")
	_, err := tx.Exec(`UPDATE thread_request_receipts INDEXED BY idx_thread_request_receipts_nudge_open
		    SET nudge_open = 0, nudge_tool_calls = `+tally[0]+`, nudge_tokens = `+tally[1]+`, nudge_turn_ms = `+tally[2]+`
		  WHERE nudge_open = 1 AND `+where, threadID, now)
	if err != nil {
		return fmt.Errorf("end the nudge runs on %s: %w", threadID, err)
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
		 OR EXISTS (SELECT 1 FROM thread_request_receipts rn INDEXED BY idx_thread_request_receipts_nudge_open
		   WHERE rn.target_thread_id = ?1 AND rn.nudge_open = 1)`, threadID).Scan(&open)
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
	// Nudge is the latest nudge run of the spawn's link, nil for none. It
	// is apart from the runs above: Open and End are those of the source
	// thread's requests alone.
	Nudge *NudgeRun
}

// NudgeRun is a nudge's run on a paired computer's worker, as the worker's
// computer recorded it on the spawn's link.
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
// and the latest nudge run of its spawn's link.
func (s *Store) ReceiptRunOf(owner, sourceThreadID, threadID string) (ReceiptRun, error) {
	var run ReceiptRun
	var open bool
	var calls, tokens, ms sql.NullInt64
	err := s.reader().QueryRow(`SELECT run_open, run_tool_calls, run_tokens, run_turn_ms
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
	var nudge NudgeRun
	var nudgeOpen bool
	err = s.reader().QueryRow(`SELECT nudge_token, nudge_open, nudge_started_at,
		    nudge_base_tool_calls, nudge_base_tokens, nudge_base_turn_ms, nudge_tool_calls, nudge_tokens, nudge_turn_ms
		  FROM thread_request_receipts INDEXED BY idx_thread_request_receipts_target
		 WHERE target_thread_id = ?1 AND owner_device_id = ?2 AND source_thread_id = ?3 AND kind = 'spawn'
		   AND nudge_token IS NOT NULL
		 ORDER BY created_at DESC LIMIT 1`,
		threadID, owner, sourceThreadID).Scan(&nudge.Token, &nudgeOpen, &nudge.StartedAt,
		&nudge.Base.ToolCalls, &nudge.Base.Tokens, &nudge.Base.TurnMs, &calls, &tokens, &ms)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return ReceiptRun{}, fmt.Errorf("store: read the nudge run of %s on %s: %w", sourceThreadID, threadID, err)
	default:
		if !nudgeOpen {
			nudge.End = &RunFigures{ToolCalls: calls.Int64, Tokens: tokens.Int64, TurnMs: ms.Int64}
		}
		run.Nudge = &nudge
	}
	return run, nil
}
