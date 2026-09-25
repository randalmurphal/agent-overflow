package store

import (
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/itemmeta"
)

// Reverts and single-row deletes of a thread's timeline. A row a fork of
// the thread shows is given to a holder first (splitShownRowsTx); the rest
// are deleted here.

// DeleteThreadItem removes one item scoped by thread and id. Intended for
// rows that were reserved internally but never became visible history, such as
// quietly-persisted queued flush rows whose provider session died before echo.
//
// A deleted row with a parent recomputes the anchors above it in the same
// transaction: one chain read from its parent. A row of the thread's own
// that a fork of it shows moves to a holder (splitShownRowsTx), which takes
// it out of the thread; an inherited row is hidden (hideInheritedItemTx).
func (s *Store) DeleteThreadItem(threadID, itemID string) error {
	return s.writeItems(threadID, nil, "delete item "+threadID+"/"+itemID, func(tx *sql.Tx, w *cardWrite) error {
		turn, own, err := ownRowTurnTx(tx, threadID, itemID)
		if err != nil {
			return err
		}
		if own {
			taken, err := splitShownRowsTx(tx, w, threadID, forkSplit{fromTurn: turn, where: "id = ?", args: []any{itemID}})
			if err != nil || taken > 0 {
				return err
			}
		}
		sharedDeleted, err := deleteSharedHistoryItemTx(tx, w, threadID, itemID)
		if err != nil || sharedDeleted > 0 {
			return err
		}
		hidden, err := hideInheritedItemTx(tx, w, threadID, itemID)
		if err != nil || hidden {
			return err
		}
		n, err := deleteItemRowsTx(tx, w, `id = ?`, []any{itemID}, fmt.Sprintf("store: delete item %s/%s", threadID, itemID))
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("store: delete item %s/%s: %w", threadID, itemID, sql.ErrNoRows)
		}
		return nil
	})
}

// ownRowTurnTx reads the turn of one of threadID's own rows, local or
// imported. own is false when the thread does not own a row with that id.
func ownRowTurnTx(tx *sql.Tx, threadID, itemID string) (int, bool, error) {
	query, args := ownTimelineArms(threadID, timelineSelection{
		Columns:  timelineIDColumns,
		KeyFirst: true,
		Where:    "items.id = ?", WhereArgs: []any{itemID},
	})
	var row timelineRow
	err := tx.QueryRow(query, args...).Scan(&row.id, &row.turn, &row.item)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: read row %s/%s: %w", threadID, itemID, err)
	}
	return row.turn, true, nil
}

// deleteItemRowsTx deletes the thread's local rows the predicate selects
// with their search rows, and records each for the subagent cards: its
// chain loses it (cardWrite.deleted). It returns how many it deleted.
func deleteItemRowsTx(tx *sql.Tx, w *cardWrite, predicate string, args []any, action string) (int64, error) {
	rows, err := tx.Query(`DELETE FROM items WHERE thread_id = ? AND (`+predicate+`) RETURNING `+subagentRowColumns(""),
		append([]any{w.threadID}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", action, err)
	}
	var deleted []subagentRow
	for rows.Next() {
		row, err := scanSubagentRow(rows)
		if err != nil {
			return 0, errors.Join(fmt.Errorf("%s: scan deleted row: %w", action, err), rows.Close())
		}
		deleted = append(deleted, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("%s: iterate deleted rows: %w", action, err)
	}
	ids := make([]string, len(deleted))
	for i, row := range deleted {
		ids[i] = row.id
		w.deleted(row)
	}
	if err := deleteThreadSearchItemsTx(tx, w.threadID, ids); err != nil {
		return 0, err
	}
	return int64(len(deleted)), nil
}

// DeleteConversationFromTurn removes items and turn rows with
// turn_index >= fromTurnIndex. Rolling back to a user message deletes
// that selected prompt too, so the predicate is inclusive. Message
// anchors follow their ITEMS via the FK cascade, never their own
// cached turn_index, which can drift from the item's (R8-3).
// Everything runs in ONE transaction so a failure rolls back the whole
// truncation.
func (s *Store) DeleteConversationFromTurn(threadID string, fromTurnIndex int) (int, HistoryStamp, error) {
	var deleted int
	var stamp HistoryStamp
	err := s.writeItems(threadID, nil, "delete conversation from turn", func(tx *sql.Tx, w *cardWrite) error {
		if err := cutAsyncQuestionsTx(tx, threadID, fromTurnIndex, "turn_index >= ?", []any{fromTurnIndex}); err != nil {
			return err
		}
		// The rows a fork of this thread shows move to a holder first; the
		// forks read no turn from here on.
		keep := timelineRow{turn: fromTurnIndex, item: nothingKept.item}
		taken, err := splitShownRowsTx(tx, w, threadID, forkSplit{
			fromTurn: fromTurnIndex, keep: &keep, turnsKeptThrough: fromTurnIndex - 1,
		})
		if err != nil {
			return err
		}
		sharedDeleted, err := deleteSharedHistoryFromTurnTx(tx, w, threadID, fromTurnIndex, "turn_index >= ?", []any{fromTurnIndex})
		if err != nil {
			return err
		}
		n, err := deleteItemRowsTx(tx, w, `turn_index >= ?`, []any{fromTurnIndex},
			fmt.Sprintf("store: delete items from turn for thread %s", threadID))
		if err != nil {
			return err
		}
		deleted = int(n+sharedDeleted) + taken
		if err := retractInheritedTx(tx, w, threadID, fromTurnIndex, "turn_index >= ?", []any{fromTurnIndex}); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`DELETE FROM turns WHERE thread_id = ? AND turn_index >= ?`,
			threadID, fromTurnIndex,
		); err != nil {
			return fmt.Errorf("store: delete turns from turn for thread %s: %w", threadID, err)
		}
		// The cut can take an anchor's preview or newest row, or a resume
		// prompt; the surviving anchors above what it deleted are
		// recomputed here, so the cut commits with every stamp exact.
		if err := w.finish(); err != nil {
			return err
		}
		// The post-cut stamps, read inside the deleting transaction so the
		// pair the `user_message:reverted` event carries describes exactly
		// this cut and not a later write. A thread deleted underneath the
		// cut reports the zero stamp; the caller's event is moot by then.
		stamp, _, err = readHistoryStampTx(tx, threadID)
		return err
	})
	// Truncating the conversation is a structural change, not a fresh
	// interaction. The next user_text persist (or a turn settle that
	// follows the resume) bumps activity through MarkThreadActivity.
	if err != nil {
		return 0, HistoryStamp{}, err
	}
	return deleted, stamp, nil
}

// DeleteConversationFromItem removes the anchor item and everything after it
// in PROVIDER order, plus the turn rows of turns left without any items.
// Message anchors of deleted user rows cascade away via their items FK.
//
// Provider order is timeline order, (turn_index, item_index), for every row
// except interrupt-promoted queued messages (itemmeta promotion marker):
// those were bumped over their turn's not-yet-persisted tail, so their
// same-turn NON-USER successors precede them in the provider transcript and
// survive the cut; same-turn user successors are later-queued messages and go.
// When the promoted row's echo stamped a provider-order boundary (the CLI
// consumed it mid-loop and its response persisted in the same turn), non-user
// successors PAST the boundary are that response (provider-order AFTER the
// message) and are deleted with it. Whenever the cut removes same-turn
// non-user content, the surviving turn row's settle metadata described that
// deleted content: completed_at is trimmed back to the last surviving row
// and the assistant_message_id cleared; token usage stays, the spend was real
// and the ledger already has it.
//
// This is the item-granular twin of DeleteConversationFromTurn, for providers
// whose conversation revert cuts at the message itself (Claude's session-file
// slice anchors on the message uuid). Queued flush messages can share a turn
// with the prompt that was running when they were enqueued; deleting the whole
// turn would take that original prompt, and the agent work before the queued
// message, down with them. When the anchor opens its turn the predicate
// degenerates to DeleteConversationFromTurn's. Codex reverts keep the
// turn-granular delete: thread/fork cuts provider history at a turn boundary,
// and SQLite must match it.
//
// Returns the ids of the anchor turn's SURVIVING items, in item order.
// The `user_message:reverted` event carries this kept-set so the
// frontend can mirror the cut exactly: the promoted-row predicate
// below removes a non-contiguous slice of the anchor turn, which no
// boundary comparison can express, and duplicating the predicate in UI
// code would fork it. Survivors are persisted rows by definition, so
// "everything in the anchor turn NOT in this list" is a complete
// removal instruction even for pane-only rows SQLite never saw. Empty
// when the anchor opened its turn (the common case: whole turn gone).
func (s *Store) DeleteConversationFromItem(threadID, itemID string) ([]string, HistoryStamp, error) {
	var kept []string
	var stamp HistoryStamp
	err := s.writeItems(threadID, nil, "delete conversation from item", func(tx *sql.Tx, w *cardWrite) error {
		var err error
		kept, stamp, err = deleteConversationFromItemTx(tx, w, threadID, itemID)
		return err
	})
	if err != nil {
		return nil, HistoryStamp{}, err
	}
	return kept, stamp, nil
}

func deleteConversationFromItemTx(tx *sql.Tx, w *cardWrite, threadID, itemID string) ([]string, HistoryStamp, error) {
	var turnIndex, itemIndex int
	var meta string
	anchorQuery, anchorArgs, err := timelineArms(tx, threadID, timelineSelection{
		Columns:  func(string, string) string { return "items.turn_index, items.item_index, items.meta" },
		KeyFirst: true,
		Where:    "items.id = ?", WhereArgs: []any{itemID},
	})
	if err != nil {
		return nil, HistoryStamp{}, err
	}
	if err := tx.QueryRow(anchorQuery, anchorArgs...).Scan(&turnIndex, &itemIndex, &meta); err != nil {
		return nil, HistoryStamp{}, fmt.Errorf("store: delete conversation from item lookup %s/%s: %w", threadID, itemID, err)
	}
	promotion, err := itemmeta.DecodePromotionState(meta)
	if err != nil {
		// Corrupt anchor meta means the provider-order cut is undecidable;
		// failing beats silently degrading to a display-order cut that the
		// session slice would disagree with.
		return nil, HistoryStamp{}, fmt.Errorf("store: delete conversation from item %s/%s: %w", threadID, itemID, err)
	}

	// deletedTurnContent: does this cut remove same-turn NON-USER rows?
	// Those are the rows the anchor turn's settle metadata describes
	// (streamed content, the response), so their deletion triggers the
	// trim below. Computed BEFORE the delete removes the evidence.
	contentPredicate := ""
	contentArgs := []any{}
	itemPredicate := `turn_index > ? OR (turn_index = ? AND item_index >= ?)`
	itemArgs := []any{threadID, turnIndex, turnIndex, itemIndex}
	if promotion.Promoted {
		// Same-turn successors up to the echo boundary that are not
		// top-level user rows (streamed assistant/tool content AND
		// parented wire-only user rows (subagent prompts nested under
		// their launching tool_call) are the interrupted round's tail:
		// they precede the promoted message in the provider transcript
		// and stay. Same-turn TOP-LEVEL user successors are later-promoted
		// queued rows and go. Past the boundary (stamped when the CLI
		// consumed the message mid-loop), everything else is the response,
		// provider-order AFTER the message, and goes with it.
		itemPredicate = `turn_index > ? OR (turn_index = ? AND item_index >= ? AND role = 'user' AND parent_id = '')`
		if promotion.HasEchoBoundary {
			itemPredicate += ` OR (turn_index = ? AND item_index > ? AND (role != 'user' OR parent_id != ''))`
			itemArgs = append(itemArgs, turnIndex, promotion.EchoBoundary)
			contentPredicate = `items.item_index > ?`
			contentArgs = []any{promotion.EchoBoundary}
		}
	} else {
		contentPredicate = `items.item_index > ?`
		contentArgs = []any{itemIndex}
	}
	deletedTurnContent := false
	if contentPredicate != "" {
		contentQuery, args, err := timelineArms(tx, threadID, timelineSelection{
			Columns:   func(string, string) string { return "1" },
			Turn:      "?",
			TurnArgs:  []any{turnIndex},
			Where:     `(items.role != 'user' OR items.parent_id != '') AND ` + contentPredicate,
			WhereArgs: contentArgs,
		})
		if err != nil {
			return nil, HistoryStamp{}, err
		}
		if err := tx.QueryRow(`SELECT EXISTS(`+contentQuery+`)`, args...).Scan(&deletedTurnContent); err != nil {
			return nil, HistoryStamp{}, fmt.Errorf("store: probe deleted turn content for thread %s: %w", threadID, err)
		}
	}
	if err := cutAsyncQuestionsTx(tx, threadID, turnIndex, itemPredicate, itemArgs[1:]); err != nil {
		return nil, HistoryStamp{}, err
	}
	// The forks of this thread read it up to its last surviving row; the
	// rows they show past it move to a holder first. The anchor turn keeps
	// its turn row while rows survive in it.
	survivor, survives, err := lastRowThroughTurnTx(tx, threadID, turnIndex, "NOT ("+itemPredicate+")", itemArgs[1:]...)
	if err != nil {
		return nil, HistoryStamp{}, err
	}
	keep, keptThrough := nothingKept, turnIndex-1
	if survives {
		keep = timelineRow{turn: survivor.turn, item: survivor.item + 1}
		if survivor.turn == turnIndex {
			keptThrough = turnIndex
		}
	}
	if _, err := splitShownRowsTx(tx, w, threadID, forkSplit{
		fromTurn: turnIndex, where: itemPredicate, args: itemArgs[1:], keep: &keep, turnsKeptThrough: keptThrough,
	}); err != nil {
		return nil, HistoryStamp{}, err
	}
	if _, err := deleteSharedHistoryFromTurnTx(tx, w, threadID, turnIndex, itemPredicate, itemArgs[1:]); err != nil {
		return nil, HistoryStamp{}, err
	}
	// Every reverted row sits at or after the anchor turn; the bound keeps
	// the delete on the turn range of the thread's index.
	if _, err := deleteItemRowsTx(tx, w, `turn_index >= ? AND (`+itemPredicate+`)`, append([]any{turnIndex}, itemArgs[1:]...),
		fmt.Sprintf("store: delete items from item for thread %s", threadID)); err != nil {
		return nil, HistoryStamp{}, err
	}
	if err := retractInheritedTx(tx, w, threadID, turnIndex, itemPredicate, itemArgs[1:]); err != nil {
		return nil, HistoryStamp{}, err
	}

	// The anchor turn's kept-set, read AFTER the delete so it reflects
	// exactly what the predicate left standing.
	keptAnchorTurnItemIDs, err := listTurnTimelineItemIDs(tx, threadID, turnIndex)
	if err != nil {
		return nil, HistoryStamp{}, err
	}

	// The anchor turn keeps its turn row while any items survive in it:
	// the remaining prefix still happened.
	survivors, survivorArgs, err := timelineArms(tx, threadID, timelineSelection{
		Columns: func(string, string) string { return "1" },
		Turn:    "turns.turn_index",
	})
	if err != nil {
		return nil, HistoryStamp{}, err
	}
	if _, err := tx.Exec(
		`DELETE FROM turns WHERE thread_id = ?
		 AND turn_index >= ?
		 AND NOT EXISTS (`+survivors+`)`,
		append([]any{threadID, turnIndex}, survivorArgs...)...,
	); err != nil {
		return nil, HistoryStamp{}, fmt.Errorf("store: delete turns from item for thread %s: %w", threadID, err)
	}

	// A surviving anchor turn that just lost streamed content ends at its
	// last surviving row, not at the settle the deleted response produced:
	// trim completed_at back (never forward: MIN) and drop the
	// assistant_message_id that now points at a deleted message. Gated on
	// deletedTurnContent, NOT on the anchor being mid-turn: an anchor that
	// is the LAST row of its turn (an at-pickup bumped quiet flush) deletes
	// nothing the settle metadata describes, and rewriting it would corrupt
	// accurate history. The completed_at IS NOT NULL guard leaves a
	// (guard-violating) active turn alone rather than fabricating a
	// settlement.
	if deletedTurnContent {
		if err := trimTurnSettleToSurvivorsTx(tx, threadID, turnIndex); err != nil {
			return nil, HistoryStamp{}, err
		}
	}

	// The surviving anchors above what the cut deleted, recomputed in the
	// cut: see DeleteConversationFromTurn.
	if err := w.finish(); err != nil {
		return nil, HistoryStamp{}, err
	}
	// Post-cut stamps, read inside the deleting transaction; see
	// DeleteConversationFromTurn.
	stamp, _, err := readHistoryStampTx(tx, threadID)
	if err != nil {
		return nil, HistoryStamp{}, err
	}
	// Like DeleteConversationFromTurn: truncation is a structural change,
	// not a fresh interaction, so no MarkThreadActivity bump.
	return keptAnchorTurnItemIDs, stamp, nil
}

// ListTurnTimelineItemIDs returns the ids of one turn's timeline rows, mutable
// and imported, in item order. A caller that writes into a cut's surviving
// anchor turn after DeleteConversationFromItem re-reads its kept set here.
func (s *Store) ListTurnTimelineItemIDs(threadID string, turnIndex int) ([]string, error) {
	return listTurnTimelineItemIDs(s.reader(), threadID, turnIndex)
}

func listTurnTimelineItemIDs(q sqlQueryer, threadID string, turnIndex int) ([]string, error) {
	query, args, err := timelineIDSelection(q, threadID, timelineSelection{Turn: "?", TurnArgs: []any{turnIndex}, OrderBy: "turn_index, item_index"})
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list turn %d items for thread %s: %w", turnIndex, threadID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan turn %d item for thread %s: %w", turnIndex, threadID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate turn %d items for thread %s: %w", turnIndex, threadID, err)
	}
	return ids, nil
}

// trimTurnSettleToSurvivorsTx rewrites a surviving turn row whose settle
// metadata described just-deleted content: completed_at trims back to the
// last surviving row's created_at (bumped anchors carry dispatch-time
// created_at OLDER than the kept tail, so the anchor is the wrong target)
// and assistant_message_id clears: the message it referenced is gone.
// No-op when the turn kept no rows (its turn row was already deleted) or
// was never settled.
func trimTurnSettleToSurvivorsTx(tx *sql.Tx, threadID string, turnIndex int) error {
	var lastKept sql.NullInt64
	query, args, err := timelineArms(tx, threadID, timelineSelection{
		Columns:  func(string, string) string { return "items.created_at AS created_at" },
		Turn:     "?",
		TurnArgs: []any{turnIndex},
	})
	if err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT MAX(created_at) FROM (\n"+query+"\n)", args...).Scan(&lastKept); err != nil {
		return fmt.Errorf("store: trim turn settle survivors lookup for thread %s: %w", threadID, err)
	}
	if !lastKept.Valid {
		return nil
	}
	if _, err := tx.Exec(
		`UPDATE turns SET completed_at = MIN(completed_at, ?), assistant_message_id = ''
		 WHERE thread_id = ? AND turn_index = ? AND completed_at IS NOT NULL`,
		lastKept.Int64, threadID, turnIndex,
	); err != nil {
		return fmt.Errorf("store: trim anchor turn settle for thread %s: %w", threadID, err)
	}
	return nil
}
