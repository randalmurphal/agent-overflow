package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/threadmode"
)

// HistoryStamp is a thread's history invalidation contract
// (docs/architecture/thread-replica-sync.md §3): the pair a client compares
// against its cached window to learn whether the window is still what a
// fresh read would return.
//
// Rev advances on EVERY persisted mutation that can change what a
// windowed item read returns (item insert/update/delete, payload
// content/meta/span writes). Equal revs mean byte-identical window reads;
// a differing rev says nothing about WHAT changed.
//
// Epoch advances on the subset a client holding a cached ORDER cannot
// survive by re-fetching a range: item deletion and item repositioning.
// Every epoch bump also bumps rev, so a rev match alone implies fully
// fresh — epoch exists to grade how stale a mismatch is.
//
// UnknownStamp (-1) is the client's "I hold no replica / my stamp is not
// trustworthy" value. It can never compare equal to a real stamp, which
// is what makes an understated client stamp cost one redundant fetch
// instead of showing stale content as fresh (§3.4).
type HistoryStamp struct {
	Rev   int64 `json:"historyRev"`
	Epoch int64 `json:"historyEpoch"`
}

// UnknownStamp is the sentinel for "no replica / stamp unknown".
const UnknownStamp int64 = -1

// UnknownHistoryStamp is the HistoryStamp a caller with no replica sends.
func UnknownHistoryStamp() HistoryStamp {
	return HistoryStamp{Rev: UnknownStamp, Epoch: UnknownStamp}
}

// SyncStatus grades a client window against the store's current stamps.
type SyncStatus string

const (
	// SyncFresh — the caller's stamps match. No page is returned; the
	// caller's cached window is byte-identical to what a read would give.
	SyncFresh SyncStatus = "fresh"
	// SyncStale — epoch matches, rev doesn't: only additive or in-place
	// changes can have happened. A page is returned and is
	// range-authoritative within its cursor bounds.
	SyncStale SyncStatus = "stale"
	// SyncRewritten — epoch differs: rows may have been deleted or moved,
	// so cached scrollback outside the returned page is unusable.
	SyncRewritten SyncStatus = "rewritten"
	// SyncGone — no thread row. The caller drops its replica entry.
	SyncGone SyncStatus = "gone"
)

// ThreadWindowSync is one SyncThreadWindow answer. Page is nil for
// SyncFresh (nothing changed) and SyncGone (nothing to send).
type ThreadWindowSync struct {
	Status SyncStatus
	Stamp  HistoryStamp
	// Generation is the store's replica generation at read time. A
	// mismatch against the client's tells it to drop the whole replica
	// for this backend rather than reason about the counters at all.
	Generation string
	Page       *PagedItems
	Scope      *TimelineScopeContext
}

// bumpHistoryRevTx advances one thread's history_rev inside a
// caller-owned transaction. It exists for every write that changes what a
// windowed read returns WITHOUT touching an `items` row, so the item
// triggers cannot see it: the payload span backfill, the whole-thread
// transfer import, and the fallback inside touchItemRowsTx for a row that
// is still imported history.
//
// A write that knows WHICH rows it changes uses bumpHistoryRevForItemTx or
// bumpHistoryRevForPayloadTx instead. Those advance this same counter
// through the trigger and additionally stamp the rows, which is what the
// per-row window digest reads (§5). Bumping the thread alone would leave a
// digest verifying a window whose content had changed.
//
// Each of those mutators names its thread in its own signature, and that
// IS the enforcement: there is no way to reach the write without saying
// whose history it belongs to.
//
// A bump that matches no thread row is an error, not a no-op: it means
// either a caller naming the wrong thread (a bug that would otherwise
// silently under-report history changes forever) or a thread deleted
// underneath the write, which is the same benign-drop shape the payload
// mutators already report as wrapped sql.ErrNoRows.
func bumpHistoryRevTx(exec sqlExecutor, threadID, label string) error {
	if threadID == "" {
		return fmt.Errorf("%s: thread id is required to advance history_rev", label)
	}
	result, err := exec.Exec(
		`UPDATE threads SET history_rev = history_rev + 1 WHERE id = ?`,
		threadID,
	)
	if err != nil {
		return fmt.Errorf("%s: bump history rev: %w", label, err)
	}
	return requireRowsAffected(result, label+": bump history rev")
}

// bumpHistoryRevForItemTx is bumpHistoryRevTx for a write that changes what
// a read of ONE KNOWN item row returns without touching the row: the
// proposed-plan state and comment mutators, whose rows decorateProposedPlanItems
// projects onto `Item.Meta` at read time.
//
// It touches the row instead of bumping the thread directly, so the item
// UPDATE trigger does the whole job: thread stamp AND the row's own `rev`.
// A window whose plan row gained an Accepted badge must not verify as
// unchanged, and only a re-stamped row says that. `SET updated_at =
// updated_at` is the touch: a column the trigger fires on, left equal, so
// no read changes but the stamp, and `rev` stays a value no Go statement
// ever chooses.
//
// A thread whose row is still IMPORTED history has nothing local to touch.
// That row cannot carry a per-row stamp at all (importedItemRevExpr), so the
// fallback is the plain thread bump — the same answer as before this column
// existed. Window verification refuses any window containing an imported row,
// so the thread stamp is the only signal such a client can use, and it moves.
func bumpHistoryRevForItemTx(tx *sql.Tx, threadID, itemID, label string) error {
	if itemID == "" {
		return fmt.Errorf("%s: item id is required to stamp an item revision", label)
	}
	return touchItemRowsTx(tx, threadID, label, touchItemRowSQL, threadID, itemID)
}

// touchItemRowSQL is bumpHistoryRevForItemTx's touch.
const touchItemRowSQL = `UPDATE items SET updated_at = updated_at WHERE thread_id = ? AND id = ?`

// bumpHistoryRevForPayloadTx is bumpHistoryRevForItemTx for the payload
// mutators: payload content and meta ride the item rows that reference the
// payload, so those rows are the ones whose read result changed. A payload
// can be referenced as a result (`payload_id`) or as a tool-call input
// (`input_payload_id`), and both projections are on the wire.
//
// UpdatePayloadSpans is deliberately NOT a caller. Preview spans are a
// derived highlight cache with a documented "empty means not computed, use
// the highlight RPC" fallback, and the client version-checks them against
// the payload content it already holds (frontend utils/payloadVersion.ts),
// so a window whose spans are behind is still a CORRECT window. It keeps the
// plain thread bump: a span backfill still grades the stamp stale, it just
// does not invalidate a per-row digest.
func bumpHistoryRevForPayloadTx(exec sqlExecutor, threadID, payloadID, label string) error {
	if payloadID == "" {
		return fmt.Errorf("%s: payload id is required to stamp an item revision", label)
	}
	return touchItemRowsTx(
		exec, threadID, label,
		touchPayloadOwnerRowsSQL,
		threadID, payloadID, threadID, payloadID, threadID,
	)
}

// touchPayloadOwnerRowsSQL is bumpHistoryRevForPayloadTx's touch, written
// as the UNION ALL of two single-column probes for the same reason
// GetThreadItemByPayloadID is: each branch states one column against one
// partial index (idx_items_payload_id, idx_items_input_payload_id), while
// a single `payload_id = ? OR input_payload_id = ?` clause puts SQLite on
// the broad idx_items_thread and scans every row of the thread. This runs
// on every streaming payload append, so the scan would be per delta batch.
//
// The bind order is thread id, payload id, thread id, payload id, thread
// id. It is a const so TestPayloadTouchProbesPayloadIndexes pins the
// production statement rather than a copy of it.
const touchPayloadOwnerRowsSQL = `UPDATE items SET updated_at = updated_at
		  WHERE thread_id = ? AND id IN (
		        SELECT id FROM items WHERE payload_id = ? AND thread_id = ?
		         UNION ALL
		        SELECT id FROM items WHERE input_payload_id = ? AND thread_id = ?
		  )`

// touchItemRowsTx runs a row touch and guarantees the thread stamp moved.
// Matching no row is not an error and not a no-op: the owner is imported
// history, and the thread stamp still has to advance or a client holding it
// would be told nothing changed.
func touchItemRowsTx(exec sqlExecutor, threadID, label, touchSQL string, args ...any) error {
	if threadID == "" {
		return fmt.Errorf("%s: thread id is required to advance history_rev", label)
	}
	result, err := exec.Exec(touchSQL, args...)
	if err != nil {
		return fmt.Errorf("%s: stamp item revision: %w", label, err)
	}
	touched, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: count stamped item revisions: %w", label, err)
	}
	if touched > 0 {
		return nil
	}
	return bumpHistoryRevTx(exec, threadID, label)
}

// readHistoryStampTx reads a thread's stamps. found=false means a deleted
// thread, which SyncThreadWindow reports as `gone`: no thread row, or a
// holder (fork_holders.go), which is what a deleted thread its forks still
// read keeps its id as.
//
// A pointer fork's stamps are its own. No ancestor write moves them: a row
// a fork shows never changes (fork_triggers.go), and a revert or delete
// that takes such a row out of its thread gives it to a holder the fork
// reads it from (fork_holders.go).
func readHistoryStampTx(q sqlQueryer, threadID string) (HistoryStamp, bool, error) {
	var stamp HistoryStamp
	err := q.QueryRow(
		`SELECT history_rev, history_epoch FROM threads WHERE id = ? AND mode <> ?`,
		threadID, threadmode.ModeHolder,
	).Scan(&stamp.Rev, &stamp.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return HistoryStamp{}, false, nil
	}
	if err != nil {
		return HistoryStamp{}, false, fmt.Errorf("store: read history stamp for %s: %w", threadID, err)
	}
	return stamp, true, nil
}

// ThreadHistoryStamp reads one thread's current stamps. Used by event
// emitters that attach a stamp to content the client already holds; a
// missing thread reports found=false rather than an error, since a
// deleted thread is an ordinary outcome for a late event.
func (s *Store) ThreadHistoryStamp(threadID string) (HistoryStamp, bool, error) {
	return readHistoryStampTx(s.reader(), threadID)
}

// SyncThreadWindow answers "is my cached window for this thread still
// current, and if not, here is the window" in one read-pool transaction
// (docs/architecture/thread-replica-sync.md §5).
//
// The single transaction is the load-bearing part: under WAL the whole
// call sees one snapshot, so the stamps returned attest EXACTLY the rows
// returned. Reading the stamps and the page separately would admit the
// one answer the contract must never give — newer stamps over older rows,
// which a client would record as fresh and never correct.
//
// It is read-only. On a WAL database it runs on the read pool, so it
// neither takes nor waits on the single writer connection and stays fast
// mid-turn. On the writer-fallback configurations that have no read pool
// (`:memory:`, non-WAL) `reader()` IS the writer, so the transaction
// serializes with flush writes like any other read there.
// `runWindowRows` sizes the page exactly as it sizes ListThreadSliceAround's:
// the two return the same window and must therefore compose it identically.
//
// `held` describes the rows the caller already has, and is nil when it has
// none. It is the second way to earn `fresh`: when the stamps do not match
// but the held rows still ARE the read (verifyHeldWindowTx), the answer is
// page-less and the returned stamp attests the caller's own rows. That is
// what keeps a reopen after a turn on the same thread free, where a stamp
// the turn invalidated cannot.
func (s *Store) SyncThreadWindow(ctx context.Context, threadID, anchorItemID string, itemBudget, runWindowRows int, have HistoryStamp, held *HeldWindow, selection TimelineSelection) (ThreadWindowSync, error) {
	return readSnapshotContext(ctx, s.reader(), "sync thread window", func(q sqlQueryer) (ThreadWindowSync, error) {
		return s.syncThreadWindow(q, threadID, anchorItemID, itemBudget, runWindowRows, have, held, selection)
	})
}

func (s *Store) syncThreadWindow(q sqlQueryer, threadID, anchorItemID string, itemBudget, runWindowRows int, have HistoryStamp, held *HeldWindow, selection TimelineSelection) (ThreadWindowSync, error) {
	stamp, found, err := readHistoryStampTx(q, threadID)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	identity, err := identityFrom(q)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	if !found {
		return ThreadWindowSync{Status: SyncGone, Generation: identity.ReplicaGeneration}, nil
	}

	scope, err := s.resolveTimelineScope(q, threadID, selection)
	if errors.Is(err, ErrTimelineScopeGone) {
		return ThreadWindowSync{Status: SyncGone, Generation: identity.ReplicaGeneration}, nil
	}
	if err != nil {
		return ThreadWindowSync{}, err
	}
	status := SyncRewritten
	switch {
	case have.Epoch == stamp.Epoch && have.Rev == stamp.Rev:
		status = SyncFresh
	case have.Epoch == stamp.Epoch:
		status = SyncStale
	}
	out := ThreadWindowSync{
		Status:     status,
		Stamp:      stamp,
		Generation: identity.ReplicaGeneration,
		Scope:      scope.context,
	}
	if status == SyncFresh {
		return out, nil
	}
	if held != nil {
		verified, err := verifyHeldWindowTx(q, threadID, *held, scope)
		if err != nil {
			return ThreadWindowSync{}, err
		}
		if verified {
			// The rows the caller holds are the rows a read would return,
			// so the current stamp attests them exactly as it would attest
			// a page. The status is the same `fresh`, and the client may
			// adopt the stamp for the window it already painted.
			out.Status = SyncFresh
			return out, nil
		}
	}

	page, err := s.listThreadSliceAround(q, threadID, anchorItemID, itemBudget, runWindowRows, scope)
	if err != nil {
		return ThreadWindowSync{}, err
	}
	out.Page = &page
	return out, nil
}
