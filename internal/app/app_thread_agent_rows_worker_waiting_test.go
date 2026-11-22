package app

// A worker that waits on something it started is not at rest
// (docs/specs/standing-agents.md, "Owned workers"): running background work,
// an armed loop or watch, a reminder that has not fired, or a provider
// timer. Its spawn's row is open and reads `waiting`, below blocked,
// running and queued, with what it waits on as its activity line; it is
// counted, never needs the user, and stays out of Past.

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadtools"
)

// workerEvent hands threadID's provider event to triage, as its session's
// read loop does.
func workerEvent(t *testing.T, a *App, threadID string, evt provider.ProviderEvent, meta map[string]any) {
	t.Helper()
	if meta != nil {
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		evt.Meta = raw
	}
	evt.ThreadID, evt.Timestamp = threadID, time.Now()
	if err := a.triage.Handle(evt); err != nil {
		t.Fatalf("handle %s %s: %v", evt.Kind, evt.ItemID, err)
	}
}

// launchWorkerShell is a turn of threadID that starts a background Bash and
// ends while it runs.
func launchWorkerShell(t *testing.T, a *App, threadID, id string) {
	t.Helper()
	workerEvent(t, a, threadID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: threadID + ":" + id}, nil)
	workerEvent(t, a, threadID, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: id},
		map[string]any{"task_id": "task-" + id, "task_type": "local_bash"})
	workerEvent(t, a, threadID, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: id, ItemType: "Bash"},
		map[string]any{"toolName": "Bash", "is_background": true, "input": map[string]any{"command": "tail -f deploy.log", "run_in_background": true}})
	workerEvent(t, a, threadID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
}

// installReplayingClaude gives the app a mock Claude that echoes each user
// message as --replay-user-messages does, so a delivered message leaves the
// pending sends once its turn takes it, and answers every turn with reply.
func installReplayingClaude(t *testing.T, app *App, reply string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/bash
replay() { printf '{"isReplay":true,%s\n' "${1#\{}"; }
n=0
while IFS= read -r line; do
  case "$line" in
    *'"type":"control_request"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      ;;
    *'"type":"user"'*)
      n=$((n + 1))
      printf '%s\n' '` + mockClaudeInitLine + `'
      replay "$line"
      printf '{"type":"assistant","message":{"id":"msg-%s","role":"assistant","content":[{"type":"text","text":%s}]}}\n' "$n" ` + shellQuoteForTest(quoteJSON(reply)) + `
      printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
      ;;
  esac
done
`
	binary := filepath.Join(dir, "mock-claude-replaying.sh")
	mockexec.Write(t, binary, script)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
}

// awaitTurnSince waits until threadID ran a turn that started at or after
// since (unix ms) and ended, with nothing left live.
func awaitTurnSince(t *testing.T, a *App, threadID string, since int64) {
	t.Helper()
	waitUntil(t, 20*time.Second, func() bool {
		index, err := a.store.LastTurnIndex(threadID)
		if err != nil {
			return false
		}
		turn, ok, err := a.store.GetTurnByThreadIndex(threadID, index)
		return err == nil && ok && turn.StartedAt >= since && turn.CompletedAt != nil &&
			threadToolsLiveState(a.triage.LiveStateSnapshotForThread(threadID)) == threadtools.LiveState{}
	})
}

// expectWaiting asserts a waiting worker's row as every reader sees it:
// open with its line, counted, not the user's, kept past the settled window
// and out of Past.
func (f *requestFixture) expectWaiting(t *testing.T, token, line string) ThreadAgentRow {
	t.Helper()
	row := f.awaitWorker(t, f.caller.ID, token, workerWaiting)
	if !row.open() || row.needsYou() || row.BlockedOn != "" || row.Activity != line {
		t.Fatalf("row = %+v; want it open, waiting on %q, not the user's", row, line)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 1 || got.NeedYou != 0 {
		t.Fatalf("summary %+v; want the waiting worker counted, not the user's", got)
	}
	if past := f.pastTokens(t, f.caller.ID); slices.Contains(past, token) {
		t.Fatalf("past = %v; want the waiting worker kept out", past)
	}
	if after := f.listingAfterWindow(t, f.caller.ID); len(after.Rows) != 1 || after.Rows[0].Worker != string(workerWaiting) {
		t.Fatalf("after the settled window rows %+v; want the waiting worker kept", after.Rows)
	}
	return row
}

// expectRested asserts the worker's row settled into Past.
func (f *requestFixture) expectRested(t *testing.T, token string) {
	t.Helper()
	f.awaitWorker(t, f.caller.ID, token, "")
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 {
		t.Fatalf("summary %+v; want nothing open once the worker rests", got)
	}
	if past := f.pastTokens(t, f.caller.ID); !slices.Contains(past, token) {
		t.Fatalf("past = %v; want the resting worker's spawn", past)
	}
}

// A worker whose turn ended with a background shell running waits on it:
// its row is open with the shell as its line until the shell ends. A
// session that ends takes the work it owned with it, and the row settles.
func TestAWorkerWaitingOnItsBackgroundWorkIsOpen(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "bg-spawn", f.caller.ID)

	f.app.threadRequestsWG.Wait()
	frames := f.callerAgentFrames(t, f.caller.ID)
	launchWorkerShell(t, f.app, worker.ID, "deploy-shell")
	work, err := f.app.runningBackgroundWorkForThread(worker.ID, "")
	if err != nil || len(work) != 1 {
		t.Fatalf("running background work %+v, %v; this test needs the shell running", work, err)
	}
	f.expectWaiting(t, "bg-spawn", work[0].Summary)
	f.awaitCallerFrame(t, f.caller.ID, frames)
	// Its shell ends: the process exits, then the CLI tells the agent.
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventBackgroundTaskTerminal, ItemID: "deploy-shell"},
		map[string]any{"task_id": "task-deploy-shell", "tool_use_id": "deploy-shell", "status": "completed", "source": "task_updated"})
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventBackgroundTaskNotification, ItemID: "deploy-shell", Content: "deploy finished"},
		map[string]any{"task_id": "task-deploy-shell", "tool_use_id": "deploy-shell", "status": "completed", "uuid": "u-deploy-shell"})
	if work, err := f.app.runningBackgroundWorkForThread(worker.ID, ""); err != nil || len(work) != 0 {
		t.Fatalf("running background work after the shell ended %+v, %v", work, err)
	}
	f.expectRested(t, "bg-spawn")

	// Its session ends while another shell runs.
	launchWorkerShell(t, f.app, worker.ID, "watch-shell")
	f.awaitWorker(t, f.caller.ID, "bg-spawn", workerWaiting)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"}, nil)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"}, nil)
	f.expectRested(t, "bg-spawn")
}

// A worker with an armed loop waits on it: open with the loop named as its
// wake rows name it,
// through a fire that runs a turn and leaves the loop armed, until the
// loop is cancelled. A reminder holds it the same way until it fires once.
func TestAWorkerWithAnArmedLoopOrReminderIsOpen(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	f.watchAgentRows()
	installReplayingClaude(t, f.app, "checked the deploy")
	worker := f.settledSpawn(t, "loop-spawn", f.caller.ID)
	caller := threadtools.Caller{ThreadID: worker.ID}

	if _, err := f.adapter().Loop(t.Context(), caller, threadtools.LoopCall{Label: "deploy check", Note: "check the deploy", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop: %v", err)
	}
	f.expectWaiting(t, "loop-spawn", "Loop · deploy check")
	wakes, err := f.app.store.ListAgentWakes(worker.ID)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes %+v, %v", wakes, err)
	}
	// The loop fires: a turn runs and ends, and the loop is still armed.
	fired := time.Now().UnixMilli()
	if err := f.app.WakeThreadWakeNow(t.Context(), worker.ID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("WakeThreadWakeNow: %v", err)
	}
	awaitTurnSince(t, f.app, worker.ID, fired)
	if rows, err := f.app.store.ListAgentWakes(worker.ID); err != nil || len(rows) != 1 || rows[0].LastRunAt < fired || rows[0].State != store.AgentWakeArmed {
		t.Fatalf("wakes after the fire %+v, %v; want the loop run and still armed", rows, err)
	}
	f.expectWaiting(t, "loop-spawn", "Loop · deploy check")
	if err := f.app.CancelThreadWake(worker.ID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake: %v", err)
	}
	f.expectRested(t, "loop-spawn")

	// A one-shot reminder holds it until it fires.
	ack, err := f.adapter().Remind(t.Context(), caller, threadtools.RemindCall{DueAtUnixMs: time.Now().Add(time.Hour).UnixMilli(), Note: "look at the canary\nthen report"})
	if err != nil {
		t.Fatalf("Remind: %v", err)
	}
	f.expectWaiting(t, "loop-spawn", "Reminder · look at the canary")
	// Its fire is a turn of the worker's; once that ends, nothing holds it.
	fired = time.Now().UnixMilli()
	if err := f.app.fireThreadReminder(f.request(t, ack.Token), time.Now()); err != nil {
		t.Fatal(err)
	}
	awaitTurnSince(t, f.app, worker.ID, fired)
	f.expectRested(t, "loop-spawn")
}

// Archiving a worker cancels its loops and deleting one forgets it: either
// way its spawn's row settles, and a loop cancelled with nothing else
// rests it.
func TestAWaitingWorkerArchivedOrDeletedRests(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	arm := func(worker store.Thread, label string) {
		t.Helper()
		if _, err := f.adapter().Loop(t.Context(), threadtools.Caller{ThreadID: worker.ID}, threadtools.LoopCall{Label: label, Note: "check", EverySeconds: 3600}); err != nil {
			t.Fatalf("Loop: %v", err)
		}
	}
	archived := f.settledSpawn(t, "archived-loop", f.caller.ID)
	arm(archived, "nightly")
	f.awaitWorker(t, f.caller.ID, "archived-loop", workerWaiting)
	if _, err := f.app.ArchiveThread(archived.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	f.awaitWorker(t, f.caller.ID, "archived-loop", "")

	deleted := f.settledSpawn(t, "deleted-loop", f.caller.ID)
	arm(deleted, "hourly")
	f.awaitWorker(t, f.caller.ID, "deleted-loop", workerWaiting)
	if err := f.app.deleteThread(deleted.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	f.awaitWorker(t, f.caller.ID, "deleted-loop", "")
	waitUntil(t, 10*time.Second, func() bool { return !slices.Contains(f.app.liveWorkerThreads(), deleted.ID) })
	if got := f.workerSummary(t, f.caller.ID); got.Open != 0 {
		t.Fatalf("summary %+v; want none", got)
	}
}

// A provider timer, a scheduled wakeup, holds a worker as the idle reaper
// holds its session: waiting until the session ends and the timer with it.
func TestAWorkerWithAProviderTimerIsOpen(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "timer-spawn", f.caller.ID)
	f.app.sessionManager().put(worker.ID, session{Provider: string(provider.Claude), Token: "timer-session"})
	handler := startedSessionEventHandler(f.app, worker.ID, "timer-session", string(provider.Claude))
	f.awaitWorker(t, f.caller.ID, "timer-spawn", "")
	wakeup, err := json.Marshal(provider.SessionWakeupMeta{ScheduledForUnixMs: time.Now().Add(30 * time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	handler(provider.ProviderEvent{Kind: provider.EventSessionWakeup, ThreadID: worker.ID, Meta: wakeup, Timestamp: time.Now()})
	waitProviderEvents(t, f.app, worker.ID)
	f.expectWaiting(t, "timer-spawn", "")

	// A stop takes the session out of the registry; the timer lives in its
	// process until the read loop's last frame says the process is gone,
	// and goes with it.
	if _, ok := f.app.sessionManager().unregister(worker.ID, "timer-session"); !ok {
		t.Fatal("the session was not registered")
	}
	f.app.sendsPending.wg.Wait()
	f.app.threadRequestsWG.Wait()
	f.expectWaiting(t, "timer-spawn", "")
	handler(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: worker.ID, Content: "disconnected", Timestamp: time.Now()})
	waitProviderEvents(t, f.app, worker.ID)
	f.expectRested(t, "timer-spawn")
}

// After a restart nothing is in memory, and the boot reads the durable
// sources once each: a worker with a loop, one with a reminder and one
// with a remote command pending are open again, waiting.
func TestWaitingWorkersAreOpenAfterARestart(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	looping := f.settledSpawn(t, "restart-loop", f.caller.ID)
	reminded := f.settledSpawn(t, "restart-remind", f.caller.ID)
	remote := f.settledSpawn(t, "restart-remote", f.caller.ID)
	if _, err := f.adapter().Loop(t.Context(), threadtools.Caller{ThreadID: looping.ID}, threadtools.LoopCall{Label: "nightly", Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop: %v", err)
	}
	if _, err := f.adapter().Remind(t.Context(), threadtools.Caller{ThreadID: reminded.ID}, threadtools.RemindCall{DueAtUnixMs: time.Now().Add(time.Hour).UnixMilli(), Note: "follow up"}); err != nil {
		t.Fatalf("Remind: %v", err)
	}
	if _, err := f.app.store.RegisterRemoteWatch(store.RemoteWatch{
		ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: remote.ID, Fingerprint: strings.Repeat("ab", 32),
		Label: "integration suite", Command: "make test", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("RegisterRemoteWatch: %v", err)
	}

	after := f.restart(t)
	after.watchAgentRows()
	if rows := after.agentRows(t, f.caller.ID).Rows; len(rows) != 0 {
		t.Fatalf("before the boot seed rows %+v; want none", rows)
	}
	// The sweep runs for the app's life, so the request work group never
	// empties: poll the listing instead of waiting on it.
	after.app.startThreadRequestSweeps()
	waitUntil(t, 10*time.Second, func() bool {
		rows := after.agentRows(t, f.caller.ID).Rows
		waiting := 0
		for _, row := range rows {
			if row.Worker == string(workerWaiting) {
				waiting++
			}
		}
		return waiting == 3
	})
	if got := after.workerSummary(t, f.caller.ID); got.Open != 3 || got.NeedYou != 0 {
		t.Fatalf("after the restart summary %+v; want the three waiting workers", got)
	}
}

// A spawn on a paired computer whose thread arms a loop there and ends its
// turn is open here, waiting: that computer's answer says so. Once the
// loop is cancelled there, the next answer settles the row.
func TestARemoteWorkerWithAnArmedLoopIsOpen(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeReplies(t, pair.dest, "watching the deploy")
	ack := pair.spawnThere(t, "watch the deploy", nil)
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool { return pair.receipt(t, ack.Token).SettledAt != 0 })
	destID, _ := pair.dest.backendIdentity()
	there := threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"}
	if _, err := (threadToolsApp{app: pair.dest}).Loop(t.Context(), there, threadtools.LoopCall{Label: "deploy check", Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop there: %v", err)
	}
	pair.poll(t)
	agentPair := &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token)}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.NestedOpen })
	rowOf := func() ThreadAgentRow {
		listing, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := agentRowByToken(listing.Rows, ack.Token)
		return row
	}
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		row = rowOf()
		return row.Worker == string(workerWaiting)
	})
	if !row.open() || row.needsYou() || row.ComputerID != pair.computer {
		t.Fatalf("row = %+v; want the remote spawn open, waiting", row)
	}
	if got, err := pair.source.GetThreadWorkerSummary(t.Context(), pair.caller.ID); err != nil || got.Open != 1 || got.NeedYou != 0 {
		t.Fatalf("summary %+v, %v; want the waiting remote worker counted", got, err)
	}

	wakes, err := pair.dest.store.ListAgentWakes(child)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes there %+v, %v", wakes, err)
	}
	if err := pair.dest.CancelThreadWake(child, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake there: %v", err)
	}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return !row.NestedOpen })
	if row := rowOf(); row.open() {
		t.Fatalf("row = %+v; want the resting remote worker settled", row)
	}
}

// An app with no store, as a session test builds one, follows no worker:
// a process's end there reassesses nothing and reads nothing.
func TestAnAppWithoutAStoreFollowsNoWorker(t *testing.T) {
	t.Parallel()
	a := &App{}
	a.noteWorkerLiveness("no-store-thread")
	a.noteWorkerWaiting("no-store-thread")
	a.threadRequestsWG.Wait()
	if live := a.liveWorkerThreads(); len(live) != 0 {
		t.Fatalf("live workers %v; want none", live)
	}
}

// A waiting row names its loop, watch or reminder as the wake rows do.
func TestAWaitingRowNamesItsWakeAsTheWakeRowsDo(t *testing.T) {
	for _, c := range []struct{ kind, label, want string }{
		{store.AgentWakeLoop, "board", "Loop · board"},
		{store.AgentWakeWatch, "CI run", "Watch · CI run"},
		{threadWakeKindReminder, "look at the canary", "Reminder · look at the canary"},
		{store.AgentWakeWatch, "", "Watch"},
	} {
		if got := wakeWaitingLine(c.kind, c.label); got != c.want {
			t.Errorf("wakeWaitingLine(%q, %q) = %q, want %q", c.kind, c.label, got, c.want)
		}
	}
}
