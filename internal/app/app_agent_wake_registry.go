package app

import (
	"context"
	"log"
	"time"

	"agent-overflow/internal/store"
)

// The registry of the supervisor's runs: one run per row at most, each on its
// own goroutine joined at shutdown, and the stops that take a run out.

// agentWakeRegistered reports whether the supervisor runs something of the
// row.
func (a *App) agentWakeRegistered(id string) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	_, running := a.agentWakes.runs[id]
	return running
}

// startAgentWakeRun registers a run for one row and runs body on its own
// goroutine, joined at shutdown with the sweep before the store closes: a
// run writes its row when it ends. False, with nothing run, when one is
// already registered for it or the app is shutting down.
func (a *App) startAgentWakeRun(row store.AgentWake, body func(ctx context.Context, run *agentWakeRun)) bool {
	a.agentWakes.lifecycle.Lock()
	defer a.agentWakes.lifecycle.Unlock()
	return a.registerAgentWakeRunLocked(row, body)
}

// agentWakeRunRegistrableLocked reports whether a run of the row could be
// registered now: the app is not shutting down and no run of the row is
// registered. The caller holds the wake lifecycle lock.
func (a *App) agentWakeRunRegistrableLocked(wakeID string) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	return a.agentWakeRunRegistrableMuLocked(wakeID)
}

func (a *App) agentWakeRunRegistrableMuLocked(wakeID string) bool {
	if a.shuttingDown.Load() || a.lifeCtx().Err() != nil {
		return false
	}
	_, running := a.agentWakes.runs[wakeID]
	return !running
}

// registerAgentWakeRunLocked is startAgentWakeRun for a caller that holds
// the wake lifecycle lock.
func (a *App) registerAgentWakeRunLocked(row store.AgentWake, body func(ctx context.Context, run *agentWakeRun)) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if !a.agentWakeRunRegistrableMuLocked(row.ID) {
		return false
	}
	if a.agentWakes.runs == nil {
		a.agentWakes.runs = make(map[string]*agentWakeRun)
	}
	ctx, cancel := context.WithCancel(a.lifeCtx())
	run := &agentWakeRun{threadID: row.ThreadID, cancel: cancel, done: make(chan struct{})}
	a.agentWakes.runs[row.ID] = run
	if !a.threadRequestsWG.Go(func() {
		defer a.endAgentWakeRun(row.ID, run)
		body(ctx, run)
	}) {
		delete(a.agentWakes.runs, row.ID)
		cancel()
		return false
	}
	return true
}

func (a *App) endAgentWakeRun(wakeID string, run *agentWakeRun) {
	a.unregisterAgentWakeRun(wakeID, run)
	run.cancel()
	close(run.done)
}

// unregisterAgentWakeRun takes run out of the registry if it is still the
// row's, and reports whether it was: a stop or a retirement may already have
// taken it, and a later run may hold the row's place.
func (a *App) unregisterAgentWakeRun(wakeID string, run *agentWakeRun) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if a.agentWakes.runs[wakeID] != run {
		return false
	}
	delete(a.agentWakes.runs, wakeID)
	return true
}

// stopAgentWakeRun stops the run of one row, if any, without waiting.
func (a *App) stopAgentWakeRun(wakeID string) *agentWakeRun {
	a.agentWakes.mu.Lock()
	run := a.agentWakes.runs[wakeID]
	delete(a.agentWakes.runs, wakeID)
	a.agentWakes.mu.Unlock()
	if run != nil {
		run.mu.Lock()
		run.stopped = true
		run.mu.Unlock()
		run.cancel()
	}
	return run
}

// stopLongRunningAgentWatch stops a row's long-running process and waits a
// bounded time for it to exit, so a replacement never runs beside it. An
// interval run in flight is left to finish: its row stays `running` and the
// new registration applies from the next run.
func (a *App) stopLongRunningAgentWatch(wakeID string) {
	a.agentWakes.mu.Lock()
	run := a.agentWakes.runs[wakeID]
	a.agentWakes.mu.Unlock()
	if run == nil {
		return
	}
	row, found, err := a.store.GetAgentWake(wakeID)
	if err == nil && found && row.State == store.AgentWakeRunning && !row.LongRunning {
		return
	}
	if stopped := a.stopAgentWakeRun(wakeID); stopped != nil {
		select {
		case <-stopped.done:
		case <-time.After(agentWakeStopWait):
			log.Printf("agent wakes: the process of %s did not exit within %s", wakeID, agentWakeStopWait)
		}
	}
}

// stopAgentWakeRunsForThread stops every run of one thread.
func (a *App) stopAgentWakeRunsForThread(threadID string) {
	a.agentWakes.mu.Lock()
	var ids []string
	for id, run := range a.agentWakes.runs {
		if run.threadID == threadID {
			ids = append(ids, id)
		}
	}
	a.agentWakes.mu.Unlock()
	for _, id := range ids {
		a.stopAgentWakeRun(id)
	}
}

func (a *App) agentWakeSlots() chan struct{} {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if a.agentWakes.slots == nil {
		a.agentWakes.slots = make(chan struct{}, agentWakeRunFanOut)
	}
	return a.agentWakes.slots
}
