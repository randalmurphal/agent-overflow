package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

func durableQueueRows(t *testing.T, app *App, threadID string) []store.FlushQueueItem {
	t.Helper()
	rows, err := app.store.ListFlushQueueItems(threadID)
	if err != nil {
		t.Fatalf("ListFlushQueueItems: %v", err)
	}
	return rows
}

// The composer clears the moment RegisterQueueItem returns, so between the
// register and the provider write the queue row is the message's only copy.
// It has to be on disk before it is in memory.
func TestRegisterQueueItemWritesItsDurableRowFirst(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-register")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	queued, err := app.RegisterQueueItem(context.Background(), thread.ID, "queued follow-up", SendMessageOptions{
		SendID:        "send-abc",
		AttachmentIDs: []string{"att-1"},
	})
	if err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	rows := durableQueueRows(t, app, thread.ID)
	if len(rows) != 1 {
		t.Fatalf("durable rows: got %d, want 1", len(rows))
	}
	if rows[0].ID != queued.ID {
		t.Errorf("row id: got %q, want the id the caller was answered with %q", rows[0].ID, queued.ID)
	}
	if rows[0].Message != "queued follow-up" {
		t.Errorf("row message: got %q", rows[0].Message)
	}
	if rows[0].SendID != "send-abc" {
		t.Errorf("row send id: got %q, want send-abc", rows[0].SendID)
	}
	if rows[0].EnqueuedAt != queued.EnqueuedAt {
		t.Errorf("row enqueued_at %d != wire %d", rows[0].EnqueuedAt, queued.EnqueuedAt)
	}
	var payload flushQueuePayload
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("decode row payload: %v", err)
	}
	if len(payload.AttachmentIDs) != 1 || payload.AttachmentIDs[0] != "att-1" {
		t.Errorf("row payload attachment ids: got %+v", payload.AttachmentIDs)
	}
}

// The first of the two durable endpoints: the dispatcher handed the message
// to the provider, so the row's job is done and it must not survive to be
// restored into the composer at the next boot.
func TestDispatchedQueueItemDropsItsDurableRow(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-dispatch")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeClaudePassthroughBinary(t)}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "flush me", SendMessageOptions{
		SendID: "send-dispatch",
	}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if len(durableQueueRows(t, app, thread.ID)) != 1 {
		t.Fatal("no durable row after register")
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })

	// The start funnel hands the batch to the dispatch worker, which
	// serializes behind the thread action lock StartSession holds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(durableQueueRows(t, app, thread.ID)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the durable row outlived the provider write — a delivered message would be restored into the composer at the next boot")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The other durable endpoint. A session death puts the queue back in the
// composer, which is a home the message keeps across a restart on its own.
func TestSessionDeathRestoreDropsTheDurableRow(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-session-death")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "not delivered", SendMessageOptions{}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	app.restoreUnconfirmedQueueOnSessionDeath(thread.ID)

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "not delivered" {
		t.Fatalf("draft content = %q, want the queued message", draft.Content)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows after the composer restore: got %+v, want none", rows)
	}
}

// A REQUEUE is not an endpoint: the provider write failed, the message is
// still undelivered, and its row stays exactly where it was.
func TestRequeuedQueueItemKeepsItsDurableRow(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-requeue")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	queued, err := app.RegisterQueueItem(context.Background(), thread.ID, "keep me", SendMessageOptions{})
	if err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	items := app.triage.QueuedFlushItems(thread.ID)
	if len(items) != 1 {
		t.Fatalf("queued items: got %d, want 1", len(items))
	}

	// A closed session fails both the steer and its fallback send.
	sess := installSteerTestSession(t, app, thread, "no-active-turn")
	if err := sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "requeue-token",
		Codex:    sess,
	})

	app.dispatchFlush(thread.ID, items)

	rows := durableQueueRows(t, app, thread.ID)
	if len(rows) != 1 || rows[0].ID != queued.ID {
		t.Fatalf("durable rows after a failed dispatch: got %+v, want the row kept", rows)
	}
}

// A drop is the opposite: the in-memory queue is discarded with no restore,
// so a surviving row would resurrect at the next boot the very messages the
// user's Stop threw away.
func TestSessionTeardownDropsDurableQueueRows(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-teardown")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "thrown away", SendMessageOptions{}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "stop-tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows after a teardown that discarded the queue: got %+v, want none", rows)
	}
}

// The boot sweep: every row still present when the process starts is residue
// from a crash, and it goes into the composer rather than to the provider.
func TestBootSweepRestoresQueuedMessagesIntoTheComposer(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("durable-boot-sweep")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	other := testThread("durable-boot-sweep-other")
	other.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread other: %v", err)
	}
	if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "typed by hand",
		Attachments:   `["att-typed"]`,
		TerminalChips: "[]",
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	for _, row := range []store.FlushQueueItem{
		{ID: "queue:one", ThreadID: thread.ID, Message: "queued one",
			Payload: json.RawMessage(`{"attachmentIds":["att-one"]}`), EnqueuedAt: 10},
		{ID: "queue:two", ThreadID: thread.ID, Message: "queued two",
			Payload: json.RawMessage(`{}`), EnqueuedAt: 20},
		{ID: "queue:other", ThreadID: other.ID, Message: "other thread", EnqueuedAt: 30},
	} {
		if err := app.store.InsertFlushQueueItem(row); err != nil {
			t.Fatalf("seed row %s: %v", row.ID, err)
		}
	}

	app.restoreDurableFlushQueueAtBoot()

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	// Queue order first, then whatever the composer itself held: the same
	// merge rule a session-death restore uses, so a person sees one composer
	// reading the way it would have if the app had never stopped.
	if draft.Content != "queued one\n\nqueued two\n\ntyped by hand" {
		t.Fatalf("draft content = %q", draft.Content)
	}
	if draft.Attachments != `["att-one","att-typed"]` {
		t.Fatalf("draft attachments = %q", draft.Attachments)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("rows after the sweep: got %+v, want none", rows)
	}
	// Every thread holding residue is swept, not just the first.
	otherDraft, _, err := app.store.GetThreadDraft(other.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft other: %v", err)
	}
	if otherDraft.Content != "other thread" {
		t.Fatalf("other draft content = %q", otherDraft.Content)
	}
	// An ordinary draft-changed event, so an attached client that was open
	// across the restart converges instead of showing an empty composer.
	if !rec.hasEvent("draft:updated") {
		t.Error("draft:updated not emitted by the boot sweep")
	}
}

// The message text lives in its own column precisely so nothing about the
// payload can take it away.
func TestBootSweepRestoresTheMessageOfAnUnreadableRow(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-boot-sweep-broken")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{
		ID: "queue:broken", ThreadID: thread.ID, Message: "still my words",
		Payload: json.RawMessage(`{"attachmentIds":`), EnqueuedAt: 10,
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	app.restoreDurableFlushQueueAtBoot()

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "still my words" {
		t.Fatalf("draft content = %q, want the message a person typed", draft.Content)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("rows after the sweep: got %+v, want none", rows)
	}
}

// The boot sweep NEVER re-dispatches: a queued message was written against a
// turn that no longer exists, on a session that is gone.
func TestBootSweepStartsNoSession(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)

	thread := testThread("durable-boot-no-dispatch")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{
		ID: "queue:boot", ThreadID: thread.ID, Message: "do not send this", EnqueuedAt: 10,
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	app.restoreDurableFlushQueueAtBoot()

	if _, ok := app.sessionManager().get(thread.ID); ok {
		t.Fatal("the boot sweep started a provider session")
	}
	if app.triage.HasQueuedFlushItems(thread.ID) {
		t.Fatal("the boot sweep put the message back on the live queue")
	}
	if rec.hasEvent("provider:queue_flushed") {
		t.Fatal("the boot sweep dispatched a queued message")
	}
}

// Idempotency, queue path: a socket that died after the frame arrived looks
// exactly like one that died before it, so the retried frame carries the same
// send id and is answered from the row the first one wrote.
func TestRegisterQueueItemAnswersARepeatedSendFromItsRow(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-idempotent-queue")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	first, err := app.RegisterQueueItem(context.Background(), thread.ID, "only once", SendMessageOptions{
		SendID: "send-repeat",
	})
	if err != nil {
		t.Fatalf("first RegisterQueueItem: %v", err)
	}
	second, err := app.RegisterQueueItem(context.Background(), thread.ID, "only once", SendMessageOptions{
		SendID: "send-repeat",
	})
	if err != nil {
		t.Fatalf("second RegisterQueueItem: %v", err)
	}

	if second.ID != first.ID || second.EnqueuedAt != first.EnqueuedAt {
		t.Fatalf("second answer = %+v, want the first one's row %+v", second, first)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 1 {
		t.Fatalf("durable rows: got %d, want 1 — the repeat queued a second copy", len(rows))
	}
	if count := app.triage.QueuedFlushItemCount(thread.ID); count != 1 {
		t.Fatalf("live queue: got %d items, want 1", count)
	}

	// An EMPTY send id disables the check rather than matching every other
	// message that also has none: an app-internal injector and a client too
	// old to mint one both send that.
	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "genuinely new", SendMessageOptions{}); err != nil {
		t.Fatalf("unidentified RegisterQueueItem: %v", err)
	}
	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "genuinely new again", SendMessageOptions{}); err != nil {
		t.Fatalf("second unidentified RegisterQueueItem: %v", err)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 3 {
		t.Fatalf("durable rows: got %d, want 3 — an empty send id must not dedupe", len(rows))
	}
}

// Idempotency, send path: the repeat is answered from the `user_text` row the
// first frame persisted, and starts nothing.
func TestSendMessageAnswersARepeatedSendFromItsItem(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-idempotent-send")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "idempotent-tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "one message", SendMessageOptions{
		SendID: "send-once",
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "one message", SendMessageOptions{
		SendID: "send-once",
	}); err != nil {
		t.Fatalf("repeated send: %v", err)
	}

	if got := userTextRowCount(t, app, thread.ID); got != 1 {
		t.Fatalf("user_text rows: got %d, want 1 — the repeat started a second turn", got)
	}

	// A DIFFERENT id on the same text is a different message: somebody typed
	// the same thing twice, which is theirs to do.
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "one message", SendMessageOptions{
		SendID: "send-twice",
	}); err != nil {
		t.Fatalf("second distinct send: %v", err)
	}
	if got := userTextRowCount(t, app, thread.ID); got != 2 {
		t.Fatalf("user_text rows: got %d, want 2", got)
	}
}

// userTextRowCount counts the thread's user_text rows. The idempotency lookup
// itself answers at most one row by id, so a test that wants to know how many
// messages a thread ended up with reads the timeline instead of it.
func userTextRowCount(t *testing.T, app *App, threadID string) int {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	n := 0
	for _, item := range items {
		if item.Kind == "user_text" {
			n++
		}
	}
	return n
}

// The window is bounded, and the id lives in `meta`: a send whose id matches a
// row is a repeat whatever else is on the thread.
func TestFindRecordedSendMatchesEitherHome(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-find-recorded")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID: "user:dispatched", ThreadID: thread.ID, TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "already sent", Meta: `{"sendId":"send-dispatched"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := app.store.InsertFlushQueueItem(store.FlushQueueItem{
		ID: "queue:waiting", ThreadID: thread.ID, SendID: "send-queued",
		Message: "still waiting", EnqueuedAt: now,
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	record, found, err := app.findRecordedSend(thread.ID, "send-dispatched")
	if err != nil || !found {
		t.Fatalf("dispatched lookup: found=%v err=%v", found, err)
	}
	if !record.dispatched || record.item.ID != "user:dispatched" {
		t.Fatalf("dispatched record = %+v", record)
	}

	record, found, err = app.findRecordedSend(thread.ID, "send-queued")
	if err != nil || !found {
		t.Fatalf("queued lookup: found=%v err=%v", found, err)
	}
	if record.dispatched || record.queued.ID != "queue:waiting" {
		t.Fatalf("queued record = %+v", record)
	}

	if _, found, err = app.findRecordedSend(thread.ID, "send-unknown"); err != nil || found {
		t.Fatalf("unknown lookup: found=%v err=%v", found, err)
	}
	// An empty id never matches, and never reads a row to find that out.
	if _, found, err = app.findRecordedSend(thread.ID, ""); err != nil || found {
		t.Fatalf("empty lookup: found=%v err=%v", found, err)
	}
	// Another thread's identical id is another thread's message.
	if _, found, err = app.findRecordedSend("some-other-thread", "send-queued"); err != nil || found {
		t.Fatalf("cross-thread lookup: found=%v err=%v", found, err)
	}
}

// The window bounds the lookup, so a send id that has scrolled out of it is
// not found. That is the accepted edge of a bounded check: the retry window a
// reconnect produces is a handful of messages wide.
func TestFindRecordedSendLooksOnlyAtTheRecentWindow(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-window")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	for i := range recentSendIDWindow + 1 {
		meta := `{"sendId":"send-plain"}`
		if i == 0 {
			meta = `{"sendId":"send-oldest"}`
		}
		if _, err := app.store.AppendItem(store.Item{
			ID: "user:" + string(rune('A'+i%26)) + string(rune('a'+i/26)), ThreadID: thread.ID, TurnIndex: i,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: "msg", Meta: meta, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
	}

	if _, found, err := app.findRecordedSend(thread.ID, "send-oldest"); err != nil || found {
		t.Fatalf("oldest lookup: found=%v err=%v, want not found past the window", found, err)
	}
}

// A queued item registered by an INJECTOR still settles its injector's
// bookkeeping when the durable row is deleted: the row deletion is composed
// into the settlement rather than replacing it.
func TestDurableRowDeletionComposesWithTheInjectorSettlement(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)

	thread := testThread("durable-settlement")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	settled := 0
	if _, err := app.registerQueueItem(thread.ID, "injected", SendMessageOptions{}, injectedQueueOptions{
		preserveDraft: true,
		onDurable:     func() { settled++ },
	}); err != nil {
		t.Fatalf("registerQueueItem: %v", err)
	}

	items := app.triage.QueuedFlushItems(thread.ID)
	if len(items) != 1 {
		t.Fatalf("queued items: got %d, want 1", len(items))
	}
	items[0].Settlement.Settle()

	if settled != 1 {
		t.Fatalf("injector settlement ran %d times, want 1", settled)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows after settling: got %+v, want none", rows)
	}
	// sync.Once: the two endpoints can race and the delete stays exactly once.
	items[0].Settlement.Settle()
	if settled != 1 {
		t.Fatalf("injector settlement ran again: %d", settled)
	}
}
