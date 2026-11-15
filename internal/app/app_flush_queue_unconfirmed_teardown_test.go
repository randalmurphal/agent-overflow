package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// unconfirmedSendFixture is a Claude thread with turn 2 running and a message
// queued behind it that the app wrote to the CLI: its quiet row is in the
// store, marked pendingFlush, and the CLI never confirms it.
type unconfirmedSendFixture struct {
	app    *App
	rec    *emitRecorder
	thread store.Thread
	row    store.Item
}

const unconfirmedSendText = "written, never read"

func newUnconfirmedSendFixture(t *testing.T, name string, fromAgent bool) *unconfirmedSendFixture {
	t.Helper()
	app, rec := newAppForFlushQueueRPC(t)
	thread := testThread(name)
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{TurnID: "turn-2", ThreadID: thread.ID, TurnIndex: 2, StartedAt: now}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: 2, Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(), thread.ID,
		claude.Config{Reaper: procutiltest.Reaper(), Retention: claude.UserHomeRetention(0), Binary: writeClaudeInterruptResponderBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "tok", Claude: sess, Liveness: newSessionLiveness(time.Now())})

	if fromAgent {
		// An agent's thread_send queued behind the running turn.
		if _, err := app.registerQueueItem(thread.ID, unconfirmedSendText, SendMessageOptions{SendID: "send-agent"}, injectedQueueOptions{
			origin:       usermessage.OriginAgentThread,
			originThread: &usermessage.OriginThread{ThreadID: "caller-thread", Token: "tok-agent"},
		}); err != nil {
			t.Fatalf("registerQueueItem: %v", err)
		}
	} else if _, err := app.RegisterQueueItem(context.Background(), thread.ID, unconfirmedSendText, SendMessageOptions{SendID: "send-user"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	f := &unconfirmedSendFixture{app: app, rec: rec, thread: thread}
	waitUntil(t, 10*time.Second, func() bool {
		items, err := app.store.ListTurnItems(thread.ID, 2)
		if err != nil {
			t.Fatalf("ListTurnItems: %v", err)
		}
		for _, item := range items {
			if item.Kind == "user_text" && item.Summary == unconfirmedSendText {
				f.row = item
				return true
			}
		}
		return false
	})
	if !rowMarkedPending(t, app, thread.ID, f.row.ID) {
		t.Fatalf("quiet row %s is not marked pendingFlush: %s", f.row.ID, f.row.Meta)
	}
	return f
}

// rowMarkedPending reads whether the row still carries pendingFlush, false
// for a row that is gone.
func rowMarkedPending(t *testing.T, app *App, threadID, itemID string) bool {
	t.Helper()
	item, found, err := app.store.GetThreadItem(threadID, itemID)
	if err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	}
	return found && strings.Contains(item.Meta, `"pendingFlush":true`)
}

func (f *unconfirmedSendFixture) rowFound(t *testing.T) bool {
	t.Helper()
	_, found, err := f.app.store.GetThreadItem(f.thread.ID, f.row.ID)
	if err != nil {
		t.Fatalf("GetThreadItem: %v", err)
	}
	return found
}

func (f *unconfirmedSendFixture) draft(t *testing.T) string {
	t.Helper()
	draft, _, err := f.app.store.GetThreadDraft(f.thread.ID)
	if err != nil {
		t.Fatalf("GetThreadDraft: %v", err)
	}
	return draft.Content
}

// A message the session was sent and never confirmed ends in exactly one
// place when its turn or session ends, and its row never keeps pendingFlush
// past the session: a Stop drops it with the rest of the queue (row gone,
// composer untouched), and a death returns it to the composer (row gone,
// text in the composer). A turn that ends before the CLI reads the message
// leaves it pending only while the session lives, and an interrupt
// promotes it, which clears the mark at once.
func TestAnUnconfirmedSendNeverOutlivesItsSessionMarked(t *testing.T) {
	t.Parallel()
	stop := func(t *testing.T, f *unconfirmedSendFixture) {
		t.Helper()
		if err := f.app.StopSession(f.thread.ID); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
	}
	dropped := func(t *testing.T, f *unconfirmedSendFixture) {
		t.Helper()
		if f.rowFound(t) {
			t.Fatalf("row %s survived the Stop that dropped its message", f.row.ID)
		}
		if draft := f.draft(t); draft != "" {
			t.Fatalf("draft = %q, want the dropped message nowhere", draft)
		}
		restored := emittedQueueRestored(f.rec)
		if len(restored) != 1 || restored[0].Reason != queueRestoredReasonDropped || len(restored[0].UserItemIDs) != 1 || restored[0].UserItemIDs[0] != f.row.ID {
			t.Fatalf("queue restored events = %+v, want one dropped event naming %s", restored, f.row.ID)
		}
	}
	for _, tc := range []struct {
		name      string
		fromAgent bool
		end       func(t *testing.T, f *unconfirmedSendFixture)
	}{
		{name: "session stopped mid-turn", end: func(t *testing.T, f *unconfirmedSendFixture) {
			stop(t, f)
			dropped(t, f)
		}},
		{name: "agent send, session stopped mid-turn", fromAgent: true, end: func(t *testing.T, f *unconfirmedSendFixture) {
			stop(t, f)
			dropped(t, f)
		}},
		{name: "turn ends without the echo, then the session stops", end: func(t *testing.T, f *unconfirmedSendFixture) {
			if err := f.app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: f.thread.ID, TurnIndex: 2, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}, Timestamp: time.Now()}); err != nil {
				t.Fatalf("turn complete: %v", err)
			}
			if !rowMarkedPending(t, f.app, f.thread.ID, f.row.ID) || len(f.app.triage.LiveStateSnapshotForThread(f.thread.ID).FlushedItems) != 1 {
				t.Fatal("the message stopped being pending while its session lives")
			}
			stop(t, f)
			dropped(t, f)
		}},
		{name: "turn interrupted, then the session stops", end: func(t *testing.T, f *unconfirmedSendFixture) {
			if err := f.app.InterruptTurn(f.thread.ID, nil); err != nil {
				t.Fatalf("InterruptTurn: %v", err)
			}
			if !f.rowFound(t) || rowMarkedPending(t, f.app, f.thread.ID, f.row.ID) {
				t.Fatal("the interrupt did not promote the row")
			}
			stop(t, f)
			dropped(t, f)
		}},
		{name: "session dies", end: func(t *testing.T, f *unconfirmedSendFixture) {
			f.app.sessionEventHandler(f.thread.ID, "tok", string(provider.Claude))(provider.ProviderEvent{
				Kind: provider.EventSessionStatus, ThreadID: f.thread.ID, Content: "error", Timestamp: time.Now(),
			})
			waitForCondition(t, "the death restore", func() bool { return strings.Contains(f.draft(t), unconfirmedSendText) })
			if f.rowFound(t) {
				t.Fatalf("row %s survived the death restore that returned its text", f.row.ID)
			}
		}},
		{name: "agent send, session dies", fromAgent: true, end: func(t *testing.T, f *unconfirmedSendFixture) {
			f.app.sessionEventHandler(f.thread.ID, "tok", string(provider.Claude))(provider.ProviderEvent{
				Kind: provider.EventSessionStatus, ThreadID: f.thread.ID, Content: "error", Timestamp: time.Now(),
			})
			waitForCondition(t, "the death restore", func() bool { return strings.Contains(f.draft(t), unconfirmedSendText) })
			if f.rowFound(t) {
				t.Fatalf("row %s survived the death restore that returned its text", f.row.ID)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUnconfirmedSendFixture(t, "unconfirmed-"+strings.ReplaceAll(tc.name, " ", "-"), tc.fromAgent)
			tc.end(t, f)
			// The next boot finds nothing to take back.
			f.app.restoreDurableFlushQueueAtBoot()
			if rows, err := f.app.store.ListUnconfirmedUserRows(); err != nil || len(rows) != 0 {
				t.Fatalf("unconfirmed rows after the session ended = %+v (%v), want none", rows, err)
			}
		})
	}
}
