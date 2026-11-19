package app

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// StopThreadAgentRow stops one Agents row of the thread: it cancels the
// request, as thread_cancel does, with the caller's wake left armed so its
// agent learns a person stopped it. When the row's caller spawned the target,
// the Stop brings that thread to rest on whichever computer holds it
// (restThreadHere, or agent_stop on a paired computer; docs/specs/
// standing-agents.md, "At rest"). A request inside that lineage is cancelled
// with its wake disarmed: the thread that made it was stopped too, and a wake
// would start it again. A computer that predates agent_stop has its request
// cancelled and its turn interrupted, and the result names it in Failures. A
// nested row a paired computer listed is stopped through that computer. A
// row that had already settled reports ended, unless it is a spawn whose
// worker the Stop found not at rest or cut: it reports stopped and its
// spawner is told (tellSpawnerOfUserStop).
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRow(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	return a.stopThreadAgentRowIn(ctx, threadID, token)
}

// stopThreadAgentRowIn stops the row of threadID's listing with token from
// the ledger, without reading the listing: a request here whose caller is
// threadID or a thread its spawns made here, or a row the last answer of
// one of those spawns on a paired computer listed, stopped through that
// computer, which finds it the same way under the thread the spawn made
// there.
func (a *App) stopThreadAgentRowIn(ctx context.Context, threadID, token string) (ThreadAgentStop, error) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		return ThreadAgentStop{}, err
	}
	if !found {
		// A spawn the retention floor reduced to its lineage has no token
		// in the ledger, and still links its thread into the listing.
		listed, _, err := newThreadAgentView(a).tree(threadID, threadAgentRowDepth, []string{token}, threadAgentRowLimit)
		if err != nil {
			return ThreadAgentStop{}, err
		}
		if len(listed) == 1 && listed[0].LineageOnly {
			row, found = listed[0].ThreadRequest, true
		}
	}
	if found {
		mine, err := a.threadAgentLineageHolds(threadID, row.CallerThreadID)
		if err != nil {
			return ThreadAgentStop{}, err
		}
		if mine {
			return a.stopThreadAgentRequest(ctx, row, map[string]bool{threadID: true}, true), nil
		}
	}
	if spawn, ok := a.cachedPeerSpawnFor(token); ok {
		mine, err := a.threadAgentLineageHolds(threadID, spawn.CallerThreadID)
		if err != nil {
			return ThreadAgentStop{}, err
		}
		if mine {
			stop, err := a.peerThreadAgentStop(ctx, spawn.TargetComputerID, spawn.MadeIn(), spawn.TargetThreadID, token)
			if err != nil {
				return ThreadAgentStop{Token: token, Outcome: threadAgentFailed, Failures: []string{err.Error()}}, nil
			}
			stop.Token = token
			return stop, nil
		}
	}
	return ThreadAgentStop{}, errorsx.Public(threadtools.CodeRequestUnknown,
		"That agent is not one this thread is waiting on.", nil)
}

// threadAgentLineageHolds reports whether caller is threadID or a thread
// spawned under it on this computer, within the listing's depth.
func (a *App) threadAgentLineageHolds(threadID, caller string) (bool, error) {
	_, holds, err := a.threadAgentLineageDepth(threadID, caller)
	return holds, err
}

// threadAgentLineageDepth reports how many spawns down from threadID caller
// is, and whether it is within the listing's depth of it at all.
func (a *App) threadAgentLineageDepth(threadID, caller string) (int, bool, error) {
	current := caller
	for depth := range threadAgentRowDepth {
		if current == threadID {
			return depth, true, nil
		}
		thread, err := a.store.GetThread(current)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, false, nil
			}
			return 0, false, err
		}
		if thread.SpawnedByThreadID == "" {
			return 0, false, nil
		}
		current = thread.SpawnedByThreadID
	}
	return threadAgentRowDepth, current == threadID, nil
}

// StopThreadAgentRows stops every Agents row of the thread as
// StopThreadAgentRow stops one, and reports each top-level row with what
// its cascade stopped beneath it, then withdraws the incoming messages
// queued for the thread as StopThreadAgentIncoming does, each reported.
// The rows are every one of the thread's own that a Stop acts on, read
// from the ledger when it is pressed (threadAgentStopRows), however many
// the tray lists; the Stops of rows here start at once, beside those that
// wait on a paired computer.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentRows(ctx context.Context, threadID string) ([]ThreadAgentStop, error) {
	top, err := a.threadAgentStopRows(threadID)
	if err != nil {
		return nil, err
	}
	// The incoming rows are the ones shown now: the wakes the Stops below
	// queue tell the agent what the person stopped, and are not withdrawn.
	incoming := a.incomingAgentSendIDs(threadID)
	out := make([]ThreadAgentStop, len(top))
	var here, there []int
	for index, row := range top {
		if row.TargetComputerID == "" {
			here = append(here, index)
		} else {
			there = append(there, index)
		}
	}
	stopEach := func(indexes []int) {
		runBounded(len(indexes), threadAgentStopsInFlight, func(i int) {
			out[indexes[i]] = a.stopThreadAgentRequest(ctx, top[indexes[i]], map[string]bool{threadID: true}, true)
		})
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopEach(there)
	}()
	stopEach(here)
	wg.Wait()
	for _, sendID := range incoming {
		out = append(out, a.withdrawIncomingAgentMessage(ctx, threadID, sendID))
	}
	return out, nil
}

// stopThreadAgentRequest stops one request for a person and, for a spawn,
// brings the spawned thread to rest, wherever it runs (stopAgentRequestBy).
// pressed is the row the person stopped, whose caller's wake stays armed; a
// request reached through the cascade has its wake disarmed first. visited
// holds the threads this Stop already reached here, so a spawn cycle ends.
func (a *App) stopThreadAgentRequest(ctx context.Context, row store.ThreadRequest, visited map[string]bool, pressed bool) ThreadAgentStop {
	return a.stopAgentRequestBy(ctx, row, visited, pressed, userRestStop())
}

// stopThreadAgentTarget brings a spawned thread on this computer to rest for
// a person's Stop (restThreadHere).
func (a *App) stopThreadAgentTarget(ctx context.Context, target string, visited map[string]bool, result *ThreadAgentStop) {
	a.restThreadHere(ctx, target, userRestStop(), visited, result)
}

// threadAgentStopRows reads what a Stop of every Agents row of threadID
// acts on: each of its own rows but a send or ask that already settled, all
// of them, as the ledger held them when the read began
// (ListThreadAgentStopRows). The tray's listing is bounded for display; a
// Stop is not, and a request made after this read is not one it stops.
func (a *App) threadAgentStopRows(threadID string) ([]store.ThreadRequest, error) {
	view := newThreadAgentView(a)
	rows, err := a.store.ListThreadAgentStopRows(store.ThreadAgentStopRead{
		Root: threadID, Waiting: view.waiting, Live: view.live, SettledSince: view.since, Page: threadAgentRowLimit,
	})
	if err != nil {
		return nil, err
	}
	if hook := a.agentStopCapturedForTest; hook != nil {
		hook(threadID)
	}
	return rows, nil
}

// threadAgentCaller names a local caller thread to a paired computer, as a
// forwarded thread_cancel does.
func (a *App) threadAgentCaller(threadID string) threadtools.Caller {
	backendID, _ := a.backendIdentity()
	caller := threadtools.Caller{ThreadID: threadID, ComputerID: backendID, ComputerName: a.backendDisplayName()}
	if thread, err := a.store.GetThread(threadID); err == nil {
		caller.Title = thread.Title
	}
	return caller
}
