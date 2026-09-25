package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Walked anchors (docs/architecture/sqlite-store.md#pointer-forks).
//
// A pointer fork serves an inherited anchor from the stamp of the lineage
// level that holds it when that stamp describes the rows the fork shows
// under it (inheritedStampReads): the stamp is clean, every row it counts
// sits below the fork's cut at that level, and no marker names the
// anchor. A level's stamp counts the rows that level shows. A thread that
// shows other rows than a level it reads under one of that level's
// anchors records the anchor in thread_fork_walked: its reads, and the
// reads of every thread that reads it through its lineage, walk the
// anchor (decorateSubagentAnchors), as they walk an imported one. The
// writers:
//
//   - fork creation, for the rows it hides (linkPointerForkTx);
//   - a thread's copy of rows another thread holds (snapshotRowsTx): a
//     settled copy differs from its original, and a later write changes
//     the copy alone;
//   - a fork's hide of one inherited row (hideInheritedItemTx);
//   - an item write or import batch (cardWrite.finish) that puts a counted
//     row under a parent the thread does not hold, or below the cut of a
//     reader that hides it (trg_items_fork_snapshot): the reader shows
//     neither the row nor the stamps of the thread's anchors above it that
//     count it;
//   - a holder that takes or copies rows whose parent it does not take
//     (markStraddledAnchorsTx): the thread that keeps the parent
//     recomputes its stamps without the rows the holder took, and the
//     holder's readers read both parts.
//
// Each records every anchorable ancestor of a row whose parent is not
// among the rows it changed, since an anchor's card counts the rows below
// it transitively, but for the thread's own anchors, which its own
// recompute keeps. A fork whose revert leaves a reader reading more of an
// ancestor than it does records its own stamped copies
// (markNarrowedCopiesTx). A marker is only ever added: a stale one costs
// its readers a walk. The foreign key removes a thread's markers with it,
// and a retired thread keeps them for its readers (holderKeptTables).
// Migration v132 recorded the markers of every thread written before.

// rowParent is a row's id and its parent's.
type rowParent struct{ id, parent string }

// rowParentsTx reads the parents of the rows ids names in viewer's
// timeline, each row by key on every arm. Ids viewer does not show are
// absent.
func rowParentsTx(tx *sql.Tx, viewer string, ids []string) ([]rowParent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	query, args, err := timelineArms(tx, viewer, timelineSelection{
		Columns:   func(_, _ string) string { return "items.id, items.parent_id" },
		KeyFirst:  true,
		Where:     "items.id IN (SELECT value FROM json_each(?))",
		WhereArgs: []any{list},
	})
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read the parents of rows in %s: %w", viewer, err)
	}
	var out []rowParent
	for rows.Next() {
		var row rowParent
		if err := rows.Scan(&row.id, &row.parent); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan a row parent in %s: %w", viewer, err), rows.Close())
		}
		out = append(out, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: read the parents of rows in %s: %w", viewer, err)
	}
	return out, nil
}

// markRowAnchorsTx records in marker's markers the anchors above the rows
// ids names in viewer's timeline (markWalkedAncestorsTx).
func markRowAnchorsTx(tx *sql.Tx, marker, viewer string, ids []string) error {
	rows, err := rowParentsTx(tx, viewer, ids)
	if err != nil {
		return err
	}
	return markWalkedAncestorsTx(tx, marker, viewer, rows)
}

// markWalkedAncestorsTx records in marker's markers the anchorable
// ancestors, read through viewer's timeline, of each of rows whose parent
// is not among rows. An anchor marker holds itself is passed over: its
// stamp is marker's to keep. The walk goes up one level of parents per
// statement, through as many levels as a card's chain (subagentChainTx),
// and stops above a row no card counts (visibleItemsFilterFor), as the
// descendant walk does.
func markWalkedAncestorsTx(tx *sql.Tx, marker, viewer string, rows []rowParent) error {
	changed := make(map[string]bool, len(rows))
	for _, row := range rows {
		changed[row.id] = true
	}
	var frontier []string
	for _, row := range rows {
		if row.parent != "" && !changed[row.parent] {
			frontier = append(frontier, row.parent)
		}
	}
	if len(frontier) == 0 {
		return nil
	}
	depth, err := forkLineageDepth(tx, viewer)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	var anchors []string
	for hops := 0; len(frontier) > 0 && hops < 64; hops++ {
		slices.Sort(frontier)
		frontier = slices.Compact(frontier)
		for _, id := range frontier {
			seen[id] = true
		}
		list, err := jsonList(frontier)
		if err != nil {
			return err
		}
		query, args := renderTimelineArms(viewer, depth, timelineSelection{
			Columns: func(_, rev string) string {
				return "items.id, items.parent_id, items.kind, items.tool_name, " +
					visibleItemsFilterFor("items.") + ", " + rev + " >= 0"
			},
			KeyFirst:  true,
			Where:     "items.id IN (SELECT value FROM json_each(?))",
			WhereArgs: []any{list},
		})
		found, err := tx.Query(query, args...)
		if err != nil {
			return fmt.Errorf("store: read the ancestors of rows in %s: %w", viewer, err)
		}
		frontier = nil
		for found.Next() {
			var id, parent, kind, tool string
			var visible, local bool
			if err := found.Scan(&id, &parent, &kind, &tool, &visible, &local); err != nil {
				return errors.Join(fmt.Errorf("store: scan an ancestor in %s: %w", viewer, err), found.Close())
			}
			if SubagentAnchorable(kind, tool) && !(local && viewer == marker) {
				anchors = append(anchors, id)
			}
			if visible && parent != "" && !seen[parent] {
				frontier = append(frontier, parent)
			}
		}
		if err := errors.Join(found.Err(), found.Close()); err != nil {
			return fmt.Errorf("store: read the ancestors of rows in %s: %w", viewer, err)
		}
	}
	return insertWalkedTx(tx, marker, anchors)
}

// insertWalkedTx records ids in marker's markers.
func insertWalkedTx(tx *sql.Tx, marker string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO thread_fork_walked (thread_id, item_id)
		SELECT ?, value FROM json_each(?)`, marker, list); err != nil {
		return fmt.Errorf("store: record the anchors %s's readers walk: %w", marker, err)
	}
	return nil
}

// markStraddledAnchorsTx records in holder's markers the anchors of
// threadID, the thread its readers read right after it, above the rows
// ids the holder just took or copied from it whose parent it did not
// take: threadID's stamps of those anchors no longer count the rows the
// readers read from the holder. A parent the holder took before is not
// in threadID's timeline: the stamp the holder keeps for it counted, when
// it moved, every row then under it that its readers show.
func markStraddledAnchorsTx(tx *sql.Tx, holder, threadID string, ids []string) error {
	rows, err := rowParentsTx(tx, holder, ids)
	if err != nil {
		return err
	}
	return markWalkedAncestorsTx(tx, holder, threadID, rows)
}

// widerReaderSQL is true while a thread that reads ?1 reads a level behind
// its level on ?1 through more rows than ?1 does: ?1 reads that ancestor
// through a lower cut, or not at all. A fork's revert lowers its own cuts
// and leaves its readers' (splitShownRowsTx lowers only their cut on the
// fork), and an unlinked fork reads no ancestor.
const widerReaderSQL = `SELECT EXISTS (
  SELECT 1 FROM thread_fork_lineage r
   CROSS JOIN thread_fork_lineage behind ON behind.thread_id = r.thread_id AND behind.depth > r.depth
   WHERE r.ancestor_id = ?1
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage own
                      WHERE own.thread_id = ?1 AND own.ancestor_id = behind.ancestor_id
                        AND (own.cut_turn_index, own.cut_item_index) >= (behind.cut_turn_index, behind.cut_item_index)))`

// markNarrowedCopiesTx records in threadID's markers its copies with a
// stamp (forkCopyStampsTx, read before the change) once threadID shows
// fewer inherited rows than a thread that reads it: the copies' stamps
// count the rows threadID shows under them, and the reader shows the rows
// threadID stopped showing too.
func markNarrowedCopiesTx(tx *sql.Tx, threadID string, copies []string) error {
	if len(copies) == 0 {
		return nil
	}
	var wider bool
	if err := tx.QueryRow(widerReaderSQL, threadID).Scan(&wider); err != nil {
		return fmt.Errorf("store: compare the readers of %s with its lineage: %w", threadID, err)
	}
	if !wider {
		return nil
	}
	return insertWalkedTx(tx, threadID, copies)
}

// placedRow is a counted row an item write inserted, moved or made
// counted: its parent and its position.
type placedRow struct {
	ID     string `json:"id"`
	Parent string `json:"parent"`
	Turn   int    `json:"turn"`
	Item   int    `json:"item"`
}

// placedAnchorsSQL lists, for the rows the JSON array ?2 names that
// thread ?1 placed, the threads that show other rows under the row's
// parent than the level holding it counts: ?1 when it is a fork (?3) and
// holds no row of the parent, and each reader of ?1 (?4) that does not
// show a row ?1 placed below its cut: it or a nearer level hides the id
// (trg_items_fork_snapshot). Keyed probes of the thread's rows, of the
// readers by idx_thread_fork_lineage_ancestor and of their hides.
var placedAnchorsSQL = `WITH placed(id, parent, turn_index, item_index) AS (
    SELECT json_extract(value, '$.id'), json_extract(value, '$.parent'),
           json_extract(value, '$.turn'), json_extract(value, '$.item')
      FROM json_each(?2))
SELECT ?1, placed.id, placed.parent FROM placed
 WHERE ?3 AND NOT EXISTS (SELECT 1 FROM items own WHERE own.thread_id = ?1 AND own.id = placed.parent)
UNION ALL
SELECT l.thread_id, placed.id, placed.parent
  FROM placed CROSS JOIN thread_fork_lineage l
 WHERE ?4 AND l.ancestor_id = ?1
   AND (placed.turn_index, placed.item_index) < (l.cut_turn_index, l.cut_item_index)
   AND NOT (` + strings.ReplaceAll(inheritedItemNotHiddenSQL, "items.id", "placed.id") + `)`

// markPlacedAnchorsTx records the markers the counted rows an item write
// of threadID placed call for (placedAnchorsSQL). A thread that neither
// reads a lineage nor has a reader costs one probe of each.
func markPlacedAnchorsTx(tx *sql.Tx, threadID string, placed []placedRow) error {
	if len(placed) == 0 {
		return nil
	}
	var fork, read bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_fork_lineage WHERE thread_id = ?1),
		       EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = ?1)`, threadID).Scan(&fork, &read); err != nil {
		return fmt.Errorf("store: probe the lineage of %s: %w", threadID, err)
	}
	if !fork && !read {
		return nil
	}
	encoded, err := json.Marshal(placed)
	if err != nil {
		return fmt.Errorf("store: encode the rows %s placed: %w", threadID, err)
	}
	rows, err := tx.Query(placedAnchorsSQL, threadID, string(encoded), fork, read)
	if err != nil {
		return fmt.Errorf("store: read the threads the rows %s placed change: %w", threadID, err)
	}
	byMarker := make(map[string][]rowParent)
	var markers []string
	for rows.Next() {
		var marker string
		var row rowParent
		if err := rows.Scan(&marker, &row.id, &row.parent); err != nil {
			return errors.Join(fmt.Errorf("store: scan a row %s placed: %w", threadID, err), rows.Close())
		}
		if _, ok := byMarker[marker]; !ok {
			markers = append(markers, marker)
		}
		byMarker[marker] = append(byMarker[marker], row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("store: read the threads the rows %s placed change: %w", threadID, err)
	}
	for _, marker := range markers {
		if err := markWalkedAncestorsTx(tx, marker, threadID, byMarker[marker]); err != nil {
			return err
		}
	}
	return nil
}
