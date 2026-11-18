package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// RemoteOutcomeUnknown is the state a command's receipt records here when
// the pairing with the computer that runs it ended before its outcome was
// known (EndPairing). No destination writes it.
const RemoteOutcomeUnknown = "unknown"

// PairingEnd is what this computer records when it will hear nothing more
// from a paired computer: the answer its open requests settle with, the
// error its unfinished commands are recorded with, and whether the person
// chose that nobody is to be told (Forget).
type PairingEnd struct {
	RequestAnswer []byte
	CommandError  string
	Disarm        bool
}

// EndedRequest is one open request EndPairing settled, with whether its
// wake is still armed as the settlement left it.
type EndedRequest struct {
	Token  string
	Notify bool
}

// PairingEnded is what EndPairing wrote.
type PairingEnded struct {
	Settled []EndedRequest
	// CommandThreads are the threads whose commands it recorded with an
	// unknown outcome.
	CommandThreads []string
}

// pairingStaleSQL matches a request or command row on one computer that a
// pairing other than the live one admitted: with no live pairing (an empty
// generation in param), every row; otherwise every row another generation
// admitted.
func pairingStaleSQL(param string) string {
	live := textParam(param)
	return `(` + live + ` = '' OR pairing <> ` + live + `)`
}

// PairingNoneHeld is the generation BindRecordPairing gives the records of a
// computer this one holds no pairing with. No pairing is ever given it, so
// they belong to none.
const PairingNoneHeld = "ended"

// UnboundPairingComputers lists the computers with requests or commands
// that name no pairing generation, as rows from before generations were
// recorded do (idx_thread_requests_unbound_pairing,
// idx_remote_watches_unbound_pairing).
func (s *Store) UnboundPairingComputers() ([]string, error) {
	return s.queryThreadIDs("store: list the computers of records with no pairing",
		`SELECT target_computer_id FROM thread_requests WHERE target_computer_id <> '' AND pairing = ''
		 UNION SELECT computer_id FROM remote_watches WHERE pairing = ''`)
}

// BindRecordPairing records generation as the pairing of every request and
// command on computerID that names none, in one transaction.
func (s *Store) BindRecordPairing(computerID, generation string) (err error) {
	action := fmt.Sprintf("store: bind the records on %s to their pairing", computerID)
	if generation == "" {
		return fmt.Errorf("%s: no generation", action)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if _, err := tx.Exec(`UPDATE thread_requests SET pairing = ?2
		  WHERE target_computer_id = `+textParam("?1")+` AND target_computer_id <> '' AND pairing = ''`, computerID, generation); err != nil {
		return fmt.Errorf("%s: requests: %w", action, err)
	}
	if _, err := tx.Exec(`UPDATE remote_watches SET pairing = ?2 WHERE computer_id = ?1 AND pairing = ''`, computerID, generation); err != nil {
		return fmt.Errorf("%s: commands: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// OpenThreadRequestTokensOn returns every open request this computer holds
// on computerID under a pairing other than live (pairingStaleSQL): the set
// EndPairing settles, which its caller locks first.
func (s *Store) OpenThreadRequestTokensOn(computerID, live string) ([]string, error) {
	return s.queryThreadIDs("store: list the open requests on "+computerID,
		`SELECT token FROM thread_requests WHERE target_computer_id = `+textParam("?1")+` AND settled_at IS NULL AND `+pairingStaleSQL("?2")+`
		  ORDER BY token`, computerID, live)
}

// EndPairing is the one transition for the pairings with computerID other
// than live, the generation of the pairing held now (empty when none is
// held): those pairings will answer nothing again. Over both ledgers, in one
// transaction, for what those pairings admitted alone: the requests named in
// open (the caller holds their settle locks, and read them with
// OpenThreadRequestTokensOn) settle `errored` with end.RequestAnswer, their
// wakes disarmed when end.Disarm is set; every request leaves the poll and
// owes no acknowledgement, its answer kept; every command not known to have
// finished is recorded as finished with an unknown outcome, so its thread is
// told once and its watch can leave; and no cancel is owed there any more.
// A row the live pairing admitted is left as it is.
func (s *Store) EndPairing(computerID, live string, open []string, end PairingEnd) (_ PairingEnded, err error) {
	action := fmt.Sprintf("store: end the pairing with %s", computerID)
	settlement := ThreadRequestSettlement{
		State: ThreadRequestErrored, Answer: end.RequestAnswer, AnswerKind: ThreadAnswerError, DisarmWake: end.Disarm,
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PairingEnded{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var ended PairingEnded
	for _, token := range open {
		settled, err := settleThreadRequest(tx, token, threadRequestOpenStates, settlement, "")
		if err != nil {
			return PairingEnded{}, err
		}
		if !settled {
			continue
		}
		var notify bool
		if err := tx.QueryRow(`SELECT notify FROM thread_requests WHERE token = ?`, token).Scan(&notify); err != nil {
			return PairingEnded{}, fmt.Errorf("%s: read the wake of %s: %w", action, token, err)
		}
		ended.Settled = append(ended.Settled, EndedRequest{Token: token, Notify: notify})
	}
	if _, err := tx.Exec(`UPDATE thread_requests SET polling = 0, ack_owed = 0, issue = '', updated_at = ?3
		  WHERE target_computer_id = `+textParam("?1")+` AND settled_at IS NOT NULL AND (polling = 1 OR ack_owed > 0)
		    AND `+pairingStaleSQL("?2"),
		computerID, live, nowMillis()); err != nil {
		return PairingEnded{}, fmt.Errorf("%s: retire its requests: %w", action, err)
	}
	now := nowMillis()
	rows, err := tx.Query(`UPDATE remote_watches
		    SET receipt = json_set(receipt, '$.id', request_id, '$.sourceThreadId', launched_in_thread_id,
		          '$.state', `+sqlTextLiteral(RemoteOutcomeUnknown)+`, '$.error', ?1, '$.finishedAt', ?2, '$.exitCode', -1),
		        error = '', next_check = 0
		  WHERE computer_id = ?3 AND notification = 'pending' AND `+pairingStaleSQL("?4")+`
		    AND (coalesce(json_extract(receipt, '$.id'), '') = '' OR json_extract(receipt, '$.state') = 'running')
		  RETURNING thread_id`, end.CommandError, now, computerID, live)
	if err != nil {
		return PairingEnded{}, fmt.Errorf("%s: record its commands' outcome: %w", action, err)
	}
	for rows.Next() {
		var thread string
		if err := rows.Scan(&thread); err != nil {
			_ = rows.Close()
			return PairingEnded{}, fmt.Errorf("%s: scan: %w", action, err)
		}
		if !slices.Contains(ended.CommandThreads, thread) {
			ended.CommandThreads = append(ended.CommandThreads, thread)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return PairingEnded{}, fmt.Errorf("%s: record its commands' outcome: %w", action, err)
	}
	if _, err := tx.Exec(`UPDATE remote_watches SET cancel_owed_at = 0, cancel_next_at = 0
		  WHERE computer_id = ?1 AND cancel_owed_at <> 0 AND `+pairingStaleSQL("?2"), computerID, live); err != nil {
		return PairingEnded{}, fmt.Errorf("%s: settle the cancels owed there: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return PairingEnded{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	for _, request := range ended.Settled {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: request.Token})
	}
	return ended, nil
}

// ListOpenThreadRequestsForComputer returns the oldest open requests this
// computer holds on one paired computer, at most limit of them, and how many
// there are: what forgetting that computer abandons, named in its warning.
func (s *Store) ListOpenThreadRequestsForComputer(computerID string, limit int) (_ []ThreadRequest, total int, err error) {
	action := fmt.Sprintf("store: list open thread requests for computer %s", computerID)
	tx, err := s.reader().Begin()
	if err != nil {
		return nil, 0, fmt.Errorf("%s: begin: %w", action, err)
	}
	// One read, so the count is never below what is listed.
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	if limit < 1 {
		limit = 50
	}
	if err := tx.QueryRow(`SELECT count(*) FROM thread_requests WHERE target_computer_id = `+boundText+` AND settled_at IS NULL`,
		computerID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("%s: count: %w", action, err)
	}
	rows, err := tx.Query(`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE target_computer_id = `+boundText+` AND settled_at IS NULL
		  ORDER BY created_at ASC, token ASC LIMIT `+strconv.Itoa(limit), computerID)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", action, err)
	}
	listed, err := collectThreadRequests(rows, action)
	if err != nil {
		return nil, 0, err
	}
	return listed, total, nil
}
