package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// FlushQueueItem is one durable row of a thread's flush queue (migration
// v85): a message the user has already sent, waiting out an active turn
// before the dispatcher writes it to the provider.
//
// `Payload` is the app's opaque flush-queue JSON (attachment ids, plan and
// diff-review provenance, composer-command provenance). The store never
// decodes it — the shape belongs to `internal/flushqueue` — and a row whose
// payload no longer decodes still carries its `Message`, which is the part a
// person typed.
type FlushQueueItem struct {
	ID       string
	ThreadID string
	// SendID is the client-minted per-send idempotency id, or "" for an
	// app-internal injector and for any client too old to mint one.
	SendID     string
	Message    string
	Payload    json.RawMessage
	EnqueuedAt int64
}

// InsertFlushQueueItem persists one queued message. It runs BEFORE the
// in-memory register, so a failure here fails the whole enqueue visibly
// rather than leaving a message whose only copy is process memory.
func (s *Store) InsertFlushQueueItem(item FlushQueueItem) error {
	return insertFlushQueueItem(s.db, item)
}

// EnsureFlushQueueItem persists a queued message that the app is putting
// back on the queue, unless its row is still there. A message taken back from
// a session it had already reached lost its row at that write; the requeue
// gives it one again before the in-memory register, as InsertFlushQueueItem
// does for a new message.
func (s *Store) EnsureFlushQueueItem(item FlushQueueItem) error {
	return writeFlushQueueItem(s.db, item, ` ON CONFLICT(id) DO NOTHING`)
}

func insertFlushQueueItem(db interface {
	Exec(string, ...any) (sql.Result, error)
}, item FlushQueueItem) error {
	return writeFlushQueueItem(db, item, "")
}

func writeFlushQueueItem(db interface {
	Exec(string, ...any) (sql.Result, error)
}, item FlushQueueItem, conflict string) error {
	if strings.TrimSpace(item.ID) == "" {
		return fmt.Errorf("store: insert flush queue item: id is required")
	}
	if strings.TrimSpace(item.ThreadID) == "" {
		return fmt.Errorf("store: insert flush queue item %s: thread id is required", item.ID)
	}
	var payload any
	if len(item.Payload) > 0 {
		payload = []byte(item.Payload)
	}
	if _, err := db.Exec(
		`INSERT INTO flush_queue_items (id, thread_id, send_id, message, payload, enqueued_at)
		 VALUES (?, ?, ?, ?, ?, ?)`+conflict,
		item.ID, item.ThreadID, item.SendID, item.Message, payload, item.EnqueuedAt,
	); err != nil {
		return fmt.Errorf("store: insert flush queue item %s: %w", item.ID, err)
	}
	return nil
}

// DeleteFlushQueueItem drops one queued message by id. A row that is already
// gone is success: the two durable endpoints (a dispatched message's
// persisted row, a session-death restore into the composer) can settle the
// same item, and neither owes the other a check.
func (s *Store) DeleteFlushQueueItem(id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM flush_queue_items WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete flush queue item %s: %w", id, err)
	}
	return nil
}

// DeleteDispatchedFlushQueueItem retires a successful provider write. Async
// answers retain recovery until a stable user-message echo confirms consumption.
func (s *Store) DeleteDispatchedFlushQueueItem(id string) error {
	_, err := s.db.Exec(`DELETE FROM flush_queue_items WHERE id=? AND NOT EXISTS (
 SELECT 1 FROM async_questions q WHERE q.thread_id=flush_queue_items.thread_id
 AND q.send_id<>'' AND q.send_id=flush_queue_items.send_id AND q.state='submitted')`, id)
	return err
}

// RestoreFlushQueueToDraft puts undelivered messages back in the composer.
// In one transaction it writes merged over expected (when they differ),
// deletes the queue rows ids, and takes back each unconfirmed user row in
// retire as RetireUndeliveredUserRow does, returning the review comments
// those rows claimed to drafts. Everything is written or nothing is, so no
// failure or crash leaves a message both in the composer and in a row a
// later restore takes back again, or in neither. A row in retire still
// marked pendingFlush that the residue rule calls history
// (unconfirmedUserRowResidueSQL) refuses the whole restore: its message was
// shown as sent and a later session ran, so neither the row nor its text may
// move. It reports whether the draft changed and the comments it returned.
func (s *Store) RestoreFlushQueueToDraft(expected, merged ThreadDraft, ids, retire []string) (bool, []DiffReviewReturnedComments, error) {
	threadID := merged.ThreadID
	if threadID == "" || expected.ThreadID != threadID {
		return false, nil, fmt.Errorf("queue restoration requires matching draft threads")
	}
	for _, itemID := range retire {
		if strings.TrimSpace(itemID) == "" {
			return false, nil, fmt.Errorf("store: queue restoration: an unconfirmed row id is empty")
		}
	}
	var changed bool
	var returned []DiffReviewReturnedComments
	err := s.writeItems(threadID, nil, "restore queued messages to the draft of "+threadID, func(tx *sql.Tx, w *cardWrite) error {
		changed, returned = false, nil
		current, _, err := getThreadDraft(tx, threadID)
		if err != nil {
			return err
		}
		if current != expected {
			return fmt.Errorf("composer changed during queue restoration; queue retained")
		}
		if merged != expected {
			if changed, err = upsertThreadDraft(tx, merged); err != nil {
				return err
			}
		}
		for _, id := range ids {
			var owner string
			err := tx.QueryRow(`SELECT thread_id FROM flush_queue_items WHERE id=?`, id).Scan(&owner)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if owner != "" && owner != threadID {
				return fmt.Errorf("queue item belongs to another thread")
			}
			if _, err := tx.Exec(`UPDATE async_questions SET state='restored',user_item_id=''
 WHERE state='submitted' AND send_id<>'' AND (thread_id,send_id) IN
 (SELECT thread_id,send_id FROM flush_queue_items WHERE id=?)`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM flush_queue_items WHERE id=?`, id); err != nil {
				return err
			}
		}
		for _, itemID := range retire {
			comments, err := retireUndeliveredUserRowTx(tx, w, threadID, itemID)
			if err != nil {
				return err
			}
			returned = append(returned, comments...)
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return changed, returned, nil
}

// DeleteFlushQueueItemsForThread drops every queued message of one thread.
// Its callers are the wholesale DROPS — a session teardown whose triage
// cleanup discards the in-memory queue, and the Codex rollback purge — where
// leaving rows behind would resurrect at the next boot exactly the messages
// the user's Stop or revert threw away.
func (s *Store) DeleteFlushQueueItemsForThread(threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE async_questions SET state='unanswered',answer='',send_id='',user_item_id=''
      WHERE thread_id=? AND state='submitted' AND send_id<>'' AND send_id IN
      (SELECT send_id FROM flush_queue_items WHERE thread_id=? AND send_id<>'')`, threadID, threadID); err != nil {
		return err
	}
	if err := retractDroppedThreadWakesTx(tx, threadID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM flush_queue_items WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: delete flush queue items for thread %s: %w", threadID, err)
	}
	return tx.Commit()
}

// ListFlushQueueItems returns one thread's queued messages in queue order.
// `rowid` is the tiebreak because two messages queued inside one millisecond
// still have a first one, and the queue is a FIFO.
func (s *Store) ListFlushQueueItems(threadID string) ([]FlushQueueItem, error) {
	rows, err := s.reader().Query(
		`SELECT id, thread_id, send_id, message, payload, enqueued_at
		   FROM flush_queue_items
		  WHERE thread_id = ?
		  ORDER BY enqueued_at ASC, rowid ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list flush queue items for thread %s: %w", threadID, err)
	}
	defer rows.Close()
	items := []FlushQueueItem{}
	for rows.Next() {
		item, err := scanFlushQueueItem(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan flush queue item for thread %s: %w", threadID, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list flush queue items for thread %s: %w", threadID, err)
	}
	return items, nil
}

const findQueuedSendSQL = `SELECT id, thread_id, send_id, message, payload, enqueued_at
 FROM flush_queue_items WHERE thread_id = ? AND send_id <> '' AND send_id = ? LIMIT 1`

// FindFlushQueueItemBySendID reads only the matching accepted message. Queues
// can contain large prompts; a duplicate check must never hydrate all of them.
func (s *Store) FindFlushQueueItemBySendID(threadID, sendID string) (FlushQueueItem, bool, error) {
	if sendID == "" {
		return FlushQueueItem{}, false, nil
	}
	item, err := scanFlushQueueItem(s.reader().QueryRow(findQueuedSendSQL, threadID, sendID))
	if errors.Is(err, sql.ErrNoRows) {
		return FlushQueueItem{}, false, nil
	}
	if err != nil {
		return FlushQueueItem{}, false, fmt.Errorf("store: find queued send for thread %s: %w", threadID, err)
	}
	return item, true, nil
}

func scanFlushQueueItem(row rowScanner) (FlushQueueItem, error) {
	var item FlushQueueItem
	var payload []byte
	err := row.Scan(&item.ID, &item.ThreadID, &item.SendID, &item.Message, &payload, &item.EnqueuedAt)
	if len(payload) > 0 {
		item.Payload = json.RawMessage(payload)
	}
	return item, err
}

// ListThreadsWithFlushQueueItems returns the threads holding at least one
// queued message. The boot sweep asks this first so a machine with no crash
// residue pays one indexed scan and reads no rows.
func (s *Store) ListThreadsWithFlushQueueItems() ([]string, error) {
	rows, err := s.reader().Query(
		`SELECT DISTINCT thread_id FROM flush_queue_items ORDER BY thread_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list threads with flush queue items: %w", err)
	}
	defer rows.Close()
	threadIDs := []string{}
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			return nil, fmt.Errorf("store: scan thread with flush queue items: %w", err)
		}
		threadIDs = append(threadIDs, threadID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list threads with flush queue items: %w", err)
	}
	return threadIDs, nil
}

// unconfirmedUserRowResidueSQL is true when the marked user row u is residue
// of the process that wrote it, not history: the thread wrote it itself and
// nothing of a later turn exists. A quiet row is written in the running turn
// and keeps its mark only while its send is pending, so a process that died
// with the message unconfirmed left the row in the thread's last turn. A later
// turn means a later session ran with the row in its timeline, and a row older
// than its thread was copied in by a fork, a holder or a transfer; both are
// history.
const unconfirmedUserRowResidueSQL = `u.created_at >= (SELECT t.created_at FROM threads t WHERE t.id = u.thread_id)
    AND NOT EXISTS (SELECT 1 FROM turns later WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index)
    AND NOT EXISTS (SELECT 1 FROM items later
                     WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index AND later.parent_id = '')`

// markedUserRowSQL is true when u is a main-timeline user row still marked
// pendingFlush. It starts with idx_items_user_text's predicate.
const markedUserRowSQL = `u.kind = 'user_text' AND u.parent_id = ''
    AND instr(u.meta, '"pendingFlush"') > 0 AND json_valid(u.meta)
    AND json_extract(u.meta, '$.pendingFlush') = 1`

const listUnconfirmedUserRowsSQL = `SELECT u.thread_id, u.id FROM items u
  WHERE ` + markedUserRowSQL + `
    AND ` + unconfirmedUserRowResidueSQL + `
  ORDER BY u.thread_id, u.turn_index, u.item_index`

// markedHistoryRowSQL finds the row (thread, id) when it is marked
// pendingFlush and the residue rule calls it history.
const markedHistoryRowSQL = `SELECT EXISTS (SELECT 1 FROM items u
  WHERE u.thread_id = ? AND u.id = ? AND ` + markedUserRowSQL + `
    AND NOT (` + unconfirmedUserRowResidueSQL + `))`

// UnconfirmedUserRow names a user row the app reserved for a queued message
// and never saw confirmed: its meta still carries `pendingFlush`
// (itemmeta.SetPendingFlush).
type UnconfirmedUserRow struct {
	ThreadID string
	ItemID   string
}

// ListUnconfirmedUserRows returns the main-timeline user rows still marked
// pendingFlush that are residue of a process that died with the message
// unconfirmed (unconfirmedUserRowResidueSQL), by thread in timeline order.
// The boot restore deletes what this returns, so a marked row the rule calls
// history is never listed. The predicate starts with idx_items_user_text's,
// so the scan is of user rows.
func (s *Store) ListUnconfirmedUserRows() ([]UnconfirmedUserRow, error) {
	rows, err := s.reader().Query(listUnconfirmedUserRowsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: list unconfirmed user rows: %w", err)
	}
	defer rows.Close()
	var out []UnconfirmedUserRow
	for rows.Next() {
		var row UnconfirmedUserRow
		if err := rows.Scan(&row.ThreadID, &row.ItemID); err != nil {
			return nil, fmt.Errorf("store: scan unconfirmed user row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list unconfirmed user rows: %w", err)
	}
	return out, nil
}
