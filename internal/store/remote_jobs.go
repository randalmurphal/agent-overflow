package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/entityid"
)

// RemoteJob is a command receipt. Accepted IDs survive history cleanup and
// restore: neither an absent reply nor a backend restart permits re-execution.
// Only live processes are held in memory. Output is a bounded settled tail.
type RemoteJob struct {
	ID             string `json:"id"`
	OwnerID        string `json:"-"`
	Fingerprint    string `json:"-"`
	SourceThreadID string `json:"sourceThreadId"`
	ProjectID      string `json:"projectId"`
	Workspace      string `json:"workspace"`
	State          string `json:"state"`
	StartedAt      int64  `json:"startedAt"`
	FinishedAt     int64  `json:"finishedAt,omitempty"`
	ExitCode       int    `json:"exitCode"`
	Output         string `json:"output,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
	Error          string `json:"error,omitempty"`
}

const RemoteJobOutputLimit = 128 << 10

var ErrRemoteJobNotFound = errors.New("remote command: command not found")

const remoteJobsV87SQL = `CREATE TABLE remote_jobs (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL, fingerprint TEXT NOT NULL,
 source_thread_id TEXT NOT NULL, project_id TEXT NOT NULL, workspace TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('running','succeeded','failed','canceled','interrupted')),
 started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
 exit_code INTEGER NOT NULL DEFAULT -1, output TEXT NOT NULL DEFAULT '',
 truncated INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_remote_jobs_active ON remote_jobs(state) WHERE state = 'running';
CREATE INDEX idx_remote_jobs_finished ON remote_jobs(finished_at DESC) WHERE state != 'running';
CREATE INDEX idx_remote_jobs_output ON remote_jobs(finished_at DESC) WHERE output != '' AND state != 'running';`

const remoteJobColumns = `id, owner_id, fingerprint, source_thread_id, project_id, workspace,
 state, started_at, finished_at, exit_code, output, truncated, error`

func scanRemoteJob(row interface{ Scan(...any) error }) (RemoteJob, error) {
	var job RemoteJob
	err := row.Scan(&job.ID, &job.OwnerID, &job.Fingerprint, &job.SourceThreadID, &job.ProjectID,
		&job.Workspace, &job.State, &job.StartedAt, &job.FinishedAt, &job.ExitCode, &job.Output, &job.Truncated, &job.Error)
	return job, err
}

func (s *Store) GetRemoteJob(id string) (RemoteJob, error) {
	job, err := scanRemoteJob(s.reader().QueryRow(`SELECT `+remoteJobColumns+` FROM remote_jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteJob{}, ErrRemoteJobNotFound
	}
	return job, err
}

// AcceptRemoteJob durably claims an ID BEFORE spawning. Fresh is false for a
// matching retry, even after completion or restart. Owner and request digest
// are immutable; a caller cannot reuse another device's receipt.
func (s *Store) AcceptRemoteJob(job RemoteJob) (accepted RemoteJob, fresh bool, err error) {
	if !entityid.Valid(job.ID) || job.OwnerID == "" || len(job.OwnerID) > 128 ||
		!validTransferDigest(job.Fingerprint) || !entityid.Valid(job.SourceThreadID) ||
		!entityid.Valid(job.ProjectID) || job.Workspace == "" || len(job.Workspace) > 32768 {
		return RemoteJob{}, false, errors.New("remote command: invalid request")
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return RemoteJob{}, false, err
	}
	defer release()
	defer tx.Rollback()
	previous, err := scanRemoteJob(tx.QueryRow(`SELECT `+remoteJobColumns+` FROM remote_jobs WHERE id = ?`, job.ID))
	if err == nil {
		if previous.OwnerID != job.OwnerID || previous.Fingerprint != job.Fingerprint {
			return RemoteJob{}, false, errors.New("remote command: request ID already belongs to another command")
		}
		return previous, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RemoteJob{}, false, err
	}
	job.State, job.StartedAt, job.ExitCode = "running", time.Now().UnixMilli(), -1
	job.FinishedAt, job.Output, job.Error, job.Truncated = 0, "", "", false
	_, err = tx.Exec(`INSERT INTO remote_jobs (`+remoteJobColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, job.OwnerID, job.Fingerprint, job.SourceThreadID, job.ProjectID, job.Workspace,
		job.State, job.StartedAt, job.FinishedAt, job.ExitCode, job.Output, job.Truncated, job.Error)
	if err != nil {
		return RemoteJob{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return RemoteJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) FinishRemoteJob(job RemoteJob) error {
	if job.State != "succeeded" && job.State != "failed" && job.State != "canceled" && job.State != "interrupted" {
		return errors.New("remote command: invalid terminal state")
	}
	if len(job.Output) > RemoteJobOutputLimit || len(job.Error) > 4096 {
		return errors.New("remote command: output exceeds its bound")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE remote_jobs SET state = ?, finished_at = ?, exit_code = ?, output = ?, truncated = ?, error = ? WHERE id = ? AND state = 'running'`,
		job.State, time.Now().UnixMilli(), job.ExitCode, job.Output, job.Truncated, job.Error, job.ID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		// A commit error can leave an acknowledged-by-SQLite result. Retrying
		// settlement is safe only for that exact result, never another state.
		previous, readErr := scanRemoteJob(tx.QueryRow(`SELECT `+remoteJobColumns+` FROM remote_jobs WHERE id = ?`, job.ID))
		if readErr == nil && previous.State == job.State && previous.ExitCode == job.ExitCode && previous.Output == job.Output && previous.Truncated == job.Truncated && previous.Error == job.Error {
			return nil
		}
		return fmt.Errorf("remote command: no active receipt to finish: %s", job.ID)
	}
	// Keep the latest 128 settled tails; older receipts retain their IDs and
	// provenance indefinitely. A history restore cannot resurrect their output
	// or turn an old retry into a new command.
	_, err = tx.Exec(`UPDATE remote_jobs SET output = '', truncated = 1 WHERE output != '' AND state != 'running'
 AND id NOT IN (SELECT id FROM remote_jobs WHERE state != 'running' ORDER BY finished_at DESC, id DESC LIMIT 128)`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverRemoteJobs runs once before admission. An accepted command may have
// run before the previous process died, so recovery NEVER launches it again.
func (s *Store) RecoverRemoteJobs() error {
	_, err := s.db.Exec(`UPDATE remote_jobs SET state = 'interrupted', finished_at = ?, error = 'The computer restarted before this command finished.' WHERE state = 'running'`, time.Now().UnixMilli())
	return err
}
