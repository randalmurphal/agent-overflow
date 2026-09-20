package store

import (
	"database/sql"
	"fmt"

	"agent-overflow/internal/threadmode"
)

// ScratchThread records where an ephemeral fork came from: the thread it was
// forked out of, the mode a Keep promotion returns it to, and the request it
// answers when it serves a `thread_ask`.
//
// The row is the only place that knowledge lives; the thread itself carries
// mode `scratch` and nothing else. Deleting the thread cascades the row.
type ScratchThread struct {
	ThreadID       string `json:"threadId"`
	SourceThreadID string `json:"sourceThreadId"`
	ReturnMode     string `json:"returnMode"`
	CreatedAt      int64  `json:"createdAt"`
	// RequestToken is empty for a `/side-chat` fork, which no request owns.
	RequestToken string `json:"requestToken,omitempty"`
}

const scratchThreadColumns = `thread_id, source_thread_id, return_mode, created_at, COALESCE(request_token, '')`

func scanScratchThread(row rowScanner) (ScratchThread, error) {
	var s ScratchThread
	err := row.Scan(&s.ThreadID, &s.SourceThreadID, &s.ReturnMode, &s.CreatedAt, &s.RequestToken)
	return s, err
}

// InsertScratchThread records a scratch fork. The thread must already exist
// and carry mode `scratch`; the return mode is validated by the table's CHECK,
// which cannot name `scratch` itself, so a promotion can never be a no-op.
func (s *Store) InsertScratchThread(row ScratchThread) error {
	if row.ThreadID == "" || row.SourceThreadID == "" || row.ReturnMode == "" {
		return fmt.Errorf("store: insert scratch thread: thread id, source thread id and return mode are required")
	}
	if row.CreatedAt == 0 {
		row.CreatedAt = nowMillis()
	}
	if _, err := s.db.Exec(
		`INSERT INTO scratch_threads (thread_id, source_thread_id, return_mode, created_at, request_token)
		 VALUES (?, ?, ?, ?, ?)`,
		row.ThreadID, row.SourceThreadID, row.ReturnMode, row.CreatedAt, nilIfEmpty(row.RequestToken),
	); err != nil {
		return fmt.Errorf("store: insert scratch thread %s: %w", row.ThreadID, err)
	}
	return nil
}

// GetScratchThread reads one scratch record. The bool reports existence so a
// caller can tell "not a scratch thread" from a read failure.
func (s *Store) GetScratchThread(threadID string) (ScratchThread, bool, error) {
	row, err := scanScratchThread(s.reader().QueryRow(
		`SELECT `+scratchThreadColumns+` FROM scratch_threads WHERE thread_id = ?`, threadID))
	if err == sql.ErrNoRows {
		return ScratchThread{}, false, nil
	}
	if err != nil {
		return ScratchThread{}, false, fmt.Errorf("store: get scratch thread %s: %w", threadID, err)
	}
	return row, true, nil
}

// ListScratchThreads returns every scratch record, oldest first. Boot uses it
// to delete the threads a restart left behind.
func (s *Store) ListScratchThreads() ([]ScratchThread, error) {
	rows, err := s.reader().Query(
		`SELECT ` + scratchThreadColumns + ` FROM scratch_threads ORDER BY created_at ASC, thread_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list scratch threads: %w", err)
	}
	defer rows.Close()
	out := []ScratchThread{}
	for rows.Next() {
		row, err := scanScratchThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan scratch thread: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate scratch threads: %w", err)
	}
	return out, nil
}

// ScratchThreadCaller names the thread whose `thread_ask` minted one
// scratch fork, through the request receipt the fork records. The bool
// reports whether the thread is a scratch fork at all, so a caller can tell
// an ordinary thread from a fork nobody owns.
//
// The owner is the receipt's source thread, not the record's own
// source_thread_id: that one names the thread that was ASKED, which is the
// thread the asking agent already knows about. A `/side-chat` fork carries
// no token and is owned by nobody, which is what keeps a person's side
// conversation out of every agent's reach.
func (s *Store) ScratchThreadCaller(threadID string) (callerThreadID string, scratch bool, err error) {
	err = s.reader().QueryRow(
		`SELECT COALESCE(receipts.source_thread_id, '')
		   FROM scratch_threads
		   LEFT JOIN thread_request_receipts receipts ON receipts.token = scratch_threads.request_token
		  WHERE scratch_threads.thread_id = ?`, threadID).Scan(&callerThreadID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read scratch thread caller %s: %w", threadID, err)
	}
	return callerThreadID, true, nil
}

// ListCallerScratchThreadIDs returns the scratch threads one calling thread
// owns: the forks its own asks minted. An empty caller owns none.
func (s *Store) ListCallerScratchThreadIDs(callerThreadID string) ([]string, error) {
	if callerThreadID == "" {
		return nil, nil
	}
	rows, err := s.reader().Query(
		`SELECT scratch_threads.thread_id
		   FROM scratch_threads
		   JOIN thread_request_receipts receipts ON receipts.token = scratch_threads.request_token
		  WHERE receipts.source_thread_id = ?
		  ORDER BY scratch_threads.thread_id ASC`, callerThreadID)
	if err != nil {
		return nil, fmt.Errorf("store: list scratch threads for caller %s: %w", callerThreadID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan caller scratch thread: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate caller scratch threads: %w", err)
	}
	return ids, nil
}

// DeleteScratchThread drops the record without touching the thread. It
// reports whether a row was there, so a caller that expects to own the
// record can tell a double delete from a first one. Deleting the thread
// removes the row through the cascade; this is for the rare case where the
// record goes and the thread stays.
func (s *Store) DeleteScratchThread(threadID string) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM scratch_threads WHERE thread_id = ?`, threadID)
	if err != nil {
		return false, fmt.Errorf("store: delete scratch thread %s: %w", threadID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: count deleted scratch thread %s: %w", threadID, err)
	}
	return affected > 0, nil
}

// PromoteScratchThread is the Keep action: one transaction that restores the
// mode recorded for the fork and drops the scratch record, so the thread is an
// ordinary sidebar thread from the next read on. It returns the promoted row.
//
// This is the only path out of `scratch`: threadmode.ValidateSet refuses the
// mode in both directions, because a caller that set the mode by hand would
// leave the scratch record behind and delete the thread at the next boot
// sweep.
func (s *Store) PromoteScratchThread(threadID string) (Thread, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, fmt.Errorf("store: begin promote scratch thread %s: %w", threadID, err)
	}
	defer tx.Rollback()

	var returnMode string
	if err := tx.QueryRow(
		`SELECT return_mode FROM scratch_threads WHERE thread_id = ?`, threadID,
	).Scan(&returnMode); err != nil {
		if err == sql.ErrNoRows {
			return Thread{}, fmt.Errorf("store: promote scratch thread %s: %w", threadID, sql.ErrNoRows)
		}
		return Thread{}, fmt.Errorf("store: read scratch thread %s: %w", threadID, err)
	}
	result, err := tx.Exec(
		`UPDATE threads SET mode = ? WHERE id = ? AND mode = ?`,
		returnMode, threadID, threadmode.ModeScratch,
	)
	if err != nil {
		return Thread{}, fmt.Errorf("store: promote scratch thread %s: %w", threadID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: promote scratch thread %s", threadID)); err != nil {
		return Thread{}, err
	}
	if _, err := tx.Exec(`DELETE FROM scratch_threads WHERE thread_id = ?`, threadID); err != nil {
		return Thread{}, fmt.Errorf("store: clear scratch record %s: %w", threadID, err)
	}
	// The title index row is keyed by thread, not by mode, so promotion
	// needs no reindex: scratch exclusion happens at query time.
	promoted, err := listThreadsByIDTx(tx, []string{threadID})
	if err != nil {
		return Thread{}, fmt.Errorf("store: read promoted scratch thread %s: %w", threadID, err)
	}
	if len(promoted) != 1 {
		return Thread{}, fmt.Errorf("store: read promoted scratch thread %s: read back %d rows, want 1", threadID, len(promoted))
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, fmt.Errorf("store: commit promote scratch thread %s: %w", threadID, err)
	}
	return promoted[0], nil
}
