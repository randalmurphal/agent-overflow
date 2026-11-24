package app

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/wake"
)

// A claimed thread takes what the app queues for it and starts nothing: a
// message delivered while a delete decides is in the thread's queue at
// once, no session starts and no queued input reaches a live one while the
// starts wait for the decision, a refused delete's starts go on, a committed
// delete's teardown gives the queue back to its owners, and a restart's
// boot gives it back the same way.

// errRefusedForTest refuses a delete at its verify (deleteRequest.admit).
var errRefusedForTest = errors.New("refused for the test")

// refusedDelete runs a delete of threadID that is refused at its verify,
// with during run once its claim is written.
func refusedDelete(t *testing.T, a *App, threadID string, during func()) {
	t.Helper()
	a.deleteClaimedForTest = func([]string) { during() }
	_, err := a.deleteThreads(deleteRequest{seeds: []string{threadID}, admit: func(string) error { return errRefusedForTest }})
	a.deleteClaimedForTest = nil
	if !errors.Is(err, errRefusedForTest) {
		t.Fatalf("the delete = %v, want it refused", err)
	}
	untouched(t, a, threadID)
}

// committedDelete runs a delete of threadID to its end, with during run
// once its claim is written, before it verifies.
func committedDelete(t *testing.T, a *App, threadID string, during func()) {
	t.Helper()
	a.deleteClaimedForTest = func([]string) { during() }
	result, err := a.DeleteThread(threadID)
	a.deleteClaimedForTest = nil
	if err != nil || len(result.WorkersNotDeleted) > 0 {
		t.Fatalf("the delete = %+v, %v", result, err)
	}
}

// runSweeps runs the app's unattended sweep for the rest of the test.
func runSweeps(t *testing.T, a *App) {
	t.Helper()
	a.appCtx, a.appCancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		a.appCancel()
		a.threadRequestsWG.Wait()
	})
	a.startThreadRequestSweeps()
}

// crashedDelete runs a delete of threadID that stops for good once its
// claim is written and during has run, as one a quit cut short does: the
// claim stays for the next boot to release, and the gates go with the
// process (abandonGates).
func crashedDelete(t *testing.T, a *App, threadID string, during func()) {
	t.Helper()
	a.deleteClaimedForTest = func([]string) {
		during()
		runtime.Goexit()
	}
	stopped := make(chan bool, 1)
	go func() {
		returned := false
		defer func() { stopped <- returned }()
		_, _ = a.DeleteThread(threadID)
		returned = true
	}()
	if within(t, "the interrupted delete", stopped) {
		t.Fatal("the delete returned instead of stopping")
	}
	a.deleteClaimedForTest = nil
	abandonGates(a)
}

// afterTeardown runs check on threadID once a delete's teardown of it has
// given its queue back, before its row drops: the owners a thread's rows
// cascade with are still there to read.
func afterTeardown(a *App, threadID string, check func()) {
	a.deletePortsForTest = func(ports *threadapp.DeletePorts) {
		stop := ports.StopSession
		ports.StopSession = func(id string) error {
			err := stop(id)
			if id == threadID {
				check()
			}
			return err
		}
	}
}

// claimFixture is a stopped thread whose provider is a mock that records
// every line it is written: a start registers its session and flushes the
// queue to it along the real path.
type claimFixture struct {
	*requestFixture
	stdin string
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	f.app.triage.SetPromptRouter(f.app.routeWorkerPrompt)
	f.bus.mu.Lock()
	f.bus.tap = f.app.tapWorkerPrompt
	f.bus.mu.Unlock()
	return &claimFixture{requestFixture: f, stdin: f.mockClaudeRecording(t, "noted")}
}

// sent counts the user messages the provider was written that carry marker.
func (f *claimFixture) sent(t *testing.T, marker string) int {
	t.Helper()
	count := 0
	for _, envelope := range recordedUserEnvelopes(t, f.stdin) {
		if text, _ := envelopeText(t, envelope); strings.Contains(text, marker) {
			count++
		}
	}
	return count
}

// settle waits out the notice starts and dispatches already begun. One
// waiting for a delete's decision is waited out only by that decision: a
// test that holds the decision waits with quietAtGates.
func (f *claimFixture) settle(t *testing.T) {
	t.Helper()
	f.app.waitAgentNoticeWork()
	if err := f.app.drainWorkerPrompts(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	f.app.waitAgentNoticeWork()
	f.app.flushDispatch.wg.Wait()
}

// startedNothing fails if the thread has a session or its provider was
// written anything, once every start begun has finished or waits for a
// delete's decision.
func (f *claimFixture) startedNothing(t *testing.T) {
	t.Helper()
	quietAtGates(t, f.app)
	if _, live := f.app.sessionManager().get(f.caller.ID); live {
		t.Error("a session started on a claimed thread")
	}
	if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
		t.Errorf("the provider of a claimed thread was written %d messages", n)
	}
	// A start the claim turned away is no failure.
	f.app.agentNoticeWork.mu.Lock()
	_, failing := f.app.agentNoticeWork.failing[f.caller.ID]
	f.app.agentNoticeWork.mu.Unlock()
	if failing {
		t.Error("a start the claim turned away was recorded as a failed start")
	}
}

// deliveredOnce waits for marker to reach the provider, and fails unless it
// reached it once.
func (f *claimFixture) deliveredOnce(t *testing.T, marker string) {
	t.Helper()
	awaitSent(t, f.stdin, func(text string) bool { return strings.Contains(text, marker) })
	f.settle(t)
	if n := f.sent(t, marker); n != 1 {
		t.Fatalf("the provider was written %q %d times, want once", marker, n)
	}
}

// queuedSendIDsOf lists the durable queue's send ids of threadID with prefix.
func queuedSendIDsOf(t *testing.T, a *App, threadID, prefix string) []string {
	t.Helper()
	var out []string
	for _, row := range durableQueueRows(t, a, threadID) {
		if strings.HasPrefix(row.SendID, prefix) {
			out = append(out, row.SendID)
		}
	}
	return out
}

// owedReplyWake stores answer as the reply to a request caller made, its
// wake armed, owed and due now, with spent of its retry attempts already
// used.
func owedReplyWake(t *testing.T, a *App, caller, token, answer string, spent int64) {
	t.Helper()
	if err := a.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSend, State: store.ThreadRequestRunning}); err != nil {
		t.Fatal(err)
	}
	if settled, err := a.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestReplied, Answer: []byte(answer), AnswerKind: store.ThreadAnswerReply, ArmWake: true,
	}); err != nil || !settled {
		t.Fatalf("settle %s = %v, %v", token, settled, err)
	}
	if booked, err := a.store.ScheduleThreadRequestWake(token, 0, spent); err != nil || !booked {
		t.Fatalf("schedule the wake of %s = %v, %v", token, booked, err)
	}
}

func requestRow(t *testing.T, a *App, token string) store.ThreadRequest {
	t.Helper()
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found {
		t.Fatalf("request %s = %v, %v", token, found, err)
	}
	return row
}

func wakeRow(t *testing.T, a *App, id string) store.AgentWake {
	t.Helper()
	row, found, err := a.store.GetAgentWake(id)
	if err != nil || !found {
		t.Fatalf("wake %s = %v, %v", id, found, err)
	}
	return row
}

// claimedDelivery is one kind of message the app queues for a thread,
// delivered while a delete holds the thread's claim.
type claimedDelivery struct {
	// marker is in the message's text.
	marker string
	// deliver delivers it the way the app does and returns the send id it
	// is queued under.
	deliver func(t *testing.T) string
	// returned fails unless the message's owner has it back, read through
	// a: at a committed delete's teardown, and after a restart's boot.
	returned func(t *testing.T, a *App)
	// committed, when set, replaces returned at a committed delete's
	// teardown: a wake the thread itself owns ends with the thread.
	committed func(t *testing.T, a *App)
	// delivered, when set, checks the owner after a refused delete's
	// release delivered the message.
	delivered func(t *testing.T)
	// redeliver, after a restart's boot, has the owner deliver it again as
	// the owner does; nil for an owner that keeps it for a read. Each kind
	// redelivers under the send id prefix again.
	redeliver func(t *testing.T, after *requestFixture)
	again     string
}

// claimedDeliveries are the app's deliveries to a thread, by kind, built on
// f's thread.
func claimedDeliveries() map[string]func(t *testing.T, f *claimFixture) claimedDelivery {
	requestReturned := func(token string, late bool) func(t *testing.T, a *App) {
		return func(t *testing.T, a *App) {
			t.Helper()
			row := requestRow(t, a, token)
			delivered := row.DeliveredAt
			if late {
				delivered = row.LateDeliveredAt
			}
			if delivered != 0 || row.Notify {
				t.Errorf("the request = %+v, want its message given back to it, disarmed", row)
			}
		}
	}
	wakeReturned := func(id, marker string) func(t *testing.T, a *App) {
		return func(t *testing.T, a *App) {
			t.Helper()
			if row := wakeRow(t, a, id); row.Undelivered == "" || !strings.Contains(row.LastOutput, marker) {
				t.Errorf("the wake = %+v, want its message recorded as not delivered and kept", row)
			}
		}
	}
	wakeEnded := func(id string) func(t *testing.T, a *App) {
		return func(t *testing.T, a *App) {
			t.Helper()
			if row := wakeRow(t, a, id); row.State != store.AgentWakeCancelled {
				t.Errorf("the wake = %+v, want it ended with its thread", row)
			}
		}
	}
	wakeReplayed := func(id string) func(t *testing.T, after *requestFixture) {
		return func(t *testing.T, after *requestFixture) {
			t.Helper()
			if row := wakeRow(t, after.app, id); row.State != store.AgentWakeArmed || row.NextAt > time.Now().UnixMilli() {
				t.Fatalf("the wake after the boot = %+v, want it due", row)
			}
			after.app.recoverAgentWakesAtBoot()
		}
	}
	return map[string]func(t *testing.T, f *claimFixture) claimedDelivery{
		"reply": func(t *testing.T, f *claimFixture) claimedDelivery {
			token := "reply-" + entityid.New()
			owedReplyWake(t, f.app, f.caller.ID, token, "reply "+token, 0)
			return claimedDelivery{
				marker: "reply " + token,
				deliver: func(*testing.T) string {
					f.app.retryUndeliveredThreadWakes(time.Now())
					return threadWakeSendID(token)
				},
				returned: requestReturned(token, false),
				again:    threadWakeSendID(token),
			}
		},
		"late reply": func(t *testing.T, f *claimFixture) claimedDelivery {
			token := "late-" + entityid.New()
			owedReplyWake(t, f.app, f.caller.ID, token, "first answer", 0)
			if _, err := f.app.store.MarkThreadRequestDelivered(token, store.ThreadWakeQueued, 0, false); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.store.ConfirmThreadWake(token, false, "", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.store.StoreThreadRequestLateReply(token, []byte("late "+token), 0, nil, true); err != nil {
				t.Fatal(err)
			}
			return claimedDelivery{
				marker: "late " + token,
				deliver: func(*testing.T) string {
					f.app.retryUndeliveredThreadWakes(time.Now())
					return threadWakeLateSendID(token)
				},
				returned: requestReturned(token, true),
				again:    threadWakeLateSendID(token),
			}
		},
		"reminder": func(t *testing.T, f *claimFixture) claimedDelivery {
			token := "remind-" + entityid.New()
			insertTestReminder(t, f.app, token, f.caller.ID, "remind "+token, time.Now().Add(-time.Second).UnixMilli())
			return claimedDelivery{
				marker: "remind " + token,
				deliver: func(*testing.T) string {
					f.app.fireDueThreadReminders(time.Now())
					return threadWakeSendID(token)
				},
				returned: requestReturned(token, false),
				again:    threadWakeSendID(token),
			}
		},
		"loop wake": func(t *testing.T, f *claimFixture) claimedDelivery {
			marker := "loop " + entityid.New()
			loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: marker,
				IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
			return claimedDelivery{
				marker: marker,
				deliver: func(t *testing.T) string {
					now := time.Now()
					if err := f.app.fireAgentWake(loop, now, agentWakeFire{at: now.UnixMilli()}); err != nil {
						t.Fatal("the fire did not start")
					}
					return agentWakeSendID(loop.ID, now.UnixMilli())
				},
				returned:  func(t *testing.T, a *App) { wakeReturnedLoop(t, a, loop.ID) },
				committed: wakeEnded(loop.ID),
				redeliver: wakeReplayed(loop.ID),
				again:     agentWakeSendIDPrefix(loop.ID),
			}
		},
		"watch wake": func(t *testing.T, f *claimFixture) claimedDelivery {
			// A watch is a standing agent's.
			testAssistant(t, f.app, f.caller.ID)
			marker := "watch-" + entityid.New()
			watch := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeWatch, Label: "inbox",
				Command: "echo " + marker, IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
			return claimedDelivery{
				marker: marker,
				deliver: func(t *testing.T) string {
					now := time.Now()
					if err := f.app.fireAgentWake(watch, now, agentWakeFire{at: now.UnixMilli()}); err != nil {
						t.Fatal("the fire did not start")
					}
					return agentWakeSendID(watch.ID, now.UnixMilli())
				},
				returned:  wakeReturned(watch.ID, marker),
				committed: wakeEnded(watch.ID),
				redeliver: wakeReplayed(watch.ID),
				again:     agentWakeSendIDPrefix(watch.ID),
			}
		},
		"worker prompt": func(t *testing.T, f *claimFixture) claimedDelivery {
			worker := f.plainThread(t)
			recordSpawn(t, f.app, "spawn-"+entityid.New(), f.caller.ID, worker.ID)
			// In the sidebar: the spawner's delete does not take it, and its
			// prompts still go to the spawner.
			if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone); err != nil {
				t.Fatal(err)
			}
			marker := "deploy-" + entityid.New()
			return claimedDelivery{
				marker: marker,
				deliver: func(t *testing.T) string {
					askApproval(t, f.app, worker.ID, "approval-1", marker)
					var sendID string
					waitUntil(t, 10*time.Second, func() bool {
						_ = f.app.drainWorkerPrompts(t.Context(), 10*time.Second)
						ids := queuedSendIDsOf(t, f.app, f.caller.ID, workerPromptSendIDRoot)
						if len(ids) == 1 {
							sendID = ids[0]
						}
						return sendID != ""
					})
					return sendID
				},
				returned: func(t *testing.T, a *App) {
					t.Helper()
					if len(a.triage.PendingInteractiveRequests(worker.ID).Approvals) == 0 {
						// A restart ended the session that asked it, and the
						// boot dropped its wake with it.
						return
					}
					waitUntil(t, 10*time.Second, func() bool {
						_ = a.drainWorkerPrompts(t.Context(), 10*time.Second)
						pending := a.triage.PendingInteractiveRequests(worker.ID).Approvals
						return len(pending) == 1 && pending[0].RoutedTo == nil
					})
				},
				again: workerPromptSendIDRoot,
			}
		},
		"remote completion": func(t *testing.T, f *claimFixture) claimedDelivery {
			w := store.RemoteWatch{ComputerID: entityid.New(), RequestID: entityid.New(), ThreadID: f.caller.ID, Fingerprint: strings.Repeat("a", 64), Label: "build"}
			if _, err := f.app.store.RegisterRemoteWatch(w); err != nil {
				t.Fatal(err)
			}
			if err := f.app.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, store.RemoteJob{ID: w.RequestID, SourceThreadID: f.caller.ID, State: "succeeded"}, "", 0); err != nil {
				t.Fatal(err)
			}
			deliver := func(t *testing.T, a *App) {
				t.Helper()
				current, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
				if err != nil {
					t.Fatal(err)
				}
				if err := a.deliverRemoteCompletion(current, remoteCompletionOutput{Unavailable: true}); err != nil {
					t.Fatalf("deliver the completion: %v", err)
				}
			}
			return claimedDelivery{
				marker: w.RequestID,
				deliver: func(t *testing.T) string {
					deliver(t, f.app)
					return remoteCompletionSendID(w)
				},
				returned: func(t *testing.T, a *App) {
					t.Helper()
					if current, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID); err != nil || current.Notification != "pending" {
						t.Errorf("the watch = %+v, %v; want its completion pending again", current, err)
					}
				},
				redeliver: func(t *testing.T, after *requestFixture) { deliver(t, after.app) },
				again:     remoteCompletionSendID(w),
			}
		},
		"moved loop wake": func(t *testing.T, f *claimFixture) claimedDelivery {
			marker := "moved loop " + entityid.New()
			old, loop := handOffToCaller(t, f, store.AgentWake{Kind: store.AgentWakeLoop, Label: "board", Note: "board"}, marker)
			return claimedDelivery{
				marker:    marker,
				deliver:   func(*testing.T) string { return moveQueue(t, f, old, loop) },
				returned:  func(t *testing.T, a *App) { wakeReturnedLoop(t, a, loop.ID) },
				committed: wakeEnded(loop.ID),
				redeliver: wakeReplayed(loop.ID),
				again:     agentWakeSendIDPrefix(loop.ID),
			}
		},
		"workflow wake": func(t *testing.T, f *claimFixture) claimedDelivery {
			marker := "workflow-" + entityid.New()
			var durable atomic.Int32
			// The run's record of the wake is settled only once the message
			// reached the provider; otherwise its queued claim stays, and the
			// run's next identical wake is delivered again
			// (workflowapp.deliverWorkflowWake).
			unsettled := func(t *testing.T, _ *App) {
				t.Helper()
				if n := durable.Load(); n != 0 {
					t.Errorf("the run's wake record was settled %d times for a message the agent never took", n)
				}
			}
			return claimedDelivery{
				marker: marker,
				deliver: func(t *testing.T) string {
					if err := f.app.queueWorkflowWake(f.caller.ID, "release", wake.Message{Notice: wake.DataNotice, Data: marker},
						func() { durable.Add(1) }); err != nil {
						t.Fatalf("queue the workflow wake: %v", err)
					}
					ids := queuedSendIDsOf(t, f.app, f.caller.ID, workflowWakeSendIDRoot)
					if len(ids) != 1 {
						t.Fatalf("workflow wakes queued = %q, want one", ids)
					}
					return ids[0]
				},
				delivered: func(t *testing.T) {
					t.Helper()
					waitUntil(t, 10*time.Second, func() bool { return durable.Load() == 1 })
				},
				committed: unsettled,
				// The boot puts a queued workflow wake in the composer, with
				// the run's record unsettled.
				returned: func(t *testing.T, a *App) {
					t.Helper()
					unsettled(t, a)
					if draft, _, err := a.store.GetThreadDraft(f.caller.ID); err != nil || !strings.Contains(draft.Content, marker) {
						t.Errorf("the composer after the boot = %q, %v; want the wake", draft.Content, err)
					}
				},
				again: workflowWakeSendIDRoot,
			}
		},
		"moved watch wake": func(t *testing.T, f *claimFixture) claimedDelivery {
			marker := "moved-watch-" + entityid.New()
			old, watch := handOffToCaller(t, f, store.AgentWake{Kind: store.AgentWakeWatch, Label: "inbox", Command: "echo " + marker}, marker)
			return claimedDelivery{
				marker:    marker,
				deliver:   func(*testing.T) string { return moveQueue(t, f, old, watch) },
				returned:  wakeReturned(watch.ID, marker),
				committed: wakeEnded(watch.ID),
				redeliver: wakeReplayed(watch.ID),
				again:     agentWakeSendIDPrefix(watch.ID),
			}
		},
	}
}

// wakeReturnedLoop fails unless a loop's wake is recorded on its row as not
// delivered: a loop's replay renders its note again.
func wakeReturnedLoop(t *testing.T, a *App, id string) {
	t.Helper()
	if row := wakeRow(t, a, id); row.Undelivered == "" {
		t.Errorf("the loop = %+v, want its wake recorded as not delivered", row)
	}
}

// handOffToCaller makes f's thread the successor of a new assistant thread
// whose wake of kind is queued saying marker, and returns that thread and
// the wake.
func handOffToCaller(t *testing.T, f *claimFixture, wake store.AgentWake, marker string) (string, store.AgentWake) {
	t.Helper()
	old := f.plainThread(t)
	testAssistant(t, f.app, old.ID)
	wake.ThreadID, wake.IntervalSeconds, wake.NextAt = old.ID, 3600, time.Now().Add(time.Hour).UnixMilli()
	row := registerTestWake(t, f.app, wake)
	fireAt := time.Now().Add(-time.Minute).UnixMilli()
	if err := f.app.store.QueueAgentWake(store.FlushQueueItem{ID: entityid.New(), ThreadID: old.ID, SendID: agentWakeSendID(row.ID, fireAt),
		Message: testAgentWake(t, row, marker).Text(), EnqueuedAt: 1}, agentWakeSendIDPrefix(row.ID), 0); err != nil {
		t.Fatal(err)
	}
	placeInAssistantHome(t, f.app, f.caller.ID, old.ID)
	if _, err := f.app.store.HandOffAssistant(old.ID, f.caller.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return old.ID, row
}

// moveQueue moves the superseded thread's queue on, as the handoff's
// teardown of it does, and returns the send id the wake is queued under.
func moveQueue(t *testing.T, f *claimFixture, old string, wake store.AgentWake) string {
	t.Helper()
	held, owed, _ := f.app.settleQueueForTeardown(old, false, dropQueue)
	f.app.giveBackToOwners(old, held)
	f.app.giveBackTeardownRequests(old, owed)
	var sendID string
	waitUntil(t, 10*time.Second, func() bool {
		if ids := queuedSendIDsOf(t, f.app, f.caller.ID, agentWakeSendIDPrefix(wake.ID)); len(ids) == 1 {
			sendID = ids[0]
		}
		return sendID != ""
	})
	return sendID
}

// askApproval has workerID's session ask for an approval that describes
// description.
func askApproval(t *testing.T, a *App, workerID, requestID, description string) {
	t.Helper()
	request := provider.ApprovalRequest{RequestID: requestID, ThreadID: workerID, ToolName: "Bash", Description: description, Kind: "command"}
	meta, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: workerID, ItemID: requestID, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}
}

// Every kind of message the app delivers to a thread a delete claimed is in
// the thread's queue at once, and nothing starts. A refused delete's
// release delivers it to the provider once, with no clock moved and no
// sweep running. A committed delete's teardown gives it back to its owner,
// and no start is left waiting.
// A restart with the claim left behind gives it back to its owner at boot,
// which delivers it again once where the owner does.
func TestAMessageForAClaimedThreadIsQueuedAndWaitsForTheDecision(t *testing.T) {
	t.Parallel()
	for kind, build := range claimedDeliveries() {
		for _, outcome := range []string{"refused", "committed", "restart"} {
			t.Run(kind+" "+outcome, func(t *testing.T) {
				t.Parallel()
				f := newClaimFixture(t)
				c := build(t, f)
				var sendID string
				during := func() {
					sendID = c.deliver(t)
					awaitQueuedWake(t, f.app, f.caller.ID, sendID)
					waitAtGates(t, f.app, 1)
					f.startedNothing(t)
					if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 1 {
						t.Errorf("queued on a claimed thread = %+v, want the one message", rows)
					}
					if n := waitingAtGates(f.app); n != 1 {
						t.Errorf("%d starts wait at the delete's gate, want the one the message needs", n)
					}
				}
				switch outcome {
				case "refused":
					refusedDelete(t, f.app, f.caller.ID, during)
					f.deliveredOnce(t, c.marker)
					if c.delivered != nil {
						c.delivered(t)
					}
				case "committed":
					tornDown := false
					afterTeardown(f.app, f.caller.ID, func() {
						tornDown = true
						if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
							t.Errorf("the queue after the teardown = %+v, want it given back", rows)
						}
						if c.committed != nil {
							c.committed(t, f.app)
						} else {
							c.returned(t, f.app)
						}
					})
					committedDelete(t, f.app, f.caller.ID, during)
					f.settle(t)
					if !tornDown {
						t.Fatal("the thread was not torn down")
					}
					if n := f.sent(t, c.marker); n != 0 {
						t.Fatalf("a committed thread's provider was written the message %d times", n)
					}
					nothingWaits(t, f.app)
				case "restart":
					crashedDelete(t, f.app, f.caller.ID, during)
					f.settle(t)
					after := f.restart(t)
					after.holdCallerSession()
					after.app.configureTriageQueueCallbacks()
					after.app.releaseUndecidedDeletes()
					after.app.restoreDurableFlushQueueAtBoot()
					untouched(t, after.app, f.caller.ID)
					if ids := queuedSendIDsOf(t, after.app, f.caller.ID, c.again); len(ids) != 0 {
						t.Fatalf("the queue after the boot = %q, want it given back", ids)
					}
					c.returned(t, after.app)
					if c.redeliver == nil {
						return
					}
					c.redeliver(t, after)
					waitUntil(t, 10*time.Second, func() bool { return len(queuedSendIDsOf(t, after.app, f.caller.ID, c.again)) > 0 })
					after.app.threadRequestsWG.Wait()
					after.app.waitAgentNoticeWork()
					if ids := queuedSendIDsOf(t, after.app, f.caller.ID, c.again); len(ids) != 1 {
						t.Fatalf("redelivered after the boot = %q, want once", ids)
					}
				}
			})
		}
	}
}

// A spawner's answer to a worker's async questions queued while a delete
// holds the worker is in the worker's queue at once and starts nothing; a
// refused delete's release starts the worker and sends it once, a
// committed delete's teardown and a restart's boot give it back to its
// questions.
func TestAWorkerAnswerForAClaimedWorkerIsQueuedAndWaitsForTheDecision(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"refused", "committed", "restart"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newAsyncQuestionFixture(t, "claimed-answer-"+outcome, store.PlacementStandalone)
			var starts sync.Mutex
			stdinLog := ""
			// The worker's start is a real one's: refused while the worker is
			// claimed, then a live session the queue flushes to. The
			// spawner's provider is missing, so its wakes stay queued.
			f.app.startSessionFn = func(threadID string) error {
				if threadID != f.worker.ID {
					return errors.New("the provider binary is missing")
				}
				current, err := f.app.store.GetThread(threadID)
				if err != nil {
					return err
				}
				if err := f.app.store.CheckThreadExecutionAccess(current); err != nil {
					return err
				}
				log := f.workerRecordingSession(t)
				starts.Lock()
				stdinLog = log
				starts.Unlock()
				f.app.triage.FlushQueuedItems(threadID)
				return nil
			}
			startedLog := func() string {
				starts.Lock()
				defer starts.Unlock()
				return stdinLog
			}
			var sendID string
			during := func() {
				sendID = f.answered(t)
				quietAtGates(t, f.app)
				if startedLog() != "" {
					t.Error("a claimed worker was started")
				}
			}
			switch outcome {
			case "refused":
				refusedDelete(t, f.app, f.worker.ID, during)
				f.drain(t)
				waitUntil(t, 10*time.Second, func() bool { return startedLog() != "" })
				if envelopes := waitForRecordedUserEnvelopes(t, startedLog(), 1); len(envelopes) != 1 {
					t.Fatalf("the worker was written %d messages, want the answer once", len(envelopes))
				}
			case "committed":
				afterTeardown(f.app, f.worker.ID, func() { f.requireReturned(t, sendID) })
				committedDelete(t, f.app, f.worker.ID, during)
				f.drain(t)
				if startedLog() != "" {
					t.Fatal("a committed worker was started")
				}
			case "restart":
				crashedDelete(t, f.app, f.worker.ID, during)
				f.drain(t)
				// The registry and the in-memory queue are gone with the
				// restart, the durable rows stay.
				after := f.restart(t)
				after.app.releaseUndecidedDeletes()
				untouched(t, after.app, f.worker.ID)
				after.app.restoreDurableFlushQueueAtBoot()
				after.drain(t)
				after.requireReturned(t, sendID)
				after.requireThePersons(t)
			}
		})
	}
}

// r4 finding 1: a fire a claim turned away was put back armed before its
// run unregistered. A fire now queues its wake on a claimed thread and
// finishes as any fire does, through the one retirement step: the row is
// armed at its next interval with no run left registered, nothing is put
// back, and the release delivers the wake once.
func TestAFireOnAClaimedThreadFinishesAsAnyFire(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "check the board",
		IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	refusedDelete(t, f.app, f.caller.ID, func() {
		now := time.Now()
		if err := f.app.fireAgentWake(loop, now, agentWakeFire{at: now.UnixMilli()}); err != nil {
			t.Fatal("the fire did not start")
		}
		awaitQueuedWake(t, f.app, f.caller.ID, agentWakeSendID(loop.ID, now.UnixMilli()))
		waitUntil(t, 10*time.Second, func() bool { return wakeRow(t, f.app, loop.ID).State == store.AgentWakeArmed })
		row := wakeRow(t, f.app, loop.ID)
		if row.LastOutcome != agentWakeOutcomeDelivered || row.NextAt <= now.UnixMilli() || row.Undelivered != "" {
			t.Errorf("the loop = %+v, want its fire delivered and its next one an interval out", row)
		}
		waitUntil(t, 10*time.Second, func() bool {
			f.app.agentWakes.mu.Lock()
			defer f.app.agentWakes.mu.Unlock()
			_, running := f.app.agentWakes.runs[loop.ID]
			return !running
		})
		f.startedNothing(t)
	})
	f.deliveredOnce(t, "check the board")
}

// r4 finding 2 and r5 finding 2: a stopped thread's outside message is
// queued before a delete claims it and its start meets the claim; a
// worker's approval wake reaches it during the claim, and the person
// answers the approval in the worker, which is in the sidebar, while the
// delete decides. The answer withdraws the approval's wake from the claimed
// thread's queue at once, and the refused delete's release starts a real
// session that is sent the outside message alone.
func TestACanceledNoticeIsGoneBeforeTheReleaseStartsTheRest(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	worker := f.plainThread(t)
	recordSpawn(t, f.app, "spawn-canceled", f.caller.ID, worker.ID)
	if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	// The outside message's start waits until the claim is written, as one
	// racing the claim does.
	gate := make(chan struct{})
	f.app.startSessionFn = func(threadID string) error {
		<-gate
		return f.app.startSessionNow(threadID)
	}
	outside, err := f.app.SendExternalThreadMessage(t.Context(), f.caller.ID, "the deploy finished")
	if err != nil {
		t.Fatal(err)
	}
	refusedDelete(t, f.app, f.caller.ID, func() {
		close(gate)
		quietAtGates(t, f.app)
		askApproval(t, f.app, worker.ID, "approval-canceled", "make release")
		waitUntil(t, 10*time.Second, func() bool {
			_ = f.app.drainWorkerPrompts(t.Context(), 10*time.Second)
			return len(queuedSendIDsOf(t, f.app, f.caller.ID, workerPromptSendIDRoot)) == 1
		})
		f.startedNothing(t)
		resolveApprovalOn(t, f.app, worker.ID, "approval-canceled")
		quietAtGates(t, f.app)
		rows := durableQueueRows(t, f.app, f.caller.ID)
		if len(rows) != 1 || rows[0].SendID != outside {
			t.Errorf("the queue before the release = %+v, want the outside message alone", rows)
		}
	})
	f.deliveredOnce(t, "the deploy finished")
	if n := f.sent(t, "make release"); n != 0 {
		t.Fatalf("the answered approval reached the provider %d times", n)
	}
}

// r4 finding 3 and r5 finding 3: a reply wake on its last retry attempt,
// booked and waiting for a delivery slot when a delete claims its caller,
// is delivered to the provider once, whether its delivery lands during the
// claim or after a release whose sweep pass read the booked attempt and
// went to sleep: the claim turns no delivery away, so there is no attempt
// to give back and nothing to wake the sweep for.
func TestAReplyWakeOnItsLastAttemptAcrossAClaimIsDeliveredOnce(t *testing.T) {
	t.Parallel()
	for _, crossing := range []string{"delivered during the claim", "delivered after the release"} {
		t.Run(crossing, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			token := "last-" + entityid.New()
			owedReplyWake(t, f.app, f.caller.ID, token, "reply "+token, threadWakeRetryAttempts-1)
			slots := f.app.threadWakeDeliverySlots()
			for range cap(slots) {
				slots <- struct{}{}
			}
			runSweeps(t, f.app)
			waitUntil(t, 10*time.Second, func() bool { return requestRow(t, f.app, token).WakeAttempts == threadWakeRetryAttempts })
			free := func() {
				for range cap(slots) {
					<-slots
				}
			}
			refusedDelete(t, f.app, f.caller.ID, func() {
				if crossing == "delivered during the claim" {
					free()
					awaitQueuedWake(t, f.app, f.caller.ID, threadWakeSendID(token))
					f.startedNothing(t)
				}
			})
			if crossing == "delivered after the release" {
				free()
			}
			f.deliveredOnce(t, "reply "+token)
			if row := requestRow(t, f.app, token); row.WakeIssue != "" || row.DeliveredAt == 0 {
				t.Fatalf("the request = %+v, want it delivered with nothing noted", row)
			}
		})
	}
}

// A claim held across many sweep passes writes nothing to the claimed
// thread's request, wake or watch rows beyond the deliveries themselves:
// once its owed reply, its reminder, its loop's fire and its remote
// completion are queued, later passes find nothing due.
func TestAClaimHeldAcrossManySweepsWritesNothing(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	token := "owed-" + entityid.New()
	owedReplyWake(t, f.app, f.caller.ID, token, "owed reply", 0)
	insertTestReminder(t, f.app, "remind-"+entityid.New(), f.caller.ID, "reminded", time.Now().Add(-time.Second).UnixMilli())
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "board",
		IntervalSeconds: 3600, NextAt: time.Now().Add(-time.Second).UnixMilli()})
	w := store.RemoteWatch{ComputerID: entityid.New(), RequestID: entityid.New(), ThreadID: f.caller.ID, Fingerprint: strings.Repeat("a", 64), Label: "build"}
	if _, err := f.app.store.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, store.RemoteJob{ID: w.RequestID, SourceThreadID: f.caller.ID, State: "succeeded"}, "", 0); err != nil {
		t.Fatal(err)
	}
	pass := func(now time.Time) {
		f.app.fireDueThreadReminders(now)
		f.app.fireDueAgentWakes(now)
		f.app.retryUndeliveredThreadWakes(now)
		rows, err := f.app.store.ListRemoteWatches("", now.UnixMilli(), 32)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			f.app.checkRemoteWatch(row)
		}
		f.app.threadRequestSettling.Wait()
		quietAtGates(t, f.app)
	}
	refusedDelete(t, f.app, f.caller.ID, func() {
		now := time.Now()
		pass(now)
		current, err := f.app.store.GetRemoteWatch(w.ComputerID, w.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.app.deliverRemoteCompletion(current, remoteCompletionOutput{Unavailable: true}); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, 10*time.Second, func() bool { return wakeRow(t, f.app, loop.ID).State == store.AgentWakeArmed })
		quietAtGates(t, f.app)
		if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 4 {
			t.Errorf("queued while claimed = %+v, want the four deliveries", rows)
		}
		before := f.app.store.WriteCount()
		for n := range 3 * threadWakeRetryAttempts {
			// Short of the loop's next interval, so nothing comes due anew.
			pass(now.Add(time.Duration(n) * time.Minute))
		}
		if writes := f.app.store.WriteCount() - before; writes != 0 {
			t.Errorf("%d writes across the passes, want none", writes)
		}
		f.startedNothing(t)
	})
	f.deliveredOnce(t, "owed reply")
}

// A live session's queue is not dispatched while a delete claims its
// thread: its one dispatch waits for the decision with the session kept,
// what the app queues meanwhile (reminders, a workflow wake and a loop's
// wake, recorded as delivered) is handed to that dispatch and stays in the
// durable queue, a message withdrawn meanwhile leaves it,
// a refused delete's release sends the rest once in the order they were
// queued, and a committed delete's teardown gives them back to their owners.
func TestALiveSessionsQueueWaitsForTheDecision(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"refused", "committed"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newClaimFixture(t)
			if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
				t.Fatal(err)
			}
			tokens := []string{"first-" + entityid.New(), "withdrawn-" + entityid.New(), "last-" + entityid.New()}
			workflowMarker, loopMarker := "workflow-"+entityid.New(), "loop "+entityid.New()
			loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: loopMarker,
				IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
			// handedToDispatch fails unless the message queued under sendID was
			// handed to the session's dispatch, whose one worker waits at the
			// delete's gate: a message held back for a later turn boundary
			// would be sent without waiting for the decision's release.
			handedToDispatch := func(what, sendID string) {
				t.Helper()
				row := awaitQueuedWake(t, f.app, f.caller.ID, sendID)
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
					f.app.flushDispatch.mu.Lock()
					held := slices.Clone(f.app.flushDispatch.dispatching[f.caller.ID])
					for _, batch := range f.app.flushDispatch.queues[f.caller.ID] {
						held = append(held, batch.items...)
					}
					f.app.flushDispatch.mu.Unlock()
					if slices.ContainsFunc(held, func(item triage.QueuedFlushItem) bool { return item.ID == row.ID }) {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("the %s was not handed to the live session's dispatch: %+v", what, held)
					}
				}
			}
			during := func() {
				for _, token := range tokens {
					insertTestReminder(t, f.app, token, f.caller.ID, "remind "+token, time.Now().Add(-time.Second).UnixMilli())
					f.app.fireDueThreadReminders(time.Now())
					awaitQueuedWake(t, f.app, f.caller.ID, threadWakeSendID(token))
				}
				if _, err := f.app.removeQueuedItem(t.Context(), f.caller.ID, threadWakeSendID(tokens[1])); err != nil {
					t.Fatalf("withdraw: %v", err)
				}
				if err := f.app.queueWorkflowWake(f.caller.ID, "release", wake.Message{Notice: wake.DataNotice, Data: workflowMarker}, nil); err != nil {
					t.Fatalf("queue the workflow wake: %v", err)
				}
				if ids := queuedSendIDsOf(t, f.app, f.caller.ID, workflowWakeSendIDRoot); len(ids) == 1 {
					handedToDispatch("workflow wake", ids[0])
				}
				now := time.Now()
				if err := f.app.fireAgentWake(loop, now, agentWakeFire{at: now.UnixMilli()}); err != nil {
					t.Fatal("the fire did not start")
				}
				handedToDispatch("loop's wake", agentWakeSendID(loop.ID, now.UnixMilli()))
				// The fire records the wake it queued as delivered.
				waitUntil(t, 10*time.Second, func() bool { return wakeRow(t, f.app, loop.ID).State == store.AgentWakeArmed })
				if row := wakeRow(t, f.app, loop.ID); row.LastOutcome != agentWakeOutcomeDelivered || row.Undelivered != "" {
					t.Errorf("the loop = %+v, want its fire recorded as delivered", row)
				}
				quietAtGates(t, f.app)
				if ids := queuedSendIDsOf(t, f.app, f.caller.ID, threadWakeSendID("")); len(ids) != 2 {
					t.Errorf("queued while claimed = %q, want the two not withdrawn", ids)
				}
				if ids := queuedSendIDsOf(t, f.app, f.caller.ID, workflowWakeSendIDRoot); len(ids) != 1 {
					t.Errorf("workflow wakes queued while claimed = %q, want one", ids)
				}
				if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
					t.Errorf("a claimed thread's live session was written %d messages", n)
				}
				if _, live := f.app.sessionManager().get(f.caller.ID); !live {
					t.Error("the claim ended the thread's live session")
				}
				if n := waitingAtGates(f.app); n != 1 {
					t.Errorf("%d dispatches wait at the delete's gate, want the session's one", n)
				}
			}
			if outcome == "committed" {
				afterTeardown(f.app, f.caller.ID, func() {
					for _, token := range []string{tokens[0], tokens[2]} {
						if row := requestRow(t, f.app, token); row.DeliveredAt != 0 {
							t.Errorf("the reminder at the teardown = %+v, want it given back", row)
						}
					}
				})
				committedDelete(t, f.app, f.caller.ID, during)
				f.settle(t)
				if n := len(settledUserEnvelopes(t, f.stdin)); n != 0 {
					t.Fatalf("a committed thread's session was written %d messages", n)
				}
				return
			}
			refusedDelete(t, f.app, f.caller.ID, during)
			f.deliveredOnce(t, "remind "+tokens[0])
			f.deliveredOnce(t, "remind "+tokens[2])
			f.deliveredOnce(t, workflowMarker)
			f.deliveredOnce(t, loopMarker)
			if n := f.sent(t, "remind "+tokens[1]); n != 0 {
				t.Fatalf("the withdrawn reminder reached the provider %d times", n)
			}
			var order []string
			for _, envelope := range recordedUserEnvelopes(t, f.stdin) {
				text, _ := envelopeText(t, envelope)
				order = append(order, text)
			}
			first := slices.IndexFunc(order, func(s string) bool { return strings.Contains(s, tokens[0]) })
			last := slices.IndexFunc(order, func(s string) bool { return strings.Contains(s, tokens[2]) })
			if first < 0 || last < 0 || first > last {
				t.Fatalf("sent in the order %q, want the first reminder before the last", order)
			}
		})
	}
}

// A person's Stop of a worker whose spawner a delete claimed tells the
// spawner in its queue at once, and a usage-limit continue is queued the
// same way: both are app messages, and the release starts the thread for
// them.
func TestAStoppedWorkerNoticeAndAContinueToAClaimedThreadAreQueued(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	worker := f.plainThread(t)
	spawn := store.ThreadRequest{Token: "tok-stopped", CallerThreadID: f.caller.ID, TargetThreadID: worker.ID, Kind: store.ThreadRequestSpawn}
	const stopped = "worker-stopped:tok-stopped"
	refusedDelete(t, f.app, f.caller.ID, func() {
		var result ThreadAgentStop
		f.app.tellSpawnerOfUserStop(spawn, stopped, &result)
		if len(result.Failures) != 0 {
			t.Errorf("the Stop's failures = %q, want none", result.Failures)
		}
		awaitQueuedWake(t, f.app, f.caller.ID, stopped)
		waitAtGates(t, f.app, 1)
		if err := f.app.continueAfterUsageLimit(t.Context(), f.caller.ID, func() bool { return true }); err != nil {
			t.Errorf("the continue = %v, want it queued", err)
		}
		if ids := queuedSendIDsOf(t, f.app, f.caller.ID, "usage-continue:"); len(ids) != 1 {
			t.Errorf("continues queued = %q, want one", ids)
		}
		// Each start waits at the delete's gate.
		waitAtGates(t, f.app, 2)
		f.startedNothing(t)
	})
	f.settle(t)
	waitUntil(t, 20*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 0 })
	// The provider records what it reads after the write that retired the rows.
	waitUntil(t, 20*time.Second, func() bool { return len(recordedUserEnvelopes(t, f.stdin)) > 0 })
}
