package store

import (
	"errors"
	"time"
)

// TransferJob is the small scheduler projection. Recovery JSON is loaded only
// by an active worker, never by a startup scan of every pending operation.
type TransferJob struct {
	ID, Direction, Phase, Error string
	NextAttemptAt               int64
	UpdatedAt                   int64
	RetryCount                  int
}

func (s *Store) NextThreadTransferJobs(limit int) ([]TransferJob, error) {
	if limit < 1 || limit > 128 {
		return nil, errors.New("transfer: invalid recovery page size")
	}
	rows, err := s.reader().Query(`SELECT id,direction,phase,error,next_attempt_at,updated_at,retry_count FROM thread_transfers
WHERE (phase IN ('complete','canceled') AND cleanup_pending = 1) OR
(phase NOT IN ('complete','canceled') AND
((direction = 'outgoing' AND (peer_state IS NOT NULL OR cancel_requested = 1)) OR (direction = 'incoming' AND archive_size > 0)))
ORDER BY next_attempt_at,created_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []TransferJob
	for rows.Next() {
		var job TransferJob
		if err := rows.Scan(&job.ID, &job.Direction, &job.Phase, &job.Error, &job.NextAttemptAt, &job.UpdatedAt, &job.RetryCount); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// Restart recovery rechecks even parked incoming work: the process may have
// stopped after persisting activation proof but before waking its worker.
func (s *Store) WakeThreadTransferJobs() error {
	_, err := s.db.Exec(`UPDATE thread_transfers SET next_attempt_at = 0,retry_count = 0 WHERE (phase NOT IN ('complete','canceled') OR cleanup_pending = 1) AND (next_attempt_at <> 0 OR retry_count <> 0)`)
	return err
}

func (s *Store) WakeThreadTransferJob(id string) error {
	_, err := s.db.Exec(`UPDATE thread_transfers SET next_attempt_at = 0,retry_count = 0 WHERE id = ? AND (phase NOT IN ('complete','canceled') OR cleanup_pending = 1)`, id)
	return err
}

func (s *Store) FinishThreadTransferAttempt(id string, nextAttempt int64, retries int, message string) error {
	if retries < 0 || retries > 32 || len(message) > 4096 {
		return errors.New("transfer: invalid retry state")
	}
	_, err := s.db.Exec(`UPDATE thread_transfers SET next_attempt_at = ?,retry_count = ?,
updated_at = CASE WHEN error <> ? THEN ? ELSE updated_at END,error = ? WHERE id = ? AND (phase NOT IN ('complete','canceled') OR cleanup_pending = 1)`,
		nextAttempt, retries, message, time.Now().UnixMilli(), message, id)
	return err
}

// The journal and ownership survive archive removal. A crash before this write
// merely repeats the idempotent cleanup; nonterminal recovery bytes cannot be
// declared disposable by a caller.
func (s *Store) FinishThreadTransferCleanup(id string) error {
	result, err := s.db.Exec(`UPDATE thread_transfers SET cleanup_pending = 0,error = '',retry_count = 0 WHERE id = ? AND phase IN ('complete','canceled')`, id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return errors.New("transfer: operation is not ready for cleanup")
	}
	return nil
}
