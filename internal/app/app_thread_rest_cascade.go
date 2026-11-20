package app

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// What a rest stops below its thread: every request the thread made, each
// spawned thread brought to rest by the same rule on whichever computer it
// runs, and the message that tells a spawner a person stopped its worker.

// restRequestsPage bounds one read of the requests a rest stops.
const restRequestsPage = 200

// stopRequestsOfRestingThread stops every request the thread made: a
// reminder is dropped, a send or ask is cancelled with its wake disarmed and
// only the turn it started cut, and a spawn's thread is brought to rest by
// the same rule, on whichever computer it runs (stopAgentRequestBy). What it
// stops is what the ledger held once the session had ended, read whole before
// any of it is stopped, as a Stop All reads its rows; a request made after
// that read belongs to whoever started the thread again.
func (a *App) stopRequestsOfRestingThread(ctx context.Context, threadID string, by restStop, visited map[string]bool, result *ThreadAgentStop) {
	rows, err := a.threadRestRequests(threadID)
	if err != nil {
		result.Failures = append(result.Failures, "list its agents: "+err.Error())
		return
	}
	if hook := a.agentStopCapturedForTest; hook != nil {
		hook(threadID)
	}
	for _, row := range rows {
		if row.Kind == store.ThreadRequestRemind {
			if _, err := a.dropThreadReminder(row); err != nil {
				result.Failures = append(result.Failures, "drop its reminder: "+err.Error())
			}
			continue
		}
		stop := a.stopAgentRequestBy(ctx, row, visited, false, by)
		if stop.Outcome == threadAgentStopped {
			result.Nested++
		}
		result.Nested += stop.Nested
		result.Background += stop.Background
		result.Failures = append(result.Failures, stop.Failures...)
	}
}

// threadRestRequests reads every request a rest of threadID stops
// (ListThreadRestRequests), page by page.
func (a *App) threadRestRequests(threadID string) ([]store.ThreadRequest, error) {
	var out []store.ThreadRequest
	var after store.ThreadRestCursor
	for {
		rows, err := a.store.ListThreadRestRequests(threadID, after, restRequestsPage)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < restRequestsPage {
			return out, nil
		}
		after = after.After(rows[len(rows)-1])
	}
}

// stopAgentRequestBy stops one request and, for a spawn, brings the thread it
// made to rest by the same rule, wherever it runs. pressed is the row a
// person stopped, whose caller's wake stays armed so its agent learns of it;
// any other request has its wake disarmed first, because the thread that made
// it is being stopped too and a wake would start it again. A send or ask
// only has its request cancelled: the cancel stops the turn the request
// started there and nothing else of a thread that belongs to someone else.
func (a *App) stopAgentRequestBy(ctx context.Context, row store.ThreadRequest, visited map[string]bool, pressed bool, by restStop) ThreadAgentStop {
	result := ThreadAgentStop{Token: row.Token, Outcome: threadAgentStopped}
	current, found, err := a.store.GetThreadRequest(row.Token)
	if err != nil {
		result.Outcome = threadAgentFailed
		result.Failures = append(result.Failures, "read the request: "+err.Error())
		return result
	}
	if !found {
		result.Outcome = threadAgentEnded
		if !row.LineageOnly {
			return result
		}
		// Past the retention floor a spawn's token is unknown, and its row
		// still links the thread it made: the Stop reaches into that thread.
		current = row
	}
	settled := threadRequestSettled(current)
	if settled {
		result.Outcome = threadAgentEnded
	} else if !pressed && current.Notify {
		if _, err := a.store.SetThreadRequestNotify(current.Token, false); err != nil {
			result.Failures = append(result.Failures, "disarm its wake: "+err.Error())
		}
	}
	// The Stop reaches into the worker for a run the row's caller started
	// (app_thread_agent_rows_runs.go): a spawn's until its run ended, and
	// the open run of a send to the worker the caller spawned. A person's
	// Stop of a row whose run ended leaves what the worker does since, a
	// run someone else started, as it is; a cascade, which stops the
	// lineage, reaches every spawn.
	spawned := current.TargetThreadID != "" &&
		((current.Kind == store.ThreadRequestSpawn && (!pressed || !current.RunEnded())) ||
			(current.Kind == store.ThreadRequestSend && pressed && current.RunOpen()))
	// A Stop of a spawn that already settled that finds its worker not at
	// rest, or whose rest stopped anything of it or below it
	// (restStoppedWork), reports it stopped, and ended otherwise. When a
	// person pressed it, the spawner is told too, since no settlement
	// tells it. A cascade tells nobody: the spawner it reaches is being
	// brought to rest itself. An agent's own thread_cancel does not come
	// through here.
	outlived := pressed && settled && spawned
	if current.TargetComputerID != "" {
		notice := a.workerStoppedNotice(current, outlived, &result)
		a.stopRemoteAgentRequestBy(ctx, current, settled, spawned, by, &result)
		if result.Outcome == threadAgentStopped {
			a.tellSpawnerOfUserStop(current, notice, &result)
		}
		return result
	}
	if !settled {
		effect, err := a.stopThreadRequestWorkBy(ctx, current.Token, by.answer, func(store.ThreadRequestReceipt) turnStop { return by.turn })
		if err != nil {
			result.Outcome = threadAgentFailed
			result.Failures = append(result.Failures, "cancel the request: "+err.Error())
		}
		result.Interrupted = effect == threadtools.EffectInterrupted
	}
	if !spawned || visited[current.TargetThreadID] {
		return result
	}
	visited[current.TargetThreadID] = true
	working := a.workerNotAtRest(current.TargetThreadID, &result)
	notice := a.workerStoppedNotice(current, outlived, &result)
	before := result
	a.restThreadHere(ctx, current.TargetThreadID, by, visited, &result)
	if settled && (working || restStoppedWork(before, result)) {
		result.Outcome = threadAgentStopped
		a.tellSpawnerOfUserStop(current, notice, &result)
	}
	return result
}

// workerStoppedNotice is the send id of the message a person's Stop of a
// worker that outlived its spawn tells the spawner with, empty for any other
// Stop. It is read before the rest, as the Stop finds the worker, so Stops
// racing on one stopping of it read the same send id (workerStoppedSendID).
// A read that fails is named in result and tells nothing.
func (a *App) workerStoppedNotice(spawn store.ThreadRequest, outlived bool, result *ThreadAgentStop) string {
	if !outlived {
		return ""
	}
	sendID, err := a.workerStoppedSendID(spawn)
	if err != nil {
		result.Failures = append(result.Failures, "tell its spawner: "+err.Error())
		return ""
	}
	if hook := a.workerStopObservedForTest; hook != nil {
		hook(spawn.Token)
	}
	return sendID
}

// workerNotAtRest reports whether threadID is not at rest as a Stop reaches
// it (workerUnrest). A read that fails is named in result.Failures and counts
// as not at rest, so a Stop that may have stopped work says so.
func (a *App) workerNotAtRest(threadID string, result *ThreadAgentStop) bool {
	rest, err := a.workerUnrest(threadID)
	if err != nil {
		result.Failures = append(result.Failures, "read what it was doing: "+err.Error())
		return true
	}
	return rest.unrest != ""
}

// restStoppedWork reports whether a rest added to after, over before, a cut
// turn, stopped background work, or an agent stopped below the thread: a
// thread with a worker below it that is not at rest is not at rest itself.
func restStoppedWork(before, after ThreadAgentStop) bool {
	return (after.Interrupted && !before.Interrupted) || after.Background > before.Background ||
		after.Nested > before.Nested
}

// workerStoppedSendIDRoot opens the send id of the message that tells a
// spawner the user stopped a worker that outlived its spawn,
// `worker-stopped:<token>:<n>`, n counting the Stops of the worker that
// stopped something (workerStoppedSendID).
const workerStoppedSendIDRoot = "worker-stopped:"

// workerStoppedSendID names a Stop's message to spawn's caller: the first n
// no message the caller holds or took in carries. The message of a Stop is
// recorded once its rest is over, the worker then at rest, so a Stop that
// stops the worker again, after it was continued, reads the next n, and
// Stops that found it working before the first message was recorded read
// the same n, which keeps them to one message.
func (a *App) workerStoppedSendID(spawn store.ThreadRequest) (string, error) {
	for n := 1; ; n++ {
		sendID := fmt.Sprintf("%s%s:%d", workerStoppedSendIDRoot, spawn.Token, n)
		_, found, err := a.findRecordedSend(spawn.CallerThreadID, sendID)
		if err != nil || !found {
			return sendID, err
		}
	}
}

// tellSpawnerOfUserStop tells the thread that made spawn that the user
// stopped the worker it made after the spawn had settled, as an app-written
// message with sendID (workerStoppedNotice) attributed to the worker, the
// way the spawn's answer was; an empty sendID tells nothing. The spawn and
// its answer stay as they are. An archived spawner is told nothing; any
// other failure is named in the result.
func (a *App) tellSpawnerOfUserStop(spawn store.ThreadRequest, sendID string, result *ThreadAgentStop) {
	if sendID == "" {
		return
	}
	title, computerID, computerName := spawn.TargetThreadTitle, "", ""
	if spawn.TargetComputerID != "" {
		computerName, computerID = a.threadTools().requestComputerName(spawn)
	} else if thread, err := a.store.GetThread(spawn.TargetThreadID); err == nil {
		title = thread.Title
	}
	// The Stop's failures, named as its result names them: the worker is at
	// rest when there are none.
	note, err := usermessage.StoppedWorker(threadtools.WorkerStopped{
		Title: title, ThreadID: spawn.TargetThreadID, Token: spawn.Token, Failures: result.Failures,
	}, computerID, computerName)
	if err != nil {
		result.Failures = append(result.Failures, "tell its spawner: "+err.Error())
		return
	}
	err = a.queueAgentNotice(agentNotice{
		threadID: spawn.CallerThreadID,
		sendID:   sendID,
		prepare:  func() (usermessage.AgentMessage, bool, error) { return note, true, nil },
		startFailed: func(err error) string {
			return "A message that the user stopped a thread this one spawned is queued, but the agent could not start: " + err.Error()
		},
	})
	if err != nil && !errors.Is(err, errThreadArchived) {
		result.Failures = append(result.Failures, "tell its spawner: "+err.Error())
	}
}

// stopRemoteAgentRequestBy is stopAgentRequestBy for a target on a paired
// computer: the request through thread_cancel, and for a spawn the rest
// there. A person's Stop asks for it through agent_stop, which reports what
// it stopped; a computer that predates agent_stop has the spawned thread's
// turn interrupted instead, and the result names it in Failures. An agent's
// stop asks through thread_cancel by thread id, which a computer that knows
// the rest applies to a thread the caller spawned, and an older one answers
// with an interrupt.
func (a *App) stopRemoteAgentRequestBy(ctx context.Context, current store.ThreadRequest, settled, spawned bool, by restStop, result *ThreadAgentStop) {
	tools := a.threadTools()
	// The destination's receipts name the thread that made the request,
	// which New Thread, Same Assistant may since have replaced.
	caller := a.threadAgentCaller(current.MadeIn())
	if !settled {
		report, err := tools.cancelRemoteRequest(ctx, caller, current, by.user)
		if err != nil {
			result.Outcome = threadAgentFailed
			result.Failures = append(result.Failures, "cancel the request: "+err.Error())
		}
		result.Interrupted = report.Effect == threadtools.EffectInterrupted
	}
	if !spawned {
		return
	}
	stopTurn := func() {
		report, err := tools.cancelRemoteThread(ctx, caller, threadtools.CancelCall{
			ThreadID: current.TargetThreadID, ComputerID: current.TargetComputerID,
		}, by.user)
		if err != nil {
			result.Failures = append(result.Failures, "interrupt its turn: "+err.Error())
		} else if report.Effect == threadtools.EffectInterrupted || report.Effect == threadtools.EffectThreadStopped {
			result.Interrupted = true
		}
	}
	if !by.user {
		stopTurn()
		return
	}
	remote, err := a.peerThreadAgentStop(ctx, current.TargetComputerID, current.MadeIn(), current.TargetThreadID, "")
	if err != nil {
		if errors.Is(err, errThreadAgentPeerOlder) {
			stopTurn()
		}
		result.Failures = append(result.Failures, "stop its work: "+err.Error())
		return
	}
	// The destination's outcome says whether its rest stopped a thread not
	// at rest (stopThreadAgentWorkHere); for a request that settled it is
	// this Stop's.
	if settled && remote.Outcome == threadAgentStopped {
		result.Outcome = threadAgentStopped
	}
	result.Interrupted = result.Interrupted || remote.Interrupted
	result.Background += remote.Background
	result.Nested += remote.Nested
	result.Failures = append(result.Failures, remote.Failures...)
}
