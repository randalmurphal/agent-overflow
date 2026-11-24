package app

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

// An agent's thread_cancel by thread id of a thread it spawned brings that
// thread to rest, attributed to the calling thread, with the spawn's wake
// disarmed since the agent reads the report. A thread it only sent to keeps
// its session and has its turn interrupted.
func TestThreadCancelOfASpawnedThreadBringsItToRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	loop := registerTestWake(t, f.app, store.AgentWake{ThreadID: child.ThreadID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	other := f.plainThread(t)
	sent, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: other.ID, Message: "look at this", Notify: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.awaitReceiptRunning(t, sent.Token)

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{ThreadID: child.ThreadID})
	if err != nil || report.Effect != threadtools.EffectThreadStopped {
		t.Fatalf("Cancel(spawned) = %+v, %v; want the thread stopped", report, err)
	}
	if _, live := f.app.sessionManager().get(child.ThreadID); live {
		t.Fatal("the spawned thread still has a session")
	}
	if spawn := f.request(t, child.Token); spawn.State != store.ThreadRequestCancelled || spawn.Notify {
		t.Fatalf("the spawn = %s notify %v; want it cancelled with its wake disarmed", spawn.State, spawn.Notify)
	}
	// The rest cuts the turn twice, once for the spawn's request and once as
	// the session ends, and the provider has acknowledged both before the
	// turn reports its end: the turn still carries one stop row.
	want := stopByThread(f.caller.Title, f.caller.ID, "")
	if labels := stopRowLabels(t, f.app, child.ThreadID); !slices.Equal(labels, []string{want.label}) {
		t.Fatalf("the cut turn's rows = %q, want [%q]", labels, want.label)
	}
	if row, _, err := f.app.store.GetAgentWake(loop.ID); err != nil || row.State != store.AgentWakeCancelled || !strings.Contains(row.LastOutcome, f.caller.Title) {
		t.Fatalf("the loop = %+v, %v; want it cancelled naming the calling thread", row, err)
	}

	report, err = f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{ThreadID: other.ID})
	if err != nil || report.Effect != threadtools.EffectInterrupted {
		t.Fatalf("Cancel(sent to) = %+v, %v; want its turn interrupted", report, err)
	}
	if _, live := f.app.sessionManager().get(other.ID); !live {
		t.Fatal("a thread the caller only sent to lost its session")
	}
	stopSessionChecked(t, f.app, other.ID)
}

// Stop All applies the row rule to every row: each spawned thread is
// brought to rest.
func TestStopAllBringsEverySpawnedThreadToRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	first := f.runningRequest(t, "one")
	second := f.runningRequest(t, "two")

	stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("StopThreadAgentRows: %v", err)
	}
	if len(stops) != 2 {
		t.Fatalf("stops = %+v, want one per row", stops)
	}
	for _, ack := range []threadtools.RequestAck{first, second} {
		if _, live := f.app.sessionManager().get(ack.ThreadID); live {
			t.Fatalf("%s still has a session after Stop All", ack.ThreadID)
		}
	}
}

// Work the stopped thread's turn starts after the Stop cancelled its loops,
// before the turn is cut, is refused: a loop would start the thread again
// and a spawn would outlive the Stop. Once the Stop is over the thread may
// start work again.
func TestAStopRefusesWorkItsTurnStartsDuringIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "work on it", Notify: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitReceiptRunning(t, ack.Token)
	// Another thread's send the rest settles once it has cancelled the
	// loops, which is when the hook registers one.
	other := f.targetIdentity(t, f.plainThread(t).ID)
	if _, err := f.adapter().Send(t.Context(), other, threadtools.SendCall{ThreadID: ack.ThreadID, Message: "and this"}); err != nil {
		t.Fatalf("the other thread's send: %v", err)
	}
	// The hook runs on the Stop's goroutine, which the test waits on.
	worker := f.targetIdentity(t, ack.ThreadID)
	var loopErr, spawnErr error
	tried := false
	f.app.cancelReadForTest = func(string) {
		if !f.app.resting.holds(ack.ThreadID) || tried {
			return
		}
		tried = true
		_, loopErr = f.app.registerAgentWake(store.AgentWake{
			ThreadID: ack.ThreadID, Kind: store.AgentWakeLoop, Label: "late", Note: "n",
			IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli(),
		}, "")
		_, spawnErr = f.adapter().Spawn(t.Context(), worker, threadtools.SpawnCall{Prompt: "more"})
	}

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	if err != nil || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	if !tried || !errors.Is(loopErr, errThreadResting) || !errors.Is(spawnErr, errThreadResting) {
		t.Fatalf("during the Stop: tried %v, loop %v, spawn %v; want both refused", tried, loopErr, spawnErr)
	}
	if wakes, err := f.app.store.ListAgentWakes(ack.ThreadID); err != nil || len(wakes) != 0 {
		t.Fatalf("loops after the Stop = %+v, %v; want none", wakes, err)
	}
	if rows, err := f.app.store.ListThreadRequestsByCaller(ack.ThreadID, 10, 0); err != nil || len(rows) != 0 {
		t.Fatalf("requests the worker made = %+v, %v; want none", rows, err)
	}
	if _, err := f.adapter().Remind(t.Context(), worker, threadtools.RemindCall{DueAtUnixMs: time.Now().Add(time.Hour).UnixMilli(), Note: "n"}); err != nil {
		t.Fatalf("a reminder after the Stop: %v", err)
	}
}

// A worker's open prompt goes with the Stop, in the worker and in the copy
// routed to its spawner.
func TestAStopDismissesTheWorkersPromptWhereverItShows(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "rest", false)
	f.drain(t)
	f.queuedPrompt(t, f.assistant.ID)

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.assistant.ID, "rest-spawn")
	if err != nil || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	f.drain(t)
	if pending := f.app.triage.PendingInteractiveRequests(f.worker.ID); len(pending.Approvals)+len(pending.UserInputs) != 0 {
		t.Fatalf("the stopped worker still asks %+v", pending)
	}
	if tokens := workerPromptTokens(t, f.app, f.assistant.ID); len(tokens) != 0 {
		t.Fatalf("the spawner still holds the worker's prompt %q", tokens)
	}
}

// A message the person queued in the stopped thread's composer gets what End
// Session gives it: the Stop and StopSession leave the same queue and draft.
func TestAStopTreatsThePersonsQueueAsEndSessionDoes(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	stopped := f.runningRequest(t, "one")
	ended := f.runningRequest(t, "two")
	for _, ack := range []threadtools.RequestAck{stopped, ended} {
		if _, err := f.app.RegisterQueueItem(t.Context(), ack.ThreadID, "my next step", SendMessageOptions{SendID: "person-" + ack.ThreadID}); err != nil {
			t.Fatalf("RegisterQueueItem: %v", err)
		}
	}

	if _, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, stopped.Token); err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	if err := f.app.StopSession(ended.ThreadID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	type left struct {
		durable, memory []string
		draft           string
	}
	leftIn := func(threadID string) left {
		durable, memory := queuedSendIDs(t, f.app, threadID)
		draft, err := f.app.GetDraft(threadID)
		if err != nil {
			t.Fatalf("GetDraft: %v", err)
		}
		return left{durable: durable, memory: memory, draft: draft.Content}
	}
	byStop, byEnd := leftIn(stopped.ThreadID), leftIn(ended.ThreadID)
	if len(byStop.durable) != len(byEnd.durable) || len(byStop.memory) != len(byEnd.memory) || byStop.draft != byEnd.draft {
		t.Fatalf("after the Stop %+v, after End Session %+v; want the same", byStop, byEnd)
	}
}

// A step of the rest that fails is named in the Stop's result, and the rest
// of it still runs: the session ends though the loops could not be cancelled.
func TestAStopReportsAFailedStepAndRunsTheRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	registerTestWake(t, f.app, store.AgentWake{ThreadID: child.ThreadID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_wake_cancel BEFORE UPDATE OF state ON agent_wakes
		WHEN NEW.state = 'cancelled' BEGIN SELECT RAISE(ABORT, 'cancel refused'); END`)

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, child.Token)
	if err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	if !slices.ContainsFunc(stop.Failures, func(failure string) bool {
		return strings.Contains(failure, "cancel the loops and watches of "+child.ThreadID)
	}) {
		t.Fatalf("failures = %q, want the refused cancel named", stop.Failures)
	}
	if _, live := f.app.sessionManager().get(child.ThreadID); live {
		t.Fatal("the session survived a failed step of the Stop")
	}
}

// A Stop that lands while the thread's session is starting waits for the
// start and ends the session it made.
func TestAStopWaitsOutASessionStart(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target := f.plainThread(t)
	f.seedAgentRow(t, "spawn-starting", f.caller.ID, store.ThreadRequestSpawn, target.ID, true)
	unlock := f.app.threadLocks().Lock(target.ID)
	done := make(chan ThreadAgentStop, 1)
	go func() {
		stop, _ := f.app.StopThreadAgentRow(context.Background(), f.caller.ID, "spawn-starting")
		done <- stop
	}()
	select {
	case stop := <-done:
		unlock()
		t.Fatalf("the Stop did not wait for the start: %+v", stop)
	case <-time.After(200 * time.Millisecond):
	}
	// The start registers its session before it lets go of the lock.
	f.app.sessionManager().put(target.ID, session{Provider: "claude", Token: "starting", Liveness: newSessionLiveness(time.Now())})
	unlock()
	select {
	case stop := <-done:
		if len(stop.Failures) != 0 {
			t.Fatalf("stop = %+v", stop)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Stop never returned")
	}
	if _, live := f.app.sessionManager().get(target.ID); live {
		t.Fatal("the session the start made survived the Stop")
	}
}

// While a Stop brings a thread to rest, an app-written message for it is
// refused and starts nothing; a person's is taken, and once the rest is
// over the app's are taken again.
func TestAThreadBeingStoppedTakesNoAppMessage(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	var starts atomic.Int32
	a.startSessionFn = func(string) error {
		starts.Add(1)
		return errors.New("no provider in this test")
	}
	thread := noticeTestThread(t, a, "resting")
	release := a.resting.hold(thread.ID)
	if _, err := a.SendExternalThreadMessage(context.Background(), thread.ID, "hello"); !errors.Is(err, errThreadResting) {
		release()
		t.Fatalf("an outside send during the rest = %v, want it refused", err)
	}
	a.waitAgentNoticeWork()
	if n := starts.Load(); n != 0 {
		release()
		t.Fatalf("the refused message started the thread %d times", n)
	}
	// A notice queued before the rest began, whose start comes once it has.
	a.startAgentNoticeSession(t.Context(), agentNotice{
		threadID: thread.ID, sendID: "wake:queued-before",
		startFailed: func(err error) string { return err.Error() },
	}, false)
	if n := starts.Load(); n != 0 {
		release()
		t.Fatalf("a notice's start during the rest started the thread %d times", n)
	}
	release()
	if _, err := a.SendExternalThreadMessage(context.Background(), thread.ID, "hello again"); err != nil {
		t.Fatalf("an outside send after the rest: %v", err)
	}
}

// A Stop cancels the commands the stopped thread started on a paired
// computer, as an archive does, and drops their notifications: a completion
// kept for it would start it again.
func TestAStopCancelsTheThreadsRemoteCommands(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	receiver.remoteJobs.Close()
	manager, err := remotejobs.New(ctx, receiver.store, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
		<-ctx.Done()
		return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	receiver.remoteJobs = manager
	t.Cleanup(manager.Close)
	started, err := source.AgentRemoteStart(ctx, input)
	if err != nil || started.State != "running" {
		t.Fatalf("start: %+v %v", started, err)
	}
	caller := testThread("remote-command-caller")
	if err := source.store.CreateThread(caller); err != nil {
		t.Fatal(err)
	}
	if err := source.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn-remote-runner", CallerThreadID: caller.ID, Kind: store.ThreadRequestSpawn,
		TargetThreadID: started.SourceThreadID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatal(err)
	}

	stop, err := source.StopThreadAgentRow(ctx, caller.ID, "spawn-remote-runner")
	if err != nil || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	waitRemoteJobState(t, receiver, input.Request.ID, func(state string) bool { return state != "running" })
	watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil || watch.Notification != "dismissed" {
		t.Fatalf("the watch = %+v, %v; want its notification dropped", watch, err)
	}
}

// A remote command's completion queued for the stopped thread and not yet
// taken in is dropped, not given back to its watch by the session end,
// which would deliver it again and start the thread.
func TestAStopDropsARemoteCompletionItsAgentHadNotTakenIn(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	f := newRequestFixture(t)
	target := f.plainThread(t)
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn-finished-runner", CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSpawn,
		TargetThreadID: target.ID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	w := store.RemoteWatch{
		ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: target.ID,
		Fingerprint: strings.Repeat("a", 64), Label: "job",
	}
	if _, err := f.app.store.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.QueueRemoteCompletion(w.ComputerID, w.RequestID, store.FlushQueueItem{
		ID: "queue:" + w.RequestID, ThreadID: target.ID, SendID: remoteCompletionSendID(w), Message: "done",
	}); err != nil {
		t.Fatal(err)
	}

	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, "spawn-finished-runner")
	if err != nil || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	watch, err := f.app.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil || watch.Notification != "dismissed" {
		t.Fatalf("the watch = %q, %v; want its completion dropped", watch.Notification, err)
	}
}

// A Stop of a spawn on a paired computer brings that thread to rest there,
// and the thread it spawned there too.
func TestAStopOfAPairedSpawnBringsItToRestThere(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	grand, found, err := pair.dest.store.GetThreadRequest(pair.grand)
	if err != nil || !found {
		t.Fatalf("the grandchild's spawn: found %v, %v", found, err)
	}

	stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
	if err != nil || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	for _, id := range []string{pair.child, grand.TargetThreadID} {
		if _, live := pair.dest.sessionManager().get(id); live {
			t.Fatalf("%s still has a session on the paired computer", id)
		}
	}
}

// interruptOnlyPeer is a destination that predates the rest: its thread_cancel
// by thread id interrupts and says so.
type interruptOnlyPeer struct{ *App }

func (p *interruptOnlyPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	return p.App.ThreadToolQuery(ctx, call)
}

func (p *interruptOnlyPeer) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool == "thread_cancel" {
		return ThreadPeerReply{Effect: threadtools.EffectInterrupted}, nil
	}
	return p.App.ThreadToolCall(ctx, call)
}

// An agent's thread_cancel of a thread it spawned on a paired computer
// disarms its spawn's wake when that computer brought the thread to rest,
// and leaves it armed when that computer only interrupted the turn, since
// the thread is still working and still owes an answer.
func TestAnAgentsCancelOfARemoteSpawnDisarmsItsWakeOnlyForARest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		serve  func(*testing.T, *App) *pairedBackend
		effect string
		notify bool
	}{
		{name: "a computer that rests it", serve: func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) },
			effect: threadtools.EffectThreadStopped, notify: false},
		{name: "a computer that predates the rest", serve: func(t *testing.T, app *App) *pairedBackend {
			return servePairedApp(t, app, func(cfg *transport.Config) {
				dispatcher := transport.NewDispatcher()
				if _, err := dispatcher.Register(&interruptOnlyPeer{App: app}, transport.RegisterOptions{
					Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
				}); err != nil {
					t.Fatalf("register the older destination: %v", err)
				}
				cfg.Dispatcher = dispatcher
			})
		}, effect: threadtools.EffectInterrupted, notify: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pair := newAgentPeerPair(t, tc.serve)
			report, err := pair.source.threadTools().cancelRemoteSpawnedThread(t.Context(),
				pair.source.threadAgentCaller(pair.caller.ID),
				threadtools.CancelCall{ThreadID: pair.child, ComputerID: pair.spawn.TargetComputerID})
			if err != nil || report.Effect != tc.effect {
				t.Fatalf("cancel = %+v, %v; want %s", report, err, tc.effect)
			}
			if row := pair.request(t, pair.spawn.Token); row.Notify != tc.notify {
				t.Fatalf("the spawn's wake armed = %v, want %v", row.Notify, tc.notify)
			}
		})
	}
}

// A request open on a thread when it is archived is settled by the session's
// end, interrupted with what the turn wrote, where a Stop settles it
// cancelled by the user (docs/specs/standing-agents.md, "At rest").
func TestAnArchiveSettlesARequestOnTheThreadAsItsSessionEnds(t *testing.T) {
	t.Parallel()
	for _, door := range []string{"archive", "stop"} {
		t.Run(door, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaudeHoldingTheTurn(t, "halfway")
			ack := f.runningRequest(t, "work on it")
			var row store.ThreadRequest
			if door == "archive" {
				if err := archiveThread(f.app, ack.ThreadID); err != nil {
					t.Fatal(err)
				}
				row = f.awaitRequestState(t, ack.Token, store.ThreadRequestInterrupted)
				if row.Cause != threadtools.CauseSessionEndedMidTurn {
					t.Fatalf("cause = %q, want the session's end mid-turn", row.Cause)
				}
				return
			}
			if _, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token); err != nil {
				t.Fatal(err)
			}
			row = f.awaitRequestState(t, ack.Token, store.ThreadRequestCancelled)
			if string(row.Answer) != threadRequestUserStop {
				t.Fatalf("answer = %q, want the user's stop", row.Answer)
			}
		})
	}
}
