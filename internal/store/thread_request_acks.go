package store

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// The acknowledgements this computer owes the computers its requests went
// to. A destination drops an answer whose latest revision nobody has
// acknowledged once its hold runs out, a day from the settlement or from
// the late reply (ExpireThreadRequestAnswers), and keeps an acknowledged
// one until the retention floor. So a revision the source holds and has not
// acknowledged is a debt: unpaid, the destination drops it and reads it as
// never collected. `ack_owed` records it in the transaction that took the
// revision (CollectThreadRequest), it survives a restart, and it is cleared
// only by the destination taking it. Its retry is paced by `next_check`, the
// poller's own clock for the row (idx_thread_requests_ack_owed).

// ThreadRequestAck is one acknowledgement owed: the destination's revision
// a request holds, and the pairing that admitted the request.
type ThreadRequestAck struct {
	Token      string
	ComputerID string
	Revision   int64
	Pairing    string
}

// oweThreadRequestAck records on ex that the request holds revision of its
// destination's receipt and owes the acknowledgement. A smaller revision
// than one already owed changes nothing. It reports whether the row
// changed.
func oweThreadRequestAck(ex sqlExecutor, token string, revision int64) (bool, error) {
	action := fmt.Sprintf("store: owe the acknowledgement of thread request %s", token)
	result, err := ex.Exec(`UPDATE thread_requests SET ack_owed = ?, updated_at = ?
		  WHERE token = ? AND target_computer_id <> '' AND lineage_only = 0 AND ack_owed < ?`,
		revision, nowMillis(), token, revision)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// OweThreadRequestAck records an acknowledgement a request owes for a
// revision it already holds, which its destination reports it never took:
// the request holds every revision up to its own, so a revision past it is
// not owed and is refused.
func (s *Store) OweThreadRequestAck(token string, revision int64) (bool, error) {
	action := fmt.Sprintf("store: owe the acknowledgement of thread request %s", token)
	result, err := s.db.Exec(`UPDATE thread_requests SET ack_owed = ?, updated_at = ?
		  WHERE token = ? AND target_computer_id <> '' AND lineage_only = 0 AND ack_owed < ? AND revision >= ?`,
		revision, nowMillis(), token, revision, revision)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// ackOwedSQL restates idx_thread_requests_ack_owed's predicate.
const ackOwedSQL = `ack_owed > 0`

// ackPage is a page of owed acknowledgements: limit, or the poll batch for
// a limit that is not positive.
func ackPage(limit int) int {
	if limit < 1 {
		return 32
	}
	return limit
}

// OwedThreadRequestAcks returns the acknowledgements owed to one computer
// for what the pairing held now (live, its generation) admitted, due or not, at
// most limit of them: every call to that computer carries them. What an
// ended pairing admitted is owed nothing (EndPairing).
func (s *Store) OwedThreadRequestAcks(computerID, live string, limit int) ([]ThreadRequestAck, error) {
	return s.queryThreadRequestAcks("store: list the acknowledgements owed to "+computerID,
		`SELECT token, target_computer_id, ack_owed, pairing FROM thread_requests
		  WHERE `+ackOwedSQL+` AND target_computer_id = `+textParam("?1")+` AND NOT `+pairingStaleSQL("?2")+`
		  ORDER BY next_check LIMIT `+strconv.Itoa(ackPage(limit)), computerID, live)
}

// DueThreadRequestAcks returns the acknowledgements whose retry is due by
// now, oldest first, at most limit of them.
func (s *Store) DueThreadRequestAcks(now int64, limit int) ([]ThreadRequestAck, error) {
	return s.queryThreadRequestAcks("store: list due thread request acknowledgements",
		`SELECT token, target_computer_id, ack_owed, pairing FROM thread_requests
		  WHERE `+ackOwedSQL+` AND next_check <= ? ORDER BY next_check LIMIT `+strconv.Itoa(ackPage(limit)), now)
}

func (s *Store) queryThreadRequestAcks(action, query string, args ...any) (_ []ThreadRequestAck, err error) {
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("%s: close: %w", action, closeErr)
		}
	}()
	var out []ThreadRequestAck
	for rows.Next() {
		var ack ThreadRequestAck
		if err := rows.Scan(&ack.Token, &ack.ComputerID, &ack.Revision, &ack.Pairing); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, ack)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", action, err)
	}
	return out, nil
}

// ClearThreadRequestAcks records that the destination took acks. An ack
// is cleared only while the revision it names is still the one owed: a
// newer revision collected while it was on its way is still owed.
func (s *Store) ClearThreadRequestAcks(acks []ThreadRequestAck) error {
	if len(acks) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ackRows(acks))
	if err != nil {
		return fmt.Errorf("store: clear thread request acknowledgements: encode: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE thread_requests SET ack_owed = 0, updated_at = ?1
		  WHERE token IN (SELECT json_extract(value, '$.token') FROM json_each(?2))
		    AND ack_owed = (SELECT json_extract(value, '$.revision') FROM json_each(?2)
		                     WHERE json_extract(value, '$.token') = thread_requests.token)`,
		nowMillis(), string(encoded)); err != nil {
		return fmt.Errorf("store: clear thread request acknowledgements: %w", err)
	}
	return nil
}

// DeferThreadRequestAcks puts off the retry of acks that did not reach their
// destination until next.
func (s *Store) DeferThreadRequestAcks(acks []ThreadRequestAck, next int64) error {
	if len(acks) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ackRows(acks))
	if err != nil {
		return fmt.Errorf("store: defer thread request acknowledgements: encode: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE thread_requests SET next_check = MAX(next_check, ?1), updated_at = ?2
		  WHERE `+ackOwedSQL+` AND token IN (SELECT json_extract(value, '$.token') FROM json_each(?3))`,
		next, nowMillis(), string(encoded)); err != nil {
		return fmt.Errorf("store: defer thread request acknowledgements: %w", err)
	}
	return nil
}

type ackRow struct {
	Token    string `json:"token"`
	Revision int64  `json:"revision"`
}

func ackRows(acks []ThreadRequestAck) []ackRow {
	rows := make([]ackRow, 0, len(acks))
	for _, ack := range acks {
		rows = append(rows, ackRow{Token: ack.Token, Revision: ack.Revision})
	}
	return rows
}
