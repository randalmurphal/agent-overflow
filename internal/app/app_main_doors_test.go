package app

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// The queue dispatcher starts the session a person's queued message needs,
// and its start is a door like any other: it starts nothing on a thread a
// Stop brought to rest or an archived thread. A refusal there is not a
// failed start: the message stays queued, the composer and the timeline are
// untouched, and no failure streak begins. An owned worker is no fence: its
// queue starts it. A delete's claim is waited on instead
// (TestTheDispatcherStartWaitsForADeleteDecision).
func TestTheDispatcherStartsNoFencedThread(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	var mu sync.Mutex
	starts := map[string]int{}
	f.app.startSessionFn = func(threadID string) error {
		mu.Lock()
		starts[threadID]++
		mu.Unlock()
		return nil
	}
	startsOf := func(threadID string) int {
		mu.Lock()
		defer mu.Unlock()
		return starts[threadID]
	}
	worker := f.settledSpawn(t, "door-owned", f.caller.ID)
	if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		thread store.Thread
		fence  func(t *testing.T, threadID string)
		starts bool
	}{
		{name: "rested", thread: f.plainThread(t), fence: func(t *testing.T, id string) {
			t.Cleanup(f.app.resting.hold(id))
		}},
		{name: "archived", thread: f.plainThread(t), fence: func(t *testing.T, id string) {
			if _, _, err := f.app.store.ArchiveThread(id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "owned worker", thread: worker, fence: func(*testing.T, string) {}, starts: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.thread.ID
			// Queued while the dispatcher waits, then fenced, then dispatched.
			holdFlushQueue(f.app)
			_, err := f.app.RegisterQueueItem(t.Context(), id, "door message "+tc.name, SendMessageOptions{SendID: "door-send-" + id})
			f.app.triage.SetFlushDispatcher(f.app.enqueueFlushDispatch)
			if err != nil {
				t.Fatalf("RegisterQueueItem: %v", err)
			}
			tc.fence(t, id)
			f.app.triage.FlushQueuedItems(id)
			if tc.starts {
				waitUntil(t, 10*time.Second, func() bool { return startsOf(id) > 0 })
				return
			}
			waitUntil(t, 10*time.Second, func() bool {
				f.app.flushDispatch.mu.Lock()
				_, busy := f.app.flushDispatch.running[id]
				f.app.flushDispatch.mu.Unlock()
				return !busy && len(f.app.triage.LiveStateSnapshotForThread(id).QueueItems) == 1
			})
			if n := startsOf(id); n != 0 {
				t.Errorf("the dispatcher started the %s thread %d times", tc.name, n)
			}
			if draft, _, err := f.app.store.GetThreadDraft(id); err != nil || draft.Content != "" {
				t.Errorf("composer = %q (%v), want the message still queued, not restored", draft.Content, err)
			}
			items, err := f.app.store.ListItems(id)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.Kind == "error" || strings.Contains(item.Summary, "could not be sent") {
					t.Errorf("the refusal wrote %s row %q", item.Kind, item.Summary)
				}
			}
			f.app.agentNoticeWork.mu.Lock()
			_, failing := f.app.agentNoticeWork.failing[id]
			f.app.agentNoticeWork.mu.Unlock()
			if failing {
				t.Error("the refusal started a failure streak")
			}
		})
	}
}

// A dispatcher start for a person's queued message on a thread with no
// session waits for a delete's decision when the delete claimed the thread
// before the dispatch read it, or while the start ran: it holds no lock,
// starts nothing and records no failure meanwhile, and the message stays
// out of the composer. A refused delete's thread is started and sent the
// message once; a committed one starts nothing and its message is its
// teardown's.
func TestTheDispatcherStartWaitsForADeleteDecision(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the dispatch", "during the start"} {
		for _, refuse := range []bool{true, false} {
			t.Run(when+"/"+map[bool]string{true: "refused", false: "committed"}[refuse], func(t *testing.T) {
				t.Parallel()
				f := newClaimFixture(t)
				id := f.caller.ID
				marker := "queued-" + entityid.New()
				holdFlushQueue(f.app)
				if _, err := f.app.RegisterQueueItem(t.Context(), id, marker, SendMessageOptions{SendID: "door-send-" + entityid.New()}); err != nil {
					t.Fatalf("RegisterQueueItem: %v", err)
				}
				f.app.triage.SetFlushDispatcher(f.app.enqueueFlushDispatch)
				waiting := func() {
					waitAtGates(t, f.app, 1)
					if _, live := f.app.sessionManager().get(id); live {
						t.Error("a claimed thread's session started")
					}
					if n := f.sent(t, marker); n != 0 {
						t.Errorf("a claimed thread's message reached the provider %d times", n)
					}
					if draft, _, err := f.app.store.GetThreadDraft(id); err != nil || draft.Content != "" {
						t.Errorf("composer = %q (%v), want the message still queued", draft.Content, err)
					}
				}
				var done <-chan error
				if when == "before the dispatch" {
					done = deleteOf(t, f.app, id, refuse, func() {
						f.app.triage.FlushQueuedItems(id)
						waiting()
					})
				} else {
					atStart, claimed := make(chan struct{}), make(chan struct{})
					var once sync.Once
					f.app.startSessionFn = func(threadID string) error {
						once.Do(func() {
							close(atStart)
							<-claimed
						})
						return f.app.startSessionNow(threadID)
					}
					f.app.triage.FlushQueuedItems(id)
					within(t, "the dispatcher's start", atStart)
					done = deleteOf(t, f.app, id, refuse, waiting)
					close(claimed)
				}
				decided(t, done, refuse)
				if !refuse {
					nothingWaits(t, f.app)
					if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
						t.Fatalf("the committed thread's provider was written %d messages", n)
					}
					return
				}
				f.deliveredOnce(t, marker)
				items, err := f.app.store.ListItems(id)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					if item.Kind == "error" || strings.Contains(item.Summary, "could not be sent") {
						t.Errorf("the wait wrote %s row %q", item.Kind, item.Summary)
					}
				}
				f.app.agentNoticeWork.mu.Lock()
				_, failing := f.app.agentNoticeWork.failing[id]
				f.app.agentNoticeWork.mu.Unlock()
				if failing {
					t.Error("the wait started a failure streak")
				}
			})
		}
	}
}

// Every door that may put a never-sent thread on the sidebar list (a draft
// write, an app row, an unarchive) asks the store's listing rule, which
// lists no owned worker: the worker's frames go out `full`, so no client
// shows a sidebar row for it, while a plain thread through the same door is
// listed.
func TestNoListingDoorListsAnOwnedWorker(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	rec := &emitRecorder{}
	f.app.testEmitHook = rec.capture
	doors := map[string]func(t *testing.T, id string){
		"draft write": func(t *testing.T, id string) {
			if err := f.app.writeThreadDraft(transport.ClientIdentity{}, store.ThreadDraft{
				ThreadID: id, Content: "typed", Attachments: "[]", TerminalChips: "[]",
			}); err != nil {
				t.Fatal(err)
			}
		},
		"app row": func(t *testing.T, id string) {
			f.app.emitErrorToThread(id, "something failed")
		},
		"unarchive": func(t *testing.T, id string) {
			if err := f.app.writeThreadDraft(transport.ClientIdentity{}, store.ThreadDraft{
				ThreadID: id, Content: "typed", Attachments: "[]", TerminalChips: "[]",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.ArchiveThread(id); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.UnarchiveThread(id); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, open := range doors {
		t.Run(name, func(t *testing.T) {
			worker := f.plainThread(t)
			recordSpawn(t, f.app, "tok-listing-"+worker.ID, f.caller.ID, worker.ID)
			plain := f.plainThread(t)
			rec.reset()
			open(t, worker.ID)
			open(t, plain.ID)
			calls := rec.snapshot()
			for _, frame := range rowFramesOf(calls, worker.ID) {
				if frame.Action == triage.ThreadActionListed {
					t.Errorf("%s listed the owned worker: %+v", name, frame)
				}
			}
			shown, _ := sidebarShown(calls)
			if shown[worker.ID] {
				t.Errorf("%s gave the owned worker a sidebar row", name)
			}
			if !shown[plain.ID] {
				t.Errorf("%s did not list the plain thread", name)
			}
		})
	}
}

// A watch script an assistant runs counts as work in its thread and in the
// checkout it runs in, so the gates that ask runningWorkIn (worktree
// removal, transfer, the update) wait for it, and it stops counting once
// the script ended.
func TestARunningWatchScriptIsWorkInItsThreadAndCheckout(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	thread := f.plainThread(t)
	checkout := t.TempDir()
	run := &agentWakeRun{threadID: thread.ID}
	run.runIn(filepath.Join(checkout, "watched"))
	f.app.agentWakes.mu.Lock()
	if f.app.agentWakes.runs == nil {
		f.app.agentWakes.runs = map[string]*agentWakeRun{}
	}
	f.app.agentWakes.runs["door-watch"] = run
	f.app.agentWakes.mu.Unlock()
	for name, scope := range map[string]workScope{
		"thread":   {threads: []string{thread.ID}},
		"checkout": {dir: checkout},
	} {
		work, err := f.app.runningWorkIn(scope)
		if err != nil || !work.running() || work.kind != workAgentWatch {
			t.Errorf("%s: running work = %+v, %v; want the watch script", name, work, err)
		}
	}
	// The watch stays with its assistant on this computer, so it holds no
	// copy of the conversation back.
	if err := f.app.checkTransferIdle(thread); err != nil {
		t.Errorf("a transfer of the thread whose watch runs was refused: %v", err)
	}
	run.runIn("")
	if work, err := f.app.runningWorkIn(workScope{dir: checkout}); err != nil || work.running() {
		t.Errorf("after the script ended: running work = %+v, %v; want none", work, err)
	}
}
