package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/sessionruntime"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// A worktree removal stops the session of every thread on it, and none of
// the messages queued behind that session is lost: one queued before the
// removal, one queued while the removal moves the row and stops the
// process, and one queued after it. The first two go back to the composer;
// the third waits in the queue, durably. A crash right after the removal
// leaves each of the three in exactly one durable home, so the next boot
// puts all of them in the composer once.
func TestWorktreeRemovalLosesNoQueuedMessage(t *testing.T) {
	t.Parallel()
	f := newWatchFixture(t)
	worktree := f.worktree(t, "feature-queue")
	attached := f.thread(t, "thread-queue", worktree, "feature-queue")
	if err := f.app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: attached.ID, TurnID: "turn-0", Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	liveness := sessionruntime.NewLiveness(time.Now())
	liveness.ActiveTurns.Store(1)
	f.app.sessionManager().put(attached.ID, session{Provider: string(provider.Claude), Token: "token-queue", Liveness: liveness})
	f.app.startSessionFn = func(string) error { return nil }

	queue := func(text, sendID string) {
		t.Helper()
		// A modern composer names the draft it consumed: an empty one,
		// so the composer content restored meanwhile stays.
		if _, err := f.app.RegisterQueueItem(context.Background(), attached.ID, text, SendMessageOptions{SendID: sendID, ConsumeDraft: &DraftSnapshot{}}); err != nil {
			t.Fatalf("RegisterQueueItem(%s): %v", text, err)
		}
	}
	queue("queued before the removal", "send-before")

	// The removal moves the row and announces it before it stops the
	// process. A message queued in that window is queued while the session
	// is detached and not yet stopped.
	var once sync.Once
	f.app.testEmitHook = func(name string, data any) {
		f.rows.hook(name, data)
		f.events.capture(name, data)
		evt, ok := data.(triage.ThreadUpdateEvent)
		if name != "thread:updated" || !ok || evt.Thread == nil || evt.Thread.ID != attached.ID || !samePath(evt.Thread.WorkspacePath, f.repo) {
			return
		}
		once.Do(func() { queue("queued during the removal", "send-during") })
	}

	testutil.RunGit(t, f.repo, "worktree", "remove", "--force", worktree)
	f.app.reconcileProjectWorktrees(f.repo, nil)
	f.assertAtRoot(t, attached.ID)

	queue("queued after the removal", "send-after")

	draft, _, err := f.app.store.GetThreadDraft(attached.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	for _, text := range []string{"queued before the removal", "queued during the removal"} {
		if strings.Count(draft.Content, text) != 1 {
			t.Errorf("draft = %q, want %q in it once", draft.Content, text)
		}
	}
	if strings.Contains(draft.Content, "queued after the removal") {
		t.Errorf("draft = %q, want the message queued after the removal still queued", draft.Content)
	}
	rows := durableQueueRows(t, f.app, attached.ID)
	if len(rows) != 1 || rows[0].SendID != "send-after" {
		t.Fatalf("durable queue rows = %+v, want only the message queued after the removal", rows)
	}

	// The crash: nothing in memory survives, and the next boot restores
	// what is on disk.
	f.app.restoreDurableFlushQueueAtBoot()
	draft, _, err = f.app.store.GetThreadDraft(attached.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft after boot: %v", err)
	}
	for _, text := range []string{"queued before the removal", "queued during the removal", "queued after the removal"} {
		if strings.Count(draft.Content, text) != 1 {
			t.Errorf("draft after boot = %q, want %q in it once", draft.Content, text)
		}
	}
}

// A reconnect stops the session and starts another; nobody asked for its
// queue to be thrown away. A message queued behind the old session goes back
// to the composer, as it does when a session dies.
func TestReconnectSessionReturnsTheQueueToTheComposer(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	app.triage.SetFlushDispatcher(nil)
	thread := testThread("reconnect-keeps-queue")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "reconnect-tok", Liveness: newSessionLiveness(time.Now())})
	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "still wanted", SendMessageOptions{SendID: "send-reconnect"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	app.startSessionFn = func(string) error { return nil }

	if err := app.ReconnectSession(thread.ID); err != nil {
		t.Fatalf("ReconnectSession: %v", err)
	}

	draft, _, err := app.store.GetThreadDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	if draft.Content != "still wanted" {
		t.Fatalf("draft = %q, want the queued message back in the composer", draft.Content)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable queue rows = %+v, want none once the composer has the message", rows)
	}
}

// A message a restore had to put back on the queue, after the provider write
// that retired its durable row, is durable again: a crash before it is
// delivered leaves it for the boot restore. Its dispatch retires the row.
func TestRequeuedUnconfirmedMessageIsDurableAgain(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	app.triage.SetFlushDispatcher(nil)
	thread := testThread("requeue-durable")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	app.requeueUnconfirmedFlushItems(thread.ID, []triage.UnconfirmedFlushItem{{
		QueueItemID: "queue:taken-back",
		Message:     "sent once, never confirmed",
		Payload:     []byte(`{"sendId":"send-taken-back"}`),
		EnqueuedAt:  42,
	}})

	rows := durableQueueRows(t, app, thread.ID)
	if len(rows) != 1 || rows[0].ID != "queue:taken-back" || rows[0].SendID != "send-taken-back" || rows[0].Message != "sent once, never confirmed" {
		t.Fatalf("durable rows = %+v, want the requeued message with its send id", rows)
	}
	queued := app.triage.QueuedFlushItems(thread.ID)
	if len(queued) != 1 {
		t.Fatalf("queued = %+v, want the message queued once", queued)
	}
	app.settleFlushGroup(thread.ID, queued)
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows after its dispatch settled = %+v, want none", rows)
	}
}
