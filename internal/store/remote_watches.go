package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
)

// RemoteWatch owns completion delivery on the originating computer. The
// destination receipt remains authoritative for execution. Queued means the
// ordinary message queue owns delivery/recovery, not that the agent read it.
type RemoteWatch struct {
	ComputerID  string `json:"computerId"`
	RequestID   string `json:"requestId"`
	ThreadID    string `json:"threadId"`
	Fingerprint string `json:"-"`
	Label       string `json:"label"`
	// Command is the display text of what runs: quoted argv, or the
	// interpreter plus "script". The label defaults to it when omitted.
	Command      string    `json:"command"`
	Receipt      RemoteJob `json:"receipt"`
	Error        string    `json:"error,omitempty"`
	Notification string    `json:"notification"`
	NextCheck    int64     `json:"-"`
	CreatedAt    int64     `json:"createdAt"`
}

const remoteWatchesV91SQL = `CREATE TABLE remote_watches (
 computer_id TEXT NOT NULL, request_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 fingerprint TEXT NOT NULL, label TEXT NOT NULL, receipt TEXT NOT NULL DEFAULT '{}',
 error TEXT NOT NULL DEFAULT '', notification TEXT NOT NULL DEFAULT 'pending'
 CHECK(notification IN ('pending','queued','dismissed')),
 next_check INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 PRIMARY KEY(computer_id,request_id)
);
CREATE INDEX idx_remote_watches_pending ON remote_watches(next_check) WHERE notification='pending';
CREATE INDEX idx_remote_watches_thread ON remote_watches(thread_id,created_at DESC);`

const remoteWatchesCommandV98SQL = `ALTER TABLE remote_watches ADD COLUMN command TEXT NOT NULL DEFAULT '';`

const remoteWatchColumns = `computer_id,request_id,thread_id,fingerprint,label,command,receipt,error,notification,next_check,created_at`

func scanRemoteWatch(row rowScanner) (RemoteWatch, error) {
	var w RemoteWatch
	var raw string
	err := row.Scan(&w.ComputerID, &w.RequestID, &w.ThreadID, &w.Fingerprint, &w.Label, &w.Command, &raw, &w.Error, &w.Notification, &w.NextCheck, &w.CreatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &w.Receipt)
	}
	return w, err
}

func (s *Store) GetRemoteWatch(computerID, requestID string) (RemoteWatch, error) {
	return scanRemoteWatch(s.reader().QueryRow(`SELECT `+remoteWatchColumns+` FROM remote_watches WHERE computer_id=? AND request_id=?`, computerID, requestID))
}

// Register before any network mutation. Retrying never replaces provenance or
// revives delivery already handed to the message queue.
// True permits cleanup on a definite refusal (new or previously refused).
// False preserves an earlier accepted/uncertain attempt, even if a retry fails.
func (s *Store) RegisterRemoteWatch(w RemoteWatch) (bool, error) {
	if !entityid.Valid(w.ComputerID) || !entityid.Valid(w.RequestID) || !entityid.Valid(w.ThreadID) || !validTransferDigest(w.Fingerprint) || len(w.Label) > 1024 || len(w.Command) > 1024 {
		return false, errors.New("invalid remote job watch")
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return false, err
	}
	defer release()
	defer tx.Rollback()
	old, err := scanRemoteWatch(tx.QueryRow(`SELECT `+remoteWatchColumns+` FROM remote_watches WHERE computer_id=? AND request_id=?`, w.ComputerID, w.RequestID))
	if err == nil {
		if old.ThreadID != w.ThreadID || old.Fingerprint != w.Fingerprint {
			return false, errorsx.Public("remote_request_conflict", "This request_id already belongs to another command or conversation. Retry only the original project, workspace, argv/script and timeout.", nil)
		}
		if old.Notification != "dismissed" || old.Receipt.ID != "" {
			return false, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	fresh := errors.Is(err, sql.ErrNoRows)
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM remote_watches WHERE notification='pending'`).Scan(&count); err != nil {
		return false, err
	}
	if count >= 256 {
		return false, errorsx.Public("remote_tracking_capacity", "256 remote jobs are awaiting completion or reconnection. Resolve those jobs before starting more.", nil)
	}
	if !fresh {
		_, err = tx.Exec(`UPDATE remote_watches SET notification='pending',next_check=? WHERE computer_id=? AND request_id=?`, time.Now().Add(25*time.Second).UnixMilli(), w.ComputerID, w.RequestID)
		if err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	_, err = tx.Exec(`INSERT INTO remote_watches(computer_id,request_id,thread_id,fingerprint,label,command,next_check,created_at) VALUES(?,?,?,?,?,?,?,?)`, w.ComputerID, w.RequestID, w.ThreadID, w.Fingerprint, w.Label, w.Command, time.Now().Add(25*time.Second).UnixMilli(), time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Update only the observed receipt; never overwrite notification ownership.
// A stale reply must not defer a known completion or replace its error either.
func (s *Store) ObserveRemoteWatch(computerID, requestID string, receipt RemoteJob, issue string, next int64) error {
	receipt.Output = "" // Output is fetched on demand; the monitor never retains logs.
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE remote_watches SET
 notification=CASE WHEN notification='dismissed' AND coalesce(json_extract(receipt,'$.id'),'')='' AND ?<>'' THEN 'pending' ELSE notification END,
 receipt=CASE WHEN coalesce(json_extract(receipt,'$.id'),'')<>'' AND json_extract(receipt,'$.state')<>'running' THEN receipt ELSE ? END,
 error=?,next_check=? WHERE computer_id=? AND request_id=?
 AND (coalesce(json_extract(receipt,'$.id'),'')='' OR
 (?<>'' AND (json_extract(receipt,'$.state')='running' OR ?=json_extract(receipt,'$.state'))))`, receipt.ID, string(raw), issue, next, computerID, requestID, receipt.ID, receipt.State)
	return err
}

func (s *Store) ListRemoteWatches(threadID string, due int64, limit int) ([]RemoteWatch, error) {
	if limit < 1 || limit > 256 {
		limit = 100
	}
	query := `SELECT ` + remoteWatchColumns + ` FROM remote_watches WHERE thread_id=? ORDER BY (notification='pending') DESC, created_at DESC LIMIT ?`
	args := []any{threadID, limit}
	if threadID == "" {
		query = `SELECT ` + remoteWatchColumns + ` FROM remote_watches WHERE notification='pending' AND next_check<=? ORDER BY next_check LIMIT ?`
		args = []any{due, limit}
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteWatch{}
	for rows.Next() {
		w, e := scanRemoteWatch(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) DismissRemoteWatch(computerID, requestID string) error {
	_, err := s.db.Exec(`UPDATE remote_watches SET notification='dismissed' WHERE computer_id=? AND request_id=? AND notification='pending'`, computerID, requestID)
	return err
}

// A definite refusal releases only this still-unaccepted watch. A concurrent
// status response proving acceptance always wins over refusal cleanup.
func (s *Store) RefuseRemoteWatch(computerID, requestID, issue string) error {
	_, err := s.db.Exec(`UPDATE remote_watches SET notification='dismissed',error=? WHERE computer_id=? AND request_id=? AND notification='pending' AND coalesce(json_extract(receipt,'$.id'),'')=''`, issue, computerID, requestID)
	return err
}

func (s *Store) RemoteWatchThreadIDs() ([]string, error) {
	rows, err := s.reader().Query(`SELECT DISTINCT thread_id FROM remote_watches WHERE notification='pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// QueueRemoteCompletion transfers delivery responsibility and inserts the
// ordinary queue message in ONE durable transaction. A crash on either side
// cannot create a second notification or lose both copies.
func (s *Store) QueueRemoteCompletion(computerID, requestID string, item FlushQueueItem) error {
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE remote_watches SET notification='queued' WHERE computer_id=? AND request_id=? AND thread_id=? AND notification='pending'`, computerID, requestID, item.ThreadID)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("remote completion already handed to the message queue")
	}
	if err = insertFlushQueueItem(tx, item); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) HasPendingRemoteWatches(threadID string) (bool, error) {
	var pending bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM remote_watches WHERE thread_id=? AND notification='pending')`, threadID).Scan(&pending)
	return pending, err
}

func (s *Store) HasPendingRemoteWatchesForComputer(computerID string) (bool, error) {
	var pending bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM remote_watches WHERE computer_id=? AND notification='pending')`, computerID).Scan(&pending)
	return pending, err
}

func (s *Store) HasUnfinishedRemoteWatches(threadID string) (bool, error) {
	var unfinished bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM remote_watches WHERE thread_id=? AND notification='pending' AND (coalesce(json_extract(receipt,'$.id'),'')='' OR json_extract(receipt,'$.state')='running'))`, threadID).Scan(&unfinished)
	return unfinished, err
}
