package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// thread_loop and thread_watch: a thread's own schedule
// (docs/specs/standing-agents.md, "Wake supervisor"). The rows live in
// `agent_wakes`; the thread request sweep fires them (app_agent_wake_runs.go)
// and delivers each wake through the queue reminders use
// (app_agent_wake_delivery.go).
//
// A wake's `running` state is the supervisor's own execution, never the
// agent's turn: a watch script in flight, a long-running watch's process
// alive, or a loop between its fire and its queued message.

const (
	// agentWakeFloor is the shortest interval a loop or watch may register.
	// A fire is a turn, so a shorter one spends the user's usage on a
	// clock; a harness boot lowers it (IsolationConfig.AgentWakeFloor).
	agentWakeFloor = threadtools.WakeFloorSeconds * time.Second
)

// agentWakeState is the supervisor's in-process state. Zero value ready.
type agentWakeState struct {
	// lifecycle orders a run's admission against every cancel: a fire's
	// claim and its run's registration are one step under it, and so are a
	// cancel's durable write and its stop of the runs. A cancel therefore
	// either comes first, and the claim fails, or finds the run registered
	// and stops it. Taken before mu, and never while holding it.
	lifecycle sync.Mutex
	mu        sync.Mutex
	// floorOverride replaces agentWakeFloor on a harness boot.
	floorOverride time.Duration
	// runs is every run in flight by wake id: an interval run between its
	// claim and its finish, or a long-running watch's supervisor.
	runs map[string]*agentWakeRun
	// slots bounds the interval runs that execute at once. Built on first
	// use.
	slots chan struct{}
	// ticks marks the threads whose current turn only wakes started (see
	// noteAgentWakeDispatch).
	ticks map[string]bool
	// intake is, per thread and turn, every message the provider delivered
	// into that turn, by its row, and whether only wakes are in it
	// (noteAgentTurnIntake). It holds the turns from the last one that
	// ended on.
	intake map[string]map[int]map[string]bool
}

// agentWakeRun is one run in flight.
type agentWakeRun struct {
	threadID string
	cancel   context.CancelFunc
	done     chan struct{}
	// stopped is set before cancel when the row was cancelled or replaced:
	// the run then writes nothing, because the row is no longer its own.
	// A cancel with stopped unset is the app shutting down.
	mu      sync.Mutex
	stopped bool
}

func (r *agentWakeRun) wasStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (a *App) agentWakeFloor() time.Duration {
	if a.agentWakes.floorOverride > 0 {
		return a.agentWakes.floorOverride
	}
	return agentWakeFloor
}

func newAgentWakeID() string { return entityid.New() }

// Loop registers or replaces one loop of the caller's thread.
func (t threadToolsApp) Loop(_ context.Context, caller threadtools.Caller, call threadtools.LoopCall) (threadtools.WakeState, error) {
	if _, err := t.localThread(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	}
	interval, err := t.app.checkAgentWakeInterval(call.EverySeconds)
	if err != nil {
		return threadtools.WakeState{}, err
	}
	row, err := t.app.registerAgentWake(store.AgentWake{
		ThreadID: caller.ThreadID, Kind: store.AgentWakeLoop, Label: call.Label, Note: call.Note,
		IntervalSeconds: int64(call.EverySeconds), NextAt: time.Now().Add(interval).UnixMilli(),
	})
	if err != nil {
		return threadtools.WakeState{}, err
	}
	return agentWakeToolState(row), nil
}

// Watch registers or replaces one watch of the caller's thread. Only an
// assistant's current thread may: a watch runs a script with the
// assistant's home, and no other thread has one.
func (t threadToolsApp) Watch(_ context.Context, caller threadtools.Caller, call threadtools.WatchCall) (threadtools.WakeState, error) {
	if _, err := t.localThread(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	}
	if _, standing, err := t.app.store.GetStandingAgent(caller.ThreadID); err != nil {
		return threadtools.WakeState{}, err
	} else if !standing {
		return threadtools.WakeState{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"thread_watch is for assistants, and this thread is not an assistant's current thread: a watch runs a script with the assistant's own home directory. Use thread_loop, which any thread may register, or ask the user to create an assistant for this work.", nil)
	}
	row := store.AgentWake{
		ThreadID: caller.ThreadID, Kind: store.AgentWakeWatch, Label: call.Label,
		Command: call.Command, Cwd: call.Cwd, LongRunning: call.LongRunning,
	}
	if !call.LongRunning {
		interval, err := t.app.checkAgentWakeInterval(call.EverySeconds)
		if err != nil {
			return threadtools.WakeState{}, err
		}
		switch {
		case call.TimeoutSeconds == 0:
			row.TimeoutSeconds = threadtools.DefaultWakeTimeoutSeconds
		case call.TimeoutSeconds < 1 || call.TimeoutSeconds > threadtools.MaxWakeTimeoutSeconds:
			return threadtools.WakeState{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
				"timeout_seconds must be between 1 and %d.", threadtools.MaxWakeTimeoutSeconds), nil)
		default:
			row.TimeoutSeconds = int64(call.TimeoutSeconds)
		}
		// The first run is one interval out, as a loop's first fire is, so
		// it never lands inside the turn that registered it.
		row.IntervalSeconds, row.NextAt = int64(call.EverySeconds), time.Now().Add(interval).UnixMilli()
	}
	stored, err := t.app.registerAgentWake(row)
	if err != nil {
		return threadtools.WakeState{}, err
	}
	return agentWakeToolState(stored), nil
}

// ListWakes lists the caller's live loops and watches.
func (t threadToolsApp) ListWakes(_ context.Context, caller threadtools.Caller) ([]threadtools.WakeState, error) {
	rows, err := t.app.store.ListAgentWakes(caller.ThreadID)
	if err != nil {
		return nil, err
	}
	out := make([]threadtools.WakeState, 0, len(rows))
	for _, row := range rows {
		out = append(out, agentWakeToolState(row))
	}
	return out, nil
}

// CancelWake cancels the caller's live loop or watch with this label, stops
// what it has running and withdraws its wake still queued.
func (t threadToolsApp) CancelWake(_ context.Context, caller threadtools.Caller, label, kind string) (threadtools.WakeState, bool, error) {
	row, cancelled, err := t.app.cancelAgentWake(caller.ThreadID, label, kind)
	if err != nil || !cancelled {
		return threadtools.WakeState{}, false, err
	}
	return agentWakeToolState(row), true, nil
}

// cancelAgentWake cancels a thread's live loop or watch by label, stops what
// it has running and withdraws its wake still queued. It is the one cancel of
// a single row: thread_cancel and the activity rail's Cancel both take it.
// Not cancelled is no live row of that kind under that label. A wake that
// could not be taken back is an error, with the row already cancelled.
func (a *App) cancelAgentWake(threadID, label, kind string) (store.AgentWake, bool, error) {
	a.agentWakes.lifecycle.Lock()
	row, cancelled, err := a.store.CancelAgentWake(threadID, label, kind, agentWakeOutcomeCancelled)
	if err == nil && cancelled {
		a.emitAgentWakeChanged(threadID, row.ID)
		a.stopAgentWakeRun(row.ID)
	}
	a.agentWakes.lifecycle.Unlock()
	if err != nil || !cancelled {
		return store.AgentWake{}, false, err
	}
	if err := a.withdrawCancelledAgentWakes(row.ThreadID, row.ID); err != nil {
		log.Printf("agent wakes: %v", err)
		return store.AgentWake{}, false, fmt.Errorf("the %s %q is cancelled, but its wake already queued could not be taken back and may still arrive", row.Kind, row.Label)
	}
	row.State, row.NextAt = store.AgentWakeCancelled, 0
	return row, true, nil
}

// checkAgentWakeInterval refuses an interval below the computer's floor.
func (a *App) checkAgentWakeInterval(seconds int) (time.Duration, error) {
	interval := time.Duration(seconds) * time.Second
	if floor := a.agentWakeFloor(); interval < floor {
		return 0, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"every_seconds must be at least %d on this computer: each fire is a turn.", int(floor/time.Second)), nil)
	}
	return interval, nil
}

// registerAgentWake writes one registration and brings the supervisor in
// line with it: a long-running watch's process is restarted with the new
// command, a row that stopped being one has its process stopped, and the
// sweep is woken for an interval row whose first fire may be sooner than its
// sleep.
func (a *App) registerAgentWake(row store.AgentWake) (store.AgentWake, error) {
	row.ID = newAgentWakeID()
	stored, err := a.store.RegisterAgentWake(row)
	switch {
	case errors.Is(err, store.ErrAgentWakeLabelTaken):
		return store.AgentWake{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"Another of this thread's loops or watches is already named %q. Choose another label, or stop that one first with thread_cancel label.", row.Label), err)
	case errors.Is(err, store.ErrAgentWakeLimit):
		return store.AgentWake{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"This thread already has %d loops and watches, the most one thread keeps. Stop one with thread_cancel label first.", store.MaxAgentWakesPerThread), err)
	case err != nil:
		return store.AgentWake{}, err
	}
	a.emitAgentWakeChanged(stored.ThreadID, stored.ID)
	if stored.Kind == store.AgentWakeWatch {
		a.stopLongRunningAgentWatch(stored.ID)
		if stored.LongRunning {
			a.startAgentWatchProcess(stored)
		}
	}
	a.nudgeThreadRequestSweep()
	return stored, nil
}

// agentWakeToolState is one row as thread_status lists it.
func agentWakeToolState(row store.AgentWake) threadtools.WakeState {
	return threadtools.WakeState{
		Kind: row.Kind, Label: row.Label, State: row.State, Note: row.Note,
		Command: row.Command, Cwd: row.Cwd, EverySeconds: row.IntervalSeconds,
		TimeoutSeconds: row.TimeoutSeconds, LongRunning: row.LongRunning, NextFireAt: row.NextAt,
		LastRunAt: row.LastRunAt, LastOutcome: row.LastOutcome, LastOutput: row.LastOutput,
		Undelivered: row.Undelivered, Missed: row.Missed,
	}
}

// cancelAgentWakesForThread cancels every loop and watch of a thread that is
// being archived, deleted or moved, and stops what they have running. Rows a
// delete has already cascaded away still have their runs stopped by thread.
// It may run under the thread's locks, as a delete's and an archive's do,
// and stopping a run does not wait for it. So the callers that keep the
// thread withdraw its queued wakes themselves (withdrawCancelledAgentWakes);
// a deleted thread's queue goes with it. The runs stop even when the rows
// could not be cancelled, and that failure is returned.
func (a *App) cancelAgentWakesForThread(threadID, outcome string) error {
	a.agentWakes.lifecycle.Lock()
	ids, err := a.store.CancelAgentWakesForThread(threadID, "", outcome)
	if len(ids) > 0 {
		a.emitAgentWakesRemoved(threadID, ids)
	}
	a.stopAgentWakeRunsForThread(threadID)
	a.agentWakes.lifecycle.Unlock()
	if err != nil {
		return fmt.Errorf("cancel the loops and watches of %s: %w", threadID, err)
	}
	return nil
}

// noteAgentWakeDispatch records whether the turn a flush dispatch starts is a
// tick: one only loop and watch wakes started. It decides only whether that
// turn's start retracts a notification when no delivery has named what the
// turn took in yet (agentWakeTickStarted); whether its end is quiet is read
// from its deliveries (agentTurnIsTick). A dispatch into a running turn
// starts nothing, but a person's message joining a tick makes the turn
// theirs.
func (a *App) noteAgentWakeDispatch(threadID string, allWakes, newTurn bool) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	switch {
	case newTurn && allWakes:
		if a.agentWakes.ticks == nil {
			a.agentWakes.ticks = make(map[string]bool)
		}
		a.agentWakes.ticks[threadID] = true
	case newTurn || !allWakes:
		delete(a.agentWakes.ticks, threadID)
	}
}

// onSendDelivered is triage's send-delivered hook: the delivery is what a
// turn took in (noteAgentTurnIntake) and binds the requests it carries
// (bindDeliveredRequests).
func (a *App) onSendDelivered(threadID string, delivery triage.SendDelivery) {
	a.noteAgentTurnIntake(threadID, delivery)
	a.bindDeliveredRequests(threadID, delivery)
}

// noteAgentTurnIntake records a message the provider delivered into a turn:
// a turn is a tick when it took messages in and only wakes are in them,
// whichever turn their rows show in. A message taken in before its turn
// opened is recorded when that turn opens and reports it again. A merge
// that emptied a turn moves what it took in to the turn that ran the merged
// message.
func (a *App) noteAgentTurnIntake(threadID string, delivery triage.SendDelivery) {
	if delivery.AwaitingTurn {
		return
	}
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if a.agentWakes.intake == nil {
		a.agentWakes.intake = make(map[string]map[int]map[string]bool)
	}
	turns := a.agentWakes.intake[threadID]
	if turns == nil {
		turns = make(map[int]map[string]bool)
		a.agentWakes.intake[threadID] = turns
	}
	if delivery.Rebind {
		delete(turns, delivery.ReboundFrom)
	}
	messages := turns[delivery.TurnIndex]
	if messages == nil {
		messages = make(map[string]bool)
		turns[delivery.TurnIndex] = messages
	}
	tick := usermessage.AllTicks(delivery.Members)
	if prior, ok := messages[delivery.UserItemID]; ok {
		tick = tick && prior
	}
	messages[delivery.UserItemID] = tick
}

// turnTookOnlyWakesLocked reports what turnIndex took in: whether it took
// anything, and whether only wakes are in it. Caller holds agentWakes.mu.
func (a *App) turnTookOnlyWakesLocked(threadID string, turnIndex int) (took, tick bool) {
	messages := a.agentWakes.intake[threadID][turnIndex]
	if len(messages) == 0 {
		return false, false
	}
	for _, wakes := range messages {
		if !wakes {
			return true, false
		}
	}
	return true, true
}

// tapAgentWakeTurns keeps the marks that say which of a thread's turns are
// ticks, from the turn starts and ends on the emit funnel. It runs before
// the notification tap, which reads a start's mark (agentWakeTickStarted).
func (a *App) tapAgentWakeTurns(name eventchan.Channel, data any) {
	switch name {
	case eventchan.ProviderTurnStarted:
		if event, ok := data.(triage.TurnStartedEvent); ok {
			a.noteAgentWakeTurnStart(event.ThreadID, event.TurnIndex)
		}
	case eventchan.ProviderTurnCompleted:
		// CountsAsActivity is a top-level turn's end, not a subagent's.
		if event, ok := data.(triage.TurnCompletedEvent); ok && event.CountsAsActivity {
			a.endAgentWakeTickTurn(event.ThreadID, event.TurnIndex)
		}
	}
}

// agentWakeTickStarted reports whether the turn threadID started last is a
// tick, by the mark its start left (noteAgentWakeTurnStart).
func (a *App) agentWakeTickStarted(threadID string) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	return a.agentWakes.ticks[threadID]
}

// noteAgentWakeTurnStart marks a starting turn by what it took in, when its
// deliveries came before its start (a message Claude `started` before the
// turn's init): a tick when only wakes are in them. A turn that has taken
// nothing in yet keeps the mark its dispatch left (noteAgentWakeDispatch).
func (a *App) noteAgentWakeTurnStart(threadID string, turnIndex int) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	took, tick := a.turnTookOnlyWakesLocked(threadID, turnIndex)
	switch {
	case !took:
	case tick:
		if a.agentWakes.ticks == nil {
			a.agentWakes.ticks = make(map[string]bool)
		}
		a.agentWakes.ticks[threadID] = true
	default:
		delete(a.agentWakes.ticks, threadID)
	}
}

// endAgentWakeTickTurn runs when a top-level turn ends. It forgets the
// thread's tick mark, so a turn the provider starts on its own is not one,
// and what earlier turns took in. What this turn took in stays for a later
// round of it (agentTurnIsTick), until a later turn ends.
func (a *App) endAgentWakeTickTurn(threadID string, turnIndex int) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	delete(a.agentWakes.ticks, threadID)
	turns := a.agentWakes.intake[threadID]
	for turn := range turns {
		if turn < turnIndex {
			delete(turns, turn)
		}
	}
	if len(turns) == 0 {
		delete(a.agentWakes.intake, threadID)
	}
}

// agentTurnIsTick reports whether a top-level turn is a tick, which nobody
// needs to read when it ends cleanly (triage.SetQuietTurnClassifier): it
// took messages in and every one is a loop or watch wake. A turn that took
// nothing in is the provider's own (a background task's notification, a
// scheduled wakeup) and is never one; a person's message in it makes it
// theirs.
func (a *App) agentTurnIsTick(threadID string, turnIndex int) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	_, tick := a.turnTookOnlyWakesLocked(threadID, turnIndex)
	return tick
}

// forgetAgentWakeTurns drops a thread's turn marks when its session ends:
// the next session's turns are its own.
func (a *App) forgetAgentWakeTurns(threadID string) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	delete(a.agentWakes.ticks, threadID)
	delete(a.agentWakes.intake, threadID)
}

// flushGroupIsAgentWakes reports whether every message of a dispatch is a
// loop or watch wake.
func flushGroupIsAgentWakes(members []flushMember) bool {
	for _, member := range members {
		if !usermessage.IsTickSendID(member.payload.SendID) {
			return false
		}
	}
	return len(members) > 0
}
