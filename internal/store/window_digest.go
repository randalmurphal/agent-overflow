package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf16"
)

// UnstampedItemRev is the revision an emitter puts on a wire row whose
// content it deliberately altered, so the row is NOT a copy of what a read
// returns. The streaming reveal does exactly that: it persists a block's
// text and then pushes the row with the summary blanked, because the client
// animates the same text as deltas.
//
// A client folds those events into a row it holds, and the held row only
// becomes a copy of the stored row once the settle patch lands (which
// carries the real revision). Until then it must not be able to prove
// freshness, and a negative revision cannot match any stamp, so a window
// containing one always pages. Emitting the persisted revision beside
// altered content would be the one way to earn a FALSE `fresh`.
const UnstampedItemRev int64 = -1

// MaxHeldWindowItems is the largest held window SyncThreadWindow will
// verify. It matches the app layer's window budget cap: a window bigger
// than a page could ever be did not come from a page, and verifying one
// would let an unintended LAN-attached caller spend the read pool on an
// arbitrarily long range scan. Beyond the cap the answer is a page, which
// is bounded by the same number.
const MaxHeldWindowItems = 2000

// HeldWindow is the caller's description of the rows it already holds for
// a thread (docs/architecture/thread-replica-sync.md §5). It is the
// evidence behind the second route to a page-less `fresh`: the stamps did
// not match, but these rows are still exactly what a read would return.
//
// It describes rows, never content: the edges and the count bound the
// range, and Digest folds the (id, rev) pairs inside it. The server never
// trusts any of it on its own — every field is re-derived from the
// database inside the sync transaction and compared.
type HeldWindow struct {
	// OldestItemID / NewestItemID are the window's inclusive edges in
	// (turn_index, item_index) order. Both must be visible top-level rows
	// of the thread; a window whose edge has been deleted or re-parented
	// is not a window any more.
	OldestItemID string `json:"oldestItemId"`
	NewestItemID string `json:"newestItemId"`
	// Count is how many visible top-level rows the caller holds between
	// the edges, inclusive. It is checked before the digest so a wrong
	// count cannot be hidden by a hash collision, and it bounds the work
	// the verification query is allowed to do.
	Count int `json:"count"`
	// HasMoreOlder / HasMoreNewer are the caller's belief about history
	// outside the edges. They ride the window because they drive the
	// "Load older messages" affordance: a window that is row-identical but
	// wrong about its ends still renders wrongly.
	HasMoreOlder bool `json:"hasMoreOlder"`
	HasMoreNewer bool `json:"hasMoreNewer"`
	// Digest is WindowDigest over the held rows, 16 lowercase hex chars.
	Digest string `json:"digest"`
}

// WindowDigestRow is one (id, rev) pair in a window digest. The tags are
// the shared fixture's field names (testdata/window_digest_vectors.json),
// which the TypeScript copy reads under the same keys.
type WindowDigestRow struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev"`
}

// Window digest framing bytes. Unit separator between a row's id and its
// rev, record separator after each row, so no concatenation of ids and
// decimal digits can produce another window's canonical string. Both are
// outside the id alphabet (entity ids are ASCII words, ':' and '-').
const (
	windowDigestFieldSep = 0x1f
	windowDigestRowSep   = 0x1e
)

// FNV-1a 64 parameters. The fold is written out rather than taken from
// hash/fnv because the canonical string is hashed as UTF-16 code units,
// not bytes: that is the unit the TypeScript side reads without an
// allocation, and folding the same units here makes the contract hold for
// every id rather than only for ASCII ones.
const (
	windowDigestOffset uint64 = 0xcbf29ce484222325
	windowDigestPrime  uint64 = 0x100000001b3
)

// WindowDigest folds an ordered run of timeline rows into the 16-hex-char
// digest a client sends with a held window.
//
// The canonical string is `id + 0x1f + decimal rev + 0x1e` per row, in
// (turn_index, item_index) order, hashed with FNV-1a 64 over its UTF-16
// code units. The shared fixture at testdata/window_digest_vectors.json is
// the executable half of this contract; the TypeScript implementation
// (frontend/src/lib/utils/fnv1a.ts, framed by
// frontend/src/lib/stores/threadWindowDigest.ts) reads a byte-identical
// copy.
//
// The empty run has a digest (the FNV offset basis rendered as hex) and it
// is never confusable with a real one, because every non-empty run appends
// at least one separator.
func WindowDigest(rows []WindowDigestRow) string {
	h := windowDigestOffset
	unit := func(u uint16) {
		h = (h ^ uint64(u)) * windowDigestPrime
	}
	fold := func(s string) {
		for _, r := range s {
			if r >= 0x10000 {
				hi, lo := utf16.EncodeRune(r)
				unit(uint16(hi))
				unit(uint16(lo))
				continue
			}
			unit(uint16(r))
		}
	}
	var buf [20]byte
	for _, row := range rows {
		fold(row.ID)
		unit(windowDigestFieldSep)
		fold(string(strconv.AppendInt(buf[:0], row.Rev, 10)))
		unit(windowDigestRowSep)
	}
	return fmt.Sprintf("%016x", h)
}

// verifyHeldWindowTx reports whether the rows a caller says it holds are
// still exactly what a windowed read of the thread would return, using the
// caller's transaction so the answer describes the same WAL snapshot as
// the stamps beside it.
//
// Every check is a refusal to trust the request:
//
//   - the window's SHAPE must be one a page could have produced
//     (heldWindowHasUsableShape), which is decided before any statement
//     runs so a malformed request costs no database work;
//   - the edges must both resolve to visible top-level rows of THIS thread
//     and be in order, or the range is meaningless;
//   - the rows inside the range, under the same filter and order every
//     page uses, must number exactly Count and fold to Digest, which is
//     what makes "the same rows, unchanged" a proof rather than a claim;
//   - the has-more probes outside the edges must agree, because a window
//     with the right rows and the wrong ends renders the wrong affordance;
//   - no row may be imported history: those carry no per-row stamp
//     (importedItemRevExpr), so their content could have changed under a
//     local payload overlay with nothing in the digest to show it.
//
// Anything that fails returns false, which costs one page — the same
// answer the caller would have got without sending a window at all.
func verifyHeldWindowTx(q sqlQueryer, threadID string, held HeldWindow) (bool, error) {
	if !heldWindowHasUsableShape(held) {
		return false, nil
	}
	oldest, found, err := windowEdgeCursorTx(q, threadID, held.OldestItemID)
	if err != nil || !found {
		return false, err
	}
	newest, found, err := windowEdgeCursorTx(q, threadID, held.NewestItemID)
	if err != nil || !found {
		return false, err
	}
	if newest.TurnIndex < oldest.TurnIndex ||
		(newest.TurnIndex == oldest.TurnIndex && newest.ItemIndex < oldest.ItemIndex) {
		return false, nil
	}

	rows, err := windowDigestRowsTx(q, threadID, oldest, newest, held.Count+1)
	if err != nil {
		return false, err
	}
	if len(rows) != held.Count {
		return false, nil
	}
	for _, row := range rows {
		if row.Rev < 0 {
			return false, nil
		}
	}
	if WindowDigest(rows) != held.Digest {
		return false, nil
	}

	hasOlder, err := hasOlderItems(q, threadID, oldest)
	if err != nil {
		return false, err
	}
	if hasOlder != held.HasMoreOlder {
		return false, nil
	}
	hasNewer, err := hasNewerItems(q, threadID, newest)
	if err != nil {
		return false, err
	}
	return hasNewer == held.HasMoreNewer, nil
}

// maxHeldWindowIDBytes bounds an edge id a caller may send. Entity ids
// are short generated strings; the bound mirrors the frontend's own id
// cap and exists so a hostile request cannot make the edge lookups carry
// arbitrarily long keys.
const maxHeldWindowIDBytes = 512

// heldWindowHasUsableShape rejects a window that cannot be the product of
// a page before any statement runs. Every field here is caller-supplied
// and unauthenticated on a LAN-attached connection, so a malformed one is
// answered with a page rather than with database work: a count outside
// the page cap, a digest that is not this algorithm's 16 lowercase hex
// characters, or an edge id that is empty or longer than any id the store
// mints could only ever fail the checks below.
func heldWindowHasUsableShape(held HeldWindow) bool {
	if held.Count <= 0 || held.Count > MaxHeldWindowItems {
		return false
	}
	if len(held.Digest) != 16 {
		return false
	}
	for i := 0; i < len(held.Digest); i++ {
		c := held.Digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	for _, id := range []string{held.OldestItemID, held.NewestItemID} {
		if id == "" || len(id) > maxHeldWindowIDBytes {
			return false
		}
	}
	return true
}

// windowEdgeCursorTx resolves one edge id to its timeline coordinate,
// admitting only the rows a window can contain. A single-row lookup is the
// one shape the compound `timeline_items` view is right for.
func windowEdgeCursorTx(q sqlQueryer, threadID, itemID string) (TimelineCursor, bool, error) {
	if itemID == "" {
		return TimelineCursor{}, false, nil
	}
	cursor := TimelineCursor{ItemID: itemID}
	err := q.QueryRow(
		`SELECT turn_index, item_index FROM timeline_items
		  WHERE thread_id = ? AND id = ?
		    AND `+visibleItemsFilter+`
		    AND `+topLevelItemsFilter,
		threadID, itemID,
	).Scan(&cursor.TurnIndex, &cursor.ItemIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return TimelineCursor{}, false, nil
	}
	if err != nil {
		return TimelineCursor{}, false, fmt.Errorf(
			"store: resolve held window edge %s/%s: %w", threadID, itemID, err)
	}
	return cursor, true, nil
}

// windowDigestRowsTx selects the (id, rev) pairs inside an inclusive
// coordinate range, under the same filter and ordering a page uses, so the
// digest describes the rows a page WOULD contain. `limit` is the caller's
// claimed count plus one: one extra row is enough to prove the count wrong
// and stops a lying request from scanning the thread.
//
// It selects two narrow columns off the ordering index and never joins
// payloads: this runs on a cold open where the point is to ship no rows at
// all.
//
// The limit is validated HERE rather than trusted from the caller: it is
// the only bound on how much of a thread this statement may walk, and
// timelineArms renders a non-positive limit as no LIMIT clause at all.
func windowDigestRowsTx(
	q sqlQueryer,
	threadID string,
	oldest, newest TimelineCursor,
	limit int,
) ([]WindowDigestRow, error) {
	if limit <= 0 || limit > MaxHeldWindowItems+1 {
		return nil, fmt.Errorf(
			"store: held window row limit %d for %s is outside 1..%d",
			limit, threadID, MaxHeldWindowItems+1)
	}
	selection, args := timelineArms(threadID, timelineSelection{
		Columns: func(_, revExpr string) string {
			return `items.id AS id, ` + revExpr + ` AS rev,
			        items.turn_index AS turn_index, items.item_index AS item_index`
		},
		Where: windowedTimelineFilter + `
		   AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index >= ?))
		   AND (items.turn_index < ? OR (items.turn_index = ? AND items.item_index <= ?))`,
		WhereArgs: []any{
			oldest.TurnIndex, oldest.TurnIndex, oldest.ItemIndex,
			newest.TurnIndex, newest.TurnIndex, newest.ItemIndex,
		},
		OrderBy: "turn_index ASC, item_index ASC",
		Limit:   limit,
	})
	rows, err := q.Query("SELECT id, rev FROM (\n"+selection+"\n)", args...)
	if err != nil {
		return nil, fmt.Errorf("store: read held window rows for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := make([]WindowDigestRow, 0, limit)
	for rows.Next() {
		var row WindowDigestRow
		if err := rows.Scan(&row.ID, &row.Rev); err != nil {
			return nil, fmt.Errorf("store: scan held window row for %s: %w", threadID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate held window rows for %s: %w", threadID, err)
	}
	return out, nil
}
