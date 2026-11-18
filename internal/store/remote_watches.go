package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
)

// RemoteWatch owns completion delivery on the originating computer. The
// destination receipt remains authoritative for execution. Queued means the
// ordinary message queue owns delivery/recovery, not that the agent read it.
type RemoteWatch struct {
	ComputerID string `json:"computerId"`
	RequestID  string `json:"requestId"`
	ThreadID   string `json:"threadId"`
	// LaunchedIn is the thread that launched the job, which the
	// destination's receipt names as its source. It differs from ThreadID,
	// the thread that owns the watch and receives the completion, once
	// New Thread, Same Assistant moved the watch to the assistant's next
	// thread.
	LaunchedIn  string `json:"-"`
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
	// ConfirmedAt records the provider's echo of the queued completion. A
	// queued one without it may never have reached the agent.
	ConfirmedAt int64 `json:"-"`
	// WakeAttempts counts the completions a session end gave back, which
	// pace the next delivery (WithdrawRemoteCompletion); WakeIssue says why
	// delivery was given up, the completion kept on the watch.
	WakeAttempts int64  `json:"-"`
	WakeIssue    string `json:"wakeIssue,omitempty"`
	// CancelOwedAt is when a cancel the job is owed was first owed, 0 when
	// none is (OweRemoteCancel).
	CancelOwedAt int64 `json:"-"`
	// StoppedAt is when a rest or an archive dismissed the completion, 0
	// when none did (StopRemoteWatch, DismissUndeliveredRemoteWatches). Such
	// a dismissal is final: nothing brings the completion back.
	StoppedAt int64 `json:"-"`
	// Pairing is the generation of the pairing with ComputerID the command
	// was admitted under (ThreadRequest.Pairing).
	Pairing string `json:"-"`
	// ComputerName is what the person called that computer when the command
	// was admitted, which names it once no pairing here does.
	ComputerName string `json:"-"`
}

// remoteWatchesV91SQL is the table as v91 made it. v154 rebuilt it with
// thread_id cascading from its thread (migration_v154_retire_features.go).
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

const remoteWatchColumns = `computer_id,request_id,thread_id,launched_in_thread_id,fingerprint,label,command,receipt,error,notification,next_check,created_at,coalesce(confirmed_at,0),wake_attempts,wake_issue,cancel_owed_at,stopped_at,pairing,computer_name`

func scanRemoteWatch(row rowScanner) (RemoteWatch, error) {
	var w RemoteWatch
	var raw string
	err := row.Scan(&w.ComputerID, &w.RequestID, &w.ThreadID, &w.LaunchedIn, &w.Fingerprint, &w.Label, &w.Command, &raw, &w.Error, &w.Notification, &w.NextCheck, &w.CreatedAt, &w.ConfirmedAt, &w.WakeAttempts, &w.WakeIssue, &w.CancelOwedAt, &w.StoppedAt, &w.Pairing, &w.ComputerName)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &w.Receipt)
	}
	return w, err
}

func (s *Store) GetRemoteWatch(computerID, requestID string) (RemoteWatch, error) {
	return scanRemoteWatch(s.reader().QueryRow(`SELECT `+remoteWatchColumns+` FROM remote_watches WHERE computer_id=? AND request_id=?`, computerID, requestID))
}

// Refused reports a watch released because the destination refused its
// start before accepting it: dismissed with no receipt and by no rest or
// archive. A retry under its ID registers fresh; any other watch stands for
// a start that may have run.
func (w RemoteWatch) Refused() bool {
	return w.Notification == "dismissed" && w.Receipt.ID == "" && w.StoppedAt == 0
}

// Register before any network mutation. Retrying never replaces provenance or
// revives delivery already handed to the message queue.
// True permits cleanup on a definite refusal (new or previously refused).
// False preserves an earlier accepted/uncertain attempt, even if a retry fails.
func (s *Store) RegisterRemoteWatch(w RemoteWatch) (bool, error) {
	if !entityid.Valid(w.ComputerID) || !entityid.Valid(w.RequestID) || !entityid.Valid(w.ThreadID) || !validTransferDigest(w.Fingerprint) || len(w.Label) > 1024 || len(w.Command) > 1024 || len(w.ComputerName) > 1024 {
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
		if old.LaunchedIn != w.ThreadID || old.Fingerprint != w.Fingerprint {
			return false, errorsx.Public("remote_request_conflict", "This request_id already belongs to another command or conversation. Retry only the original project, workspace, argv/script and timeout.", nil)
		}
		if !old.Refused() {
			return false, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	fresh := errors.Is(err, sql.ErrNoRows)
	var count, archived int
	if err = tx.QueryRow(`SELECT count(*), coalesce(sum(CASE WHEN `+archivedRemoteCompletion+` THEN 1 ELSE 0 END),0) FROM remote_watches WHERE notification='pending'`).Scan(&count, &archived); err != nil {
		return false, err
	}
	if count >= 256 {
		held := ""
		if archived > 0 {
			held = fmt.Sprintf(" %d of them are finished jobs held for archived conversations; unarchive those conversations to receive the results.", archived)
		}
		return false, errorsx.Public("remote_tracking_capacity", "256 remote jobs are awaiting completion or reconnection."+held+" Resolve those jobs before starting more.", nil)
	}
	if !fresh {
		// A start refused before is tried again under the pairing held now.
		_, err = tx.Exec(`UPDATE remote_watches SET notification='pending',next_check=?,pairing=?,computer_name=? WHERE computer_id=? AND request_id=?`, time.Now().Add(25*time.Second).UnixMilli(), w.Pairing, w.ComputerName, w.ComputerID, w.RequestID)
		if err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	_, err = tx.Exec(`INSERT INTO remote_watches(computer_id,request_id,thread_id,launched_in_thread_id,fingerprint,label,command,next_check,created_at,pairing,computer_name) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, w.ComputerID, w.RequestID, w.ThreadID, w.ThreadID, w.Fingerprint, w.Label, w.Command, time.Now().Add(25*time.Second).UnixMilli(), time.Now().UnixMilli(), w.Pairing, w.ComputerName)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Update only the observed receipt; never overwrite notification ownership.
// A stale reply must not defer a known completion or replace its error either.
// Proof that the destination accepted a start refused earlier brings its
// completion back; a dismissal by a rest or an archive (stopped_at), or one
// whose cancel is owed, stays.
func (s *Store) ObserveRemoteWatch(computerID, requestID string, receipt RemoteJob, issue string, next int64) error {
	receipt.Output = "" // Output is fetched on demand; the monitor never retains logs.
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE remote_watches SET
 notification=CASE WHEN notification='dismissed' AND stopped_at=0 AND cancel_owed_at=0 AND coalesce(json_extract(receipt,'$.id'),'')='' AND ?<>'' THEN 'pending' ELSE notification END,
 receipt=CASE WHEN coalesce(json_extract(receipt,'$.id'),'')<>'' AND json_extract(receipt,'$.state')<>'running' THEN receipt ELSE ? END,
 error=?,next_check=? WHERE computer_id=? AND request_id=?
 AND (coalesce(json_extract(receipt,'$.id'),'')='' OR
 (?<>'' AND (json_extract(receipt,'$.state')='running' OR ?=json_extract(receipt,'$.state'))))`, receipt.ID, string(raw), issue, next, computerID, requestID, receipt.ID, receipt.State)
	return err
}

// remoteWatchCheckable is a watch the watcher checks when it comes due. An
// archived thread's completions wait for it to be unarchived, and must not
// take the due list's places meanwhile. A job whose cancel is owed is not
// polled: its watch waits for the cancel to settle. It states the predicate
// of idx_remote_watches_pending.
const remoteWatchCheckable = `notification='pending' AND cancel_owed_at=0
 AND NOT EXISTS (SELECT 1 FROM threads t WHERE t.id=remote_watches.thread_id AND t.archived=1)`

// NextRemoteWatchCheck is when the earliest watch the watcher checks comes
// due, unset when there is none.
func (s *Store) NextRemoteWatchCheck() (ScheduledMoment, error) {
	return s.nextRemoteWatchMoment(nextRemoteWatchCheckSQL)
}

const nextRemoteWatchCheckSQL = `SELECT next_check FROM remote_watches WHERE ` + remoteWatchCheckable + ` ORDER BY next_check LIMIT 1`

// NextOwedRemoteCancel is when the earliest owed cancel is sent next, unset
// when none is owed.
func (s *Store) NextOwedRemoteCancel() (ScheduledMoment, error) {
	return s.nextRemoteWatchMoment(nextOwedRemoteCancelSQL)
}

const nextOwedRemoteCancelSQL = `SELECT cancel_next_at FROM remote_watches WHERE ` + owedRemoteCancel + ` ORDER BY cancel_next_at LIMIT 1`

func (s *Store) nextRemoteWatchMoment(query string) (ScheduledMoment, error) {
	var at int64
	switch err := s.reader().QueryRow(query).Scan(&at); {
	case errors.Is(err, sql.ErrNoRows):
		return ScheduledMoment{}, nil
	case err != nil:
		return ScheduledMoment{}, fmt.Errorf("store: read the next remote command check: %w", err)
	}
	return ScheduledMoment{At: at, Set: true}, nil
}

func (s *Store) ListRemoteWatches(threadID string, due int64, limit int) ([]RemoteWatch, error) {
	if limit < 1 || limit > 256 {
		limit = 100
	}
	query := `SELECT ` + remoteWatchColumns + ` FROM remote_watches WHERE thread_id=? ORDER BY (notification='pending') DESC, created_at DESC`
	args := []any{threadID}
	if threadID == "" {
		query = `SELECT ` + remoteWatchColumns + ` FROM remote_watches WHERE ` + remoteWatchCheckable + ` AND next_check<=? ORDER BY next_check`
		args = []any{due}
	}
	query += ` LIMIT ` + strconv.Itoa(limit)
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

// StopRemoteWatch is a rest's or an archive's dismissal of a pending
// notification, final (stopped_at). With keepFinished it applies only while
// the receipt still says the job has not finished: a finish recorded first,
// by the watcher or a reply, keeps it.
func (s *Store) StopRemoteWatch(computerID, requestID string, keepFinished bool) error {
	query := `UPDATE remote_watches SET notification='dismissed',stopped_at=? WHERE computer_id=? AND request_id=? AND notification='pending'`
	if keepFinished {
		query += ` AND (coalesce(json_extract(receipt,'$.id'),'')='' OR json_extract(receipt,'$.state')='running')`
	}
	_, err := s.db.Exec(query, time.Now().UnixMilli(), computerID, requestID)
	return err
}

// DismissUndeliveredRemoteWatches drops every notification of threadID's
// remote commands that the agent has not taken in: a pending one, and one
// queued that the provider has not confirmed, which a withdrawal or a session
// teardown would otherwise give back to its watch for the watcher to deliver
// again. It reports how many it dropped.
func (s *Store) DismissUndeliveredRemoteWatches(threadID string) (int64, error) {
	r, err := s.db.Exec(`UPDATE remote_watches SET notification='dismissed',stopped_at=?
 WHERE thread_id=? AND (notification='pending' OR (notification='queued' AND confirmed_at IS NULL))`, time.Now().UnixMilli(), threadID)
	if err != nil {
		return 0, fmt.Errorf("store: dismiss the undelivered remote commands of %s: %w", threadID, err)
	}
	return r.RowsAffected()
}

// A definite refusal releases only this still-unaccepted watch. A concurrent
// status response proving acceptance always wins over refusal cleanup.
func (s *Store) RefuseRemoteWatch(computerID, requestID, issue string) error {
	_, err := s.db.Exec(`UPDATE remote_watches SET notification='dismissed',error=? WHERE computer_id=? AND request_id=? AND notification='pending' AND coalesce(json_extract(receipt,'$.id'),'')=''`, issue, computerID, requestID)
	return err
}

// remoteWatchThreadIDsSQL reads through the partial index
// idx_remote_watches_pending, whose predicate it restates.
const remoteWatchThreadIDsSQL = `SELECT DISTINCT thread_id FROM remote_watches INDEXED BY idx_remote_watches_pending
	WHERE notification='pending'`

func (s *Store) RemoteWatchThreadIDs() ([]string, error) {
	rows, err := s.reader().Query(remoteWatchThreadIDsSQL)
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

// WakeWithdrawal is which delivered message a withdrawal of an answer or a
// completion takes back to its owner.
type WakeWithdrawal int

const (
	// WithdrawQueued takes back only a message still on the queue.
	WithdrawQueued WakeWithdrawal = iota
	// WithdrawUnconfirmed also takes back one the provider was handed and
	// never confirmed.
	WithdrawUnconfirmed
)

// CompletionReturn is how a completion goes back to its watch: due at once
// (the zero value), due at NextCheck as attempt Attempts, or given up with
// Issue, which dismisses the watch with the completion kept on it.
type CompletionReturn struct {
	NextCheck int64
	Attempts  int64
	Issue     string
}

// WithdrawRemoteCompletion takes a completion's message that never reached
// the agent back out of the queue and returns delivery to its watch, in ONE
// durable transaction. The watch goes back to pending when the message was
// still queued: one already dispatched has been delivered. One the provider
// was handed and never confirmed (unconfirmed) has no row left and goes
// back all the same, while the watch still reads queued. One the provider
// confirmed is never given back, by any caller: a queue row can outlive its
// message's echo (the process died between the provider write and the
// settlement that deletes the row, or that delete failed), and such a row
// goes with the watch left queued and confirmed.
//
// paced, when set, decides when the watcher delivers it again from the
// completions given back so far (WakeAttempts), read in the same
// transaction; nil makes it due at once.
func (s *Store) WithdrawRemoteCompletion(computerID, requestID, rowID string, scope WakeWithdrawal, paced func(attempts int64) CompletionReturn) error {
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return err
	}
	defer release()
	defer func() { _ = tx.Rollback() }()
	r, err := tx.Exec(`DELETE FROM flush_queue_items WHERE id=?`, rowID)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 && scope == WithdrawQueued {
		return tx.Commit()
	}
	where := ` WHERE computer_id=? AND request_id=? AND notification='queued' AND confirmed_at IS NULL`
	back := CompletionReturn{}
	if paced != nil {
		var attempts int64
		err = tx.QueryRow(`SELECT wake_attempts FROM remote_watches`+where, computerID, requestID).Scan(&attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit()
		}
		if err != nil {
			return err
		}
		back = paced(attempts)
	}
	switch {
	case back.Issue != "":
		_, err = tx.Exec(`UPDATE remote_watches SET notification='dismissed',wake_issue=?`+where, back.Issue, computerID, requestID)
	case back.NextCheck != 0:
		_, err = tx.Exec(`UPDATE remote_watches SET notification='pending',next_check=?,wake_attempts=?`+where, back.NextCheck, back.Attempts, computerID, requestID)
	default:
		_, err = tx.Exec(`UPDATE remote_watches SET notification='pending'`+where, computerID, requestID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmRemoteCompletion records the provider's echo of a queued
// completion: the second phase of its delivery mark, after which no
// session end or restart gives it back. It reports whether the watch was
// still unconfirmed.
func (s *Store) ConfirmRemoteCompletion(computerID, requestID string, at int64) (bool, error) {
	r, err := s.db.Exec(`UPDATE remote_watches SET confirmed_at=? WHERE computer_id=? AND request_id=? AND notification='queued' AND confirmed_at IS NULL`, at, computerID, requestID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// UnconfirmedRemoteCompletions returns the watches whose completion was
// queued and never confirmed: at boot, no session holds them, so the agent
// may never have taken them in.
func (s *Store) UnconfirmedRemoteCompletions() ([]RemoteWatch, error) {
	rows, err := s.reader().Query(`SELECT ` + remoteWatchColumns + ` FROM remote_watches WHERE notification='queued' AND confirmed_at IS NULL`)
	if err != nil {
		return nil, err
	}
	var out []RemoteWatch
	for rows.Next() {
		w, err := scanRemoteWatch(rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, w)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func (s *Store) HasPendingRemoteWatches(threadID string) (bool, error) {
	var pending bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM remote_watches WHERE thread_id=? AND notification='pending')`, threadID).Scan(&pending)
	return pending, err
}

// archivedRemoteCompletion matches a pending watch whose job has finished
// and whose conversation is archived: a completion the archive kept for
// the unarchive.
const archivedRemoteCompletion = `notification='pending' AND coalesce(json_extract(receipt,'$.id'),'')<>'' AND json_extract(receipt,'$.state')<>'running'
 AND EXISTS (SELECT 1 FROM threads t WHERE t.id=remote_watches.thread_id AND t.archived=1)`

// PendingRemoteWatchesForComputer counts what forgetting a computer drops
// of the commands there: the completions held for archived conversations;
// the other pending notifications, which still need the computer; and the
// cancels owed there for commands whose notification a stop dismissed,
// which need it too.
func (s *Store) PendingRemoteWatchesForComputer(computerID string) (archived, other, stopped int, err error) {
	err = s.reader().QueryRow(`SELECT coalesce(sum(CASE WHEN `+archivedRemoteCompletion+` THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN notification='pending' AND NOT (`+archivedRemoteCompletion+`) THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN notification<>'pending' THEN 1 ELSE 0 END),0)
 FROM remote_watches WHERE computer_id=? AND (notification='pending' OR cancel_owed_at<>0)`, computerID).Scan(&archived, &other, &stopped)
	return archived, other, stopped, err
}

// DismissArchivedRemoteCompletions drops the completions held for a
// computer's archived conversations and returns those conversations.
func (s *Store) DismissArchivedRemoteCompletions(computerID string) ([]string, error) {
	rows, err := s.db.Query(`UPDATE remote_watches SET notification='dismissed' WHERE computer_id=? AND `+archivedRemoteCompletion+` RETURNING thread_id`, computerID)
	if err != nil {
		return nil, err
	}
	var threads []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if !slices.Contains(threads, id) {
			threads = append(threads, id)
		}
	}
	return threads, errors.Join(rows.Err(), rows.Close())
}

// remoteWatchDoneSQL matches a watch with nothing left to deliver: dismissed,
// or its completion queued and confirmed by the provider's echo. It is the
// predicate of idx_remote_watches_done.
const remoteWatchDoneSQL = `(notification = 'dismissed' OR (notification = 'queued' AND confirmed_at IS NOT NULL))`

// DeleteRemoteWatchesBefore deletes the watches with nothing left to
// deliver that were made before cutoff, and reports how many. A pending
// watch, or a queued completion the provider has not confirmed, is still
// owed and stays whatever its age. It reads first, on the read pool, and
// takes the writer only when a watch is past the floor.
func (s *Store) DeleteRemoteWatchesBefore(cutoff int64) (int64, error) {
	var due bool
	if err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM remote_watches WHERE `+remoteWatchDoneSQL+` AND created_at < ?)`,
		cutoff).Scan(&due); err != nil {
		return 0, fmt.Errorf("store: read whether remote watches are past %d: %w", cutoff, err)
	}
	if !due {
		return 0, nil
	}
	result, err := s.db.Exec(`DELETE FROM remote_watches WHERE `+remoteWatchDoneSQL+` AND created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: delete remote watches before %d: %w", cutoff, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count deleted remote watches: %w", err)
	}
	return deleted, nil
}

// OweRemoteCancel records that the job is owed a cancel the destination did
// not take, to be sent again at next. The first owing time is kept, so the
// watcher knows when the destination has ended the job on its own.
func (s *Store) OweRemoteCancel(computerID, requestID string, now, next int64) error {
	_, err := s.db.Exec(`UPDATE remote_watches SET
 cancel_owed_at=CASE WHEN cancel_owed_at=0 THEN ? ELSE cancel_owed_at END, cancel_next_at=?
 WHERE computer_id=? AND request_id=?`, now, next, computerID, requestID)
	return err
}

// owedRemoteCancel is a watch owed a cancel, the predicate of
// idx_remote_watches_cancel_owed.
const owedRemoteCancel = `cancel_owed_at <> 0`

// owedRemoteCancelsDueSQL is the watcher's due list of owed cancels.
const owedRemoteCancelsDueSQL = `SELECT ` + remoteWatchColumns + ` FROM remote_watches WHERE ` + owedRemoteCancel + ` AND cancel_next_at <= ? ORDER BY cancel_next_at`

// ListOwedRemoteCancels returns, soonest first, the owed cancels due by due.
func (s *Store) ListOwedRemoteCancels(due int64, limit int) ([]RemoteWatch, error) {
	if limit < 1 || limit > 256 {
		limit = 100
	}
	rows, err := s.reader().Query(owedRemoteCancelsDueSQL+` LIMIT `+strconv.Itoa(limit), due)
	if err != nil {
		return nil, err
	}
	out := []RemoteWatch{}
	for rows.Next() {
		w, e := scanRemoteWatch(rows)
		if e != nil {
			return nil, errors.Join(e, rows.Close())
		}
		out = append(out, w)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// SettleRemoteCancel records that the job owes no cancel any more.
func (s *Store) SettleRemoteCancel(computerID, requestID string) error {
	_, err := s.db.Exec(`UPDATE remote_watches SET cancel_owed_at=0, cancel_next_at=0 WHERE computer_id=? AND request_id=?`, computerID, requestID)
	return err
}

func (s *Store) HasUnfinishedRemoteWatches(threadID string) (bool, error) {
	var unfinished bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM remote_watches WHERE thread_id=? AND notification='pending' AND (coalesce(json_extract(receipt,'$.id'),'')='' OR json_extract(receipt,'$.state')='running'))`, threadID).Scan(&unfinished)
	return unfinished, err
}
