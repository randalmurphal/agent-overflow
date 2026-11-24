package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// A withdrawn queued message is not sent, queued or restored whatever copy
// of it a handoff, a dispatch or a recovery still holds: its durable row is
// what says it is still wanted.

// queueHeldMessage queues marker on threadID as an app message held for the
// next boundary, so queueing it flushes nothing, and returns its send id.
func queueHeldMessage(t *testing.T, a *App, threadID, marker string) string {
	t.Helper()
	sendID := workflowWakeSendIDRoot + entityid.New()
	if _, err := a.registerQueueItem(threadID, marker, SendMessageOptions{SendID: sendID}, injectedQueueOptions{
		preserveDraft: true, held: true, appWritten: true,
	}); err != nil {
		t.Fatal(err)
	}
	return sendID
}

// holdOnce returns a hook that stops its first caller until proceed closes,
// closing at once it is reached; later callers pass straight through.
func holdOnce() (hook func(string), at, proceed chan struct{}) {
	at, proceed = make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	return func(string) {
		if first.CompareAndSwap(false, true) {
			close(at)
			<-proceed
		}
	}, at, proceed
}

// gone fails if marker reached the provider, is queued, durable or in
// memory, or is in the thread's composer draft.
func (f *claimFixture) neverSeen(t *testing.T, sendID, marker string) {
	t.Helper()
	if ids := queuedSendIDsOf(t, f.app, f.caller.ID, sendID); len(ids) != 0 {
		t.Errorf("the withdrawn message's row is back: %q", ids)
	}
	for _, item := range f.app.queueSnapshotForThread(f.caller.ID) {
		if strings.Contains(item.Message, marker) {
			t.Errorf("the withdrawn message is queued again: %+v", item)
		}
	}
	if draft, _, err := f.app.store.GetThreadDraft(f.caller.ID); err != nil {
		t.Fatal(err)
	} else if strings.Contains(draft.Content, marker) {
		t.Errorf("the withdrawn message was restored to the composer: %q", draft.Content)
	}
	if n := f.sent(t, marker); n != 0 {
		t.Errorf("the withdrawn message reached the provider %d times", n)
	}
}

// A message withdrawn while a boundary's drain hands it to the dispatch, the
// handoff then held past a recovery of the session and its replacement's
// start, is not sent to the replacement, queued or restored when the
// handoff lands.
func TestAWithdrawalInTheHandoffOutlivesARecovery(t *testing.T) {
	t.Parallel()
	for _, recovery := range []string{"a session death", "a stop and a restart"} {
		t.Run(recovery, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			withdrawn := "withdrawn-" + entityid.New()
			sendID := queueHeldMessage(t, f.app, f.caller.ID, withdrawn)
			hook, at, proceed := holdOnce()
			f.app.flushHandoffForTest = hook
			drained := make(chan bool, 1)
			go func() { drained <- f.app.triage.FlushQueuedItems(f.caller.ID) }()
			within(t, "the handoff", at)
			if removal, err := f.app.removeQueuedItem(t.Context(), f.caller.ID, sendID); err != nil || removal != queueRemovalRemoved {
				t.Fatalf("the withdrawal = %v, %v; want it removed", removal, err)
			}
			if recovery == "a session death" {
				f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)
			}
			if err := f.app.StopSession(f.caller.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			close(proceed)
			within(t, "the handoff's end", drained)
			after := "after-" + entityid.New()
			queueTestWake(t, f.app, f.caller.ID, after)
			f.deliveredOnce(t, after)
			f.neverSeen(t, sendID, withdrawn)
		})
	}
}

// A session recovered after a dispatch released the thread's action lock,
// and before its worker retired the batch, restores nothing it withdrew
// while the dispatch waited for that lock.
func TestARecoveryAfterADispatchReturnsRestoresNoWithdrawnMessage(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatal(err)
	}
	unlock := f.app.threadLocks().Lock(f.caller.ID)
	withdrawn := "withdrawn-" + entityid.New()
	sendID := queueTestWake(t, f.app, f.caller.ID, withdrawn)
	heldAtTheActionLock(t, f.app, f.caller.ID)
	if removal, err := f.app.removeQueuedItem(t.Context(), f.caller.ID, sendID); err != nil || removal != queueRemovalRemoved {
		t.Fatalf("the withdrawal = %v, %v; want it removed", removal, err)
	}
	hook, at, proceed := holdOnce()
	f.app.flushDispatchReturnedForTest = hook
	unlock()
	within(t, "the dispatch's return", at)
	f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)
	close(proceed)
	f.settle(t)
	f.neverSeen(t, sendID, withdrawn)
}

// S r8 finding 1, and S r7 finding 1 with A r5 finding 2: a delete claims a
// live thread whose dispatch waits at the gate with batch A when an older
// boundary drain's handoff lands batch B behind it, B emptied by a
// withdrawal or not. The refused delete's release sends A, then B, once
// each, and leaves no row behind.
func TestABatchLandingWhileAnotherWaitsForTheDecisionIsSent(t *testing.T) {
	t.Parallel()
	for _, emptied := range []bool{true, false} {
		name := "B emptied by a withdrawal"
		if !emptied {
			name = "B still holding its message"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			a := "batch-a-" + entityid.New()
			b := "batch-b-" + entityid.New()
			refusedDelete(t, f.app, f.caller.ID, func() {
				bSendID := queueHeldMessage(t, f.app, f.caller.ID, b)
				hook, at, proceed := holdOnce()
				f.app.flushHandoffForTest = hook
				drained := make(chan bool, 1)
				go func() { drained <- f.app.triage.FlushQueuedItems(f.caller.ID) }()
				within(t, "B's handoff", at)
				queueTestWake(t, f.app, f.caller.ID, a)
				waitAtGates(t, f.app, 1)
				if emptied {
					if removal, err := f.app.removeQueuedItem(t.Context(), f.caller.ID, bSendID); err != nil || removal != queueRemovalRemoved {
						t.Fatalf("B's withdrawal = %v, %v; want it removed", removal, err)
					}
				}
				close(proceed)
				within(t, "B's handoff's end", drained)
				quietAtGates(t, f.app)
				if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
					t.Errorf("a claimed thread's provider was written %d messages", n)
				}
			})
			f.deliveredOnce(t, a)
			if emptied {
				if n := f.sent(t, b); n != 0 {
					t.Fatalf("the withdrawn B reached the provider %d times", n)
				}
			} else {
				f.deliveredOnce(t, b)
			}
			waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 0 })
			nothingWaits(t, f.app)
		})
	}
}

// queueHeldPersonMessage queues text on threadID as a person's message held
// for the next boundary, and returns its queue item id and send id.
func queueHeldPersonMessage(t *testing.T, a *App, threadID, text string) (string, string) {
	t.Helper()
	sendID := "send-" + entityid.New()
	item, err := a.registerQueueItem(threadID, text, SendMessageOptions{SendID: sendID}, injectedQueueOptions{preserveDraft: true, held: true})
	if err != nil {
		t.Fatal(err)
	}
	return item.ID, sendID
}

// codexQueue is a live Codex thread whose mock accepts every steer and
// echoes none: each queued message is its own dispatch group, and a message
// the dispatch wrote stays among the thread's unconfirmed sends.
type codexQueue struct {
	app          *App
	dbPath       string
	thread       store.Thread
	closeSession func() error
}

func newCodexQueue(t *testing.T) *codexQueue {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	app.configureTriageQueueCallbacks()
	thread := testThread("withdrawal-codex-" + entityid.New())
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "turn-3", ThreadID: thread.ID, TurnIndex: 3, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	q := &codexQueue{app: app, dbPath: dbPath, thread: thread}
	q.live(t)
	return q
}

// live gives the thread a new session.
func (q *codexQueue) live(t *testing.T) {
	t.Helper()
	sess := installSteerTestSession(t, q.app, q.thread, "ok")
	q.closeSession = sess.Close
	q.app.sessionManager().put(q.thread.ID, session{
		Provider: string(provider.Codex), Token: "tok-" + entityid.New(), Codex: sess, Liveness: newSessionLiveness(time.Now()),
	})
}

// flush drains the queue as a boundary does and waits out the dispatch.
func (q *codexQueue) flush(t *testing.T) {
	t.Helper()
	if !q.app.triage.FlushQueuedItems(q.thread.ID) {
		t.Fatal("the queue did not drain")
	}
	q.app.flushDispatch.wg.Wait()
}

// sent reports whether a dispatch wrote the message queued as id.
func (q *codexQueue) sent(id string) bool {
	for _, send := range q.app.triage.UnconfirmedFlushSends(q.thread.ID) {
		if send.QueueItemID == id {
			return true
		}
	}
	return false
}

// queued reports whether id is in the thread's queue snapshot and among its
// durable rows.
func (q *codexQueue) queued(t *testing.T, id string) (memory, durable bool) {
	t.Helper()
	for _, item := range q.app.queueSnapshotForThread(q.thread.ID) {
		memory = memory || item.ID == id
	}
	for _, row := range durableQueueRows(t, q.app, q.thread.ID) {
		durable = durable || row.ID == id
	}
	return memory, durable
}

// atGroupCommit runs do once, in the dispatch, at the nth group's commit
// point: right before it, or right after it when committed is true.
func (q *codexQueue) atGroupCommit(n int, committed bool, do func()) {
	var seen atomic.Int32
	q.app.flushGroupCommitForTest = func(_ string, at bool) {
		if at == committed && int(seen.Add(1)) == n {
			do()
		}
	}
}

func (q *codexQueue) withdraw(t *testing.T, sendID string) (queueRemoval, error) {
	return q.app.removeQueuedItem(t.Context(), q.thread.ID, sendID)
}

// A withdrawal and the commit point of its message's dispatch group, an
// earlier group of the batch already written, are ordered either way round:
// before the commit point the message is removed and never sent, after it
// the withdrawal finds it dispatched and it is sent. Never both, never
// neither.
func TestAWithdrawalAndItsGroupsCommitPointOrderOneMessage(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		name := "before the commit point"
		if committed {
			name = "after the commit point"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := newCodexQueue(t)
			first, _ := queueHeldPersonMessage(t, q.app, q.thread.ID, "first")
			second, sendID := queueHeldPersonMessage(t, q.app, q.thread.ID, "second")
			var removal queueRemoval
			var err error
			q.atGroupCommit(2, committed, func() { removal, err = q.withdraw(t, sendID) })
			q.flush(t)
			if err != nil {
				t.Fatal(err)
			}
			if !q.sent(first) {
				t.Fatal("the earlier group was not written")
			}
			removed, sent := removal == queueRemovalRemoved, q.sent(second)
			if removed == sent {
				t.Fatalf("the withdrawal = %v and the message sent = %v; want exactly one", removal, sent)
			}
			if committed {
				if removal != queueRemovalDispatched {
					t.Fatalf("the withdrawal after the commit point = %v, want dispatched", removal)
				}
				return
			}
			if !removed {
				t.Fatalf("the withdrawal before the commit point = %v, want removed", removal)
			}
			if memory, durable := q.queued(t, second); memory || durable {
				t.Fatalf("the withdrawn message is queued (memory %v, durable %v)", memory, durable)
			}
		})
	}
}

// A dispatch writes the first of three messages and fails the second, which
// puts the second and third back on the queue. A withdrawal of the third
// during the failing write removes it before the requeue reads the rows; a
// withdrawal of the second once the dispatch returned removes it, its write
// over though the worker still holds the batch. The
// next drain sends what is left, and never the withdrawn one.
func TestAWithdrawalAroundAFailedGroupsRequeue(t *testing.T) {
	t.Parallel()
	for _, during := range []bool{true, false} {
		name := "the unattempted third during the failing write"
		if !during {
			name = "the failed second after the requeue"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := newCodexQueue(t)
			first, _ := queueHeldPersonMessage(t, q.app, q.thread.ID, "first")
			second, secondSend := queueHeldPersonMessage(t, q.app, q.thread.ID, "second")
			third, thirdSend := queueHeldPersonMessage(t, q.app, q.thread.ID, "third")
			var removal queueRemoval
			var err, closeErr error
			q.atGroupCommit(2, true, func() {
				if during {
					removal, err = q.withdraw(t, thirdSend)
				}
				// The second message's write fails: its session is gone.
				closeErr = q.closeSession()
			})
			withdrawn, kept := third, second
			if !during {
				withdrawn, kept = second, third
				// Once the dispatch returned, before its worker retires the
				// batch: the failed group's write must be over by then.
				var once atomic.Bool
				q.app.flushDispatchReturnedForTest = func(string) {
					if once.CompareAndSwap(false, true) {
						removal, err = q.withdraw(t, secondSend)
					}
				}
			}
			q.flush(t)
			q.app.flushGroupCommitForTest, q.app.flushDispatchReturnedForTest = nil, nil
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if !q.sent(first) || q.sent(second) || q.sent(third) {
				t.Fatalf("written: first %v, second %v, third %v; want the first alone", q.sent(first), q.sent(second), q.sent(third))
			}
			if err != nil || removal != queueRemovalRemoved {
				t.Fatalf("the withdrawal = %v, %v; want it removed", removal, err)
			}
			if memory, durable := q.queued(t, withdrawn); memory || durable {
				t.Fatalf("the withdrawn message is queued (memory %v, durable %v)", memory, durable)
			}
			if memory, durable := q.queued(t, kept); !memory || !durable {
				t.Fatalf("the requeued message is not queued (memory %v, durable %v)", memory, durable)
			}
			q.live(t)
			q.flush(t)
			if !q.sent(kept) || q.sent(withdrawn) {
				t.Fatalf("the next drain wrote the kept message %v and the withdrawn one %v", q.sent(kept), q.sent(withdrawn))
			}
		})
	}
}

// A withdrawn message whose copy in triage's handoff still carries the
// timeline row an earlier dispatch left (QueuedFlushItem.StaleUserItemID)
// takes that row out of the timeline when the copy is dropped: at its group's
// commit point with a live session, or as its batch goes back to the queue
// for a resting thread with none. Nothing is sent.
func TestAWithdrawnCopyInTheHandoffTakesItsTimelineRow(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{true, false} {
		name := "at the commit point"
		if !live {
			name = "as its batch goes back"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := newCodexQueue(t)
			now := time.Now().UnixMilli()
			stale := "user:0:flush:1"
			if _, err := q.app.store.AppendItem(store.Item{
				ID: stale, ThreadID: q.thread.ID, TurnIndex: 0, Kind: "user_text", Role: "user", Status: "completed",
				Summary: "withdrawn", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			sendID := "send-" + entityid.New()
			id := "queue:" + entityid.New()
			payload := flushQueuePayloadJSON(t, flushQueuePayload{SendID: sendID})
			if err := q.app.store.InsertFlushQueueItem(store.FlushQueueItem{ID: id, ThreadID: q.thread.ID, SendID: sendID, Message: "withdrawn", Payload: payload, EnqueuedAt: now}); err != nil {
				t.Fatal(err)
			}
			q.app.triage.RegisterQueueItem(q.thread.ID, triage.QueuedFlushItem{ID: id, Message: "withdrawn", Payload: payload, EnqueuedAt: now, StaleUserItemID: stale})
			if !live {
				if _, ok := q.app.sessionManager().take(q.thread.ID); !ok {
					t.Fatal("the thread had no session")
				}
				if err := q.closeSession(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(q.app.resting.hold(q.thread.ID))
			}
			hook, at, proceed := holdOnce()
			q.app.flushHandoffForTest = hook
			drained := make(chan bool, 1)
			go func() { drained <- q.app.triage.FlushQueuedItems(q.thread.ID) }()
			within(t, "the handoff", at)
			if removal, err := q.withdraw(t, sendID); err != nil || removal != queueRemovalRemoved {
				t.Fatalf("the withdrawal = %v, %v; want it removed", removal, err)
			}
			if _, found, err := q.app.store.GetThreadItem(q.thread.ID, stale); err != nil || !found {
				t.Fatalf("the row went before its copy was dropped: found %v, %v", found, err)
			}
			close(proceed)
			within(t, "the handoff's end", drained)
			q.app.flushDispatch.wg.Wait()
			if q.sent(id) {
				t.Fatal("the withdrawn message was sent")
			}
			if _, found, err := q.app.store.GetThreadItem(q.thread.ID, stale); err != nil || found {
				t.Fatalf("the withdrawn copy's row is still in the timeline: found %v, %v", found, err)
			}
			if memory, durable := q.queued(t, id); memory || durable {
				t.Fatalf("the withdrawn message is queued (memory %v, durable %v)", memory, durable)
			}
		})
	}
}

// A message in triage's handoff when a recovery takes the session's queue
// goes where the recovery took the rest of the queue, never to the session
// that replaces it: a death's restore puts it in the composer, and a Stop's
// drop took its row with the rest. A message requeued without a row after
// its write goes the same way (S r8 finding 3): into the composer once
// after a death, and nowhere after a Stop. A drop of the overtaken
// generation that comes while the death's restore of the handoff waits for
// the thread's action lock, a revert's or a Stop's, has the last word: the
// handoff goes nowhere.
func TestAHandoffARecoveryOvertookGoesWithTheRestOfItsQueue(t *testing.T) {
	t.Parallel()
	drops := map[string]func(a *App, threadID string){
		"":                nil,
		", then a revert": func(a *App, threadID string) { a.dropFlushQueue(threadID) },
		", then a stop's drop": func(a *App, threadID string) {
			held, owed, _ := a.settleQueueForTeardown(threadID, false, dropQueue)
			a.giveBackToOwners(threadID, held)
			a.giveBackTeardownRequests(threadID, owed)
		},
	}
	for _, death := range []bool{true, false} {
		for later, drop := range drops {
			if !death && drop != nil {
				continue
			}
			name := "a session death" + later
			if !death {
				name = "a stop and a restart"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newClaimFixture(t)
				if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
					t.Fatal(err)
				}
				marker := "overtaken-" + entityid.New()
				id, _ := queueHeldPersonMessage(t, f.app, f.caller.ID, marker)
				rowless := "rowless-" + entityid.New()
				f.app.triage.RegisterQueueItem(f.caller.ID, triage.QueuedFlushItem{ID: "queue:" + entityid.New(), Message: rowless, NoRow: true})
				hook, at, proceed := holdOnce()
				f.app.flushHandoffForTest = hook
				drained := make(chan bool, 1)
				go func() { drained <- f.app.triage.FlushQueuedItems(f.caller.ID) }()
				within(t, "the handoff", at)
				if death {
					f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)
				} else if err := f.app.StopSession(f.caller.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
					t.Fatal(err)
				}
				unlock := func() {}
				if drop != nil {
					unlock = f.app.threadLocks().Lock(f.caller.ID)
				}
				close(proceed)
				within(t, "the handoff's end", drained)
				if drop != nil {
					drop(f.app, f.caller.ID)
					unlock()
				}
				f.settle(t)
				after := "after-" + entityid.New()
				queueTestWake(t, f.app, f.caller.ID, after)
				f.deliveredOnce(t, after)
				draft, _, err := f.app.store.GetThreadDraft(f.caller.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, overtaken := range []string{marker, rowless} {
					if n := f.sent(t, overtaken); n != 0 {
						t.Fatalf("the replacement session was written %q %d times", overtaken, n)
					}
					want := 0
					if death && drop == nil {
						want = 1
					}
					if n := strings.Count(draft.Content, overtaken); n != want {
						t.Fatalf("the composer holds %q %d times, want %d: %q", overtaken, n, want, draft.Content)
					}
					for _, item := range f.app.queueSnapshotForThread(f.caller.ID) {
						if strings.Contains(item.Message, overtaken) {
							t.Fatalf("%q is queued again: %+v", overtaken, item)
						}
					}
				}
				for _, row := range durableQueueRows(t, f.app, f.caller.ID) {
					if row.ID == id {
						t.Fatalf("the overtaken message's row is left: %+v", row)
					}
				}
			})
		}
	}
}

// A crash at a group's commit point, before the provider write, leaves the
// message's row where the next boot finds it: the boot puts it in the
// composer, as it would have without the commit point.
func TestACrashAtAGroupsCommitPointLeavesTheMessageToTheBoot(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatal(err)
	}
	marker := "crashed-" + entityid.New()
	queueHeldPersonMessage(t, f.app, f.caller.ID, marker)
	var draft string
	var readErr error
	var crashed atomic.Bool
	f.app.flushGroupCommitForTest = func(_ string, committed bool) {
		if !committed || !crashed.CompareAndSwap(false, true) {
			return
		}
		after := f.restart(t)
		after.app.restoreDurableFlushQueueAtBoot()
		restored, _, err := after.app.store.GetThreadDraft(f.caller.ID)
		draft, readErr = restored.Content, err
	}
	if !f.app.triage.FlushQueuedItems(f.caller.ID) {
		t.Fatal("the queue did not drain")
	}
	f.settle(t)
	if !crashed.Load() {
		t.Fatal("no group reached its commit point")
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(draft, marker) {
		t.Fatalf("the boot after the crash restored %q, want the message", draft)
	}
}

// A message the provider was handed and did not confirm goes back on the
// queue without its row, which went at the write, when a session end cannot
// put it in the composer, and so does an interrupted Codex message whose
// resend fails: the next drain sends each.
func TestAMessageRequeuedAfterItsWriteIsSentAgain(t *testing.T) {
	t.Parallel()
	t.Run("a session end's restore", func(t *testing.T) {
		t.Parallel()
		q := newCodexQueue(t)
		id, _ := queueHeldPersonMessage(t, q.app, q.thread.ID, "handed over")
		q.flush(t)
		if !q.sent(id) {
			t.Fatal("the message was not written")
		}
		execOnStore(t, q.dbPath, `CREATE TRIGGER refuse_draft_insert BEFORE INSERT ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
		execOnStore(t, q.dbPath, `CREATE TRIGGER refuse_draft_update BEFORE UPDATE ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
		requeued := q.app.restoreUnconfirmedQueueOnSessionDeath(q.thread.ID)
		execOnStore(t, q.dbPath, `DROP TRIGGER refuse_draft_insert`)
		execOnStore(t, q.dbPath, `DROP TRIGGER refuse_draft_update`)
		if len(requeued) != 1 || q.sent(id) {
			t.Fatalf("requeued %+v (still unconfirmed: %v), want the message back on the queue", requeued, q.sent(id))
		}
		q.live(t)
		q.flush(t)
		if !q.sent(id) {
			t.Fatal("the next drain did not send the requeued message")
		}
		// Written again with no row, and again not confirmed: the next
		// session end puts it in the composer.
		q.app.restoreUnconfirmedQueueOnSessionDeath(q.thread.ID)
		requireDraft(t, q.app, q.thread.ID, "handed over")
	})
	t.Run("a failed resend", func(t *testing.T) {
		t.Parallel()
		q := newCodexQueue(t)
		q.app.requeueEagerPersistedFlushes(q.thread.ID, []triage.EagerPersistedFlush{{
			UserItemID: "user:3:eager:1", Content: "interrupted", QueueItemID: "queue:interrupted", EnqueuedAt: 1,
			QueueMembers: []triage.FlushQueueMember{{QueueItemID: "queue:interrupted", NoRow: true}},
		}})
		q.flush(t)
		if !q.sent("queue:interrupted") {
			t.Fatal("the next drain did not send the requeued message")
		}
	})
}

// A worker's answer keeps its row past the provider write, until the echo
// confirms it: a withdrawal in between finds it dispatched and leaves the
// row, rather than reporting removed a message the worker was sent.
func TestAWithdrawalOfAWrittenAnswerAwaitingItsEchoFindsItDispatched(t *testing.T) {
	t.Parallel()
	q := newCodexQueue(t)
	sendID := workerAnswerSendIDRoot + entityid.New()
	execOnStore(t, q.dbPath, `INSERT INTO async_questions (thread_id, item_id, question_index, title, options, state, answer, send_id, created_at)
		VALUES ('`+q.thread.ID+`', 'question:ask', 0, 'Which scope?', '[]', 'submitted', 'One', '`+sendID+`', 1)`)
	item, err := q.app.registerQueueItem(q.thread.ID, "Question: Which scope?\nAnswer: One", SendMessageOptions{SendID: sendID}, injectedQueueOptions{
		preserveDraft: true, held: true, appWritten: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	q.flush(t)
	if !q.sent(item.ID) {
		t.Fatal("the answer was not written")
	}
	if _, durable := q.queued(t, item.ID); !durable {
		t.Fatal("the written answer's row went before its echo")
	}
	removal, err := q.withdraw(t, sendID)
	if err != nil || removal != queueRemovalDispatched {
		t.Fatalf("the withdrawal = %v, %v; want it dispatched", removal, err)
	}
	if _, durable := q.queued(t, item.ID); !durable {
		t.Fatal("the withdrawal deleted the written answer's row")
	}
}

// A recovery that takes the session's queue while a dispatch waits for a
// delete's decision has the batch: the dispatch, once the delete is refused,
// finds its generation ended and sends nothing, so the message is in the
// composer once and nowhere else, with a row or requeued without one.
func TestARecoveryWhileADispatchWaitsForTheDecisionKeepsTheBatch(t *testing.T) {
	t.Parallel()
	for _, rowless := range []bool{false, true} {
		name := "a queued message"
		if rowless {
			name = "a message requeued after its write"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			marker := "recovered-" + entityid.New()
			if rowless {
				f.app.triage.RegisterQueueItem(f.caller.ID, triage.QueuedFlushItem{ID: "queue:" + entityid.New(), Message: marker, NoRow: true})
			} else {
				queueHeldPersonMessage(t, f.app, f.caller.ID, marker)
			}
			refusedDelete(t, f.app, f.caller.ID, func() {
				go f.app.triage.FlushQueuedItems(f.caller.ID)
				waitAtGates(t, f.app, 1)
				f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)
			})
			nothingWaits(t, f.app)
			after := "after-" + entityid.New()
			queueTestWake(t, f.app, f.caller.ID, after)
			f.deliveredOnce(t, after)
			if n := f.sent(t, marker); n != 0 {
				t.Fatalf("the recovered message was sent %d times", n)
			}
			for _, item := range f.app.queueSnapshotForThread(f.caller.ID) {
				if strings.Contains(item.Message, marker) {
					t.Fatalf("the recovered message is queued again: %+v", item)
				}
			}
			draft, _, err := f.app.store.GetThreadDraft(f.caller.ID)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(draft.Content, marker); n != 1 {
				t.Fatalf("the composer holds the recovered message %d times: %q", n, draft.Content)
			}
		})
	}
}

// S r8 finding 2: a queued message never written to a provider, whose
// restore to the composer fails at a session's death, and which the person
// withdraws as the restore reports the failure, is gone: never queued again,
// in the composer or sent. So is one withdrawn after the restore requeued
// it and before the session's cleanup registers the requeue again.
func TestAMessageWithdrawnAroundAFailedRestoreStaysWithdrawn(t *testing.T) {
	t.Parallel()
	for _, at := range []string{"as the restore reports the failure", "before the requeue is registered again"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			withdrawn := "withdrawn-" + entityid.New()
			sendID := queueHeldMessage(t, f.app, f.caller.ID, withdrawn)
			withdraw := func() {
				if removal, err := f.app.removeQueuedItem(t.Context(), f.caller.ID, sendID); err != nil || removal != queueRemovalRemoved {
					t.Errorf("the withdrawal = %v, %v; want it removed", removal, err)
				}
			}
			if at == "as the restore reports the failure" {
				var once atomic.Bool
				f.app.triage.SetEventHook(func(evt provider.ProviderEvent) {
					f.bus.observeRouterEvent(evt)
					if evt.Kind == provider.EventError && evt.ThreadID == f.caller.ID && once.CompareAndSwap(false, true) {
						withdraw()
					}
				})
			}
			execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_draft_insert BEFORE INSERT ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
			execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_draft_update BEFORE UPDATE ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
			requeued := f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)
			execOnStore(t, f.dbPath, `DROP TRIGGER refuse_draft_insert`)
			execOnStore(t, f.dbPath, `DROP TRIGGER refuse_draft_update`)
			if at == "before the requeue is registered again" {
				if len(requeued) != 1 {
					t.Fatalf("requeued %+v, want the message whose restore failed", requeued)
				}
				withdraw()
				// What a session's teardown does once its cleanup wiped the queue.
				f.app.triage.CleanupThread(f.caller.ID)
				f.app.requeueUnconfirmedFlushItems(f.caller.ID, requeued, queueRestoredReasonSessionDied)
			}
			after := "after-" + entityid.New()
			queueTestWake(t, f.app, f.caller.ID, after)
			f.deliveredOnce(t, after)
			f.neverSeen(t, sendID, withdrawn)
		})
	}
}

// A withdrawal that comes once a requeue has read which rows stand, and
// before it queues the messages again, waits for the requeue and then takes
// the message back out: it is not shown queued again with its row gone. So
// for a failed restore's requeue, under the hold its rows were read in, and
// for a teardown's re-registration of what that restore requeued.
func TestAWithdrawalWaitsForARequeueThatReadItsRow(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"a failed restore", "a re-registration"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			withdrawn := "withdrawn-" + entityid.New()
			sendID := queueHeldMessage(t, f.app, f.caller.ID, withdrawn)
			var removal queueRemoval
			var removalErr error
			removed := make(chan struct{})
			var once atomic.Bool
			hook := func(threadID string) {
				if threadID != f.caller.ID || !once.CompareAndSwap(false, true) {
					return
				}
				go func() {
					defer close(removed)
					removal, removalErr = f.app.removeQueuedItem(context.Background(), f.caller.ID, sendID)
				}()
				// A withdrawal the requeue does not hold off finishes well within this.
				select {
				case <-removed:
				case <-time.After(time.Second):
				}
			}
			execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_draft_insert BEFORE INSERT ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
			execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_draft_update BEFORE UPDATE ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
			if by == "a failed restore" {
				f.app.requeueRowsReadForTest = hook
			}
			requeued := f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)
			execOnStore(t, f.dbPath, `DROP TRIGGER refuse_draft_insert`)
			execOnStore(t, f.dbPath, `DROP TRIGGER refuse_draft_update`)
			if by == "a re-registration" {
				f.app.triage.CleanupThread(f.caller.ID)
				f.app.requeueRowsReadForTest = hook
				f.app.requeueUnconfirmedFlushItems(f.caller.ID, requeued, queueRestoredReasonSessionDied)
			}
			within(t, "the withdrawal", removed)
			if removalErr != nil || removal != queueRemovalRemoved {
				t.Fatalf("the withdrawal = %v, %v; want it removed", removal, removalErr)
			}
			f.neverSeen(t, sendID, withdrawn)
			after := "after-" + entityid.New()
			queueTestWake(t, f.app, f.caller.ID, after)
			f.deliveredOnce(t, after)
			f.neverSeen(t, sendID, withdrawn)
		})
	}
}

// A message the provider was handed and a failed restore queues again gets
// its durable row back, so it is withdrawn like any queued message, and the
// requeue hands it back as queued: a withdrawal before a teardown's
// re-registration keeps it withdrawn, rather than the re-registration
// writing its row again.
func TestARequeuedUnconfirmedSendIsWithdrawnLikeAQueuedOne(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, app, "requeued-send")
	sendID := workflowWakeSendIDRoot + entityid.New()
	unlock := app.threadApplication().LockMutations(thread.ID)
	requeued, failures := app.requeueUnconfirmedFlushItemsLocked(thread.ID, []triage.UnconfirmedFlushItem{{
		QueueItemID: "queue:handed", Message: "handed over", EnqueuedAt: 1,
		QueueMembers: []triage.FlushQueueMember{{QueueItemID: "queue:handed", NoRow: true}},
		Payload:      flushQueuePayloadJSON(t, flushQueuePayload{SendID: sendID}),
	}}, queueRestoredReasonSessionDied)
	unlock()
	if failures.persist != nil || failures.retire != nil {
		t.Fatalf("requeue: %+v", failures)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 1 || rows[0].ID != "queue:handed" {
		t.Fatalf("durable rows after the requeue = %+v, want the message's row again", rows)
	}
	if queued := app.triage.QueuedFlushItems(thread.ID); len(queued) != 1 || queued[0].NoRow {
		t.Fatalf("queued = %+v, want the message with its row", queued)
	}
	if removal, err := app.removeQueuedItem(t.Context(), thread.ID, sendID); err != nil || removal != queueRemovalRemoved {
		t.Fatalf("the withdrawal = %v, %v; want it removed", removal, err)
	}
	// What a session's teardown does once its cleanup wiped the queue.
	app.triage.CleanupThread(thread.ID)
	app.requeueUnconfirmedFlushItems(thread.ID, requeued, queueRestoredReasonSessionDied)
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows after the re-registration = %+v, want the withdrawal to stand", rows)
	}
	if queued := app.triage.QueuedFlushItems(thread.ID); len(queued) != 0 {
		t.Fatalf("queued after the re-registration = %+v, want nothing", queued)
	}
}

// A joined message one of whose messages was withdrawn, and which does not
// split by message (its parts do not describe its text, or its row does not
// decode), goes to the composer whole, the withdrawn text with it, as any
// join that does not split: it is not queued or sent again, its timeline row
// goes, and the withdrawn message is not owed again. When the composer write
// fails the thread says so, and nothing is queued either.
func TestAnUnsplittableJoinWithAWithdrawnMessageGoesToTheComposerWhole(t *testing.T) {
	t.Parallel()
	const own, wakeText = "my own words", "The board loop fired."
	content := own + agentmeta.JoinSeparator + wakeText
	for _, variant := range []string{"corrupt parts in the row", "corrupt parts in the payload", "undecodable meta"} {
		for _, entry := range []string{"a session end's restore", "a requeue"} {
			for _, draftFails := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s, %s, draft write fails %v", variant, entry, draftFails), func(t *testing.T) {
					t.Parallel()
					q := newCodexQueue(t)
					wake, err := q.app.store.RegisterAgentWake(store.AgentWake{
						ID: "wake-" + entityid.New(), ThreadID: q.thread.ID, Kind: store.AgentWakeLoop, Label: "board",
						IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
					})
					if err != nil {
						t.Fatal(err)
					}
					person, wakeSend := "person-"+entityid.New(), agentWakeSendID(wake.ID, 1000)
					sendIDs := []string{person, wakeSend}
					parts := []agentmeta.JoinedPart{{SendID: person, Bytes: len(own)}, {SendID: wakeSend, Bytes: len(wakeText)}}
					corrupt := slices.Clone(parts)
					corrupt[0].Bytes = 3
					item := triage.UnconfirmedFlushItem{
						Message: content, EnqueuedAt: 1,
						QueueMembers: []triage.FlushQueueMember{
							// The wake's row is gone: it was withdrawn.
							{QueueItemID: "queue:wake", SendID: wakeSend},
							{QueueItemID: "queue:person", SendID: person, NoRow: true},
						},
					}
					row := store.Item{
						ID: "user:0:flush:joined", ThreadID: q.thread.ID, Kind: "user_text", Role: "user", Status: "completed",
						Summary: content, CreatedAt: 1, UpdatedAt: 1,
					}
					switch variant {
					case "corrupt parts in the row":
						if row.Meta, err = usermessage.MarshalMeta(usermessage.Meta{SendID: person, JoinedSendIDs: sendIDs, JoinedParts: corrupt}); err != nil {
							t.Fatal(err)
						}
					case "corrupt parts in the payload":
						item.Payload = flushQueuePayloadJSON(t, flushQueuePayload{SendID: person, JoinedSendIDs: sendIDs, JoinedParts: corrupt})
					default:
						row.Meta = `{"joinedSendIds":5}`
						item.JoinedParts = parts
					}
					if item.Payload == nil {
						if _, err := q.app.store.AppendItem(row); err != nil {
							t.Fatal(err)
						}
						item.UserItemID, item.QuietItem = row.ID, &row
					}
					if draftFails {
						execOnStore(t, q.dbPath, `CREATE TRIGGER refuse_draft_insert BEFORE INSERT ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
						execOnStore(t, q.dbPath, `CREATE TRIGGER refuse_draft_update BEFORE UPDATE ON thread_drafts BEGIN SELECT RAISE(ABORT, 'injected draft failure'); END`)
					}
					var settled atomic.Int32
					item.Settlement = triage.NewFlushSettlement(func(string) { settled.Add(1) })

					if entry == "a requeue" {
						item.StaleUserItemID = item.UserItemID
						q.app.requeueUnconfirmedFlushItems(q.thread.ID, []triage.UnconfirmedFlushItem{item}, queueRestoredReasonSessionDied)
					} else if requeued := q.app.restoreFlushItemsToDraftLocked(q.thread.ID, []triage.UnconfirmedFlushItem{item}, queueRestoredReasonSessionDied); len(requeued) != 0 {
						t.Fatalf("requeued %+v, want nothing", requeued)
					}

					if queued := q.app.triage.QueuedFlushItems(q.thread.ID); len(queued) != 0 {
						t.Fatalf("queued = %+v, want nothing", queued)
					}
					if rows := durableQueueRows(t, q.app, q.thread.ID); len(rows) != 0 {
						t.Fatalf("durable rows = %+v, want none", rows)
					}
					if settled.Load() != 1 {
						t.Fatalf("settled %d times, want once: the message is not coming back", settled.Load())
					}
					failures := threadErrorRows(t, q.app.store, q.thread.ID, "Could not restore queued messages")
					if draftFails {
						if len(failures) == 0 {
							t.Fatal("the failed composer write left the thread no error")
						}
						return
					}
					if len(failures) != 0 {
						t.Fatalf("errors %q, want none", failures)
					}
					requireDraft(t, q.app, q.thread.ID, content)
					if _, found, err := q.app.store.GetThreadItem(q.thread.ID, row.ID); err != nil || found {
						t.Fatalf("the message's row is still in the timeline: found %v, %v", found, err)
					}
					if saved, found, err := q.app.store.GetAgentWake(wake.ID); err != nil || !found || saved.Undelivered != "" {
						t.Fatalf("the withdrawn wake was owed again: %+v, found %v, %v", saved, found, err)
					}
				})
			}
		}
	}
}

// A requeue splits a worker's answer that keeps its row from the joined
// message it was sent in, so a later withdrawal can take it alone. When the
// rest does not split from it, none of it is queued again, where a
// withdrawal could not take the answer alone: all of it goes to the composer,
// and the answer goes back to its questions, as any join that does not split.
func TestARequeuedJoinThatDoesNotSplitFromItsAnswerGoesToTheComposerWhole(t *testing.T) {
	t.Parallel()
	q := newCodexQueue(t)
	const own, answer = "my own words", "Question: Which scope?\nAnswer: One"
	content := own + agentmeta.JoinSeparator + answer
	person, answerSend := "person-"+entityid.New(), workerAnswerSendIDRoot+entityid.New()
	execOnStore(t, q.dbPath, `INSERT INTO async_questions (thread_id, item_id, question_index, title, options, state, answer, send_id, created_at)
		VALUES ('`+q.thread.ID+`', 'question:ask', 0, 'Which scope?', '[]', 'submitted', 'One', '`+answerSend+`', 1)`)
	if err := q.app.store.InsertFlushQueueItem(store.FlushQueueItem{ID: "queue:answer", ThreadID: q.thread.ID, SendID: answerSend, Message: answer, EnqueuedAt: 1}); err != nil {
		t.Fatal(err)
	}
	meta, err := usermessage.MarshalMeta(usermessage.Meta{SendID: person, JoinedSendIDs: []string{person, answerSend}, JoinedParts: []agentmeta.JoinedPart{
		{SendID: person, Bytes: 3}, {SendID: answerSend, Bytes: len(answer)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	row := store.Item{
		ID: "user:0:flush:joined", ThreadID: q.thread.ID, Kind: "user_text", Role: "user", Status: "completed",
		Summary: content, Meta: meta, CreatedAt: 1, UpdatedAt: 1,
	}
	if _, err := q.app.store.AppendItem(row); err != nil {
		t.Fatal(err)
	}

	q.app.requeueUnconfirmedFlushItems(q.thread.ID, []triage.UnconfirmedFlushItem{{
		UserItemID: row.ID, Message: content, EnqueuedAt: 1, QuietItem: &row, StaleUserItemID: row.ID,
		QueueMembers: []triage.FlushQueueMember{
			{QueueItemID: "queue:answer", SendID: answerSend},
			{QueueItemID: "queue:person", SendID: person, NoRow: true},
		},
	}}, queueRestoredReasonSessionDied)

	if queued := q.app.triage.QueuedFlushItems(q.thread.ID); len(queued) != 0 {
		t.Fatalf("queued = %+v, want nothing", queued)
	}
	if rows := durableQueueRows(t, q.app, q.thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows = %+v, want none", rows)
	}
	requireDraft(t, q.app, q.thread.ID, content)
	if _, found, err := q.app.store.GetThreadItem(q.thread.ID, row.ID); err != nil || found {
		t.Fatalf("the message's row is still in the timeline: found %v, %v", found, err)
	}
	if questions, err := q.app.store.ListAsyncQuestions(q.thread.ID, "question:ask"); err != nil || len(questions) != 1 || questions[0].State != "unanswered" {
		t.Fatalf("questions = %+v, %v; want the answer's question unanswered again", questions, err)
	}
}

// A teardown joins its requeue's outcome to the restore's: the composer
// restores of both are finished, so neither's failure goes unreported.
func TestARequeueOutcomeJoinKeepsBothComposerRestores(t *testing.T) {
	t.Parallel()
	first, second := errors.New("first"), errors.New("second")
	joined := requeueOutcome{composer: []flushRestore{{failed: first}}}.join(requeueOutcome{composer: []flushRestore{{failed: second}}})
	if len(joined.composer) != 2 || joined.composer[0].failed != first || joined.composer[1].failed != second {
		t.Fatalf("joined composer restores = %+v, want both", joined.composer)
	}
}
