package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const (
	goWindowDigestVectorsPath = "testdata/window_digest_vectors.json"
	tsWindowDigestVectorsPath = "../../frontend/src/test/fixtures/windowDigestVectors.json"
)

type windowDigestVectorFile struct {
	Algorithm string `json:"algorithm"`
	// MaxHeldWindowItems is the page cap both sides enforce. It rides the
	// shared fixture because a client that trims its held window to a
	// different number than the server verifies would silently pay a page
	// on every reopen, which no digest case would catch.
	MaxHeldWindowItems int `json:"maxHeldWindowItems"`
	Cases              []struct {
		Name   string            `json:"name"`
		Rows   []WindowDigestRow `json:"rows"`
		Digest string            `json:"digest"`
	} `json:"cases"`
}

// TestWindowDigestMatchesSharedVectors is one half of a cross-language
// contract: a digest a client computes in TypeScript has to equal the one
// the server computes in Go, or every held window would be refused. The
// vectors are the executable statement of the canonical form, so a change
// to either implementation that is not a change to both fails here.
func TestWindowDigestMatchesSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(goWindowDigestVectorsPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var file windowDigestVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if file.Algorithm != "fnv1a64" {
		t.Fatalf("vector algorithm = %q, want fnv1a64", file.Algorithm)
	}
	if len(file.Cases) == 0 {
		t.Fatal("vector file carries no cases")
	}
	if file.MaxHeldWindowItems != MaxHeldWindowItems {
		t.Errorf("vector maxHeldWindowItems = %d, want %d",
			file.MaxHeldWindowItems, MaxHeldWindowItems)
	}
	for _, c := range file.Cases {
		if got := WindowDigest(c.Rows); got != c.Digest {
			t.Errorf("digest(%s) = %s, want %s", c.Name, got, c.Digest)
		}
	}
}

// TestWindowDigestVectorsAreSharedByteForByte keeps the two copies one
// file. The frontend test suite cannot read out of internal/, so the
// fixture is duplicated; a duplicate nobody compares is a fixture that
// drifts and takes the contract with it.
func TestWindowDigestVectorsAreSharedByteForByte(t *testing.T) {
	goCopy, err := os.ReadFile(goWindowDigestVectorsPath)
	if err != nil {
		t.Fatalf("read Go vectors: %v", err)
	}
	tsCopy, err := os.ReadFile(tsWindowDigestVectorsPath)
	if err != nil {
		t.Fatalf("read TypeScript vectors: %v", err)
	}
	if string(goCopy) != string(tsCopy) {
		t.Fatalf("%s and %s differ; they must be byte-identical",
			goWindowDigestVectorsPath, tsWindowDigestVectorsPath)
	}
}

// heldWindowFromStore builds the window a client that had just read this
// thread would hold, so the verification tests start from a window that
// IS the read and then break exactly one thing about it.
func heldWindowFromStore(t *testing.T, s *Store, threadID string) HeldWindow {
	t.Helper()
	page, err := s.ListThreadSliceAround(threadID, "", 200)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("window is empty")
	}
	rows := make([]WindowDigestRow, 0, len(page.Items))
	for _, item := range page.Items {
		rows = append(rows, WindowDigestRow{ID: item.ID, Rev: item.Rev})
	}
	return HeldWindow{
		OldestItemID: page.Items[0].ID,
		NewestItemID: page.Items[len(page.Items)-1].ID,
		Count:        len(page.Items),
		HasMoreOlder: page.HasMoreOlder,
		HasMoreNewer: page.HasMoreNewer,
		Digest:       WindowDigest(rows),
	}
}

// TestSyncThreadWindowVerifiesHeldWindow is the feature: a client whose
// stamp is stale — which is every client that had the thread open while
// anything was written — still gets a page-less `fresh` when the rows it
// holds are the rows a read would return.
//
// The write that makes the stamp stale is a plan_update notification,
// because it is a real one: the frontend renders those out of band and
// paging.go filters them out, so the window genuinely did not change
// while the thread counter did.
func TestSyncThreadWindowVerifiesHeldWindow(t *testing.T) {
	s := newTestStore(t)
	seedSyncThread(t, s, "t", 4)
	ctx := context.Background()

	held := heldWindowFromStore(t, s, "t")
	stale := historyStampOf(t, s, "t")

	notification := contractItem("t", "plan-note", 9)
	notification.Kind = "notification"
	notification.Role = "system"
	notification.ToolName = "plan_update"
	if err := s.InsertItem(notification); err != nil {
		t.Fatalf("insert plan_update notification: %v", err)
	}
	if now := historyStampOf(t, s, "t"); now == stale {
		t.Fatal("fixture no longer makes the client stamp stale")
	}

	got, err := s.SyncThreadWindow(ctx, "t", "", 200, stale, &held)
	if err != nil {
		t.Fatalf("sync with held window: %v", err)
	}
	if got.Status != SyncFresh {
		t.Fatalf("status = %q, want fresh", got.Status)
	}
	if got.Page != nil {
		t.Fatalf("fresh answer carried a page of %d items", len(got.Page.Items))
	}
	if want := historyStampOf(t, s, "t"); got.Stamp != want {
		t.Fatalf("stamp = %+v, want the current %+v", got.Stamp, want)
	}

	// Without the window, the same request pays for a page.
	got, err = s.SyncThreadWindow(ctx, "t", "", 200, stale, nil)
	if err != nil {
		t.Fatalf("sync without held window: %v", err)
	}
	if got.Status == SyncFresh || got.Page == nil {
		t.Fatalf("status = %q page=%v, want a paged answer", got.Status, got.Page != nil)
	}
}

// TestSyncThreadWindowRejectsWrongHeldWindows walks every way a window can
// stop being the read. Each case earns a page, which is the same answer
// the client would have got for sending nothing — the failure mode this
// guards is the opposite one, a window accepted while the rows moved on.
func TestSyncThreadWindowRejectsWrongHeldWindows(t *testing.T) {
	cases := []struct {
		name string
		// mutate breaks the window, the store, or both. It runs after the
		// window has been captured from a clean read.
		mutate func(t *testing.T, s *Store, held *HeldWindow)
	}{
		{
			name: "count disagrees",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.Count--
			},
		},
		{
			name: "digest disagrees",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.Digest = "0000000000000000"
			},
		},
		{
			name: "a held row's content changed",
			mutate: func(t *testing.T, s *Store, _ *HeldWindow) {
				if err := s.UpdateItemMeta("t", "t-i2", `{"changed":true}`); err != nil {
					t.Fatalf("update row: %v", err)
				}
			},
		},
		{
			name: "a payload the window renders changed",
			mutate: func(t *testing.T, s *Store, _ *HeldWindow) {
				if err := s.AppendPayloadData("t", "pay", []byte(" more"), "{}", 4000); err != nil {
					t.Fatalf("append payload: %v", err)
				}
			},
		},
		{
			name: "the oldest edge is gone",
			mutate: func(t *testing.T, s *Store, held *HeldWindow) {
				held.OldestItemID = "no-such-row"
			},
		},
		{
			name: "the newest edge is gone",
			mutate: func(t *testing.T, s *Store, held *HeldWindow) {
				if err := s.DeleteThreadItem("t", held.NewestItemID); err != nil {
					t.Fatalf("delete newest: %v", err)
				}
			},
		},
		{
			name: "the edges are inverted",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.OldestItemID, held.NewestItemID = held.NewestItemID, held.OldestItemID
			},
		},
		{
			name: "an edge is not a top-level row",
			mutate: func(t *testing.T, s *Store, held *HeldWindow) {
				child := contractItem("t", "child-edge", 9)
				child.ParentID = "t-i0"
				if err := s.InsertItem(child); err != nil {
					t.Fatalf("insert child: %v", err)
				}
				held.NewestItemID = "child-edge"
			},
		},
		{
			name: "has-more-older disagrees",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.HasMoreOlder = !held.HasMoreOlder
			},
		},
		{
			name: "has-more-newer disagrees",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.HasMoreNewer = !held.HasMoreNewer
			},
		},
		{
			name: "a visible row appeared between the edges",
			mutate: func(t *testing.T, s *Store, _ *HeldWindow) {
				// Between t-i1 and t-i2, so both edges still resolve and
				// only the interior differs.
				inserted := contractItem("t", "wedged", 5)
				if _, err := s.db.Exec(
					`UPDATE items SET item_index = item_index + 10
					  WHERE thread_id = 't' AND item_index >= 2`,
				); err != nil {
					t.Fatalf("make room: %v", err)
				}
				if err := s.InsertItem(inserted); err != nil {
					t.Fatalf("insert wedged row: %v", err)
				}
			},
		},
		{
			name: "the window is larger than a page can be",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.Count = MaxHeldWindowItems + 1
			},
		},
		{
			name: "the window claims no rows",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.Count = 0
			},
		},
		{
			// An id is only an id WITHIN a thread: an edge naming a row
			// that exists, but in another thread, is not an edge of this
			// window.
			name: "an edge names a row of a different thread",
			mutate: func(t *testing.T, s *Store, held *HeldWindow) {
				seedSyncThread(t, s, "other", 2)
				held.OldestItemID = "other-i0"
			},
		},
		{
			// plan_update notifications are filtered out of every page, so
			// one can never be an edge — the same filter windowEdgeCursorTx
			// applies.
			name: "an edge is a plan_update notification",
			mutate: func(t *testing.T, s *Store, held *HeldWindow) {
				held.NewestItemID = "plan-note"
			},
		},
		{
			name: "the digest is not a digest",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.Digest = "XYZ"
			},
		},
		{
			name: "the digest is the right characters at the wrong length",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.Digest = "abcdef0123456789ab"
			},
		},
		{
			// The canonical form is lowercase; an uppercase copy could
			// never equal a computed digest, so it is refused on shape.
			name: "the digest is uppercase hex",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.Digest = "ABCDEF0123456789"
			},
		},
		{
			name: "an edge id is empty",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.OldestItemID = ""
			},
		},
		{
			name: "an edge id is longer than any id the store mints",
			mutate: func(_ *testing.T, _ *Store, held *HeldWindow) {
				held.NewestItemID = strings.Repeat("x", maxHeldWindowIDBytes+1)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedSyncThread(t, s, "t", 4)
			// One row carries a payload so the payload case has something
			// to change; it rides every case so the fixtures stay one shape.
			item := contractItem("t", "t-i1", 1)
			item.PayloadID = "pay"
			if _, err := s.UpsertItem(item, &Payload{
				ID: "pay", Kind: "text", Meta: "{}", Data: []byte("base"), CreatedAt: 1000,
			}); err != nil {
				t.Fatalf("attach payload: %v", err)
			}

			held := heldWindowFromStore(t, s, "t")
			stale := historyStampOf(t, s, "t")
			// Every case runs with a stale stamp: a matching one answers
			// `fresh` before the window is looked at, so it would hide
			// whether the window itself was accepted. A plan_update
			// notification moves the counter without changing a window row.
			notification := contractItem("t", "plan-note", 20)
			notification.Kind = "notification"
			notification.Role = "system"
			notification.ToolName = "plan_update"
			if err := s.InsertItem(notification); err != nil {
				t.Fatalf("insert plan_update notification: %v", err)
			}
			tc.mutate(t, s, &held)

			got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, stale, &held)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if got.Status == SyncFresh {
				t.Fatalf("status = fresh over a window that is no longer the read")
			}
			if got.Page == nil {
				t.Fatal("non-fresh answer carried no page")
			}
		})
	}
}

// seedWideSyncThread writes `items` rows straight through SQL in one
// transaction. The cap tests need thousands of rows and the accessor path
// would spend the whole test budget on round trips; the rows only have to
// be visible top-level rows with distinct stamps, which the triggers give
// them.
func seedWideSyncThread(t *testing.T, s *Store, threadID string, items int) {
	t.Helper()
	seedContractThread(t, s, threadID)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, summary,
		 parent_id, is_background, completion_of, tool_name, decision, meta,
		 created_at, updated_at)
		VALUES (?, ?, 0, ?, 'assistant_text', 'assistant', 'completed', 'row',
		 '', 0, '', '', '', '{}', 1000, 1000)`)
	if err != nil {
		t.Fatalf("prepare seed insert: %v", err)
	}
	defer stmt.Close()
	for i := 0; i < items; i++ {
		if _, err := stmt.Exec(threadID+"-i"+itoa(i), threadID, i); err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// heldWindowOverItems builds the window a client holding exactly these
// rows would send.
func heldWindowOverItems(items []Item, hasOlder, hasNewer bool) HeldWindow {
	rows := make([]WindowDigestRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, WindowDigestRow{ID: item.ID, Rev: item.Rev})
	}
	return HeldWindow{
		OldestItemID: items[0].ID,
		NewestItemID: items[len(items)-1].ID,
		Count:        len(items),
		HasMoreOlder: hasOlder,
		HasMoreNewer: hasNewer,
		Digest:       WindowDigest(rows),
	}
}

// TestHeldWindowVerifiesExactlyUpToThePageCap pins both sides of
// MaxHeldWindowItems. The cap is the only bound on how much of a thread a
// verification may walk, so it has to hold at the boundary: a window of
// exactly the cap is a window a page could have produced and verifies,
// and one row more is refused before any statement runs rather than
// scanning a thread on an unauthenticated caller's say-so.
func TestHeldWindowVerifiesExactlyUpToThePageCap(t *testing.T) {
	s := newTestStore(t)
	seedWideSyncThread(t, s, "t", MaxHeldWindowItems+1)

	page, err := s.ListThreadSliceAround("t", "", MaxHeldWindowItems+2)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if len(page.Items) != MaxHeldWindowItems+1 {
		t.Fatalf("read %d rows, want %d", len(page.Items), MaxHeldWindowItems+1)
	}
	stale := historyStampOf(t, s, "t")
	stale.Rev--

	atCap := heldWindowOverItems(page.Items[:MaxHeldWindowItems], false, true)
	got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, stale, &atCap)
	if err != nil {
		t.Fatalf("sync at the cap: %v", err)
	}
	if got.Status != SyncFresh || got.Page != nil {
		t.Fatalf("a window of exactly %d rows: status = %q page=%v, want a page-less fresh",
			MaxHeldWindowItems, got.Status, got.Page != nil)
	}

	overCap := heldWindowOverItems(page.Items, false, false)
	got, err = s.SyncThreadWindow(context.Background(), "t", "", 200, stale, &overCap)
	if err != nil {
		t.Fatalf("sync over the cap: %v", err)
	}
	if got.Status == SyncFresh {
		t.Fatalf("verified a window of %d rows, past the %d cap",
			len(page.Items), MaxHeldWindowItems)
	}
	if got.Page == nil {
		t.Fatal("the refused window got no page")
	}
}

// TestHeldWindowIgnoresRowsAPageWouldNotReturn pins the filter the digest
// shares with every page read. A subagent child sits between the edges in
// coordinate order and is not a window row, so a client that never saw it
// still describes the window exactly.
func TestHeldWindowIgnoresRowsAPageWouldNotReturn(t *testing.T) {
	s := newTestStore(t)
	seedSyncThread(t, s, "t", 4)

	child := contractItem("t", "child", 10)
	child.ParentID = "t-i1"
	if err := s.InsertItem(child); err != nil {
		t.Fatalf("insert child: %v", err)
	}

	held := heldWindowFromStore(t, s, "t")
	if held.Count != 4 {
		t.Fatalf("held window counted %d rows, want the 4 top-level ones", held.Count)
	}
	stale := historyStampOf(t, s, "t")

	// A write the window cannot render: the stamp moves, the rows do not.
	notification := contractItem("t", "plan-note", 11)
	notification.Kind = "notification"
	notification.Role = "system"
	notification.ToolName = "plan_update"
	if err := s.InsertItem(notification); err != nil {
		t.Fatalf("insert plan_update notification: %v", err)
	}

	got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, stale, &held)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got.Status != SyncFresh || got.Page != nil {
		t.Fatalf("status = %q page=%v, want a page-less fresh", got.Status, got.Page != nil)
	}
}

// TestHeldWindowSeesThroughToChildWrites is the other side of the same
// filter: a child row is not a window row, but a write to one changes what
// the window renders (decorateSubagentAnchors summarises it onto the
// anchor), so the anchor's stamp moves and the window must page.
func TestHeldWindowSeesThroughToChildWrites(t *testing.T) {
	s := newTestStore(t)
	seedSyncThread(t, s, "t", 4)

	held := heldWindowFromStore(t, s, "t")
	stale := historyStampOf(t, s, "t")

	child := contractItem("t", "child", 10)
	child.ParentID = "t-i1"
	if err := s.InsertItem(child); err != nil {
		t.Fatalf("insert child: %v", err)
	}

	got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, stale, &held)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got.Status == SyncFresh {
		t.Fatal("verified a window whose anchor gained a child")
	}
}

// TestHeldWindowRefusesImportedRows is the limit of the digest. Imported
// history carries no per-row stamp, so a window holding one cannot be
// proven current and must page.
//
// It cannot carry one: an imported row's read result changes without its
// chunk reference changing. A payload mutator copies the imported payload
// into the thread's local overlay (ensureLocalPayloadTx) and the imported
// arm hydrates the local copy first, and a local child parented to an
// imported tool_call changes that anchor's decorated meta. Neither write
// has an `items` row to stamp, so any chunk-derived revision would hold
// still while the wire row changed — a false `fresh`. -1 is the honest
// answer; see importedItemRevExpr.
func TestHeldWindowRefusesImportedRows(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	imported := contractItem("t", "imported", 0)
	imported.TurnIndex = 1
	if err := s.ApplyImportBatch("t", ImportBatch{
		Turns: []Turn{{TurnID: "t:1", ThreadID: "t", TurnIndex: 1, StartedAt: 1000}},
		Rows:  []ImportRow{{Item: imported}},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	if _, err := s.AppendItem(contractItem("t", "local", 0)); err != nil {
		t.Fatalf("append local row: %v", err)
	}

	held := heldWindowFromStore(t, s, "t")
	stale := historyStampOf(t, s, "t")
	stale.Rev--

	got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, stale, &held)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got.Status == SyncFresh {
		t.Fatal("verified a window containing an unstampable imported row")
	}
}

// TestHeldWindowVerifiesLocalTailOfImportedThread bounds the cost of that
// refusal, which is the reason it is acceptable. A thread with imported
// history is not permanently unverifiable: only a window that actually
// CONTAINS imported rows is refused. The window a reader holds after a
// turn on an imported thread is its live tail, and that verifies.
func TestHeldWindowVerifiesLocalTailOfImportedThread(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	imported := contractItem("t", "imported", 0)
	imported.TurnIndex = 1
	if err := s.ApplyImportBatch("t", ImportBatch{
		Turns: []Turn{{TurnID: "t:1", ThreadID: "t", TurnIndex: 1, StartedAt: 1000}},
		Rows:  []ImportRow{{Item: imported}},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	for _, id := range []string{"local-1", "local-2"} {
		row := contractItem("t", id, 0)
		row.TurnIndex = 2
		if _, err := s.AppendItem(row); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	page, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	tail := page.Items[len(page.Items)-2:]
	if tail[0].ID != "local-1" || tail[1].ID != "local-2" {
		t.Fatalf("tail is %s,%s, want the two local rows", tail[0].ID, tail[1].ID)
	}
	rows := []WindowDigestRow{{ID: tail[0].ID, Rev: tail[0].Rev}, {ID: tail[1].ID, Rev: tail[1].Rev}}
	held := HeldWindow{
		OldestItemID: tail[0].ID,
		NewestItemID: tail[1].ID,
		Count:        2,
		HasMoreOlder: true,
		HasMoreNewer: false,
		Digest:       WindowDigest(rows),
	}

	stale := historyStampOf(t, s, "t")
	stale.Rev--

	got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, stale, &held)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got.Status != SyncFresh || got.Page != nil {
		t.Fatalf("status = %q page=%v, want a page-less fresh over the local tail", got.Status, got.Page != nil)
	}
}
