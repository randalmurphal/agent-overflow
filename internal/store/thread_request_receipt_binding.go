package store

import "fmt"

// A receipt's message on its way to the provider: the dispatch mark, the
// binding to the turn that took the message in, a rebind after a merge fold,
// and the return to accepted when the provider never took it in.

// MarkThreadReceiptDispatched records the user row the request wrote once
// the message is handed to the provider, and moves the receipt out of
// `accepted` with no turn bound. The provider can still hold the message for
// a turn of its own, so the turn waits for BindThreadReceiptTurn and no turn
// end settles the receipt until then. A source row on this computer moves
// from `accepted` to `running` in the same transaction, whether or not the
// receipt still was `accepted`: a delivery that bound it first moved the
// receipt, and the source follows either way.
func (s *Store) MarkThreadReceiptDispatched(token, messageItemID string) (bool, error) {
	action := fmt.Sprintf("store: mark thread receipt %s dispatched", token)
	return s.writeReceiptAndRunSource(action, token,
		`UPDATE thread_request_receipts
		    SET state = 'running', message_item_id = ?, updated_at = ?
		  WHERE token = ? AND state = 'accepted'`,
		messageItemID, nowMillis(), token)
}

// BindThreadReceiptTurn records the turn the request's message was
// delivered into, the turn whose end settles it, and whether that turn
// opened for the message (ownTurn). It and RebindThreadReceiptTurn are the
// only writers of turn_id and own_turn, and the first delivery wins: a
// receipt that already names a turn, or has settled, is left alone. A
// delivery can arrive before the dispatch mark, so it moves an `accepted`
// receipt too. A source row on this computer moves from `accepted` to
// `running` in the same transaction, as MarkThreadReceiptDispatched moves
// it.
func (s *Store) BindThreadReceiptTurn(token, messageItemID, turnID string, ownTurn bool) (bool, error) {
	action := fmt.Sprintf("store: bind thread receipt %s to its turn", token)
	if turnID == "" {
		return false, fmt.Errorf("%s: turn id is required", action)
	}
	return s.writeReceiptAndRunSource(action, token,
		`UPDATE thread_request_receipts
		    SET state = 'running', message_item_id = ?, turn_id = ?, own_turn = ?, updated_at = ?
		  WHERE token = ? AND (state = 'accepted' OR (state = 'running' AND turn_id = ''))`,
		messageItemID, turnID, ownTurn, nowMillis(), token)
}

// writeReceiptAndRunSource runs one conditional receipt write and advances
// the token's source row from `accepted` to `running`, in one transaction.
// It reports whether the receipt write applied. A request another computer
// made has no source row here, and the advance changes nothing.
func (s *Store) writeReceiptAndRunSource(action, token, query string, args ...any) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	applied, err := rowsChanged(result, action)
	if err != nil {
		return false, err
	}
	result, err = tx.Exec(`UPDATE thread_requests SET state = 'running', updated_at = ? WHERE token = ? AND state = 'accepted'`,
		nowMillis(), token)
	if err != nil {
		return false, fmt.Errorf("%s: advance the request to running: %w", action, err)
	}
	advanced, err := rowsChanged(result, action)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	if advanced || applied {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	}
	return applied, nil
}

// RebindThreadReceiptTurn moves a running receipt from the turn its delivery
// named (fromTurnID, which is turn fromIndex of threadID) to toTurnID, once a
// merge fold has shown the provider ran the message in that later turn. It
// moves only from a turn that never ran, one with no rows left. Such a turn
// can carry a completion all the same, written when the next turn opened, and
// that completion answers nobody. A receipt bound anywhere else, or settled,
// is left alone.
func (s *Store) RebindThreadReceiptTurn(token, messageItemID, threadID string, fromIndex int, fromTurnID, toTurnID string, ownTurn bool) (bool, error) {
	action := fmt.Sprintf("store: rebind thread receipt %s to its turn", token)
	if fromTurnID == "" || toTurnID == "" {
		return false, fmt.Errorf("%s: both turn ids are required", action)
	}
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET message_item_id = ?, turn_id = ?, own_turn = ?, updated_at = ?
		  WHERE token = ? AND state = 'running' AND turn_id = ?
		    AND NOT EXISTS (SELECT 1 FROM timeline_items WHERE thread_id = ? AND turn_index = `+boundInt+`)`,
		messageItemID, toTurnID, ownTurn, nowMillis(), token, fromTurnID, threadID, fromIndex,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// ReturnThreadReceipt takes a receipt back to `accepted` when its message
// goes back before the provider took it in: the message goes back on
// the queue, and the next delivery binds the receipt again
// (BindThreadReceiptTurn). It reports whether the receipt is open and
// accepted afterwards; a settled or cancelled one is left as it is.
func (s *Store) ReturnThreadReceipt(token string) (bool, error) {
	action := fmt.Sprintf("store: return thread receipt %s", token)
	result, err := s.db.Exec(
		`UPDATE thread_request_receipts
		    SET state = 'accepted', message_item_id = '', turn_id = '', own_turn = 0, updated_at = ?
		  WHERE token = ? AND state IN ('accepted', 'running')`,
		nowMillis(), token,
	)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}
