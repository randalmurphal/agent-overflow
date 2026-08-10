package store

import (
	"context"
	"path/filepath"
	"testing"
)

func seedSyncThread(t *testing.T, s *Store, threadID string, items int) {
	t.Helper()
	seedContractThread(t, s, threadID)
	for i := 0; i < items; i++ {
		item := contractItem(threadID, threadID+"-i"+itoa(i), i)
		item.Summary = "row " + itoa(i)
		if _, err := s.AppendItem(item); err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestSyncThreadWindowStatuses walks the four answers of design §5 over
// one thread, as a sequence rather than four independent fixtures: the
// same client stamp that reads `fresh` must read `stale` after an
// in-place write and `rewritten` after a cut.
func TestSyncThreadWindowStatuses(t *testing.T) {
	s := newTestStore(t)
	seedSyncThread(t, s, "t", 4)
	ctx := context.Background()

	identity, err := s.Identity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	held := historyStampOf(t, s, "t")

	// fresh — stamps match, no page.
	got, err := s.SyncThreadWindow(ctx, "t", "", 200, held)
	if err != nil {
		t.Fatalf("sync (fresh): %v", err)
	}
	if got.Status != SyncFresh {
		t.Fatalf("status = %q, want fresh", got.Status)
	}
	if got.Page != nil {
		t.Fatalf("fresh answer carried a page of %d items", len(got.Page.Items))
	}
	if got.Stamp != held {
		t.Fatalf("stamp = %+v, want %+v", got.Stamp, held)
	}
	if got.Generation != identity.ReplicaGeneration {
		t.Fatalf("generation = %q, want %q", got.Generation, identity.ReplicaGeneration)
	}

	// stale — an in-place content write moves rev only.
	if err := s.UpdateItemMeta("t", "t-i0", `{"x":1}`); err != nil {
		t.Fatalf("update item meta: %v", err)
	}
	got, err = s.SyncThreadWindow(ctx, "t", "", 200, held)
	if err != nil {
		t.Fatalf("sync (stale): %v", err)
	}
	if got.Status != SyncStale {
		t.Fatalf("status = %q, want stale", got.Status)
	}
	if got.Page == nil {
		t.Fatal("stale answer must carry the window")
	}
	window, err := s.ListThreadSliceAround("t", "", 200)
	if err != nil {
		t.Fatalf("list thread slice around: %v", err)
	}
	if len(got.Page.Items) != len(window.Items) {
		t.Fatalf("stale page has %d items, want the slice-around window's %d",
			len(got.Page.Items), len(window.Items))
	}
	for i := range window.Items {
		if got.Page.Items[i].ID != window.Items[i].ID {
			t.Fatalf("stale page item %d = %q, want %q", i, got.Page.Items[i].ID, window.Items[i].ID)
		}
	}
	if got.Page.OldestCursor != window.OldestCursor || got.Page.NewestCursor != window.NewestCursor {
		t.Fatalf("stale page cursors %+v/%+v, want %+v/%+v",
			got.Page.OldestCursor, got.Page.NewestCursor, window.OldestCursor, window.NewestCursor)
	}
	if got.Stamp == held {
		t.Fatal("stale answer returned the caller's stamps unchanged")
	}

	// rewritten — a cut moves epoch, and the old stamp can no longer be
	// graded as merely stale.
	if _, _, err := s.DeleteConversationFromItem("t", "t-i3"); err != nil {
		t.Fatalf("delete conversation from item: %v", err)
	}
	got, err = s.SyncThreadWindow(ctx, "t", "", 200, held)
	if err != nil {
		t.Fatalf("sync (rewritten): %v", err)
	}
	if got.Status != SyncRewritten {
		t.Fatalf("status = %q, want rewritten", got.Status)
	}
	if got.Page == nil {
		t.Fatal("rewritten answer must carry the window")
	}

	// A caller holding no replica at all is never told "fresh".
	got, err = s.SyncThreadWindow(ctx, "t", "", 200, UnknownHistoryStamp())
	if err != nil {
		t.Fatalf("sync (no replica): %v", err)
	}
	if got.Status == SyncFresh {
		t.Fatal("a caller with no replica must never be answered fresh")
	}
	if got.Page == nil {
		t.Fatal("a caller with no replica must receive the window")
	}

	// gone — no thread row.
	got, err = s.SyncThreadWindow(ctx, "no-such-thread", "", 200, held)
	if err != nil {
		t.Fatalf("sync (gone): %v", err)
	}
	if got.Status != SyncGone {
		t.Fatalf("status = %q, want gone", got.Status)
	}
	if got.Page != nil {
		t.Fatal("gone answer must not carry a page")
	}
	if got.Generation != identity.ReplicaGeneration {
		t.Fatalf("gone answer generation = %q, want %q", got.Generation, identity.ReplicaGeneration)
	}
}

// TestSyncThreadWindowHonorsAnchor pins that the window is positioned by
// the caller's anchor exactly as ListThreadSliceAround positions it — the
// sync RPC is that read plus the stamps, never a different read.
func TestSyncThreadWindowHonorsAnchor(t *testing.T) {
	s := newTestStore(t)
	seedSyncThread(t, s, "t", 12)

	got, err := s.SyncThreadWindow(context.Background(), "t", "t-i2", 4, UnknownHistoryStamp())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got.Page == nil {
		t.Fatal("expected a page")
	}
	want, err := s.ListThreadSliceAround("t", "t-i2", 4)
	if err != nil {
		t.Fatalf("list thread slice around: %v", err)
	}
	if len(got.Page.Items) != len(want.Items) {
		t.Fatalf("anchored page has %d items, want %d", len(got.Page.Items), len(want.Items))
	}
	for i := range want.Items {
		if got.Page.Items[i].ID != want.Items[i].ID {
			t.Fatalf("anchored page item %d = %q, want %q", i, got.Page.Items[i].ID, want.Items[i].ID)
		}
	}
	if got.Page.HasMoreOlder != want.HasMoreOlder || got.Page.HasMoreNewer != want.HasMoreNewer {
		t.Fatalf("has-more flags = (%v, %v), want (%v, %v)",
			got.Page.HasMoreOlder, got.Page.HasMoreNewer, want.HasMoreOlder, want.HasMoreNewer)
	}
}

// TestSyncThreadWindowSameTxAttestation is the load-bearing property of
// §9: the stamps a sync answer carries describe EXACTLY the rows it
// carries. It reproduces SyncThreadWindow's own statement sequence
// against a transaction the test controls, and commits a writer's insert
// in the middle of it. Under WAL the read transaction keeps its snapshot,
// so the window must show the pre-write rows — never the newer stamp over
// older rows, which a client would record as fresh and never correct.
func TestSyncThreadWindowSameTxAttestation(t *testing.T) {
	s := newTestStore(t)
	seedSyncThread(t, s, "t", 3)
	if s.read == nil {
		t.Skip("no read pool: reads and writes share the writer connection")
	}

	tx, err := s.read.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer tx.Rollback()

	// Statement 1 of SyncThreadWindow: the stamps. This is what pins the
	// WAL snapshot for the rest of the transaction.
	stamp, found, err := readHistoryStampTx(tx, "t")
	if err != nil || !found {
		t.Fatalf("read stamps in tx: err=%v found=%v", err, found)
	}

	// A concurrent writer commits between the two reads.
	if _, err := s.AppendItem(contractItem("t", "t-late", 99)); err != nil {
		t.Fatalf("concurrent append: %v", err)
	}
	committed := historyStampOf(t, s, "t")
	if committed.Rev <= stamp.Rev {
		t.Fatalf("fixture did not advance rev: %d -> %d", stamp.Rev, committed.Rev)
	}

	// Statement 2: the window, still on the snapshot the stamps came from.
	page, err := s.listThreadSliceAround(tx, "t", "", 200)
	if err != nil {
		t.Fatalf("window in tx: %v", err)
	}
	for _, item := range page.Items {
		if item.ID == "t-late" {
			t.Fatal("window escaped its snapshot: it shows a row committed after the stamps were read")
		}
	}
	if len(page.Items) != 3 {
		t.Fatalf("window has %d items, want the 3 the stamps attest", len(page.Items))
	}
}

// TestRestoreFromRemintsReplicaGeneration — a restore rewinds every
// thread's counters to the snapshot's values, so clients holding stamps
// from the replaced future must be invalidated by something the counters
// cannot express. The generation is that something; backend_id is not,
// and must survive.
func TestRestoreFromRemintsReplicaGeneration(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "before snapshot")

	snapshotIdentity, err := st.Identity()
	if err != nil {
		t.Fatalf("identity before snapshot: %v", err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	// Diverge: more history lands, so the live counters run ahead of the
	// snapshot's — exactly the state a client's stamps would describe.
	if err := st.InsertItem(Item{
		ID: "t1-i2", ThreadID: "t1", TurnIndex: 1, ItemIndex: 1,
		Kind: "assistant_text", Role: "assistant", Summary: "after",
		CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	diverged := historyStampOf(t, st, "t1")

	returned, err := st.RestoreFrom(snap)
	if err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	restored, err := st.Identity()
	if err != nil {
		t.Fatalf("identity after restore: %v", err)
	}
	// The returned identity is what callers republish to the transport
	// manifest; if it drifted from what the store now holds, the client
	// invalidation circuit would re-open exactly where it was soldered.
	if returned != restored {
		t.Fatalf("RestoreFrom returned %+v but the store holds %+v", returned, restored)
	}
	if restored.ReplicaGeneration == snapshotIdentity.ReplicaGeneration {
		t.Fatal("restore kept the replica generation; clients holding rewound stamps would never be invalidated")
	}
	if restored.ReplicaGeneration == "" {
		t.Fatal("restore left the replica generation empty")
	}
	if restored.BackendID != snapshotIdentity.BackendID {
		t.Fatalf("backend id changed across restore: %q -> %q", snapshotIdentity.BackendID, restored.BackendID)
	}

	// The counters themselves are free to rewind — that is precisely why
	// the generation exists.
	after := historyStampOf(t, st, "t1")
	if after.Rev >= diverged.Rev {
		t.Fatalf("restore did not rewind history_rev (%d -> %d); the fixture no longer covers the case",
			diverged.Rev, after.Rev)
	}

	// And the store still answers sync calls with the new generation.
	got, err := st.SyncThreadWindow(context.Background(), "t1", "", 200, UnknownHistoryStamp())
	if err != nil {
		t.Fatalf("sync after restore: %v", err)
	}
	if got.Generation != restored.ReplicaGeneration {
		t.Fatalf("sync generation = %q, want the re-minted %q", got.Generation, restored.ReplicaGeneration)
	}
}

// TestRestoreFromKeepsThisStoresBackendID — a snapshot is minted by
// whatever store produced it, and the harness restores recordings taken
// on OTHER databases. `store_meta` is an ordinary user table, so the
// row copy would hand this store the snapshot's backend id: two stores
// claiming one identity, and every replica keyed to the real id orphaned.
// The fixture uses a genuinely foreign snapshot on purpose — a
// self-snapshot cannot fail this, because the id it restores is already
// the right one.
func TestRestoreFromKeepsThisStoresBackendID(t *testing.T) {
	source := snapshotTestStore(t)
	seedSnapshotFixture(t, source, "t1", "from the other store")
	sourceIdentity, err := source.Identity()
	if err != nil {
		t.Fatalf("source identity: %v", err)
	}
	snap := filepath.Join(t.TempDir(), "foreign.db")
	if err := source.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	live := snapshotTestStore(t)
	liveIdentity, err := live.Identity()
	if err != nil {
		t.Fatalf("live identity: %v", err)
	}
	if liveIdentity.BackendID == sourceIdentity.BackendID {
		t.Fatal("fixture is broken: the two stores minted the same backend id")
	}

	restored, err := live.RestoreFrom(snap)
	if err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	if restored.BackendID != liveIdentity.BackendID {
		t.Fatalf("backend id after restore = %q, want this store's %q (snapshot carried %q)",
			restored.BackendID, liveIdentity.BackendID, sourceIdentity.BackendID)
	}
	if restored.ReplicaGeneration == sourceIdentity.ReplicaGeneration ||
		restored.ReplicaGeneration == liveIdentity.ReplicaGeneration {
		t.Fatalf("replica generation %q was not re-minted (source %q, live %q)",
			restored.ReplicaGeneration, sourceIdentity.ReplicaGeneration, liveIdentity.ReplicaGeneration)
	}
	// The stored row must agree with what the caller republishes.
	held, err := live.Identity()
	if err != nil {
		t.Fatalf("identity after restore: %v", err)
	}
	if held != restored {
		t.Fatalf("RestoreFrom returned %+v but the store holds %+v", restored, held)
	}
	// And the snapshot's rows did land — this is a restore, not a no-op.
	threads, err := live.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != "t1" {
		t.Fatalf("threads after restore = %+v, want the snapshot's t1", threads)
	}
}

// TestRestoreFromKeepsHistoryTriggersLive — the restore drops the three
// item triggers for the duration of the row copy (so the copy's counters
// are the snapshot's by construction, not by table ordering) and
// recreates them from the same const migration v55 installs. A restore
// that forgot the second half would leave the store silently unable to
// invalidate any client for the rest of the file's life.
func TestRestoreFromKeepsHistoryTriggersLive(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "x")
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	for _, trigger := range []string{
		"trg_items_rev_insert", "trg_items_rev_update", "trg_items_rev_delete",
	} {
		var found string
		if err := st.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?`, trigger,
		).Scan(&found); err != nil {
			t.Fatalf("trigger %s missing after restore: %v", trigger, err)
		}
	}

	// Existing is not enough — they must still FIRE.
	before := historyStampOf(t, st, "t1")
	if err := st.InsertItem(Item{
		ID: "post-restore", ThreadID: "t1", TurnIndex: 1, ItemIndex: 5,
		Kind: "assistant_text", Role: "assistant", Summary: "after",
		CreatedAt: 9, UpdatedAt: 9,
	}); err != nil {
		t.Fatalf("InsertItem after restore: %v", err)
	}
	afterInsert := historyStampOf(t, st, "t1")
	if afterInsert.Rev-before.Rev != 1 || afterInsert.Epoch != before.Epoch {
		t.Fatalf("insert after restore: (rev, epoch) delta = (%d, %d), want (1, 0)",
			afterInsert.Rev-before.Rev, afterInsert.Epoch-before.Epoch)
	}
	if err := st.DeleteThreadItem("t1", "post-restore"); err != nil {
		t.Fatalf("DeleteThreadItem after restore: %v", err)
	}
	afterDelete := historyStampOf(t, st, "t1")
	if afterDelete.Rev-afterInsert.Rev != 1 || afterDelete.Epoch-afterInsert.Epoch != 1 {
		t.Fatalf("delete after restore: (rev, epoch) delta = (%d, %d), want (1, 1)",
			afterDelete.Rev-afterInsert.Rev, afterDelete.Epoch-afterInsert.Epoch)
	}
}

// TestRestoreFromLandsTheSnapshotsCounters — with the triggers dropped
// for the copy, a restored thread's stamps are EXACTLY the snapshot's,
// rather than the snapshot's plus whatever per-row trigger arithmetic the
// copy happened to produce. That is what makes a restored store's stamps
// mean the same thing as the snapshot's did.
func TestRestoreFromLandsTheSnapshotsCounters(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "x")
	// More rows, so the copy has something to fire triggers over.
	for i := 0; i < 3; i++ {
		if err := st.InsertItem(Item{
			ID: "t1-extra-" + itoa(i), ThreadID: "t1", TurnIndex: 1, ItemIndex: 10 + i,
			Kind: "assistant_text", Role: "assistant", Summary: "row",
			CreatedAt: 2, UpdatedAt: 2,
		}); err != nil {
			t.Fatalf("InsertItem %d: %v", i, err)
		}
	}
	atSnapshot := historyStampOf(t, st, "t1")

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	if err := st.DeleteThreadItem("t1", "t1-extra-0"); err != nil {
		t.Fatalf("diverge: %v", err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	if got := historyStampOf(t, st, "t1"); got != atSnapshot {
		t.Fatalf("restored stamps = %+v, want the snapshot's %+v", got, atSnapshot)
	}
}

// TestSyncThreadWindowZeroStampMatchesOnlyAnUntouchedThread is the read-
// path half of migration v55's `UPDATE threads SET history_rev = 1`.
// (0, 0) is the JSON zero value of the request's stamp pair, so a client
// that sends no stamps asks exactly this question — and it must never be
// answered `fresh` over a thread that has history.
func TestSyncThreadWindowZeroStampMatchesOnlyAnUntouchedThread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedSyncThread(t, s, "with-history", 3)
	mustCreateThread(t, s, "untouched")

	zero := HistoryStamp{}
	got, err := s.SyncThreadWindow(ctx, "with-history", "", 200, zero)
	if err != nil {
		t.Fatalf("sync (with history): %v", err)
	}
	if got.Status == SyncFresh {
		t.Fatal("a zero stamp read as fresh over a thread with history")
	}
	if got.Page == nil {
		t.Fatal("a non-fresh answer must carry the window")
	}

	// A thread with no item writes since v55 genuinely IS at (0, 0), and
	// a page-less fresh is the truthful answer to its empty window.
	got, err = s.SyncThreadWindow(ctx, "untouched", "", 200, zero)
	if err != nil {
		t.Fatalf("sync (untouched): %v", err)
	}
	if got.Status != SyncFresh {
		t.Fatalf("untouched thread status = %q, want fresh", got.Status)
	}
	if got.Stamp != zero {
		t.Fatalf("untouched thread stamp = %+v, want the zero pair", got.Stamp)
	}
}

// TestIdentityIsStableAcrossReopen — backend_id keys the client's replica
// database, so a restart that re-minted it would orphan every cached
// window on disk.
func TestIdentityIsStableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	first, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before, err := first.Identity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { second.Close() })
	after, err := second.Identity()
	if err != nil {
		t.Fatalf("identity after reopen: %v", err)
	}
	if after != before {
		t.Fatalf("identity changed across reopen: %+v -> %+v", before, after)
	}
}

// TestSyncThreadWindowAttestsUnderConcurrentWrites drives the REAL
// SyncThreadWindow against a live writer, pinning the rule
// internal/store/AGENTS.md marks non-negotiable: stamps and page come
// from one transaction. The fixture makes attestation checkable from
// the outside — every append advances rev by exactly one (the v55
// insert trigger) and adds exactly one visible row, so for every
// response, page length MUST equal seeded + (rev - base). A
// SyncThreadWindow split into two transactions fails this under load;
// the hand-rolled statement-sequence test above would not notice.
func TestSyncThreadWindowAttestsUnderConcurrentWrites(t *testing.T) {
	s := newTestStore(t)
	const seeded = 3
	seedSyncThread(t, s, "t", seeded)
	base := historyStampOf(t, s, "t")

	const writes = 40
	writerDone := make(chan error, 1)
	go func() {
		for i := 0; i < writes; i++ {
			item := contractItem("t", "w-"+itoa(i), 100+i)
			if _, err := s.AppendItem(item); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	for {
		sync, err := s.SyncThreadWindow(context.Background(), "t", "", 200, UnknownHistoryStamp())
		if err != nil {
			t.Fatalf("sync during writes: %v", err)
		}
		if sync.Page == nil {
			t.Fatal("unknown stamp must always return a page")
		}
		wantRows := seeded + int(sync.Stamp.Rev-base.Rev)
		if len(sync.Page.Items) != wantRows {
			t.Fatalf("stamps and page disagree: rev %d attests %d rows, page has %d",
				sync.Stamp.Rev, wantRows, len(sync.Page.Items))
		}
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatalf("writer: %v", err)
			}
			// One final read over the settled store.
			final, err := s.SyncThreadWindow(context.Background(), "t", "", 200, UnknownHistoryStamp())
			if err != nil {
				t.Fatalf("final sync: %v", err)
			}
			if got := len(final.Page.Items); got != seeded+writes {
				t.Fatalf("final page has %d rows, want %d", got, seeded+writes)
			}
			return
		default:
		}
	}
}

// TestEnsureStoreIdentityHealsAMissingRow — losing store_meta (a restore
// from an ancient snapshot, hand surgery) must cost every client its
// replica, not the store its ability to identify itself. Open-time
// INSERT OR IGNORE re-mints a full identity; a present row is untouched.
func TestEnsureStoreIdentityHealsAMissingRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.sqlite")
	s1, err := New(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	before, err := s1.Identity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if _, err := s1.db.Exec(`DELETE FROM store_meta`); err != nil {
		t.Fatalf("drop identity row: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatalf("reopen after losing store_meta: %v", err)
	}
	defer s2.Close()
	healed, err := s2.Identity()
	if err != nil {
		t.Fatalf("identity after heal: %v", err)
	}
	if healed.BackendID == "" || healed.ReplicaGeneration == "" {
		t.Fatalf("healed identity is incomplete: %+v", healed)
	}
	if healed.BackendID == before.BackendID {
		t.Fatal("heal reused the lost backend id; a fresh identity must drop every replica keyed to the old one")
	}
}
