package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

func TestNormalizeThreadWindowSyncErrorClassifiesOnlyExpiredContexts(t *testing.T) {
	storeErr := errors.New("sql: Rows are closed")

	ordinary := normalizeThreadWindowSyncError(context.Background(), storeErr)
	if errors.Is(ordinary, transport.ErrTemporarilyUnavailable) {
		t.Fatalf("ordinary store error was mislabeled transient: %v", ordinary)
	}
	if !errors.Is(ordinary, storeErr) {
		t.Fatalf("ordinary store error lost its cause: %v", ordinary)
	}
	canceled, cancelCanceled := context.WithCancel(context.Background())
	cancelCanceled()
	canceledErr := normalizeThreadWindowSyncError(canceled, storeErr)
	if errors.Is(canceledErr, transport.ErrTemporarilyUnavailable) {
		t.Fatalf("canceled sync error was mislabeled as a timeout: %v", canceledErr)
	}

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	transient := normalizeThreadWindowSyncError(expired, storeErr)
	if !errors.Is(transient, transport.ErrTemporarilyUnavailable) {
		t.Fatalf("expired sync error = %v, want transient classification", transient)
	}
	if !errors.Is(transient, context.DeadlineExceeded) {
		t.Fatalf("expired sync error lost its context cause: %v", transient)
	}
	if errors.Is(transient, storeErr) {
		t.Fatal("driver artifact survived after the context cause explained the failure")
	}
}

func appHistoryStamp(t *testing.T, a *App, threadID string) store.HistoryStamp {
	t.Helper()
	stamp, found, err := a.store.ThreadHistoryStamp(threadID)
	if err != nil {
		t.Fatalf("thread history stamp: %v", err)
	}
	if !found {
		t.Fatalf("thread %s has no row", threadID)
	}
	return stamp
}

// seedSyncBindingThread builds a four-row idle thread on the standard
// app fixture (which poisons provider spawns and detaches HOME — nothing
// here starts a session).
func seedSyncBindingThread(t *testing.T, a *App) store.Thread {
	t.Helper()
	thread, err := createTestThread(t, a, "claude", t.TempDir(), "", "chat")
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertUserItem(t, a.store, thread.ID, "user:0", 0, "first")
	insertAssistantTextItem(t, a.store, thread.ID, "asst:0", 0, "reply 0")
	insertUserItem(t, a.store, thread.ID, "user:1", 1, "second")
	insertAssistantTextItem(t, a.store, thread.ID, "asst:1", 1, "reply 1")
	return thread
}

// TestSyncThreadWindowBindingAnswersOverASequence drives the binding
// through the transitions a real client makes with ONE held stamp:
// current, then behind an in-place write, then behind a cut. The
// per-status store behaviour has its own contract test; what this pins is
// the wire projection — that "fresh" really is the page-less ~100-byte
// answer the design exists for, and that a page, when sent, arrives with
// an allocated Items slice so the client decodes `[]` rather than `null`.
func TestSyncThreadWindowBindingAnswersOverASequence(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedSyncBindingThread(t, app)
	held := appHistoryStamp(t, app, thread.ID)

	current := SyncThreadWindowRequest{HaveRev: held.Rev, HaveEpoch: held.Epoch}
	got, err := app.SyncThreadWindow(thread.ID, current)
	if err != nil {
		t.Fatalf("sync (fresh): %v", err)
	}
	if got.Status != string(store.SyncFresh) {
		t.Fatalf("status = %q, want fresh", got.Status)
	}
	if got.Page != nil {
		t.Fatalf("fresh answer carried a page of %d items", len(got.Page.Items))
	}
	if got.Rev != held.Rev || got.Epoch != held.Epoch {
		t.Fatalf("stamps = (%d, %d), want (%d, %d)", got.Rev, got.Epoch, held.Rev, held.Epoch)
	}
	if got.Generation == "" {
		t.Fatal("answer carried no replica generation: a client cannot tell a restore happened")
	}
	generation := got.Generation

	if err := app.store.UpdateItemMeta(thread.ID, "asst:1", `{"x":1}`); err != nil {
		t.Fatalf("update item meta: %v", err)
	}
	got, err = app.SyncThreadWindow(thread.ID, current)
	if err != nil {
		t.Fatalf("sync (stale): %v", err)
	}
	if got.Status != string(store.SyncStale) {
		t.Fatalf("status = %q, want stale", got.Status)
	}
	if got.Page == nil {
		t.Fatal("stale answer must carry the window")
	}
	if got.Page.Items == nil {
		t.Fatal("page.Items is nil: it would serialize as null, not []")
	}
	if len(got.Page.Items) != 4 {
		t.Fatalf("stale page has %d items, want 4", len(got.Page.Items))
	}
	if got.Rev <= held.Rev {
		t.Fatalf("stale answer rev = %d, want > the caller's %d", got.Rev, held.Rev)
	}
	if got.Epoch != held.Epoch {
		t.Fatalf("an in-place write moved epoch: %d -> %d", held.Epoch, got.Epoch)
	}

	if _, _, err := app.store.DeleteConversationFromItem(thread.ID, "user:1"); err != nil {
		t.Fatalf("delete conversation from item: %v", err)
	}
	got, err = app.SyncThreadWindow(thread.ID, current)
	if err != nil {
		t.Fatalf("sync (rewritten): %v", err)
	}
	if got.Status != string(store.SyncRewritten) {
		t.Fatalf("status = %q, want rewritten", got.Status)
	}
	if got.Page == nil || len(got.Page.Items) != 2 {
		t.Fatalf("rewritten answer must carry the post-cut window, got %+v", got.Page)
	}
	if got.Generation != generation {
		t.Fatalf("generation changed without a restore: %q -> %q", generation, got.Generation)
	}

	got, err = app.SyncThreadWindow("no-such-thread", current)
	if err != nil {
		t.Fatalf("sync (gone): %v", err)
	}
	if got.Status != string(store.SyncGone) {
		t.Fatalf("status = %q, want gone", got.Status)
	}
	if got.Page != nil {
		t.Fatal("gone answer must not carry a page")
	}
}

// TestSyncThreadWindowBindingNormalizesItemBudget — the request field is
// client-supplied, so a nonsense value must land on the same bounded
// window ListThreadSliceAround would return rather than reaching SQLite
// as a negative LIMIT.
func TestSyncThreadWindowBindingNormalizesItemBudget(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedSyncBindingThread(t, app)

	want, err := app.ListThreadSliceAround(thread.ID, "", 0)
	if err != nil {
		t.Fatalf("list thread slice around: %v", err)
	}
	for _, budget := range []int{0, -1, maxWindowItems + 1} {
		got, err := app.SyncThreadWindow(thread.ID, SyncThreadWindowRequest{
			ItemBudget: budget,
			HaveRev:    store.UnknownStamp,
			HaveEpoch:  store.UnknownStamp,
		})
		if err != nil {
			t.Fatalf("sync (budget %d): %v", budget, err)
		}
		if got.Page == nil {
			t.Fatalf("budget %d: a caller holding no replica must receive the window", budget)
		}
		if len(got.Page.Items) != len(want.Items) {
			t.Fatalf("budget %d returned %d items, want the default window's %d",
				budget, len(got.Page.Items), len(want.Items))
		}
	}
}

func TestClampSliceItemBudget(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{in: 0, want: sliceAroundDefaultItems},
		{in: -1, want: sliceAroundDefaultItems},
		{in: 1, want: 1},
		{in: maxWindowItems, want: maxWindowItems},
		{in: maxWindowItems + 1, want: maxWindowItems},
	} {
		if got := clampSliceItemBudget(tc.in); got != tc.want {
			t.Fatalf("clampSliceItemBudget(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestInterruptRevertEventCarriesPostCutStamps — the un-send path writes
// nothing to items after the cut, so the event's stamps must be exactly
// the store's. A client that adopted a PRE-cut stamp here would keep the
// truncated rows in its replica forever, since no later write mentions
// them.
func TestInterruptRevertEventCarriesPostCutStamps(t *testing.T) {
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	bus := newCapturedEventBus()
	app.testEmitHook = bus.emit

	thread := createAppTestThread(t, app, "revert-stamps", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "the original prompt")
	before := appHistoryStamp(t, app, thread.ID)

	result, err := app.InterruptAndRevertIfClean(thread.ID)
	if err != nil {
		t.Fatalf("interrupt-and-revert: %v", err)
	}
	if !result.Reverted {
		t.Fatalf("expected Reverted=true, got reason=%q", result.Reason)
	}

	after := appHistoryStamp(t, app, thread.ID)
	if after.Epoch <= before.Epoch {
		t.Fatalf("fixture cut nothing: epoch %d -> %d", before.Epoch, after.Epoch)
	}
	_, ev := findRevertedEvent(t, bus)
	if ev.HistoryRev != after.Rev || ev.HistoryEpoch != after.Epoch {
		t.Fatalf("event stamps = (%d, %d), want the post-cut (%d, %d)",
			ev.HistoryRev, ev.HistoryEpoch, after.Rev, after.Epoch)
	}
}

// TestRevertAndResendEventCarriesPostCutStamps is the same contract on
// the edit-and-resend path, where the saga appends the replacement
// message right behind the event. The stamps must describe the CUT (so
// the client drops the truncated rows) and may lag the replacement row —
// which arrives as its own item event. Understating is the safe
// direction; the epoch, which is what forces the client to re-read, must
// be exact.
func TestRevertAndResendEventCarriesPostCutStamps(t *testing.T) {
	app, bus := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-stamps")
	before := appHistoryStamp(t, app, thread.ID)

	if err := app.RevertConversationAndResendMessage(thread.ID, "user:1",
		RevertAndResendOptions{Content: "rewritten second"}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	after := appHistoryStamp(t, app, thread.ID)
	_, ev := findRevertedEvent(t, bus)
	if ev.HistoryEpoch <= before.Epoch {
		t.Fatalf("event epoch = %d, want above the pre-cut %d", ev.HistoryEpoch, before.Epoch)
	}
	if ev.HistoryEpoch != after.Epoch {
		t.Fatalf("event epoch = %d, want the post-cut %d (a resend appends, it does not reposition)",
			ev.HistoryEpoch, after.Epoch)
	}
	if ev.HistoryRev > after.Rev {
		t.Fatalf("event rev = %d, ahead of the store's %d", ev.HistoryRev, after.Rev)
	}
	if ev.HistoryRev < before.Rev {
		t.Fatalf("event rev = %d, below the pre-cut %d: the cut itself bumps rev",
			ev.HistoryRev, before.Rev)
	}
}
