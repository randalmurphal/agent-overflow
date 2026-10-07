package app

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// sendsPendingFrames returns the provider:sends_pending answers published
// for threadID, in order, once the publisher has nothing left to do.
func sendsPendingFrames(t *testing.T, app *App, rec *emitRecorder, threadID string) []bool {
	t.Helper()
	waitSendsPendingIdle(t, app)
	var out []bool
	for _, call := range rec.snapshot() {
		if call.Channel != string(eventchan.ProviderSendsPending) {
			continue
		}
		evt := call.Data.(SendsPendingEvent)
		if evt.ThreadID == threadID {
			out = append(out, evt.Pending)
		}
	}
	return out
}

// waitSendsPendingIdle waits until the publisher has nothing left to read.
// A test that replaces app.triage waits first: the worker reads the router.
func waitSendsPendingIdle(t *testing.T, app *App) {
	t.Helper()
	waitFor(t, "sends-pending publisher to go idle", func() bool {
		s := &app.sendsPending
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.running && len(s.dirty) == 0
	})
}

func assertSendsPendingFrames(t *testing.T, app *App, rec *emitRecorder, threadID, stage string, want ...bool) {
	t.Helper()
	if got := sendsPendingFrames(t, app, rec, threadID); !slices.Equal(got, want) {
		t.Fatalf("%s: sends_pending frames = %v, want %v", stage, got, want)
	}
}

// One frame per change of answer, however many mutations led to it.
func TestSendsPendingPublishesChangesOnly(t *testing.T) {
	t.Parallel()
	app, rec := newAppForFlushQueueRPC(t)
	const threadID = "sends-pending-changes"

	app.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{ID: "q1", Message: "first"})
	app.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{ID: "q2", Message: "second"})
	assertSendsPendingFrames(t, app, rec, threadID, "queued", true)

	if _, ok := app.triage.RemoveQueuedFlushItem(threadID, "q1"); !ok {
		t.Fatal("q1 not removed")
	}
	assertSendsPendingFrames(t, app, rec, threadID, "one still queued", true)

	if _, ok := app.triage.RemoveQueuedFlushItem(threadID, "q2"); !ok {
		t.Fatal("q2 not removed")
	}
	assertSendsPendingFrames(t, app, rec, threadID, "queue emptied", true, false)
	if _, listed := app.sendsPendingThreads()[threadID]; listed {
		t.Fatal("snapshot set still names the thread")
	}
}

// A batch the App dispatcher holds has left triage; it is still pending.
func TestSendsPendingCoversDispatcherHeldBatch(t *testing.T) {
	t.Parallel()
	app, rec := newAppForFlushQueueRPC(t)
	const threadID = "sends-pending-dispatch"

	app.beginFlushDispatchVisibility(threadID, []triage.QueuedFlushItem{{ID: "q1", Message: "held"}})
	assertSendsPendingFrames(t, app, rec, threadID, "held by the dispatcher", true)
	app.endFlushDispatchVisibility(threadID)
	assertSendsPendingFrames(t, app, rec, threadID, "released", true, false)
}

// The whole handoff chain, from the triage queue through the App
// dispatcher and the Codex steer to the provider's echo, reads as one
// pending stretch: no idle frame until the echo lands.
func TestSendsPendingSpansTheFlushToItsEcho(t *testing.T) {
	t.Parallel()
	app, rec := newAppForFlushQueueRPC(t)
	thread := testThread("sends-pending-echo")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "flush-token",
		Codex:    installSteerTestSession(t, app, thread, "ok"),
	})

	app.triage.RegisterQueueItem(thread.ID, triage.QueuedFlushItem{ID: "queue:abc", Message: "drained", Payload: json.RawMessage(`{}`)})
	if !app.triage.FlushQueuedItems(thread.ID) {
		t.Fatal("queue did not drain")
	}
	waitFor(t, "the steer to register its pending send", func() bool {
		return app.triage.HasPendingSendForThread(thread.ID) && rec.hasEvent(string(eventchan.ProviderQueueFlushed))
	})
	assertSendsPendingFrames(t, app, rec, thread.ID, "flushed, awaiting the echo", true)

	echoMeta, _ := json.Marshal(map[string]any{"provider_item_id": "wire-user-1", "client_id": "user:3:flush:1"})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: thread.ID, TurnIndex: 3, ItemID: "user:3:flush:1",
		Content: "drained", Meta: echoMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	assertSendsPendingFrames(t, app, rec, thread.ID, "echoed", true, false)
}

// The snapshot names a thread whose only activity is a pending send, and
// folds the flag into the row of a thread that has other activity.
func TestListThreadLiveActivityReportsSendsPending(t *testing.T) {
	t.Parallel()
	app, rec := newAppForFlushQueueRPC(t)
	running := seedLiveStateTodoThread(t, app, "pending-running")
	queuedOnly := seedLiveStateTodoThread(t, app, "pending-queued-only")
	idle := seedLiveStateTodoThread(t, app, "pending-idle")

	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: running.ID, TurnID: "turn-1", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	app.triage.RegisterQueueItem(running.ID, triage.QueuedFlushItem{ID: "q1", Message: "next"})
	app.triage.RegisterQueueItem(queuedOnly.ID, triage.QueuedFlushItem{ID: "q2", Message: "waiting"})
	assertSendsPendingFrames(t, app, rec, queuedOnly.ID, "queued", true)
	assertSendsPendingFrames(t, app, rec, running.ID, "queued behind the turn", true)

	rows, err := app.ListThreadLiveActivity()
	if err != nil {
		t.Fatal(err)
	}
	byThread := map[string]ThreadLiveActivity{}
	for _, row := range rows {
		if _, dup := byThread[row.ThreadID]; dup {
			t.Fatalf("thread %s listed twice: %+v", row.ThreadID, rows)
		}
		byThread[row.ThreadID] = row
	}
	if _, listed := byThread[idle.ID]; listed || len(rows) != 2 {
		t.Fatalf("rows = %+v, want the running and queued threads", rows)
	}
	if got := byThread[running.ID]; got.ActiveTurn == nil || !got.SendsPending {
		t.Fatalf("running row = %+v, want its turn and sends pending", got)
	}
	got := byThread[queuedOnly.ID]
	if got.ActiveTurn != nil || !got.SendsPending || got.ApprovalRequestIDs == nil || got.UserInputRequestIDs == nil {
		t.Fatalf("queued-only row = %+v, want only sends pending, with empty request lists", got)
	}
}

// Once stopped, the publisher joins its worker and publishes nothing more.
func TestSendsPendingStopsPublishing(t *testing.T) {
	t.Parallel()
	app, rec := newAppForFlushQueueRPC(t)
	const threadID = "sends-pending-stop"

	app.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{ID: "q1", Message: "queued"})
	app.stopSendsPending()
	before := sendsPendingFrames(t, app, rec, threadID)
	app.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{ID: "q2", Message: "late"})
	if _, ok := app.triage.RemoveQueuedFlushItem(threadID, "q1"); !ok {
		t.Fatal("q1 not removed")
	}
	if _, ok := app.triage.RemoveQueuedFlushItem(threadID, "q2"); !ok {
		t.Fatal("q2 not removed")
	}
	if got := sendsPendingFrames(t, app, rec, threadID); !slices.Equal(got, before) {
		t.Fatalf("frames after stop = %v, want unchanged %v", got, before)
	}
}

// The dispatcher's own exits end the pending stretch: a revert that throws
// the batches away, a session end that drains them, and a dispatch that
// leaves nothing behind.
func TestSendsPendingClearsOnDispatcherExits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		exit func(*App, string)
	}{
		{"rollback", func(app *App, threadID string) { app.clearFlushDispatchForRollback(threadID) }},
		{"session end", func(app *App, threadID string) { app.drainFlushDispatchForSessionEnd(threadID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, rec := newAppForFlushQueueRPC(t)
			threadID := "sends-pending-" + tc.name
			// Stand in for a running worker so the batch stays queued.
			app.flushDispatch.mu.Lock()
			app.ensureFlushDispatchMapsLocked()
			app.flushDispatch.running[threadID] = true
			app.flushDispatch.mu.Unlock()
			app.enqueueFlushDispatch(threadID, []triage.QueuedFlushItem{{ID: "q1", Message: "queued"}})
			assertSendsPendingFrames(t, app, rec, threadID, "queued in the dispatcher", true)
			tc.exit(app, threadID)
			assertSendsPendingFrames(t, app, rec, threadID, "exited", true, false)
		})
	}
	t.Run("dispatch that drops its message", func(t *testing.T) {
		t.Parallel()
		app, rec := newAppForFlushQueueRPC(t)
		thread := testThread("sends-pending-dropped")
		thread.Provider = string(provider.Codex)
		thread.WorkspacePath = t.TempDir()
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		// An empty message (admitted by older clients) is settled and
		// dropped rather than delivered or requeued.
		app.enqueueFlushDispatch(thread.ID, []triage.QueuedFlushItem{{ID: "q1", Payload: json.RawMessage(`{}`)}})
		waitFor(t, "the dispatch worker to finish", func() bool {
			app.flushDispatch.mu.Lock()
			defer app.flushDispatch.mu.Unlock()
			return !app.flushDispatch.running[thread.ID]
		})
		// The publisher may read only after the drop and publish nothing;
		// either way the last answer published is idle.
		frames := sendsPendingFrames(t, app, rec, thread.ID)
		if (len(frames) > 0 && frames[len(frames)-1]) || app.triage.SendsPending(thread.ID) {
			t.Fatalf("frames = %v, want none or the last one idle", frames)
		}
		if _, listed := app.sendsPendingThreads()[thread.ID]; listed {
			t.Fatal("snapshot set still names the thread")
		}
	})
}

// RegisterQueueItem answers with the thread's sends-pending state once the
// message is queued, as of the newest frame on the channel: the frame that
// reported it is at or below the answer's sequence, every later change above.
func TestRegisterQueueItemAnswersSendsPendingAtItsFrameSequence(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	bus := transport.NewEventBus(64)
	t.Cleanup(bus.Close)
	app.SetEventBus(bus)
	sub := bus.Subscribe()
	t.Cleanup(sub.Close)
	thread := testThread("sends-pending-order")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	type frame struct {
		SendsPendingEvent
		seq uint64
	}
	var frames []frame
	// collect reads the bus until n sends-pending frames have arrived.
	collect := func(n int) {
		t.Helper()
		waitSendsPendingIdle(t, app)
		deadline := time.After(2 * time.Second)
		for len(frames) < n {
			select {
			case e := <-sub.Events():
				if e.Channel != string(eventchan.ProviderSendsPending) {
					continue
				}
				var evt SendsPendingEvent
				if err := json.Unmarshal(e.Data, &evt); err != nil {
					t.Fatalf("decode %s: %v", e.Data, err)
				}
				frames = append(frames, frame{evt, e.Seq})
			case <-deadline:
				t.Fatalf("frames %+v, want %d", frames, n)
			}
		}
	}
	register := func(message, sendID string) SendsPendingAnswer {
		t.Helper()
		item, err := app.RegisterQueueItem(context.Background(), thread.ID, message, SendMessageOptions{SendID: sendID})
		if err != nil {
			t.Fatalf("RegisterQueueItem(%q): %v", sendID, err)
		}
		if item.ID == "" {
			t.Fatalf("RegisterQueueItem(%q) = %+v", sendID, item)
		}
		return item.SendsPending
	}

	// The worker is stopped so the reply's own publish emits each frame;
	// the test publishes the other changes itself.
	app.stopSendsPending()

	// Another thread's frame comes first: the sequence is the channel's.
	app.triage.RegisterQueueItem("sends-pending-other", triage.QueuedFlushItem{ID: "other", Message: "other"})
	app.publishSendsPending("sends-pending-other")
	collect(1)

	first := register("first", "send-1")
	collect(2)
	reported := frames[1]
	if reported.ThreadID != thread.ID || !reported.Pending || !first.Pending || first.Sequence != reported.seq {
		t.Fatalf("answer %+v, frames %+v: want pending as of the frame that reported it", first, frames)
	}
	// The answer did not change, so neither did its sequence.
	if second := register("second", "send-2"); second != first {
		t.Fatalf("second answer = %+v, want %+v", second, first)
	}

	for _, item := range app.triage.QueuedFlushItems(thread.ID) {
		if _, ok := app.triage.RemoveQueuedFlushItem(thread.ID, item.ID); !ok {
			t.Fatalf("%s not removed", item.ID)
		}
	}
	cleared := app.publishSendsPending(thread.ID)
	collect(3)
	if got := frames[2]; got.ThreadID != thread.ID || got.Pending || cleared.Pending || cleared.Sequence != got.seq || got.seq <= first.Sequence {
		t.Fatalf("cleared answer %+v, frames %+v: want the clear as of its own frame, after %d", cleared, frames, first.Sequence)
	}
}
