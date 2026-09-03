package store

import (
	"encoding/json"
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
	if _, err := s.db.Exec(
		`INSERT INTO flush_queue_items (id, thread_id, send_id, message, payload, enqueued_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
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

// DeleteFlushQueueItemsForThread drops every queued message of one thread.
// Its callers are the wholesale DROPS — a session teardown whose triage
// cleanup discards the in-memory queue, and the Codex rollback purge — where
// leaving rows behind would resurrect at the next boot exactly the messages
// the user's Stop or revert threw away.
func (s *Store) DeleteFlushQueueItemsForThread(threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM flush_queue_items WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: delete flush queue items for thread %s: %w", threadID, err)
	}
	return nil
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
		var item FlushQueueItem
		var payload []byte
		if err := rows.Scan(
			&item.ID, &item.ThreadID, &item.SendID, &item.Message, &payload, &item.EnqueuedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan flush queue item for thread %s: %w", threadID, err)
		}
		if len(payload) > 0 {
			item.Payload = json.RawMessage(payload)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list flush queue items for thread %s: %w", threadID, err)
	}
	return items, nil
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
