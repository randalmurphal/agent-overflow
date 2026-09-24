package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ItemReadIsDecorated reports whether a page read of this row can differ
// from the stored row: a subagent anchor takes its descendant aggregate
// from its children (decorateSubagentAnchors) and a proposed plan takes
// its state and comment count from the plan tables
// (decorateProposedPlanItems). Completions also carry their launch context.
// An emitter must push such a row as a page
// would read it, or a client holding the undecorated copy at the stored
// revision could prove a window fresh whose card is behind (§3.1).
//
// The predicate is the decorators' own admission test, widened to every
// row they would consider; ItemReadNeedsDecoration narrows it.
func ItemReadIsDecorated(item Item) bool {
	switch item.Kind {
	case "tool_call":
		return item.ToolName != "collab_agent"
	case "tool_completion":
		return item.CompletionOf != ""
	}
	return item.Role == "assistant" && item.PayloadKind == "proposed_plan"
}

// ItemReadNeedsDecoration is ItemReadIsDecorated narrowed for tool calls
// by the row's own stamp (decorateSubagentAnchors' walk decision): a clean
// stamped anchor and a plain unstamped tool call read as stored, so the
// write's read-back is the page read. Imported, dirty and readTime rows,
// unstamped carriers, and unstamped rows of a thread whose backfill is
// pending still need ListWireItems. Completions and plans always do.
func (s *Store) ItemReadNeedsDecoration(item Item) (bool, error) {
	if !ItemReadIsDecorated(item) {
		return false, nil
	}
	if item.Kind != "tool_call" || item.PayloadKind == "proposed_plan" {
		return true, nil
	}
	walk, err := subagentWalkProbe(s.reader(), item.ThreadID)(item)
	if err != nil {
		return false, fmt.Errorf("store: decide decoration of %s/%s: %w", item.ThreadID, item.ID, err)
	}
	return walk, nil
}

// firstChildAnchorsSQL selects the clean stamped anchors whose round holds
// exactly the written row: its parent, or the carrier a resume prompt
// names. Two primary-key probes.
var firstChildAnchorsSQL = `SELECT a.id FROM items a
 WHERE a.thread_id = ?1
   AND a.id IN (?2, COALESCE((SELECT ` + aggPromptCarrierSQL("p.") + ` FROM items p
                               WHERE p.thread_id = ?1 AND p.id = ?3 AND ` + aggPromptSQL("p.") + `), ''))
   AND ` + aggAnchorableSQL("a.") + ` AND ` + aggCleanSQL("a.meta") + `
   AND ` + aggJX("a.meta", aggCountPath) + ` = 1
   AND (` + aggJX("a.meta", aggNewestPath+"[0]") + `, ` + aggJX("a.meta", aggNewestPath+"[1]") + `) = (?4, ?5)`

// ListFirstChildWireAnchors returns, as a page reads them, the anchors
// whose card the written child just opened: a clean stamped parent (or,
// for a resume prompt, the carrier it names) whose round now holds this
// row alone. The emitter pushes them at once, so a new agent's card
// appears with its first row; later changes to the card wait for the
// quiet-point refresh. The child's identity and position decide; its rev
// does not, so a streaming row pushed blanked is judged the same way.
func (s *Store) ListFirstChildWireAnchors(child Item) ([]Item, error) {
	if child.ParentID == "" {
		return nil, nil
	}
	return readSnapshot(s.reader(), "first child anchors", func(q sqlQueryer) ([]Item, error) {
		ids, err := subagentAnchorIDs(q, firstChildAnchorsSQL,
			child.ThreadID, child.ParentID, child.ID, child.TurnIndex, child.ItemIndex)
		if err != nil {
			return nil, fmt.Errorf("store: select first child anchors of %s/%s: %w", child.ThreadID, child.ID, err)
		}
		if len(ids) == 0 {
			return nil, nil
		}
		return s.listWireItemsTx(q, child.ThreadID, ids)
	})
}

// ListWireItems reads the named rows exactly as a page would: hydrated
// and decorated, in one read transaction, so each row's content and its
// `rev` describe one snapshot. Missing ids are simply absent from the
// result. Rows come back in timeline order, which keeps a parent ahead
// of its children on the wire (the client admits a child in the same
// batch only behind its parent).
func (s *Store) ListWireItems(threadID string, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return []Item{}, nil
	}
	return readSnapshot(s.reader(), "wire items", func(q sqlQueryer) ([]Item, error) {
		return s.listWireItemsTx(q, threadID, ids)
	})
}

func (s *Store) listWireItemsTx(q sqlQueryer, threadID string, ids []string) ([]Item, error) {
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	selectedSQL, selectedArgs := timelineIDSelection(threadID, timelineSelection{
		Where:     "items.id IN (" + placeholders(len(ids)) + ")",
		WhereArgs: args,
	})
	return s.querySelectedPagedItems(q, threadID, selectedSQL, selectedArgs...)
}

// ListWireItemsBehind returns, as a page would read them now, every row
// whose read result the writes of the given rows changed and whose
// client copy is behind. emitted maps each written row's id to the
// revision it was last pushed at.
//
// The candidate set is the one the history triggers stamped for each
// write (stampedRowIDsSQL), plus the launch a completion sibling settles
// (backgroundSettleTriggersSQL rewrites the launch's meta on a completion
// insert, and the update trigger stamps it from there). A written row is
// returned only when its stored revision has moved past the pushed one:
// a sibling write stamped it after its own push. A written row that no
// longer exists contributes nothing; its parent's copy then costs the
// client a page, never a false fresh.
//
// This is the emit-side half of the per-row revision contract for rows
// that change without being written (§3.1): the trigger moves the
// anchor's `rev`, and this read is what lets the emitter push the anchor
// at that revision so a client's held window can still verify.
func (s *Store) ListWireItemsBehind(threadID string, emitted map[string]int64) ([]Item, error) {
	if len(emitted) == 0 {
		return []Item{}, nil
	}
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin behind wire item read for %s: %w", threadID, err)
	}
	defer tx.Rollback()

	candidates := make(map[string]struct{})
	current := make(map[string]int64, len(emitted))
	for id := range emitted {
		var parentID, completionOf string
		var rev int64
		err := tx.QueryRow(
			`SELECT parent_id, completion_of, rev FROM timeline_items WHERE thread_id = ? AND id = ?`,
			threadID, id,
		).Scan(&parentID, &completionOf, &rev)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: resolve written row %s/%s: %w", threadID, id, err)
		}
		current[id] = rev
		if err := collectStampedRowIDs(tx, threadID, id, parentID, candidates); err != nil {
			return nil, err
		}
		if completionOf == "" {
			continue
		}
		var launchParent string
		err = tx.QueryRow(
			`SELECT parent_id FROM items WHERE thread_id = ? AND id = ?`,
			threadID, completionOf,
		).Scan(&launchParent)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: resolve launch %s/%s: %w", threadID, completionOf, err)
		}
		if err := collectStampedRowIDs(tx, threadID, completionOf, launchParent, candidates); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		if pushed, written := emitted[id]; written {
			if rev, exists := current[id]; !exists || rev == pushed {
				continue
			}
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []Item{}, nil
	}
	return s.listWireItemsTx(tx, threadID, ids)
}

// stampedRowIDsByParamsSQL is stampedRowIDsSQL with the written row's
// coordinates as parameters: the one statement the triggers and
// ListWireItemsBehind share (TestStampedRowIDsByParamsProbesIndexes pins
// its plan).
var stampedRowIDsByParamsSQL = stampedRowIDsFor("?1", "?2", "?3")

func collectStampedRowIDs(q sqlQueryer, threadID, itemID, parentID string, into map[string]struct{}) error {
	rows, err := q.Query(stampedRowIDsByParamsSQL, threadID, itemID, parentID)
	if err != nil {
		return fmt.Errorf("store: select rows stamped by %s/%s: %w", threadID, itemID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("store: scan row stamped by %s/%s: %w", threadID, itemID, err)
		}
		into[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate rows stamped by %s/%s: %w", threadID, itemID, err)
	}
	return nil
}
