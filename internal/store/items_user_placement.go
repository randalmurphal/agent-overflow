package store

import (
	"database/sql"
	"fmt"
	"strings"

	"agent-overflow/internal/itemmeta"
)

// CaptureUserPlacementBoundary records a stable predecessor, excluding the
// user rows that will move. Call while the provider-order drain is serialized.
// Empty means the turn's head, including when only moving rows currently exist.
func (s *Store) CaptureUserPlacementBoundary(threadID string, turnIndex int, movingItemIDs []string) (string, error) {
	query := `SELECT id FROM timeline_items WHERE thread_id = ? AND turn_index = ?`
	args := []any{threadID, turnIndex}
	if len(movingItemIDs) != 0 {
		query += ` AND id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(movingItemIDs)), ",") + `)`
		for _, id := range movingItemIDs {
			args = append(args, id)
		}
	}
	var id string
	err := s.reader().QueryRow(query+` ORDER BY item_index DESC, id DESC LIMIT 1`, args...).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: capture user placement boundary: %w", err)
	}
	return id, nil
}

// PlaceUserItemsAfterBoundary inserts or moves a top-level user-message group
// after its captured predecessor. A retry uses that same predecessor even if
// response rows have since arrived. Existing content is preserved; only the
// first row's metadata is transformed using the predecessor's CURRENT index
// (the placed first-row index for head). All affected positions and numeric promotion boundaries commit
// atomically. Returns every changed row, with the user group first, for emission.
func (s *Store) PlaceUserItemsAfterBoundary(threadID string, turnIndex int, boundaryID string, items []Item, transformFirstMeta func(string, int) (string, error), updatedAt int64) ([]Item, error) {
	if threadID == "" || len(items) == 0 {
		return nil, fmt.Errorf("store: user placement requires a thread and rows")
	}
	moving := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.ID == "" || item.ID == boundaryID {
			return nil, fmt.Errorf("store: invalid user placement item %q", item.ID)
		}
		if _, found := moving[item.ID]; found {
			return nil, fmt.Errorf("store: duplicate user placement item %q", item.ID)
		}
		moving[item.ID] = struct{}{}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin user placement: %w", err)
	}
	defer tx.Rollback()
	boundary := -1
	if boundaryID != "" {
		if err := tx.QueryRow(`SELECT item_index FROM timeline_items WHERE thread_id = ? AND turn_index = ? AND id = ?`, threadID, turnIndex, boundaryID).Scan(&boundary); err != nil {
			return nil, fmt.Errorf("store: resolve user placement boundary %s: %w", boundaryID, err)
		}
	}
	start := boundary + 1
	group, existing, err := loadUserPlacementGroupTx(tx, threadID, turnIndex, items)
	if err != nil {
		return nil, err
	}
	suffix, err := userPlacementSuffixTx(tx, threadID, turnIndex, boundaryID, boundary, moving)
	if err != nil {
		return nil, err
	}
	if boundaryID == "" && len(suffix) > 0 && suffix[0].index < start {
		start = suffix[0].index
	}
	delta := 0
	if len(suffix) > 0 && suffix[0].index < start+len(group) {
		delta = start + len(group) - suffix[0].index
	}
	changedIDs := make([]string, 0, len(group)+len(suffix))
	changed := make(map[string]bool, len(group)+len(suffix))
	mark := func(id string) {
		if !changed[id] {
			changed[id] = true
			changedIDs = append(changedIDs, id)
		}
	}
	for _, item := range group {
		mark(item.ID)
	}
	// UNIQUE(thread, turn, item_index) is immediate. Temporarily move the
	// existing group below the turn head so swaps and suffix shifts cannot
	// collide with its old slots; only final rows escape the transaction.
	park := false
	for i, item := range group {
		if existing[i] && item.ItemIndex != start+i {
			park = true
			break
		}
	}
	if park {
		var minimum int
		if err := tx.QueryRow(`SELECT MIN(item_index) FROM timeline_items WHERE thread_id = ? AND turn_index = ?`, threadID, turnIndex).Scan(&minimum); err != nil {
			return nil, err
		}
		for i, item := range group {
			if !existing[i] {
				continue
			}
			if _, err := tx.Exec(`UPDATE items SET item_index = ? WHERE thread_id = ? AND id = ?`, minimum-i-1, threadID, item.ID); err != nil {
				return nil, err
			}
		}
	}
	if delta > 0 {
		if err := shiftUserPlacementSuffixTx(tx, threadID, turnIndex, suffix, delta, group, updatedAt, mark); err != nil {
			return nil, err
		}
	}
	for i, item := range group {
		if i == 0 && transformFirstMeta != nil {
			resolved := boundary
			if boundaryID == "" {
				resolved = start
			}
			item.Meta, err = transformFirstMeta(item.Meta, resolved)
			if err != nil {
				return nil, err
			}
		}
		item.ItemIndex = start + i
		item.UpdatedAt = updatedAt
		if existing[i] {
			if _, err := tx.Exec(`UPDATE items SET item_index = ?, meta = ?, updated_at = ? WHERE thread_id = ? AND id = ?`, item.ItemIndex, item.Meta, updatedAt, threadID, item.ID); err != nil {
				return nil, err
			}
		} else {
			applyItemDefaults(&item)
			if err := insertItemTx(tx, item, "store: insert placed user message"); err != nil {
				return nil, err
			}
		}
	}
	result := make([]Item, 0, len(changedIDs))
	for _, id := range changedIDs {
		item, err := readBackItemTx(tx, threadID, id)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit user placement: %w", err)
	}
	return result, nil
}

// UpdateItemMetaAtBoundary keeps an interrupt-anchored row in place while
// resolving the separate consumption boundary in the metadata transaction.
func (s *Store) UpdateItemMetaAtBoundary(threadID, itemID, boundaryID string, transform func(string, int) (string, error), updatedAt int64) (Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, err
	}
	defer tx.Rollback()
	if err := requireMutableItemTx(tx, threadID, itemID, "store: confirm user metadata"); err != nil {
		return Item{}, err
	}
	item, err := readBackItemTx(tx, threadID, itemID)
	if err != nil {
		return Item{}, err
	}
	boundary := -1
	if boundaryID != "" {
		if err := tx.QueryRow(`SELECT item_index FROM timeline_items WHERE thread_id = ? AND turn_index = ? AND id = ?`, threadID, item.TurnIndex, boundaryID).Scan(&boundary); err != nil {
			return Item{}, err
		}
	}
	meta, err := transform(item.Meta, boundary)
	if err != nil {
		return Item{}, err
	}
	if meta != item.Meta {
		if _, err := tx.Exec(`UPDATE items SET meta = ?, updated_at = ? WHERE thread_id = ? AND id = ?`, meta, updatedAt, threadID, itemID); err != nil {
			return Item{}, err
		}
	}
	item, err = readBackItemTx(tx, threadID, itemID)
	if err != nil {
		return Item{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, err
	}
	return item, nil
}

func loadUserPlacementGroupTx(tx *sql.Tx, threadID string, turnIndex int, items []Item) ([]Item, []bool, error) {
	// Copy only the caller's small group. Existing row content always wins.
	group := append([]Item(nil), items...)
	existing := make([]bool, len(group))
	for i := range group {
		item := &group[i]
		row, readErr := readBackItemTx(tx, threadID, item.ID)
		if readErr == sql.ErrNoRows {
			localized, err := localizeImportedItemTx(tx, threadID, item.ID, "store: place user item")
			if err != nil {
				return nil, nil, err
			}
			if localized {
				row, readErr = readBackItemTx(tx, threadID, item.ID)
			}
		}
		if readErr == nil {
			*item = row
			existing[i] = true
		} else if readErr != sql.ErrNoRows {
			return nil, nil, readErr
		}
		if item.ThreadID != threadID || item.TurnIndex != turnIndex || item.Kind != "user_text" || item.Role != "user" || item.ParentID != "" {
			return nil, nil, fmt.Errorf("store: placement item %s is not a top-level user message in the target turn", item.ID)
		}
		if !existing[i] && (item.Status == "" || item.CreatedAt == 0) {
			return nil, nil, fmt.Errorf("store: missing user placement row %s requires complete content", item.ID)
		}
	}
	return group, existing, nil
}

type userPlacementPosition struct {
	id    string
	index int
}

func userPlacementSuffixTx(tx *sql.Tx, threadID string, turnIndex int, boundaryID string, boundary int, moving map[string]struct{}) ([]userPlacementPosition, error) {
	// Find the first nonmoving suffix row. Most confirmations append and have
	// no suffix; delayed retries pay only for the rows they actually displace.
	suffixQuery := `SELECT id, item_index FROM timeline_items WHERE thread_id = ? AND turn_index = ?`
	suffixArgs := []any{threadID, turnIndex}
	if boundaryID != "" {
		suffixQuery += ` AND item_index > ?`
		suffixArgs = append(suffixArgs, boundary)
	}
	rows, err := tx.Query(suffixQuery+` ORDER BY item_index, id`, suffixArgs...)
	if err != nil {
		return nil, err
	}
	var suffix []userPlacementPosition
	for rows.Next() {
		var row userPlacementPosition
		if err := rows.Scan(&row.id, &row.index); err != nil {
			rows.Close()
			return nil, err
		}
		if _, skip := moving[row.id]; !skip {
			suffix = append(suffix, row)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return suffix, nil
}

func shiftUserPlacementSuffixTx(tx *sql.Tx, threadID string, turnIndex int, suffix []userPlacementPosition, delta int, group []Item, updatedAt int64, mark func(string)) error {

	for _, row := range suffix {
		if err := requireMutableItemTx(tx, threadID, row.id, "store: shift user placement suffix"); err != nil {
			return err
		}
	}
	// Localize the entire suffix before changing positions: a moved row
	// must not collide with a still-imported sibling's old slot.
	for i := len(suffix) - 1; i >= 0; i-- {
		row := suffix[i]
		if _, err := tx.Exec(`UPDATE items SET item_index = ?, updated_at = ? WHERE thread_id = ? AND id = ?`, row.index+delta, updatedAt, threadID, row.id); err != nil {
			return err
		}
		mark(row.id)
	}
	// Boundaries are numeric history metadata. User rows moving around them
	// do not change provider-order membership, but displaced content does.
	metas, err := tx.Query(`SELECT id, meta FROM timeline_items WHERE thread_id = ? AND turn_index = ? AND meta LIKE '%"promoted_echo_boundary"%'`, threadID, turnIndex)
	if err != nil {
		return err
	}
	type metaUpdate struct{ id, meta string }
	var updates []metaUpdate
	for metas.Next() {
		var id, meta string
		if err := metas.Scan(&id, &meta); err != nil {
			metas.Close()
			return err
		}
		state, err := itemmeta.DecodePromotionState(meta)
		if err != nil {
			metas.Close()
			return err
		}
		if state.HasEchoBoundary && state.EchoBoundary >= suffix[0].index {
			meta, err = itemmeta.MarkPromotedEchoBoundary(meta, state.EchoBoundary+delta)
			if err != nil {
				metas.Close()
				return err
			}
			updates = append(updates, metaUpdate{id, meta})
		}
	}
	err = metas.Err()
	metas.Close()
	if err != nil {
		return err
	}
	for _, update := range updates {
		if err := requireMutableItemTx(tx, threadID, update.id, "store: rebase user placement boundary"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE items SET meta = ?, updated_at = ? WHERE thread_id = ? AND id = ?`, update.meta, updatedAt, threadID, update.id); err != nil {
			return err
		}
		for i := range group {
			if group[i].ID == update.id {
				group[i].Meta = update.meta
			}
		}
		mark(update.id)
	}
	return nil
}
